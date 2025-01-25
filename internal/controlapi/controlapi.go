package controlapi

import (
	"context"
	"time"
)

// Device represents a VPN node in the network
type Device struct {
	ID               string
	Name             string
	Hostname         string
	Created          time.Time
	LastSeen         time.Time
	IPAddrs          []string
	IsOnline         bool
	Tags             []string
	AdvertisedRoutes []string
}

type PreauthKey struct {
	Key  string
	Tags []string
}

// ControlApi defines a generic interface for VPN control plane operations
type ControlApi interface {
	// CreateKey generates a new auth key for an untrusted, ephemeral, preauthorized device
	CreateKey(ctx context.Context) (*PreauthKey, error)

	// ListDevices returns all devices in the network
	ListDevices(ctx context.Context) ([]Device, error)

	// ApproveExitNode enables the advertised routes for a device, making it an exit node
	ApproveExitNode(ctx context.Context, deviceID string) error

	// DeleteDevice removes a device from the network
	DeleteDevice(ctx context.Context, deviceID string) error
}
