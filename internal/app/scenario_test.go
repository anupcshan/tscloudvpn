package app_test

import (
	"net/netip"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
)

// TestScenario_HappyPath tests the full lifecycle:
// create → peer appears → health check discovers it → running → delete
func TestScenario_HappyPath(t *testing.T) {
	h := NewTestHarness(t)
	defer h.Cleanup()

	hostname := "fake-fake-us-east"

	// Pre-register device so Create() finds it during Tailscale registration wait
	h.ControlAPI.AddDevice(controlapi.Device{
		Hostname: hostname,
		Created:  time.Now().Add(time.Second),
	})

	// Add peer to mock Tailscale so health checks see it
	h.Transport.AddPeer(hostname, netip.MustParseAddr("100.64.0.2"))

	// Create the instance
	h.CreateInstance("fake", "fake-us-east")

	// Wait for it to reach running state via SSE events
	h.WaitForSSEEvent(15*time.Second, "node-card-running")

	// Delete it
	h.DeleteInstance("fake", "fake-us-east")
	h.Transport.RemovePeer(hostname)

	// Verify the node card disappears from SSE events
	h.WaitForSSEEvent(10*time.Second, "No active nodes")
}

// TestScenario_PeerDisappears tests that when a running peer disappears
// from Tailscale, the health check detects it and transitions to idle.
func TestScenario_PeerDisappears(t *testing.T) {
	h := NewTestHarness(t)
	defer h.Cleanup()

	hostname := "fake-fake-us-east"

	// Set up a running instance
	h.ControlAPI.AddDevice(controlapi.Device{
		Hostname: hostname,
		Created:  time.Now().Add(time.Second),
	})
	h.Transport.AddPeer(hostname, netip.MustParseAddr("100.64.0.2"))

	h.CreateInstance("fake", "fake-us-east")

	// Wait for it to be running
	h.WaitForSSEEvent(15*time.Second, "node-card-running")

	// Remove the peer — simulating the instance disappearing from Tailscale
	h.Transport.RemovePeer(hostname)

	// Health check should detect the peer is gone. Node card should disappear.
	h.WaitForSSEEvent(10*time.Second, "No active nodes")
}

// TestScenario_SlowRegistration tests that a node shows "launching" state
// while waiting for the peer to appear in Tailscale.
func TestScenario_SlowRegistration(t *testing.T) {
	h := NewTestHarness(t)
	defer h.Cleanup()

	hostname := "fake-fake-us-east"

	// Do NOT add the device to ControlAPI yet — simulates slow registration
	h.CreateInstance("fake", "fake-us-east")

	// Should show as launching in SSE events
	h.WaitForSSEEvent(5*time.Second, "launching")

	// Simulate the device registering
	h.ControlAPI.AddDevice(controlapi.Device{
		Hostname: hostname,
		Created:  time.Now().Add(time.Second),
	})
	h.Transport.AddPeer(hostname, netip.MustParseAddr("100.64.0.2"))

	// Should transition to running
	h.WaitForSSEEvent(15*time.Second, "node-card-running")
}
