package gcp

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"math/rand"
	"net/netip"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/aws/aws-sdk-go-v2/aws"
	cloudbilling "google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

const (
	ubuntuLatestImage   = "projects/ubuntu-os-cloud/global/images/family/ubuntu-2404-lts-amd64"
	providerName        = "gcp"
	cacheDuration       = 24 * time.Hour // Cache machine types for 24 hours
	firewallRuleName    = "tscloudvpn-allow-vpn"
	sshFirewallRuleName = "tscloudvpn-allow-ssh"
	networkTag          = "tscloudvpn"
	// Tailscale uses this port for inbound connections - see https://tailscale.com/kb/1257/connection-types#hard-nat
	tailscaledInboundPort = "41641"
)

var (
	regionLocationMap = map[string]string{
		// From https://cloud.google.com/compute/docs/regions-zones#available
		// There isn't a way to get this info programmatically
		"africa-south1":           "Johannesburg, South Africa",
		"asia-east1":              "Changhua County, Taiwan, APAC",
		"asia-east2":              "Hong Kong, APAC",
		"asia-northeast1":         "Tokyo, Japan, APAC",
		"asia-northeast2":         "Osaka, Japan, APAC",
		"asia-northeast3":         "Seoul, South Korea, APAC",
		"asia-south1":             "Mumbai, India, APAC",
		"asia-south2":             "Delhi, India, APAC",
		"asia-southeast1":         "Jurong West, Singapore, APAC",
		"asia-southeast2":         "Jakarta, Indonesia, APAC",
		"australia-southeast1":    "Sydney, Australia, APAC",
		"australia-southeast2":    "Melbourne, Australia, APAC",
		"europe-central2":         "Warsaw, Poland, Europe",
		"europe-north1":           "Hamina, Finland, Europe",
		"europe-north2":           "Stockholm, Sweden, Europe",
		"europe-southwest1":       "Madrid, Spain, Europe",
		"europe-west1":            "St. Ghislain, Belgium, Europe",
		"europe-west10":           "Berlin, Germany, Europe",
		"europe-west12":           "Turin, Italy, Europe",
		"europe-west2":            "London, England, Europe",
		"europe-west3":            "Frankfurt, Germany, Europe",
		"europe-west4":            "Eemshaven, Netherlands, Europe",
		"europe-west6":            "Zurich, Switzerland, Europe",
		"europe-west8":            "Milan, Italy, Europe",
		"europe-west9":            "Paris, France, Europe",
		"me-central1":             "Doha, Qatar, Middle East",
		"me-central2":             "Dammam, Saudi Arabia, Middle East",
		"me-west1":                "Tel Aviv, Israel, Middle East",
		"northamerica-northeast1": "Montréal, Québec, North America",
		"northamerica-northeast2": "Toronto, Ontario, North America",
		"northamerica-south1":     "Queretaro, Mexico, North America",
		"southamerica-east1":      "Osasco, São Paulo, Brazil, South America",
		"southamerica-west1":      "Santiago, Chile, South America",
		"us-central1":             "Council Bluffs, Iowa, North America",
		"us-east1":                "Moncks Corner, South Carolina, North America",
		"us-east4":                "Ashburn, Virginia, North America",
		"us-east5":                "Columbus, Ohio, North America",
		"us-south1":               "Dallas, Texas, North America",
		"us-west1":                "The Dalles, Oregon, North America",
		"us-west2":                "Los Angeles, California, North America",
		"us-west3":                "Salt Lake City, Utah, North America",
		"us-west4":                "Las Vegas, Nevada, North America",
	}
)

type regionMachineType struct {
	MachineType string
	HourlyCost  float64
}

type gcpProvider struct {
	projectId      string
	serviceAccount string
	service        *compute.Service
	billingService *cloudbilling.APIService
	ownerID        string // Unique identifier for this tscloudvpn instance

	// Cache for per-machine-type per-region pricing from Cloud Billing API
	priceCacheOnce sync.Once
	// machineType -> region -> hourly price
	priceCache map[string]map[string]float64

	// Cache for machine types per region, sorted by price (cheapest first)
	regionMachineTypeCacheLock sync.RWMutex
	regionMachineTypeCache     map[string][]regionMachineType
	regionMachineTypeCacheTime time.Time
}

