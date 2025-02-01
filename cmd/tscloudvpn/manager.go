package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
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

type PingResult struct {
	Timestamp        time.Time
	Success          bool
	Latency          time.Duration
	DirectConnection bool
}

type PingHistory struct {
	mu               sync.RWMutex
	history          []PingResult // Ring buffer of last 100 results
	successCount     int
	totalLatency     time.Duration
	lastFailure      time.Time
	position         int  // Current position in ring buffer
	DirectConnection bool // Most recent direct connection state
}

func NewPingHistory() *PingHistory {
	return &PingHistory{
		history: make([]PingResult, 100), // Fixed size of 100
	}
}

func (ph *PingHistory) AddResult(success bool, latency time.Duration, directConnection bool) {
	ph.mu.Lock()
	defer ph.mu.Unlock()

	// If the entry at current position was successful, decrease success count
	if ph.history[ph.position].Success {
		ph.successCount--
		ph.totalLatency -= ph.history[ph.position].Latency
	}

	// Add new result
	ph.history[ph.position] = PingResult{
		Timestamp:        time.Now(),
		Success:          success,
		Latency:          latency,
		DirectConnection: directConnection,
	}

	if success {
		ph.successCount++
		ph.totalLatency += latency
	} else {
		ph.lastFailure = time.Now()
	}

	// Move position forward
	ph.position = (ph.position + 1) % 100
}

func (ph *PingHistory) GetStats() (successRate float64, avgLatency time.Duration, stdDevLatency time.Duration, timeSinceFailure time.Duration, directConnection bool) {
	ph.mu.RLock()
	defer ph.mu.RUnlock()

	// Count non-zero entries to handle startup period
	total := 0
	for _, result := range ph.history {
		if !result.Timestamp.IsZero() {
			total++
		}
	}

	if total == 0 {
		return 0, 0, 0, 0, false
	}

	successRate = float64(ph.successCount) / float64(total)
	if ph.successCount > 0 {
		avgLatency = ph.totalLatency / time.Duration(ph.successCount)

		// Calculate standard deviation
		var sumSquaredDiffSeconds float64
		for _, result := range ph.history {
			if !result.Timestamp.IsZero() && result.Success {
				diff := (result.Latency - avgLatency).Seconds()
				sumSquaredDiffSeconds += diff * diff
			}
		}
		if ph.successCount > 1 {
			stdDevLatency = time.Duration(1000*1000*math.Sqrt(sumSquaredDiffSeconds)/float64(ph.successCount-1)) * time.Microsecond
		}
	}

	if !ph.lastFailure.IsZero() {
		timeSinceFailure = time.Since(ph.lastFailure)
	}

	// Return most recent direct connection state
	for i := len(ph.history) - 1; i >= 0; i-- {
		idx := (ph.position - 1 - i + len(ph.history)) % len(ph.history)
		result := ph.history[idx]
		if !result.Timestamp.IsZero() && result.Success {
			directConnection = result.DirectConnection
			break
		}
	}

	return successRate, avgLatency, stdDevLatency, timeSinceFailure, directConnection
}

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
	pingHistories      *ConcurrentMap[providers.HostName, *PingHistory]
	launchTSMap        *ConcurrentMap[providers.HostName, time.Time]
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
		pingHistories:      NewConcurrentMap[providers.HostName, *PingHistory](),
		launchTSMap:        NewConcurrentMap[providers.HostName, time.Time](),
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

	ticker := time.NewTicker(time.Second)
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
					history := m.pingHistories.Get(peerHostName)
					result, err := m.tsLocalClient.Ping(subctx, peer.TailscaleIPs[0], tailcfg.PingDisco)
					if err != nil {
						log.Printf("Ping error from %s (%s): %s", peer.HostName, peer.TailscaleIPs[0], err)
						history.AddResult(false, 0, false)
					} else {
						latency := time.Duration(result.LatencySeconds*1000000) * time.Microsecond
						isDirectConnection := result.Endpoint != ""

						if history == nil {
							history = NewPingHistory()
							m.pingHistories.Set(peerHostName, history)
						}
						history.AddResult(true, latency, isDirectConnection)
					}
					return nil
				})
			}

			_ = errG.Wait()
		}()
	}
}

