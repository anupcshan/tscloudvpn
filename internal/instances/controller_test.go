package instances

import (
	"context"
	"log"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/services"
	"github.com/anupcshan/tscloudvpn/internal/tsclient"
	"github.com/stretchr/testify/require"
)

// Ensure services import is used.
var _ = &services.ExitNode

// MockProvider implements a simple mock provider for testing
type MockProvider struct {
	hostname providers.HostName
	status   providers.InstanceStatus
}

func (m *MockProvider) CreateInstance(ctx context.Context, req providers.CreateRequest) (providers.Instance, error) {
	return providers.Instance{
		Hostname:     string(m.hostname),
		ProviderID:   "mock-123",
		ProviderName: "mock",
	}, nil
}

func (m *MockProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	return m.status, nil
}

func (m *MockProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	return []providers.Region{{Code: "test-region", LongName: "Test Region"}}, nil
}

func (m *MockProvider) Hostname(region string) providers.HostName {
	return m.hostname
}

func (m *MockProvider) GetRegionHourlyEstimate(region string) float64 {
	return 0.05
}

func (m *MockProvider) DeleteInstance(ctx context.Context, instanceID providers.Instance) error {
	return nil
}

func (m *MockProvider) ListInstances(ctx context.Context, region string) ([]providers.Instance, error) {
	if m.status == providers.InstanceStatusRunning {
		return []providers.Instance{{
			Hostname:     string(m.hostname),
			ProviderID:   "mock-123",
			ProviderName: "mock",
		}}, nil
	}
	return []providers.Instance{}, nil
}

// MockControlApi implements a simple mock control API for testing
type MockControlApi struct {
	mu      sync.RWMutex
	devices []controlapi.Device
}

func (m *MockControlApi) CreateKey(ctx context.Context, tags []string) (*controlapi.PreauthKey, error) {
	return &controlapi.PreauthKey{Key: "mock-key"}, nil
}

func (m *MockControlApi) ListDevices(ctx context.Context) ([]controlapi.Device, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.devices, nil
}

func (m *MockControlApi) DeleteDevice(ctx context.Context, device *controlapi.Device) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Remove device from mock list
	for i, d := range m.devices {
		if d.Hostname == device.Hostname {
			m.devices = slices.Delete(m.devices, i, i+1)
			return nil
		}
	}
	return nil
}

// AddDevice adds a device to the mock (for testing)
func (m *MockControlApi) AddDevice(device controlapi.Device) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.devices = append(m.devices, device)
}

func TestController_NewController(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()

	controller := NewController(ctx, logger, providers.HostName("test-instance"), &services.ExitNode, nil)
	controller.Start()
	defer controller.Stop()

	if controller == nil {
		t.Fatal("NewController returned nil")
	}

	status := controller.Status()
	if status.Hostname != "test-instance" {
		t.Errorf("Expected hostname 'test-instance', got %s", status.Hostname)
	}
	if status.State != StateIdle {
		t.Errorf("Expected instance state to be StateIdle, got %d", status.State)
	}
	if status.PingStats.SuccessRate != 0 {
		t.Error("Expected initial success rate to be 0")
	}
}

func TestRegistry_CreateAndDeleteInstance(t *testing.T) {
	logger := log.Default()
	controlApi := &MockControlApi{}
	providers := map[string]providers.Provider{
		"mock": &MockProvider{
			hostname: "mock-test-region",
			status:   providers.InstanceStatusMissing,
		},
	}

	registry := NewRegistry(logger, "", controlApi, nil, providers)
	defer registry.Shutdown()

	ctx := context.Background()

	// Test creating an instance
	err := registry.CreateInstance(ctx, "exit", "mock", "test-region")
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Test getting instance status
	status, err := registry.GetInstanceStatus("exit", "mock", "test-region")
	if err != nil {
		t.Fatalf("Failed to get instance status: %v", err)
	}

	if status.Provider != "mock" {
		t.Errorf("Expected provider 'mock', got %s", status.Provider)
	}
	if status.Region != "test-region" {
		t.Errorf("Expected region 'test-region', got %s", status.Region)
	}

	// Add a mock device to simulate successful creation
	controlApi.AddDevice(controlapi.Device{
		Hostname: "mock-test-region",
	})

	// Test deleting the instance
	err = registry.DeleteInstance("exit", "mock", "test-region")
	if err != nil {
		t.Fatalf("Failed to delete instance: %v", err)
	}

	// Verify instance is gone
	_, err = registry.GetInstanceStatus("exit", "mock", "test-region")
	if err == nil {
		t.Error("Expected error when getting status of deleted instance")
	}
}

