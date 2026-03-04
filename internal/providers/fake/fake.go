package fake

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/config"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

// InstanceState represents the internal state of a fake instance
type InstanceState struct {
	ID        string
	Region    string
	Status    providers.InstanceStatus
	CreatedAt time.Time
}

// ProviderConfig allows controlling the behavior of the fake provider
type ProviderConfig struct {
	// CreateDelay simulates the time it takes to create an instance
	CreateDelay time.Duration
	// CreateFailure causes creation to fail if set
	CreateFailure error
	// StatusCheckDelay simulates the time it takes to check status
	StatusCheckDelay time.Duration
	// StatusFailure causes status checks to fail if set
	StatusFailure error
	// RegionListDelay simulates the time it takes to list regions
	RegionListDelay time.Duration
	// RegionListFailure causes region listing to fail if set
	RegionListFailure error
	// PricePerHour sets the price for all regions
	PricePerHour float64
}

// DefaultConfig returns a configuration suitable for testing
func DefaultConfig() *ProviderConfig {
	return &ProviderConfig{
		CreateDelay:      100 * time.Millisecond,
		StatusCheckDelay: 50 * time.Millisecond,
		RegionListDelay:  50 * time.Millisecond,
		PricePerHour:     0.001, // Very cheap for testing
	}
}

// FakeProvider implements a configurable fake provider for testing
type FakeProvider struct {
	mu        sync.RWMutex
	instances map[string]*InstanceState // key: region
	config    *ProviderConfig
	counter   int // for generating unique IDs
}

// New creates a new fake provider instance
func New(ctx context.Context, cfg *config.Config) (providers.Provider, error) {
	return NewWithConfig(DefaultConfig()), nil
}

// NewWithConfig creates a new fake provider with custom configuration
func NewWithConfig(config *ProviderConfig) *FakeProvider {
	if config == nil {
		config = DefaultConfig()
	}

	return &FakeProvider{
		instances: make(map[string]*InstanceState),
		config:    config,
	}
}

