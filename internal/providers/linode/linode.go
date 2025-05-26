package linode

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"strconv"
	"strings"
	"text/template"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/linode/linodego"
	"golang.org/x/exp/rand"
	"golang.org/x/oauth2"
)

type linodeProvider struct {
	client *linodego.Client
	token  string
	sshKey string
}

func New(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	if cfg.Providers.Linode.Token == "" {
		// No token set. Nothing to do
		return nil, nil
	}

	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.Providers.Linode.Token})
	oauth2Client := oauth2.NewClient(ctx, tokenSource)
	client := linodego.NewClient(oauth2Client)

	return &linodeProvider{
		client: &client,
		sshKey: cfg.SSH.PublicKey,
		token:  cfg.Providers.Linode.Token,
	}, nil
}

func linodeInstanceHostname(region string) string {
	return fmt.Sprintf("linode-%s", region)
}

func (l *linodeProvider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	tmplOut := new(bytes.Buffer)
	hostname := linodeInstanceHostname(region)
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
		OnExit: fmt.Sprintf(`
		export TOKEN=$(curl -s -X PUT -H "Metadata-Token-Expiry-Seconds: 3600" http://169.254.169.254/v1/token)
		export INSTANCE_ID=$(curl -s -H 'Accept: application/json' -H "Metadata-Token: $TOKEN" http://169.254.169.254/v1/instance | jq -r .id)
		curl -H 'Authorization: Bearer %s' -X DELETE https://api.linode.com/v4/linode/instances/$INSTANCE_ID`, l.token),
		SSHKey: l.sshKey,
	}); err != nil {
		return providers.InstanceID{}, err
	}

	createOpts := linodego.InstanceCreateOptions{
		Label:    fmt.Sprintf("tscloudvpn-%s", region),
		Region:   region,
		Type:     "g6-nanode-1",
		Image:    "linode/debian12",
		RootPass: generateRandomPassword(),
		Metadata: &linodego.InstanceMetadataOptions{
			UserData: base64.StdEncoding.EncodeToString(tmplOut.Bytes()),
		},
	}

	instance, err := l.client.CreateInstance(ctx, createOpts)
	if err != nil {
		return providers.InstanceID{}, fmt.Errorf("failed to create Linode instance: %w", err)
	}

	log.Printf("Launched Linode instance %d", instance.ID)

	return providers.InstanceID{
		Hostname:     hostname,
		ProviderID:   strconv.Itoa(instance.ID),
		ProviderName: "ec2",
	}, nil
}

func (l *linodeProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	instances, err := l.client.ListInstances(ctx, nil)
	if err != nil {
		return providers.InstanceStatusMissing, err
	}

	for _, instance := range instances {
		if instance.Region == region {
			return providers.InstanceStatusRunning, nil
		}
	}

	return providers.InstanceStatusMissing, nil
}

func (l *linodeProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	regions, err := l.client.ListRegions(ctx, nil)
	if err != nil {
		return nil, err
	}

	var result []providers.Region
	for _, r := range regions {
		if r.Status != "ok" {
			continue
		}
		result = append(result, providers.Region{
			Code:     r.ID,
			LongName: r.Label,
		})
	}
	return result, nil
}

func (l *linodeProvider) Hostname(region string) providers.HostName {
	return providers.HostName(linodeInstanceHostname(region))
}

// GetRegionPrice returns the hourly price for the g6-nanode-1 instance
func (l *linodeProvider) GetRegionPrice(region string) float64 {
	typeInfo, err := l.client.GetType(context.Background(), "g6-nanode-1")
	if err != nil {
		// Log error but return the hardcoded price as fallback
		log.Printf("Failed to get instance type pricing: %v", err)
		return 0
	}

	// Check for regional pricing
	for _, rp := range typeInfo.RegionPrices {
		if rp.ID == region {
			return float64(rp.Hourly)
		}
	}

	return float64(typeInfo.Price.Hourly)
}

func generateRandomPassword() string {
	// Generate a random password
	charset := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*()_+-=[]{}|;:,.<>?"
	length := 16
	b := make([]byte, length)
	for i := range b {
		b[i] = charset[rand.Intn(len(charset))]
	}
	return string(b)
}

func init() {
	providers.Register("linode", New)
}