func TestRegistry_GetAllInstanceStatuses(t *testing.T) {
	logger := log.Default()
	controlApi := &MockControlApi{}
	providers := map[string]providers.Provider{
		"mock1": &MockProvider{
			hostname: "mock1-test-region",
			status:   providers.InstanceStatusMissing,
		},
		"mock2": &MockProvider{
			hostname: "mock2-test-region",
			status:   providers.InstanceStatusMissing,
		},
	}

	registry := NewRegistry(logger, "", controlApi, nil, providers)
	defer registry.Shutdown()

	ctx := context.Background()

	// Create instances
	err := registry.CreateInstance(ctx, "exit", "mock1", "test-region")
	if err != nil {
		t.Fatalf("Failed to create instance 1: %v", err)
	}

	err = registry.CreateInstance(ctx, "exit", "mock2", "test-region")
	if err != nil {
		t.Fatalf("Failed to create instance 2: %v", err)
	}

	// Get all statuses
	statuses := registry.GetAllInstanceStatuses()

	if len(statuses) != 2 {
		t.Errorf("Expected 2 instances, got %d", len(statuses))
	}

	// Verify both instances are present
	hasMock1 := false
	hasMock2 := false
	for key, status := range statuses {
		if key == "exit-mock1-test-region" && status.Provider == "mock1" {
			hasMock1 = true
		}
		if key == "exit-mock2-test-region" && status.Provider == "mock2" {
			hasMock2 = true
		}
	}

	if !hasMock1 {
		t.Error("mock1 instance not found in all statuses")
	}
	if !hasMock2 {
		t.Error("mock2 instance not found in all statuses")
	}
}

func TestRegistry_DiscoverExistingInstances(t *testing.T) {
	logger := log.Default()
	tsClient := tsclient.NewMockClient()
	controlApi := &MockControlApi{
		// Pre-populate with existing devices (with service tags)
		devices: []controlapi.Device{
			{
				Hostname: "mock1-test-region",
				Created:  time.Now().Add(-time.Hour), // Created 1 hour ago
				Tags:     services.ExitNode.Tags,
			},
			{
				Hostname: "mock2-other-region",
				Created:  time.Now().Add(-30 * time.Minute), // Created 30 minutes ago
				Tags:     services.ExitNode.Tags,
			},
		},
	}

	// Set up identity responses for discovery
	tsClient.SetNodeIdentity("mock1-test-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock1", Region: "test-region",
	})
	tsClient.SetNodeIdentity("mock2-other-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock2", Region: "other-region",
	})
	tsClient.AddPeer("mock1-test-region", netip.MustParseAddr("100.64.0.1"))
	tsClient.AddPeer("mock2-other-region", netip.MustParseAddr("100.64.0.2"))

	providers := map[string]providers.Provider{
		"mock1": &MockProvider{
			hostname: "mock1-test-region",
			status:   providers.InstanceStatusRunning,
		},
		"mock2": &MockProvider{
			hostname: "mock2-other-region",
			status:   providers.InstanceStatusRunning,
		},
	}

	registry := NewRegistry(logger, "", controlApi, tsClient, providers)
	defer registry.Shutdown()
	registry.Start(context.Background())

	// Wait for discovery to complete - use Eventually since discovery is async
	require.Eventually(t, func() bool {
		return len(registry.GetAllInstanceStatuses()) == 2
	}, 5*time.Second, 10*time.Millisecond, "Expected 2 discovered instances")

	allStatuses := registry.GetAllInstanceStatuses()

	// Verify the discovered instances have correct status
	for key, status := range allStatuses {
		if status.State != StateRunning {
			t.Errorf("Discovered instance %s should have state StateRunning, got %d", key, status.State)
		}
		if status.CreatedAt.IsZero() {
			t.Errorf("Discovered instance %s should have creation time set", key)
		}
	}

	// Test that creating an instance that already exists doesn't duplicate it
	ctx := context.Background()
	err := registry.CreateInstance(ctx, "exit", "mock1", "test-region")
	if err != nil {
		t.Errorf("Creating existing instance should not fail: %v", err)
	}

	// Should still have only 2 instances
	allStatuses = registry.GetAllInstanceStatuses()
	if len(allStatuses) != 2 {
		t.Errorf("Expected 2 instances after trying to create existing one, got %d", len(allStatuses))
	}
}

