package gcp

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/aws/aws-sdk-go-v2/aws"
	"google.golang.org/api/compute/v1"
	"google.golang.org/api/option"
)

const (
	debianLatestImage = "projects/debian-cloud/global/images/family/debian-12"
	providerName      = "gcp"
	cacheDuration     = 24 * time.Hour // Cache machine types for 24 hours
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
	sshKey         string
	ownerID        string // Unique identifier for this tscloudvpn instance

	// Cache for machine types per region
	regionMachineTypeCacheLock sync.RWMutex
	regionMachineTypeCache     map[string]regionMachineType
	regionMachineTypeCacheTime time.Time
}

func NewProvider(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.GCP.CredentialsJSON == "" || cfg.Providers.GCP.ProjectID == "" || cfg.Providers.GCP.ServiceAccount == "" {
		return nil, nil
	}

	service, err := compute.NewService(ctx, option.WithCredentialsJSON([]byte(cfg.Providers.GCP.CredentialsJSON)))
	if err != nil {
		return nil, err
	}

	prov := &gcpProvider{
		projectId:      cfg.Providers.GCP.ProjectID,
		serviceAccount: cfg.Providers.GCP.ServiceAccount,
		service:        service,
		sshKey:         cfg.SSH.PublicKey,
		ownerID:        providers.GetOwnerID(cfg),
	}

	go prov.ensureMachineTypeCache()
	return prov, nil
}

func gcpInstanceHostname(region string) string {
	return fmt.Sprintf("gcp-%s", region)
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
// and determines the cheapest option. This replaces the hardcoded f1-micro.
func (g *gcpProvider) loadRegionMachineTypes() (map[string]regionMachineType, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := make(map[string]regionMachineType)
	var resultLock sync.Mutex
	var wg sync.WaitGroup

	// Get all regions first
	regionsList, err := compute.NewRegionsService(g.service).List(g.projectId).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to list regions: %w", err)
	}

	// Common cheap machine types to try, in order of preference (cheapest first)
	// These are the typical low-cost options available across GCP regions
	cheapMachineTypes := []string{
		"f1-micro",      // Legacy option, not available in all regions
		"e2-micro",      // Cheapest, non-legacy shared-core option
		"g1-small",      // Legacy option
		"e2-small",      // Slightly more expensive but widely available
		"n1-standard-1", // Fallback option
	}

	// Hardcoded pricing map as GCP Billing API requires special setup
	// In production, consider using the Cloud Billing API for real-time pricing
	// https://cloud.google.com/billing/v1/how-tos/catalog-api
	machineTypePricing := map[string]float64{
		"f1-micro":      0.0076, // Legacy pricing
		"e2-micro":      0.0084, // Cheapest non-legacy option
		"e2-small":      0.0134, // Small but reliable
		"n1-standard-1": 0.0475, // Standard fallback
	}

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

			// Use the first zone to check machine type availability
			zone := zones.Items[0].Name

			// Try each cheap machine type until we find one that's available
			for _, machineType := range cheapMachineTypes {
				_, err := compute.NewMachineTypesService(g.service).Get(g.projectId, zone, machineType).Context(ctx).Do()
				if err == nil {
					// This machine type is available in this region
					price := machineTypePricing[machineType]
					resultLock.Lock()
					result[region.Name] = regionMachineType{
						MachineType: machineType,
						HourlyCost:  price,
					}
					resultLock.Unlock()
					log.Printf("Region %s: using machine type %s at $%.4f/hr", region.Name, machineType, price)
					break
				}
			}

			// If no machine type was found, log a warning and use a default
			if _, ok := result[region.Name]; !ok {
				log.Printf("Warning: No preferred machine type available in region %s, using e2-micro as fallback", region.Name)
				resultLock.Lock()
				result[region.Name] = regionMachineType{
					MachineType: "e2-micro",
					HourlyCost:  0.0086,
				}
				resultLock.Unlock()
			}
		}()
	}

	wg.Wait()

	return result, nil
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

