package controlapi

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Device represents a VPN node in the network
type Device struct {
	// Private fields - used by control API implementations
	tailscaleID      string
	headscaleID      uint64

	// Public fields
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
	Key        string
	ControlURL string
	Tags       []string
}

func (k PreauthKey) GetCLIArgs() []string {
	var args []string
	args = append(args, fmt.Sprintf("--authkey=%s", k.Key))
	args = append(args, fmt.Sprintf("--advertise-tags=%s", strings.Join(k.Tags, ",")))
	if k.ControlURL != "" {
		args = append(args, fmt.Sprintf("--login-server=%s", k.ControlURL))
	}
	return args
}

// ControlApi defines a generic interface for VPN control plane operations
type ControlApi interface {
	// CreateKey generates a new auth key for an untrusted, ephemeral, preauthorized device
	CreateKey(ctx context.Context) (*PreauthKey, error)

	// ListDevices returns all devices in the network
	ListDevices(ctx context.Context) ([]Device, error)

	// ApproveExitNode enables the advertised routes for a device, making it an exit node
	ApproveExitNode(ctx context.Context, device *Device) error

	// DeleteDevice removes a device from the network
	DeleteDevice(ctx context.Context, device *Device) error
}
