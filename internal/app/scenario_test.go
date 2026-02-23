package app_test

import (
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/controlapi"
	"github.com/stretchr/testify/require"
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
	require.Eventually(t, func() bool {
		events := h.GetSSEEvents(2 * time.Second)
		return strings.Contains(events, "Fake US East") && strings.Contains(events, "node-card")
	}, 15*time.Second, 500*time.Millisecond, "Expected running node card in SSE events")

	// Delete it
	h.DeleteInstance("fake", "fake-us-east")
	h.Transport.RemovePeer(hostname)

	// Verify the node card disappears from SSE events
	require.Eventually(t, func() bool {
		events := h.GetSSEEvents(2 * time.Second)
		return strings.Contains(events, "No active nodes")
	}, 10*time.Second, 500*time.Millisecond, "Expected no active nodes after deletion")
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
	require.Eventually(t, func() bool {
		events := h.GetSSEEvents(2 * time.Second)
		return strings.Contains(events, "node-card") && strings.Contains(events, "Fake US East")
	}, 15*time.Second, 500*time.Millisecond, "Expected node to be running")

	// Remove the peer — simulating the instance disappearing from Tailscale
	h.Transport.RemovePeer(hostname)

	// Health check should detect the peer is gone. Node card should disappear.
	require.Eventually(t, func() bool {
		events := h.GetSSEEvents(2 * time.Second)
		return strings.Contains(events, "No active nodes")
	}, 10*time.Second, 500*time.Millisecond, "Expected node card to disappear after peer removed")
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
	require.Eventually(t, func() bool {
		events := h.GetSSEEvents(2 * time.Second)
		return strings.Contains(events, "launching")
	}, 5*time.Second, 500*time.Millisecond, "Expected launching state while waiting for registration")

	// Simulate the device registering
	h.ControlAPI.AddDevice(controlapi.Device{
		Hostname: hostname,
		Created:  time.Now().Add(time.Second),
	})
	h.Transport.AddPeer(hostname, netip.MustParseAddr("100.64.0.2"))

	// Should transition to running
	require.Eventually(t, func() bool {
		events := h.GetSSEEvents(2 * time.Second)
		return strings.Contains(events, "node-card") && strings.Contains(events, "Fake US East")
	}, 15*time.Second, 500*time.Millisecond, "Expected node to transition to running")
}
