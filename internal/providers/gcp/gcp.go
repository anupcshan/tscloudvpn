package gcp

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"text/template"

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

type gcpProvider struct {
	projectId      string
	serviceAccount string
	service        *compute.Service
}

func NewProvider(ctx context.Context) (providers.Provider, error) {
	gcpCredentialsJson := os.Getenv("GCP_CREDENTIALS_JSON")
	gcpProjectId := os.Getenv("GCP_PROJECT_ID")
	gcpServiceAccount := os.Getenv("GCP_SERVICE_ACCOUNT")

	if gcpCredentialsJson == "" || gcpProjectId == "" || gcpServiceAccount == "" {
		return nil, nil
	}

	service, err := compute.NewService(ctx, option.WithCredentialsJSON([]byte(gcpCredentialsJson)))
	if err != nil {
		return nil, err
	}

	return &gcpProvider{
		projectId:      gcpProjectId,
		serviceAccount: gcpServiceAccount,
		service:        service,
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
		regions = append(regions, providers.Region{
			Code:     region.Name,
			LongName: region.Name,
		})
	}

	sort.Slice(regions, func(i, j int) bool {
		return regions[i].Code < regions[j].Code
	})

	return regions, nil
}

func (g *gcpProvider) CreateInstance(ctx context.Context, region string, key tailscale.Key) (string, error) {
	prefix := "https://www.googleapis.com/compute/v1/projects/" + g.projectId
	zone := region + "-c" // Choose region with suffix c. Chosen by fair dice roll. Guaranteed to to be random.
	name := "tscloudvpn-" + zone

	tmplOut := new(bytes.Buffer)
	hostname := gcpInstanceHostname(region)
	if err := template.Must(template.New("tmpl").Parse(providers.InitData)).Execute(tmplOut, struct {
		Args   string
		OnExit string
	}{
		Args: fmt.Sprintf(
			`--advertise-tags="%s" --authkey="%s" --hostname=%s`,
			strings.Join(key.Capabilities.Devices.Create.Tags, ","),
			key.Key,
			hostname,
		),
		OnExit: fmt.Sprintf(`gcloud compute instances delete %s --quiet --zone=%s`, name, zone),
	}); err != nil {
		return "", err
	}

	_, err := compute.NewInstancesService(g.service).Insert(g.projectId, zone, &compute.Instance{
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

func (g *gcpProvider) Hostname(region string) string {
	return gcpInstanceHostname(region)
}

func init() {
	providers.Register(providerName, NewProvider)
}
