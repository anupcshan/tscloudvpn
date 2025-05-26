package server

import (
	"context"
	"log"
	"net"
	"net/http"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	httputils "github.com/anupcshan/tscloudvpn/internal/http"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"tailscale.com/client/local"
	"tailscale.com/tsnet"
)

// Config holds the configuration for the server
type Config struct {
	CloudProviders map[string]providers.Provider
	TSLocalClient  *local.Client
	Controller     controlapi.ControlApi
}

// Server handles HTTP requests for the application
type Server struct {
	config  *Config
	manager *Manager
}

// New creates a new server with the given configuration
func New(config *Config) *Server {
	return &Server{
		config:  config,
		manager: NewManager(context.Background(), config.CloudProviders, config.TSLocalClient),
	}
}

// Serve starts the HTTP server on the given listener
func (s *Server) Serve(ctx context.Context, listen net.Listener) error {
	mux := s.setupRoutes(ctx)
	log.Printf("Listening on %s", listen.Addr())
	return http.Serve(listen, httputils.LogRequest(mux))
}

// setupRoutes configures all HTTP routes
func (s *Server) setupRoutes(ctx context.Context) *http.ServeMux {
	mux := http.NewServeMux()

	// Setup all routes through the manager
	s.manager.SetupRoutes(ctx, mux, s.config.Controller)

	return mux
}

// TSNetServer creates and configures a tsnet server
type TSNetServer struct {
	server *tsnet.Server
}

// NewTSNetServer creates a new tsnet server with the given auth key and control URL
func NewTSNetServer(authKey, controlURL string) *TSNetServer {
	tsnetSrv := &tsnet.Server{
		Hostname:   "tscloudvpn",
		Ephemeral:  true,
		AuthKey:    authKey,
		ControlURL: controlURL,
		Logf:       func(string, ...any) {}, // Silence logspam from tsnet
	}

	return &TSNetServer{server: tsnetSrv}
}

// Listen starts listening on the specified network and address
func (ts *TSNetServer) Listen(network, address string) (net.Listener, error) {
	return ts.server.Listen(network, address)
}

// LocalClient returns a Tailscale local client
func (ts *TSNetServer) LocalClient() (*local.Client, error) {
	return ts.server.LocalClient()
}

// Close closes the tsnet server
func (ts *TSNetServer) Close() error {
	return ts.server.Close()
}
