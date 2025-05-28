package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

func TestFakeProvider_CreateInstance(t *testing.T) {
	ctx := context.Background()
	fakeProvider := NewWithConfig(DefaultConfig())

	// Test creating an instance
	key := &controlapi.PreauthKey{Key: "test-key"}
	instanceID, err := fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	if instanceID.Hostname != "fake-fake-us-east" {
		t.Errorf("Expected hostname 'fake-fake-us-east', got %s", instanceID.Hostname)
	}
	if instanceID.ProviderName != "fake" {
		t.Errorf("Expected provider name 'fake', got %s", instanceID.ProviderName)
	}
	if instanceID.ProviderID == "" {
		t.Error("Expected non-empty provider ID")
	}

	// Verify instance exists in provider
	instance, exists := fakeProvider.GetInstance("fake-us-east")
	if !exists {
		t.Error("Instance should exist after creation")
	}
	if instance.Status != providers.InstanceStatusRunning {
		t.Errorf("Expected status running, got %v", instance.Status)
	}
	if instance.Region != "fake-us-east" {
		t.Errorf("Expected region 'fake-us-east', got %s", instance.Region)
	}
}

func TestFakeProvider_GetInstanceStatus(t *testing.T) {
	ctx := context.Background()
	fakeProvider := NewWithConfig(DefaultConfig())

	// Test status for non-existent instance
	status, err := fakeProvider.GetInstanceStatus(ctx, "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to get instance status: %v", err)
	}
	if status != providers.InstanceStatusMissing {
		t.Errorf("Expected status missing, got %v", status)
	}

	// Create an instance
	key := &controlapi.PreauthKey{Key: "test-key"}
	_, err = fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Test status for existing instance
	status, err = fakeProvider.GetInstanceStatus(ctx, "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to get instance status: %v", err)
	}
	if status != providers.InstanceStatusRunning {
		t.Errorf("Expected status running, got %v", status)
	}
}

func TestFakeProvider_ListRegions(t *testing.T) {
	ctx := context.Background()
	fakeProvider := NewWithConfig(DefaultConfig())

	regions, err := fakeProvider.ListRegions(ctx)
	if err != nil {
		t.Fatalf("Failed to list regions: %v", err)
	}

	expectedRegions := map[string]string{
		"fake-us-east":    "Fake US East",
		"fake-us-west":    "Fake US West",
		"fake-eu-central": "Fake EU Central",
		"fake-ap-south":   "Fake Asia Pacific South",
	}

	if len(regions) != len(expectedRegions) {
		t.Errorf("Expected %d regions, got %d", len(expectedRegions), len(regions))
	}

	for _, region := range regions {
		expectedName, exists := expectedRegions[region.Code]
		if !exists {
			t.Errorf("Unexpected region code: %s", region.Code)
		}
		if region.LongName != expectedName {
			t.Errorf("Expected region name %s, got %s", expectedName, region.LongName)
		}
	}
}

func TestFakeProvider_Hostname(t *testing.T) {
	fakeProvider := NewWithConfig(DefaultConfig())

	hostname := fakeProvider.Hostname("us-east")
	expected := "fake-us-east"
	if string(hostname) != expected {
		t.Errorf("Expected hostname %s, got %s", expected, hostname)
	}
}

func TestFakeProvider_GetRegionPrice(t *testing.T) {
	fakeProvider := NewWithConfig(DefaultConfig())

	price := fakeProvider.GetRegionPrice("fake-us-east")
	expected := 0.001
	if price != expected {
		t.Errorf("Expected price %f, got %f", expected, price)
	}

	// Test with custom price
	config := DefaultConfig()
	config.PricePerHour = 0.05
	fakeProvider.UpdateConfig(config)

	price = fakeProvider.GetRegionPrice("fake-us-east")
	expected = 0.05
	if price != expected {
		t.Errorf("Expected price %f, got %f", expected, price)
	}
}

func TestFakeProvider_ConfigurableFailures(t *testing.T) {
	ctx := context.Background()

	// Test create failure
	config := DefaultConfig()
	config.CreateFailure = errors.New("test create failure")
	fakeProvider := NewWithConfig(config)

	key := &controlapi.PreauthKey{Key: "test-key"}
	_, err := fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err == nil {
		t.Error("Expected create to fail")
	}
	if err.Error() != "test create failure" {
		t.Errorf("Expected specific error message, got: %v", err)
	}

	// Test status failure
	config = DefaultConfig()
	config.StatusFailure = errors.New("test status failure")
	fakeProvider = NewWithConfig(config)

	_, err = fakeProvider.GetInstanceStatus(ctx, "fake-us-east")
	if err == nil {
		t.Error("Expected status check to fail")
	}
	if err.Error() != "test status failure" {
		t.Errorf("Expected specific error message, got: %v", err)
	}

	// Test region list failure
	config = DefaultConfig()
	config.RegionListFailure = errors.New("test region list failure")
	fakeProvider = NewWithConfig(config)

	_, err = fakeProvider.ListRegions(ctx)
	if err == nil {
		t.Error("Expected region list to fail")
	}
	if err.Error() != "test region list failure" {
		t.Errorf("Expected specific error message, got: %v", err)
	}
}