func NewProvider(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.GCP.CredentialsJSON == "" || cfg.Providers.GCP.ProjectID == "" || cfg.Providers.GCP.ServiceAccount == "" {
		return nil, nil
	}

	credOpt := option.WithCredentialsJSON([]byte(cfg.Providers.GCP.CredentialsJSON))
	service, err := compute.NewService(ctx, credOpt)
	if err != nil {
		return nil, err
	}

	billingService, err := cloudbilling.NewService(ctx, credOpt)
	if err != nil {
		return nil, err
	}

	prov := &gcpProvider{
		projectId:      cfg.Providers.GCP.ProjectID,
		serviceAccount: cfg.Providers.GCP.ServiceAccount,
		service:        service,
		billingService: billingService,
		ownerID:        providers.GetOwnerID(cfg),
	}

	go prov.populatePriceCache()
	go prov.ensureMachineTypeCache()
	return prov, nil
}

func gcpInstanceHostname(region string) string {
	return fmt.Sprintf("gcp-%s", region)
}

func (g *gcpProvider) buildLabels(extra map[string]string) map[string]string {
	labels := map[string]string{
		"tscloudvpn":       "true",
		"tscloudvpn-owner": g.sanitizeLabelValue(g.ownerID),
	}
	for k, v := range extra {
		labels[g.sanitizeLabelValue(k)] = g.sanitizeLabelValue(v)
	}
	return labels
}

// sanitizeLabelValue converts a string to a valid GCP label value
// GCP labels must be lowercase and contain only letters, numbers, underscores, and dashes
func (g *gcpProvider) sanitizeLabelValue(value string) string {
	value = strings.ToLower(value)
	var result strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			result.WriteRune(r)
		} else if r == '.' || r == '@' || r == ' ' {
			result.WriteRune('-')
		}
	}
	return result.String()
}

func (g *gcpProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	regionsList, err := compute.NewRegionsService(g.service).List(g.projectId).Context(ctx).Do()
	if err != nil {
		return nil, err
	}

	var regions []providers.Region
	for _, region := range regionsList.Items {
		longName := region.Name
		if regionLocation, ok := regionLocationMap[region.Name]; ok {
			longName = regionLocation
		}
		regions = append(regions, providers.Region{
			Code:     region.Name,
			LongName: longName,
		})
	}

	sort.Slice(regions, func(i, j int) bool {
		return regions[i].Code < regions[j].Code
	})

	return regions, nil
}

// loadRegionMachineTypes queries GCP for available machine types in each region
// and returns them sorted by price (cheapest first) based on the Cloud Billing API.
func (g *gcpProvider) loadRegionMachineTypes() (map[string][]regionMachineType, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result := make(map[string][]regionMachineType)
	var resultLock sync.Mutex
	var wg sync.WaitGroup

	// Get all regions first
	regionsList, err := compute.NewRegionsService(g.service).List(g.projectId).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to list regions: %w", err)
	}

	// Ensure price cache is populated
	g.populatePriceCache()

	for _, region := range regionsList.Items {
		wg.Add(1)
		region := region
		go func() {
			defer wg.Done()

			// Get zones for this region
			zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, region.Name)).Do()
			if err != nil {
				log.Printf("Failed to list zones for region %s: %v", region.Name, err)
				return
			}

			if len(zones.Items) == 0 {
				return
			}

			// List all machine types available in the first zone
			zone := zones.Items[0].Name
			mtList, err := compute.NewMachineTypesService(g.service).List(g.projectId, zone).Context(ctx).Do()
			if err != nil {
				log.Printf("Failed to list machine types for zone %s: %v", zone, err)
				return
			}

			// Collect all priceable x86_64 machine types
			var available []regionMachineType
			for _, mt := range mtList.Items {
				if mt.Architecture != "" && mt.Architecture != "X86_64" {
					continue
				}
				price := g.computeInstancePrice(mt, region.Name)
				if price == 0 {
					continue // Can't price this machine family
				}
				available = append(available, regionMachineType{
					MachineType: mt.Name,
					HourlyCost:  price,
				})
			}

			// Sort by price, cheapest first
			sort.Slice(available, func(i, j int) bool {
				return available[i].HourlyCost < available[j].HourlyCost
			})

			if len(available) > 0 {
				resultLock.Lock()
				result[region.Name] = available
				resultLock.Unlock()
			}
		}()
	}

	wg.Wait()

	return result, nil
}

// computeEngineServiceID is the well-known Cloud Billing service ID for Compute Engine.
const computeEngineServiceID = "services/6F81-5844-456A"

