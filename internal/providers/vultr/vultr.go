package vultr

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/bradenaw/juniper/xmaps"
	"github.com/bradenaw/juniper/xslices"
	"github.com/vultr/govultr/v3"
	"golang.org/x/oauth2"
)

const (
	providerName  = "vultr"
	cacheDuration = 24 * time.Hour // Cache prices for 24 hours
)

type vultrSize struct {
	PlanID     string
	HourlyCost float64
}

type vultrProvider struct {
	vultrClient         *govultr.Client
	apiKey              string
	sshKey              string
	ownerID             string // Unique identifier for this tscloudvpn instance
	ownerTag            string // Tag combining owner key and value for filtering
	regionSizeCacheLock sync.RWMutex
	regionSizeCache     map[string]vultrSize
	regionSizeCacheTime time.Time
}

func NewProvider(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.Vultr.APIKey == "" {
		return nil, nil
	}

	config := &oauth2.Config{}
	ts := config.TokenSource(ctx, &oauth2.Token{AccessToken: cfg.Providers.Vultr.APIKey})
	vultrClient := govultr.NewClient(oauth2.NewClient(ctx, ts))
	ownerID := providers.GetOwnerID(cfg)

	return &vultrProvider{
		vultrClient:     vultrClient,
		apiKey:          cfg.Providers.Vultr.APIKey,
		sshKey:          cfg.SSH.PublicKey,
		ownerID:         ownerID,
		ownerTag:        fmt.Sprintf("%s:%s", providers.OwnerTagKey, ownerID),
		regionSizeCache: make(map[string]vultrSize),
	}, nil
}

