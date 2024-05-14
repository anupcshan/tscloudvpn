package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/utils"
	"github.com/bradenaw/juniper/xmaps"
	"github.com/bradenaw/juniper/xslices"
	"github.com/hako/durafmt"
	tailscale_go "github.com/tailscale/tailscale-client-go/tailscale"
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
	pingMap            *ConcurrentMap[providers.HostName, time.Time]
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
		pingMap:            NewConcurrentMap[providers.HostName, time.Time](),
		lazyListRegionsMap: lazyListRegionsMap,
	}

	go m.initOnce(ctx)

	return m
}

func (m *Manager) initOnce(ctx context.Context) {
	expectedHostnameMap := xmaps.Set[providers.HostName]{}
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
				peerHostName := providers.HostName(peer.HostName)
				if !expectedHostnameMap.Contains(peerHostName) {
					continue
				}
				errG.Go(func() error {
					_, err := m.tsLocalClient.Ping(subctx, peer.TailscaleIPs[0], tailcfg.PingDisco)
					if err != nil {
						log.Printf("Ping error from %s (%s): %s", peer.HostName, peer.TailscaleIPs[0], err)
					} else {
						m.pingMap.Set(peerHostName, time.Now())
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

		deviceMap := make(map[providers.HostName]*ipnstate.PeerStatus)
		for _, peer := range tsStatus.Peer {
			deviceMap[providers.HostName(peer.HostName)] = peer
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

func (m *Manager) Serve(ctx context.Context, listen net.Listener, tsClient *tailscale_go.Client) error {
	mux := http.NewServeMux()
	mux.Handle("/", http.RedirectHandler("/regions", http.StatusTemporaryRedirect))
	mux.Handle("/events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		dataCache := make(map[string]string)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			status, err := m.GetStatus(ctx)
			if err != nil {
				log.Printf("Error getting status: %s", err)
				continue
			}

			data := map[string]string{
				"active-nodes":   fmt.Sprintf("%d", status.ActiveNodes),
				"region-count":   fmt.Sprintf("%d", status.RegionCount),
				"provider-count": fmt.Sprintf("%d", status.ProviderCount),
			}

			for _, region := range status.Detail {
				hasNodeKey := fmt.Sprintf("%s-%s-hasnode", region.Provider, region.Region)
				if region.HasNode {
					if region.RecentPingSuccess {
						data[hasNodeKey] = fmt.Sprintf(`<span class="label label-success" title="{{.CreatedTS}}" style="margin-right: 0.25em">running for %s</span>`, region.SinceCreated)
					} else {
						data[hasNodeKey] = fmt.Sprintf(`<span class="label label-warning" title="{{.CreatedTS}}" style="margin-right: 0.25em">running for %s</span>`, region.SinceCreated)
					}
				} else {
					data[hasNodeKey] = ""
				}
			}

			for k, v := range data {
				if dataCache[k] == v {
					continue
				}

				fmt.Fprintf(w, "event: %s\n", k)
				fmt.Fprintf(w, "data: %s\n", v)
				fmt.Fprint(w, "\n\n")
				dataCache[k] = v
			}

			w.(http.Flusher).Flush()

			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	}))

	mux.HandleFunc("/regions", func(w http.ResponseWriter, r *http.Request) {
		status, err := m.GetStatus(ctx)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		if err := templates.ExecuteTemplate(w, "list_regions.tmpl", status); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
		}
	})

	for providerName, provider := range m.cloudProviders {
		provider := provider
		providerName := providerName

		lazyListRegions := m.lazyListRegionsMap[providerName]
		for _, region := range lazyListRegions() {
			region := region
			mux.HandleFunc(fmt.Sprintf("/providers/%s/regions/%s", providerName, region.Code), func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if r.PostFormValue("action") == "delete" {
						ctx := r.Context()
						devices, err := tsClient.Devices(ctx)
						if err != nil {
							w.WriteHeader(http.StatusInternalServerError)
							w.Write([]byte(err.Error()))
							return
						}

						filtered := xslices.Filter(devices, func(device tailscale_go.Device) bool {
							return providers.HostName(device.Hostname) == provider.Hostname(region.Code)
						})

						if len(filtered) > 0 {
							err := tsClient.DeleteDevice(ctx, filtered[0].ID)
							if err != nil {
								w.WriteHeader(http.StatusInternalServerError)
								w.Write([]byte(err.Error()))
								return
							} else {
								fmt.Fprint(w, "ok")
							}
						}
					} else {
						logger := log.New(io.MultiWriter(flushWriter{w}, os.Stderr), "", log.Lshortfile|log.Lmicroseconds)
						ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Minute)
						defer cancelFunc()
						err := createInstance(ctx, logger, tsClient, provider, region.Code)
						if err != nil {
							w.Write([]byte(err.Error()))
						} else {
							w.Write([]byte("ok"))
						}
					}

					return
				}

				fmt.Fprintf(w, "Method %s not implemented", r.Method)
			})
		}
	}

	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets.Assets))))

	log.Printf("Listening on %s", listen.Addr())
	return http.Serve(listen, logRequest(mux))
}
