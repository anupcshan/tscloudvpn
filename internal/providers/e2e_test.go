//go:build e2e
// +build e2e

package providers_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"

	// Import all provider implementations
	_ "github.com/anupcshan/tscloudvpn/internal/providers/digitalocean"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/ec2"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/gcp"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/hetzner"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/linode"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/vultr"
)

// E2ETestConfig holds configuration for end-to-end tests
type E2ETestConfig struct {
	Config           *config.Config
	ControlAPI       controlapi.ControlApi
	EnabledProviders map[string]bool
	TestRegions      map[string]string // provider -> region
	Timeout          time.Duration
	SkipCleanup      bool // For debugging failed tests
}

// MockControlAPI provides a test implementation of ControlApi for E2E tests
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

func (m *MockControlAPI) ApproveExitNode(ctx context.Context, deviceID string) error {
	return nil // No-op for testing
}

func (m *MockControlAPI) DeleteDevice(ctx context.Context, deviceID string) error {
	delete(m.devices, deviceID)
	return nil
}

// AddDevice simulates a device registering with the control plane
func (m *MockControlAPI) AddDevice(device controlapi.Device) {
	m.devices[device.ID] = device
}

// GetDevice returns a device by ID
func (m *MockControlAPI) GetDevice(deviceID string) (controlapi.Device, bool) {
	device, exists := m.devices[deviceID]
	return device, exists
}

// SetupE2ETestConfig creates test configuration from environment variables
func SetupE2ETestConfig(t *testing.T) *E2ETestConfig {
	cfg := config.LoadFromEnv()
	controlAPI := NewMockControlAPI()

	// Default test configuration
	testConfig := &E2ETestConfig{
		Config:           cfg,
		ControlAPI:       controlAPI,
		EnabledProviders: make(map[string]bool),
		TestRegions:      make(map[string]string),
		Timeout:          10 * time.Minute,
		SkipCleanup:      os.Getenv("E2E_SKIP_CLEANUP") == "true",
	}

	// Check which providers have credentials configured
	if cfg.Providers.DigitalOcean.Token != "" {
		testConfig.EnabledProviders["do"] = true
		testConfig.TestRegions["do"] = "nyc1" // Cheap region
	}

	if cfg.Providers.Vultr.APIKey != "" {
		testConfig.EnabledProviders["vultr"] = true
		testConfig.TestRegions["vultr"] = "ewr" // Newark - usually cheap
	}

	if cfg.Providers.Linode.Token != "" {
		testConfig.EnabledProviders["linode"] = true
		testConfig.TestRegions["linode"] = "us-east" // Fremont - usually cheap
	}

	// AWS - check if credentials are available
	if os.Getenv("AWS_ACCESS_KEY_ID") != "" || cfg.Providers.AWS.AccessKey != "" {
		testConfig.EnabledProviders["ec2"] = true
		testConfig.TestRegions["ec2"] = "us-east-1" // Virginia - usually cheapest
	}

	// GCP - check if credentials are available
	if cfg.Providers.GCP.CredentialsJSON != "" || os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "" {
		testConfig.EnabledProviders["gcp"] = true
		testConfig.TestRegions["gcp"] = "us-central1-a" // Iowa - usually cheap
	}

	// Override regions from environment if specified
	if region := os.Getenv("E2E_DO_REGION"); region != "" {
		testConfig.TestRegions["do"] = region
	}
	if region := os.Getenv("E2E_VULTR_REGION"); region != "" {
		testConfig.TestRegions["vultr"] = region
	}
	if region := os.Getenv("E2E_LINODE_REGION"); region != "" {
		testConfig.TestRegions["linode"] = region
	}
	if region := os.Getenv("E2E_AWS_REGION"); region != "" {
		testConfig.TestRegions["ec2"] = region
	}
	if region := os.Getenv("E2E_GCP_REGION"); region != "" {
		testConfig.TestRegions["gcp"] = region
	}

	// Parse timeout from environment
	if timeoutStr := os.Getenv("E2E_TIMEOUT"); timeoutStr != "" {
		if timeout, err := time.ParseDuration(timeoutStr); err == nil {
			testConfig.Timeout = timeout
		}
	}

	return testConfig
}

// TestE2E_AllProvidersLifecycle tests the complete lifecycle for each cloud provider
func TestE2E_AllProvidersLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E tests in short mode")
	}

	testConfig := SetupE2ETestConfig(t)

	if len(testConfig.EnabledProviders) == 0 {
		t.Skip("No cloud provider credentials found - set environment variables for at least one provider")
	}

	t.Logf("Configured providers for E2E tests: %v", getEnabledProviderNames(testConfig.EnabledProviders))
	t.Logf("Test regions: %v", testConfig.TestRegions)
	t.Logf("Test timeout: %v", testConfig.Timeout)

	for providerName := range testConfig.EnabledProviders {
		t.Run(fmt.Sprintf("Provider_%s", providerName), func(t *testing.T) {
			testProviderLifecycle(t, testConfig, providerName)
		})
	}
}

