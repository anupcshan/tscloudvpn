package main

import (
	"context"
	"flag"
	"fmt"
	html_template "html/template"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	_ "embed"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/ec2"
	"github.com/anupcshan/tscloudvpn/internal/utils"
	"github.com/bradenaw/juniper/xmaps"
	"github.com/bradenaw/juniper/xslices"
	"github.com/felixge/httpsnoop"
	"github.com/hako/durafmt"

	"github.com/tailscale/tailscale-client-go/tailscale"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

var (
	templates = html_template.Must(html_template.New("root").ParseFS(assets.Assets, "*.tmpl"))
)

func createInstance(ctx context.Context, logger *log.Logger, tsClient *tailscale.Client, provider providers.Provider, region string) error {
	capabilities := tailscale.KeyCapabilities{}
	capabilities.Devices.Create.Tags = []string{"tag:untrusted"}
	capabilities.Devices.Create.Ephemeral = true
	capabilities.Devices.Create.Reusable = false
	capabilities.Devices.Create.Preauthorized = true
	key, err := tsClient.CreateKey(ctx, capabilities)
	if err != nil {
		return err
	}

	launchTime := time.Now()

	hostname, err := provider.CreateInstance(ctx, region, key)
	if err != nil {
		return err
	}

	logger.Printf("Launched instance %s", hostname)
	logger.Printf("Waiting for instance to register on Tailscale")

	for {
		devices, err := tsClient.Devices(ctx)
		if err != nil {
			return err
		}

		var deviceId string
		var nodeName string
		for _, device := range devices {
			if device.Hostname == hostname && launchTime.Before(device.Created.Time) {
				deviceId = device.ID
				nodeName = strings.SplitN(device.Name, ".", 2)[0]
				logger.Printf("Instance registered on Tailscale with ID %s, name %s", deviceId, nodeName)
				break
			}
		}

		if deviceId == "" {
			time.Sleep(time.Second)
			continue
		}

		routes, err := tsClient.DeviceSubnetRoutes(ctx, deviceId)
		if err != nil {
			return err
		}

		logger.Printf("Approving exit node %s", nodeName)
		if err := tsClient.SetDeviceSubnetRoutes(ctx, deviceId, routes.Advertised); err != nil {
			return err
		}

		break
	}

	return nil
}

type flushWriter struct {
	w io.Writer
}

func (f flushWriter) Write(b []byte) (int, error) {
	n, err := f.w.Write(b)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}

	return n, err
}

type statusInfo[T any] struct {
	ProviderCount int
	RegionCount   int
	ActiveNodes   int
	Detail        T
}

func wrapWithStatusInfo[T any](t T, cloudProviders map[string]providers.Provider, lazyListRegionsMap map[string]func() []providers.Region, tsStatus *ipnstate.Status) statusInfo[T] {
	regionCount := 0
	deviceMap := xmaps.Set[string]{}
	expectedHostnameMap := xmaps.Set[string]{}
	for _, peer := range tsStatus.Peer {
		deviceMap.Add(peer.HostName)
	}
	for providerName, f := range lazyListRegionsMap {
		for _, region := range f() {
			regionCount++
			expectedHostnameMap.Add(cloudProviders[providerName].Hostname(region.Code))
		}
	}
	return statusInfo[T]{
		ProviderCount: len(cloudProviders),
		RegionCount:   regionCount,
		ActiveNodes:   len(xmaps.Intersection(deviceMap, expectedHostnameMap)),
		Detail:        t,
	}
}

