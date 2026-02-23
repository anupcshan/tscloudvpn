package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	httputils "github.com/anupcshan/tscloudvpn/internal/http"
	"github.com/anupcshan/tscloudvpn/internal/instances"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/status"
	"github.com/anupcshan/tscloudvpn/internal/utils"

	"github.com/anupcshan/tscloudvpn/internal/tsclient"
	"github.com/hako/durafmt"
)

type Manager struct {
	cloudProviders     map[string]providers.Provider
	lazyListRegionsMap map[string]func() []providers.Region
	instanceRegistry   *instances.Registry
	tsLocalClient      tsclient.TailscaleClient
}

func NewManager(
	ctx context.Context,
	logger *log.Logger,
	cloudProviders map[string]providers.Provider,
	tsLocalClient tsclient.TailscaleClient,
	controlApi controlapi.ControlApi,
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

	instanceRegistry := instances.NewRegistry(logger, controlApi, tsLocalClient, cloudProviders)

	m := &Manager{
		cloudProviders:     cloudProviders,
		tsLocalClient:      tsLocalClient,
		instanceRegistry:   instanceRegistry,
		lazyListRegionsMap: lazyListRegionsMap,
	}

	return m
}

type mappedRegion struct {
	Provider          string
	ProviderLabel     string
	Region            string
	LongName          string
	HasNode           bool
	SinceCreated      string
	SinceLaunched     string
	PriceCentsPerHour float64 // Hourly cost in USD cents
	PingStats         struct {
		SuccessRate      float64
		AvgLatency       time.Duration
		StdDev           time.Duration
		TimeSinceFailure time.Duration
		ConnectionType   string
	}
	CreatedTS  time.Time
	LaunchedTS time.Time
}

func (m *Manager) GetStatus(ctx context.Context) (status.Info[[]mappedRegion], error) {
	var zero status.Info[[]mappedRegion]
	var mappedRegions []mappedRegion

	peers, err := m.tsLocalClient.GetPeers(ctx)
	if err != nil {
		return zero, err
	}

	peerHostnames := make([]string, len(peers))
	for i, p := range peers {
		peerHostnames[i] = p.Hostname
	}

	// Get all instance statuses from the registry
	allInstanceStatuses := m.instanceRegistry.GetAllInstanceStatuses()

	for providerName, provider := range m.cloudProviders {
		provider := provider
		providerName := providerName

		regions := m.lazyListRegionsMap[providerName]()

		for _, region := range regions {
			key := fmt.Sprintf("%s-%s", providerName, region.Code)
			instanceStatus, hasInstance := allInstanceStatuses[key]

			var sinceCreated string
			var sinceLaunched string
			var createdTS time.Time
			var launchedTS time.Time
			var pingStats struct {
				SuccessRate      float64
				AvgLatency       time.Duration
				StdDev           time.Duration
				TimeSinceFailure time.Duration
				ConnectionType   string
			}

			if hasInstance && instanceStatus.IsRunning {
				createdTS = instanceStatus.CreatedAt
				launchedTS = instanceStatus.LaunchedAt
				if !createdTS.IsZero() {
					sinceCreated = durafmt.ParseShort(time.Since(createdTS)).InternationalString()
				}
				if !launchedTS.IsZero() {
					sinceLaunched = durafmt.ParseShort(time.Since(launchedTS)).InternationalString()
				}
				pingStats = instanceStatus.PingStats
			}

			mappedRegions = append(mappedRegions, mappedRegion{
				Provider:          providerName,
				ProviderLabel:     providers.ProviderLabels[providerName],
				Region:            region.Code,
				LongName:          region.LongName,
				HasNode:           hasInstance && instanceStatus.IsRunning,
				CreatedTS:         createdTS,
				LaunchedTS:        launchedTS,
				SinceCreated:      sinceCreated,
				SinceLaunched:     sinceLaunched,
				PriceCentsPerHour: provider.GetRegionPrice(region.Code) * 100,
				PingStats:         pingStats,
			})
		}
	}

	sort.Slice(mappedRegions, func(i, j int) bool {
		if mappedRegions[i].Provider != mappedRegions[j].Provider {
			return mappedRegions[i].Provider < mappedRegions[j].Provider
		}
		return mappedRegions[i].Region < mappedRegions[j].Region
	})

	return status.WrapWithInfo(mappedRegions, m.cloudProviders, m.lazyListRegionsMap, peerHostnames), nil
}

