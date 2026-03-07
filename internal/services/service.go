// Package services defines the service types that tscloudvpn can run on cloud VMs.
package services

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/providers"
)

//go:embed speedtest_init.sh.tmpl
var speedtestInitData string

//go:embed fileserver_init.sh.tmpl
var fileserverInitData string

// InstanceNameInput provides the inputs for constructing an instance name.
type InstanceNameInput struct {
	UserProvidedName string // from API request (persistent services)
	Provider         string
	Region           string
}

// ServiceType defines a category of service that runs on cloud VMs.
type ServiceType struct {
	// Name is the DNS-safe short identifier used in hostnames and URLs.
	Name string

	// Label is the human-readable name shown in the UI.
	Label string

	// InstanceName constructs the service-scoped instance name from the
	// given input. Exit nodes use provider-region (e.g. "do-nyc1"),
	// persistent services use the user-provided name (e.g. "documents").
	InstanceName func(InstanceNameInput) string

	// Hostname derives the globally-unique Tailscale hostname from the
	// instance name. For exit nodes this is the identity (e.g. "do-nyc1"),
	// for other services it prepends the service name (e.g. "speedtest-do-nyc1").
	Hostname func(instanceName string) string

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

	// FormatStats renders service-specific stats from the raw stats JSON
	// into label/value pairs for display in the UI. If nil, no extra
	// stats are shown (idle/active pill is always shown from last_active).
	FormatStats func(body []byte) []StatDisplay

	// Persistence, if non-nil, means this service uses a named volume.
	Persistence *PersistenceConfig

	// NamedInstances, if true, means the user provides the instance name
	// (e.g., "photos", "docs"). If false, the name is derived from
	// provider+region (e.g., "do-nyc1"). Controls UI rendering.
	NamedInstances bool

	// Links defines service endpoints shown in the UI for running instances.
	Links []ServiceLink

	// Env holds version URLs, checksums, and other values templated into
	// the init script.
	Env map[string]string
}

// StatDisplay is a label/value pair for service-specific stats in the UI.
type StatDisplay struct {
	Label string // "traffic", "apps", "connections"
	Value string // "1.2 GB", "3", "42"
}

// ServiceLink defines an endpoint shown in the UI for a running instance.
type ServiceLink struct {
	Label  string // "Web UI", "ADB"
	Format string // "http://%s", "adb connect %s:5555" — %s = hostname
	Render string // "link" = clickable <a>, "copy" = text with copy button
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

// All is the catalog of all known service types.
var All = []*ServiceType{&ExitNode, &SpeedTest, &FileServer}

// ByName returns the ServiceType with the given name, or nil if not found.
func ByName(name string) *ServiceType {
	for _, svc := range All {
		if svc.Name == name {
			return svc
		}
	}
	return nil
}

// ExitNode is the service type for Tailscale exit nodes.
var ExitNode = ServiceType{
	Name:  "exit",
	Label: "Exit Node",
	InstanceName: func(in InstanceNameInput) string {
		return in.Provider + "-" + in.Region
	},
	Hostname: func(instanceName string) string { return instanceName },
	InitScript:     providers.InitData,
	TailscaleFlags: []string{"--advertise-exit-node", "--advertise-connector"},
	Tags:           []string{"tag:untrusted"},
	VMRequirements: VMRequirements{},
	IdleTimeout:    4 * time.Hour,
	StatsPath:      "/stats.json",
	ParseStats:     parseLastActiveStats,
	FormatStats:    formatExitNodeStats,
}

// SpeedTest is the service type for LibreSpeed speed test servers.
var SpeedTest = ServiceType{
	Name:  "speedtest",
	Label: "Speed Test",
	InstanceName: func(in InstanceNameInput) string {
		return in.Provider + "-" + in.Region
	},
	Hostname: func(instanceName string) string { return "speedtest-" + instanceName },
	InitScript:     speedtestInitData,
	TailscaleFlags: nil,
	Tags:           []string{"tag:untrusted"},
	VMRequirements: VMRequirements{},
	IdleTimeout:    2 * time.Hour,
	StatsPath:      "/stats.json",
	ParseStats:     parseLastActiveStats,
	FormatStats:    formatSpeedTestStats,
	Links: []ServiceLink{
		{Label: "LibreSpeed", Format: "http://%s", Render: "link"},
	},
}

// FileServer is the service type for filebrowser-based file servers.
var FileServer = ServiceType{
	Name:  "files",
	Label: "File Server",
	InstanceName: func(in InstanceNameInput) string {
		return in.UserProvidedName
	},
	Hostname: func(instanceName string) string { return "files-" + instanceName },
	InitScript:     fileserverInitData,
	TailscaleFlags: nil,
	Tags:           []string{"tag:untrusted"},
	NamedInstances: true,
	VMRequirements: VMRequirements{},
	IdleTimeout:    2 * time.Hour,
	StatsPath:      "/stats.json",
	ParseStats:     parseLastActiveStats,
	Persistence:    &PersistenceConfig{MountPoint: "/data", Size: "1T", FSType: "zfs"},
	Links: []ServiceLink{
		{Label: "File Browser", Format: "http://%s", Render: "link"},
	},
}

// formatExitNodeStats returns traffic stats from the exit node's stats JSON.
func formatExitNodeStats(body []byte) []StatDisplay {
	var payload struct {
		ForwardedBytes int64 `json:"forwarded_bytes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return []StatDisplay{
		{Label: "traffic", Value: formatBytes(payload.ForwardedBytes)},
	}
}

func formatBytes(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(mb))
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/float64(kb))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// formatSpeedTestStats returns total throughput from the speedtest's stats JSON.
func formatSpeedTestStats(body []byte) []StatDisplay {
	var payload struct {
		TotalBytes int64 `json:"total_bytes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return []StatDisplay{
		{Label: "throughput", Value: formatBytes(payload.TotalBytes)},
	}
}

// parseLastActiveStats extracts last_active from a stats JSON.
func parseLastActiveStats(body []byte) (time.Time, error) {
	var payload struct {
		LastActive int64 `json:"last_active"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return time.Time{}, err
	}
	return time.Unix(payload.LastActive, 0), nil
}