func Main() error {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	cloudProviders := make(map[string]providers.Provider)

	for key, providerFactory := range providers.ProviderFactoryRegistry {
		log.Printf("Processing cloud provider %s", key)
		cloudProvider, err := providerFactory(ctx)
		if err != nil {
			return err
		}
		if cloudProvider == nil {
			log.Printf("Skipping unconfigured cloud provider %s", key)
			continue
		}

		cloudProviders[key] = cloudProvider
	}

	oauthClientId := os.Getenv("TAILSCALE_CLIENT_ID")
	oauthSecret := os.Getenv("TAILSCALE_CLIENT_SECRET")
	tailnet := os.Getenv("TAILSCALE_TAILNET")

	tsClient, err := tailscale.NewClient(
		"",
		tailnet,
		tailscale.WithOAuthClientCredentials(
			oauthClientId,
			oauthSecret,
			[]string{"devices", "routes"},
		),
	)
	if err != nil {
		return err
	}

	capabilities := tailscale.KeyCapabilities{}
	capabilities.Devices.Create.Tags = []string{"tag:untrusted"}
	capabilities.Devices.Create.Ephemeral = true
	capabilities.Devices.Create.Reusable = true
	capabilities.Devices.Create.Preauthorized = true
	key, err := tsClient.CreateKey(ctx, capabilities)
	if err != nil {
		return err
	}

	tsnetSrv := &tsnet.Server{
		Hostname:  "tscloudvpn",
		Ephemeral: true,
		AuthKey:   key.Key,
		Logf:      func(string, ...any) {}, // Silence logspam from tsnet
	}

	ln, err := tsnetSrv.Listen("tcp", ":80")
	if err != nil {
		return err
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigs
		cancelFunc()
	}()

	defer cancelFunc()

	go func() {
		<-ctx.Done()
		ln.Close()
		tsnetSrv.Close()
	}()

	tsLocalClient, err := tsnetSrv.LocalClient()
	if err != nil {
		return err
	}

	lazyListRegionsMap := make(map[string]func() []providers.Region)

	for providerName, provider := range cloudProviders {
		provider := provider

		lazyListRegionsMap[providerName] = utils.LazyWithErrors(
			func() ([]providers.Region, error) {
				return provider.ListRegions(ctx)
			},
		)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.RedirectHandler("/providers", http.StatusTemporaryRedirect))
	mux.HandleFunc("/providers", func(w http.ResponseWriter, r *http.Request) {
		var providerNames []string
		for providerName := range cloudProviders {
			providerNames = append(providerNames, providerName)
		}
		sort.Strings(providerNames)

		tsStatus, err := tsLocalClient.Status(ctx)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(err.Error()))
			return
		}

		if err := templates.ExecuteTemplate(w, "list_providers.tmpl", wrapWithStatusInfo(providerNames, cloudProviders, lazyListRegionsMap, tsStatus)); err != nil {
			w.Write([]byte(err.Error()))
		}
	})

	for providerName, provider := range cloudProviders {
		provider := provider
		providerName := providerName

		lazyListRegions := lazyListRegionsMap[providerName]

		mux.HandleFunc(fmt.Sprintf("/providers/%s", providerName), func(w http.ResponseWriter, r *http.Request) {
			regions := lazyListRegions()

			tsStatus, err := tsLocalClient.Status(ctx)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				w.Write([]byte(err.Error()))
				return
			}

			deviceMap := make(map[string]*ipnstate.PeerStatus)
			for _, peer := range tsStatus.Peer {
				deviceMap[peer.HostName] = peer
			}

			type mappedRegion struct {
				Provider     string
				Region       string
				LongName     string
				HasNode      bool
				SinceCreated string
				CreatedTS    time.Time
			}

			mappedRegions := xslices.Map(regions, func(region providers.Region) mappedRegion {
				node, hasNode := deviceMap[provider.Hostname(region.Code)]
				var sinceCreated string
				var createdTS time.Time
				if hasNode {
					createdTS = node.Created
					sinceCreated = durafmt.ParseShort(time.Since(node.Created)).String()
				}
				return mappedRegion{
					Provider:     providerName,
					Region:       region.Code,
					LongName:     region.LongName,
					HasNode:      hasNode,
					CreatedTS:    createdTS,
					SinceCreated: sinceCreated,
				}
			})

			if err := templates.ExecuteTemplate(w, "list_regions.tmpl", wrapWithStatusInfo(mappedRegions, cloudProviders, lazyListRegionsMap, tsStatus)); err != nil {
				w.Write([]byte(err.Error()))
			}
		})

		for _, region := range lazyListRegions() {
			region := region
			mux.HandleFunc(fmt.Sprintf("/providers/%s/regions/%s", providerName, region), func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if r.PostFormValue("action") == "delete" {
						ctx := r.Context()
						devices, err := tsClient.Devices(ctx)
						if err != nil {
							w.WriteHeader(http.StatusInternalServerError)
							w.Write([]byte(err.Error()))
							return
						}

						filtered := xslices.Filter(devices, func(device tailscale.Device) bool {
							return device.Hostname == provider.Hostname(region.Code)
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

	return http.Serve(ln, logRequest(mux))
}

func logRequest(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m := httpsnoop.CaptureMetrics(handler, w, r)
		log.Printf(
			"%s %d %s %s %s",
			r.RemoteAddr,
			m.Code,
			r.Method,
			r.URL,
			m.Duration,
		)
	})
}

func main() {
	flag.Parse()

	if err := Main(); err != nil {
		log.Fatal(err)
	}
}
