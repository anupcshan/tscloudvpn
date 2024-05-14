package gcp

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log"
	"math/rand"
	"os"
	"slices"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/tailscale/tailscale-client-go/tailscale"
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

func NewProvider(ctx context.Context, sshKey string) (providers.Provider, error) {
	gcpCredentialsJsonFile := os.Getenv("GCP_CREDENTIALS_JSON_FILE")
	gcpProjectId := os.Getenv("GCP_PROJECT_ID")
	gcpServiceAccount := os.Getenv("GCP_SERVICE_ACCOUNT")

	if gcpCredentialsJsonFile == "" || gcpProjectId == "" || gcpServiceAccount == "" {
		return nil, nil
	}

	gcpCredentialsJson, err := os.ReadFile(gcpCredentialsJsonFile)
	if err != nil {
		return nil, err
	}

	service, err := compute.NewService(ctx, option.WithCredentialsJSON([]byte(gcpCredentialsJson)))
	if err != nil {
		return nil, err
	}

	return &gcpProvider{
		projectId:      gcpProjectId,
		serviceAccount: gcpServiceAccount,
		service:        service,
		sshKey:         sshKey,
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

func (g *gcpProvider) CreateInstance(ctx context.Context, region string, key tailscale.Key) (string, error) {
	prefix := "https://www.googleapis.com/compute/v1/projects/" + g.projectId
	zones, err := compute.NewZonesService(g.service).List(g.projectId).Context(ctx).Filter(fmt.Sprintf(`name="%s-*"`, region)).Do()
	if err != nil {
		return "", err
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
			`--advertise-tags="%s" --authkey="%s" --hostname=%s`,
			strings.Join(key.Capabilities.Devices.Create.Tags, ","),
			key.Key,
			hostname,
		),
		OnExit: fmt.Sprintf(`gcloud compute instances delete %s --quiet --zone=%s`, name, zone),
		SSHKey: g.sshKey,
	}); err != nil {
		return "", err
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
		return "", err
	}

	log.Printf("Launched instance %s", name)

	return hostname, nil
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

func (g *gcpProvider) Hostname(region string) providers.HostName {
	return providers.HostName(gcpInstanceHostname(region))
}

func init() {
	providers.Register(providerName, NewProvider)
}
