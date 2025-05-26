package providers

import (
	"context"
	_ "embed"

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
	Hostname     string // e.g., "ec2-us-west-2", "do-nyc1"
	ProviderID   string // e.g., "i-1234567890abcdef0", "123456789"
	ProviderName string // e.g., "ec2", "do", "linode", "gcp", "vultr"
}

const (
	// No running instance in this region
	InstanceStatusMissing InstanceStatus = iota
	// Running means an instance exists - it could be starting/stopping/running
	InstanceStatusRunning
)

type Provider interface {
	CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (InstanceID, error)
	GetInstanceStatus(ctx context.Context, region string) (InstanceStatus, error)
	ListRegions(ctx context.Context) ([]Region, error)
	Hostname(region string) HostName
	GetRegionPrice(region string) float64 // Get hourly price for a region
}

type ProviderFactory func(ctx context.Context, cfg *config.Config) (Provider, error)

func Register(name string, providerFactory ProviderFactory) {
	ProviderFactoryRegistry[name] = providerFactory
}

var (
	//go:embed install.sh.tmpl
	InitData                string
	ProviderFactoryRegistry = make(map[string]ProviderFactory)
)