func (m *Manager) SetupRoutes(ctx context.Context, mux *http.ServeMux, controller controlapi.ControlApi) {
	mux.Handle("/events", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")

		dataCache := make(map[string]string)

		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		buildRunningNodesTableHTML := func(detail []mappedRegion) string {
			var html strings.Builder
			hasCards := false

			// Launching nodes first
			for _, region := range detail {
				if region.HasNode {
					continue
				}
				instanceStatus, err := m.instanceRegistry.GetInstanceStatus(region.Provider, region.Region)
				if err != nil || instanceStatus.State != instances.StateLaunching {
					continue
				}
				hasCards = true
				elapsed := durafmt.ParseShort(time.Since(instanceStatus.LaunchedAt)).InternationalString()

				html.WriteString(`<div class="node-card node-card-launching">`)
				html.WriteString(`<div>`)
				html.WriteString(`<div class="node-main">`)
				fmt.Fprintf(&html, `<span class="node-location">%s</span>`, region.LongName)
				fmt.Fprintf(&html, `<span class="node-provider">%s</span>`, region.ProviderLabel)
				fmt.Fprintf(&html, `<span class="pill pill-blue">launching %s...</span>`, elapsed)
				html.WriteString(`</div>`)
				html.WriteString(`<div class="node-meta" style="margin-top:6px">`)
				fmt.Fprintf(&html, `<span>cost <span class="val">%.2fc/hr</span></span>`, region.PriceCentsPerHour)
				html.WriteString(`</div>`)
				html.WriteString(`</div>`)
				html.WriteString(`</div>`)
			}

			// Running nodes
			for _, node := range detail {
				if !node.HasNode {
					continue
				}
				hasCards = true
				connectionType := node.PingStats.ConnectionType

				pillClass := "pill-green"
				if node.PingStats.SuccessRate < 0.80 {
					pillClass = "pill-yellow"
				}

				html.WriteString(`<div class="node-card">`)
				html.WriteString(`<div>`)
				html.WriteString(`<div class="node-main">`)
				fmt.Fprintf(&html, `<span class="node-location">%s</span>`, node.LongName)
				fmt.Fprintf(&html, `<span class="node-provider">%s</span>`, node.ProviderLabel)
				fmt.Fprintf(&html, `<span class="pill %s">%s</span>`, pillClass, connectionType)
				html.WriteString(`</div>`)
				html.WriteString(`<div class="node-meta" style="margin-top:6px">`)
				fmt.Fprintf(&html, `<span>uptime <span class="val">%s</span></span>`, node.SinceCreated)
				fmt.Fprintf(&html, `<span>latency <span class="val">%s ± %s</span></span>`,
					node.PingStats.AvgLatency.Round(time.Millisecond), node.PingStats.StdDev.Round(time.Millisecond))
				fmt.Fprintf(&html, `<span>cost <span class="val">%.2fc/hr</span></span>`, node.PriceCentsPerHour)
				fmt.Fprintf(&html, `<span>success <span class="val">%.0f%%</span></span>`, node.PingStats.SuccessRate*100)
				html.WriteString(`</div>`)
				html.WriteString(`</div>`)
				fmt.Fprintf(&html, `<button class="btn btn-danger" hx-ext="disable-element" `+
					`hx-disable-element="self" hx-delete="/providers/%s/regions/%s">Delete</button>`,
					node.Provider, node.Region)
				html.WriteString(`</div>`)
			}

			if !hasCards {
				return `<div class="no-nodes">No active nodes</div>`
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
					pillClass := "pill-green"
					if region.PingStats.SuccessRate < 0.80 {
						pillClass = "pill-yellow"
					}

					data[hasNodeKey] = fmt.Sprintf(`<span class="pill %s">running %s</span>`, pillClass, region.SinceCreated)
					data[buttonKey] = fmt.Sprintf(`<button class="btn btn-danger" hx-ext="disable-element" hx-disable-element="self" hx-delete="%s">Delete</button>`, opURL)
				} else {
					// Check if instance is being launched
					instanceStatus, err := m.instanceRegistry.GetInstanceStatus(region.Provider, region.Region)
					disabledFragment := ""
					if err == nil && instanceStatus.State == instances.StateLaunching {
						data[hasNodeKey] = fmt.Sprintf(`<span class="pill pill-blue">launching %s...</span>`, durafmt.ParseShort(time.Since(instanceStatus.LaunchedAt)).InternationalString())
						disabledFragment = "disabled"
					} else {
						data[hasNodeKey] = ""
					}
					data[buttonKey] = fmt.Sprintf(`<button class="btn btn-create" hx-ext="disable-element" hx-disable-element="self" hx-put="%s" %s>Create</button>`, opURL, disabledFragment)
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

	for providerName := range m.cloudProviders {
		providerName := providerName

		lazyListRegions := m.lazyListRegionsMap[providerName]
		for _, region := range lazyListRegions() {
			region := region
			mux.HandleFunc(fmt.Sprintf("/providers/%s/regions/%s", providerName, region.Code), func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case "DELETE":
					err := m.instanceRegistry.DeleteInstance(providerName, region.Code)
					if err != nil {
						w.WriteHeader(http.StatusInternalServerError)
						w.Write([]byte(err.Error()))
						return
					}
					fmt.Fprint(w, "ok")

				case "PUT":
					w.Header().Set("Content-Type", "text/plain")
					w.Header().Set("Transfer-Encoding", "chunked")
					logger := log.New(io.MultiWriter(httputils.NewFlushWriter(w), os.Stderr), "", log.Lshortfile|log.Lmicroseconds)
					ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancelFunc()

					err := m.instanceRegistry.CreateInstance(ctx, providerName, region.Code)
					if err != nil {
						logger.Printf("Failed to create instance: %s", err)
						w.Write([]byte(err.Error()))
					} else {
						logger.Printf("Instance creation initiated")
						w.Write([]byte("ok"))
					}

				default:
					fmt.Fprintf(w, "Method %s not implemented", r.Method)
				}
			})
		}
	}

	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets.Assets))))
}

// Shutdown stops all instance controllers and cleans up resources
func (m *Manager) Shutdown() {
	m.instanceRegistry.Shutdown()
}
