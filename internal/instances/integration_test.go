package instances

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/providers/fake"
	"github.com/anupcshan/tscloudvpn/internal/services"
	"github.com/anupcshan/tscloudvpn/internal/tsclient"
	"github.com/stretchr/testify/require"
)

// IntegrationTestControlApi implements a more realistic control API for integration testing
type IntegrationTestControlApi struct {
	mu                sync.RWMutex
	devices           []controlapi.Device
	keyCounter        int
	createKeyDelay    time.Duration
	createKeyError    error
	listDevicesDelay  time.Duration
	listDevicesError  error
	deleteDeviceDelay time.Duration
	deleteDeviceError error
}

// NewIntegrationTestControlApi creates a new test control API
func NewIntegrationTestControlApi() *IntegrationTestControlApi {
	return &IntegrationTestControlApi{
		devices: make([]controlapi.Device, 0),
	}
}

// CreateKey creates a preauth key with optional delay/failure
func (api *IntegrationTestControlApi) CreateKey(ctx context.Context, tags []string) (*controlapi.PreauthKey, error) {
	if api.createKeyDelay > 0 {
		select {
		case <-time.After(api.createKeyDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if api.createKeyError != nil {
		return nil, api.createKeyError
	}

	api.mu.Lock()
	api.keyCounter++
	keyID := fmt.Sprintf("test-key-%d", api.keyCounter)
	api.mu.Unlock()

	return &controlapi.PreauthKey{
		Key: keyID,
	}, nil
}

// ListDevices returns all devices with optional delay/failure
func (api *IntegrationTestControlApi) ListDevices(ctx context.Context) ([]controlapi.Device, error) {
	if api.listDevicesDelay > 0 {
		select {
		case <-time.After(api.listDevicesDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	if api.listDevicesError != nil {
		return nil, api.listDevicesError
	}

	api.mu.RLock()
	defer api.mu.RUnlock()

	// Return a copy
	devices := make([]controlapi.Device, len(api.devices))
	copy(devices, api.devices)
	return devices, nil
}

// DeleteDevice removes a device by ID with optional delay/failure
func (api *IntegrationTestControlApi) DeleteDevice(ctx context.Context, device *controlapi.Device) error {
	if api.deleteDeviceDelay > 0 {
		select {
		case <-time.After(api.deleteDeviceDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if api.deleteDeviceError != nil {
		return api.deleteDeviceError
	}

	api.mu.Lock()
	defer api.mu.Unlock()

	for i, d := range api.devices {
		if d.Hostname == device.Hostname {
			api.devices = slices.Delete(api.devices, i, i+1)
			return nil
		}
	}

	return fmt.Errorf("device not found: %s", device.Hostname)
}

// AddDevice adds a device to the mock (for testing)
func (api *IntegrationTestControlApi) AddDevice(device controlapi.Device) {
	api.mu.Lock()
	defer api.mu.Unlock()
	api.devices = append(api.devices, device)
}

// RemoveDevice removes a device by hostname (for testing)
func (api *IntegrationTestControlApi) RemoveDevice(hostname string) {
	api.mu.Lock()
	defer api.mu.Unlock()
	for i, d := range api.devices {
		if d.Hostname == hostname {
			api.devices = slices.Delete(api.devices, i, i+1)
			return
		}
	}
}

// SetCreateKeyDelay sets delay for CreateKey operations
func (api *IntegrationTestControlApi) SetCreateKeyDelay(delay time.Duration) {
	api.mu.Lock()
	api.createKeyDelay = delay
	api.mu.Unlock()
}

// SetCreateKeyError sets error for CreateKey operations
func (api *IntegrationTestControlApi) SetCreateKeyError(err error) {
	api.mu.Lock()
	api.createKeyError = err
	api.mu.Unlock()
}

// SetListDevicesDelay sets delay for ListDevices operations
func (api *IntegrationTestControlApi) SetListDevicesDelay(delay time.Duration) {
	api.mu.Lock()
	api.listDevicesDelay = delay
	api.mu.Unlock()
}

// SetListDevicesError sets error for ListDevices operations
func (api *IntegrationTestControlApi) SetListDevicesError(err error) {
	api.mu.Lock()
	api.listDevicesError = err
	api.mu.Unlock()
}

// SetDeleteDeviceDelay sets delay for DeleteDevice operations
func (api *IntegrationTestControlApi) SetDeleteDeviceDelay(delay time.Duration) {
	api.mu.Lock()
	api.deleteDeviceDelay = delay
	api.mu.Unlock()
}

// SetDeleteDeviceError sets error for DeleteDevice operations
func (api *IntegrationTestControlApi) SetDeleteDeviceError(err error) {
	api.mu.Lock()
	api.deleteDeviceError = err
	api.mu.Unlock()
}

func TestIntegration_RegistryWithFakeProvider_BasicLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)
	fakeProvider := fake.NewWithConfig(fake.DefaultConfig())
	controlApi := NewIntegrationTestControlApi()

	providerMap := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	registry := NewRegistry(logger, "", controlApi, nil, providerMap)
	defer registry.Shutdown()

	// Test instance creation via registry
	err := registry.CreateInstance(ctx, "fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// After creation, state should be Launching
	status, err := registry.GetInstanceStatus("fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to get instance status: %v", err)
	}
	if status.State != StateLaunching {
		t.Errorf("Expected instance state to be StateLaunching, got %d", status.State)
	}
	if status.Hostname != "fake-fake-us-east" {
		t.Errorf("Expected hostname 'fake-fake-us-east', got %s", status.Hostname)
	}

	// Wait for instance to be created in the fake provider
	require.Eventually(t, func() bool {
		_, exists := fakeProvider.GetInstance("fake-us-east")
		return exists
	}, 10*time.Second, 100*time.Millisecond, "Expected instance in fake provider")

	// Check that instance was created in fake provider
	instance, exists := fakeProvider.GetInstance("fake-us-east")
	if !exists {
		t.Error("Instance was not created in fake provider")
	}
	if instance.Status != providers.InstanceStatusRunning {
		t.Errorf("Expected instance status to be running, got %v", instance.Status)
	}

	// Verify LaunchedAt is set
	status, _ = registry.GetInstanceStatus("fake", "fake-us-east")
	if status.LaunchedAt.IsZero() {
		t.Error("Expected LaunchedAt to be set after creation")
	}

	// Add device to control API to simulate device registration (needed for deletion)
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-us-east",
		Created:  time.Now(),
	})

	// Test deletion via registry
	err = registry.DeleteInstance("fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to delete instance: %v", err)
	}

	// Verify device was removed from control API
	devices, err := controlApi.ListDevices(ctx)
	if err != nil {
		t.Fatalf("Failed to list devices: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("Expected 0 devices after deletion, got %d", len(devices))
	}

	// Verify instance is gone from registry
	_, err = registry.GetInstanceStatus("fake", "fake-us-east")
	if err == nil {
		t.Error("Expected error when getting status of deleted instance")
	}

	// Verify cloud instance was deleted
	_, exists = fakeProvider.GetInstance("fake-us-east")
	if exists {
		t.Error("Instance was not deleted in fake provider")
	}
}

func TestIntegration_RegistryWithFakeProvider_MultipleInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()
	fakeProvider := fake.NewWithConfig(fake.DefaultConfig())

	providers := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	registry := NewRegistry(logger, "", controlApi, nil, providers)
	defer registry.Shutdown()

	ctx := context.Background()

	// Create multiple instances
	regions := []string{"fake-us-east", "fake-us-west", "fake-eu-central"}

	// Pre-add devices to simulate registration
	for _, region := range regions {
		controlApi.AddDevice(controlapi.Device{
			Hostname: fmt.Sprintf("fake-%s", region),
			Created:  time.Now().Add(time.Second), // Future timestamp
		})
	}

	for _, region := range regions {
		err := registry.CreateInstance(ctx, "fake", region)
		if err != nil {
			t.Fatalf("Failed to create instance in %s: %v", region, err)
		}
	}

	// Wait for all instances to finish creation (they stay Launching since tsClient is nil)
	require.Eventually(t, func() bool {
		statuses := registry.GetAllInstanceStatuses()
		if len(statuses) != 3 {
			return false
		}
		for _, s := range statuses {
			if s.State != StateLaunching {
				return false
			}
		}
		return true
	}, 10*time.Second, 100*time.Millisecond, "Expected 3 launching instances")

	// Wait for all instances to be created in the provider
	// (StateLaunching is set before provider.CreateInstance() completes)
	require.Eventually(t, func() bool {
		return len(fakeProvider.GetAllInstances()) == 3
	}, 10*time.Second, 100*time.Millisecond, "Expected 3 instances in fake provider")

	// Verify all instances were created
	allInstances := fakeProvider.GetAllInstances()
	if len(allInstances) != 3 {
		t.Errorf("Expected 3 instances in fake provider, got %d", len(allInstances))
	}

	for _, region := range regions {
		if _, exists := allInstances[region]; !exists {
			t.Errorf("Instance not found in fake provider for region %s", region)
		}
	}

	// Get all instance statuses from registry
	statuses := registry.GetAllInstanceStatuses()
	if len(statuses) != 3 {
		t.Errorf("Expected 3 instances in registry, got %d", len(statuses))
	}

	// Verify each instance status
	for _, region := range regions {
		key := fmt.Sprintf("fake-%s", region)
		status, exists := statuses[key]
		if !exists {
			t.Errorf("Instance status not found for %s", key)
			continue
		}

		if status.Provider != "fake" {
			t.Errorf("Expected provider 'fake', got %s", status.Provider)
		}
		// Region is populated by GetInstanceStatus(), not GetAllInstanceStatuses()
		if status.State != StateLaunching {
			t.Errorf("Expected instance %s to have state StateLaunching, got %d", key, status.State)
		}
	}

	// Test deleting one instance
	err := registry.DeleteInstance("fake", "fake-us-west")
	if err != nil {
		t.Fatalf("Failed to delete instance: %v", err)
	}

	// Verify instance was removed from registry
	statuses = registry.GetAllInstanceStatuses()
	if len(statuses) != 2 {
		t.Errorf("Expected 2 instances after deletion, got %d", len(statuses))
	}

	// Verify the correct instance was removed
	if _, exists := statuses["fake-fake-us-west"]; exists {
		t.Error("Instance fake-fake-us-west should have been deleted")
	}
}

func TestIntegration_RegistryWithFakeProvider_CreateFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)

	// Configure fake provider to fail creation
	config := fake.DefaultConfig()
	config.CreateFailure = errors.New("simulated creation failure")
	fakeProvider := fake.NewWithConfig(config)
	controlApi := NewIntegrationTestControlApi()

	providerMap := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	registry := NewRegistry(logger, "", controlApi, nil, providerMap)
	defer registry.Shutdown()

	// Create instance - registry returns nil immediately, failure happens async
	err := registry.CreateInstance(ctx, "fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Registry CreateInstance should not fail immediately: %v", err)
	}

	// Wait for the async creation to fail and transition to StateFailed
	require.Eventually(t, func() bool {
		status, err := registry.GetInstanceStatus("fake", "fake-us-east")
		return err == nil && status.State == StateFailed
	}, 5*time.Second, 100*time.Millisecond, "Expected instance to be in StateFailed")

	// Verify the failed status has the error message
	status, err := registry.GetInstanceStatus("fake", "fake-us-east")
	require.NoError(t, err)
	require.Contains(t, status.LastError, "simulated creation failure")

	// Verify no instance was created in the fake provider
	_, exists := fakeProvider.GetInstance("fake-us-east")
	if exists {
		t.Error("Instance should not exist after failed creation")
	}
}

func TestIntegration_RegistryWithFakeProvider_ProviderFailures(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()

	// Start with working provider
	fakeProvider := fake.NewWithConfig(fake.DefaultConfig())
	providers := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	registry := NewRegistry(logger, "", controlApi, nil, providers)
	defer registry.Shutdown()

	ctx := context.Background()

	// Pre-add device to simulate registration for the first instance
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-us-east",
		Created:  time.Now().Add(time.Second),
	})

	// Successfully create an instance first
	err := registry.CreateInstance(ctx, "fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Wait for instance to be created (stays Launching since tsClient is nil)
	require.Eventually(t, func() bool {
		status, err := registry.GetInstanceStatus("fake", "fake-us-east")
		return err == nil && status.State == StateLaunching
	}, 10*time.Second, 100*time.Millisecond, "Expected instance to be launching")

	// Wait for the cloud instance to actually be created in the provider
	// (StateLaunching is set before provider.CreateInstance() completes)
	require.Eventually(t, func() bool {
		_, exists := fakeProvider.GetInstance("fake-us-east")
		return exists
	}, 10*time.Second, 100*time.Millisecond, "Expected instance in fake provider")

	// Now configure provider to fail instance creation
	config := fake.DefaultConfig()
	config.CreateFailure = errors.New("simulated creation failure")
	fakeProvider.UpdateConfig(config)

	// First instance should still be tracked
	_, err = registry.GetInstanceStatus("fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Registry should still return status for existing instance: %v", err)
	}

	// Try to create another instance with failing provider
	err = registry.CreateInstance(ctx, "fake", "fake-us-west")
	if err != nil {
		t.Fatalf("Registry creation shouldn't fail immediately: %v", err)
	}

	// Wait for failed creation to transition to StateFailed
	require.Eventually(t, func() bool {
		status, err := registry.GetInstanceStatus("fake", "fake-us-west")
		return err == nil && status.State == StateFailed
	}, 5*time.Second, 100*time.Millisecond, "Expected failed instance to be in StateFailed")

	// Verify the failed instance has an error message
	failedStatus, err := registry.GetInstanceStatus("fake", "fake-us-west")
	require.NoError(t, err)
	require.Contains(t, failedStatus.LastError, "simulated creation failure")

	// Verify the first instance is still tracked
	_, err = registry.GetInstanceStatus("fake", "fake-us-east")
	require.NoError(t, err)
}

