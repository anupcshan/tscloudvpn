// Package services defines the service types that tscloudvpn can run on cloud VMs.
package services

import (
	"encoding/json"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/providers"
)

// ServiceType defines a category of service that runs on cloud VMs.
type ServiceType struct {
	// Name is the DNS-safe short identifier used in hostnames and URLs.
	Name string

	// Label is the human-readable name shown in the UI.
	Label string

	// InitScript is the cloud-init template text (text/template format).
	InitScript string

	// TailscaleFlags are appended to `tailscale up` in the init script.
	TailscaleFlags []string

	// Tags are the Tailscale ACL tags requested for this service's auth keys.
	Tags []string

	// VMRequirements tells providers what minimum specs to select.
	VMRequirements VMRequirements

	// IdleTimeout is how long the service can be idle before auto-deletion.
	// Zero means no idle shutdown.
	IdleTimeout time.Duration

	// StatsPath is the HTTP path on the node to poll for stats.
	// Empty string means don't fetch stats.
	StatsPath string

	// ParseStats extracts the last-active time from a stats response body.
	// If nil, no stats-based idle detection is performed.
	ParseStats func(body []byte) (lastActive time.Time, err error)

	// Persistence, if non-nil, means this service uses a named volume.
	Persistence *PersistenceConfig

	// Env holds version URLs, checksums, and other values templated into
	// the init script.
	Env map[string]string
}

// VMRequirements specifies minimum VM capabilities for a service.
type VMRequirements struct {
	MinVCPUs   int  // 0 = cheapest available
	MinRAMGB   int  // 0 = cheapest available
	MinDiskGB  int  // 0 = provider default
	NestedVirt bool // requires KVM support
}

// PersistenceConfig defines persistent block storage for a service.
type PersistenceConfig struct {
	MountPoint string // e.g., "/data"
	Size       string // e.g., "10G"
	FSType     string // always "zfs" for persistent services
}

// ExitNode is the service type for Tailscale exit nodes.
var ExitNode = ServiceType{
	Name:           "exit",
	Label:          "Exit Node",
	InitScript:     providers.InitData,
	TailscaleFlags: []string{"--advertise-exit-node", "--advertise-connector"},
	Tags:           []string{"tag:untrusted"},
	VMRequirements: VMRequirements{},
	IdleTimeout:    4 * time.Hour,
	StatsPath:      "/stats.json",
	ParseStats:     parseExitNodeStats,
}

// parseExitNodeStats extracts last_active from the exit node's stats JSON.
func parseExitNodeStats(body []byte) (time.Time, error) {
	var payload struct {
		LastActive int64 `json:"last_active"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return time.Time{}, err
	}
	return time.Unix(payload.LastActive, 0), nil
}