func TestRegistry_PeriodicDiscovery(t *testing.T) {
	logger := log.Default()
	tsClient := tsclient.NewMockClient()
	controlApi := &MockControlApi{
		devices: []controlapi.Device{},
	}

	providerMap := map[string]providers.Provider{
		"mock": &MockProvider{
			hostname: "mock-test-region",
			status:   providers.InstanceStatusRunning,
		},
	}

	// Set up identity for when the device appears
	tsClient.SetNodeIdentity("mock-test-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock", Region: "test-region",
	})
	tsClient.AddPeer("mock-test-region", netip.MustParseAddr("100.64.0.1"))

	registry := NewRegistry(logger, "", controlApi, tsClient, providerMap)
	defer registry.Shutdown()

	ctx := context.Background()

	// Initial discovery finds nothing
	registry.discoverInstances(ctx)
	require.Equal(t, 0, len(registry.GetAllInstanceStatuses()))

	// Simulate another tscloudvpn instance creating a node (with tags)
	controlApi.AddDevice(controlapi.Device{
		Hostname: "mock-test-region",
		Created:  time.Now().Add(-time.Minute),
		Tags:     services.ExitNode.Tags,
	})

	// Next discovery finds the new instance
	registry.discoverInstances(ctx)
	require.Equal(t, 1, len(registry.GetAllInstanceStatuses()))

	status, err := registry.GetInstanceStatus("exit", "mock", "test-region")
	require.NoError(t, err)
	require.Equal(t, StateRunning, status.State)
	require.False(t, status.CreatedAt.IsZero())
}

func TestRegistry_PeriodicDiscovery_Idempotent(t *testing.T) {
	logger := log.Default()
	tsClient := tsclient.NewMockClient()
	controlApi := &MockControlApi{
		devices: []controlapi.Device{
			{
				Hostname: "mock-test-region",
				Created:  time.Now().Add(-time.Hour),
				Tags:     services.ExitNode.Tags,
			},
		},
	}

	providerMap := map[string]providers.Provider{
		"mock": &MockProvider{
			hostname: "mock-test-region",
			status:   providers.InstanceStatusRunning,
		},
	}

	tsClient.SetNodeIdentity("mock-test-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock", Region: "test-region",
	})
	tsClient.AddPeer("mock-test-region", netip.MustParseAddr("100.64.0.1"))

	registry := NewRegistry(logger, "", controlApi, tsClient, providerMap)
	defer registry.Shutdown()

	ctx := context.Background()

	// Run discovery 3 times
	registry.discoverInstances(ctx)
	registry.discoverInstances(ctx)
	registry.discoverInstances(ctx)

	// Should still have exactly 1 instance
	require.Equal(t, 1, len(registry.GetAllInstanceStatuses()))
}

func TestRegistry_Discovery_SkipsUntaggedDevices(t *testing.T) {
	logger := log.Default()
	tsClient := tsclient.NewMockClient()
	controlApi := &MockControlApi{
		devices: []controlapi.Device{
			{
				Hostname: "mock-test-region",
				Created:  time.Now().Add(-time.Hour),
				Tags:     services.ExitNode.Tags, // Tagged — should be discovered
			},
			{
				Hostname: "personal-laptop",
				Created:  time.Now().Add(-24 * time.Hour),
				Tags:     nil, // No tags — should be skipped
			},
			{
				Hostname: "other-service",
				Created:  time.Now().Add(-2 * time.Hour),
				Tags:     []string{"tag:other"}, // Wrong tag — should be skipped
			},
		},
	}

	providerMap := map[string]providers.Provider{
		"mock": &MockProvider{
			hostname: "mock-test-region",
			status:   providers.InstanceStatusRunning,
		},
	}

	tsClient.SetNodeIdentity("mock-test-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock", Region: "test-region",
	})
	tsClient.AddPeer("mock-test-region", netip.MustParseAddr("100.64.0.1"))

	registry := NewRegistry(logger, "", controlApi, tsClient, providerMap)
	defer registry.Shutdown()

	registry.discoverInstances(context.Background())

	// Only the tagged device should be discovered
	require.Equal(t, 1, len(registry.GetAllInstanceStatuses()))
	_, err := registry.GetInstanceStatus("exit", "mock", "test-region")
	require.NoError(t, err)
}