func TestIntegration_RegistryWithFakeProvider_DiscoverExistingInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)
	tsClient := tsclient.NewMockClient()
	controlApi := NewIntegrationTestControlApi()

	// Pre-populate control API with existing devices (with service tags)
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-us-east",
		Created:  time.Now().Add(-time.Hour),
		Tags:     services.ExitNode.Tags,
	})
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-eu-central",
		Created:  time.Now().Add(-30 * time.Minute),
		Tags:     services.ExitNode.Tags,
	})

	// Set up identity responses for discovery
	tsClient.SetNodeIdentity("fake-fake-us-east", &tsclient.NodeIdentity{
		Service: "exit", Provider: "fake", Region: "fake-us-east",
	})
	tsClient.SetNodeIdentity("fake-fake-eu-central", &tsclient.NodeIdentity{
		Service: "exit", Provider: "fake", Region: "fake-eu-central",
	})
	tsClient.AddPeer("fake-fake-us-east", netip.MustParseAddr("100.64.0.1"))
	tsClient.AddPeer("fake-fake-eu-central", netip.MustParseAddr("100.64.0.2"))

	fakeProvider := fake.NewWithConfig(fake.DefaultConfig())
	providers := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	registry := NewRegistry(logger, "", controlApi, tsClient, providers)
	defer registry.Shutdown()
	registry.Start(context.Background())

	// Wait for discovery to complete
	require.Eventually(t, func() bool {
		return len(registry.GetAllInstanceStatuses()) == 2
	}, 10*time.Second, 100*time.Millisecond, "Expected 2 discovered instances")

	// Verify discovered instances are tracked
	statuses := registry.GetAllInstanceStatuses()

	// Check specific instances
	for key, status := range statuses {
		if status.State != StateRunning {
			t.Errorf("Discovered instance %s should have state StateRunning, got %d", key, status.State)
		}
		if status.CreatedAt.IsZero() {
			t.Errorf("Discovered instance %s should have creation time", key)
		}
		if !status.LaunchedAt.IsZero() {
			t.Errorf("Discovered instance %s should have zero launch time", key)
		}
		if status.Provider != "fake" {
			t.Errorf("Expected provider 'fake', got %s", status.Provider)
		}
	}

	// Test that creating an existing instance doesn't duplicate it
	ctx := context.Background()
	err := registry.CreateInstance(ctx, "fake", "fake-us-east")
	if err != nil {
		t.Errorf("Creating existing instance should not fail: %v", err)
	}

	// Should still have 2 instances (CreateInstance returns immediately for running instances)
	statuses = registry.GetAllInstanceStatuses()
	if len(statuses) != 2 {
		t.Errorf("Expected 2 instances after creating existing one, got %d", len(statuses))
	}
}

