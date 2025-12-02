package instances

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/providers/fake"
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
func (api *IntegrationTestControlApi) CreateKey(ctx context.Context) (*controlapi.PreauthKey, error) {
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

// ApproveExitNode approves a device as an exit node
func (api *IntegrationTestControlApi) ApproveExitNode(ctx context.Context, device *controlapi.Device) error {
	return nil // No-op for testing
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

func TestIntegration_ControllerWithFakeProvider_BasicLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)
	fakeProvider := fake.NewWithConfig(fake.DefaultConfig())
	controlApi := NewIntegrationTestControlApi()

	controller := NewController(ctx, logger, fakeProvider, "fake-us-east", controlApi, nil)
	defer controller.Stop()

	// Test initial status
	status := controller.Status()
	if status.Hostname != "fake-fake-us-east" {
		t.Errorf("Expected hostname 'fake-fake-us-east', got %s", status.Hostname)
	}
	if status.Region != "fake-us-east" {
		t.Errorf("Expected region 'fake-us-east', got %s", status.Region)
	}
	if status.IsRunning {
		t.Error("Expected instance to not be running initially")
	}

	// Add device to control API before creation to simulate quick registration
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-us-east",
		Created:  time.Now().Add(time.Second), // Created slightly in the future
	})

	// Test instance creation
	err := controller.Create()
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Check that instance was created in fake provider
	instance, exists := fakeProvider.GetInstance("fake-us-east")
	if !exists {
		t.Error("Instance was not created in fake provider")
	}
	if instance.Status != providers.InstanceStatusRunning {
		t.Errorf("Expected instance status to be running, got %v", instance.Status)
	}

	// Test status after creation
	status = controller.Status()
	if !status.IsRunning {
		t.Error("Expected instance to be running after creation")
	}
	if status.LaunchedAt.IsZero() {
		t.Error("Expected LaunchedAt to be set after creation")
	}

	// Test deletion
	err = controller.Delete()
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

	// Test status after deletion
	status = controller.Status()
	if status.IsRunning {
		t.Error("Expected instance to not be running after deletion")
	}

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

	registry := NewRegistry(logger, controlApi, nil, providers, true)
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

	// Wait for all instances to be running
	require.Eventually(t, func() bool {
		statuses := registry.GetAllInstanceStatuses()
		if len(statuses) != 3 {
			return false
		}
		for _, status := range statuses {
			if !status.IsRunning {
				return false
			}
		}
		return true
	}, 10*time.Second, 100*time.Millisecond, "Expected 3 running instances")

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
		if status.Region != region {
			t.Errorf("Expected region %s, got %s", region, status.Region)
		}
		if !status.IsRunning {
			t.Errorf("Expected instance %s to be running", key)
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

func TestIntegration_ControllerWithFakeProvider_CreateFailure(t *testing.T) {
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

	controller := NewController(ctx, logger, fakeProvider, "fake-us-east", controlApi, nil)
	defer controller.Stop()

	// Test creation failure
	err := controller.Create()
	if err == nil {
		t.Fatal("Expected creation to fail, but it succeeded")
	}
	if err.Error() != "simulated creation failure" {
		t.Errorf("Expected specific error message, got: %v", err)
	}

	// Verify no instance was created
	_, exists := fakeProvider.GetInstance("fake-us-east")
	if exists {
		t.Error("Instance should not exist after failed creation")
	}

	// Verify controller status reflects failure
	status := controller.Status()
	if status.IsRunning {
		t.Error("Expected instance to not be running after failed creation")
	}
	if !status.LaunchedAt.IsZero() {
		t.Error("Expected LaunchedAt to be zero after failed creation")
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

	registry := NewRegistry(logger, controlApi, nil, providers, true)
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

	// Wait for instance to be running
	require.Eventually(t, func() bool {
		status, err := registry.GetInstanceStatus("fake", "fake-us-east")
		return err == nil && status.IsRunning
	}, 10*time.Second, 100*time.Millisecond, "Expected instance to be running")

	// Now configure provider to fail status checks
	config := fake.DefaultConfig()
	config.StatusFailure = errors.New("simulated status failure")
	fakeProvider.UpdateConfig(config)

	// Status checks should now fail, but instance should still be tracked
	_, err = registry.GetInstanceStatus("fake", "fake-us-east")
	if err != nil {
		t.Fatalf("Registry should still return status even if provider fails: %v", err)
	}

	// Try to create another instance with failing provider
	err = registry.CreateInstance(ctx, "fake", "fake-us-west")
	if err != nil {
		t.Fatalf("Registry creation shouldn't fail immediately: %v", err)
	}

	// Wait for failed creation to be cleaned up (controller removed from registry)
	require.Eventually(t, func() bool {
		statuses := registry.GetAllInstanceStatuses()
		return len(statuses) == 1
	}, 5*time.Second, 100*time.Millisecond, "Expected only 1 instance after failed creation")

	// Verify only the first instance remains
	statuses := registry.GetAllInstanceStatuses()
	if len(statuses) != 1 {
		t.Errorf("Expected 1 instance after failed creation, got %d", len(statuses))
		for key := range statuses {
			t.Logf("Found instance: %s", key)
		}
	}
}

func TestIntegration_RegistryWithFakeProvider_DiscoverExistingInstances(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()

	// Pre-populate control API with existing devices
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-us-east",
		Created:  time.Now().Add(-time.Hour),
	})
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-eu-central",
		Created:  time.Now().Add(-30 * time.Minute),
	})

	fakeProvider := fake.NewWithConfig(fake.DefaultConfig())
	providers := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	// Create registry - this should trigger discovery
	registry := NewRegistry(logger, controlApi, nil, providers, true)
	defer registry.Shutdown()

	// Wait for discovery to complete
	require.Eventually(t, func() bool {
		return len(registry.GetAllInstanceStatuses()) == 2
	}, 10*time.Second, 100*time.Millisecond, "Expected 2 discovered instances")

	// Verify discovered instances are tracked
	statuses := registry.GetAllInstanceStatuses()

	// Check specific instances
	for key, status := range statuses {
		if !status.IsRunning {
			t.Errorf("Discovered instance %s should be running", key)
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

func TestIntegration_ControllerWithFakeProvider_SlowOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)

	// Configure fake provider with realistic delays
	config := fake.DefaultConfig()
	config.CreateDelay = 500 * time.Millisecond
	config.StatusCheckDelay = 100 * time.Millisecond
	fakeProvider := fake.NewWithConfig(config)
	controlApi := NewIntegrationTestControlApi()

	// Pre-add device to simulate registration
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-us-east",
		Created:  time.Now().Add(2 * time.Second), // Future timestamp
	})

	controller := NewController(ctx, logger, fakeProvider, "fake-us-east", controlApi, nil)
	defer controller.Stop()

	// Test creation with delay
	start := time.Now()
	err := controller.Create()
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}
	duration := time.Since(start)

	// Should have taken at least the configured delay
	if duration < config.CreateDelay {
		t.Errorf("Creation took %v, expected at least %v", duration, config.CreateDelay)
	}

	// Verify instance was created
	status := controller.Status()
	if !status.IsRunning {
		t.Error("Expected instance to be running after slow creation")
	}
}