func TestRegistry_Discovery_SkipsUnknownProvider(t *testing.T) {
	logger := log.Default()
	tsClient := tsclient.NewMockClient()
	controlApi := &MockControlApi{
		devices: []controlapi.Device{
			{
				Hostname: "unknown-us-east",
				Created:  time.Now().Add(-time.Hour),
				Tags:     services.ExitNode.Tags,
			},
		},
	}

	// Identity reports a provider we don't have
	tsClient.SetNodeIdentity("unknown-us-east", &tsclient.NodeIdentity{
		Service: "exit", Provider: "aws", Region: "us-east-1",
	})

	providerMap := map[string]providers.Provider{
		"mock": &MockProvider{hostname: "mock-test-region"},
	}

	registry := NewRegistry(logger, "", controlApi, tsClient, providerMap)
	defer registry.Shutdown()

	registry.discoverInstances(context.Background())

	// Should discover nothing — provider "aws" is not registered
	require.Equal(t, 0, len(registry.GetAllInstanceStatuses()))
}

func TestController_IdleShutdown_StatsIdle(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()

	controller := NewController(ctx, logger, providers.HostName("test-instance"), &services.ExitNode, nil)
	controller.Start()
	defer controller.Stop()

	controller.state = StateRunning
	controller.nodeStats = &NodeStats{
		ForwardedBytes: 3000,
		LastActive:     time.Now().Add(-5 * time.Hour),
	}

	require.True(t, controller.shouldIdleShutdown(), "Should trigger idle shutdown when stats show idle > 4h")
}

func TestController_IdleShutdown_StatsActive(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()

	controller := NewController(ctx, logger, providers.HostName("test-instance"), &services.ExitNode, nil)
	controller.Start()
	defer controller.Stop()

	controller.state = StateRunning
	controller.nodeStats = &NodeStats{
		ForwardedBytes: 3000,
		LastActive:     time.Now().Add(-10 * time.Minute),
	}

	require.False(t, controller.shouldIdleShutdown(), "Should not idle shutdown when stats show recent activity")
}

func TestController_IdleShutdown_NoStatsWatchdogExpired(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()

	controller := NewController(ctx, logger, providers.HostName("test-instance"), &services.ExitNode, nil)
	controller.Start()
	defer controller.Stop()

	controller.state = StateRunning
	controller.watchingSince = time.Now().Add(-9 * time.Hour) // watching for > 8h

	require.True(t, controller.shouldIdleShutdown(), "Should trigger idle shutdown when no stats and watched > 8h")
}

func TestController_IdleShutdown_NoStatsWatchdogNotExpired(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()

	controller := NewController(ctx, logger, providers.HostName("test-instance"), &services.ExitNode, nil)
	controller.Start()
	defer controller.Stop()

	controller.state = StateRunning
	// watchingSince defaults to time.Now() in NewController, so < 8h

	require.False(t, controller.shouldIdleShutdown(), "Should not idle shutdown when no stats and watched < 8h")
}

func TestController_IdleShutdown_NotRunning(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()

	controller := NewController(ctx, logger, providers.HostName("test-instance"), &services.ExitNode, nil)
	controller.Start()
	defer controller.Stop()

	// State is StateIdle (default)
	controller.nodeStats = &NodeStats{
		LastActive: time.Now().Add(-5 * time.Hour),
	}

	require.False(t, controller.shouldIdleShutdown(), "Should not idle shutdown when not in StateRunning")
}

func TestController_NodeStats(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()

	controller := NewController(ctx, logger, providers.HostName("test-instance"), &services.ExitNode, nil)
	controller.Start()
	defer controller.Stop()

	// Initially no stats
	status := controller.Status()
	require.Nil(t, status.NodeStats, "NodeStats should be nil initially")

	lastActive := time.Now().Add(-5 * time.Minute)
	controller.nodeStats = &NodeStats{
		ForwardedBytes: 3000,
		LastActive:     lastActive,
		ReceivedAt:     time.Now(),
	}

	status = controller.Status()
	require.NotNil(t, status.NodeStats, "NodeStats should be set after reporting")
	require.Equal(t, int64(3000), status.NodeStats.ForwardedBytes)
	require.Equal(t, lastActive.Unix(), status.NodeStats.LastActive.Unix())
	require.False(t, status.NodeStats.ReceivedAt.IsZero(), "ReceivedAt should be set")
}