func TestIntegration_RegistryWithFakeProvider_SlowOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)

	// Configure fake provider with realistic delays
	config := fake.DefaultConfig()
	config.CreateDelay = 500 * time.Millisecond
	config.StatusCheckDelay = 100 * time.Millisecond
	fakeProvider := fake.NewWithConfig(config)
	controlApi := NewIntegrationTestControlApi()

	providerMap := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	registry := NewRegistry(logger, "", controlApi, nil, providerMap)
	defer registry.Shutdown()

	ctx := context.Background()

	// Create instance - returns immediately, creation happens async
	start := time.Now()
	err := registry.CreateInstance(ctx, "fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// CreateInstance should return quickly (async creation)
	createCallDuration := time.Since(start)
	if createCallDuration > 200*time.Millisecond {
		t.Errorf("CreateInstance took %v, expected it to return quickly (async)", createCallDuration)
	}

	// Instance should immediately be in StateLaunching
	status, err := registry.GetInstanceStatus("fake", "fake-us-east")
	require.NoError(t, err)
	require.Equal(t, StateLaunching, status.State)

	// Wait for the slow creation to complete in the fake provider
	require.Eventually(t, func() bool {
		_, exists := fakeProvider.GetInstance("fake-us-east")
		return exists
	}, 10*time.Second, 100*time.Millisecond, "Expected instance in fake provider after delay")

	// Total time should have been at least the configured delay
	totalDuration := time.Since(start)
	if totalDuration < config.CreateDelay {
		t.Errorf("Total creation took %v, expected at least %v", totalDuration, config.CreateDelay)
	}
}