// testProviderLifecycle tests the complete lifecycle for a single provider
func testProviderLifecycle(t *testing.T, testConfig *E2ETestConfig, providerName string) {
	ctx, cancel := context.WithTimeout(context.Background(), testConfig.Timeout)
	defer cancel()

	region := testConfig.TestRegions[providerName]
	if region == "" {
		t.Fatalf("No test region configured for provider %s", providerName)
	}

	// Initialize provider
	providerFactory := providers.ProviderFactoryRegistry[providerName]
	if providerFactory == nil {
		t.Fatalf("Provider %s not registered", providerName)
	}

	provider, err := providerFactory(ctx, testConfig.Config)
	if err != nil {
		t.Fatalf("Failed to initialize provider %s: %v", providerName, err)
	}
	if provider == nil {
		t.Fatalf("Provider %s returned nil (likely missing credentials)", providerName)
	}

	t.Logf("Testing provider %s in region %s", providerName, region)

	// Test 1: Verify region listing
	t.Run("ListRegions", func(t *testing.T) {
		regions, err := provider.ListRegions(ctx)
		if err != nil {
			t.Fatalf("Failed to list regions: %v", err)
		}
		if len(regions) == 0 {
			t.Fatal("No regions returned")
		}

		// Verify our test region exists
		found := false
		for _, r := range regions {
			if r.Code == region {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("Test region %s not found in available regions", region)
		}
		t.Logf("Found %d regions, test region %s is available", len(regions), region)
	})

	// Test 2: Check initial instance status (should be missing)
	t.Run("InitialStatus", func(t *testing.T) {
		status, err := provider.GetInstanceStatus(ctx, region)
		if err != nil {
			t.Fatalf("Failed to get initial instance status: %v", err)
		}
		if status != providers.InstanceStatusMissing {
			t.Errorf("Expected InstanceStatusMissing, got %v", status)
		}
		t.Logf("Initial status check passed: no existing instance")
	})

	// Test 3: Create instance
	var instanceID providers.InstanceID
	t.Run("CreateInstance", func(t *testing.T) {
		// Create preauth key
		key, err := testConfig.ControlAPI.CreateKey(ctx)
		if err != nil {
			t.Fatalf("Failed to create preauth key: %v", err)
		}

		// Create instance
		instanceID, err = provider.CreateInstance(ctx, region, key)
		if err != nil {
			t.Fatalf("Failed to create instance: %v", err)
		}

		// Validate instance ID
		if instanceID.Hostname == "" {
			t.Error("Instance hostname is empty")
		}
		if instanceID.ProviderID == "" {
			t.Error("Instance provider ID is empty")
		}
		if instanceID.ProviderName == "" {
			t.Error("Instance provider name is empty")
		}

		t.Logf("Created instance: hostname=%s, providerID=%s, providerName=%s",
			instanceID.Hostname, instanceID.ProviderID, instanceID.ProviderName)

		// Add simulated device to mock control API
		mockAPI := testConfig.ControlAPI.(*MockControlAPI)
		mockAPI.AddDevice(controlapi.Device{
			ID:       instanceID.ProviderID,
			Hostname: instanceID.Hostname,
			Created:  time.Now(),
			LastSeen: time.Now(),
			IPAddrs:  []string{"100.64.0.1"}, // Mock Tailscale IP
			IsOnline: true,
			Tags:     []string{"tag:exit"},
		})
	})

	// Ensure cleanup happens even if tests fail
	defer func() {
		if !testConfig.SkipCleanup && instanceID.ProviderID != "" {
			t.Logf("Cleaning up instance %s", instanceID.ProviderID)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cleanupCancel()

			if err := provider.DeleteInstance(cleanupCtx, instanceID); err != nil {
				t.Errorf("Failed to cleanup instance %s: %v", instanceID.ProviderID, err)
			} else {
				t.Logf("Successfully cleaned up instance %s", instanceID.ProviderID)
			}
		}
	}()

	// Test 4: Verify instance is running
	t.Run("VerifyRunning", func(t *testing.T) {
		// Wait for instance to be running
		var status providers.InstanceStatus
		var err error
		for attempts := 0; attempts < 30; attempts++ {
			status, err = provider.GetInstanceStatus(ctx, region)
			if err != nil {
				t.Fatalf("Failed to get instance status: %v", err)
			}
			if status == providers.InstanceStatusRunning {
				break
			}
			time.Sleep(10 * time.Second)
		}

		if status != providers.InstanceStatusRunning {
			t.Fatalf("Instance not running after waiting, status: %v", status)
		}
		t.Logf("Instance is running")
	})

	// Test 5: List instances and verify our instance is there
	t.Run("ListInstances", func(t *testing.T) {
		instances, err := provider.ListInstances(ctx, region)
		if err != nil {
			t.Fatalf("Failed to list instances: %v", err)
		}

		found := false
		for _, inst := range instances {
			if inst.ProviderID == instanceID.ProviderID {
				found = true
				break
			}
		}

		if !found {
			t.Errorf("Created instance %s not found in list of instances", instanceID.ProviderID)
		}
		t.Logf("Found %d instances in region, our instance is listed", len(instances))
	})

	// Test 6: Verify hostname generation
	t.Run("HostnameGeneration", func(t *testing.T) {
		hostname := provider.Hostname(region)
		if hostname == "" {
			t.Error("Generated hostname is empty")
		}
		if !strings.Contains(string(hostname), providerName) {
			t.Errorf("Hostname %s does not contain provider name %s", hostname, providerName)
		}
		if !strings.Contains(string(hostname), region) {
			t.Errorf("Hostname %s does not contain region %s", hostname, region)
		}
		t.Logf("Generated hostname: %s", hostname)
	})

	// Test 7: Verify pricing information
	t.Run("PricingInfo", func(t *testing.T) {
		price := provider.GetRegionPrice(region)
		if price <= 0 {
			t.Errorf("Region price should be positive, got %f", price)
		}
		t.Logf("Region %s price: $%.4f/hour", region, price)
	})

	// Test 8: Delete instance
	t.Run("DeleteInstance", func(t *testing.T) {
		err := provider.DeleteInstance(ctx, instanceID)
		if err != nil {
			t.Fatalf("Failed to delete instance: %v", err)
		}
		t.Logf("Successfully initiated instance deletion")

		// Wait for instance to be deleted
		var status providers.InstanceStatus
		for attempts := 0; attempts < 30; attempts++ {
			status, err = provider.GetInstanceStatus(ctx, region)
			if err != nil {
				t.Fatalf("Failed to get instance status during deletion: %v", err)
			}
			if status == providers.InstanceStatusMissing {
				break
			}
			time.Sleep(10 * time.Second)
		}

		if status != providers.InstanceStatusMissing {
			t.Errorf("Instance still exists after deletion attempt, status: %v", status)
		}

		// Remove from mock control API
		mockAPI := testConfig.ControlAPI.(*MockControlAPI)
		err = mockAPI.DeleteDevice(ctx, instanceID.ProviderID)
		if err != nil {
			t.Errorf("Failed to remove device from control API: %v", err)
		}

		t.Logf("Instance deletion verified")
		// Clear instanceID to prevent duplicate cleanup
		instanceID = providers.InstanceID{}
	})

	// Test 9: Verify instance is gone via cloud provider API
	t.Run("VerifyDeletion", func(t *testing.T) {
		instances, err := provider.ListInstances(ctx, region)
		if err != nil {
			t.Fatalf("Failed to list instances after deletion: %v", err)
		}

		// Verify our instance is not in the list
		for _, inst := range instances {
			if inst.ProviderID == instanceID.ProviderID {
				t.Errorf("Deleted instance %s still appears in instance list", instanceID.ProviderID)
			}
		}

		t.Logf("Deletion verification passed: instance not found in provider instance list")
	})
}

// getEnabledProviderNames returns a sorted list of enabled provider names
func getEnabledProviderNames(enabled map[string]bool) []string {
	names := make([]string, 0, len(enabled))
	for name := range enabled {
		names = append(names, name)
	}
	return names
}

// TestE2E_MultipleProvidersParallel tests multiple providers in parallel
func TestE2E_MultipleProvidersParallel(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E tests in short mode")
	}

	testConfig := SetupE2ETestConfig(t)

	if len(testConfig.EnabledProviders) < 2 {
		t.Skip("Need at least 2 providers for parallel test")
	}

	t.Logf("Running parallel E2E test for providers: %v", getEnabledProviderNames(testConfig.EnabledProviders))

	// Run providers in parallel
	for providerName := range testConfig.EnabledProviders {
		providerName := providerName // Capture for goroutine
		t.Run(fmt.Sprintf("Parallel_%s", providerName), func(t *testing.T) {
			t.Parallel()
			testProviderLifecycle(t, testConfig, providerName)
		})
	}
}

// TestE2E_StressTest creates and deletes instances multiple times
func TestE2E_StressTest(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping E2E tests in short mode")
	}

	if os.Getenv("E2E_STRESS_TEST") != "true" {
		t.Skip("Stress test disabled - set E2E_STRESS_TEST=true to enable")
	}

	testConfig := SetupE2ETestConfig(t)

	if len(testConfig.EnabledProviders) == 0 {
		t.Skip("No cloud provider credentials found")
	}

	// Pick the first available provider for stress testing
	var providerName string
	for name := range testConfig.EnabledProviders {
		providerName = name
		break
	}

	iterations := 3
	if iterStr := os.Getenv("E2E_STRESS_ITERATIONS"); iterStr != "" {
		if iter, err := fmt.Sscanf(iterStr, "%d", &iterations); err == nil && iter == 1 {
			// Successfully parsed
		}
	}

	t.Logf("Running stress test with %d iterations on provider %s", iterations, providerName)

	for i := 0; i < iterations; i++ {
		t.Run(fmt.Sprintf("Iteration_%d", i+1), func(t *testing.T) {
			testProviderLifecycle(t, testConfig, providerName)
		})
	}
}
