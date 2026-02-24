package tsclient

import (
	"context"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

// MockClient is a test implementation of TailscaleClient with controllable peer state.
type MockClient struct {
	mu          sync.RWMutex
	peers       map[string]PeerInfo
	pingLatency time.Duration
	nodeStats   *NodeStatsResult // if set, FetchNodeStats returns this
}

// NewMockClient creates a MockClient with no peers and 10ms default ping latency.
func NewMockClient() *MockClient {
	return &MockClient{
		peers:       make(map[string]PeerInfo),
		pingLatency: 10 * time.Millisecond,
	}
}

// AddPeer adds a peer visible in the mock Tailscale network.
func (m *MockClient) AddPeer(hostname string, ip netip.Addr) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.peers[hostname] = PeerInfo{
		Hostname:     hostname,
		Created:      time.Now(),
		TailscaleIPs: []netip.Addr{ip},
	}
}

// RemovePeer removes a peer from the mock Tailscale network.
func (m *MockClient) RemovePeer(hostname string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.peers, hostname)
}

// SetPingLatency sets the latency returned by PingPeer.
func (m *MockClient) SetPingLatency(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pingLatency = d
}

func (m *MockClient) GetPeers(ctx context.Context) ([]PeerInfo, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	peers := make([]PeerInfo, 0, len(m.peers))
	for _, p := range m.peers {
		peers = append(peers, p)
	}
	return peers, nil
}

func (m *MockClient) PingPeer(ctx context.Context, addr netip.Addr) (PingResult, error) {
	m.mu.RLock()
	latency := m.pingLatency
	m.mu.RUnlock()

	return PingResult{
		Latency:        latency,
		ConnectionType: "direct",
	}, nil
}

// SetNodeStats sets the stats that FetchNodeStats will return.
func (m *MockClient) SetNodeStats(stats *NodeStatsResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodeStats = stats
}

func (m *MockClient) FetchNodeStats(ctx context.Context, hostname string) (NodeStatsResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if m.nodeStats != nil {
		return *m.nodeStats, nil
	}
	return NodeStatsResult{}, fmt.Errorf("no stats available")
}