func TestIntegration_RegistryDelete_CloudDeletionFailure_StillSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)
	tsClient := tsclient.NewMockClient()
	controlApi := NewIntegrationTestControlApi()

	// Create a mock provider that fails on delete
	mockProvider := &MockProviderWithDeleteFailure{
		hostname:  "mock-test-region",
		instances: make(map[string]bool),
	}
	mockProvider.instances["test-region"] = true

	providerMap := map[string]providers.Provider{
		"mock": mockProvider,
	}

	// Pre-add device to control API (simulates discovered instance, with tags)
	controlApi.AddDevice(controlapi.Device{
		Hostname: "mock-test-region",
		Created:  time.Now().Add(-time.Minute),
		Tags:     services.ExitNode.Tags,
	})

	// Set up identity for discovery
	tsClient.SetNodeIdentity("mock-test-region", &tsclient.NodeIdentity{
		Service: "exit", Provider: "mock", Region: "test-region",
	})
	tsClient.AddPeer("mock-test-region", netip.MustParseAddr("100.64.0.1"))

	registry := NewRegistry(logger, "", controlApi, tsClient, providerMap)
	defer registry.Shutdown()

	// Discover the existing instance so it's tracked in the registry
	registry.discoverInstances(ctx)
	require.Equal(t, 1, len(registry.GetAllInstanceStatuses()))

	// Delete instance via registry - should succeed even if cloud delete fails
	// (Tailscale device is deleted, GC will clean up orphaned cloud instance)
	err := registry.DeleteInstance("mock", "test-region")
	if err != nil {
		t.Fatalf("Delete should succeed even if cloud deletion fails: %v", err)
	}

	// Verify device was deleted from control API
	devices, _ := controlApi.ListDevices(ctx)
	if len(devices) != 0 {
		t.Errorf("Expected 0 devices after deletion, got %d", len(devices))
	}

	// Verify delete was attempted on cloud provider
	if !mockProvider.deleteAttempted {
		t.Error("Cloud deletion should have been attempted")
	}

	// Verify instance is gone from registry
	_, err = registry.GetInstanceStatus("mock", "test-region")
	if err == nil {
		t.Error("Expected error when getting status of deleted instance")
	}
}