// perInstanceGroups maps Billing API ResourceGroup values to machine family
// prefixes for legacy types that have per-instance-hour pricing.
var perInstanceGroups = map[string]string{
	"F1Micro": "f1",
	"G1Small": "g1",
}

// skuDescriptionExclusions filters out non-standard VM SKUs that share the
// generic CPU/RAM ResourceGroups but represent different pricing models.
var skuDescriptionExclusions = []string{
	"Sole Tenancy",
	"Custom",
	"DWS",
	"Committed",
	"Reserved",
}

// moneyToFloat64 converts a Cloud Billing Money value to float64.
func moneyToFloat64(m *cloudbilling.Money) float64 {
	if m == nil {
		return 0
	}
	return float64(m.Units) + float64(m.Nanos)/1e9
}

// machineFamily returns the family prefix for a machine type (e.g., "e2" from "e2-micro").
func machineFamily(machineType string) string {
	if idx := strings.Index(machineType, "-"); idx >= 0 {
		return machineType[:idx]
	}
	return machineType
}

// skuFamily extracts a normalized machine family from a Billing API SKU description.
// Examples:
//
//	"E2 Instance Core running in Iowa" → "e2"
//	"N2D AMD Instance Core running in Israel" → "n2d"
//	"N1 Predefined Instance Core running in Doha" → "n1"
//	"C3D Instance Ram running in Singapore" → "c3d"
//
// Returns empty string if the description doesn't match the expected pattern.
func skuFamily(description string) string {
	idx := strings.Index(description, " Instance ")
	if idx < 0 {
		return ""
	}
	// The family is always the first word in the prefix
	parts := strings.Fields(description[:idx])
	if len(parts) == 0 {
		return ""
	}
	return strings.ToLower(parts[0])
}

// populatePriceCache fetches per-unit rates from the Cloud Billing Catalog API.
// Prices are stored keyed by "<family>-cpu", "<family>-ram" for CPU+RAM families,
// or "<family>" for per-instance legacy types (f1-micro, g1-small).
func (g *gcpProvider) populatePriceCache() {
	g.priceCacheOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()

		// priceKey -> region -> hourly rate
		prices := make(map[string]map[string]float64)

		err := cloudbilling.NewServicesSkusService(g.billingService).
			List(computeEngineServiceID).
			CurrencyCode("USD").
			Context(ctx).
			Pages(ctx, func(resp *cloudbilling.ListSkusResponse) error {
				for _, sku := range resp.Skus {
					if sku.Category == nil ||
						sku.Category.ResourceFamily != "Compute" ||
						sku.Category.UsageType != "OnDemand" {
						continue
					}

					// Extract price from tiered rates
					if len(sku.PricingInfo) == 0 ||
						sku.PricingInfo[0].PricingExpression == nil ||
						len(sku.PricingInfo[0].PricingExpression.TieredRates) == 0 {
						continue
					}

					rates := sku.PricingInfo[0].PricingExpression.TieredRates
					unitPrice := moneyToFloat64(rates[len(rates)-1].UnitPrice)
					if unitPrice == 0 {
						continue
					}

					group := sku.Category.ResourceGroup
					var priceKey string

					switch {
					case perInstanceGroups[group] != "":
						// Per-instance legacy types (F1Micro, G1Small)
						priceKey = perInstanceGroups[group]

					case group == "N1Standard":
						// N1 has its own ResourceGroup but uses CPU+RAM pricing
						if strings.Contains(sku.Description, " Core ") {
							priceKey = "n1-cpu"
						} else if strings.Contains(sku.Description, " Ram ") {
							priceKey = "n1-ram"
						} else {
							continue
						}

					case group == "CPU" || group == "RAM":
						// Generic CPU/RAM groups — extract family from Description
						excluded := false
						for _, excl := range skuDescriptionExclusions {
							if strings.Contains(sku.Description, excl) {
								excluded = true
								break
							}
						}
						if excluded {
							continue
						}

						family := skuFamily(sku.Description)
						if family == "" {
							continue
						}
						if group == "CPU" {
							priceKey = family + "-cpu"
						} else {
							priceKey = family + "-ram"
						}

					default:
						continue
					}

					if prices[priceKey] == nil {
						prices[priceKey] = make(map[string]float64)
					}
					for _, region := range sku.ServiceRegions {
						prices[priceKey][region] = unitPrice
					}
				}
				return nil
			})

		if err != nil {
			log.Printf("Failed to fetch GCP pricing: %v", err)
			return
		}

		g.priceCache = prices
		log.Printf("GCP price cache populated with %d price keys", len(g.priceCache))
	})
}

