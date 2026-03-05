package providers

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net/netip"
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
	Hostname     string    // e.g., "ec2-us-west-2", "speedtest-ec2-us-west-2"
	ProviderID   string    // e.g., "i-1234567890abcdef0", "123456789"
	ProviderName string    // e.g., "ec2", "do", "linode", "gcp", "vultr", "hetzner"
	Region       string    // e.g., "us-west-2", "nyc1" — cloud provider region
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
	Hostname string            // Tailscale hostname (e.g. "do-nyc1", "photos")
	Region   string            // Cloud provider region
	UserData string            // Rendered init script
	SSHKey   string            // SSH public key for providers that require one to create a VM
	Debug    bool              // When true, open SSH port in firewalls and delay ERR shutdown
	Tags     map[string]string // Cloud resource tags (service type, instance name, etc.)
}

type Provider interface {
	CreateInstance(ctx context.Context, req CreateRequest) (Instance, error)
	DeleteInstance(ctx context.Context, instanceID Instance) error
	GetInstanceStatus(ctx context.Context, region string) (InstanceStatus, error)
	ListInstances(ctx context.Context, region string) ([]Instance, error)
	ListRegions(ctx context.Context) ([]Region, error)
	GetRegionHourlyEstimate(region string) float64 // Get hourly price for a region
}

// AllInstanceLister is an optional interface that providers can implement
// to list all instances across all regions in a single API call.
// The GC will use this when available instead of iterating per-region.
type AllInstanceLister interface {
	ListAllInstances(ctx context.Context) ([]Instance, error)
}

// PublicIPGetter is an optional interface that providers can implement
// to look up the public IP of a running instance. Used for SSH diagnostics
// in integration tests.
type PublicIPGetter interface {
	GetPublicIP(ctx context.Context, instance Instance) (netip.Addr, error)
	// DebugSSHUser returns the username for SSH debug connections (e.g. "root", "azureuser").
	DebugSSHUser() string
}

type ProviderFactory func(ctx context.Context, cfg *config.Config) (Provider, error)

func Register(name string, label string, providerFactory ProviderFactory) {
	ProviderFactoryRegistry[name] = providerFactory
	ProviderLabels[name] = label
}

const (
	// OwnerTagKey is the tag/label key used to identify the tscloudvpn instance that owns a cloud resource
	OwnerTagKey = "tscloudvpn-owner"
	// ServiceTagKey identifies the service type of a cloud resource (e.g. "exit", "files")
	ServiceTagKey = "tscloudvpn-service"
	// NameTagKey identifies the instance name of a cloud resource (e.g. "do-nyc1", "photos")
	NameTagKey = "tscloudvpn-instance-name"
)

var (
	//go:embed install.sh.tmpl
	InitData                string
	ProviderFactoryRegistry = make(map[string]ProviderFactory)
	ProviderLabels          = make(map[string]string)
)

// ExtractInstanceName extracts the instance name from a "key:value" flat tag list,
// for providers that don't support structured key-value tags (DO, Vultr, Linode).
// Returns fallback for VMs created before tagging was introduced.
func ExtractInstanceName(tags []string, fallback string) string {
	prefix := NameTagKey + ":"
	for _, tag := range tags {
		if strings.HasPrefix(tag, prefix) {
			return strings.TrimPrefix(tag, prefix)
		}
	}
	return fallback
}

// InitScriptData contains the template variables for the init script.
type InitScriptData struct {
	Args     string // Tailscale CLI arguments (--authkey, --hostname, etc.)
	SSHKey   string // SSH public key for authorized_keys
	Service  string // Service type name (e.g., "exit")
	Provider string // Provider short name (e.g., "do")
	Region   string // Region code (e.g., "nyc1")
	Debug    bool   // When true, delay ERR shutdown for SSH diagnostics
}

// RenderUserData renders the given init script template with the given parameters.
func RenderUserData(templateText string, hostname string, key *controlapi.PreauthKey, sshKey string, service string, provider string, region string, debug bool) (string, error) {
	var buf bytes.Buffer
	if err := template.Must(template.New("tmpl").Parse(templateText)).Execute(&buf, InitScriptData{
		Args: fmt.Sprintf(
			`%s --hostname=%s`,
			strings.Join(key.GetCLIArgs(), " "),
			hostname,
		),
		SSHKey:   sshKey,
		Service:  service,
		Provider: provider,
		Region:   region,
		Debug:    debug,
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
