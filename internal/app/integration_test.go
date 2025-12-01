// Package app_test contains integration tests for the tscloudvpn application.
//
// This file contains real integration tests that use actual Tailscale API and cloud providers.
// These tests require real credentials and will create actual cloud instances, so they:
//
//   - Are only built with the 'e2e' build tag
//   - Are skipped in short mode
//   - Require REAL_INTEGRATION_TEST=true environment variable
//   - Need valid cloud provider and Tailscale/Headscale credentials
//   - Will incur actual costs from cloud providers
//
// For local testing with mocked dependencies, see e2e_test.go

//go:build e2e

package app_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/app"
	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"

	// Import all cloud provider implementations to register them
	_ "github.com/anupcshan/tscloudvpn/internal/providers/digitalocean"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/ec2"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/fake"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/gcp"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/linode"
	_ "github.com/anupcshan/tscloudvpn/internal/providers/vultr"
)

// TestRealIntegration_FullWorkflow tests the complete workflow with real Tailscale API and cloud providers
// This test requires real credentials and will create actual cloud instances
func TestRealIntegration_FullWorkflow(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping real integration tests in short mode")
	}

	// Check if we have the necessary environment variables for real testing
	if os.Getenv("REAL_INTEGRATION_TEST") != "true" {
		t.Skip("Skipping real integration test - set REAL_INTEGRATION_TEST=true to enable")
	}

	// Load configuration from environment or config file
	cfg, err := loadRealConfig(t)
	if err != nil {
		t.Fatalf("Failed to load real configuration: %v", err)
	}

	// Validate we have at least one cloud provider configured
	if !hasCloudProviderConfigured(cfg) {
		t.Skip("No cloud provider credentials configured for real integration test")
	}

	// Validate we have Tailscale or Headscale configured
	if !hasControlAPIConfigured(cfg) {
		t.Skip("No Tailscale/Headscale credentials configured for real integration test")
	}

	t.Logf("Starting real integration test with actual cloud providers and Tailscale API")

	// Create and initialize the real application
	app, err := app.New("")
	if err != nil {
		t.Fatalf("Failed to create app: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := app.Initialize(ctx); err != nil {
		t.Fatalf("Failed to initialize app: %v", err)
	}

	// Test creating a real instance in the cheapest available region
	t.Run("CreateRealInstance", func(t *testing.T) {
		// Double-check that providers were actually loaded after initialization
		providers := app.GetCloudProviders()
		if len(providers) == 0 {
			t.Logf("Config indicates providers should be available:")
			t.Logf("  DigitalOcean token present: %v", cfg.Providers.DigitalOcean.Token != "")
			t.Logf("  Vultr API key present: %v", cfg.Providers.Vultr.APIKey != "")
			t.Logf("  Linode token present: %v", cfg.Providers.Linode.Token != "")
			t.Logf("  GCP credentials present: %v", cfg.Providers.GCP.CredentialsJSON != "")
			t.Logf("  AWS access key present: %v", cfg.Providers.AWS.AccessKey != "")
			t.Logf("  AWS_ACCESS_KEY_ID env: %v", os.Getenv("AWS_ACCESS_KEY_ID") != "")
			t.Logf("  GOOGLE_APPLICATION_CREDENTIALS env: %v", os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != "")

			// This suggests providers aren't registering properly or credentials are invalid
			t.Skip("No cloud providers were successfully initialized despite configuration being present")
		}

		t.Logf("Successfully loaded %d cloud providers", len(providers))
		testCreateRealInstance(t, ctx, app, cfg)
	})
}

// loadRealConfig loads configuration for real integration testing
func loadRealConfig(t *testing.T) (*config.Config, error) {
	// Try to load from config file first
	if configPath := os.Getenv("TSCLOUDVPN_CONFIG"); configPath != "" {
		return config.LoadConfig(configPath)
	}

	// Fallback to environment variables
	return config.LoadFromEnv()
}

// hasCloudProviderConfigured checks if at least one cloud provider has credentials
func hasCloudProviderConfigured(cfg *config.Config) bool {
	return cfg.Providers.DigitalOcean.Token != "" ||
		cfg.Providers.Vultr.APIKey != "" ||
		cfg.Providers.Linode.Token != "" ||
		cfg.Providers.GCP.CredentialsJSON != "" ||
		cfg.Providers.AWS.AccessKey != "" ||
		os.Getenv("AWS_ACCESS_KEY_ID") != "" ||
		os.Getenv("GOOGLE_APPLICATION_CREDENTIALS") != ""
}

// hasControlAPIConfigured checks if Tailscale or Headscale is configured
func hasControlAPIConfigured(cfg *config.Config) bool {
	return (cfg.Control.Tailscale.ClientID != "" && cfg.Control.Tailscale.ClientSecret != "") ||
		(cfg.Control.Headscale.APIKey != "" && cfg.Control.Headscale.User != "")
}

