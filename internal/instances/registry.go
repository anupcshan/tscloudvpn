package instances

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"tailscale.com/client/local"
)

// Registry manages all instance controllers
type Registry struct {
	mu          sync.RWMutex
	controllers map[string]*Controller // key: "provider-region"
	logger      *log.Logger
	controlApi  controlapi.ControlApi
	tsClient    *local.Client
	providers   map[string]providers.Provider
}

// NewRegistry creates a new instance registry
func NewRegistry(
	logger *log.Logger,
	controlApi controlapi.ControlApi,
	tsClient *local.Client,
	providers map[string]providers.Provider,
) *Registry {
	r := &Registry{
		controllers: make(map[string]*Controller),
		logger:      logger,
		controlApi:  controlApi,
		tsClient:    tsClient,
		providers:   providers,
	}

	// Discover existing instances on startup
	go r.discoverExistingInstances(context.Background())

	// Start garbage collector to clean up orphaned cloud instances
	gc := NewGarbageCollector(logger, controlApi, providers)
	go gc.Run(context.Background())

	return r
}

// CreateInstance creates a new instance with its controller
func (r *Registry) CreateInstance(ctx context.Context, providerName, region string) error {
	key := fmt.Sprintf("%s-%s", providerName, region)

	r.mu.Lock()
	// Check if controller already exists
	if controller, exists := r.controllers[key]; exists {
		r.mu.Unlock()
		// If instance already exists and is running, just return success
		status := controller.Status()
		if status.IsRunning {
			r.logger.Printf("Instance %s already running, no action needed", key)
			return nil
		}
		// If controller exists but instance isn't running, proceed with creation
		r.mu.Lock()
	} else {
		// Create new controller if it doesn't exist
		provider, exists := r.providers[providerName]
		if !exists {
			r.mu.Unlock()
			return fmt.Errorf("unknown provider: %s", providerName)
		}

		// Create controller with a background context that won't be canceled
		// when the HTTP request ends
		controller := NewController(context.Background(), r.logger, provider, region, r.controlApi, r.tsClient)
		r.controllers[key] = controller
	}
	controller := r.controllers[key]
	r.mu.Unlock()

	// Start instance creation
	go func() {
		if err := controller.Create(); err != nil {
			r.logger.Printf("Failed to create instance %s: %s", key, err)
			// Remove failed controller
			r.mu.Lock()
			delete(r.controllers, key)
			r.mu.Unlock()
			controller.Stop()
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

// discoverExistingInstances finds and registers existing instances on startup
func (r *Registry) discoverExistingInstances(ctx context.Context) {
	r.logger.Printf("Discovering existing instances...")

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
	discoveredCount := 0
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
					r.logger.Printf("Discovered existing instance: %s", hostname)

					// Create controller for existing instance with background context
					controller := NewController(context.Background(), r.logger, provider, region.Code, r.controlApi, r.tsClient)

					// Mark as running and set creation time
					controller.mu.Lock()
					controller.state = StateRunning
					controller.createdAt = device.Created
					controller.mu.Unlock()

					r.controllers[key] = controller
					discoveredCount++
				}
				r.mu.Unlock()
			}
		}
	}

	r.logger.Printf("Instance discovery completed. Found %d existing instances.", discoveredCount)
}
