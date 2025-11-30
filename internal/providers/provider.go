package providers

import (
	"context"
	_ "embed"
	"fmt"
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

type InstanceID struct {
	Hostname     string    // e.g., "ec2-us-west-2", "do-nyc1"
	ProviderID   string    // e.g., "i-1234567890abcdef0", "123456789"
	ProviderName string    // e.g., "ec2", "do", "linode", "gcp", "vultr", "hetzner"
	CreatedAt    time.Time // Instance creation time from cloud provider (for GC grace period)
}

const (
	// No running instance in this region
	InstanceStatusMissing InstanceStatus = iota
	// Running means an instance exists - it could be starting/stopping/running
	InstanceStatusRunning
)

type Provider interface {
	CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (InstanceID, error)
	DeleteInstance(ctx context.Context, instanceID InstanceID) error
	GetInstanceStatus(ctx context.Context, region string) (InstanceStatus, error)
	ListInstances(ctx context.Context, region string) ([]InstanceID, error)
	ListRegions(ctx context.Context) ([]Region, error)
	Hostname(region string) HostName
	GetRegionPrice(region string) float64 // Get hourly price for a region
}

type ProviderFactory func(ctx context.Context, cfg *config.Config) (Provider, error)

func Register(name string, providerFactory ProviderFactory) {
	ProviderFactoryRegistry[name] = providerFactory
}

const (
	// OwnerTagKey is the tag/label key used to identify the tscloudvpn instance that owns a cloud resource
	OwnerTagKey = "tscloudvpn-owner"
)

var (
	//go:embed install.sh.tmpl
	InitData                string
	ProviderFactoryRegistry = make(map[string]ProviderFactory)
)

// GetOwnerID returns a unique identifier for the current tscloudvpn instance
// based on the control plane configuration (Tailnet name or Headscale user ID)
func GetOwnerID(cfg *config.Config) string {
	if cfg.Control.Type == "headscale" {
		return fmt.Sprintf("headscale-%d", cfg.Control.Headscale.UserID)
	}
	return fmt.Sprintf("tailscale-%s", cfg.Control.Tailscale.Tailnet)
}