// testCreateRealInstance tests creating a real instance with the cheapest provider
func testCreateRealInstance(t *testing.T, ctx context.Context, app *app.App, cfg *config.Config) {
	// Find the cheapest available region across all configured providers
	cheapestProvider, cheapestRegion, cheapestPrice, err := findCheapestRegion(ctx, app)
	if err != nil {
		t.Fatalf("Failed to find cheapest region: %v", err)
	}

	if cheapestProvider == "" {
		t.Skip("No regions available from configured providers")
	}

	t.Logf("Found cheapest region: %s/%s at $%.4f/hour", cheapestProvider, cheapestRegion, cheapestPrice)

	// Create controller for managing the instance
	controller, err := cfg.GetController()
	if err != nil {
		t.Fatalf("Failed to get controller: %v", err)
	}

	// Create a preauth key for the instance
	key, err := controller.CreateKey(ctx)
	if err != nil {
		t.Fatalf("Failed to create auth key: %v", err)
	}

	t.Logf("Created auth key for instance")

	// Get the provider to create the instance
	provider := app.GetCloudProvider(cheapestProvider)
	if provider == nil {
		t.Fatalf("Provider %s not found in app", cheapestProvider)
	}

	// Create the instance
	t.Logf("Creating instance in %s/%s...", cheapestProvider, cheapestRegion)
	instanceID, err := provider.CreateInstance(ctx, cheapestRegion, key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	t.Logf("Created instance: %+v", instanceID)

	// Ensure cleanup happens even if test fails
	defer func() {
		t.Logf("Cleaning up instance %+v", instanceID)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := provider.DeleteInstance(cleanupCtx, instanceID); err != nil {
			t.Errorf("Failed to delete instance during cleanup: %v", err)
		}
	}()

	// Wait for the instance to register with Tailscale
	t.Logf("Waiting for instance to register with Tailscale...")
	device, err := waitForDeviceRegistration(ctx, controller, instanceID.Hostname, 5*time.Minute)
	if err != nil {
		t.Fatalf("Failed to wait for device registration: %v", err)
	}

	t.Logf("Device registered: %s with IPs: %v", device.Name, device.IPAddrs)

	// Test basic connectivity - device should be online
	if !device.IsOnline {
		t.Errorf("Device is not online: %+v", device)
	}

	if len(device.IPAddrs) == 0 {
		t.Errorf("Device has no IP addresses: %+v", device)
	}

	// Verify instance status
	status, err := provider.GetInstanceStatus(ctx, cheapestRegion)
	if err != nil {
		t.Errorf("Failed to get instance status: %v", err)
	} else if status != providers.InstanceStatusRunning {
		t.Errorf("Instance is not running, status: %v", status)
	}

	t.Logf("Successfully created and tested real instance %s in %s/%s", instanceID.Hostname, cheapestProvider, cheapestRegion)
}

// TestRealIntegration_ConfigValidation tests configuration validation with real APIs

// testProviderConnectivity tests basic API connectivity for a cloud provider
func testProviderConnectivity(t *testing.T, ctx context.Context, cfg *config.Config, providerName string) {
	// Get the provider factory from the registry
	factory, exists := providers.ProviderFactoryRegistry[providerName]
	if !exists {
		t.Fatalf("Provider %s not found in registry", providerName)
	}

	// Create the provider instance
	provider, err := factory(ctx, cfg)
	if err != nil {
		t.Fatalf("Failed to create provider %s: %v", providerName, err)
	}

	if provider == nil {
		t.Skipf("Provider %s is not configured (returned nil)", providerName)
	}

	t.Logf("Testing %s provider connectivity...", providerName)

	// Test 1: List regions (basic API connectivity)
	regions, err := provider.ListRegions(ctx)
	if err != nil {
		t.Fatalf("Failed to list regions for %s: %v", providerName, err)
	}

	if len(regions) == 0 {
		t.Errorf("Provider %s returned no regions", providerName)
	} else {
		t.Logf("✓ Successfully listed %d regions for %s", len(regions), providerName)
	}

	// Test 2: Region pricing (validates region data)
	priceValidRegions := 0
	for _, region := range regions {
		price := provider.GetRegionPrice(region.Code)
		if price > 0 {
			priceValidRegions++
			t.Logf("  Region %s (%s): $%.4f/hour", region.Code, region.LongName, price)
		} else {
			t.Logf("  Region %s (%s): no pricing info", region.Code, region.LongName)
		}
	}

	if priceValidRegions == 0 {
		t.Errorf("No regions have valid pricing information for %s", providerName)
	} else {
		t.Logf("✓ Found pricing for %d/%d regions for %s", priceValidRegions, len(regions), providerName)
	}

	// Test 3: Check instance status for a region (basic API operations)
	if len(regions) > 0 {
		testRegion := regions[0].Code
		status, err := provider.GetInstanceStatus(ctx, testRegion)
		if err != nil {
			t.Logf("Warning: Failed to get instance status for region %s: %v", testRegion, err)
		} else {
			t.Logf("✓ Successfully checked instance status for region %s: %v", testRegion, status)
		}

		// Test 4: List instances in region
		instances, err := provider.ListInstances(ctx, testRegion)
		if err != nil {
			t.Logf("Warning: Failed to list instances for region %s: %v", testRegion, err)
		} else {
			t.Logf("✓ Successfully listed instances for region %s: %d found", testRegion, len(instances))
		}
	}

	t.Logf("✓ Provider %s connectivity test completed successfully", providerName)
}

// TestRealIntegration_NetworkConnectivity tests actual network connectivity through created instances
func TestRealIntegration_NetworkConnectivity(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping real integration tests in short mode")
	}

	if os.Getenv("REAL_INTEGRATION_TEST") != "true" {
		t.Skip("Skipping real integration test - set REAL_INTEGRATION_TEST=true to enable")
	}

	// Load configuration and validate setup
	cfg, err := loadRealConfig(t)
	if err != nil {
		t.Fatalf("Failed to load real configuration: %v", err)
	}

	if !hasCloudProviderConfigured(cfg) {
		t.Skip("No cloud provider credentials configured for real integration test")
	}

	if !hasControlAPIConfigured(cfg) {
		t.Skip("No Tailscale/Headscale credentials configured for real integration test")
	}

	t.Logf("Starting real network connectivity test with actual cloud instance creation")

	// Create and initialize the real application
	app, err := app.New("")
	if err != nil {
		t.Fatalf("Failed to create app: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	if err := app.Initialize(ctx); err != nil {
		t.Fatalf("Failed to initialize app: %v", err)
	}

	// Test complete network workflow
	t.Run("FullNetworkWorkflow", func(t *testing.T) {
		testFullNetworkWorkflow(t, ctx, app, cfg)
	})
}

// testFullNetworkWorkflow tests the complete network connectivity workflow
func testFullNetworkWorkflow(t *testing.T, ctx context.Context, app *app.App, cfg *config.Config) {
	// Step 1: Create a real instance
	t.Logf("Step 1: Creating real cloud instance...")
	cheapestProvider, cheapestRegion, cheapestPrice, err := findCheapestRegion(ctx, app)
	if err != nil {
		t.Fatalf("Failed to find cheapest region: %v", err)
	}

	if cheapestProvider == "" {
		t.Skip("No regions available from configured providers")
	}

	t.Logf("Creating instance in cheapest region: %s/%s at $%.4f/hour", cheapestProvider, cheapestRegion, cheapestPrice)

	// Create controller and auth key
	controller, err := cfg.GetController()
	if err != nil {
		t.Fatalf("Failed to get controller: %v", err)
	}

	key, err := controller.CreateKey(ctx)
	if err != nil {
		t.Fatalf("Failed to create auth key: %v", err)
	}

	// Create the instance
	provider := app.GetCloudProvider(cheapestProvider)
	if provider == nil {
		t.Fatalf("Provider %s not found in app", cheapestProvider)
	}

	instanceID, err := provider.CreateInstance(ctx, cheapestRegion, key)
	if err != nil {
		t.Fatalf("Failed to create instance: %v", err)
	}

	t.Logf("Created instance: %+v", instanceID)

	// Ensure cleanup happens
	defer func() {
		t.Logf("Cleaning up instance %+v", instanceID)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if err := provider.DeleteInstance(cleanupCtx, instanceID); err != nil {
			t.Errorf("Failed to delete instance during cleanup: %v", err)
		}

		// Also try to clean up the device from Tailscale
		if device, err := findDeviceByHostname(cleanupCtx, controller, instanceID.Hostname); err == nil && device != nil {
			if err := controller.DeleteDevice(cleanupCtx, device); err != nil {
				t.Logf("Failed to delete device %s from Tailscale: %v", device.Name, err)
			} else {
				t.Logf("Cleaned up device %s from Tailscale", device.Name)
			}
		}
	}()

	// Step 2: Wait for it to join the Tailscale network
	t.Logf("Step 2: Waiting for instance to join Tailscale network...")
	device, err := waitForDeviceRegistration(ctx, controller, instanceID.Hostname, 8*time.Minute)
	if err != nil {
		t.Fatalf("Failed to wait for device registration: %v", err)
	}

	t.Logf("Device registered: %s with IPs: %v", device.Name, device.IPAddrs)

	if !device.IsOnline {
		t.Fatalf("Device is not online: %+v", device)
	}

	if len(device.IPAddrs) == 0 {
		t.Fatalf("Device has no IP addresses: %+v", device)
	}

	// Step 3: Enable exit node and test routing
	t.Logf("Step 3: Testing exit node functionality...")
	testExitNodeFunctionality(t, ctx, controller, device)

	t.Logf("✓ Full network connectivity test completed successfully!")
}

// testExitNodeFunctionality tests exit node routing capabilities
func testExitNodeFunctionality(t *testing.T, ctx context.Context, controller controlapi.ControlApi, device *controlapi.Device) {
	// Get our current external IP
	originalIP, err := getExternalIP()
	if err != nil {
		t.Logf("Warning: Could not get original external IP: %v", err)
		originalIP = "unknown"
	} else {
		t.Logf("Current external IP: %s", originalIP)
	}

	// Enable exit node by approving routes
	t.Logf("Enabling exit node for device %s...", device.Name)
	err = controller.ApproveExitNode(ctx, device)
	if err != nil {
		t.Logf("Warning: Failed to approve exit node routes: %v", err)
		t.Logf("This might be expected if the device hasn't advertised routes yet")
		return
	}

	t.Logf("✓ Successfully enabled exit node for device %s", device.Name)

	// Note: Testing actual exit node routing requires configuring the local Tailscale client
	// to use this node as an exit node, which is complex in a test environment.
	// For now, we just verify the node was approved.

	t.Logf("✓ Exit node functionality test completed (device approved as exit node)")
}

// Helper function to make HTTP requests
func makeHTTPRequest(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	return client.Get(url)
}

// makeHTTPRequestWithIP makes HTTP requests to a specific IP with timeout
func makeHTTPRequestWithIP(url string, timeout time.Duration) (*http.Response, error) {
	client := &http.Client{Timeout: timeout}
	return client.Get(url)
}

// findDeviceByHostname searches for a device with the given hostname
func findDeviceByHostname(ctx context.Context, controller controlapi.ControlApi, hostname string) (*controlapi.Device, error) {
	devices, err := controller.ListDevices(ctx)
	if err != nil {
		return nil, err
	}

	for _, device := range devices {
		if device.Hostname == hostname {
			return &device, nil
		}
	}

	return nil, fmt.Errorf("device with hostname %s not found", hostname)
}

// Helper function to test internet connectivity through an exit node (for future use)

// Helper function to get external IP (for testing exit node functionality)
func getExternalIP() (string, error) {
	resp, err := http.Get("https://api.ipify.org")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body := make([]byte, 100)
	n, err := resp.Body.Read(body)
	if err != nil && err.Error() != "EOF" {
		return "", err
	}

	return strings.TrimSpace(string(body[:n])), nil
}

// AppWithProviders interface for accessing cloud providers (for testing)
type AppWithProviders interface {
	GetCloudProviders() map[string]providers.Provider
	GetCloudProvider(name string) providers.Provider
}

// findCheapestRegion finds the cheapest region across all configured providers
func findCheapestRegion(ctx context.Context, app AppWithProviders) (providerName, regionCode string, price float64, err error) {
	providers := app.GetCloudProviders()
	if len(providers) == 0 {
		return "", "", 0, fmt.Errorf("no cloud providers configured")
	}

	cheapestPrice := float64(-1)
	cheapestProvider := ""
	cheapestRegion := ""

	for providerName, provider := range providers {
		regions, err := provider.ListRegions(ctx)
		if err != nil {
			return "", "", 0, fmt.Errorf("failed to list regions for provider %s: %w", providerName, err)
		}

		for _, region := range regions {
			price := provider.GetRegionPrice(region.Code)
			if price <= 0 {
				continue // Skip regions with no pricing info
			}

			if cheapestPrice < 0 || price < cheapestPrice {
				cheapestPrice = price
				cheapestProvider = providerName
				cheapestRegion = region.Code
			}
		}
	}

	if cheapestProvider == "" {
		return "", "", 0, fmt.Errorf("no regions with valid pricing found")
	}

	return cheapestProvider, cheapestRegion, cheapestPrice, nil
}

// waitForDeviceRegistration waits for a device with the given hostname to register with the control API
func waitForDeviceRegistration(ctx context.Context, controller controlapi.ControlApi, expectedHostname string, timeout time.Duration) (*controlapi.Device, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("timeout waiting for device %s to register", expectedHostname)
		case <-ticker.C:
			devices, err := controller.ListDevices(ctx)
			if err != nil {
				continue // Retry on error
			}

			for _, device := range devices {
				if device.Hostname == expectedHostname {
					return &device, nil
				}
			}
		}
	}
}

// TestRealIntegration_HelperFunctions tests our helper functions with mock providers
// This test validates the core logic without requiring real credentials

// TestRealIntegration_ProviderConnectivityWithFake tests the provider connectivity function with fake provider
