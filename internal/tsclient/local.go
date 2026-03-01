package tsclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/tailcfg"
)

// LocalClient wraps a Tailscale *local.Client to implement TailscaleClient.
type LocalClient struct {
	client     *local.Client
	httpClient *http.Client // HTTP client that routes through the Tailscale network
}

// NewLocalClient creates a TailscaleClient backed by a real Tailscale local API client.
// The httpClient should route through the Tailscale network (e.g. from tsnet.Server.HTTPClient()).
func NewLocalClient(client *local.Client, httpClient *http.Client) *LocalClient {
	return &LocalClient{client: client, httpClient: httpClient}
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

func (c *LocalClient) FetchNodeStats(ctx context.Context, hostname string) (NodeStatsResult, error) {
	url := fmt.Sprintf("http://%s:8245/stats.json", hostname)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return NodeStatsResult{}, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return NodeStatsResult{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return NodeStatsResult{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var payload struct {
		ForwardedBytes int64 `json:"forwarded_bytes"`
		LastActive     int64 `json:"last_active"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return NodeStatsResult{}, err
	}

	return NodeStatsResult{
		ForwardedBytes: payload.ForwardedBytes,
		LastActive:     time.Unix(payload.LastActive, 0),
	}, nil
}

func (c *LocalClient) FetchNodeIdentity(ctx context.Context, hostname string) (NodeIdentity, error) {
	url := fmt.Sprintf("http://%s:8245/identity.json", hostname)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return NodeIdentity{}, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return NodeIdentity{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return NodeIdentity{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}

	var identity NodeIdentity
	if err := json.NewDecoder(resp.Body).Decode(&identity); err != nil {
		return NodeIdentity{}, err
	}

	return identity, nil
}
