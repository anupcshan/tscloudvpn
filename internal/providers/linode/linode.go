package linode

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"net/netip"
	"sort"
	"strconv"
	"sync"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/linode/linodego"
	"golang.org/x/oauth2"
	"math/rand/v2"
)

// instanceTypeSpec holds the specs and pricing for one instance type.
type instanceTypeSpec struct {
	Name       string
	VCPUs      int
	RamMB      int
	DiskMB     int
	HourlyCost float64
}

type linodeProvider struct {
	client              *linodego.Client
	ownerID             string // Unique identifier for this tscloudvpn instance
	ownerTag            string // Tag combining owner key and value for filtering
	listTypesOnce       sync.Once
	regionInstanceTypes map[string][]instanceTypeSpec // region -> sorted by cost (cheapest first)
}

func New(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.Linode.Token == "" {
		// No token set. Nothing to do
		return nil, nil
	}

	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Providers.Linode.Token})
	oauth2Client := oauth2.NewClient(ctx, tokenSource)
	client := linodego.NewClient(oauth2Client)
	ownerID := providers.GetOwnerID(cfg)

	p := &linodeProvider{
		client:   &client,
		ownerID:  ownerID,
		ownerTag: fmt.Sprintf("%s:%s", providers.OwnerTagKey, ownerID),
	}

	go p.populateTypeCache()
	return p, nil
}

func linodeInstanceHostname(region string) string {
	return fmt.Sprintf("linode-%s", region)
}

func (l *linodeProvider) buildTags(extra map[string]string) []string {
	tags := []string{"tscloudvpn", l.ownerTag}
	for k, v := range extra {
		tags = append(tags, fmt.Sprintf("%s:%s", k, v))
	}
	return tags
}

func (l *linodeProvider) DeleteInstance(ctx context.Context, instanceID providers.Instance) error {
	linodeID, err := strconv.Atoi(instanceID.ProviderID)
	if err != nil {
		return fmt.Errorf("invalid Linode ID: %w", err)
	}

	err = l.client.DeleteInstance(ctx, linodeID)
	if err != nil {
		return fmt.Errorf("failed to delete Linode instance: %w", err)
	}

	log.Printf("Deleted Linode instance %d", linodeID)
	return nil
}

func (l *linodeProvider) CreateInstance(ctx context.Context, req providers.CreateRequest) (providers.Instance, error) {
	createOpts := linodego.InstanceCreateOptions{
		Label:    fmt.Sprintf("tscloudvpn-%s", req.Region),
		Region:   req.Region,
		Type:     l.cheapestInstanceType(req.Region),
		Image:    "linode/ubuntu24.04",
		RootPass: generateRandomPassword(),
		Tags:     l.buildTags(req.Tags),
		Metadata: &linodego.InstanceMetadataOptions{
			UserData: base64.StdEncoding.EncodeToString([]byte(req.UserData)),
		},
	}

	instance, err := l.client.CreateInstance(ctx, createOpts)
	if err != nil {
		return providers.Instance{}, fmt.Errorf("failed to create Linode instance: %w", err)
	}

	log.Printf("Launched Linode instance %d", instance.ID)

	return providers.Instance{
		Hostname:     req.Hostname,
		ProviderID:   strconv.Itoa(instance.ID),
		ProviderName: "linode",
		Region:       req.Region,
		HourlyCost:   l.GetRegionHourlyEstimate(req.Region),
	}, nil
}

func (l *linodeProvider) DebugSSHUser() string { return "root" }

func (l *linodeProvider) GetPublicIP(ctx context.Context, instance providers.Instance) (netip.Addr, error) {
	linodeID, err := strconv.Atoi(instance.ProviderID)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid Linode ID: %w", err)
	}

	inst, err := l.client.GetInstance(ctx, linodeID)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to get instance: %w", err)
	}

	if len(inst.IPv4) == 0 {
		return netip.Addr{}, fmt.Errorf("no IPv4 addresses for instance %d", linodeID)
	}
	addr, err := netip.ParseAddr(inst.IPv4[0].String())
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to parse IP %v: %w", inst.IPv4[0], err)
	}
	return addr, nil
}