func TestIntegration_RegistryDelete_DeviceNotInTailscale(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)

	// Use a fast provider config
	config := fake.DefaultConfig()
	config.CreateDelay = 0
	config.StatusCheckDelay = 0
	fakeProvider := fake.NewWithConfig(config)

	controlApi := NewIntegrationTestControlApi()

	providerMap := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	registry := NewRegistry(logger, "", controlApi, nil, providerMap)
	defer registry.Shutdown()

	// Create instance via registry
	err := registry.CreateInstance(ctx, "fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Wait for instance to be created in the fake provider
	require.Eventually(t, func() bool {
		_, exists := fakeProvider.GetInstance("fake-us-east")
		return exists
	}, 10*time.Second, 100*time.Millisecond, "Expected instance in fake provider")

	// Verify instance exists in fake provider
	_, exists := fakeProvider.GetInstance("fake-us-east")
	if !exists {
		t.Error("Instance should exist in fake provider after creation")
	}

	// Device was never added to control API (or was removed externally).
	// Verify no device in control API.
	devices, _ := controlApi.ListDevices(ctx)
	if len(devices) != 0 {
		t.Fatalf("Expected 0 devices in control API, got %d", len(devices))
	}

	// Delete instance via registry - should succeed even though device is not in Tailscale
	err = registry.DeleteInstance("fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Delete should succeed even if device not in Tailscale: %v", err)
	}

	// Verify cloud instance was deleted
	_, exists = fakeProvider.GetInstance("fake-us-east")
	if exists {
		t.Error("Cloud instance should be deleted even if Tailscale device was already gone")
	}
}