func TestRegistry_IdleShutdownCallback(t *testing.T) {
	logger := log.Default()
	controlApi := &MockControlApi{}

	shutdownCalled := make(chan struct{}, 1)
	provider := &MockProvider{
		hostname: "mock-test-region",
	}
	providerMap := map[string]providers.Provider{
		"mock": provider,
	}

	registry := NewRegistry(logger, "", controlApi, nil, providerMap)
	defer registry.Shutdown()

	ctx := context.Background()
	err := registry.CreateInstance(ctx, "exit", "mock", "test-region")
	require.NoError(t, err)

	// Manually set the callback to a test channel to verify it's wired
	registry.mu.RLock()
	controller := registry.controllers["exit-mock-test-region"].controller
	registry.mu.RUnlock()

	controller.mu.Lock()
	controller.onIdleShutdown = func() {
		shutdownCalled <- struct{}{}
	}
	controller.state = StateRunning
	controller.nodeStats = &NodeStats{
		LastActive: time.Now().Add(-5 * time.Hour),
	}
	controller.mu.Unlock()

	controller.mu.RLock()
	require.True(t, controller.shouldIdleShutdown())
	controller.mu.RUnlock()
}

func TestDiscoveredController_RemovedWhenPeerDisappears(t *testing.T) {
	t.Parallel()
	logger := log.Default()
	tsClient := tsclient.NewMockClient()
	controlApi := &MockControlApi{}
	provider := &MockProvider{
		hostname: "mock-test-region",
		status:   providers.InstanceStatusRunning,
	}
	providerMap := map[string]providers.Provider{"mock": provider}

	registry := NewRegistry(logger, "", controlApi, tsClient, providerMap)
	defer registry.Shutdown()

	// Simulate a device in the control plane and a visible peer
	controlApi.AddDevice(controlapi.Device{
		Hostname: "mock-test-region",
		Created:  time.Now().Add(-time.Minute),
		Tags:     services.ExitNode.Tags,
	})
	tsClient.SetNodeIdentity("mock-test-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock", Region: "test-region",
	})
	tsClient.AddPeer("mock-test-region", netip.MustParseAddr("100.64.0.1"))

	// Discovery creates a controller
	registry.discoverInstances(context.Background())
	require.Equal(t, 1, len(registry.GetAllInstanceStatuses()))

	// Peer disappears
	tsClient.RemovePeer("mock-test-region")

	// Wait for the health check to notice and remove the controller
	require.Eventually(t, func() bool {
		return len(registry.GetAllInstanceStatuses()) == 0
	}, 5*time.Second, 50*time.Millisecond,
		"Controller should be removed when peer disappears")
}

func TestDiscoveredController_StaleStatsNotAppliedToNewInstance(t *testing.T) {
	t.Parallel()
	logger := log.Default()
	tsClient := tsclient.NewMockClient()
	controlApi := &MockControlApi{}
	provider := &MockProvider{
		hostname: "mock-test-region",
		status:   providers.InstanceStatusRunning,
	}
	providerMap := map[string]providers.Provider{"mock": provider}

	registry := NewRegistry(logger, "", controlApi, tsClient, providerMap)
	defer registry.Shutdown()

	// Phase 1: First instance appears and gets stats
	controlApi.AddDevice(controlapi.Device{
		Hostname: "mock-test-region",
		Created:  time.Now().Add(-time.Minute),
		Tags:     services.ExitNode.Tags,
	})
	tsClient.SetNodeIdentity("mock-test-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock", Region: "test-region",
	})
	tsClient.AddPeer("mock-test-region", netip.MustParseAddr("100.64.0.1"))
	tsClient.SetNodeStats(&tsclient.NodeStatsResult{
		ForwardedBytes: 100,
		LastActive:     time.Now(), // Recently active
	})

	registry.discoverInstances(context.Background())
	require.Equal(t, 1, len(registry.GetAllInstanceStatuses()))

	// Wait for health check to fetch stats
	require.Eventually(t, func() bool {
		status, err := registry.GetInstanceStatus("exit", "mock", "test-region")
		return err == nil && status.NodeStats != nil
	}, 5*time.Second, 50*time.Millisecond,
		"Stats should be fetched")

	// First instance disappears
	tsClient.RemovePeer("mock-test-region")

	// Wait for controller to be removed
	require.Eventually(t, func() bool {
		return len(registry.GetAllInstanceStatuses()) == 0
	}, 5*time.Second, 50*time.Millisecond,
		"Controller should be removed when peer disappears")

	// Phase 2: New instance appears with fresh stats
	tsClient.SetNodeStats(&tsclient.NodeStatsResult{
		ForwardedBytes: 0,
		LastActive:     time.Now(), // Just booted
	})
	tsClient.AddPeer("mock-test-region", netip.MustParseAddr("100.64.0.1"))

	// Discovery creates a fresh controller
	registry.discoverInstances(context.Background())
	require.Equal(t, 1, len(registry.GetAllInstanceStatuses()))

	// The new controller should NOT trigger idle shutdown — it has no stale stats
	time.Sleep(2 * time.Second)

	// Controller should still exist (not idle-shutdown)
	require.Equal(t, 1, len(registry.GetAllInstanceStatuses()),
		"New instance should not be killed by stale stats from old instance")
}

