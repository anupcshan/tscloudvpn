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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/stats"
	"github.com/anupcshan/tscloudvpn/internal/utils"
	"github.com/bradenaw/juniper/xmaps"
	"github.com/bradenaw/juniper/xslices"
	"github.com/hako/durafmt"
	"golang.org/x/sync/errgroup"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

const historySize = 250

type PingResult struct {
	Timestamp            time.Time
	Success              bool
	Latency              time.Duration
	ConnectionType       string
	PreviousLatencyDelta time.Duration
}

type PingHistory struct {
	mu                   sync.RWMutex
	history              []PingResult // Ring buffer of last historySize results
	successCount         int
	totalLatency         time.Duration
	lastFailure          time.Time
	position             int // Current position in ring buffer
	totalJitter          time.Duration
	consecutiveJitterCnt int
	lastSuccessLatency   time.Duration
}

func NewPingHistory() *PingHistory {
	return &PingHistory{
		history: make([]PingResult, historySize),
	}
}

func (ph *PingHistory) AddResult(success bool, latency time.Duration, connectionType string) {
	ph.mu.Lock()
	defer ph.mu.Unlock()

	// If the entry at current position was successful, decrease success count
	if ph.history[ph.position].Success {
		ph.successCount--
		ph.totalLatency -= ph.history[ph.position].Latency
	}

	if ph.history[ph.position].PreviousLatencyDelta > 0 {
		ph.totalJitter -= ph.history[ph.position].PreviousLatencyDelta
		ph.consecutiveJitterCnt--
	}

	// Add new result
	ph.history[ph.position] = PingResult{
		Timestamp:      time.Now(),
		Success:        success,
		Latency:        latency,
		ConnectionType: connectionType,
	}

	if success {
		ph.successCount++
		ph.totalLatency += latency

		// Calculate jitter only if we have a previous successful latency
		if ph.lastSuccessLatency > 0 {
			jitter := latency - ph.lastSuccessLatency
			if jitter < 0 {
				jitter = -jitter
			}
			ph.totalJitter += jitter
			ph.consecutiveJitterCnt++
			ph.history[ph.position].PreviousLatencyDelta = jitter
		}
		ph.lastSuccessLatency = latency
	} else {
		ph.lastFailure = time.Now()
		ph.lastSuccessLatency = 0
	}

	// Move position forward
	ph.position = (ph.position + 1) % historySize
}

