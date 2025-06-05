package hetzner

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

const (
	cacheDuration = 24 * time.Hour // Cache prices for 24 hours
)

type regionServerType struct {
	ServerTypeID int64
	Name         string
	HourlyCost   float64
}

type hetznerProvider struct {
	client *hcloud.Client
	apiKey string
	sshKey string

	regionServerTypeCacheLock sync.RWMutex
	regionServerTypeCache     map[string]regionServerType
	regionServerTypeCacheTime time.Time
}

func New(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.Hetzner.Token == "" {
		// No token set. Nothing to do
		return nil, nil
	}

	token := cfg.Providers.Hetzner.Token
	client := hcloud.NewClient(hcloud.WithToken(token))

	return &hetznerProvider{
		client: client,
		apiKey: token,
		sshKey: cfg.SSH.PublicKey,
	}, nil
}

func hetznerInstanceHostname(region string) string {
	return fmt.Sprintf("hetzner-%s", region)
}

func (h *hetznerProvider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	// Ensure region server type cache is populated
	h.regionServerTypeCacheLock.Lock()
	if time.Since(h.regionServerTypeCacheTime) >= cacheDuration || h.regionServerTypeCache == nil {
		var err error
		h.regionServerTypeCache, err = h.loadRegionServerTypes(ctx)
		if err != nil {
			h.regionServerTypeCacheLock.Unlock()
			return providers.InstanceID{}, fmt.Errorf("failed to load region server types: %w", err)
		}
		h.regionServerTypeCacheTime = time.Now()
	}
	h.regionServerTypeCacheLock.Unlock()

	tmplOut := new(bytes.Buffer)
	hostname := hetznerInstanceHostname(region)
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
		OnExit: fmt.Sprintf("curl -X DELETE -H 'Authorization: Bearer %s' https://api.hetzner.cloud/v1/servers/$(curl -s http://169.254.169.254/hetzner/v1/metadata/instance-id)", h.apiKey),
		SSHKey: h.sshKey,
	}); err != nil {
		return providers.InstanceID{}, err
	}

	// Get the cheapest server type for this region
	regionST, ok := h.regionServerTypeCache[region]
	if !ok {
		return providers.InstanceID{}, fmt.Errorf("no server types available for region %s", region)
	}

	// Create server
	createOpts := hcloud.ServerCreateOpts{
		Name:       fmt.Sprintf("tscloudvpn-%s", region),
		ServerType: &hcloud.ServerType{ID: regionST.ServerTypeID},
		Image:      &hcloud.Image{Name: "debian-12"},
		Location:   &hcloud.Location{Name: region},
		UserData:   tmplOut.String(),
	}

	result, _, err := h.client.Server.Create(ctx, createOpts)
	if err != nil {
		return providers.InstanceID{}, fmt.Errorf("failed to create server: %w", err)
	}

	log.Printf("Launched instance %d", result.Server.ID)

	return providers.InstanceID{
		Hostname:     hostname,
		ProviderID:   strconv.FormatInt(result.Server.ID, 10),
		ProviderName: "hetzner",
	}, nil
}

func (h *hetznerProvider) DeleteInstance(ctx context.Context, instanceID providers.InstanceID) error {
	serverID, err := strconv.ParseInt(instanceID.ProviderID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid server ID: %w", err)
	}

	_, _, err = h.client.Server.DeleteWithResult(ctx, &hcloud.Server{ID: serverID})
	if err != nil {
		return fmt.Errorf("failed to delete server: %w", err)
	}

	log.Printf("Deleted instance %d", serverID)
	return nil
}

func (h *hetznerProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	servers, err := h.client.Server.All(ctx)
	if err != nil {
		return providers.InstanceStatusMissing, err
	}

	for _, server := range servers {
		if server.Datacenter.Location.Name == region {
			return providers.InstanceStatusRunning, nil
		}
	}

	return providers.InstanceStatusMissing, nil
}

func (h *hetznerProvider) ListInstances(ctx context.Context, region string) ([]providers.InstanceID, error) {
	servers, err := h.client.Server.All(ctx)
	if err != nil {
		return nil, err
	}

	var instances []providers.InstanceID
	for _, server := range servers {
		if server.Datacenter.Location.Name == region {
			instances = append(instances, providers.InstanceID{
				Hostname:     hetznerInstanceHostname(region),
				ProviderID:   strconv.FormatInt(server.ID, 10),
				ProviderName: "hetzner",
			})
		}
	}

	return instances, nil
}

func (h *hetznerProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	locations, err := h.client.Location.All(ctx)
	if err != nil {
		return nil, err
	}

	var result []providers.Region
	for _, location := range locations {
		result = append(result, providers.Region{
			Code:     location.Name,
			LongName: fmt.Sprintf("%s, %s", location.City, location.Country),
		})
	}
	return result, nil
}

func (h *hetznerProvider) Hostname(region string) providers.HostName {
	return providers.HostName(hetznerInstanceHostname(region))
}

func (h *hetznerProvider) loadRegionServerTypes(ctx context.Context) (map[string]regionServerType, error) {
	serverTypes, err := h.client.ServerType.All(ctx)
	if err != nil {
		return nil, err
	}

	locations, err := h.client.Location.All(ctx)
	if err != nil {
		return nil, err
	}

	result := make(map[string]regionServerType)

	// Sort server types by price (cheapest first)
	// We'll compare prices for the first available location
	sort.Slice(serverTypes, func(i, j int) bool {
		if len(serverTypes[i].Pricings) == 0 || len(serverTypes[j].Pricings) == 0 {
			return len(serverTypes[i].Pricings) > len(serverTypes[j].Pricings)
		}
		priceI, _ := strconv.ParseFloat(serverTypes[i].Pricings[0].Hourly.Net, 64)
		priceJ, _ := strconv.ParseFloat(serverTypes[j].Pricings[0].Hourly.Net, 64)
		return priceI < priceJ
	})

	// For each location, find the cheapest server type available
	for _, location := range locations {
		for _, serverType := range serverTypes {
			// Check if this server type is available in this location
			available := false
			var hourlyPrice float64
			for _, pricing := range serverType.Pricings {
				if pricing.Location.Name == location.Name {
					available = true
					hourlyPrice, _ = strconv.ParseFloat(pricing.Hourly.Net, 64)
					break
				}
			}

			if available {
				// Use the first (cheapest) available server type for this location
				if _, exists := result[location.Name]; !exists {
					result[location.Name] = regionServerType{
						ServerTypeID: serverType.ID,
						Name:         serverType.Name,
						HourlyCost:   hourlyPrice,
					}
				}
			}
		}
	}

	return result, nil
}

// GetRegionPrice returns the hourly price for the cheapest server in the region
func (h *hetznerProvider) GetRegionPrice(region string) float64 {
	h.regionServerTypeCacheLock.Lock()
	defer h.regionServerTypeCacheLock.Unlock()

	// Check cache first
	if time.Since(h.regionServerTypeCacheTime) < cacheDuration && h.regionServerTypeCache != nil {
		if st, ok := h.regionServerTypeCache[region]; ok {
			return st.HourlyCost
		}
		return 0
	}

	// Load cache if needed
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var err error
	h.regionServerTypeCache, err = h.loadRegionServerTypes(ctx)
	if err != nil {
		log.Printf("Failed to load region server types: %v", err)
		return 0
	}

	h.regionServerTypeCacheTime = time.Now()

	if st, ok := h.regionServerTypeCache[region]; ok {
		return st.HourlyCost
	}
	return 0
}

func init() {
	providers.Register("hetzner", New)
}