// MockProviderWithDeleteFailure is a test helper that fails on DeleteInstance
type MockProviderWithDeleteFailure struct {
	hostname        providers.HostName
	instances       map[string]bool
	deleteAttempted bool
}

func (m *MockProviderWithDeleteFailure) CreateInstance(ctx context.Context, req providers.CreateRequest) (providers.Instance, error) {
	m.instances[req.Region] = true
	return providers.Instance{
		Hostname:     string(m.hostname),
		ProviderID:   "mock-123",
		ProviderName: "mock",
	}, nil
}

func (m *MockProviderWithDeleteFailure) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	if m.instances[region] {
		return providers.InstanceStatusRunning, nil
	}
	return providers.InstanceStatusMissing, nil
}

func (m *MockProviderWithDeleteFailure) ListRegions(ctx context.Context) ([]providers.Region, error) {
	return []providers.Region{{Code: "test-region", LongName: "Test Region"}}, nil
}

func (m *MockProviderWithDeleteFailure) Hostname(region string) providers.HostName {
	return m.hostname
}

func (m *MockProviderWithDeleteFailure) GetRegionHourlyEstimate(region string) float64 {
	return 0.05
}

func (m *MockProviderWithDeleteFailure) DeleteInstance(ctx context.Context, instanceID providers.Instance) error {
	m.deleteAttempted = true
	return errors.New("simulated cloud deletion failure")
}

func (m *MockProviderWithDeleteFailure) ListInstances(ctx context.Context, region string) ([]providers.Instance, error) {
	if m.instances[region] {
		return []providers.Instance{{
			Hostname:     string(m.hostname),
			ProviderID:   "mock-123",
			ProviderName: "mock",
		}}, nil
	}
	return []providers.Instance{}, nil
}
