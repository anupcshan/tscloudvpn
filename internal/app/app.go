package app

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/server"
)

// App represents the main application
type App struct {
	config         *config.Config
	cloudProviders map[string]providers.Provider
	server         *server.Server
	tsnetServer    *server.TSNetServer
}

// New creates a new application instance
func New(configFile string) (*App, error) {
	log.SetFlags(log.Lshortfile | log.Lmicroseconds)

	var cfg *config.Config
	var err error

	if configFile != "" {
		cfg, err = config.LoadConfig(configFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load config from %s: %v", configFile, err)
		}
	} else {
		cfg, err = config.LoadDefaultConfig()
		if err != nil && err != os.ErrNotExist {
			return nil, fmt.Errorf("failed to load config from default locations: %v", err)
		}

		if err == os.ErrNotExist {
			log.Println("No config file found, falling back to environment variables")
			cfg = config.LoadFromEnv()
		}
	}

	return &App{
		config: cfg,
	}, nil
}

// Initialize initializes the application components
func (a *App) Initialize(ctx context.Context) error {
	cloudProviders, err := a.initCloudProviders(ctx)
	if err != nil {
		return err
	}
	a.cloudProviders = cloudProviders

	controller, err := a.config.GetController()
	if err != nil {
		return err
	}

	// Create auth key for tsnet server
	authKey, err := controller.CreateKey(ctx)
	if err != nil {
		return err
	}

	a.tsnetServer = server.NewTSNetServer(authKey.Key, a.config.Control.Headscale.URL)

	tsLocalClient, err := a.tsnetServer.LocalClient()
	if err != nil {
		return err
	}

	a.server = server.New(&server.Config{
		CloudProviders: cloudProviders,
		TSLocalClient:  tsLocalClient,
		Controller:     controller,
	})

	return nil
}

// Run starts the application and blocks until shutdown
func (a *App) Run(ctx context.Context) error {
	ctx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()

	ln, err := a.tsnetServer.Listen("tcp", ":80")
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
		a.tsnetServer.Close()
	}()

	return a.server.Serve(ctx, ln)
}

// initCloudProviders initializes all configured cloud providers
func (a *App) initCloudProviders(ctx context.Context) (map[string]providers.Provider, error) {
	cloudProviders := make(map[string]providers.Provider)

	for key, providerFactory := range providers.ProviderFactoryRegistry {
		log.Printf("Processing cloud provider %s", key)
		cloudProvider, err := providerFactory(ctx, a.config)
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
