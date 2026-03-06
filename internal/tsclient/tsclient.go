package tsclient

import (
	"context"
	"net/netip"
	"time"
)

// PeerInfo contains the subset of Tailscale peer information we use.
type PeerInfo struct {
	Hostname     string
	Created      time.Time
	TailscaleIPs []netip.Addr
}

// PingResult contains the result of pinging a Tailscale peer.
type PingResult struct {
	Latency        time.Duration
	ConnectionType string // "direct" or "relayed via <DERP region>"
}

// NodeStatsResult contains statistics fetched from a managed node.
type NodeStatsResult struct {
	ForwardedBytes int64
	LastActive     time.Time
	RawBody        []byte // raw JSON for service-specific FormatStats
}

// NodeIdentity contains the static identity of a managed VM,
// written at boot by the init script and served at /identity.json.
type NodeIdentity struct {
	Service  string `json:"service"`
	Provider string `json:"provider"`
	Region   string `json:"region"`
	Name     string `json:"name"` // instance name (e.g., "photos", "do-nyc1")
}

// TailscaleClient abstracts the Tailscale local API client.
type TailscaleClient interface {
	// GetPeers returns all currently visible Tailscale peers.
	GetPeers(ctx context.Context) ([]PeerInfo, error)

	// PingPeer sends a disco ping to the peer at the given Tailscale IP.
	PingPeer(ctx context.Context, addr netip.Addr) (PingResult, error)

	// FetchNodeStats fetches traffic statistics from an exit node via tailscale serve.
	FetchNodeStats(ctx context.Context, hostname string) (NodeStatsResult, error)

	// FetchNodeIdentity fetches the static identity of a managed VM from its
	// identity endpoint (port 8245/identity.json).
	FetchNodeIdentity(ctx context.Context, hostname string) (NodeIdentity, error)
}
