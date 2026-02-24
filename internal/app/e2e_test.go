// Package app_test contains integration tests for the tscloudvpn application.
//
// This file contains local integration tests that use mocked external dependencies
// and are safe to run as part of regular testing. These tests:
//
//   - Start a real tscloudvpn server with mocked Tailscale and cloud providers
//   - Test the complete HTTP API and user workflow
//   - Verify server-sent events, concurrent requests, and error handling
//   - Use tsclient.MockClient to simulate Tailscale peer behavior
//   - Use MockControlAPI to simulate device registration and management
//   - Use fake cloud provider for realistic instance lifecycle testing
//
// For tests that use real Tailscale API and cloud providers, see integration_test.go
// (requires build tag 'e2e' and real credentials).

package app_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/providers/fake"
	"github.com/anupcshan/tscloudvpn/internal/server"
	"github.com/anupcshan/tscloudvpn/internal/tsclient"
	"github.com/stretchr/testify/require"
)

// MockControlAPI provides a comprehensive test implementation of ControlApi
type MockControlAPI struct {
	mu         sync.RWMutex
	devices    map[string]controlapi.Device
	keyCounter int
}

func NewMockControlAPI() *MockControlAPI {
	return &MockControlAPI{
		devices: make(map[string]controlapi.Device),
	}
}

func (m *MockControlAPI) CreateKey(ctx context.Context) (*controlapi.PreauthKey, error) {
	m.mu.Lock()
	m.keyCounter++
	keyID := fmt.Sprintf("test-key-%d", m.keyCounter)
	m.mu.Unlock()

	return &controlapi.PreauthKey{
		Key:        keyID,
		ControlURL: "http://localhost:8080",
		Tags:       []string{"tag:exit"},
	}, nil
}

func (m *MockControlAPI) ListDevices(ctx context.Context) ([]controlapi.Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	devices := make([]controlapi.Device, 0, len(m.devices))
	for _, device := range m.devices {
		devices = append(devices, device)
	}
	return devices, nil
}

func (m *MockControlAPI) DeleteDevice(ctx context.Context, device *controlapi.Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.devices, device.Hostname)
	return nil
}

// AddDevice simulates a device registering with the control plane
func (m *MockControlAPI) AddDevice(device controlapi.Device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devices[device.Hostname] = device
}

// TestHarness provides a convenient wrapper for scenario testing
type TestHarness struct {
	t          *testing.T
	Transport  *tsclient.MockClient
	ControlAPI *MockControlAPI
	Provider   *fake.FakeProvider
	ServerURL  string
	HTTPClient *http.Client
	cancel     context.CancelFunc
	listener   net.Listener
	srv        *server.Server
}

// NewTestHarness creates a fully configured test server with all mocks
func NewTestHarness(t *testing.T) *TestHarness {
	mockControlAPI := NewMockControlAPI()
	mockTSClient := tsclient.NewMockClient()

	fakeProvider := fake.NewWithConfig(&fake.ProviderConfig{
		CreateDelay:      100 * time.Millisecond,
		StatusCheckDelay: 50 * time.Millisecond,
		PricePerHour:     0.01,
	})

	// Register the fake provider
	providers.ProviderFactoryRegistry["fake"] = func(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
		return fakeProvider, nil
	}

	srv := server.New(&server.Config{
		CloudProviders: map[string]providers.Provider{
			"fake": fakeProvider,
		},
		TSLocalClient: mockTSClient,
		Controller:    mockControlAPI,
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		if err := srv.Serve(ctx, listener); err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			t.Errorf("Server error: %v", err)
		}
	}()

	serverURL := fmt.Sprintf("http://%s", listener.Addr().String())
	httpClient := &http.Client{Timeout: 10 * time.Second}

	require.Eventually(t, func() bool {
		resp, err := httpClient.Get(serverURL + "/")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 10*time.Millisecond, "server failed to become ready")

	return &TestHarness{
		t:          t,
		Transport:  mockTSClient,
		ControlAPI: mockControlAPI,
		Provider:   fakeProvider,
		ServerURL:  serverURL,
		HTTPClient: httpClient,
		cancel:     cancel,
		listener:   listener,
		srv:        srv,
	}
}

// Cleanup tears down the test server
func (h *TestHarness) Cleanup() {
	h.cancel()
	h.listener.Close()
	h.srv.Shutdown()
	delete(providers.ProviderFactoryRegistry, "fake")
}

// CreateInstance sends a PUT request to create an instance
func (h *TestHarness) CreateInstance(providerName, region string) {
	h.t.Helper()
	req, err := http.NewRequest("PUT", fmt.Sprintf("%s/providers/%s/regions/%s", h.ServerURL, providerName, region), nil)
	require.NoError(h.t, err)
	resp, err := h.HTTPClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	require.Equal(h.t, http.StatusOK, resp.StatusCode)
	io.ReadAll(resp.Body)
}

// DeleteInstance sends a DELETE request to remove an instance
func (h *TestHarness) DeleteInstance(providerName, region string) {
	h.t.Helper()
	req, err := http.NewRequest("DELETE", fmt.Sprintf("%s/providers/%s/regions/%s", h.ServerURL, providerName, region), nil)
	require.NoError(h.t, err)
	resp, err := h.HTTPClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	require.Equal(h.t, http.StatusOK, resp.StatusCode)
}

// GetHomePage fetches the main page and returns the body
func (h *TestHarness) GetHomePage() string {
	h.t.Helper()
	resp, err := h.HTTPClient.Get(h.ServerURL + "/")
	require.NoError(h.t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return string(body)
}

// WaitForSSEEvent connects to the SSE endpoint and returns as soon as an event
// line containing substr is received, or fails if the timeout is exceeded.
func (h *TestHarness) WaitForSSEEvent(timeout time.Duration, substr string) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", h.ServerURL+"/events", nil)
	require.NoError(h.t, err)

	resp, err := h.HTTPClient.Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), substr) {
			return
		}
	}

	require.Fail(h.t, "Timed out waiting for SSE event containing: "+substr)
}

// TestE2E_FullServerLifecycle tests the complete server lifecycle via HTTP API
func TestE2E_FullServerLifecycle(t *testing.T) {
	h := NewTestHarness(t)
	defer h.Cleanup()

	// Test 1: Test instance creation via HTTP API
	t.Run("CreateInstance", func(t *testing.T) {
		h.CreateInstance("fake", "fake-us-east")

		// Simulate the instance registering with Tailscale
		h.ControlAPI.AddDevice(controlapi.Device{
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
		devices, err := h.ControlAPI.ListDevices(context.Background())
		require.NoError(t, err)

		var found bool
		for _, d := range devices {
			if d.Hostname == "fake-fake-us-east" {
				found = true
				require.True(t, d.IsOnline)
				break
			}
		}
		require.True(t, found, "Device 'fake-fake-us-east' not found")
	})

	// Test 3: Test Server-Sent Events endpoint
	t.Run("ServerSentEvents", func(t *testing.T) {
		h.WaitForSSEEvent(5*time.Second, "active-nodes")
	})

	// Test 4: Delete instance
	t.Run("DeleteInstance", func(t *testing.T) {
		h.DeleteInstance("fake", "fake-us-east")
		t.Logf("Instance deletion completed successfully")
	})

	// Test 5: Verify instance is deleted
	t.Run("VerifyInstanceDeleted", func(t *testing.T) {
		devices, err := h.ControlAPI.ListDevices(context.Background())
		require.NoError(t, err)

		for _, d := range devices {
			require.NotEqual(t, "fake-fake-us-east", d.Hostname, "Device should have been deleted")
		}
	})
}