func TestFakeProvider_ConfigurableDelays(t *testing.T) {
	ctx := context.Background()

	// Test create delay
	config := DefaultConfig()
	config.CreateDelay = 100 * time.Millisecond
	fakeProvider := NewWithConfig(config)

	key := &controlapi.PreauthKey{Key: "test-key"}
	start := time.Now()
	_, err := fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}
	duration := time.Since(start)

	if duration < config.CreateDelay {
		t.Errorf("Create took %v, expected at least %v", duration, config.CreateDelay)
	}

	// Test status delay
	config = DefaultConfig()
	config.StatusCheckDelay = 50 * time.Millisecond
	fakeProvider = NewWithConfig(config)

	start = time.Now()
	_, err = fakeProvider.GetInstanceStatus(ctx, "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	duration = time.Since(start)

	if duration < config.StatusCheckDelay {
		t.Errorf("Status check took %v, expected at least %v", duration, config.StatusCheckDelay)
	}

	// Test region list delay
	config = DefaultConfig()
	config.RegionListDelay = 50 * time.Millisecond
	fakeProvider = NewWithConfig(config)

	start = time.Now()
	_, err = fakeProvider.ListRegions(ctx)
	if err != nil {
		t.Fatalf("Failed to list regions: %v", err)
	}
	duration = time.Since(start)

	if duration < config.RegionListDelay {
		t.Errorf("Region list took %v, expected at least %v", duration, config.RegionListDelay)
	}
}

func TestFakeProvider_ContextCancellation(t *testing.T) {
	// Test create cancellation
	config := DefaultConfig()
	config.CreateDelay = 500 * time.Millisecond
	fakeProvider := NewWithConfig(config)

	ctx, cancel := context.WithCancel(context.Background())
	key := &controlapi.PreauthKey{Key: "test-key"}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err == nil {
		t.Error("Expected create to be canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Expected context.Canceled, got: %v", err)
	}

	// Verify no instance was created
	_, exists := fakeProvider.GetInstance("fake-us-east")
	if exists {
		t.Error("Instance should not exist after canceled creation")
	}
}

func TestFakeProvider_SetInstanceStatus(t *testing.T) {
	ctx := context.Background()
	fakeProvider := NewWithConfig(DefaultConfig())

	// Create an instance
	key := &controlapi.PreauthKey{Key: "test-key"}
	_, err := fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Change status to missing
	fakeProvider.SetInstanceStatus("fake-us-east", providers.InstanceStatusMissing)

	status, err := fakeProvider.GetInstanceStatus(ctx, "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if status != providers.InstanceStatusMissing {
		t.Errorf("Expected status missing, got %v", status)
	}
}

func TestFakeProvider_DeleteInstance(t *testing.T) {
	ctx := context.Background()
	fakeProvider := NewWithConfig(DefaultConfig())

	// Create an instance
	key := &controlapi.PreauthKey{Key: "test-key"}
	_, err := fakeProvider.CreateInstance(ctx, "fake-us-east", key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	// Verify it exists
	_, exists := fakeProvider.GetInstance("fake-us-east")
	if !exists {
		t.Error("Instance should exist after creation")
	}

	// Delete it
	fakeProvider.DeleteInstance("fake-us-east")

	// Verify it's gone
	_, exists = fakeProvider.GetInstance("fake-us-east")
	if exists {
		t.Error("Instance should not exist after deletion")
	}

	// Verify status shows missing
	status, err := fakeProvider.GetInstanceStatus(ctx, "fake-us-east")
	if err != nil {
		t.Fatalf("Failed to get status: %v", err)
	}
	if status != providers.InstanceStatusMissing {
		t.Errorf("Expected status missing, got %v", status)
	}
}

func TestFakeProvider_GetAllInstances(t *testing.T) {
	ctx := context.Background()
	fakeProvider := NewWithConfig(DefaultConfig())

	// Initially empty
	allInstances := fakeProvider.GetAllInstances()
	if len(allInstances) != 0 {
		t.Errorf("Expected 0 instances initially, got %d", len(allInstances))
	}

	// Create multiple instances
	regions := []string{"fake-us-east", "fake-us-west", "fake-eu-central"}
	key := &controlapi.PreauthKey{Key: "test-key"}

	for _, region := range regions {
		_, err := fakeProvider.CreateInstance(ctx, region, key)
		if err != nil {
			t.Fatalf("Failed to create instance in %s: %v", region, err)
		}
	}

	// Verify all instances exist
	allInstances = fakeProvider.GetAllInstances()
	if len(allInstances) != 3 {
		t.Errorf("Expected 3 instances, got %d", len(allInstances))
	}

	for _, region := range regions {
		instance, exists := allInstances[region]
		if !exists {
			t.Errorf("Instance not found for region %s", region)
			continue
		}
		if instance.Region != region {
			t.Errorf("Expected region %s, got %s", region, instance.Region)
		}
		if instance.Status != providers.InstanceStatusRunning {
			t.Errorf("Expected status running for %s, got %v", region, instance.Status)
		}
	}
}
