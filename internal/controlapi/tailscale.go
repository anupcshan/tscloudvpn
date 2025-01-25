package controlapi

import (
	"context"
	"time"

	"github.com/tailscale/tailscale-client-go/tailscale"
)

// TailscaleClient implements the ControlApi interface for Tailscale
type TailscaleClient struct {
	client *tailscale.Client
}

// NewTailscaleClient creates a new TailscaleClient instance
func NewTailscaleClient(tailnet string, oauthClientId string, oauthSecret string) (*TailscaleClient, error) {
	client, err := tailscale.NewClient("", tailnet, tailscale.WithOAuthClientCredentials(
		oauthClientId,
		oauthSecret,
		[]string{"devices", "routes"},
	))
	if err != nil {
		return nil, err
	}
	return &TailscaleClient{client: client}, nil
}

// CreateKey implements ControlApi.CreateKey
func (c *TailscaleClient) CreateKey(ctx context.Context) (*PreauthKey, error) {
	capabilities := tailscale.KeyCapabilities{}
	capabilities.Devices.Create.Tags = []string{"tag:untrusted"}
	capabilities.Devices.Create.Ephemeral = true
	capabilities.Devices.Create.Reusable = false
	capabilities.Devices.Create.Preauthorized = true

	key, err := c.client.CreateKey(ctx, capabilities)
	if err != nil {
		return nil, err
	}

	return &PreauthKey{
		Key:  key.Key,
		Tags: key.Capabilities.Devices.Create.Tags,
	}, nil
}

// ListDevices implements ControlApi.ListDevices
func (c *TailscaleClient) ListDevices(ctx context.Context) ([]Device, error) {
	tsDevices, err := c.client.Devices(ctx)
	if err != nil {
		return nil, err
	}

	devices := make([]Device, len(tsDevices))
	for i, d := range tsDevices {
		// Get routes for each device
		routes, err := c.client.DeviceSubnetRoutes(ctx, d.ID)
		if err != nil {
			return nil, err
		}

		devices[i] = Device{
			ID:               d.ID,
			Name:             d.Name,
			Hostname:         d.Hostname,
			Created:          d.Created.Time,
			LastSeen:         d.LastSeen.Time,
			IPAddrs:          d.Addresses,
			IsOnline:         time.Since(d.LastSeen.Time) < 5*time.Minute, // Consider online if seen in last 5 minutes
			Tags:             d.Tags,
			AdvertisedRoutes: routes.Advertised,
		}
	}
	return devices, nil
}

// ApproveExitNode implements ControlApi.ApproveExitNode
func (c *TailscaleClient) ApproveExitNode(ctx context.Context, deviceID string) error {
	routes, err := c.client.DeviceSubnetRoutes(ctx, deviceID)
	if err != nil {
		return err
	}
	return c.client.SetDeviceSubnetRoutes(ctx, deviceID, routes.Advertised)
}

// DeleteDevice implements ControlApi.DeleteDevice
func (c *TailscaleClient) DeleteDevice(ctx context.Context, deviceID string) error {
	return c.client.DeleteDevice(ctx, deviceID)
}

var _ ControlApi = (*TailscaleClient)(nil)
