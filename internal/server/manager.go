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

	"github.com/hako/durafmt"
	"tailscale.com/client/local"
)

type Manager struct {
	cloudProviders     map[string]providers.Provider
	lazyListRegionsMap map[string]func() []providers.Region
	instanceRegistry   *instances.Registry
	tsLocalClient      *local.Client
}

func NewManager(
	ctx context.Context,
	logger *log.Logger,
	cloudProviders map[string]providers.Provider,
	tsLocalClient *local.Client,
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

	tsStatus, err := m.tsLocalClient.Status(ctx)
	if err != nil {
		return zero, err
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

	return status.WrapWithInfo(mappedRegions, m.cloudProviders, m.lazyListRegionsMap, tsStatus), nil
}

func (m *Manager) SetupRoutes(ctx context.Context, mux *http.ServeMux, controller controlapi.ControlApi) {
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
				fmt.Fprintf(&html, "<td>%s</td>", node.Provider)
				fmt.Fprintf(&html, "<td>%s</td>", node.Region)
				fmt.Fprintf(&html, "<td>%s</td>", node.SinceCreated)
				fmt.Fprintf(&html, "<td>%.2fc/hr</td>", node.PriceCentsPerHour)
				fmt.Fprintf(&html, "<td>%s</td>", connectionType)
				fmt.Fprintf(&html, `<td><span class="label %s">%.1f%%</span></td>`,
					successRateClass, node.PingStats.SuccessRate*100)
				fmt.Fprintf(&html, "<td>%s ± %s</td>",
					node.PingStats.AvgLatency.Round(time.Millisecond), node.PingStats.StdDev.Round(time.Millisecond))
				fmt.Fprintf(&html, `<td><button class="btn btn-danger" hx-ext="disable-element" `+
					`hx-disable-element="self" hx-delete="/providers/%s/regions/%s">Delete</button></td>`,
					node.Provider, node.Region)
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
						"Success Rate: %.1f%% Avg Latency: %s ± %s stddev Last Failure: %s Created: %s Connection: %s",
						region.PingStats.SuccessRate*100,
						region.PingStats.AvgLatency.Round(time.Millisecond),
						region.PingStats.StdDev.Round(time.Millisecond),
						lastFailureStr,
						region.CreatedTS.Round(time.Second),
						connectionType,
					)

					data[hasNodeKey] = fmt.Sprintf(`<span class="label %s" title="%s" style="margin-right: 0.25em">running for %s</span>`, labelClass, tooltip, region.SinceCreated)
					data[buttonKey] = fmt.Sprintf(`<button class="btn btn-danger" hx-ext="disable-element" hx-disable-element="self" hx-delete="%s">Delete</button>`, opURL)
				} else {
					// Check if instance is being launched
					instanceStatus, err := m.instanceRegistry.GetInstanceStatus(region.Provider, region.Region)
					disabledFragment := ""
					if err == nil && !instanceStatus.LaunchedAt.IsZero() && !instanceStatus.IsRunning {
						// Instance is launching but not yet running
						data[hasNodeKey] = fmt.Sprintf(`<span class="badge badge-info">Launched instance %s ago ...</span>`, durafmt.ParseShort(time.Since(instanceStatus.LaunchedAt)).InternationalString())
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
