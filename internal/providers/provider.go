package providers

import (
	"context"

	"github.com/tailscale/tailscale-client-go/tailscale"
)

type Provider interface {
	CreateInstance(ctx context.Context, region string, key tailscale.Key) (string, error)
	ListRegions(ctx context.Context) ([]string, error)
	Hostname(region string) string
}
