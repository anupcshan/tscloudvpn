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
	"github.com/bradenaw/juniper/xslices"
	"github.com/vultr/govultr/v3"
	"golang.org/x/oauth2"
)

const (
	providerName  = "vultr"
	cacheDuration = 24 * time.Hour // Cache prices for 24 hours
)

type vultrProvider struct {
	vultrClient     *govultr.Client
	apiKey          string
	sshKey          string
	priceCacheMutex sync.RWMutex
	priceCache      map[string]float64
	priceCacheTime  time.Time
}

func NewProvider(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.Vultr.APIKey == "" {
		return nil, nil
	}

	config := &oauth2.Config{}
	ts := config.TokenSource(ctx, &oauth2.Token{AccessToken: cfg.Providers.Vultr.APIKey})
	vultrClient := govultr.NewClient(oauth2.NewClient(ctx, ts))

	return &vultrProvider{
		vultrClient: vultrClient,
		apiKey:      cfg.Providers.Vultr.APIKey,
		sshKey:      cfg.SSH.PublicKey,
		priceCache:  make(map[string]float64),
	}, nil
}

// prefetchPrices loads all region prices to warm up the cache
func (v *vultrProvider) prefetchPrices() {
	// Only prefetch if cache is empty or expired
	v.priceCacheMutex.RLock()
	shouldPrefetch := len(v.priceCache) == 0 || time.Since(v.priceCacheTime) >= cacheDuration
	v.priceCacheMutex.RUnlock()

	if !shouldPrefetch {
		return
	}

	v.priceCacheMutex.Lock()
	defer v.priceCacheMutex.Unlock()

	shouldPrefetch = len(v.priceCache) == 0 || time.Since(v.priceCacheTime) >= cacheDuration
	if !shouldPrefetch {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First get all regions
	regions, err := v.ListRegions(ctx)
	if err != nil {
		log.Printf("Failed to prefetch Vultr regions: %v", err)
		return
	}

	// Now fetch all plans once (efficient)
	plans, _, _, err := v.vultrClient.Plan.List(ctx, "vc2", &govultr.ListOptions{
		PerPage: 500,
	})
	if err != nil {
		log.Printf("Failed to prefetch Vultr plans: %v", err)
		return
	}

	// Clear existing cache
	v.priceCache = make(map[string]float64)
	v.priceCacheTime = time.Now()

	// Process each region
	for _, region := range regions {
		regionCode := region.Code

		// Find valid plans for this region
		validPlans := xslices.Filter(plans, func(plan govultr.Plan) bool {
			return xslices.Index(plan.Locations, regionCode) != -1
		})

		if len(validPlans) > 0 {
			// Sort to find cheapest plan
			sort.Slice(validPlans, func(i, j int) bool {
				return validPlans[i].MonthlyCost < validPlans[j].MonthlyCost
			})

			// Calculate hourly price
			v.priceCache[regionCode] = float64(validPlans[0].MonthlyCost) / 30.0 / 24.0
		} else {
			// Use fallback price if no plans found
			v.priceCache[regionCode] = 0.005
		}
	}

	log.Printf("Vultr price cache populated with %d regions", len(v.priceCache))
}

func vultrInstanceHostname(region string) string {
	return fmt.Sprintf("vultr-%s", region)
}

func (v *vultrProvider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (string, error) {
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
		return "", err
	}

	plans, _, _, err := v.vultrClient.Plan.List(ctx, "vc2", &govultr.ListOptions{
		PerPage: 500,
	})
	if err != nil {
		return "", err
	}

	plans = xslices.Filter(plans, func(plan govultr.Plan) bool {
		return xslices.Index(plan.Locations, region) != -1
	})

	if len(plans) == 0 {
		return "", fmt.Errorf("no plans available in region %s", region)
	}

	sort.Slice(plans, func(i, j int) bool {
		return plans[i].MonthlyCost < plans[j].MonthlyCost
	})

	log.Printf("Lowest cost plan: %+v", plans[0])

	oses, _, _, err := v.vultrClient.OS.List(ctx, nil)
	if err != nil {
		return "", err
	}

	oses = xslices.Filter(oses, func(os govultr.OS) bool {
		return os.Family == "debian" && os.Arch == "x64" && strings.HasPrefix(os.Name, "Debian 12 x64")
	})

	if len(oses) == 0 {
		return "", fmt.Errorf("no OSes available")
	}

	log.Printf("Selected OS: %+v", oses[0])

	instance, _, err := v.vultrClient.Instance.Create(ctx, &govultr.InstanceCreateReq{
		Region:     region,
		Label:      "tscloudvpn",
		Hostname:   hostname,
		Tags:       []string{"tscloudvpn"},
		Plan:       plans[0].ID,
		UserData:   base64.StdEncoding.EncodeToString(tmplOut.Bytes()),
		OsID:       oses[0].ID,
		EnableVPC2: govultr.BoolToBoolPtr(true),
	})
	if err != nil {
		return "", err
	}
	_ = instance

	return hostname, nil
}

func (v *vultrProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	instances, _, _, err := v.vultrClient.Instance.List(ctx, &govultr.ListOptions{
		Region: region,
		Label:  "tscloudvpn",
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

func (v *vultrProvider) Hostname(region string) providers.HostName {
	return providers.HostName(vultrInstanceHostname(region))
}

// GetRegionPrice returns the hourly price for the cheapest plan in the region
func (v *vultrProvider) GetRegionPrice(region string) float64 {
	// Check cache first
	v.priceCacheMutex.RLock()
	if price, ok := v.priceCache[region]; ok {
		// If cache is still valid (not expired)
		if time.Since(v.priceCacheTime) < cacheDuration {
			v.priceCacheMutex.RUnlock()
			return price
		}
	}
	v.priceCacheMutex.RUnlock()

	// Cache miss or expired, fetch prices from API
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plans, _, _, err := v.vultrClient.Plan.List(ctx, "vc2", &govultr.ListOptions{
		PerPage: 500,
	})

	// Lock for writing to cache
	v.priceCacheMutex.Lock()
	defer v.priceCacheMutex.Unlock()

	// Check if we need to refresh the entire cache
	if time.Since(v.priceCacheTime) >= cacheDuration {
		// Clear the cache if it's expired
		v.priceCache = make(map[string]float64)
	}

	if err == nil {
		// Filter plans available in this region
		validPlans := xslices.Filter(plans, func(plan govultr.Plan) bool {
			return xslices.Index(plan.Locations, region) != -1
		})

		if len(validPlans) > 0 {
			// Find the cheapest plan
			sort.Slice(validPlans, func(i, j int) bool {
				return validPlans[i].MonthlyCost < validPlans[j].MonthlyCost
			})

			// Convert monthly cost to hourly (MonthlyCost is in dollars as float32)
			price := float64(validPlans[0].MonthlyCost) / 30.0 / 24.0 // Approximate hourly rate

			// Update cache
			v.priceCache[region] = price
			v.priceCacheTime = time.Now()

			return price
		}
	}

	// If not in cache and API failed, use fallback price
	fallbackPrice := 0.005 // Standard hourly rate for $3.50/month plan
	v.priceCache[region] = fallbackPrice
	if v.priceCacheTime.IsZero() {
		v.priceCacheTime = time.Now()
	}

	return fallbackPrice
}

func (v *vultrProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	regions, _, _, err := v.vultrClient.Region.List(ctx, nil)
	if err != nil {
		return nil, err
	}

	var result []providers.Region
	for _, region := range regions {
		result = append(result, providers.Region{
			LongName: fmt.Sprintf("%s, %s, %s", region.City, region.Country, region.Continent),
			Code:     region.ID,
		})
	}

	// Start prefetching prices in the background
	go v.prefetchPrices()

	return result, nil
}

func init() {
	providers.Register(providerName, NewProvider)
}
