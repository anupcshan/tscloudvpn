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
	"github.com/anupcshan/tscloudvpn/internal/r2"
	"github.com/anupcshan/tscloudvpn/internal/services"
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
	sshKey string,
	cloudProviders map[string]providers.Provider,
	tsLocalClient tsclient.TailscaleClient,
	controlApi controlapi.ControlApi,
	r2TokenManager *r2.TokenManager,
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

	instanceRegistry := instances.NewRegistry(logger, sshKey, controlApi, tsLocalClient, cloudProviders, r2TokenManager)
	instanceRegistry.Start(ctx)

	m := &Manager{
		cloudProviders:     cloudProviders,
		tsLocalClient:      tsLocalClient,
		instanceRegistry:   instanceRegistry,
		lazyListRegionsMap: lazyListRegionsMap,
	}

	return m
}

type mappedRegion struct {
	Service           string
	ServiceLabel      string
	InstanceName      string
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
	State      instances.InstanceState
	LastError  string
	NodeStats  *instances.NodeStats
	Links      []resolvedLink
}

// resolvedLink is a ServiceLink with the hostname substituted.
type resolvedLink struct {
	Label  string
	URL    string // Format with hostname filled in
	Render string // "link" or "copy"
}

// pageTemplateData holds all data for the main page template.
type pageTemplateData struct {
	ProviderCount int
	RegionCount   int
	ActiveNodes   int
	Services      []serviceTabData
}

// serviceTabData holds region data for one service type tab.
type serviceTabData struct {
	Name           string
	Label          string
	NamedInstances bool
	Regions        []mappedRegion
	Providers      []providerOption // for named instance dropdowns
}

// providerOption is a provider+regions pair for the named instance UI dropdowns.
type providerOption struct {
	Name    string
	Label   string
	Regions []regionOption
}

type regionOption struct {
	Code     string
	LongName string
}