// computeInstancePrice calculates the hourly price for a machine type in a region
// by combining per-unit rates from the Billing API with specs from the Compute API.
func (g *gcpProvider) computeInstancePrice(mt *compute.MachineType, region string) float64 {
	if g.priceCache == nil {
		return 0
	}

	family := machineFamily(mt.Name)

	// Per-instance pricing (f1-micro, g1-small)
	if regions, ok := g.priceCache[family]; ok {
		if price, ok := regions[region]; ok {
			return price
		}
	}

	// CPU+RAM pricing: rates from Billing API, specs from Compute API
	var cpuRate, ramRate float64
	if regions, ok := g.priceCache[family+"-cpu"]; ok {
		cpuRate = regions[region]
	}
	if regions, ok := g.priceCache[family+"-ram"]; ok {
		ramRate = regions[region]
	}
	if cpuRate == 0 && ramRate == 0 {
		return 0
	}

	cpus := float64(mt.GuestCpus)
	ramGB := float64(mt.MemoryMb) / 1024.0
	return cpuRate*cpus + ramRate*ramGB
}

// ensureMachineTypeCache ensures the machine type cache is populated and fresh
func (g *gcpProvider) ensureMachineTypeCache() error {
	g.regionMachineTypeCacheLock.Lock()
	defer g.regionMachineTypeCacheLock.Unlock()

	if time.Since(g.regionMachineTypeCacheTime) < cacheDuration && g.regionMachineTypeCache != nil {
		return nil // Cache is still fresh
	}

	var err error
	g.regionMachineTypeCache, err = g.loadRegionMachineTypes()
	if err != nil {
		return fmt.Errorf("failed to load region machine types: %w", err)
	}

	g.regionMachineTypeCacheTime = time.Now()
	log.Printf("GCP machine type cache populated with %d regions", len(g.regionMachineTypeCache))
	return nil
}

func (g *gcpProvider) DeleteInstance(ctx context.Context, instanceID providers.Instance) error {
	// Extract region from hostname (e.g., "gcp-us-central1" -> "us-central1")
	region := strings.TrimPrefix(instanceID.Hostname, "gcp-")
	zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, region)).Do()
	if err != nil {
		return err
	}

	// Find the zone where the instance exists
	for _, zone := range zones.Items {
		_, err := compute.NewInstancesService(g.service).Get(g.projectId, zone.Name, instanceID.ProviderID).Context(ctx).Do()
		if err == nil {
			// Instance found, delete it
			_, err = compute.NewInstancesService(g.service).Delete(g.projectId, zone.Name, instanceID.ProviderID).Context(ctx).Do()
			if err != nil {
				return fmt.Errorf("failed to delete instance: %w", err)
			}
			log.Printf("Deleted instance %s", instanceID.ProviderID)
			return nil
		}
	}

	return fmt.Errorf("instance not found: %s", instanceID.ProviderID)
}

// getOrCreateFirewallRule ensures a firewall rule exists that allows inbound
// UDP traffic on the Tailscale port for instances tagged with "tscloudvpn".
func (g *gcpProvider) getOrCreateFirewallRule(ctx context.Context) error {
	// Check if rule already exists
	_, err := compute.NewFirewallsService(g.service).Get(g.projectId, firewallRuleName).Context(ctx).Do()
	if err == nil {
		return nil
	}

	// Create the firewall rule
	op, err := compute.NewFirewallsService(g.service).Insert(g.projectId, &compute.Firewall{
		Name:      firewallRuleName,
		Network:   "global/networks/default",
		Direction: "INGRESS",
		Priority:  1000,
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "udp",
				Ports:      []string{tailscaledInboundPort},
			},
		},
		SourceRanges: []string{"0.0.0.0/0"},
		TargetTags:   []string{networkTag},
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to create firewall rule: %w", err)
	}

	// Wait for the operation to complete
	op, err = compute.NewGlobalOperationsService(g.service).Wait(g.projectId, op.Name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed waiting for firewall rule creation: %w", err)
	}
	if op.Error != nil {
		var msgs []string
		for _, e := range op.Error.Errors {
			msgs = append(msgs, fmt.Sprintf("%s: %s", e.Code, e.Message))
		}
		return fmt.Errorf("firewall rule creation failed: %s", strings.Join(msgs, "; "))
	}

	log.Printf("Created GCP firewall rule %s", firewallRuleName)
	return nil
}

