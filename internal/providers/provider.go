package providers

import (
	"context"

	"github.com/tailscale/tailscale-client-go/tailscale"
)

type Provider interface {
	CreateInstance(ctx context.Context, region string, key tailscale.Key) (string, error)
	ListRegions(ctx context.Context) ([]string, error)
	Hostname(region string) string
	GetName() string
}

type ProviderFactory func(ctx context.Context) (Provider, error)

var ProviderFactoryRegistry = make(map[string]ProviderFactory)

func Register(name string, providerFactory ProviderFactory) {
	ProviderFactoryRegistry[name] = providerFactory
}