func (l *linodeProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	// Filter instances by our owner tag
	filter := fmt.Sprintf(`{"tags": "%s"}`, l.ownerTag)
	instances, err := l.client.ListInstances(ctx, linodego.NewListOptions(0, filter))
	if err != nil {
		return providers.InstanceStatusMissing, err
	}

	for _, instance := range instances {
		if instance.Region == region {
			return providers.InstanceStatusRunning, nil
		}
	}

	return providers.InstanceStatusMissing, nil
}

func (l *linodeProvider) ListInstances(ctx context.Context, region string) ([]providers.Instance, error) {
	// Filter instances by our owner tag
	filter := fmt.Sprintf(`{"tags": "%s"}`, l.ownerTag)
	instances, err := l.client.ListInstances(ctx, linodego.NewListOptions(0, filter))
	if err != nil {
		return nil, err
	}

	var instanceIDs []providers.Instance
	for _, instance := range instances {
		if instance.Region == region {
			instanceIDs = append(instanceIDs, providers.Instance{
				Hostname:     providers.ExtractInstanceName(instance.Tags, linodeInstanceHostname(region)),
				ProviderID:   strconv.Itoa(instance.ID),
				ProviderName: "linode",
				Region:       region,
				CreatedAt:    *instance.Created,
				HourlyCost:   l.GetRegionHourlyEstimate(region),
			})
		}
	}

	return instanceIDs, nil
}

func (l *linodeProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	regions, err := l.client.ListRegions(ctx, nil)
	if err != nil {
		return nil, err
	}

	var result []providers.Region
	for _, r := range regions {
		if r.Status != "ok" {
			continue
		}
		result = append(result, providers.Region{
			Code:     r.ID,
			LongName: r.Label,
		})
	}
	return result, nil
}

// populateTypeCache fetches all Linode instance types and builds a per-region
// list sorted by cost. Called once on first use.
func (l *linodeProvider) populateTypeCache() {
	l.listTypesOnce.Do(func() {
		ctx := context.Background()

		types, err := l.client.ListTypes(ctx, nil)
		if err != nil {
			log.Printf("Error listing Linode types: %v", err)
			return
		}

		regions, err := l.client.ListRegions(ctx, nil)
		if err != nil {
			log.Printf("Error listing Linode regions for type cache: %v", err)
			return
		}

		regionTypes := make(map[string][]instanceTypeSpec)

		for _, region := range regions {
			if region.Status != "ok" {
				continue
			}
			for _, t := range types {
				if t.Price == nil {
					continue
				}
				price := float64(t.Price.Hourly)
				for _, rp := range t.RegionPrices {
					if rp.ID == region.ID {
						price = float64(rp.Hourly)
						break
					}
				}
				regionTypes[region.ID] = append(regionTypes[region.ID], instanceTypeSpec{
					Name:       t.ID,
					VCPUs:      t.VCPUs,
					RamMB:      t.Memory,
					DiskMB:     t.Disk,
					HourlyCost: price,
				})
			}
		}

		for region := range regionTypes {
			sort.Slice(regionTypes[region], func(i, j int) bool {
				return regionTypes[region][i].HourlyCost < regionTypes[region][j].HourlyCost
			})
		}

		l.regionInstanceTypes = regionTypes
		entries := 0
		for _, v := range regionTypes {
			entries += len(v)
		}
		log.Printf("Linode type cache populated: %d regions, %d entries", len(regionTypes), entries)
	})
}

// GetRegionHourlyEstimate returns the hourly price for the cheapest instance type in the region.
func (l *linodeProvider) GetRegionHourlyEstimate(region string) float64 {
	l.populateTypeCache()

	specs := l.regionInstanceTypes[region]
	if len(specs) == 0 {
		return 0
	}
	return specs[0].HourlyCost
}

// cheapestInstanceType returns the cheapest instance type ID for a region.
func (l *linodeProvider) cheapestInstanceType(region string) string {
	l.populateTypeCache()

	specs := l.regionInstanceTypes[region]
	if len(specs) == 0 {
		return "g6-nanode-1"
	}
	return specs[0].Name
}

func generateRandomPassword() string {
	// Generate a random password
	charset := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*()_+-=[]{}|;:,.<>?"
	length := 16
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.IntN(len(charset))]
	}
	return string(b)
}

func init() {
	providers.Register("linode", "Linode", New)
}
