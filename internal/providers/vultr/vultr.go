package vultr

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"text/template"

	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/bradenaw/juniper/xslices"
	"github.com/tailscale/tailscale-client-go/tailscale"
	"github.com/vultr/govultr/v3"
	"golang.org/x/oauth2"
)

const (
	providerName = "vultr"
)

type vultrProvider struct {
	vultrClient *govultr.Client
	apiKey      string
	sshKey      string
}

func NewProvider(ctx context.Context, sshKey string) (providers.Provider, error) {
	apiKey := os.Getenv("VULTR_API_KEY")
	if apiKey == "" {
		return nil, nil
	}

	config := &oauth2.Config{}
	ts := config.TokenSource(ctx, &oauth2.Token{AccessToken: apiKey})
	vultrClient := govultr.NewClient(oauth2.NewClient(ctx, ts))

	return &vultrProvider{
		vultrClient: vultrClient,
		apiKey:      apiKey,
		sshKey:      sshKey,
	}, nil
}

func vultrInstanceHostname(region string) string {
	return fmt.Sprintf("vultr-%s", region)
}

func (v *vultrProvider) CreateInstance(ctx context.Context, region string, key tailscale.Key) (string, error) {
	tmplOut := new(bytes.Buffer)
	hostname := vultrInstanceHostname(region)
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
		OnExit: fmt.Sprintf("curl https://api.vultr.com/v2/instances/$(curl -s http://169.254.169.254/v1.json | jq -r '.\"instance-v2-id\"') -X DELETE -H 'Authorization: Bearer %s'", v.apiKey),
		SSHKey: v.sshKey,
	}); err != nil {
		return "", err
	}

	plans, _, _, err := v.vultrClient.Plan.List(ctx, "all", &govultr.ListOptions{
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
	return result, nil
}

func init() {
	providers.Register(providerName, NewProvider)
}
