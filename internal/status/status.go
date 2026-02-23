package status

import (
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

// Info wraps any type T with status information about the cloud infrastructure
type Info[T any] struct {
	ProviderCount int
	RegionCount   int
	ActiveNodes   int
	Detail        T
}

// WrapWithInfo wraps the given detail with status information computed from
// cloud providers, regions, and the set of peer hostnames visible in Tailscale
func WrapWithInfo[T any](
	t T,
	cloudProviders map[string]providers.Provider,
	lazyListRegionsMap map[string]func() []providers.Region,
	peerHostnames []string,
) Info[T] {
	regionCount := 0
	peerSet := make(map[providers.HostName]struct{}, len(peerHostnames))
	for _, h := range peerHostnames {
		peerSet[providers.HostName(h)] = struct{}{}
	}

	activeNodes := 0
	for providerName, f := range lazyListRegionsMap {
		for _, region := range f() {
			regionCount++
			hostname := cloudProviders[providerName].Hostname(region.Code)
			if _, ok := peerSet[hostname]; ok {
				activeNodes++
			}
		}
	}

	return Info[T]{
		ProviderCount: len(cloudProviders),
		RegionCount:   regionCount,
		ActiveNodes:   activeNodes,
		Detail:        t,
	}
}
