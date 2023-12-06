package providers

import (
	"context"
	_ "embed"

	"github.com/tailscale/tailscale-client-go/tailscale"
)

type Region struct {
	Code     string
	LongName string
}

type InstanceStatus int

const (
	// No running instance in this region
	InstanceStatusMissing InstanceStatus = iota
	// Running means an instance exists - it could be starting/stopping/running
	InstanceStatusRunning
)

type Provider interface {
	CreateInstance(ctx context.Context, region string, key tailscale.Key) (string, error)
	GetInstanceStatus(ctx context.Context, region string) (InstanceStatus, error)
	ListRegions(ctx context.Context) ([]Region, error)
	Hostname(region string) string
}

type ProviderFactory func(ctx context.Context, sshKey string) (Provider, error)

func Register(name string, providerFactory ProviderFactory) {
	ProviderFactoryRegistry[name] = providerFactory
}

var (
	//go:embed install.sh.tmpl
	InitData                string
	ProviderFactoryRegistry = make(map[string]ProviderFactory)
)
