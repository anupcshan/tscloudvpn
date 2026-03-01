package instances

import (
	"context"
	"log"
	"math"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/providers"
	"github.com/anupcshan/tscloudvpn/internal/services"
	"github.com/anupcshan/tscloudvpn/internal/tsclient"
)

const (
	historySize         = 250
	healthCheckInterval = time.Second
	statsFetchInterval  = 30 // fetch stats every N health check ticks
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

// NodeStats contains traffic statistics fetched from an exit node.
type NodeStats struct {
	ForwardedBytes int64     // Bytes forwarded through the exit node (from iptables ts-forward chain)
	LastActive     time.Time // When IP-forwarded traffic was last observed
	ReceivedAt     time.Time // When the control plane received this report
}

// InstanceState represents the lifecycle state of an instance
type InstanceState int

const (
	StateIdle      InstanceState = iota // No activity or instance gone
	StateLaunching                      // Create() called, cloud API in progress
	StateRunning                        // Instance is up, peer visible in Tailscale
	StateFailed                         // Create() failed
)

// InstanceStatus represents the current state of an instance
type InstanceStatus struct {
	Hostname   providers.HostName
	Provider   string
	Region     string
	State      InstanceState
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
	NodeStats  *NodeStats // nil if node has never reported stats
	LastError  string     // error message if State == StateFailed
	HourlyCost float64    // actual hourly cost of the instance
}

// statsWatchdogThreshold is how long the control plane waits for a node to
// report stats before treating it as idle. This is measured from when the
// control plane started watching the node (not the node's age), so restarts
// give every node a fresh window.
const statsWatchdogThreshold = 8 * time.Hour

// Controller monitors the health of a single instance. It does not create
// or delete instances — that is handled by the Registry.
type Controller struct {
	mu                    sync.RWMutex
	ctx                   context.Context
	cancel                context.CancelFunc
	logger                *log.Logger
	hostname              providers.HostName
	serviceType           *services.ServiceType
	tsClient              tsclient.TailscaleClient
	ping                  *PingHistory
	launchedAt            time.Time
	createdAt             time.Time
	state                 InstanceState
	done                  chan struct{}
	started               bool // whether Start() was called
	healthCheckTicker     *time.Ticker
	nodeStats             *NodeStats // latest stats fetched from the node; nil if never fetched
	watchingSince         time.Time  // when we started monitoring this node (for stats watchdog)
	onIdleShutdown        func()     // callback when idle shutdown is triggered
	idleShutdownTriggered bool       // prevents calling onIdleShutdown more than once
	onPeerGone            func()     // callback when peer disappears (discovered controllers only)
	healthCheckCount      int        // counter for throttling stats fetches
	lastError             string     // error message when state is StateFailed
	hourlyCost            float64    // actual hourly cost of the instance
}

// NewController creates a new instance controller that monitors health.
// It does not start the health check loop — call Start() to begin monitoring.
func NewController(
	ctx context.Context,
	logger *log.Logger,
	hostname providers.HostName,
	serviceType *services.ServiceType,
	tsClient tsclient.TailscaleClient,
) *Controller {
	ctx, cancel := context.WithCancel(ctx)

	return &Controller{
		ctx:           ctx,
		cancel:        cancel,
		logger:        logger,
		hostname:      hostname,
		serviceType:   serviceType,
		tsClient:      tsClient,
		ping:          NewPingHistory(),
		done:          make(chan struct{}),
		watchingSince: time.Now(),
	}
}

// Start begins the health monitoring loop in a background goroutine.
func (c *Controller) Start() {
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
	go c.monitorHealth()
}

// SetLaunching transitions the controller to launching state.
func (c *Controller) SetLaunching() {
	c.mu.Lock()
	c.state = StateLaunching
	c.launchedAt = time.Now()
	c.mu.Unlock()
}

// SetHourlyCost records the actual hourly cost of the instance.
func (c *Controller) SetHourlyCost(cost float64) {
	c.mu.Lock()
	c.hourlyCost = cost
	c.mu.Unlock()
}

// Status returns the current status of the instance
func (c *Controller) Status() InstanceStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()

	status := InstanceStatus{
		Hostname:   c.hostname,
		Provider:   "", // Provider name will be set by caller
		Region:     "",  // Will be set by caller
		State:      c.state,
		IsRunning:  c.state == StateRunning,
		CreatedAt:  c.createdAt,
		LaunchedAt: c.launchedAt,
		LastError:  c.lastError,
		HourlyCost: c.hourlyCost,
	}

	status.PingStats.SuccessRate, status.PingStats.AvgLatency, status.PingStats.StdDev, status.PingStats.TimeSinceFailure, status.PingStats.ConnectionType = c.ping.GetStats()
	status.NodeStats = c.nodeStats

	return status
}