func (g *gcpProvider) getOrCreateSSHFirewallRule(ctx context.Context) error {
	_, err := compute.NewFirewallsService(g.service).Get(g.projectId, sshFirewallRuleName).Context(ctx).Do()
	if err == nil {
		return nil
	}

	op, err := compute.NewFirewallsService(g.service).Insert(g.projectId, &compute.Firewall{
		Name:      sshFirewallRuleName,
		Network:   "global/networks/default",
		Direction: "INGRESS",
		Priority:  1000,
		Allowed: []*compute.FirewallAllowed{
			{
				IPProtocol: "tcp",
				Ports:      []string{"22"},
			},
		},
		SourceRanges: []string{"0.0.0.0/0"},
		TargetTags:   []string{networkTag},
	}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed to create SSH firewall rule: %w", err)
	}

	op, err = compute.NewGlobalOperationsService(g.service).Wait(g.projectId, op.Name).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("failed waiting for SSH firewall rule creation: %w", err)
	}
	if op.Error != nil {
		var msgs []string
		for _, e := range op.Error.Errors {
			msgs = append(msgs, fmt.Sprintf("%s: %s", e.Code, e.Message))
		}
		return fmt.Errorf("SSH firewall rule creation failed: %s", strings.Join(msgs, "; "))
	}

	log.Printf("Created GCP SSH firewall rule %s", sshFirewallRuleName)
	return nil
}

func (g *gcpProvider) DebugSSHUser() string { return "root" }

func (g *gcpProvider) GetPublicIP(ctx context.Context, instance providers.Instance) (netip.Addr, error) {
	zone := strings.TrimPrefix(instance.ProviderID, "tscloudvpn-")

	inst, err := compute.NewInstancesService(g.service).Get(g.projectId, zone, instance.ProviderID).Context(ctx).Do()
	if err != nil {
		return netip.Addr{}, fmt.Errorf("failed to get instance: %w", err)
	}

	for _, ni := range inst.NetworkInterfaces {
		for _, ac := range ni.AccessConfigs {
			if ac.NatIP != "" {
				addr, err := netip.ParseAddr(ac.NatIP)
				if err != nil {
					return netip.Addr{}, fmt.Errorf("failed to parse IP %q: %w", ac.NatIP, err)
				}
				return addr, nil
			}
		}
	}
	return netip.Addr{}, fmt.Errorf("no public IP found for instance %s", instance.ProviderID)
}