// prefetchPrices loads all region sizes to warm up the cache
func (v *vultrProvider) prefetchPrices() {
	// Only prefetch if cache is empty or expired
	v.regionSizeCacheLock.RLock()
	shouldPrefetch := len(v.regionSizeCache) == 0 || time.Since(v.regionSizeCacheTime) >= cacheDuration
	v.regionSizeCacheLock.RUnlock()

	if !shouldPrefetch {
		return
	}

	v.regionSizeCacheLock.Lock()
	defer v.regionSizeCacheLock.Unlock()

	shouldPrefetch = len(v.regionSizeCache) == 0 || time.Since(v.regionSizeCacheTime) >= cacheDuration
	if !shouldPrefetch {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Now fetch all plans once (efficient)
	plans, _, _, err := v.vultrClient.Plan.List(ctx, "all", &govultr.ListOptions{
		PerPage: 500,
	})
	if err != nil {
		log.Printf("Failed to prefetch Vultr plans: %v", err)
		return
	}

	// Clear existing cache
	v.regionSizeCache = make(map[string]vultrSize)
	v.regionSizeCacheTime = time.Now()

	regions := xmaps.Set[string]{}
	plans = xslices.Filter(plans, func(plan govultr.Plan) bool {
		// Skip free and IPv6 plans
		return plan.MonthlyCost > 0 && !strings.HasSuffix(plan.ID, "-v6")
	})

	for _, plan := range plans {
		for _, location := range plan.Locations {
			regions.Add(location)
		}
	}

	// Process each region
	for region := range regions {
		validPlans := xslices.Filter(plans, func(plan govultr.Plan) bool {
			return xslices.Index(plan.Locations, region) != -1
		})

		// Sort to find cheapest plan
		sort.Slice(validPlans, func(i, j int) bool {
			return validPlans[i].MonthlyCost < validPlans[j].MonthlyCost
		})

		cheapestPlan := validPlans[0]
		// Store both plan ID and hourly cost
		v.regionSizeCache[region] = vultrSize{
			PlanID:     cheapestPlan.ID,
			HourlyCost: float64(cheapestPlan.MonthlyCost) / 30.0 / 24.0,
		}
	}

	log.Printf("Vultr region size cache populated with %d regions", len(v.regionSizeCache))
}

func vultrInstanceHostname(region string) string {
	return fmt.Sprintf("vultr-%s", region)
}

func (v *vultrProvider) DeleteInstance(ctx context.Context, instanceID providers.InstanceID) error {
	err := v.vultrClient.Instance.Delete(ctx, instanceID.ProviderID)
	if err != nil {
		return fmt.Errorf("failed to delete Vultr instance: %w", err)
	}

	log.Printf("Deleted Vultr instance %s", instanceID.ProviderID)
	return nil
}

func (v *vultrProvider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	tmplOut := new(bytes.Buffer)
	hostname := vultrInstanceHostname(region)
	if err := template.Must(template.New("tmpl").Parse(providers.InitData)).Execute(tmplOut, struct {
		Args   string
		OnExit string
		SSHKey string
	}{
		Args: fmt.Sprintf(
			`%s --hostname=%s`,
			strings.Join(key.GetCLIArgs(), " "),
			hostname,
		),
		OnExit: fmt.Sprintf("curl https://api.vultr.com/v2/instances/$(curl -s http://169.254.169.254/v1.json | jq -r '.\"instance-v2-id\"') -X DELETE -H 'Authorization: Bearer %s'", v.apiKey),
		SSHKey: v.sshKey,
	}); err != nil {
		return providers.InstanceID{}, err
	}

	// Get cached plan ID for the region
	v.regionSizeCacheLock.RLock()
	regionSize, ok := v.regionSizeCache[region]
	if !ok || time.Since(v.regionSizeCacheTime) >= cacheDuration {
		v.regionSizeCacheLock.RUnlock()
		// Cache miss or expired, trigger a refresh
		v.prefetchPrices()
		// Try again after refresh
		v.regionSizeCacheLock.RLock()
		regionSize, ok = v.regionSizeCache[region]
	}
	v.regionSizeCacheLock.RUnlock()

	if !ok || regionSize.PlanID == "" {
		return providers.InstanceID{}, fmt.Errorf("no plans available in region %s", region)
	}

	log.Printf("Using cached plan ID %s for region %s", regionSize.PlanID, region)

	oses, _, _, err := v.vultrClient.OS.List(ctx, nil)
	if err != nil {
		return providers.InstanceID{}, err
	}

	oses = xslices.Filter(oses, func(os govultr.OS) bool {
		return os.Family == "debian" && os.Arch == "x64" && strings.HasPrefix(os.Name, "Debian 12 x64")
	})

	if len(oses) == 0 {
		return providers.InstanceID{}, fmt.Errorf("no OSes available")
	}

	log.Printf("Selected OS: %+v", oses[0])

	instance, _, err := v.vultrClient.Instance.Create(ctx, &govultr.InstanceCreateReq{
		Region:     region,
		Label:      "tscloudvpn",
		Hostname:   hostname,
		Tags:       []string{"tscloudvpn", v.ownerTag},
		Plan:       regionSize.PlanID,
		UserData:   base64.StdEncoding.EncodeToString(tmplOut.Bytes()),
		OsID:       oses[0].ID,
		EnableVPC2: govultr.BoolToBoolPtr(true),
	})
	if err != nil {
		return providers.InstanceID{}, err
	}

	return providers.InstanceID{
		Hostname:     hostname,
		ProviderID:   instance.ID,
		ProviderName: "vultr",
	}, nil
}

func (v *vultrProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	instances, _, _, err := v.vultrClient.Instance.List(ctx, &govultr.ListOptions{
		Region: region,
		Label:  "tscloudvpn",
		Tag:    v.ownerTag,
	})

	if err != nil {
		return providers.InstanceStatusMissing, err
	}

	if len(instances) == 0 {
		return providers.InstanceStatusMissing, nil
	}

	if instances[0].Status == "active" {
		return providers.InstanceStatusRunning, nil
	}

	return providers.InstanceStatusMissing, nil
}

func (v *vultrProvider) ListInstances(ctx context.Context, region string) ([]providers.InstanceID, error) {
	instances, _, _, err := v.vultrClient.Instance.List(ctx, &govultr.ListOptions{
		Region: region,
		Label:  "tscloudvpn",
		Tag:    v.ownerTag,
	})

	if err != nil {
		return nil, err
	}

	var instanceIDs []providers.InstanceID
	for _, instance := range instances {
		createdAt, _ := time.Parse(time.RFC3339, instance.DateCreated)
		instanceIDs = append(instanceIDs, providers.InstanceID{
			Hostname:     vultrInstanceHostname(region),
			ProviderID:   instance.ID,
			ProviderName: providerName,
			CreatedAt:    createdAt,
		})
	}

	return instanceIDs, nil
}

func (v *vultrProvider) Hostname(region string) providers.HostName {
	return providers.HostName(vultrInstanceHostname(region))
}

// GetRegionPrice returns the hourly price for the cheapest plan in the region
func (v *vultrProvider) GetRegionPrice(region string) float64 {
	v.prefetchPrices()

	// Check cache first
	v.regionSizeCacheLock.RLock()
	defer v.regionSizeCacheLock.RUnlock()

	return v.regionSizeCache[region].HourlyCost
}

func (v *vultrProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	v.prefetchPrices()

	regions, _, _, err := v.vultrClient.Region.List(ctx, nil)
	if err != nil {
		return nil, err
	}

	var result []providers.Region
	v.regionSizeCacheLock.RLock()
	defer v.regionSizeCacheLock.RUnlock()

	for _, region := range regions {
		if _, ok := v.regionSizeCache[region.ID]; ok {
			result = append(result, providers.Region{
				LongName: fmt.Sprintf("%s, %s, %s", region.City, region.Country, region.Continent),
				Code:     region.ID,
			})
		}
	}

	// Start prefetching prices in the background

	return result, nil
}

func init() {
	providers.Register(providerName, NewProvider)
}
