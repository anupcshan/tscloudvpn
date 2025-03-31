package digitalocean

import (
	"bytes"
	"context"
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

	"github.com/digitalocean/godo"
)

const (
	cacheDuration = 24 * time.Hour // Cache prices for 24 hours
)

type regionSize struct {
	SizeSlug   string
	HourlyCost float64
}

type digitaloceanProvider struct {
	client *godo.Client
	token  string
	sshKey string

	regionSizeCacheLock sync.RWMutex
	regionSizeCache     map[string]regionSize
	regionSizeCacheTime time.Time
}

func New(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.DigitalOcean.Token == "" {
		// No token set. Nothing to do
		return nil, nil
	}

	token := cfg.Providers.DigitalOcean.Token
	client := godo.NewFromToken(token)

	return &digitaloceanProvider{
		client: client,
		sshKey: cfg.SSH.PublicKey,
		token:  token,
	}, nil
}

func doInstanceHostname(region string) string {
	return fmt.Sprintf("do-%s", region)
}

func (d *digitaloceanProvider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (string, error) {
	tmplOut := new(bytes.Buffer)
	hostname := doInstanceHostname(region)
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
		OnExit: fmt.Sprintf("curl https://api.digitalocean.com/v2/droplets/$(curl -s http://169.254.169.254/metadata/v1/id) -X DELETE -H 'Authorization: Bearer %s'", d.token),
		SSHKey: d.sshKey,
	}); err != nil {
		return "", err
	}

	createRequest := &godo.DropletCreateRequest{
		Name:   fmt.Sprintf("tscloudvpn-%s", region),
		Region: region,
		Size:   "s-1vcpu-1gb", // TODO: Change this to use the cheapest size from regionSizeCache
		Image: godo.DropletCreateImage{
			Slug: "debian-12-x64",
		},
		UserData: tmplOut.String(),
	}

	droplet, _, err := d.client.Droplets.Create(ctx, createRequest)
	if err != nil {
		return "", fmt.Errorf("failed to create droplet: %w", err)
	}

	log.Printf("Launched instance %d", droplet.ID)

	return hostname, nil
}

func (d *digitaloceanProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	droplets, _, err := d.client.Droplets.List(ctx, &godo.ListOptions{})
	if err != nil {
		return providers.InstanceStatusMissing, err
	}

	for _, d := range droplets {
		if d.Region.Slug == region {
			return providers.InstanceStatusRunning, nil
		}
	}

	return providers.InstanceStatusMissing, nil
}

func (d *digitaloceanProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	regions, _, err := d.client.Regions.List(ctx, &godo.ListOptions{})
	if err != nil {
		return nil, err
	}

	var result []providers.Region
	for _, r := range regions {
		if !r.Available {
			continue
		}
		result = append(result, providers.Region{
			Code:     r.Slug,
			LongName: r.Name,
		})
	}
	return result, nil
}

func (d *digitaloceanProvider) Hostname(region string) providers.HostName {
	return providers.HostName(doInstanceHostname(region))
}

func (d *digitaloceanProvider) loadRegionSizes() (map[string]regionSize, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sizes, _, err := d.client.Sizes.List(ctx, &godo.ListOptions{
		PerPage: 200,
	})
	if err != nil {
		return nil, err
	}

	result := make(map[string]regionSize)
	sort.Slice(sizes, func(i, j int) bool {
		return sizes[i].PriceHourly < sizes[j].PriceHourly
	})

	for _, size := range sizes {
		for _, region := range size.Regions {
			if _, ok := result[region]; ok {
				continue
			}

			result[region] = regionSize{
				SizeSlug:   size.Slug,
				HourlyCost: size.PriceHourly,
			}
		}
	}
	return result, nil
}

// GetRegionPrice returns the hourly price for the cheapest instance
func (d *digitaloceanProvider) GetRegionPrice(region string) float64 {
	d.regionSizeCacheLock.Lock()
	defer d.regionSizeCacheLock.Unlock()

	// Check cache first
	if time.Since(d.regionSizeCacheTime) < cacheDuration && d.regionSizeCache != nil {
		return d.regionSizeCache[region].HourlyCost
	}

	var err error
	d.regionSizeCache, err = d.loadRegionSizes()
	if err != nil {
		log.Printf("Failed to load region sizes: %v", err)
		return 0
	}

	d.regionSizeCacheTime = time.Now()

	return d.regionSizeCache[region].HourlyCost
}

func init() {
	providers.Register("do", New)
}
