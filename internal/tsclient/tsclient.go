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

// TailscaleClient abstracts the Tailscale local API client.
type TailscaleClient interface {
	// GetPeers returns all currently visible Tailscale peers.
	GetPeers(ctx context.Context) ([]PeerInfo, error)

	// PingPeer sends a disco ping to the peer at the given Tailscale IP.
	PingPeer(ctx context.Context, addr netip.Addr) (PingResult, error)
}
