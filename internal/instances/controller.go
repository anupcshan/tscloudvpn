package instances

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/anupcshan/tscloudvpn/internal/providers"
	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tailcfg"
)

const (
	historySize         = 250
	healthCheckInterval = time.Second
)

// PingResult represents a single ping attempt result
type PingResult struct {
	Timestamp      time.Time
	Success        bool
	Latency        time.Duration
	ConnectionType string
}

// PingHistory tracks ping results for health monitoring
type PingHistory struct {
	mu                    sync.RWMutex
	history               []PingResult // Ring buffer of last historySize results
	successCount          int
	totalLatency          time.Duration
	lastFailure           time.Time
	position              int     // Current position in ring buffer
	totalLatencySquaredNs float64 // Use float64 to avoid overflow with large latencies
}

// NewPingHistory creates a new ping history tracker
func NewPingHistory() *PingHistory {
	return NewPingHistoryWithSize(historySize)
}

// NewPingHistoryWithSize creates a new ping history tracker with a custom buffer size
func NewPingHistoryWithSize(size int) *PingHistory {
	return &PingHistory{
		history: make([]PingResult, size),
	}
}

// AddResult adds a ping result to the history
func (ph *PingHistory) AddResult(success bool, latency time.Duration, connectionType string) {
	ph.mu.Lock()
	defer ph.mu.Unlock()

	// If the entry at current position was successful, remove its contribution
	if ph.history[ph.position].Success {
		ph.successCount--
		ph.totalLatency -= ph.history[ph.position].Latency
		latencyNs := float64(ph.history[ph.position].Latency)
		ph.totalLatencySquaredNs -= latencyNs * latencyNs
	}

	// Add new result
	ph.history[ph.position] = PingResult{
		Timestamp:      time.Now(),
		Success:        success,
		Latency:        latency,
		ConnectionType: connectionType,
	}

	if success {
		ph.successCount++
		ph.totalLatency += latency
		latencyNs := float64(latency)
		ph.totalLatencySquaredNs += latencyNs * latencyNs
	} else {
		ph.lastFailure = time.Now()
	}

	// Move position forward
	ph.position = (ph.position + 1) % len(ph.history)
}

// GetStats returns current ping statistics
func (ph *PingHistory) GetStats() (successRate float64, avgLatency time.Duration, stddev time.Duration, timeSinceFailure time.Duration, connectionType string) {
	ph.mu.RLock()
	defer ph.mu.RUnlock()

	// Count non-zero entries to handle startup period
	total := 0
	for _, result := range ph.history {
		if !result.Timestamp.IsZero() {
			total++
		}
	}

	if total == 0 {
		return 0, 0, 0, 0, ""
	}

	successRate = float64(ph.successCount) / float64(total)
	if ph.successCount > 0 {
		avgLatency = ph.totalLatency / time.Duration(ph.successCount)

		// Calculate standard deviation: stddev = sqrt(E[X²] - E[X]²)
		if ph.successCount > 1 {
			meanNs := float64(ph.totalLatency) / float64(ph.successCount)
			meanOfSquares := ph.totalLatencySquaredNs / float64(ph.successCount)
			variance := meanOfSquares - (meanNs * meanNs)
			if variance > 0 {
				stddev = time.Duration(math.Sqrt(variance))
			}
		}
	}

	if !ph.lastFailure.IsZero() {
		timeSinceFailure = time.Since(ph.lastFailure)
	}

	// Return most recent connection type
	for i := len(ph.history) - 1; i >= 0; i-- {
		idx := (ph.position - 1 - i + len(ph.history)) % len(ph.history)
		result := ph.history[idx]
		if !result.Timestamp.IsZero() && result.Success {
			connectionType = result.ConnectionType
			break
		}
	}

	return successRate, avgLatency, stddev, timeSinceFailure, connectionType
}

// InstanceStatus represents the current state of an instance
type InstanceStatus struct {
	Hostname   providers.HostName
	Provider   string
	Region     string
	IsRunning  bool
	CreatedAt  time.Time
	LaunchedAt time.Time
	PingStats  struct {
		SuccessRate      float64
		AvgLatency       time.Duration
		StdDev           time.Duration
		TimeSinceFailure time.Duration
		ConnectionType   string
	}
}

// Controller manages the entire lifecycle of a single instance
type Controller struct {
	mu                sync.RWMutex
	ctx               context.Context
	cancel            context.CancelFunc
	logger            *log.Logger
	provider          providers.Provider
	region            string
	controlApi        controlapi.ControlApi
	tsClient          *local.Client
	ping              *PingHistory
	launchedAt        time.Time
	createdAt         time.Time
	isRunning         bool
	done              chan struct{}
	healthCheckTicker *time.Ticker
}

// NewController creates a new instance controller
func NewController(
	ctx context.Context,
	logger *log.Logger,
	provider providers.Provider,
	region string,
	controlApi controlapi.ControlApi,
	tsClient *local.Client,
) *Controller {
	ctx, cancel := context.WithCancel(ctx)

	c := &Controller{
		ctx:        ctx,
		cancel:     cancel,
		logger:     logger,
		provider:   provider,
		region:     region,
		controlApi: controlApi,
		tsClient:   tsClient,
		ping:       NewPingHistory(),
		done:       make(chan struct{}),
	}

	// Start health monitoring in background
	go c.monitorHealth()

	return c
}