// fetchStats fetches traffic stats from the node's HTTP stats endpoint.
func (c *Controller) fetchStats(hostname providers.HostName) {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()

	result, err := c.tsClient.FetchNodeStats(ctx, string(hostname))
	if err != nil {
		c.logger.Printf("Stats fetch failed for %s: %v", hostname, err)
		return
	}

	stats := &NodeStats{
		ForwardedBytes: result.ForwardedBytes,
		LastActive:     result.LastActive,
		ReceivedAt:     time.Now(),
	}

	c.mu.Lock()
	c.nodeStats = stats
	c.mu.Unlock()
}

// shouldIdleShutdown returns true if the node should be terminated due to idleness.
func (c *Controller) shouldIdleShutdown() bool {
	// Only consider idle shutdown for running nodes
	if c.state != StateRunning {
		return false
	}

	// No idle shutdown if the service type doesn't define a timeout
	if c.serviceType.IdleTimeout == 0 {
		return false
	}

	if c.nodeStats != nil {
		// Stats received: idle if no forwarded traffic for threshold duration
		return time.Since(c.nodeStats.LastActive) > c.serviceType.IdleTimeout
	}

	// No stats ever received: watchdog timer
	return time.Since(c.watchingSince) > statsWatchdogThreshold
}

// SetFailed transitions the controller to the failed state with an error message
// and stops the health check goroutine.
func (c *Controller) SetFailed(err error) {
	c.mu.Lock()
	c.state = StateFailed
	c.lastError = err.Error()
	c.mu.Unlock()
	c.Stop()
}

// Stop stops the controller and cleans up resources.
// Safe to call even if Start() was never called.
func (c *Controller) Stop() {
	c.cancel()
	c.mu.RLock()
	started := c.started
	c.mu.RUnlock()
	if started {
		<-c.done
	}
}

// monitorHealth runs the health check loop for this instance
func (c *Controller) monitorHealth() {
	defer close(c.done)

	c.healthCheckTicker = time.NewTicker(healthCheckInterval)
	defer c.healthCheckTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.healthCheckTicker.C:
			c.performHealthCheck(c.hostname)
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

	peers, err := c.tsClient.GetPeers(ctx)
	if err != nil {
		c.logger.Printf("Error getting Tailscale peers for %s: %s", hostname, err)
		return
	}

	// Find our peer
	var peer *tsclient.PeerInfo
	for i, p := range peers {
		if providers.HostName(p.Hostname) == hostname {
			peer = &peers[i]
			break
		}
	}

	if peer == nil {
		// Instance not registered yet or removed
		c.mu.Lock()
		wasRunning := c.state == StateRunning
		if wasRunning {
			c.state = StateIdle
			c.launchedAt = time.Time{}
		}
		c.mu.Unlock()

		// For discovered controllers: peer gone means instance is gone,
		// stop watching and let discovery create a fresh controller if it returns.
		if wasRunning && c.onPeerGone != nil {
			c.logger.Printf("Peer gone for %s, stopping controller", hostname)
			c.onPeerGone()
		}
		return
	}

	c.mu.Lock()
	if c.state != StateRunning {
		c.state = StateRunning
		c.createdAt = peer.Created
	}
	c.mu.Unlock()

	// Perform ping
	result, err := c.tsClient.PingPeer(ctx, peer.TailscaleIPs[0])
	if err != nil {
		c.logger.Printf("Ping error from %s (%s): %s", peer.Hostname, peer.TailscaleIPs[0], err)
		c.ping.AddResult(false, 0, "")
	} else {
		c.ping.AddResult(true, result.Latency, result.ConnectionType)
	}

	// Fetch stats from the node periodically (not every health check tick)
	if c.healthCheckCount%statsFetchInterval == 0 {
		c.fetchStats(hostname)
	}
	c.healthCheckCount++

	// Check for idle shutdown (only trigger once)
	c.mu.RLock()
	shouldShutdown := c.shouldIdleShutdown() && !c.idleShutdownTriggered
	c.mu.RUnlock()

	if shouldShutdown && c.onIdleShutdown != nil {
		c.mu.Lock()
		c.idleShutdownTriggered = true
		c.mu.Unlock()
		c.logger.Printf("Idle shutdown triggered for %s", hostname)
		c.onIdleShutdown()
	}
}