type mappedRegion struct {
	Provider     string
	Region       string
	LongName     string
	HasNode      bool
	SinceCreated string
	PingStats    struct {
		SuccessRate      float64
		AvgLatency       time.Duration
		StdDevLatency    time.Duration
		TimeSinceFailure time.Duration
		DirectConnection bool
	}
	CreatedTS time.Time
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
			var pingStats struct {
				SuccessRate      float64
				AvgLatency       time.Duration
				StdDevLatency    time.Duration
				TimeSinceFailure time.Duration
				DirectConnection bool
			}
			if hasNode {
				createdTS = node.Created
				sinceCreated = durafmt.ParseShort(time.Since(node.Created)).InternationalString()

				history := m.pingHistories.Get(provider.Hostname(region.Code))
				if history != nil {
					pingStats.SuccessRate, pingStats.AvgLatency, pingStats.StdDevLatency, pingStats.TimeSinceFailure, pingStats.DirectConnection = history.GetStats()
				}
			}
			return mappedRegion{
				Provider:     providerName,
				Region:       region.Code,
				LongName:     region.LongName,
				HasNode:      hasNode,
				CreatedTS:    createdTS,
				SinceCreated: sinceCreated,
				PingStats:    pingStats,
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

func (m *Manager) Serve(ctx context.Context, listen net.Listener, controller controlapi.ControlApi) error {
	mux := http.NewServeMux()
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
				buttonKey := fmt.Sprintf("%s-%s-button", region.Provider, region.Region)
				opURL := fmt.Sprintf("/providers/%s/regions/%s", region.Provider, region.Region)
				if region.HasNode {
					labelClass := "label-danger"
					if region.PingStats.SuccessRate >= 0.95 {
						labelClass = "label-success"
					} else if region.PingStats.SuccessRate >= 0.80 {
						labelClass = "label-warning"
					}

					lastFailureStr := "never"
					if region.PingStats.TimeSinceFailure > 0 {
						lastFailureStr = durafmt.ParseShort(region.PingStats.TimeSinceFailure).String() + " ago"
					}

					connectionType := "Relay"
					if region.PingStats.DirectConnection {
						connectionType = "Direct"
					}

					tooltip := fmt.Sprintf(
						"Success Rate: %.1f%% Avg Latency: %s (±%s) Last Failure: %s Created: %s Connection: %s",
						region.PingStats.SuccessRate*100,
						region.PingStats.AvgLatency.Round(time.Millisecond),
						region.PingStats.StdDevLatency.Round(time.Millisecond),
						lastFailureStr,
						region.CreatedTS.Round(time.Second),
						connectionType,
					)

					data[hasNodeKey] = fmt.Sprintf(`<span class="label %s" title="%s" style="margin-right: 0.25em">running for %s</span>`, labelClass, tooltip, region.SinceCreated)
					data[buttonKey] = fmt.Sprintf(`<button class="btn btn-danger" hx-ext="disable-element" hx-disable-element="self" hx-delete="%s">Delete</button>`, opURL)
				} else {
					launchTS := m.launchTSMap.Get(providers.HostName(fmt.Sprintf("%s-%s", region.Provider, region.Region)))
					disabledFragment := ""
					if !launchTS.IsZero() {
						data[hasNodeKey] = fmt.Sprintf(`<span class="badge badge-info">Launched instance %s ago ...</span>`, durafmt.ParseShort(time.Since(launchTS)).InternationalString())
						disabledFragment = "disabled"
					} else {
						data[hasNodeKey] = ""
					}
					data[buttonKey] = fmt.Sprintf(`<button class="btn btn-primary" hx-ext="disable-element" hx-disable-element="self" hx-put="%s" %s>Create</button>`, opURL, disabledFragment)
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

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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
				switch r.Method {
				case "DELETE":
					ctx := r.Context()
					devices, err := controller.ListDevices(ctx)
					if err != nil {
						w.WriteHeader(http.StatusInternalServerError)
						w.Write([]byte(err.Error()))
						return
					}

					filtered := xslices.Filter(devices, func(device controlapi.Device) bool {
						return providers.HostName(device.Hostname) == provider.Hostname(region.Code)
					})

					if len(filtered) > 0 {
						err := controller.DeleteDevice(ctx, filtered[0].ID)
						if err != nil {
							w.WriteHeader(http.StatusInternalServerError)
							w.Write([]byte(err.Error()))
							return
						} else {
							fmt.Fprint(w, "ok")
						}
					}

				case "PUT":
					logger := log.New(io.MultiWriter(flushWriter{w}, os.Stderr), "", log.Lshortfile|log.Lmicroseconds)
					ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Minute)
					defer func() {
						cancelFunc()
						m.launchTSMap.Set(provider.Hostname(region.Code), time.Time{})
					}()
					m.launchTSMap.Set(provider.Hostname(region.Code), time.Now())
					err := createInstance(ctx, logger, controller, provider, region.Code)
					if err != nil {
						w.Write([]byte(err.Error()))
					} else {
						w.Write([]byte("ok"))
					}

				default:
					fmt.Fprintf(w, "Method %s not implemented", r.Method)
				}
			})
		}
	}

	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets.Assets))))

	log.Printf("Listening on %s", listen.Addr())
	return http.Serve(listen, logRequest(mux))
}