func TestIntegration_ControllerDelete_CloudDeletionFailure_StillSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	ctx := context.Background()
	logger := log.New(os.Stderr, "[TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()

	// Create a mock provider that fails on delete
	mockProvider := &MockProviderWithDeleteFailure{
		hostname:  "mock-test-region",
		instances: make(map[string]bool),
	}
	mockProvider.instances["test-region"] = true

	// Pre-add device to control API
	controlApi.AddDevice(controlapi.Device{
		Hostname: "mock-test-region",
		Created:  time.Now(),
	})

	controller := NewController(ctx, logger, mockProvider, "test-region", controlApi, nil)
	defer controller.Stop()

	// Mark controller as running (simulate existing instance)
	controller.mu.Lock()
	controller.isRunning = true
	controller.mu.Unlock()

	// Delete instance - should succeed even if cloud delete fails
	// (Tailscale device is deleted, GC will clean up orphaned cloud instance)
	err := controller.Delete()
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
}

func TestIntegration_ControllerDelete_DeviceNotInTailscale(t *testing.T) {
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

	// Pre-add device so Create() can complete
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-us-east",
		Created:  time.Now().Add(time.Second),
	})

	controller := NewController(ctx, logger, fakeProvider, "fake-us-east", controlApi, nil)
	defer controller.Stop()

	// Create instance
	err := controller.Create()
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Verify instance exists in fake provider
	_, exists := fakeProvider.GetInstance("fake-us-east")
	if !exists {
		t.Error("Instance should exist in fake provider after creation")
	}

	// NOW remove the device from control API to simulate it being deleted externally
	controlApi.RemoveDevice("fake-fake-us-east")

	// Verify device is gone from control API
	devices, _ := controlApi.ListDevices(ctx)
	if len(devices) != 0 {
		t.Fatalf("Expected device to be removed from control API, got %d devices", len(devices))
	}

	// Delete instance - should succeed and delete cloud instance even though device is gone
	err = controller.Delete()
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

func (m *MockProviderWithDeleteFailure) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	m.instances[region] = true
	return providers.InstanceID{
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

func (m *MockProviderWithDeleteFailure) GetRegionPrice(region string) float64 {
	return 0.05
}

func (m *MockProviderWithDeleteFailure) DeleteInstance(ctx context.Context, instanceID providers.InstanceID) error {
	m.deleteAttempted = true
	return errors.New("simulated cloud deletion failure")
}

func (m *MockProviderWithDeleteFailure) ListInstances(ctx context.Context, region string) ([]providers.InstanceID, error) {
	if m.instances[region] {
		return []providers.InstanceID{{
			Hostname:     string(m.hostname),
			ProviderID:   "mock-123",
			ProviderName: "mock",
		}}, nil
	}
	return []providers.InstanceID{}, nil
}