func TestDiscoveredController_IdleShutdownCallbackNotFiredAfterPeerGone(t *testing.T) {
	t.Parallel()
	logger := log.Default()
	tsClient := tsclient.NewMockClient()
	controlApi := &MockControlApi{}
	provider := &MockProvider{
		hostname: "mock-test-region",
		status:   providers.InstanceStatusRunning,
	}
	providerMap := map[string]providers.Provider{"mock": provider}

	registry := NewRegistry(logger, "", controlApi, tsClient, providerMap)
	defer registry.Shutdown()

	// Set up a discovered instance with stats that would trigger idle shutdown
	controlApi.AddDevice(controlapi.Device{
		Hostname: "mock-test-region",
		Created:  time.Now().Add(-time.Minute),
		Tags:     services.ExitNode.Tags,
	})
	tsClient.SetNodeIdentity("mock-test-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock", Region: "test-region",
	})
	tsClient.AddPeer("mock-test-region", netip.MustParseAddr("100.64.0.1"))
	tsClient.SetNodeStats(&tsclient.NodeStatsResult{
		ForwardedBytes: 100,
		LastActive:     time.Now().Add(-5 * time.Hour), // Would trigger idle shutdown
	})

	idleShutdownCalled := make(chan struct{}, 1)
	registry.discoverInstances(context.Background())

	// Override the idle shutdown callback to track if it fires
	registry.mu.Lock()
	controller := registry.controllers["exit-mock-test-region"].controller
	controller.mu.Lock()
	controller.onIdleShutdown = func() { idleShutdownCalled <- struct{}{} }
	controller.mu.Unlock()
	registry.mu.Unlock()

	// Peer disappears before idle shutdown fires
	tsClient.RemovePeer("mock-test-region")

	// Wait for controller removal
	require.Eventually(t, func() bool {
		return len(registry.GetAllInstanceStatuses()) == 0
	}, 5*time.Second, 50*time.Millisecond,
		"Controller should be removed when peer disappears")

	// Idle shutdown should NOT have been called — controller was removed first
	select {
	case <-idleShutdownCalled:
		t.Fatal("Idle shutdown should not fire after peer is gone and controller is removed")
	case <-time.After(2 * time.Second):
		// Good — no idle shutdown fired
	}
}

func TestRegistry_CreateInstance_ContextCancellation(t *testing.T) {
	logger := log.Default()
	controlApi := &MockControlApi{}
	providers := map[string]providers.Provider{
		"mock": &MockProvider{
			hostname: "mock-test-region",
			status:   providers.InstanceStatusMissing,
		},
	}

	registry := NewRegistry(logger, "", controlApi, nil, providers)
	defer registry.Shutdown()

	// Create a context that we'll cancel immediately
	ctx, cancel := context.WithCancel(context.Background())

	// Start instance creation
	err := registry.CreateInstance(ctx, "exit", "mock", "test-region")
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Cancel the context immediately after starting creation
	cancel()

	// Verify that instance creation wasn't affected by context cancellation
	status, err := registry.GetInstanceStatus("exit", "mock", "test-region")
	if err != nil {
		t.Fatalf("Failed to get instance status: %v", err)
	}

	// The instance should exist and creation should have proceeded despite context cancellation
	if status.Provider != "mock" {
		t.Errorf("Expected provider 'mock', got %s", status.Provider)
	}
	if status.Region != "test-region" {
		t.Errorf("Expected region 'test-region', got %s", status.Region)
	}
}
