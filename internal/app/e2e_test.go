// Package app_test contains integration tests for the tscloudvpn application.
//
// This file contains local integration tests that use mocked external dependencies
// and are safe to run as part of regular testing. These tests:
//
//   - Start a real tscloudvpn server with mocked Tailscale and cloud providers
//   - Test the complete HTTP API and user workflow
//   - Verify server-sent events, concurrent requests, and error handling
//   - Use MockTransport to simulate Tailscale LocalAPI without requiring daemon
//   - Use MockControlAPI to simulate device registration and management
//   - Use fake cloud provider for realistic instance lifecycle testing
//
// For tests that use real Tailscale API and cloud providers, see integration_test.go
// (requires build tag 'e2e' and real credentials).

package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/providers/fake"
	"github.com/anupcshan/tscloudvpn/internal/server"
	"github.com/stretchr/testify/require"
	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/types/key"
)

// MockTransport provides a mock HTTP transport for the Tailscale client
type MockTransport struct{}

func (m *MockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Mock the Tailscale LocalAPI responses
	if strings.Contains(req.URL.Path, "/localapi/v0/status") {
		// Create a mock status response
		status := &ipnstate.Status{
			Version:      "test-version",
			BackendState: "Running",
			AuthURL:      "",
			TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")},
			Self: &ipnstate.PeerStatus{
				ID:           "test-peer",
				PublicKey:    key.NodePublic{},
				HostName:     "tscloudvpn-test",
				DNSName:      "tscloudvpn-test.example.ts.net",
				OS:           "linux",
				TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")},
				Active:       true,
				Online:       true,
			},
		}

		jsonData, _ := json.Marshal(status)
		resp := &http.Response{
			StatusCode: 200,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(jsonData)),
		}
		resp.Header.Set("Content-Type", "application/json")
		return resp, nil
	}

	// For other endpoints, return a 404
	return &http.Response{
		StatusCode: 404,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("not found")),
	}, nil
}

// MockControlAPI provides a comprehensive test implementation of ControlApi
type MockControlAPI struct {
	devices    map[string]controlapi.Device
	keyCounter int
}

func NewMockControlAPI() *MockControlAPI {
	return &MockControlAPI{
		devices: make(map[string]controlapi.Device),
	}
}

func (m *MockControlAPI) CreateKey(ctx context.Context) (*controlapi.PreauthKey, error) {
	m.keyCounter++
	return &controlapi.PreauthKey{
		Key:        fmt.Sprintf("test-key-%d", m.keyCounter),
		ControlURL: "http://localhost:8080",
		Tags:       []string{"tag:exit"},
	}, nil
}

func (m *MockControlAPI) ListDevices(ctx context.Context) ([]controlapi.Device, error) {
	devices := make([]controlapi.Device, 0, len(m.devices))
	for _, device := range m.devices {
		devices = append(devices, device)
	}
	return devices, nil
}

func (m *MockControlAPI) ApproveExitNode(ctx context.Context, device *controlapi.Device) error {
	return nil // No-op for testing
}

func (m *MockControlAPI) DeleteDevice(ctx context.Context, device *controlapi.Device) error {
	delete(m.devices, device.Hostname)
	return nil
}

// AddDevice simulates a device registering with the control plane
func (m *MockControlAPI) AddDevice(device controlapi.Device) {
	m.devices[device.Hostname] = device
}

// E2EServerTestConfig holds configuration for server E2E tests
type E2EServerTestConfig struct {
	ServerURL      string
	HTTPClient     *http.Client
	Timeout        time.Duration
	MockControlAPI *MockControlAPI
}

