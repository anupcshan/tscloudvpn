package hetzner

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
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
	client  *hcloud.Client
	ownerID string // Unique identifier for this tscloudvpn instance

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

	// Sanitize ownerID for Hetzner labels (only alphanumeric, hyphens, underscores allowed)
	ownerID := sanitizeLabelValue(providers.GetOwnerID(cfg))

	return &hetznerProvider{
		client:  client,
		ownerID: ownerID,
	}, nil
}

// sanitizeLabelValue converts a string to a valid Hetzner label value
// Hetzner labels only allow alphanumeric characters, hyphens, and underscores
func sanitizeLabelValue(s string) string {
	var result strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			result.WriteRune(r)
		} else {
			// Replace invalid characters with underscore
			result.WriteRune('_')
		}
	}
	return result.String()
}

func hetznerInstanceHostname(region string) string {
	return fmt.Sprintf("hetzner-%s", region)
}

func (h *hetznerProvider) CreateInstance(ctx context.Context, req providers.CreateRequest) (providers.Instance, error) {
	// Ensure region server type cache is populated
	h.regionServerTypeCacheLock.Lock()
	if time.Since(h.regionServerTypeCacheTime) >= cacheDuration || h.regionServerTypeCache == nil {
		var err error
		h.regionServerTypeCache, err = h.loadRegionServerTypes(ctx)
		if err != nil {
			h.regionServerTypeCacheLock.Unlock()
			return providers.Instance{}, fmt.Errorf("failed to load region server types: %w", err)
		}
		h.regionServerTypeCacheTime = time.Now()
	}
	h.regionServerTypeCacheLock.Unlock()

	hostname := hetznerInstanceHostname(req.Region)

	// Get the cheapest server type for this region
	regionST, ok := h.regionServerTypeCache[req.Region]
	if !ok {
		return providers.Instance{}, fmt.Errorf("no server types available for region %s", req.Region)
	}

	// Create server
	createOpts := hcloud.ServerCreateOpts{
		Name:       fmt.Sprintf("tscloudvpn-%s", req.Region),
		ServerType: &hcloud.ServerType{ID: regionST.ServerTypeID},
		Image:      &hcloud.Image{Name: "ubuntu-24.04"},
		Location:   &hcloud.Location{Name: req.Region},
		UserData:   req.UserData,
		Labels: map[string]string{
			"tscloudvpn":          "true",
			providers.OwnerTagKey: h.ownerID,
		},
	}

	result, _, err := h.client.Server.Create(ctx, createOpts)
	if err != nil {
		return providers.Instance{}, fmt.Errorf("failed to create server: %w", err)
	}

	log.Printf("Launched instance %d", result.Server.ID)

	return providers.Instance{
		Hostname:     hostname,
		ProviderID:   strconv.FormatInt(result.Server.ID, 10),
		ProviderName: "hetzner",
		HourlyCost:   regionST.HourlyCost,
	}, nil
}

func (h *hetznerProvider) DebugSSHUser() string { return "root" }

func (h *hetznerProvider) GetPublicIP(ctx context.Context, instance providers.Instance) (netip.Addr, error) {
	serverID, err := strconv.ParseInt(instance.ProviderID, 10, 64)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid server ID: %w", err)
	}

	server, _, err := h.client.Server.GetByID(ctx, serverID)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to get server: %w", err)
	}
	if server.PublicNet.IPv4.IP.IsUnspecified() {
		return netip.Addr{}, fmt.Errorf("no public IPv4 for server %d", serverID)
	}
	addr, ok := netip.AddrFromSlice(server.PublicNet.IPv4.IP)
	if !ok {
		return netip.Addr{}, fmt.Errorf("failed to parse IP %v", server.PublicNet.IPv4.IP)
	}
	return addr, nil
}

func (h *hetznerProvider) DeleteInstance(ctx context.Context, instanceID providers.Instance) error {
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
	// Filter servers by our owner label
	servers, err := h.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf("tscloudvpn,%s=%s", providers.OwnerTagKey, h.ownerID),
		},
	})
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

func (h *hetznerProvider) ListInstances(ctx context.Context, region string) ([]providers.Instance, error) {
	// Filter servers by our owner label
	servers, err := h.client.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf("tscloudvpn,%s=%s", providers.OwnerTagKey, h.ownerID),
		},
	})
	if err != nil {
		return nil, err
	}

	var instances []providers.Instance
	for _, server := range servers {
		if server.Datacenter.Location.Name == region {
			instances = append(instances, providers.Instance{
				Hostname:     hetznerInstanceHostname(region),
				ProviderID:   strconv.FormatInt(server.ID, 10),
				ProviderName: "hetzner",
				CreatedAt:    server.Created,
				HourlyCost:   h.GetRegionHourlyEstimate(region),
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

// GetRegionHourlyEstimate returns the hourly price for the cheapest server in the region
func (h *hetznerProvider) GetRegionHourlyEstimate(region string) float64 {
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
	providers.Register("hetzner", "Hetzner", New)
}
