package instances

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/services"
	"github.com/anupcshan/tscloudvpn/internal/tsclient"
)

const discoveryInterval = time.Second

// controllerEntry wraps a Controller with the metadata the registry needs
// to populate InstanceStatus without parsing map keys.
type controllerEntry struct {
	controller *Controller
	service    string
	provider   string
	region     string
}

// Registry manages all instance controllers
type Registry struct {
	mu          sync.RWMutex
	controllers map[string]*controllerEntry // key: "service-provider-region"
	logger      *log.Logger
	sshKey      string
	controlApi  controlapi.ControlApi
	tsClient    tsclient.TailscaleClient
	providers   map[string]providers.Provider
}

func registryKey(service, provider, region string) string {
	return fmt.Sprintf("%s-%s-%s", service, provider, region)
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
		controllers: make(map[string]*controllerEntry),
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
func (r *Registry) CreateInstance(ctx context.Context, serviceName, providerName, region string) error {
	key := registryKey(serviceName, providerName, region)

	r.mu.Lock()
	// Check if controller already exists
	if existing, exists := r.controllers[key]; exists {
		status := existing.controller.Status()
		if status.IsRunning {
			r.mu.Unlock()
			r.logger.Printf("Instance %s already running, no action needed", key)
			return nil
		}
		if status.State == StateFailed {
			// Clean up failed controller before creating a new one
			delete(r.controllers, key)
		} else if status.State == StateLaunching {
			// Already launching, nothing to do
			r.mu.Unlock()
			return nil
		}
	}

	// Look up service type
	svcType := services.ByName(serviceName)
	if svcType == nil {
		r.mu.Unlock()
		return fmt.Errorf("unknown service: %s", serviceName)
	}

	// Look up provider
	provider, exists := r.providers[providerName]
	if !exists {
		r.mu.Unlock()
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	hostname := provider.Hostname(region)

	// Create controller for monitoring (does not start health loop yet)
	controller := NewController(context.Background(), r.logger, hostname, svcType, r.tsClient)
	controller.onIdleShutdown = r.makeIdleShutdownCallback(serviceName, providerName, region)
	controller.SetLaunching()
	r.controllers[key] = &controllerEntry{
		controller: controller,
		service:    serviceName,
		provider:   providerName,
		region:     region,
	}
	r.mu.Unlock()

	// Start instance creation in background
	go func() {
		instance, err := r.createCloudInstance(context.Background(), svcType, providerName, provider, region)
		if err != nil {
			r.logger.Printf("Failed to create instance %s: %s", key, err)
			controller.SetFailed(err)
			return
		}
		controller.SetHourlyCost(instance.HourlyCost)

		// Start health monitoring now that the instance is launching
		controller.Start()
	}()

	return nil
}

// createCloudInstance handles the cloud provisioning: create auth key,
// render init script, call provider.
func (r *Registry) createCloudInstance(ctx context.Context, svcType *services.ServiceType, providerName string, provider providers.Provider, region string) (providers.Instance, error) {
	authKey, err := r.controlApi.CreateKey(ctx, svcType.Tags)
	if err != nil {
		return providers.Instance{}, err
	}

	hostname := string(provider.Hostname(region))
	userData, err := providers.RenderUserData(hostname, authKey, r.sshKey, svcType.Name, providerName, region)
	if err != nil {
		return providers.Instance{}, err
	}

	createdInstance, err := provider.CreateInstance(ctx, providers.CreateRequest{
		Region:   region,
		UserData: userData,
		SSHKey:   r.sshKey,
	})
	if err != nil {
		r.logger.Printf("Failed to launch instance %s: %s", hostname, err)
		return providers.Instance{}, err
	}

	r.logger.Printf("Launched instance %s", createdInstance.Hostname)
	return createdInstance, nil
}

// DeleteInstance deletes an instance and its controller
func (r *Registry) DeleteInstance(serviceName, providerName, region string) error {
	key := registryKey(serviceName, providerName, region)

	r.mu.Lock()
	entry, exists := r.controllers[key]
	if !exists {
		r.mu.Unlock()
		return fmt.Errorf("instance controller not found for %s", key)
	}
	delete(r.controllers, key)
	r.mu.Unlock()

	// If the controller failed, no cloud resources to clean up
	if entry.controller.Status().State == StateFailed {
		return nil
	}

	// Look up provider
	provider, exists := r.providers[providerName]
	if !exists {
		entry.controller.Stop()
		return fmt.Errorf("unknown provider: %s", providerName)
	}

	hostname := provider.Hostname(region)

	// Step 1: Delete from Tailscale/Headscale
	r.deleteFromControlPlane(hostname)

	// Step 2: Delete from cloud provider
	r.deleteFromCloudProvider(provider, region, hostname)

	entry.controller.Stop()
	return nil
}

// deleteFromControlPlane removes a device from Tailscale/Headscale.
func (r *Registry) deleteFromControlPlane(hostname providers.HostName) {
	devices, err := r.controlApi.ListDevices(context.Background())
	if err != nil {
		r.logger.Printf("Warning: failed to list devices: %v", err)
		return
	}

	for i, device := range devices {
		if providers.HostName(device.Hostname) == hostname {
			if err := r.controlApi.DeleteDevice(context.Background(), &devices[i]); err != nil {
				r.logger.Printf("Warning: failed to delete device %s from control plane: %v", hostname, err)
			} else {
				r.logger.Printf("Deleted device %s from control plane", hostname)
			}
			return
		}
	}

	r.logger.Printf("Device %s not found in control plane, may have already been deleted", hostname)
}

// deleteFromCloudProvider removes a cloud instance.
func (r *Registry) deleteFromCloudProvider(provider providers.Provider, region string, hostname providers.HostName) {
	instances, err := provider.ListInstances(context.Background(), region)
	if err != nil {
		r.logger.Printf("Warning: failed to list cloud instances: %v", err)
		return
	}

	for _, instance := range instances {
		if instance.Hostname == string(hostname) {
			if err := provider.DeleteInstance(context.Background(), instance); err != nil {
				r.logger.Printf("Warning: failed to delete cloud instance %s: %v (will be cleaned up by GC)", instance.ProviderID, err)
			} else {
				r.logger.Printf("Deleted cloud instance %s", instance.ProviderID)
			}
			return
		}
	}
}

// GetInstanceStatus returns the status of a specific instance
func (r *Registry) GetInstanceStatus(serviceName, providerName, region string) (InstanceStatus, error) {
	key := registryKey(serviceName, providerName, region)

	r.mu.RLock()
	entry, exists := r.controllers[key]
	r.mu.RUnlock()

	if !exists {
		return InstanceStatus{}, fmt.Errorf("instance controller not found for %s", key)
	}

	status := entry.controller.Status()
	status.Service = entry.service
	if svc := services.ByName(entry.service); svc != nil {
		status.ServiceLabel = svc.Label
	}
	status.Provider = entry.provider
	status.Region = entry.region
	return status, nil
}

// GetAllInstanceStatuses returns the status of all instances
func (r *Registry) GetAllInstanceStatuses() map[string]InstanceStatus {
	r.mu.RLock()
	defer r.mu.RUnlock()

	statuses := make(map[string]InstanceStatus)
	for key, entry := range r.controllers {
		status := entry.controller.Status()
		status.Service = entry.service
		if svc := services.ByName(entry.service); svc != nil {
			status.ServiceLabel = svc.Label
		}
		status.Provider = entry.provider
		status.Region = entry.region
		statuses[key] = status
	}
	return statuses
}

// makeIdleShutdownCallback returns a function that deletes the given instance
// when called. It runs the deletion in a separate goroutine to avoid blocking
// the health check loop.
func (r *Registry) makeIdleShutdownCallback(serviceName, providerName, region string) func() {
	return func() {
		go func() {
			if err := r.DeleteInstance(serviceName, providerName, region); err != nil {
				r.logger.Printf("Idle shutdown delete failed for %s: %v", registryKey(serviceName, providerName, region), err)
			}
		}()
	}
}

// makePeerGoneCallback returns a function that stops and removes the controller
// when the peer disappears. Used for discovered controllers only — they have no
// attachment to the VM and should stop watching when the peer is gone.
func (r *Registry) makePeerGoneCallback(serviceName, providerName, region string) func() {
	key := registryKey(serviceName, providerName, region)
	return func() {
		// Run in a goroutine to avoid deadlocking the health check loop
		// (Stop waits for the goroutine that's calling this callback).
		go func() {
			r.mu.Lock()
			entry, exists := r.controllers[key]
			if exists {
				delete(r.controllers, key)
			}
			r.mu.Unlock()

			if exists {
				entry.controller.Stop()
			}
		}()
	}
}

// Shutdown stops all controllers
func (r *Registry) Shutdown() {
	r.mu.Lock()
	entries := make([]*controllerEntry, 0, len(r.controllers))
	for _, entry := range r.controllers {
		entries = append(entries, entry)
	}
	r.controllers = make(map[string]*controllerEntry)
	r.mu.Unlock()

	// Stop all controllers
	for _, entry := range entries {
		entry.controller.Stop()
	}
}

// hasServiceTag returns true if the device has any tag matching a known service type.
func hasServiceTag(device controlapi.Device) bool {
	for _, tag := range device.Tags {
		for _, svc := range services.All {
			for _, svcTag := range svc.Tags {
				if tag == svcTag {
					return true
				}
			}
		}
	}
	return false
}

// discoverInstances finds and registers instances not yet tracked by this registry.
// It filters control API devices by service tags, then fetches the identity
// endpoint from untracked devices to learn their service/provider/region.
func (r *Registry) discoverInstances(ctx context.Context) {
	// Get all devices from control API
	devices, err := r.controlApi.ListDevices(ctx)
	if err != nil {
		r.logger.Printf("Failed to list devices for discovery: %v", err)
		return
	}

	// Build set of hostnames already tracked by controllers
	r.mu.RLock()
	trackedHostnames := make(map[providers.HostName]bool, len(r.controllers))
	for _, entry := range r.controllers {
		trackedHostnames[entry.controller.hostname] = true
	}
	r.mu.RUnlock()

	for _, device := range devices {
		hostname := providers.HostName(device.Hostname)

		// Skip devices we're already tracking
		if trackedHostnames[hostname] {
			continue
		}

		// Skip devices without a known service tag
		if !hasServiceTag(device) {
			continue
		}

		// Fetch identity to learn service/provider/region
		identity, err := r.tsClient.FetchNodeIdentity(ctx, device.Hostname)
		if err != nil {
			// Node not ready yet (still booting) or not a tscloudvpn node.
			// Will retry on next discovery cycle.
			continue
		}

		// Look up service type
		svcType := services.ByName(identity.Service)
		if svcType == nil {
			continue
		}

		// Check that the provider is one we know about
		provider, exists := r.providers[identity.Provider]
		if !exists {
			continue
		}

		key := registryKey(identity.Service, identity.Provider, identity.Region)

		r.mu.Lock()
		// Double-check under write lock
		if _, alreadyExists := r.controllers[key]; !alreadyExists {
			r.logger.Printf("Discovered instance: %s (service=%s, provider=%s, region=%s)",
				hostname, identity.Service, identity.Provider, identity.Region)

			// Look up actual instance cost from the cloud provider
			var hourlyCost float64
			if cloudInstances, err := provider.ListInstances(ctx, identity.Region); err == nil {
				for _, inst := range cloudInstances {
					if inst.Hostname == string(hostname) {
						hourlyCost = inst.HourlyCost
						break
					}
				}
			}

			// Create controller for monitoring only
			controller := NewController(context.Background(), r.logger, hostname, svcType, r.tsClient)
			controller.onIdleShutdown = r.makeIdleShutdownCallback(identity.Service, identity.Provider, identity.Region)
			controller.onPeerGone = r.makePeerGoneCallback(identity.Service, identity.Provider, identity.Region)

			// Mark as running and set creation time and actual cost
			controller.mu.Lock()
			controller.state = StateRunning
			controller.createdAt = device.Created
			controller.hourlyCost = hourlyCost
			controller.mu.Unlock()

			// Start health monitoring
			controller.Start()

			r.controllers[key] = &controllerEntry{
				controller: controller,
				service:    identity.Service,
				provider:   identity.Provider,
				region:     identity.Region,
			}
		}
		r.mu.Unlock()
	}
}
