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

type gcpProvider struct {
	projectId      string
	serviceAccount string
	service        *compute.Service
	sshKey         string
}

func NewProvider(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.GCP.CredentialsJSON == "" || cfg.Providers.GCP.ProjectID == "" || cfg.Providers.GCP.ServiceAccount == "" {
		return nil, nil
	}

	service, err := compute.NewService(ctx, option.WithCredentialsJSON([]byte(cfg.Providers.GCP.CredentialsJSON)))
	if err != nil {
		return nil, err
	}

	return &gcpProvider{
		projectId:      cfg.Providers.GCP.ProjectID,
		serviceAccount: cfg.Providers.GCP.ServiceAccount,
		service:        service,
		sshKey:         cfg.SSH.PublicKey,
	}, nil
}

func gcpInstanceHostname(region string) string {
	return fmt.Sprintf("gcp-%s", region)
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
	prefix := "https://www.googleapis.com/compute/v1/projects/" + g.projectId
	zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, region)).Do()
	if err != nil {
		return providers.InstanceID{}, err
	}

	zone := zones.Items[rand.Intn(len(zones.Items))].Name
	log.Printf("Creating instance in zone %s", zone)

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
		MachineType: prefix + "/zones/" + zone + "/machineTypes/f1-micro",
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
			"tscloudvpn": "true",
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

	var instances []*compute.Instance
	for _, zone := range zones.Items {
		instanceList, err := compute.NewInstancesService(g.service).List(g.projectId, zone.Name).Filter("labels.tscloudvpn:*").Context(ctx).Do()
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

	var instanceIDs []providers.InstanceID
	for _, zone := range zones.Items {
		instanceList, err := compute.NewInstancesService(g.service).List(g.projectId, zone.Name).Filter("labels.tscloudvpn:*").Context(ctx).Do()
		if err != nil {
			return nil, err
		}

		for _, instance := range instanceList.Items {
			if instance.Status != "TERMINATED" {
				instanceIDs = append(instanceIDs, providers.InstanceID{
					Hostname:     gcpInstanceHostname(region),
					ProviderID:   instance.Name,
					ProviderName: providerName,
				})
			}
		}
	}

	return instanceIDs, nil
}

func (g *gcpProvider) Hostname(region string) providers.HostName {
	return providers.HostName(gcpInstanceHostname(region))
}

// GetRegionPrice returns the hourly price for the f1-micro instance in the specified region
func (g *gcpProvider) GetRegionPrice(region string) float64 {
	// GCP f1-micro instance prices vary by region
	// In a production environment, consider using the GCP Cloud Billing API
	// https://cloud.google.com/billing/v1/how-tos/catalog-api
	prices := map[string]float64{
		"us-central1":             0.0076, // Iowa
		"us-east1":                0.0076, // South Carolina
		"us-east4":                0.0085, // Northern Virginia
		"us-west1":                0.0076, // Oregon
		"us-west2":                0.0091, // Los Angeles
		"us-west3":                0.0091, // Salt Lake City
		"us-west4":                0.0076, // Las Vegas
		"europe-west1":            0.0084, // Belgium
		"europe-west2":            0.0096, // London
		"europe-west3":            0.0096, // Frankfurt
		"europe-west4":            0.0084, // Netherlands
		"europe-west6":            0.0109, // Zurich
		"asia-east1":              0.0090, // Taiwan
		"asia-east2":              0.0103, // Hong Kong
		"asia-northeast1":         0.0092, // Tokyo
		"asia-northeast2":         0.0092, // Osaka
		"asia-northeast3":         0.0092, // Seoul
		"asia-south1":             0.0083, // Mumbai
		"asia-southeast1":         0.0090, // Singapore
		"asia-southeast2":         0.0097, // Jakarta
		"australia-southeast1":    0.0103, // Sydney
		"southamerica-east1":      0.0120, // Sao Paulo
		"northamerica-northeast1": 0.0084, // Montreal
	}

	if price, ok := prices[region]; ok {
		return price
	}
	return 0.0098 // Default price if region not found
}

func init() {
	providers.Register(providerName, NewProvider)
}