func (g *gcpProvider) DeleteInstance(ctx context.Context, instanceID providers.InstanceID) error {
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

func (g *gcpProvider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	// Ensure machine type cache is populated
	if err := g.ensureMachineTypeCache(); err != nil {
		return providers.InstanceID{}, fmt.Errorf("failed to ensure machine type cache: %w", err)
	}

	// Get the appropriate machine type for this region
	g.regionMachineTypeCacheLock.RLock()
	regionMachineType, ok := g.regionMachineTypeCache[region]
	g.regionMachineTypeCacheLock.RUnlock()

	if !ok {
		return providers.InstanceID{}, fmt.Errorf("no suitable machine type found for region %s", region)
	}

	prefix := "https://www.googleapis.com/compute/v1/projects/" + g.projectId
	zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, region)).Do()
	if err != nil {
		return providers.InstanceID{}, err
	}

	zone := zones.Items[rand.Intn(len(zones.Items))].Name
	log.Printf("Creating instance in zone %s using machine type %s", zone, regionMachineType.MachineType)

	name := "tscloudvpn-" + zone

	tmplOut := new(bytes.Buffer)
	hostname := gcpInstanceHostname(region)
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
		OnExit: fmt.Sprintf(`gcloud compute instances delete %s --quiet --zone=%s`, name, zone),
		SSHKey: g.sshKey,
	}); err != nil {
		return providers.InstanceID{}, err
	}

	_, err = compute.NewInstancesService(g.service).Insert(g.projectId, zone, &compute.Instance{
		Name:        name,
		MachineType: prefix + "/zones/" + zone + "/machineTypes/" + regionMachineType.MachineType,
		Disks: []*compute.AttachedDisk{
			{
				AutoDelete: true,
				Boot:       true,
				InitializeParams: &compute.AttachedDiskInitializeParams{
					SourceImage: debianLatestImage,
				},
			},
		},
		Labels: map[string]string{
			"tscloudvpn":               "true",
			"tscloudvpn-owner":         g.sanitizeLabelValue(g.ownerID),
		},
		NetworkInterfaces: []*compute.NetworkInterface{
			{
				AccessConfigs: []*compute.AccessConfig{
					{
						Name:        "External NAT",
						NetworkTier: "PREMIUM",
					},
				},
				StackType:  "IPV4_ONLY",
				Subnetwork: fmt.Sprintf("projects/%s/regions/%s/subnetworks/default", g.projectId, region),
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
					Value: aws.String(tmplOut.String()),
				},
			},
		},
	}).Context(ctx).Do()
	if err != nil {
		return providers.InstanceID{}, err
	}

	log.Printf("Launched instance %s", name)

	return providers.InstanceID{
		Hostname:     hostname,
		ProviderID:   name,
		ProviderName: providerName,
	}, nil
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

func (g *gcpProvider) ListInstances(ctx context.Context, region string) ([]providers.InstanceID, error) {
	zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, region)).Do()
	if err != nil {
		return nil, err
	}

	sanitizedOwnerID := g.sanitizeLabelValue(g.ownerID)
	var instanceIDs []providers.InstanceID
	for _, zone := range zones.Items {
		// Filter by both tscloudvpn label and owner label
		filter := fmt.Sprintf("labels.tscloudvpn:* AND labels.tscloudvpn-owner=%s", sanitizedOwnerID)
		instanceList, err := compute.NewInstancesService(g.service).List(g.projectId, zone.Name).Filter(filter).Context(ctx).Do()
		if err != nil {
			return nil, err
		}

		for _, instance := range instanceList.Items {
			if instance.Status != "TERMINATED" {
				createdAt, _ := time.Parse(time.RFC3339, instance.CreationTimestamp)
				instanceIDs = append(instanceIDs, providers.InstanceID{
					Hostname:     gcpInstanceHostname(region),
					ProviderID:   instance.Name,
					ProviderName: providerName,
					CreatedAt:    createdAt,
				})
			}
		}
	}

	return instanceIDs, nil
}

func (g *gcpProvider) Hostname(region string) providers.HostName {
	return providers.HostName(gcpInstanceHostname(region))
}

// GetRegionPrice returns the hourly price for the cheapest available instance in the specified region
func (g *gcpProvider) GetRegionPrice(region string) float64 {
	// Ensure machine type cache is populated
	if err := g.ensureMachineTypeCache(); err != nil {
		log.Printf("Failed to ensure machine type cache: %v", err)
		return 0.0084 // Return default e2-micro price as fallback
	}

	g.regionMachineTypeCacheLock.RLock()
	defer g.regionMachineTypeCacheLock.RUnlock()

	if regionMachineType, ok := g.regionMachineTypeCache[region]; ok {
		return regionMachineType.HourlyCost
	}

	// Default price if region not found in cache
	return 0.0084 // e2-micro default price
}

func init() {
	providers.Register(providerName, NewProvider)
}
