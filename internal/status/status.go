package status

import (
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/bradenaw/juniper/xmaps"
	"tailscale.com/ipn/ipnstate"
)

// Info wraps any type T with status information about the cloud infrastructure
type Info[T any] struct {
	ProviderCount int
	RegionCount   int
	ActiveNodes   int
	Detail        T
}

// WrapWithInfo wraps the given detail with status information computed from
// cloud providers, regions, and Tailscale status
func WrapWithInfo[T any](
	t T,
	cloudProviders map[string]providers.Provider,
	lazyListRegionsMap map[string]func() []providers.Region,
	tsStatus *ipnstate.Status,
) Info[T] {
	regionCount := 0
	deviceMap := xmaps.Set[providers.HostName]{}
	expectedHostnameMap := xmaps.Set[providers.HostName]{}

	for _, peer := range tsStatus.Peer {
		deviceMap.Add(providers.HostName(peer.HostName))
	}

	for providerName, f := range lazyListRegionsMap {
		for _, region := range f() {
			regionCount++
			expectedHostnameMap.Add(cloudProviders[providerName].Hostname(region.Code))
		}
	}

	return Info[T]{
		ProviderCount: len(cloudProviders),
		RegionCount:   regionCount,
		ActiveNodes:   len(xmaps.Intersection(deviceMap, expectedHostnameMap)),
		Detail:        t,
	}
}
