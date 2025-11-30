package instances

import (
	"context"
	"log"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
)

const (
	// gcInterval is how often the garbage collector runs
	gcInterval = 5 * time.Minute

	// gcGracePeriod is how long to wait before deleting an orphaned instance
	// This prevents deleting instances that are still booting up
	gcGracePeriod = 10 * time.Minute
)

// GarbageCollector periodically finds and deletes orphaned cloud instances
// (instances that exist in the cloud but not in Tailscale/Headscale)
type GarbageCollector struct {
	logger         *log.Logger
	controlApi     controlapi.ControlApi
	providers      map[string]providers.Provider
	enableDeletion bool // If false, only log what would be deleted (dry-run mode)
}

// NewGarbageCollector creates a new garbage collector
func NewGarbageCollector(
	logger *log.Logger,
	controlApi controlapi.ControlApi,
	providers map[string]providers.Provider,
	enableDeletion bool,
) *GarbageCollector {
	return &GarbageCollector{
		logger:         logger,
		controlApi:     controlApi,
		providers:      providers,
		enableDeletion: enableDeletion,
	}
}

// Run starts the garbage collector loop
func (gc *GarbageCollector) Run(ctx context.Context) {
	mode := "DRY-RUN"
	if gc.enableDeletion {
		mode = "ENABLED"
	}
	gc.logger.Printf("Starting garbage collector (interval: %v, grace period: %v, deletion: %s)", gcInterval, gcGracePeriod, mode)

	// Run immediately on startup
	gc.collect(ctx)

	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			gc.logger.Printf("Garbage collector stopped")
			return
		case <-ticker.C:
			gc.collect(ctx)
		}
	}
}

// collect performs a single garbage collection cycle
func (gc *GarbageCollector) collect(ctx context.Context) {
	gc.logger.Printf("Running garbage collection...")

	// Step 1: Get all devices from Tailscale/Headscale
	devices, err := gc.controlApi.ListDevices(ctx)
	if err != nil {
		gc.logger.Printf("GC: failed to list devices: %v", err)
		return
	}

	// Build a set of hostnames that exist in Tailscale
	tailscaleHostnames := make(map[string]bool)
	for _, device := range devices {
		tailscaleHostnames[device.Hostname] = true
	}

	gc.logger.Printf("GC: found %d devices in control plane", len(tailscaleHostnames))

	// Step 2: Check each provider/region for orphaned instances
	totalOrphaned := 0
	totalDeleted := 0

	for providerName, provider := range gc.providers {
		regions, err := provider.ListRegions(ctx)
		if err != nil {
			gc.logger.Printf("GC: failed to list regions for %s: %v", providerName, err)
			continue
		}

		for _, region := range regions {
			instances, err := provider.ListInstances(ctx, region.Code)
			if err != nil {
				gc.logger.Printf("GC: failed to list instances for %s/%s: %v", providerName, region.Code, err)
				continue
			}

			for _, instance := range instances {
				// Check if this instance exists in Tailscale
				if tailscaleHostnames[instance.Hostname] {
					// Instance exists in both cloud and Tailscale - all good
					continue
				}

				// Instance is orphaned (exists in cloud but not in Tailscale)
				totalOrphaned++

				// Check grace period - don't delete young instances that might still be booting
				instanceAge := time.Since(instance.CreatedAt)
				if instanceAge < gcGracePeriod {
					gc.logger.Printf("GC: skipping young orphaned instance %s/%s (age: %v, grace period: %v)",
						providerName, instance.Hostname, instanceAge.Round(time.Second), gcGracePeriod)
					continue
				}

				// Delete the orphaned instance (or log in dry-run mode)
				if !gc.enableDeletion {
					gc.logger.Printf("GC: [DRY-RUN] would delete orphaned instance %s/%s (provider ID: %s, age: %v)",
						providerName, instance.Hostname, instance.ProviderID, instanceAge.Round(time.Second))
					continue
				}

				gc.logger.Printf("GC: deleting orphaned instance %s/%s (provider ID: %s, age: %v)",
					providerName, instance.Hostname, instance.ProviderID, instanceAge.Round(time.Second))

				if err := provider.DeleteInstance(ctx, instance); err != nil {
					gc.logger.Printf("GC: failed to delete instance %s/%s: %v", providerName, instance.Hostname, err)
				} else {
					totalDeleted++
					gc.logger.Printf("GC: successfully deleted orphaned instance %s/%s", providerName, instance.Hostname)
				}
			}
		}
	}

	if totalOrphaned > 0 {
		gc.logger.Printf("GC: found %d orphaned instances, deleted %d", totalOrphaned, totalDeleted)
	} else {
		gc.logger.Printf("GC: no orphaned instances found")
	}
}