// CreateInstance simulates creating a cloud instance
func (f *FakeProvider) CreateInstance(ctx context.Context, req providers.CreateRequest) (providers.Instance, error) {
	region := req.Region
	f.mu.RLock()
	createDelay := f.config.CreateDelay
	createFailure := f.config.CreateFailure
	f.mu.RUnlock()

	// Simulate creation delay
	if createDelay > 0 {
		select {
		case <-time.After(createDelay):
		case <-ctx.Done():
			return providers.Instance{}, ctx.Err()
		}
	}

	// Check for configured failure
	if createFailure != nil {
		return providers.Instance{}, createFailure
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Check if instance already exists
	if existing, exists := f.instances[region]; exists {
		if existing.Status == providers.InstanceStatusRunning {
			return providers.Instance{
				Hostname:     string(f.Hostname(region)),
				ProviderID:   existing.ID,
				ProviderName: "fake",
				HourlyCost:   f.config.PricePerHour,
			}, nil
		}
	}

	// Generate unique ID
	f.counter++
	instanceID := fmt.Sprintf("fake-%d-%d", time.Now().Unix(), f.counter)

	// Create new instance
	instance := &InstanceState{
		ID:        instanceID,
		Region:    region,
		Status:    providers.InstanceStatusRunning,
		CreatedAt: time.Now(),
	}

	f.instances[region] = instance

	return providers.Instance{
		Hostname:     string(f.Hostname(region)),
		ProviderID:   instanceID,
		ProviderName: "fake",
		HourlyCost:   f.config.PricePerHour,
	}, nil
}

// GetInstanceStatus returns the status of an instance in a region
func (f *FakeProvider) GetInstanceStatus(ctx context.Context, region string) (providers.InstanceStatus, error) {
	f.mu.RLock()
	statusCheckDelay := f.config.StatusCheckDelay
	statusFailure := f.config.StatusFailure
	f.mu.RUnlock()

	// Simulate status check delay
	if statusCheckDelay > 0 {
		select {
		case <-time.After(statusCheckDelay):
		case <-ctx.Done():
			return providers.InstanceStatusMissing, ctx.Err()
		}
	}

	// Check for configured failure
	if statusFailure != nil {
		return providers.InstanceStatusMissing, statusFailure
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	instance, exists := f.instances[region]
	if !exists {
		return providers.InstanceStatusMissing, nil
	}

	return instance.Status, nil
}

// ListRegions returns a fixed list of fake regions
func (f *FakeProvider) ListRegions(ctx context.Context) ([]providers.Region, error) {
	f.mu.RLock()
	regionListDelay := f.config.RegionListDelay
	regionListFailure := f.config.RegionListFailure
	f.mu.RUnlock()

	// Simulate region list delay
	if regionListDelay > 0 {
		select {
		case <-time.After(regionListDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Check for configured failure
	if regionListFailure != nil {
		return nil, regionListFailure
	}

	return []providers.Region{
		{Code: "fake-us-east", LongName: "Fake US East"},
		{Code: "fake-us-west", LongName: "Fake US West"},
		{Code: "fake-eu-central", LongName: "Fake EU Central"},
		{Code: "fake-ap-south", LongName: "Fake Asia Pacific South"},
	}, nil
}

// Hostname returns the hostname for a region
func (f *FakeProvider) Hostname(region string) providers.HostName {
	return providers.HostName(fmt.Sprintf("fake-%s", region))
}

// GetRegionHourlyEstimate returns the configured price for any region
func (f *FakeProvider) GetRegionHourlyEstimate(region string) float64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.config.PricePerHour
}

// ListInstances returns all instances in a specific region
func (f *FakeProvider) ListInstances(ctx context.Context, region string) ([]providers.Instance, error) {
	f.mu.RLock()
	statusCheckDelay := f.config.StatusCheckDelay
	statusFailure := f.config.StatusFailure
	f.mu.RUnlock()

	// Simulate list delay
	if statusCheckDelay > 0 {
		select {
		case <-time.After(statusCheckDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	// Check for configured failure
	if statusFailure != nil {
		return nil, statusFailure
	}

	f.mu.RLock()
	defer f.mu.RUnlock()

	var instances []providers.Instance
	for instanceRegion, instance := range f.instances {
		if instanceRegion == region && instance.Status == providers.InstanceStatusRunning {
			instances = append(instances, providers.Instance{
				Hostname:     string(f.Hostname(region)),
				ProviderID:   instance.ID,
				ProviderName: "fake",
				CreatedAt:    instance.CreatedAt,
				HourlyCost:   f.config.PricePerHour,
			})
		}
	}

	return instances, nil
}

// SetInstanceStatus allows tests to control instance status
func (f *FakeProvider) SetInstanceStatus(region string, status providers.InstanceStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if instance, exists := f.instances[region]; exists {
		instance.Status = status
	}
}

func (f *FakeProvider) DebugSSHUser() string { return "root" }

func (f *FakeProvider) GetPublicIP(ctx context.Context, instance providers.Instance) (netip.Addr, error) {
	return netip.MustParseAddr("192.0.2.1"), nil
}

// DeleteInstance removes an instance by InstanceID
func (f *FakeProvider) DeleteInstance(ctx context.Context, instanceID providers.Instance) error {
	// Simulate deletion delay
	if f.config.CreateDelay > 0 {
		select {
		case <-time.After(f.config.CreateDelay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// Check for configured failure
	if f.config.CreateFailure != nil {
		return f.config.CreateFailure
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	// Find and delete the instance by ProviderID
	for region, instance := range f.instances {
		if instance.ID == instanceID.ProviderID {
			delete(f.instances, region)
			return nil
		}
	}

	// Instance not found
	return fmt.Errorf("instance not found: %s", instanceID.ProviderID)
}

// DeleteInstanceByRegion removes an instance by region (for testing)
func (f *FakeProvider) DeleteInstanceByRegion(region string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	delete(f.instances, region)
}

// GetInstance returns instance details (for testing)
func (f *FakeProvider) GetInstance(region string) (*InstanceState, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	instance, exists := f.instances[region]
	return instance, exists
}

// GetAllInstances returns all instances (for testing)
func (f *FakeProvider) GetAllInstances() map[string]*InstanceState {
	f.mu.RLock()
	defer f.mu.RUnlock()

	result := make(map[string]*InstanceState)
	for region, instance := range f.instances {
		result[region] = &InstanceState{
			ID:        instance.ID,
			Region:    instance.Region,
			Status:    instance.Status,
			CreatedAt: instance.CreatedAt,
		}
	}
	return result
}

// UpdateConfig updates the provider configuration (for testing)
func (f *FakeProvider) UpdateConfig(config *ProviderConfig) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.config = config
}

func init() {
	providers.Register("fake", "Fake", New)
}
