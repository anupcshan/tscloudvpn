package tsclient

import (
	"context"
	"net/netip"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
)

// LocalClient wraps a Tailscale *local.Client to implement TailscaleClient.
type LocalClient struct {
	client *local.Client
}

// NewLocalClient creates a TailscaleClient backed by a real Tailscale local API client.
func NewLocalClient(client *local.Client) *LocalClient {
	return &LocalClient{client: client}
}

func (c *LocalClient) GetPeers(ctx context.Context) ([]PeerInfo, error) {
	status, err := c.client.Status(ctx)
	if err != nil {
		return nil, err
	}

	peers := make([]PeerInfo, 0, len(status.Peer))
	for _, p := range status.Peer {
		peers = append(peers, PeerInfo{
			Hostname:     p.HostName,
			Created:      p.Created,
			TailscaleIPs: p.TailscaleIPs,
		})
	}
	return peers, nil
}

func (c *LocalClient) PingPeer(ctx context.Context, addr netip.Addr) (PingResult, error) {
	result, err := c.client.Ping(ctx, addr, tailcfg.PingDisco)
	if err != nil {
		return PingResult{}, err
	}

	latency := time.Duration(result.LatencySeconds*1000000) * time.Microsecond
	connectionType := "direct"
	if result.Endpoint == "" && result.DERPRegionCode != "" {
		connectionType = "relayed via " + result.DERPRegionCode
	}

	return PingResult{
		Latency:        latency,
		ConnectionType: connectionType,
	}, nil
}
