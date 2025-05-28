package instances

import (
	"context"
	"log"
	"slices"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

// MockProvider implements a simple mock provider for testing
type MockProvider struct {
	hostname providers.HostName
	status   providers.InstanceStatus
}

func (m *MockProvider) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	return providers.InstanceID{
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

func (m *MockProvider) GetRegionPrice(region string) float64 {
	return 0.05
}

func (m *MockProvider) DeleteInstance(ctx context.Context, instanceID providers.InstanceID) error {
	return nil
}

func (m *MockProvider) ListInstances(ctx context.Context, region string) ([]providers.InstanceID, error) {
	if m.status == providers.InstanceStatusRunning {
		return []providers.InstanceID{{
			Hostname:     string(m.hostname),
			ProviderID:   "mock-123",
			ProviderName: "mock",
		}}, nil
	}
	return []providers.InstanceID{}, nil
}

// MockControlApi implements a simple mock control API for testing
type MockControlApi struct {
	devices []controlapi.Device
}

func (m *MockControlApi) CreateKey(ctx context.Context) (*controlapi.PreauthKey, error) {
	return &controlapi.PreauthKey{Key: "mock-key"}, nil
}

func (m *MockControlApi) ListDevices(ctx context.Context) ([]controlapi.Device, error) {
	return m.devices, nil
}

func (m *MockControlApi) DeleteDevice(ctx context.Context, deviceID string) error {
	// Remove device from mock list
	for i, device := range m.devices {
		if device.ID == deviceID {
			m.devices = slices.Delete(m.devices, i, i+1)
			return nil
		}
	}
	return nil
}

func (m *MockControlApi) ApproveExitNode(ctx context.Context, deviceID string) error {
	return nil
}

// AddDevice adds a device to the mock (for testing)
func (m *MockControlApi) AddDevice(device controlapi.Device) {
	m.devices = append(m.devices, device)
}

func TestController_NewController(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()
	provider := &MockProvider{
		hostname: "test-instance",
		status:   providers.InstanceStatusMissing,
	}
	controlApi := &MockControlApi{}

	controller := NewController(ctx, logger, provider, "test-region", controlApi, nil)
	defer controller.Stop()

	if controller == nil {
		t.Fatal("NewController returned nil")
	}

	status := controller.Status()
	if status.Hostname != "test-instance" {
		t.Errorf("Expected hostname 'test-instance', got %s", status.Hostname)
	}
	if status.Region != "test-region" {
		t.Errorf("Expected region 'test-region', got %s", status.Region)
	}
	if status.IsRunning {
		t.Error("Expected instance to not be running initially")
	}
}

func TestController_Status(t *testing.T) {
	ctx := context.Background()
	logger := log.Default()
	provider := &MockProvider{
		hostname: "test-instance",
		status:   providers.InstanceStatusRunning,
	}
	controlApi := &MockControlApi{}

	controller := NewController(ctx, logger, provider, "test-region", controlApi, nil)
	defer controller.Stop()

	// Test initial status
	status := controller.Status()
	if status.IsRunning {
		t.Error("Expected instance to not be running initially")
	}

	// Verify the ping stats are initialized
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

	registry := NewRegistry(logger, controlApi, nil, providers)
	defer registry.Shutdown()

	ctx := context.Background()

	// Test creating an instance
	err := registry.CreateInstance(ctx, "mock", "test-region")
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Wait a bit for the creation to be processed
	time.Sleep(10 * time.Millisecond)

	// Test getting instance status
	status, err := registry.GetInstanceStatus("mock", "test-region")
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
		ID:       "mock-device-123",
		Hostname: "mock-test-region",
	})

	// Test deleting the instance
	err = registry.DeleteInstance("mock", "test-region")
	if err != nil {
		t.Fatalf("Failed to delete instance: %v", err)
	}

	// Verify instance is gone
	_, err = registry.GetInstanceStatus("mock", "test-region")
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

	registry := NewRegistry(logger, controlApi, nil, providers)
	defer registry.Shutdown()

	ctx := context.Background()

	// Create instances
	err := registry.CreateInstance(ctx, "mock1", "test-region")
	if err != nil {
		t.Fatalf("Failed to create instance 1: %v", err)
	}

	err = registry.CreateInstance(ctx, "mock2", "test-region")
	if err != nil {
		t.Fatalf("Failed to create instance 2: %v", err)
	}

	// Wait a bit for the creation to be processed
	time.Sleep(10 * time.Millisecond)

	// Get all statuses
	statuses := registry.GetAllInstanceStatuses()

	if len(statuses) != 2 {
		t.Errorf("Expected 2 instances, got %d", len(statuses))
	}

	// Verify both instances are present
	hasMock1 := false
	hasMock2 := false
	for key, status := range statuses {
		if key == "mock1-test-region" && status.Provider == "mock1" {
			hasMock1 = true
		}
		if key == "mock2-test-region" && status.Provider == "mock2" {
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
	controlApi := &MockControlApi{
		// Pre-populate with existing devices
		devices: []controlapi.Device{
			{
				ID:       "existing-device-1",
				Hostname: "mock1-test-region",
				Created:  time.Now().Add(-time.Hour), // Created 1 hour ago
			},
			{
				ID:       "existing-device-2",
				Hostname: "mock2-other-region",
				Created:  time.Now().Add(-30 * time.Minute), // Created 30 minutes ago
			},
		},
	}

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

	registry := NewRegistry(logger, controlApi, nil, providers)
	defer registry.Shutdown()

	// Wait for discovery to complete
	time.Sleep(3 * time.Second)

	// Check that discovered instances are tracked
	allStatuses := registry.GetAllInstanceStatuses()

	if len(allStatuses) != 2 {
		t.Errorf("Expected 2 discovered instances, got %d", len(allStatuses))
	}

	// Verify the discovered instances have correct status
	for key, status := range allStatuses {
		if !status.IsRunning {
			t.Errorf("Discovered instance %s should be marked as running", key)
		}
		if status.CreatedAt.IsZero() {
			t.Errorf("Discovered instance %s should have creation time set", key)
		}
		if status.LaunchedAt.IsZero() {
			// LaunchedAt should be zero for discovered instances since we don't know when they were launched
			// This is expected behavior
		}
	}

	// Test that creating an instance that already exists doesn't duplicate it
	ctx := context.Background()
	err := registry.CreateInstance(ctx, "mock1", "test-region")
	if err != nil {
		t.Errorf("Creating existing instance should not fail: %v", err)
	}

	// Should still have only 2 instances
	allStatuses = registry.GetAllInstanceStatuses()
	if len(allStatuses) != 2 {
		t.Errorf("Expected 2 instances after trying to create existing one, got %d", len(allStatuses))
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

	registry := NewRegistry(logger, controlApi, nil, providers)
	defer registry.Shutdown()

	// Create a context that we'll cancel immediately
	ctx, cancel := context.WithCancel(context.Background())

	// Start instance creation
	err := registry.CreateInstance(ctx, "mock", "test-region")
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Cancel the context immediately after starting creation
	cancel()

	// Wait a bit for creation to process
	time.Sleep(20 * time.Millisecond)

	// Verify that instance creation wasn't affected by context cancellation
	status, err := registry.GetInstanceStatus("mock", "test-region")
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
