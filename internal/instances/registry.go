package instances

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/tsclient"
)

const discoveryInterval = time.Second

// Registry manages all instance controllers
type Registry struct {
	mu          sync.RWMutex
	controllers map[string]*Controller // key: "provider-region"
	logger      *log.Logger
	sshKey      string
	controlApi  controlapi.ControlApi
	tsClient    tsclient.TailscaleClient
	providers   map[string]providers.Provider
}

// NewRegistry creates a new instance registry. Call Start() to begin
// discovery of existing instances and garbage collection.
func NewRegistry(
	logger *log.Logger,
	sshKey string,
	controlApi controlapi.ControlApi,
	tsClient tsclient.TailscaleClient,
	providers map[string]providers.Provider,
) *Registry {
	return &Registry{
		controllers: make(map[string]*Controller),
		logger:      logger,
		sshKey:      sshKey,
		controlApi:  controlApi,
		tsClient:    tsClient,
		providers:   providers,
	}
}

// Start begins background discovery of instances and garbage collection.
func (r *Registry) Start(ctx context.Context) {
	go r.runDiscoveryLoop(ctx)

	gc := NewGarbageCollector(r.logger, r.controlApi, r.providers)
	go gc.Run(ctx)
}

// runDiscoveryLoop periodically discovers instances created by other
// tscloudvpn instances on the same tailnet.
func (r *Registry) runDiscoveryLoop(ctx context.Context) {
	r.discoverInstances(ctx)

	ticker := time.NewTicker(discoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.discoverInstances(ctx)
		}
	}
}

// CreateInstance creates a new instance with its controller
func (r *Registry) CreateInstance(ctx context.Context, providerName, region string) error {
	key := fmt.Sprintf("%s-%s", providerName, region)

	r.mu.Lock()
	// Check if controller already exists
	if existing, exists := r.controllers[key]; exists {
		status := existing.Status()
		if status.IsRunning {
			r.mu.Unlock()
			r.logger.Printf("Instance %s already running, no action needed", key)
			return nil
		}
		if status.State == StateFailed {
			// Clean up failed controller before creating a new one
			delete(r.controllers, key)
		} else {
			// Controller exists but instance isn't running, proceed with creation
			controller := r.controllers[key]
			r.mu.Unlock()
			go func() {
				if err := controller.Create(); err != nil {
					r.logger.Printf("Failed to create instance %s: %s", key, err)
					controller.SetFailed(err)
				}
			}()
			return nil
		}
	}

	// Create new controller
	provider, exists := r.providers[providerName]
	if !exists {
		r.mu.Unlock()
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	controller := NewController(context.Background(), r.logger, provider, region, r.sshKey, r.controlApi, r.tsClient)
	controller.onIdleShutdown = r.makeIdleShutdownCallback(providerName, region)
	r.controllers[key] = controller
	r.mu.Unlock()

	// Start instance creation
	go func() {
		if err := controller.Create(); err != nil {
			r.logger.Printf("Failed to create instance %s: %s", key, err)
			controller.SetFailed(err)
		}
	}()

	return nil
}

// DeleteInstance deletes an instance and its controller
func (r *Registry) DeleteInstance(providerName, region string) error {
	key := fmt.Sprintf("%s-%s", providerName, region)

	r.mu.Lock()
	controller, exists := r.controllers[key]
	if !exists {
		r.mu.Unlock()
		return fmt.Errorf("instance controller not found for %s", key)
	}
	delete(r.controllers, key)
	r.mu.Unlock()

	// If the controller failed, no cloud resources to clean up
	if controller.Status().State == StateFailed {
		return nil
	}

	// Delete the instance
	err := controller.Delete()
	controller.Stop()

	return err
}

// GetInstanceStatus returns the status of a specific instance
func (r *Registry) GetInstanceStatus(providerName, region string) (InstanceStatus, error) {
	key := fmt.Sprintf("%s-%s", providerName, region)

	r.mu.RLock()
	controller, exists := r.controllers[key]
	r.mu.RUnlock()

	if !exists {
		return InstanceStatus{}, fmt.Errorf("instance controller not found for %s", key)
	}

	status := controller.Status()
	status.Provider = providerName
	return status, nil
}

// GetAllInstanceStatuses returns the status of all instances
func (r *Registry) GetAllInstanceStatuses() map[string]InstanceStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	statuses := make(map[string]InstanceStatus)
	for key, controller := range r.controllers {
		status := controller.Status()
		// Extract provider name from key
		for providerName := range r.providers {
			if len(key) > len(providerName) && key[:len(providerName)] == providerName {
				status.Provider = providerName
				break
			}
		}
		statuses[key] = status
	}
	return statuses
}

