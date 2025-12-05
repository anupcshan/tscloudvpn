package instances

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/providers/fake"
)

func TestGarbageCollector_NoOrphanedInstances(t *testing.T) {
	ctx := context.Background()
	logger := log.New(os.Stderr, "[GC-TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()
	fakeProvider := fake.NewWithConfig(fake.DefaultConfig())

	// Create an instance in the fake provider
	key := &controlapi.PreauthKey{Key: "test-key"}
	_, err := fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Add the corresponding device to the control API (not orphaned)
	controlApi.AddDevice(controlapi.Device{
		Hostname: "fake-fake-us-east",
		Created:  time.Now().Add(-time.Hour),
	})

	providers := map[string]providers.Provider{
		"fake": fakeProvider,
	}

	gc := NewGarbageCollector(logger, controlApi, providers)

	// Run garbage collection
	gc.collect(ctx)

	// Verify instance was NOT deleted (it's not orphaned)
	_, exists := fakeProvider.GetInstance("fake-us-east")
	if !exists {
		t.Error("Instance should still exist - it's not orphaned")
	}
}

func TestGarbageCollector_OrphanedInstance_OlderThanGracePeriod(t *testing.T) {
	ctx := context.Background()
	logger := log.New(os.Stderr, "[GC-TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()

	// Create a fake provider with no delays
	config := fake.DefaultConfig()
	config.CreateDelay = 0
	config.StatusCheckDelay = 0
	fakeProvider := fake.NewWithConfig(config)

	// Create an instance in the fake provider
	key := &controlapi.PreauthKey{Key: "test-key"}
	_, err := fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Manually set the instance creation time to be older than grace period
	// We need to access the internal state to backdate the instance
	oldCreatedAt := time.Now().Add(-15 * time.Minute) // 15 minutes ago, older than 10-minute grace period

	// Delete the instance we just created - we'll use a mock provider instead
	// that allows us to control the CreatedAt timestamp
	fakeProvider.DeleteInstanceByRegion("fake-us-east")

	// Use a mock provider that allows setting CreatedAt timestamp
	mockProvider := &MockProviderWithTimestamp{
		hostname:  "fake-fake-us-east",
		instances: make(map[string]providers.InstanceID),
	}
	mockProvider.instances["fake-us-east"] = providers.InstanceID{
		Hostname:     "fake-fake-us-east",
		ProviderID:   "mock-123",
		ProviderName: "mock",
		CreatedAt:    oldCreatedAt,
	}

	// Do NOT add to control API - this makes it orphaned
	// controlApi.AddDevice(...)

	providersMap := map[string]providers.Provider{
		"mock": mockProvider,
	}

	gc := NewGarbageCollector(logger, controlApi, providersMap)

	// Run garbage collection
	gc.collect(ctx)

	// Verify instance was deleted (it's orphaned and old)
	if !mockProvider.deleted {
		t.Error("Orphaned instance older than grace period should have been deleted")
	}
}

func TestGarbageCollector_OrphanedInstance_WithinGracePeriod(t *testing.T) {
	ctx := context.Background()
	logger := log.New(os.Stderr, "[GC-TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()

	// Create a mock provider with a recently created instance
	recentCreatedAt := time.Now().Add(-5 * time.Minute) // 5 minutes ago, within 10-minute grace period

	mockProvider := &MockProviderWithTimestamp{
		hostname:  "fake-fake-us-east",
		instances: make(map[string]providers.InstanceID),
	}
	mockProvider.instances["fake-us-east"] = providers.InstanceID{
		Hostname:     "fake-fake-us-east",
		ProviderID:   "mock-123",
		ProviderName: "mock",
		CreatedAt:    recentCreatedAt,
	}

	// Do NOT add to control API - this makes it orphaned
	// controlApi.AddDevice(...)

	providersMap := map[string]providers.Provider{
		"mock": mockProvider,
	}

	gc := NewGarbageCollector(logger, controlApi, providersMap)

	// Run garbage collection
	gc.collect(ctx)

	// Verify instance was NOT deleted (it's within grace period)
	if mockProvider.deleted {
		t.Error("Orphaned instance within grace period should NOT have been deleted")
	}
}

func TestGarbageCollector_MultipleProvidersMultipleRegions(t *testing.T) {
	ctx := context.Background()
	logger := log.New(os.Stderr, "[GC-TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()

	oldTime := time.Now().Add(-15 * time.Minute)

	// Provider 1: has orphaned instance
	mockProvider1 := &MockProviderWithTimestamp{
		hostname:  "mock1-us-east",
		instances: make(map[string]providers.InstanceID),
	}
	mockProvider1.instances["us-east"] = providers.InstanceID{
		Hostname:     "mock1-us-east",
		ProviderID:   "mock1-123",
		ProviderName: "mock1",
		CreatedAt:    oldTime,
	}

	// Provider 2: has non-orphaned instance
	mockProvider2 := &MockProviderWithTimestamp{
		hostname:  "mock2-eu-west",
		instances: make(map[string]providers.InstanceID),
	}
	mockProvider2.instances["eu-west"] = providers.InstanceID{
		Hostname:     "mock2-eu-west",
		ProviderID:   "mock2-456",
		ProviderName: "mock2",
		CreatedAt:    oldTime,
	}

	// Add device for provider 2's instance (making it NOT orphaned)
	controlApi.AddDevice(controlapi.Device{
		Hostname: "mock2-eu-west",
		Created:  oldTime,
	})

	providersMap := map[string]providers.Provider{
		"mock1": mockProvider1,
		"mock2": mockProvider2,
	}

	gc := NewGarbageCollector(logger, controlApi, providersMap)

	// Run garbage collection
	gc.collect(ctx)

	// Verify: provider1's orphaned instance should be deleted
	if !mockProvider1.deleted {
		t.Error("Provider1's orphaned instance should have been deleted")
	}

	// Verify: provider2's non-orphaned instance should NOT be deleted
	if mockProvider2.deleted {
		t.Error("Provider2's instance should NOT have been deleted (it's in Tailscale)")
	}
}

func TestGarbageCollector_DeleteFailure(t *testing.T) {
	ctx := context.Background()
	logger := log.New(os.Stderr, "[GC-TEST] ", log.LstdFlags)
	controlApi := NewIntegrationTestControlApi()

	oldTime := time.Now().Add(-15 * time.Minute)

	// Mock provider that fails on delete
	mockProvider := &MockProviderWithTimestamp{
		hostname:    "fake-fake-us-east",
		instances:   make(map[string]providers.InstanceID),
		deleteError: true,
	}
	mockProvider.instances["fake-us-east"] = providers.InstanceID{
		Hostname:     "fake-fake-us-east",
		ProviderID:   "mock-123",
		ProviderName: "mock",
		CreatedAt:    oldTime,
	}

	providersMap := map[string]providers.Provider{
		"mock": mockProvider,
	}

	gc := NewGarbageCollector(logger, controlApi, providersMap)

	// Run garbage collection - should not panic on delete failure
	gc.collect(ctx)

	// Verify delete was attempted
	if !mockProvider.deleteAttempted {
		t.Error("Delete should have been attempted")
	}
}

// MockProviderWithTimestamp is a test helper that allows setting CreatedAt times
type MockProviderWithTimestamp struct {
	hostname        providers.HostName
	instances       map[string]providers.InstanceID
	deleted         bool
	deleteAttempted bool
	deleteError     bool
}

func (m *MockProviderWithTimestamp) CreateInstance(ctx context.Context, region string, key *controlapi.PreauthKey) (providers.InstanceID, error) {
	return providers.InstanceID{
		Hostname:     string(m.hostname),
		ProviderID:   "mock-123",
		ProviderName: "mock",
		CreatedAt:    time.Now(),
	}, nil
}

func (m *MockProviderWithTimestamp) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	if _, exists := m.instances[region]; exists {
		return providers.InstanceStatusRunning, nil
	}
	return providers.InstanceStatusMissing, nil
}

func (m *MockProviderWithTimestamp) ListRegions(ctx context.Context) ([]providers.Region, error) {
	regions := make([]providers.Region, 0, len(m.instances))
	for region := range m.instances {
		regions = append(regions, providers.Region{Code: region, LongName: "Mock Region"})
	}
	return regions, nil
}

func (m *MockProviderWithTimestamp) Hostname(region string) providers.HostName {
	return m.hostname
}

func (m *MockProviderWithTimestamp) GetRegionPrice(region string) float64 {
	return 0.05
}

func (m *MockProviderWithTimestamp) DeleteInstance(ctx context.Context, instanceID providers.InstanceID) error {
	m.deleteAttempted = true
	if m.deleteError {
		return context.DeadlineExceeded
	}
	m.deleted = true
	delete(m.instances, instanceID.ProviderID)
	return nil
}

func (m *MockProviderWithTimestamp) ListInstances(ctx context.Context, region string) ([]providers.InstanceID, error) {
	var result []providers.InstanceID
	if instance, exists := m.instances[region]; exists {
		result = append(result, instance)
	}
	return result, nil
}