func (ph *PingHistory) GetStats() (successRate float64, avgLatency time.Duration, jitter time.Duration, timeSinceFailure time.Duration, connectionType string) {
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
		return 0, 0, 0, 0, ""
	}

	successRate = float64(ph.successCount) / float64(total)
	if ph.successCount > 0 {
		avgLatency = ph.totalLatency / time.Duration(ph.successCount)

		// Calculate average jitter
		if ph.consecutiveJitterCnt > 0 {
			jitter = ph.totalJitter / time.Duration(ph.consecutiveJitterCnt)
		}
	}

	if !ph.lastFailure.IsZero() {
		timeSinceFailure = time.Since(ph.lastFailure)
	}

	// Return most recent connection type
	for i := len(ph.history) - 1; i >= 0; i-- {
		idx := (ph.position - 1 - i + len(ph.history)) % len(ph.history)
		result := ph.history[idx]
		if !result.Timestamp.IsZero() && result.Success {
			connectionType = result.ConnectionType
			break
		}
	}

	return successRate, avgLatency, jitter, timeSinceFailure, connectionType
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
	statsManager       *stats.StatsManager
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

	// Initialize the stats manager
	statsManager, err := stats.NewStatsManager("")
	if err != nil {
		log.Printf("Error initializing stats manager: %v", err)
		// Continue without stats if there's an error
	}

	m := &Manager{
		cloudProviders:     cloudProviders,
		tsLocalClient:      tsLocalClient,
		pingHistories:      NewConcurrentMap[providers.HostName, *PingHistory](),
		launchTSMap:        NewConcurrentMap[providers.HostName, time.Time](),
		lazyListRegionsMap: lazyListRegionsMap,
		statsManager:       statsManager,
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
					result, err := m.tsLocalClient.Ping(subctx, peer.TailscaleIPs[0], tailcfg.PingDisco)
					history := m.pingHistories.Get(peerHostName)
					if history == nil {
						history = NewPingHistory()
						m.pingHistories.Set(peerHostName, history)
					}

					// Find provider and region from hostname
					var providerName, regionCode string
					for pName, provider := range m.cloudProviders {
						for _, f := range m.lazyListRegionsMap {
							for _, region := range f() {
								if provider.Hostname(region.Code) == peerHostName {
									providerName = pName
									regionCode = region.Code
									break
								}
							}
							if providerName != "" {
								break
							}
						}
						if providerName != "" {
							break
						}
					}

					// Record stats
					if m.statsManager != nil {
						now := time.Now()

						if err != nil {
							// Record error in history
							history.AddResult(false, 0, "")

							// Record error in persistent stats
							m.statsManager.RecordPing(stats.PingRecord{
								Hostname:     string(peerHostName),
								Timestamp:    now,
								Success:      false,
								ProviderName: providerName,
								RegionCode:   regionCode,
							})

							// Record detailed error
							m.statsManager.RecordError(stats.ErrorRecord{
								Timestamp:    now,
								ErrorType:    "ping_failure",
								ErrorMessage: err.Error(),
								ProviderName: providerName,
								RegionCode:   regionCode,
								Hostname:     string(peerHostName),
							})

							log.Printf("Ping error from %s (%s): %s", peer.HostName, peer.TailscaleIPs[0], err)
						} else {
							latency := time.Duration(result.LatencySeconds*1000000) * time.Microsecond
							connectionType := "direct"
							if result.Endpoint == "" && result.DERPRegionCode != "" {
								connectionType = "relayed via " + result.DERPRegionCode
							}

							// Record success in history
							history.AddResult(true, latency, connectionType)

							// Record success in persistent stats
							m.statsManager.RecordPing(stats.PingRecord{
								Hostname:       string(peerHostName),
								Timestamp:      now,
								Success:        true,
								Latency:        latency,
								ConnectionType: connectionType,
								ProviderName:   providerName,
								RegionCode:     regionCode,
							})
						}
					} else {
						// Original code path when stats manager is not available
						if err != nil {
							log.Printf("Ping error from %s (%s): %s", peer.HostName, peer.TailscaleIPs[0], err)
							history.AddResult(false, 0, "")
						} else {
							latency := time.Duration(result.LatencySeconds*1000000) * time.Microsecond
							connectionType := "direct"
							if result.Endpoint == "" && result.DERPRegionCode != "" {
								connectionType = "relayed via " + result.DERPRegionCode
							}

							history.AddResult(true, latency, connectionType)
						}
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
	PriceCentsPerHour float64 // Hourly cost in USD cents
	PingStats         struct {
		SuccessRate      float64
		AvgLatency       time.Duration
		Jitter           time.Duration
		TimeSinceFailure time.Duration
		ConnectionType   string
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
				Jitter           time.Duration
				TimeSinceFailure time.Duration
				ConnectionType   string
			}
			if hasNode {
				createdTS = node.Created
				sinceCreated = durafmt.ParseShort(time.Since(node.Created)).InternationalString()

				history := m.pingHistories.Get(provider.Hostname(region.Code))
				if history != nil {
					pingStats.SuccessRate, pingStats.AvgLatency, pingStats.Jitter, pingStats.TimeSinceFailure, pingStats.ConnectionType = history.GetStats()
				}
			}
			return mappedRegion{
				Provider:          providerName,
				Region:            region.Code,
				LongName:          region.LongName,
				HasNode:           hasNode,
				CreatedTS:         createdTS,
				SinceCreated:      sinceCreated,
				PriceCentsPerHour: provider.GetRegionPrice(region.Code) * 100,
				PingStats:         pingStats,
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

		buildRunningNodesTableHTML := func(detail []mappedRegion) string {
			var runningNodes []mappedRegion
			for _, r := range detail {
				if r.HasNode {
					runningNodes = append(runningNodes, r)
				}
			}

			var html strings.Builder
			for _, node := range runningNodes {
				connectionType := node.PingStats.ConnectionType

				successRateClass := "label-danger"
				if node.PingStats.SuccessRate >= 0.95 {
					successRateClass = "label-success"
				} else if node.PingStats.SuccessRate >= 0.80 {
					successRateClass = "label-warning"
				}

				html.WriteString("<tr>")
				html.WriteString(fmt.Sprintf("<td>%s</td>", node.Provider))
				html.WriteString(fmt.Sprintf("<td>%s</td>", node.Region))
				html.WriteString(fmt.Sprintf("<td>%s</td>", node.SinceCreated))
				html.WriteString(fmt.Sprintf("<td>%.2fc/hr</td>", node.PriceCentsPerHour))
				html.WriteString(fmt.Sprintf("<td>%s</td>", connectionType))
				html.WriteString(fmt.Sprintf(`<td><span class="label %s">%.1f%%</span></td>`,
					successRateClass, node.PingStats.SuccessRate*100))
				html.WriteString(fmt.Sprintf("<td>%s (±%s)</td>",
					node.PingStats.AvgLatency.Round(time.Millisecond), node.PingStats.Jitter.Round(time.Millisecond)))
				html.WriteString(fmt.Sprintf(`<td><button class="btn btn-danger" hx-ext="disable-element" `+
					`hx-disable-element="self" hx-delete="/providers/%s/regions/%s">Delete</button></td>`,
					node.Provider, node.Region))
				html.WriteString("</tr>")
			}
			return html.String()
		}

		for {
			status, err := m.GetStatus(ctx)
			if err != nil {
				log.Printf("Error getting status: %s", err)
				continue
			}

			// Generate running nodes table HTML
			runningNodesHTML := buildRunningNodesTableHTML(status.Detail)
			runningNodesKey := "running-nodes-table"
			if dataCache[runningNodesKey] != runningNodesHTML {
				fmt.Fprintf(w, "event: %s\n", runningNodesKey)
				fmt.Fprintf(w, "data: %s\n", runningNodesHTML)
				fmt.Fprint(w, "\n\n")
				dataCache[runningNodesKey] = runningNodesHTML
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

					connectionType := region.PingStats.ConnectionType

					tooltip := fmt.Sprintf(
						"Success Rate: %.1f%% Avg Latency: %s (±%s jitter) Last Failure: %s Created: %s Connection: %s",
						region.PingStats.SuccessRate*100,
						region.PingStats.AvgLatency.Round(time.Millisecond),
						region.PingStats.Jitter.Round(time.Millisecond),
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

	// Add stats endpoint for accessing statistics
	mux.HandleFunc("/stats/", func(w http.ResponseWriter, r *http.Request) {
		if m.statsManager == nil {
			http.Error(w, "Statistics tracking is not enabled", http.StatusServiceUnavailable)
			return
		}

		pathParts := strings.Split(strings.TrimPrefix(r.URL.Path, "/stats/"), "/")
		if len(pathParts) < 1 || pathParts[0] == "" {
			// Root stats page - show all available nodes (both current and historical)
			status, err := m.GetStatus(ctx)
			if err != nil {
				http.Error(w, fmt.Sprintf("Failed to get status: %v", err), http.StatusInternalServerError)
				return
			}

			// Get all historical node hostnames from the database
			allHostnames, err := m.statsManager.GetAllHostnames()
			if err != nil {
				http.Error(w, fmt.Sprintf("Failed to get node history: %v", err), http.StatusInternalServerError)
				return
			}

			// Prepare active nodes info
			type ActiveNodeInfo struct {
				Hostname string
				Provider string
				Region   string
			}
			activeNodes := []ActiveNodeInfo{}
			activeHostnames := make(map[string]bool)

			for _, region := range status.Detail {
				if region.HasNode {
					hostname := fmt.Sprintf("%s-%s", region.Provider, region.Region)
					activeNodes = append(activeNodes, ActiveNodeInfo{
						Hostname: hostname,
						Provider: region.Provider,
						Region:   region.Region,
					})
					activeHostnames[hostname] = true
				}
			}

			// Filter out active hostnames from the historical list
			historicalNodes := []string{}
			for _, hostname := range allHostnames {
				if !activeHostnames[hostname] {
					historicalNodes = append(historicalNodes, hostname)
				}
			}

			templateData := struct {
				ActiveNodes     []ActiveNodeInfo
				HistoricalNodes []string
			}{
				ActiveNodes:     activeNodes,
				HistoricalNodes: historicalNodes,
			}

			w.Header().Set("Content-Type", "text/html")
			if err := templates.ExecuteTemplate(w, "stats_index.tmpl", templateData); err != nil {
				http.Error(w, fmt.Sprintf("Failed to render template: %v", err), http.StatusInternalServerError)
			}
			return
		}

		// Get statistics for a specific node
		hostname := pathParts[0]

		// Check if the node is currently active
		status, err := m.GetStatus(ctx)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get status: %v", err), http.StatusInternalServerError)
			return
		}

		// Find if the node is currently active
		isActive := false
		nodeProvider := ""
		nodeRegion := ""
		for _, region := range status.Detail {
			if region.HasNode && fmt.Sprintf("%s-%s", region.Provider, region.Region) == hostname {
				isActive = true
				nodeProvider = region.Provider
				nodeRegion = region.Region
				break
			}
		}

		// If we don't have provider/region info, try to extract it from the hostname
		if nodeProvider == "" || nodeRegion == "" {
			parts := strings.Split(hostname, "-")
			if len(parts) >= 2 {
				nodeProvider = parts[0]
				nodeRegion = strings.Join(parts[1:], "-") // In case the region name itself has hyphens
			}
		}

		// Default to last 24 hours
		endTime := time.Now()
		startTime := endTime.Add(-24 * time.Hour)

		// Parse time range parameters if provided
		if r.URL.Query().Get("days") != "" {
			days, err := strconv.Atoi(r.URL.Query().Get("days"))
			if err == nil && days > 0 {
				startTime = endTime.Add(-time.Duration(days) * 24 * time.Hour)
			}
		}

		summary, err := m.statsManager.GetPingSummary(hostname, startTime, endTime)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get statistics: %v", err), http.StatusInternalServerError)
			return
		}

		// Get last 10 ping records for detail (reduced from 100 for better readability)
		recentPings, err := m.statsManager.GetRecentPings(hostname, 10)
		if err != nil {
			http.Error(w, fmt.Sprintf("Failed to get recent pings: %v", err), http.StatusInternalServerError)
			return
		}

		// Calculate success rate
		successRate := 0.0
		if summary.TotalPings > 0 {
			successRate = float64(summary.SuccessfulPings) / float64(summary.TotalPings) * 100
		}

		// Format recent pings for template
		type formattedPing struct {
			FormattedTime  string
			Success        bool
			Latency        time.Duration
			ConnectionType string
		}

		formattedPings := make([]formattedPing, 0, len(recentPings))
		for _, ping := range recentPings {
			formattedPings = append(formattedPings, formattedPing{
				FormattedTime:  ping.Timestamp.Format(time.RFC1123),
				Success:        ping.Success,
				Latency:        ping.Latency,
				ConnectionType: ping.ConnectionType,
			})
		}

		// For historical nodes, get the last ping timestamp
		var lastPingTime time.Time
		if !isActive && len(recentPings) > 0 {
			lastPingTime = recentPings[0].Timestamp
		}

		// Get provider-specific pricing
		var pricePerHour float64
		if nodeProvider != "" && nodeRegion != "" {
			pricePerHour = m.cloudProviders[nodeProvider].GetRegionPrice(nodeRegion)
		}

		// Prepare template data
		templateData := struct {
			Hostname     string
			StartTime    string
			EndTime      string
			Summary      stats.StatsSummary
			SuccessRate  float64
			RecentPings  []formattedPing
			Days         []int
			IsActive     bool
			Provider     string
			Region       string
			LastSeen     time.Time
			PricePerHour float64
		}{
			Hostname:     hostname,
			StartTime:    startTime.Format(time.RFC1123),
			EndTime:      endTime.Format(time.RFC1123),
			Summary:      summary,
			SuccessRate:  successRate,
			RecentPings:  formattedPings,
			Days:         []int{1, 7, 30},
			IsActive:     isActive,
			Provider:     nodeProvider,
			Region:       nodeRegion,
			LastSeen:     lastPingTime,
			PricePerHour: pricePerHour,
		}

		w.Header().Set("Content-Type", "text/html")
		if err := templates.ExecuteTemplate(w, "stats_detail.tmpl", templateData); err != nil {
			http.Error(w, fmt.Sprintf("Failed to render template: %v", err), http.StatusInternalServerError)
		}
	})

	log.Printf("Listening on %s", listen.Addr())
	return http.Serve(listen, logRequest(mux))
}