// makeIdleShutdownCallback returns a function that deletes the given instance
// when called. It runs the deletion in a separate goroutine to avoid blocking
// the health check loop.
func (r *Registry) makeIdleShutdownCallback(providerName, region string) func() {
	return func() {
		go func() {
			if err := r.DeleteInstance(providerName, region); err != nil {
				r.logger.Printf("Idle shutdown delete failed for %s-%s: %v", providerName, region, err)
			}
		}()
	}
}

// makePeerGoneCallback returns a function that stops and removes the controller
// when the peer disappears. Used for discovered controllers only — they have no
// attachment to the VM and should stop watching when the peer is gone.
func (r *Registry) makePeerGoneCallback(providerName, region string) func() {
	key := fmt.Sprintf("%s-%s", providerName, region)
	return func() {
		// Run in a goroutine to avoid deadlocking the health check loop
		// (Stop waits for the goroutine that's calling this callback).
		go func() {
			r.mu.Lock()
			controller, exists := r.controllers[key]
			if exists {
				delete(r.controllers, key)
			}
			r.mu.Unlock()

			if exists {
				controller.Stop()
			}
		}()
	}
}

// Shutdown stops all controllers
func (r *Registry) Shutdown() {
	r.mu.Lock()
	controllers := make([]*Controller, 0, len(r.controllers))
	for _, controller := range r.controllers {
		controllers = append(controllers, controller)
	}
	r.controllers = make(map[string]*Controller)
	r.mu.Unlock()

	// Stop all controllers
	for _, controller := range controllers {
		controller.Stop()
	}
}

// discoverInstances finds and registers instances not yet tracked by this registry.
func (r *Registry) discoverInstances(ctx context.Context) {
	// Get all devices from control API
	devices, err := r.controlApi.ListDevices(ctx)
	if err != nil {
		r.logger.Printf("Failed to list devices for discovery: %v", err)
		return
	}

	// Create hostname to device mapping
	deviceMap := make(map[providers.HostName]controlapi.Device)
	for _, device := range devices {
		deviceMap[providers.HostName(device.Hostname)] = device
	}

	// Check each provider-region combination
	for providerName, provider := range r.providers {
		regions, err := provider.ListRegions(ctx)
		if err != nil {
			r.logger.Printf("Failed to list regions for provider %s: %v", providerName, err)
			continue
		}

		for _, region := range regions {
			hostname := provider.Hostname(region.Code)
			key := fmt.Sprintf("%s-%s", providerName, region.Code)

			// Check if device exists for this hostname
			if device, exists := deviceMap[hostname]; exists {
				r.mu.Lock()
				// Only create controller if one doesn't already exist
				if _, alreadyExists := r.controllers[key]; !alreadyExists {
					r.logger.Printf("Discovered instance: %s", hostname)

					// Look up actual instance cost from the cloud provider
					var hourlyCost float64
					if cloudInstances, err := provider.ListInstances(ctx, region.Code); err == nil {
						for _, inst := range cloudInstances {
							if inst.Hostname == string(hostname) {
								hourlyCost = inst.HourlyCost
								break
							}
						}
					}

					// Create controller for existing instance with background context
					controller := NewController(context.Background(), r.logger, provider, region.Code, r.sshKey, r.controlApi, r.tsClient)
					controller.onIdleShutdown = r.makeIdleShutdownCallback(providerName, region.Code)
					controller.onPeerGone = r.makePeerGoneCallback(providerName, region.Code)

					// Mark as running and set creation time and actual cost
					controller.mu.Lock()
					controller.state = StateRunning
					controller.createdAt = device.Created
					controller.hourlyCost = hourlyCost
					controller.mu.Unlock()

					r.controllers[key] = controller
				}
				r.mu.Unlock()
			}
		}
	}
}
