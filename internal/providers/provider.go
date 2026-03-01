package providers

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"strings"
	"text/template"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
)

type Region struct {
	Code     string
	LongName string
}

type InstanceStatus int

type HostName string

type Instance struct {
	Hostname     string    // e.g., "ec2-us-west-2", "do-nyc1"
	ProviderID   string    // e.g., "i-1234567890abcdef0", "123456789"
	ProviderName string    // e.g., "ec2", "do", "linode", "gcp", "vultr", "hetzner"
	CreatedAt    time.Time // Instance creation time from cloud provider (for GC grace period)
	HourlyCost   float64   // Actual hourly cost of the instance
}

const (
	// No running instance in this region
	InstanceStatusMissing InstanceStatus = iota
	// Running means an instance exists - it could be starting/stopping/running
	InstanceStatusRunning
)

// CreateRequest contains everything a provider needs to create an instance.
type CreateRequest struct {
	Region   string
	UserData string // Rendered init script
	SSHKey   string // SSH public key (used by Azure OS profile; other providers can ignore)
}

type Provider interface {
	CreateInstance(ctx context.Context, req CreateRequest) (Instance, error)
	DeleteInstance(ctx context.Context, instanceID Instance) error
	GetInstanceStatus(ctx context.Context, region string) (InstanceStatus, error)
	ListInstances(ctx context.Context, region string) ([]Instance, error)
	ListRegions(ctx context.Context) ([]Region, error)
	Hostname(region string) HostName
	GetRegionHourlyEstimate(region string) float64 // Get hourly price for a region
}

// AllInstanceLister is an optional interface that providers can implement
// to list all instances across all regions in a single API call.
// The GC will use this when available instead of iterating per-region.
type AllInstanceLister interface {
	ListAllInstances(ctx context.Context) ([]Instance, error)
}

type ProviderFactory func(ctx context.Context, cfg *config.Config) (Provider, error)

func Register(name string, label string, providerFactory ProviderFactory) {
	ProviderFactoryRegistry[name] = providerFactory
	ProviderLabels[name] = label
}

const (
	// OwnerTagKey is the tag/label key used to identify the tscloudvpn instance that owns a cloud resource
	OwnerTagKey = "tscloudvpn-owner"
)

var (
	//go:embed install.sh.tmpl
	InitData                string
	ProviderFactoryRegistry = make(map[string]ProviderFactory)
	ProviderLabels          = make(map[string]string)
)

// InitScriptData contains the template variables for the init script.
type InitScriptData struct {
	Args     string // Tailscale CLI arguments (--authkey, --hostname, etc.)
	SSHKey   string // SSH public key for authorized_keys
	Service  string // Service type name (e.g., "exit")
	Provider string // Provider short name (e.g., "do")
	Region   string // Region code (e.g., "nyc1")
}

// RenderUserData renders the init script template with the given parameters.
func RenderUserData(hostname string, key *controlapi.PreauthKey, sshKey string, service string, provider string, region string) (string, error) {
	var buf bytes.Buffer
	if err := template.Must(template.New("tmpl").Parse(InitData)).Execute(&buf, InitScriptData{
		Args: fmt.Sprintf(
			`%s --hostname=%s`,
			strings.Join(key.GetCLIArgs(), " "),
			hostname,
		),
		SSHKey:   sshKey,
		Service:  service,
		Provider: provider,
		Region:   region,
	}); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// GetOwnerID returns a unique identifier for the current tscloudvpn instance
// based on the control plane configuration (Tailnet name or Headscale user ID)
func GetOwnerID(cfg *config.Config) string {
	if cfg.Control.Type == "headscale" {
		return fmt.Sprintf("headscale-%d", cfg.Control.Headscale.UserID)
	}
	return fmt.Sprintf("tailscale-%s", cfg.Control.Tailscale.Tailnet)
}
