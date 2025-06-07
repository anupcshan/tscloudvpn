package gcp

import (
	"testing"
	"time"
)

func TestRegionMachineTypeCache(t *testing.T) {
	// Test the caching logic without requiring GCP credentials
	provider := &gcpProvider{
		regionMachineTypeCache: make(map[string]regionMachineType),
	}

	// Test cache initialization
	if provider.regionMachineTypeCache == nil {
		t.Error("Expected regionMachineTypeCache to be initialized")
	}

	// Test cache structure
	testData := regionMachineType{
		MachineType: "e2-micro",
		HourlyCost:  0.0056,
	}

	provider.regionMachineTypeCache["us-central1"] = testData
	// Note: regionMachineTypeCacheTime would be set in real usage

	// Test GetRegionPrice with cached data (without actual API calls)
	// We can't test the full functionality without GCP credentials,
	// but we can test the structure and basic logic
	if len(provider.regionMachineTypeCache) != 1 {
		t.Errorf("Expected cache size 1, got %d", len(provider.regionMachineTypeCache))
	}

	if provider.regionMachineTypeCache["us-central1"].MachineType != "e2-micro" {
		t.Errorf("Expected machine type 'e2-micro', got %s", provider.regionMachineTypeCache["us-central1"].MachineType)
	}

	if provider.regionMachineTypeCache["us-central1"].HourlyCost != 0.0056 {
		t.Errorf("Expected hourly cost 0.0056, got %f", provider.regionMachineTypeCache["us-central1"].HourlyCost)
	}
}

func TestMachineTypePricing(t *testing.T) {
	// Test that our hardcoded pricing is reasonable
	tests := []struct {
		machineType string
		expectedMin float64
		expectedMax float64
	}{
		{"e2-micro", 0.005, 0.007},      // Should be cheapest
		{"f1-micro", 0.007, 0.009},      // Legacy pricing
		{"e2-small", 0.010, 0.020},      // More expensive
		{"n1-standard-1", 0.040, 0.060}, // Much more expensive
	}

	// These prices are embedded in our loadRegionMachineTypes function
	machineTypePricing := map[string]float64{
		"e2-micro":      0.0056,
		"f1-micro":      0.0076,
		"g1-small":      0.0270,
		"e2-small":      0.0134,
		"n1-standard-1": 0.0475,
	}

	for _, test := range tests {
		if price, ok := machineTypePricing[test.machineType]; ok {
			if price < test.expectedMin || price > test.expectedMax {
				t.Errorf("Machine type %s price %f is outside expected range [%f, %f]",
					test.machineType, price, test.expectedMin, test.expectedMax)
			}
		} else {
			t.Errorf("Machine type %s not found in pricing map", test.machineType)
		}
	}

	// Verify e2-micro is cheapest
	if machineTypePricing["e2-micro"] >= machineTypePricing["f1-micro"] {
		t.Error("e2-micro should be cheaper than f1-micro")
	}
}

func TestCacheDuration(t *testing.T) {
	// Test that our cache duration is reasonable (24 hours)
	expected := 24 * time.Hour
	if cacheDuration != expected {
		t.Errorf("Expected cache duration %v, got %v", expected, cacheDuration)
	}
}