func (g *gcpProvider) CreateInstance(ctx context.Context, req providers.CreateRequest) (providers.Instance, error) {
	// Ensure machine type cache is populated
	if err := g.ensureMachineTypeCache(); err != nil {
		return providers.Instance{}, fmt.Errorf("failed to ensure machine type cache: %w", err)
	}

	// Get available machine types for this region (sorted cheapest first)
	g.regionMachineTypeCacheLock.RLock()
	machineTypes, ok := g.regionMachineTypeCache[req.Region]
	g.regionMachineTypeCacheLock.RUnlock()

	if !ok || len(machineTypes) == 0 {
		return providers.Instance{}, fmt.Errorf("no suitable machine type found for region %s", req.Region)
	}

	prefix := "https://www.googleapis.com/compute/v1/projects/" + g.projectId
	zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, req.Region)).Do()
	if err != nil {
		return providers.Instance{}, err
	}

	// Ensure firewall rule exists for Tailscale inbound port
	if err := g.getOrCreateFirewallRule(ctx); err != nil {
		return providers.Instance{}, fmt.Errorf("failed to setup firewall rule: %w", err)
	}

	if req.Debug {
		if err := g.getOrCreateSSHFirewallRule(ctx); err != nil {
			log.Printf("Failed to create SSH firewall rule: %v", err)
		}
	}

	zone := zones.Items[rand.Intn(len(zones.Items))].Name
	name := "tscloudvpn-" + zone

	// Try machine types in order of price (cheapest first), falling back on capacity errors
	var lastErr error
	for _, mt := range machineTypes {
		log.Printf("Creating instance in zone %s using machine type %s", zone, mt.MachineType)

		op, err := compute.NewInstancesService(g.service).Insert(g.projectId, zone, &compute.Instance{
			Name:        name,
			MachineType: prefix + "/zones/" + zone + "/machineTypes/" + mt.MachineType,
			Tags: &compute.Tags{
				Items: []string{networkTag},
			},
			Disks: []*compute.AttachedDisk{
				{
					AutoDelete: true,
					Boot:       true,
					InitializeParams: &compute.AttachedDiskInitializeParams{
						SourceImage: ubuntuLatestImage,
					},
				},
			},
			Labels: g.buildLabels(req.Tags),
			NetworkInterfaces: []*compute.NetworkInterface{
				{
					AccessConfigs: []*compute.AccessConfig{
						{
							Name:        "External NAT",
							NetworkTier: "PREMIUM",
						},
					},
					StackType:  "IPV4_ONLY",
					Subnetwork: fmt.Sprintf("projects/%s/regions/%s/subnetworks/default", g.projectId, req.Region),
				},
			},
			ServiceAccounts: []*compute.ServiceAccount{
				{
					Email:  g.serviceAccount,
					Scopes: []string{"https://www.googleapis.com/auth/cloud-platform"},
				},
			},
			Metadata: &compute.Metadata{
				Items: []*compute.MetadataItems{
					{
						Key:   "startup-script",
						Value: aws.String(req.UserData),
					},
				},
			},
		}).Context(ctx).Do()
		if err != nil {
			lastErr = err
			log.Printf("Insert failed in %s with %s: %v, trying next machine type", zone, mt.MachineType, err)
			continue
		}

		// Wait for the operation to complete
		op, err = compute.NewZoneOperationsService(g.service).Wait(g.projectId, zone, op.Name).Context(ctx).Do()
		if err != nil {
			lastErr = fmt.Errorf("failed waiting for instance creation in %s with %s: %w", zone, mt.MachineType, err)
			log.Printf("%v, trying next machine type", lastErr)
			continue
		}
		if op.Error != nil {
			var msgs []string
			for _, e := range op.Error.Errors {
				msgs = append(msgs, fmt.Sprintf("%s: %s", e.Code, e.Message))
			}
			lastErr = fmt.Errorf("instance creation failed in %s with %s: %s", zone, mt.MachineType, strings.Join(msgs, "; "))
			log.Printf("%v, trying next machine type", lastErr)
			continue
		}

		// Success
		log.Printf("Launched instance %s (machine type: %s)", name, mt.MachineType)
		return providers.Instance{
			Hostname:     req.Hostname,
			ProviderID:   name,
			ProviderName: providerName,
			HourlyCost:   mt.HourlyCost,
		}, nil
	}

	return providers.Instance{}, fmt.Errorf("all machine types exhausted in %s: %w", zone, lastErr)
}

func (g *gcpProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, region)).Do()
	if err != nil {
		return providers.InstanceStatusMissing, err
	}

	sanitizedOwnerID := g.sanitizeLabelValue(g.ownerID)
	var instances []*compute.Instance
	for _, zone := range zones.Items {
		filter := fmt.Sprintf("labels.tscloudvpn:* AND labels.tscloudvpn-owner=%s", sanitizedOwnerID)
		instanceList, err := compute.NewInstancesService(g.service).List(g.projectId, zone.Name).Filter(filter).Context(ctx).Do()
		if err != nil {
			return providers.InstanceStatusMissing, err
		}

		instances = append(instances, instanceList.Items...)
	}

	slices.SortFunc(instances, func(i, j *compute.Instance) int {
		iTime, err := time.Parse(time.RFC3339, i.CreationTimestamp)
		if err != nil {
			return 0
		}
		jTime, err := time.Parse(time.RFC3339, j.CreationTimestamp)
		if err != nil {
			return 0
		}
		return iTime.Compare(jTime)
	})

	for _, instance := range instances {
		if instance.Status != "TERMINATED" {
			return providers.InstanceStatusRunning, nil
		}
	}

	return providers.InstanceStatusMissing, err
}

