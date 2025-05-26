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
	return &Registry{
		controllers: make(map[string]*Controller),
		logger:      logger,
		controlApi:  controlApi,
		tsClient:    tsClient,
		providers:   providers,
	}
}

// CreateInstance creates a new instance with its controller
func (r *Registry) CreateInstance(ctx context.Context, providerName, region string) error {
	key := fmt.Sprintf("%s-%s", providerName, region)

	r.mu.Lock()
	defer r.mu.Unlock()

	// Check if controller already exists
	if _, exists := r.controllers[key]; exists {
		return fmt.Errorf("instance controller already exists for %s", key)
	}

	provider, exists := r.providers[providerName]
	if !exists {
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	// Create controller
	controller := NewController(ctx, r.logger, provider, region, r.controlApi, r.tsClient)
	r.controllers[key] = controller

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