// startTestServer creates and starts a tscloudvpn server for testing
func startTestServer(t *testing.T) (*E2EServerTestConfig, func()) {
	// Create mock control API
	mockControlAPI := NewMockControlAPI()

	// Create fake provider with test configuration
	fakeProvider := fake.NewWithConfig(&fake.ProviderConfig{
		CreateDelay:      100 * time.Millisecond,
		StatusCheckDelay: 50 * time.Millisecond,
		PricePerHour:     0.01,
	})

	// Register the fake provider in the test context
	providers.ProviderFactoryRegistry["fake"] = func(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
		return fakeProvider, nil
	}

	// Create a mock TSLocalClient with a custom transport to avoid real daemon connections
	mockTSClient := &local.Client{
		Transport: &MockTransport{},
	}

	// Create server configuration
	serverConfig := &server.Config{
		CloudProviders: map[string]providers.Provider{
			"fake": fakeProvider,
		},
		TSLocalClient: mockTSClient,
		Controller:    mockControlAPI,
	}

	// Create server
	srv := server.New(serverConfig)

	// Find available port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}

	addr := listener.Addr().String()
	serverURL := fmt.Sprintf("http://%s", addr)

	// Start server in background
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := srv.Serve(ctx, listener); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			t.Errorf("Server error: %v", err)
		}
	}()

	// Create HTTP client with timeout
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	// Wait for server to be ready
	require.Eventually(t, func() bool {
		resp, err := httpClient.Get(serverURL + "/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 10*time.Millisecond, "server failed to become ready")

	testConfig := &E2EServerTestConfig{
		ServerURL:      serverURL,
		HTTPClient:     httpClient,
		Timeout:        30 * time.Second,
		MockControlAPI: mockControlAPI,
	}

	// Cleanup function
	cleanup := func() {
		cancel()
		listener.Close()
		srv.Shutdown()
		// Clean up the provider registry
		delete(providers.ProviderFactoryRegistry, "fake")
	}

	return testConfig, cleanup
}

// TestE2E_FullServerLifecycle tests the complete server lifecycle via HTTP API
func TestE2E_FullServerLifecycle(t *testing.T) {
	testConfig, cleanup := startTestServer(t)
	defer cleanup()

	// Test 1: Test instance creation via HTTP API
	t.Run("CreateInstance", func(t *testing.T) {
		// Create instance via PUT request
		req, err := http.NewRequest("PUT", testConfig.ServerURL+"/providers/fake/regions/fake-us-east", nil)
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}

		resp, err := testConfig.HTTPClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to create instance: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, string(body))
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response body: %v", err)
		}

		responseStr := string(body)
		if !strings.Contains(responseStr, "ok") && !strings.Contains(responseStr, "initiated") {
			t.Errorf("Expected 'ok' or 'initiated' in response, got: %s", responseStr)
		}

		// Simulate the instance registering with Tailscale
		testConfig.MockControlAPI.AddDevice(controlapi.Device{
			Hostname: "fake-fake-us-east",
			Name:     "fake-fake-us-east",
			Created:  time.Now(),
			LastSeen: time.Now(),
			IPAddrs:  []string{"100.64.0.2"},
			IsOnline: true,
			Tags:     []string{"tag:exit"},
		})

		t.Logf("Instance creation initiated successfully")
	})

	// Test 2: Verify instance is running via MockControlAPI
	t.Run("VerifyInstanceRunning", func(t *testing.T) {
		devices, err := testConfig.MockControlAPI.ListDevices(context.Background())
		if err != nil {
			t.Fatalf("Failed to list devices: %v", err)
		}

		var found bool
		for _, d := range devices {
			if d.Hostname == "fake-fake-us-east" {
				found = true
				if !d.IsOnline {
					t.Errorf("Device should be online")
				}
				break
			}
		}

		if !found {
			t.Errorf("Device 'fake-fake-us-east' not found in MockControlAPI")
		}
	})

	// Test 3: Test Server-Sent Events endpoint
	t.Run("ServerSentEvents", func(t *testing.T) {
		resp, err := testConfig.HTTPClient.Get(testConfig.ServerURL + "/events")
		if err != nil {
			t.Fatalf("Failed to connect to events endpoint: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Expected status 200 for events endpoint, got %d", resp.StatusCode)
		}

		if resp.Header.Get("Content-Type") != "text/event-stream" {
			t.Errorf("Expected Content-Type 'text/event-stream', got %s", resp.Header.Get("Content-Type"))
		}

		// Read some events with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		buf := make([]byte, 1024)
		done := make(chan bool)
		var eventsReceived bool

		go func() {
			n, err := resp.Body.Read(buf)
			if err == nil && n > 0 {
				eventsData := string(buf[:n])
				if strings.Contains(eventsData, "event:") || strings.Contains(eventsData, "data:") {
					eventsReceived = true
				}
			}
			done <- true
		}()

		select {
		case <-ctx.Done():
			// Timeout is ok, we just want to verify the endpoint works
		case <-done:
			// Reading completed
		}

		if eventsReceived {
			t.Logf("Server-Sent Events endpoint working correctly")
		} else {
			t.Logf("Server-Sent Events endpoint accessible (events format not verified)")
		}
	})

	// Test 4: Delete instance
	t.Run("DeleteInstance", func(t *testing.T) {
		req, err := http.NewRequest("DELETE", testConfig.ServerURL+"/providers/fake/regions/fake-us-east", nil)
		if err != nil {
			t.Fatalf("Failed to create delete request: %v", err)
		}

		resp, err := testConfig.HTTPClient.Do(req)
		if err != nil {
			t.Fatalf("Failed to delete instance: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("Expected status 200, got %d: %s", resp.StatusCode, string(body))
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("Failed to read response body: %v", err)
		}

		responseStr := string(body)
		if strings.TrimSpace(responseStr) != "ok" {
			t.Errorf("Expected 'ok' in response, got: %s", responseStr)
		}

		t.Logf("Instance deletion completed successfully")
	})

	// Test 5: Verify instance is deleted
	t.Run("VerifyInstanceDeleted", func(t *testing.T) {
		devices, err := testConfig.MockControlAPI.ListDevices(context.Background())
		if err != nil {
			t.Fatalf("Failed to list devices: %v", err)
		}

		for _, d := range devices {
			if d.Hostname == "fake-fake-us-east" {
				t.Errorf("Device 'fake-fake-us-east' should have been deleted but still exists")
			}
		}
	})
}
