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

type Provider interface {
	CreateInstance(ctx context.Context, region string, key tailscale.Key) (string, error)
	ListRegions(ctx context.Context) ([]Region, error)
	Hostname(region string) string
}

type ProviderFactory func(ctx context.Context) (Provider, error)

func Register(name string, providerFactory ProviderFactory) {
	ProviderFactoryRegistry[name] = providerFactory
}

var (
	//go:embed install.sh.tmpl
	InitData                string
	ProviderFactoryRegistry = make(map[string]ProviderFactory)
)