// Create creates and configures the instance
func (c *Controller) Create() error {
	c.mu.Lock()
	// Check if instance is already running (discovered on startup)
	if c.isRunning {
		c.mu.Unlock()
		c.logger.Printf("Instance %s is already running, skipping creation", c.provider.Hostname(c.region))
		return nil
	}
	c.launchedAt = time.Now()
	c.mu.Unlock()

	creator := NewCreator()
	err := creator.Create(c.ctx, c.logger, c.controlApi, c.provider, c.region)
	if err != nil {
		c.mu.Lock()
		c.launchedAt = time.Time{}
		c.mu.Unlock()
		return err
	}

	c.mu.Lock()
	c.isRunning = true
	c.mu.Unlock()

	return nil
}

// Delete removes the instance from both Tailscale and the cloud provider
func (c *Controller) Delete() error {
	hostname := c.provider.Hostname(c.region)

	// Step 1: Delete from Tailscale/Headscale
	devices, err := c.controlApi.ListDevices(c.ctx)
	if err != nil {
		return fmt.Errorf("failed to list devices: %w", err)
	}

	tailscaleDeleted := false
	for i, device := range devices {
		if providers.HostName(device.Hostname) == hostname {
			if err := c.controlApi.DeleteDevice(c.ctx, &devices[i]); err != nil {
				return fmt.Errorf("failed to delete device from control plane: %w", err)
			}
			tailscaleDeleted = true
			c.logger.Printf("Deleted device %s from control plane", hostname)
			break
		}
	}

	if !tailscaleDeleted {
		c.logger.Printf("Device %s not found in control plane, may have already been deleted", hostname)
	}

	// Step 2: Delete from cloud provider
	instances, err := c.provider.ListInstances(c.ctx, c.region)
	if err != nil {
		c.logger.Printf("Warning: failed to list cloud instances: %v", err)
		// Don't fail here - the device is already removed from Tailscale
		// The GC will clean up orphaned cloud instances
	} else {
		for _, instance := range instances {
			if instance.Hostname == string(hostname) {
				if err := c.provider.DeleteInstance(c.ctx, instance); err != nil {
					c.logger.Printf("Warning: failed to delete cloud instance %s: %v (will be cleaned up by GC)", instance.ProviderID, err)
					// Don't return error - the device is already removed from Tailscale
					// The GC will clean up orphaned cloud instances
				} else {
					c.logger.Printf("Deleted cloud instance %s", instance.ProviderID)
				}
				break
			}
		}
	}

	c.mu.Lock()
	c.isRunning = false
	c.mu.Unlock()

	return nil
}

// Status returns the current status of the instance
func (c *Controller) Status() InstanceStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := InstanceStatus{
		Hostname:   c.provider.Hostname(c.region),
		Provider:   "", // Provider name will be set by caller
		Region:     c.region,
		IsRunning:  c.isRunning,
		CreatedAt:  c.createdAt,
		LaunchedAt: c.launchedAt,
	}

	status.PingStats.SuccessRate, status.PingStats.AvgLatency, status.PingStats.StdDev, status.PingStats.TimeSinceFailure, status.PingStats.ConnectionType = c.ping.GetStats()

	return status
}

// Stop stops the controller and cleans up resources
func (c *Controller) Stop() {
	c.cancel()
	<-c.done
}

// monitorHealth runs the health check loop for this instance
func (c *Controller) monitorHealth() {
	defer close(c.done)

	c.healthCheckTicker = time.NewTicker(healthCheckInterval)
	defer c.healthCheckTicker.Stop()

	hostname := c.provider.Hostname(c.region)

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.healthCheckTicker.C:
			c.performHealthCheck(hostname)
		}
	}
}

// performHealthCheck performs a single health check
func (c *Controller) performHealthCheck(hostname providers.HostName) {
	// Skip health check if tsClient is nil (testing)
	if c.tsClient == nil {
		return
	}

	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()

	// Get Tailscale status to find our peer
	tsStatus, err := c.tsClient.Status(ctx)
	if err != nil {
		c.logger.Printf("Error getting Tailscale status for %s: %s", hostname, err)
		return
	}

	// Find our peer
	var peer *ipnstate.PeerStatus
	for _, p := range tsStatus.Peer {
		if providers.HostName(p.HostName) == hostname {
			peer = p
			break
		}
	}

	if peer == nil {
		// Instance not registered yet or removed
		c.mu.Lock()
		if c.isRunning {
			// Instance was running but peer disappeared - clear launchedAt
			// so the UI doesn't show a stale "Launched instance xx ago" message.
			c.launchedAt = time.Time{}
		}
		c.isRunning = false
		c.mu.Unlock()
		return
	}

	c.mu.Lock()
	if !c.isRunning {
		c.isRunning = true
		c.createdAt = peer.Created
	}
	c.mu.Unlock()

	// Perform ping
	result, err := c.tsClient.Ping(ctx, peer.TailscaleIPs[0], tailcfg.PingDisco)
	if err != nil {
		c.logger.Printf("Ping error from %s (%s): %s", peer.HostName, peer.TailscaleIPs[0], err)
		c.ping.AddResult(false, 0, "")
	} else {
		latency := time.Duration(result.LatencySeconds*1000000) * time.Microsecond
		connectionType := "direct"
		if result.Endpoint == "" && result.DERPRegionCode != "" {
			connectionType = "relayed via " + result.DERPRegionCode
		}
		c.ping.AddResult(true, latency, connectionType)
	}
}
