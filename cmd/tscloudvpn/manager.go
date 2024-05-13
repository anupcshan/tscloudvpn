package main

import (
	"context"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/utils"
	"github.com/bradenaw/juniper/xmaps"
	"github.com/bradenaw/juniper/xslices"
	"github.com/hako/durafmt"
	"golang.org/x/sync/errgroup"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

type ConcurrentMap[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V
}

func NewConcurrentMap[K comparable, V any]() *ConcurrentMap[K, V] {
	return &ConcurrentMap[K, V]{m: make(map[K]V)}
}

func (l *ConcurrentMap[K, V]) Get(k K) V {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.m[k]
}

func (l *ConcurrentMap[K, V]) Set(k K, v V) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[k] = v
}

type Manager struct {
	cloudProviders     map[string]providers.Provider
	lazyListRegionsMap map[string]func() []providers.Region
	pingMap            *ConcurrentMap[string, time.Time]
	tsLocalClient      *tailscale.LocalClient
}

func NewManager(
	ctx context.Context,
	cloudProviders map[string]providers.Provider,
	tsLocalClient *tailscale.LocalClient,
) *Manager {
	lazyListRegionsMap := make(map[string]func() []providers.Region)

	for providerName, provider := range cloudProviders {
		provider := provider

		lazyListRegionsMap[providerName] = utils.LazyWithErrors(
			func() ([]providers.Region, error) {
				return provider.ListRegions(ctx)
			},
		)
	}

	m := &Manager{
		cloudProviders:     cloudProviders,
		tsLocalClient:      tsLocalClient,
		pingMap:            NewConcurrentMap[string, time.Time](),
		lazyListRegionsMap: lazyListRegionsMap,
	}

	go m.initOnce(ctx)

	return m
}

func (m *Manager) initOnce(ctx context.Context) {
	expectedHostnameMap := xmaps.Set[string]{}
	for providerName, f := range m.lazyListRegionsMap {
		for _, region := range f() {
			expectedHostnameMap.Add(m.cloudProviders[providerName].Hostname(region.Code))
		}
	}

	ticker := time.NewTicker(5 * time.Second)
	for {
		<-ticker.C
		func() {
			subctx, cancelFunc := context.WithTimeout(ctx, 5*time.Second)
			defer cancelFunc()
			errG := errgroup.Group{}
			tsStatus, err := m.tsLocalClient.Status(ctx)
			if err != nil {
				log.Printf("Error getting status: %s", err)
				return
			}
			for _, peer := range tsStatus.Peer {
				peer := peer
				if !expectedHostnameMap.Contains(peer.HostName) {
					continue
				}
				errG.Go(func() error {
					_, err := m.tsLocalClient.Ping(subctx, peer.TailscaleIPs[0], tailcfg.PingDisco)
					if err != nil {
						log.Printf("Ping error from %s (%s): %s", peer.HostName, peer.TailscaleIPs[0], err)
					} else {
						m.pingMap.Set(peer.HostName, time.Now())
					}
					return nil
				})
			}

			_ = errG.Wait()
		}()
	}
}

type mappedRegion struct {
	Provider          string
	Region            string
	LongName          string
	HasNode           bool
	SinceCreated      string
	RecentPingSuccess bool
	CreatedTS         time.Time
}

func (m *Manager) GetStatus(ctx context.Context) (statusInfo[[]mappedRegion], error) {
	var zero statusInfo[[]mappedRegion]
	var mappedRegions []mappedRegion

	tsStatus, err := m.tsLocalClient.Status(ctx)
	if err != nil {
		return zero, err
	}

	for providerName, provider := range m.cloudProviders {
		provider := provider
		providerName := providerName

		lazyListRegions := m.lazyListRegionsMap[providerName]

		regions := lazyListRegions()

		deviceMap := make(map[string]*ipnstate.PeerStatus)
		for _, peer := range tsStatus.Peer {
			deviceMap[peer.HostName] = peer
		}

		mappedRegions = append(mappedRegions, xslices.Map(regions, func(region providers.Region) mappedRegion {
			node, hasNode := deviceMap[provider.Hostname(region.Code)]
			var sinceCreated string
			var createdTS time.Time
			var recentPingSuccess bool
			if hasNode {
				createdTS = node.Created
				sinceCreated = durafmt.ParseShort(time.Since(node.Created)).InternationalString()
				lastPingTimestamp := m.pingMap.Get(provider.Hostname(region.Code))
				if !lastPingTimestamp.IsZero() {
					timeSinceLastPing := time.Since(lastPingTimestamp)
					if timeSinceLastPing < 30*time.Second {
						recentPingSuccess = true
					}
				}
			}
			return mappedRegion{
				Provider:          providerName,
				Region:            region.Code,
				LongName:          region.LongName,
				HasNode:           hasNode,
				CreatedTS:         createdTS,
				SinceCreated:      sinceCreated,
				RecentPingSuccess: recentPingSuccess,
			}
		})...)
	}

	sort.Slice(mappedRegions, func(i, j int) bool {
		if mappedRegions[i].Provider != mappedRegions[j].Provider {
			return mappedRegions[i].Provider < mappedRegions[j].Provider
		}
		return mappedRegions[i].Region < mappedRegions[j].Region
	})

	return wrapWithStatusInfo(mappedRegions, m.cloudProviders, m.lazyListRegionsMap, tsStatus), nil
}