func (m *Manager) GetStatus(ctx context.Context, svcType *services.ServiceType) (status.Info[[]mappedRegion], error) {
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

	serviceName := svcType.Name

	for providerName, provider := range m.cloudProviders {
		provider := provider
		providerName := providerName

		regions := m.lazyListRegionsMap[providerName]()

		for _, region := range regions {
			instanceName := svcType.InstanceName(services.InstanceNameInput{
				Provider: providerName,
				Region:   region.Code,
			})
			key := fmt.Sprintf("%s-%s", serviceName, instanceName)
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

			var nodeStats *instances.NodeStats
			var state instances.InstanceState
			var lastError string
			if hasInstance {
				state = instanceStatus.State
				lastError = instanceStatus.LastError
				launchedTS = instanceStatus.LaunchedAt
				if instanceStatus.IsRunning {
					createdTS = instanceStatus.CreatedAt
					if !createdTS.IsZero() {
						sinceCreated = durafmt.ParseShort(time.Since(createdTS)).InternationalString()
					}
					if !launchedTS.IsZero() {
						sinceLaunched = durafmt.ParseShort(time.Since(launchedTS)).InternationalString()
					}
					pingStats = instanceStatus.PingStats
					nodeStats = instanceStatus.NodeStats
				}
			}

			priceCentsPerHour := provider.GetRegionHourlyEstimate(region.Code) * 100
			if hasInstance && instanceStatus.HourlyCost > 0 {
				priceCentsPerHour = instanceStatus.HourlyCost * 100
			}

			mappedRegions = append(mappedRegions, mappedRegion{
				Service:           serviceName,
				ServiceLabel:      svcType.Label,
				InstanceName:      instanceName,
				Provider:          providerName,
				ProviderLabel:     providers.ProviderLabels[providerName],
				Region:            region.Code,
				LongName:          region.LongName,
				HasNode:           hasInstance && instanceStatus.IsRunning,
				CreatedTS:         createdTS,
				LaunchedTS:        launchedTS,
				State:             state,
				LastError:         lastError,
				SinceCreated:      sinceCreated,
				SinceLaunched:     sinceLaunched,
				PriceCentsPerHour: priceCentsPerHour,
				PingStats:         pingStats,
				NodeStats:         nodeStats,
				Links:             resolveLinks(svcType.Links, svcType.Hostname(instanceName)),
			})
		}
	}

	sort.Slice(mappedRegions, func(i, j int) bool {
		if mappedRegions[i].Provider != mappedRegions[j].Provider {
			return mappedRegions[i].Provider < mappedRegions[j].Provider
		}
		return mappedRegions[i].Region < mappedRegions[j].Region
	})

	namer := func(provider, region string) string {
		instanceName := svcType.InstanceName(services.InstanceNameInput{Provider: provider, Region: region})
		return svcType.Hostname(instanceName)
	}
	return status.WrapWithInfo(mappedRegions, m.cloudProviders, m.lazyListRegionsMap, peerHostnames, namer), nil
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
				if region.State != instances.StateLaunching {
					continue
				}
				hasCards = true
				elapsed := durafmt.ParseShort(time.Since(region.LaunchedTS)).InternationalString()

				html.WriteString(`<div class="node-card node-card-launching">`)
				html.WriteString(`<div>`)
				html.WriteString(`<div class="node-main">`)
				fmt.Fprintf(&html, `<span class="node-location">%s</span>`, region.LongName)
				fmt.Fprintf(&html, `<span class="node-service">%s</span>`, region.ServiceLabel)
				fmt.Fprintf(&html, `<span class="node-provider">%s</span>`, region.ProviderLabel)
				fmt.Fprintf(&html, `<span class="pill pill-blue">launching %s...</span>`, elapsed)
				html.WriteString(`</div>`)
				html.WriteString(`<div class="node-meta" style="margin-top:6px">`)
				fmt.Fprintf(&html, `<span>cost <span class="val">%.2fc/hr</span></span>`, region.PriceCentsPerHour)
				html.WriteString(`</div>`)
				html.WriteString(`</div>`)
				html.WriteString(`</div>`)
			}

			// Failed nodes
			for _, region := range detail {
				if region.HasNode {
					continue
				}
				if region.State != instances.StateFailed {
					continue
				}
				hasCards = true

				html.WriteString(`<div class="node-card node-card-failed">`)
				html.WriteString(`<div>`)
				html.WriteString(`<div class="node-main">`)
				fmt.Fprintf(&html, `<span class="node-location">%s</span>`, region.LongName)
				fmt.Fprintf(&html, `<span class="node-service">%s</span>`, region.ServiceLabel)
				fmt.Fprintf(&html, `<span class="node-provider">%s</span>`, region.ProviderLabel)
				html.WriteString(`<span class="pill pill-red">failed</span>`)
				html.WriteString(`</div>`)
				html.WriteString(`<div class="node-meta" style="margin-top:6px">`)
				fmt.Fprintf(&html, `<span>%s</span>`, region.LastError)
				html.WriteString(`</div>`)
				html.WriteString(`</div>`)
				fmt.Fprintf(&html, `<button class="btn btn-danger" hx-ext="disable-element" `+
					`hx-disable-element="self" hx-delete="/services/%s/instances/%s">Dismiss</button>`,
					region.Service, region.InstanceName)
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

				html.WriteString(`<div class="node-card node-card-running">`)
				html.WriteString(`<div>`)
				html.WriteString(`<div class="node-main">`)
				fmt.Fprintf(&html, `<span class="node-location">%s</span>`, node.LongName)
				fmt.Fprintf(&html, `<span class="node-service">%s</span>`, node.ServiceLabel)
				fmt.Fprintf(&html, `<span class="node-provider">%s</span>`, node.ProviderLabel)
				fmt.Fprintf(&html, `<span class="pill %s">%s</span>`, pillClass, connectionType)
				html.WriteString(`</div>`)
				html.WriteString(`<div class="node-meta" style="margin-top:6px">`)
				fmt.Fprintf(&html, `<span>uptime <span class="val">%s</span></span>`, node.SinceCreated)
				fmt.Fprintf(&html, `<span>latency <span class="val">%s ± %s</span></span>`,
					node.PingStats.AvgLatency.Round(time.Millisecond), node.PingStats.StdDev.Round(time.Millisecond))
				runningCostDollars := node.PriceCentsPerHour / 100 * time.Since(node.CreatedTS).Hours()
				fmt.Fprintf(&html, `<span>cost <span class="val">$%.2f</span> (%.2fc/hr)</span>`, runningCostDollars, node.PriceCentsPerHour)
				fmt.Fprintf(&html, `<span>success <span class="val">%.0f%%</span></span>`, node.PingStats.SuccessRate*100)
				if node.NodeStats != nil {
					for _, stat := range node.NodeStats.StatsDisplay {
						fmt.Fprintf(&html, `<span>%s <span class="val">%s</span></span>`, stat.Label, stat.Value)
					}
					idleDuration := time.Since(node.NodeStats.LastActive)
					if idleDuration < 5*time.Minute {
						fmt.Fprintf(&html, `<span class="pill pill-green">active</span>`)
					} else {
						idle := durafmt.ParseShort(idleDuration).InternationalString()
						fmt.Fprintf(&html, `<span class="pill pill-yellow">idle %s</span>`, idle)
					}
				}
				if len(node.Links) > 0 {
					html.WriteString(`<div class="node-meta" style="margin-top:4px">`)
					for _, link := range node.Links {
						if link.Render == "link" {
							fmt.Fprintf(&html, `<a href="%s" class="node-link" target="_blank">%s ↗</a>`, link.URL, link.Label)
						} else {
							fmt.Fprintf(&html, `<span class="node-copy" onclick="navigator.clipboard.writeText('%s')" title="Click to copy">%s</span>`, link.URL, link.URL)
						}
					}
					html.WriteString(`</div>`)
				}
				html.WriteString(`</div>`)
				html.WriteString(`</div>`)
				fmt.Fprintf(&html, `<button class="btn btn-danger" hx-ext="disable-element" `+
					`hx-disable-element="self" hx-delete="/services/%s/instances/%s">Delete</button>`,
					node.Service, node.InstanceName)
				html.WriteString(`</div>`)
			}

			if !hasCards {
				return `<div class="no-nodes">No active nodes</div>`
			}
			return html.String()
		}

		for {
			data := make(map[string]string)

			// Running nodes table: built from registry (all service types)
			allRegions := m.getAllInstanceRegions()

			// Per-region SSE events: only for non-named services (region grid)
			for _, svcType := range services.All {
				if svcType.NamedInstances {
					continue
				}
				svcStatus, err := m.GetStatus(ctx, svcType)
				if err != nil {
					log.Printf("Error getting status for %s: %s", svcType.Name, err)
					continue
				}

				for _, region := range svcStatus.Detail {
					hasNodeKey := fmt.Sprintf("%s-%s-%s-hasnode", region.Service, region.Provider, region.Region)
					buttonKey := fmt.Sprintf("%s-%s-%s-button", region.Service, region.Provider, region.Region)
					createURL := fmt.Sprintf("/services/%s/instances/%s/providers/%s/regions/%s", region.Service, region.InstanceName, region.Provider, region.Region)
					deleteURL := fmt.Sprintf("/services/%s/instances/%s", region.Service, region.InstanceName)
					if region.HasNode {
						pillClass := "pill-green"
						if region.PingStats.SuccessRate < 0.80 {
							pillClass = "pill-yellow"
						}

						data[hasNodeKey] = fmt.Sprintf(`<span class="pill %s">running %s</span>`, pillClass, region.SinceCreated)
						data[buttonKey] = fmt.Sprintf(`<button class="btn btn-danger" hx-ext="disable-element" hx-disable-element="self" hx-delete="%s">Delete</button>`, deleteURL)
					} else {
						disabledFragment := ""
						if region.State == instances.StateLaunching {
							data[hasNodeKey] = fmt.Sprintf(`<span class="pill pill-blue">launching %s...</span>`, durafmt.ParseShort(time.Since(region.LaunchedTS)).InternationalString())
							disabledFragment = "disabled"
						} else if region.State == instances.StateFailed {
							data[hasNodeKey] = `<span class="pill pill-red">failed</span>`
						} else {
							data[hasNodeKey] = ""
						}
						data[buttonKey] = fmt.Sprintf(`<button class="btn btn-create" hx-ext="disable-element" hx-disable-element="self" hx-put="%s" %s>Create</button>`, createURL, disabledFragment)
					}
				}
			}

			// Running nodes table (aggregated across all services)
			runningNodesHTML := buildRunningNodesTableHTML(allRegions)
			runningNodesKey := "running-nodes-table"
			if dataCache[runningNodesKey] != runningNodesHTML {
				fmt.Fprintf(w, "event: %s\n", runningNodesKey)
				fmt.Fprintf(w, "data: %s\n", runningNodesHTML)
				fmt.Fprint(w, "\n\n")
				dataCache[runningNodesKey] = runningNodesHTML
			}

			// Header stats use exit node counts (primary service)
			exitStatus, err := m.GetStatus(ctx, &services.ExitNode)
			if err == nil {
				data["active-nodes"] = fmt.Sprintf("%d", exitStatus.ActiveNodes)
				data["region-count"] = fmt.Sprintf("%d", exitStatus.RegionCount)
				data["provider-count"] = fmt.Sprintf("%d", exitStatus.ProviderCount)
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
		// Build page data with all service types
		var pageData pageTemplateData

		for _, svcType := range services.All {
			svcStatus, err := m.GetStatus(ctx, svcType)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(err.Error()))
				return
			}
			tab := serviceTabData{
				Name:           svcType.Name,
				Label:          svcType.Label,
				NamedInstances: svcType.NamedInstances,
				Regions:        svcStatus.Detail,
			}
			if svcType.NamedInstances {
				tab.Providers = m.buildProviderOptions()
			}
			pageData.Services = append(pageData.Services, tab)
			// Use exit node stats for header
			if svcType.Name == "exit" {
				pageData.ProviderCount = svcStatus.ProviderCount
				pageData.RegionCount = svcStatus.RegionCount
				pageData.ActiveNodes = svcStatus.ActiveNodes
			}
		}

		if err := templates.ExecuteTemplate(w, "list_regions.tmpl", pageData); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
		}
	})

	// Create instance: PUT /services/{service}/instances/{name}/providers/{provider}/regions/{region}
	// Delete instance: DELETE /services/{service}/instances/{name}
	mux.HandleFunc("/services/", func(w http.ResponseWriter, r *http.Request) {
		// Parse path: /services/{service}/instances/{name}[/providers/{provider}/regions/{region}]
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/services/"), "/")

		if len(parts) < 3 || parts[1] != "instances" {
			http.NotFound(w, r)
			return
		}

		serviceName := parts[0]
		instanceName := parts[2]

		svcType := services.ByName(serviceName)
		if svcType == nil {
			http.Error(w, "unknown service: "+serviceName, http.StatusNotFound)
			return
		}

		switch r.Method {
		case "DELETE":
			err := m.instanceRegistry.DeleteInstance(serviceName, instanceName)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(err.Error()))
				return
			}
			fmt.Fprint(w, "ok")

		case "PUT":
			// Expect /providers/{provider}/regions/{region}
			if len(parts) < 7 || parts[3] != "providers" || parts[5] != "regions" {
				http.Error(w, "expected /services/{svc}/instances/{name}/providers/{p}/regions/{r}", http.StatusBadRequest)
				return
			}
			providerName := parts[4]
			region := parts[6]

			// For non-named services, verify the name matches what InstanceName would generate
			if !svcType.NamedInstances {
				expectedName := svcType.InstanceName(services.InstanceNameInput{
					Provider: providerName,
					Region:   region,
				})
				if instanceName != expectedName {
					http.Error(w, fmt.Sprintf("instance name mismatch: got %s, expected %s", instanceName, expectedName), http.StatusBadRequest)
					return
				}
			}

			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Transfer-Encoding", "chunked")
			logger := log.New(io.MultiWriter(httputils.NewFlushWriter(w), os.Stderr), "", log.Lshortfile|log.Lmicroseconds)
			ctx, cancelFunc := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancelFunc()

			err := m.instanceRegistry.CreateInstance(ctx, serviceName, instanceName, providerName, region)
			if err != nil {
				logger.Printf("Failed to create instance: %s", err)
				w.Write([]byte(err.Error()))
			} else {
				logger.Printf("Instance creation initiated")
				w.Write([]byte("ok"))
			}

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.Handle("/assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets.Assets))))
}

// getAllInstanceRegions builds mappedRegion entries from the registry for
// all service types. Used for the running nodes table in the SSE handler.
func (m *Manager) getAllInstanceRegions() []mappedRegion {
	allStatuses := m.instanceRegistry.GetAllInstanceStatuses()
	regions := make([]mappedRegion, 0, len(allStatuses))

	for _, status := range allStatuses {
		svcType := services.ByName(status.Service)

		var sinceCreated, sinceLaunched string
		if status.IsRunning && !status.CreatedAt.IsZero() {
			sinceCreated = durafmt.ParseShort(time.Since(status.CreatedAt)).InternationalString()
		}
		if !status.LaunchedAt.IsZero() {
			sinceLaunched = durafmt.ParseShort(time.Since(status.LaunchedAt)).InternationalString()
		}

		var priceCentsPerHour float64
		if p, ok := m.cloudProviders[status.Provider]; ok {
			priceCentsPerHour = p.GetRegionHourlyEstimate(status.Region) * 100
		}
		if status.HourlyCost > 0 {
			priceCentsPerHour = status.HourlyCost * 100
		}

		var links []resolvedLink
		if svcType != nil {
			links = resolveLinks(svcType.Links, svcType.Hostname(status.InstanceName))
		}

		regions = append(regions, mappedRegion{
			Service:           status.Service,
			ServiceLabel:      status.ServiceLabel,
			InstanceName:      status.InstanceName,
			Provider:          status.Provider,
			ProviderLabel:     providers.ProviderLabels[status.Provider],
			Region:            status.Region,
			LongName:          status.Region, // registry doesn't have long names
			HasNode:           status.IsRunning,
			CreatedTS:         status.CreatedAt,
			LaunchedTS:        status.LaunchedAt,
			State:             status.State,
			LastError:         status.LastError,
			SinceCreated:      sinceCreated,
			SinceLaunched:     sinceLaunched,
			PriceCentsPerHour: priceCentsPerHour,
			PingStats:         status.PingStats,
			NodeStats:         status.NodeStats,
			Links:             links,
		})
	}

	sort.Slice(regions, func(i, j int) bool {
		if regions[i].Service != regions[j].Service {
			return regions[i].Service < regions[j].Service
		}
		return regions[i].InstanceName < regions[j].InstanceName
	})

	return regions
}

// buildProviderOptions builds the provider/region list for named instance dropdowns.
func (m *Manager) buildProviderOptions() []providerOption {
	var opts []providerOption
	for providerName, lazyRegions := range m.lazyListRegionsMap {
		regions := lazyRegions()
		regionOpts := make([]regionOption, len(regions))
		for i, r := range regions {
			regionOpts[i] = regionOption{Code: r.Code, LongName: r.LongName}
		}
		opts = append(opts, providerOption{
			Name:    providerName,
			Label:   providers.ProviderLabels[providerName],
			Regions: regionOpts,
		})
	}
	sort.Slice(opts, func(i, j int) bool { return opts[i].Name < opts[j].Name })
	return opts
}

// resolveLinks substitutes the hostname into each ServiceLink's Format string.
func resolveLinks(links []services.ServiceLink, hostname string) []resolvedLink {
	if len(links) == 0 {
		return nil
	}
	resolved := make([]resolvedLink, len(links))
	for i, l := range links {
		resolved[i] = resolvedLink{
			Label:  l.Label,
			URL:    fmt.Sprintf(l.Format, hostname),
			Render: l.Render,
		}
	}
	return resolved
}

// Shutdown stops all instance controllers and cleans up resources
func (m *Manager) Shutdown() {
	m.instanceRegistry.Shutdown()
}
