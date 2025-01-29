package digitalocean

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"strings"
	"text/template"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"

	"github.com/digitalocean/godo"
)

type digitaloceanProvider struct {
	client *godo.Client
	token  string
	sshKey string
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
		Size:   "s-1vcpu-1gb",
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

func init() {
	providers.Register("do", New)
}
