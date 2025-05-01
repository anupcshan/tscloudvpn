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
	"strings"
	"syscall"
	"time"

	_ "embed"

	"github.com/anupcshan/tscloudvpn/cmd/tscloudvpn/assets"
	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/digitalocean"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/ec2"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/gcp"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/linode"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/vultr"
	"github.com/bradenaw/juniper/xmaps"
	"github.com/felixge/httpsnoop"

	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

var (
	templates = html_template.Must(html_template.New("root").ParseFS(assets.Assets, "*.tmpl"))
)

const (
	// Time from issuing CreateInstance to when the instance should be available on GetInstanceStatus.
	// This is needed to work around providers that have eventually consistent APIs (like Vultr).
	instanceLaunchDetectTimeout = time.Minute
)

func createInstance(ctx context.Context, logger *log.Logger, controller controlapi.ControlApi, provider providers.Provider, region string) error {
	authKey, err := controller.CreateKey(ctx)
	if err != nil {
		return err
	}

	launchTime := time.Now()

	hostname, err := provider.CreateInstance(ctx, region, authKey)
	if err != nil {
		logger.Printf("Failed to launch instance %s: %s", hostname, err)
		return err
	}

	logger.Printf("Launched instance %s", hostname)
	logger.Printf("Waiting for instance to be listed via provider API")

	startTime := time.Now()
	for {
		if time.Since(startTime) > instanceLaunchDetectTimeout {
			return fmt.Errorf("Instance %s failed to be available in provider API in %s", hostname, instanceLaunchDetectTimeout)
		}

		status, err := provider.GetInstanceStatus(ctx, region)
		if err != nil {
			return err
		}

		if status != providers.InstanceStatusRunning {
			time.Sleep(time.Second)
			continue
		}

		break
	}

	logger.Printf("Instance available in provider API")
	logger.Printf("Waiting for instance to register on Tailscale")

	for {
		time.Sleep(time.Second)

		status, err := provider.GetInstanceStatus(ctx, region)
		if err != nil {
			return err
		}

		if status != providers.InstanceStatusRunning {
			logger.Printf("Instance %s failed to launch", hostname)
			return fmt.Errorf("Instance no longer running")
		}

		devices, err := controller.ListDevices(ctx)
		if err != nil {
			return err
		}

		var deviceId string
		var nodeName string
		for _, device := range devices {
			if device.Hostname == hostname && launchTime.Before(device.Created) {
				deviceId = device.ID
				nodeName = strings.SplitN(device.Name, ".", 2)[0]
				logger.Printf("Instance registered on Tailscale with ID %s, name %s", deviceId, nodeName)
				break
			}
		}

		if deviceId == "" {
			continue
		}

		logger.Printf("Approving exit node %s", nodeName)
		if err := controller.ApproveExitNode(ctx, deviceId); err != nil {
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
	return statusInfo[T]{
		ProviderCount: len(cloudProviders),
		RegionCount:   regionCount,
		ActiveNodes:   len(xmaps.Intersection(deviceMap, expectedHostnameMap)),
		Detail:        t,
	}
}

func initCloudProviders(ctx context.Context, cfg *config.Config) (map[string]providers.Provider, error) {
	cloudProviders := make(map[string]providers.Provider)

	for key, providerFactory := range providers.ProviderFactoryRegistry {
		log.Printf("Processing cloud provider %s", key)
		cloudProvider, err := providerFactory(ctx, cfg)
		if err != nil {
			return nil, err
		}
		if cloudProvider == nil {
			log.Printf("Skipping unconfigured cloud provider %s", key)
			continue
		}

		cloudProviders[key] = cloudProvider
	}

	return cloudProviders, nil
}

var (
	configFile string
)

func init() {
	flag.StringVar(&configFile, "config", "", "Path to config file (default: search in standard locations)")
}

func Main() error {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)
	ctx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()

	var cfg *config.Config
	var err error

	if configFile != "" {
		cfg, err = config.LoadConfig(configFile)
		if err != nil {
			return fmt.Errorf("failed to load config from %s: %v", configFile, err)
		}
	} else {
		cfg, err = config.LoadDefaultConfig()
		if err != nil && err != os.ErrNotExist {
			return fmt.Errorf("failed to load config from default locations: %v", err)
		}

		if err == os.ErrNotExist {
			log.Println("No config file found, falling back to environment variables")
			cfg = config.LoadFromEnv()
		}
	}

	cloudProviders, err := initCloudProviders(ctx, cfg)
	if err != nil {
		return err
	}

	controller, err := cfg.GetController()
	if err != nil {
		return err
	}

	// Create auth key for tsnet server
	authKey, err := controller.CreateKey(ctx)
	if err != nil {
		return err
	}

	tsnetSrv := &tsnet.Server{
		Hostname:   "tscloudvpn",
		Ephemeral:  true,
		AuthKey:    authKey.Key,
		ControlURL: cfg.Control.Headscale.URL,
		Logf:       func(string, ...any) {}, // Silence logspam from tsnet
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

	mgr := NewManager(ctx, cloudProviders, tsLocalClient)

	return mgr.Serve(ctx, ln, controller)
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