func (g *gcpProvider) ListInstances(ctx context.Context, region string) ([]providers.Instance, error) {
	// Ensure price cache is available for HourlyCost lookups
	g.ensureMachineTypeCache()

	zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, region)).Do()
	if err != nil {
		return nil, err
	}

	sanitizedOwnerID := g.sanitizeLabelValue(g.ownerID)
	var instanceIDs []providers.Instance
	for _, zone := range zones.Items {
		// Filter by both tscloudvpn label and owner label
		filter := fmt.Sprintf("labels.tscloudvpn:* AND labels.tscloudvpn-owner=%s", sanitizedOwnerID)
		instanceList, err := compute.NewInstancesService(g.service).List(g.projectId, zone.Name).Filter(filter).Context(ctx).Do()
		if err != nil {
			return nil, err
		}

		for _, instance := range instanceList.Items {
			if instance.Status != "TERMINATED" {
				hostname := gcpInstanceHostname(region)
				if name, ok := instance.Labels[g.sanitizeLabelValue(providers.NameTagKey)]; ok {
					hostname = name
				}
				createdAt, _ := time.Parse(time.RFC3339, instance.CreationTimestamp)
				instanceIDs = append(instanceIDs, providers.Instance{
					Hostname:     hostname,
					ProviderID:   instance.Name,
					ProviderName: providerName,
					CreatedAt:    createdAt,
					HourlyCost:   g.lookupMachineTypeCost(instance.MachineType, region),
				})
			}
		}
	}

	return instanceIDs, nil
}

func zoneToRegion(zone string) string {
	// "us-central1-a" -> "us-central1"
	lastDash := strings.LastIndex(zone, "-")
	if lastDash < 0 {
		return zone
	}
	return zone[:lastDash]
}

// lookupMachineTypeCost returns the hourly cost for a machine type URL in a region.
// machineTypeURL is a full URL like ".../zones/us-central1-a/machineTypes/e2-small".
func (g *gcpProvider) lookupMachineTypeCost(machineTypeURL, region string) float64 {
	// Extract machine type name from URL (last path segment)
	idx := strings.LastIndex(machineTypeURL, "/")
	if idx < 0 {
		return 0
	}
	mtName := machineTypeURL[idx+1:]

	g.regionMachineTypeCacheLock.RLock()
	defer g.regionMachineTypeCacheLock.RUnlock()

	for _, mt := range g.regionMachineTypeCache[region] {
		if mt.MachineType == mtName {
			return mt.HourlyCost
		}
	}
	return 0
}

func (g *gcpProvider) ListAllInstances(ctx context.Context) ([]providers.Instance, error) {
	// Ensure price cache is available for HourlyCost lookups
	g.ensureMachineTypeCache()

	sanitizedOwnerID := g.sanitizeLabelValue(g.ownerID)
	filter := fmt.Sprintf("labels.tscloudvpn:* AND labels.tscloudvpn-owner=%s", sanitizedOwnerID)

	resp, err := compute.NewInstancesService(g.service).
		AggregatedList(g.projectId).
		Filter(filter).
		ReturnPartialSuccess(true).
		Context(ctx).
		Do()
	if err != nil {
		return nil, err
	}

	var instanceIDs []providers.Instance
	for zonePath, scopedList := range resp.Items {
		for _, instance := range scopedList.Instances {
			if instance.Status != "TERMINATED" {
				// zonePath is "zones/us-central1-a"
				zone := strings.TrimPrefix(zonePath, "zones/")
				region := zoneToRegion(zone)
				createdAt, _ := time.Parse(time.RFC3339, instance.CreationTimestamp)
				instanceIDs = append(instanceIDs, providers.Instance{
					Hostname:     gcpInstanceHostname(region),
					ProviderID:   instance.Name,
					ProviderName: providerName,
					CreatedAt:    createdAt,
					HourlyCost:   g.lookupMachineTypeCost(instance.MachineType, region),
				})
			}
		}
	}

	return instanceIDs, nil
}

// GetRegionHourlyEstimate returns the hourly price for the cheapest available instance in the specified region
func (g *gcpProvider) GetRegionHourlyEstimate(region string) float64 {
	// Ensure machine type cache is populated
	if err := g.ensureMachineTypeCache(); err != nil {
		log.Printf("Failed to ensure machine type cache: %v", err)
		return 0.0084 // Return default e2-micro price as fallback
	}

	g.regionMachineTypeCacheLock.RLock()
	defer g.regionMachineTypeCacheLock.RUnlock()

	if types, ok := g.regionMachineTypeCache[region]; ok && len(types) > 0 {
		return types[0].HourlyCost
	}

	// Default price if region not found in cache
	return 0
}

func init() {
	providers.Register(providerName, "Google Cloud", NewProvider)
}
