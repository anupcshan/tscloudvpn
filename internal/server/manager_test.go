package server

import (
	"testing"
	"time"

	"github.com/anupcshan/tscloudvpn/internal/instances"
)

func TestNewPingHistory(t *testing.T) {
	ph := instances.NewPingHistory()
	if ph == nil {
		t.Fatal("NewPingHistory returned nil")
	}
}

func TestPingHistory_AddResult(t *testing.T) {
	tests := []struct {
		name    string
		results []struct {
			Success        bool
			Latency        time.Duration
			ConnectionType string
		}
		wantSuccesses  int
		wantAvgLatency time.Duration
	}{
		{
			name: "single success",
			results: []struct {
				Success        bool
				Latency        time.Duration
				ConnectionType string
			}{
				{Success: true, Latency: 100 * time.Millisecond, ConnectionType: "direct"},
			},
			wantSuccesses:  1,
			wantAvgLatency: 100 * time.Millisecond,
		},
		{
			name: "single failure",
			results: []struct {
				Success        bool
				Latency        time.Duration
				ConnectionType string
			}{
				{Success: false, Latency: 0, ConnectionType: ""},
			},
			wantSuccesses:  0,
			wantAvgLatency: 0,
		},
		{
			name: "mixed results",
			results: []struct {
				Success        bool
				Latency        time.Duration
				ConnectionType string
			}{
				{Success: true, Latency: 100 * time.Millisecond, ConnectionType: "direct"},
				{Success: false, Latency: 0, ConnectionType: ""},
				{Success: true, Latency: 200 * time.Millisecond, ConnectionType: "direct"},
			},
			wantSuccesses:  2,
			wantAvgLatency: 150 * time.Millisecond,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ph := instances.NewPingHistory()
			for _, r := range tt.results {
				ph.AddResult(r.Success, r.Latency, r.ConnectionType)
			}

			rate, latency, _, _, _ := ph.GetStats()
			gotSuccesses := int(rate * float64(len(tt.results)))
			if gotSuccesses != tt.wantSuccesses {
				t.Errorf("Expected %d successes, got %d", tt.wantSuccesses, gotSuccesses)
			}
			if len(tt.results) > 0 && tt.wantSuccesses > 0 && latency != tt.wantAvgLatency {
				t.Errorf("Expected avg latency %v, got %v", tt.wantAvgLatency, latency)
			}
		})
	}
}

func TestPingHistory_StdDev(t *testing.T) {
	ph := instances.NewPingHistory()

	// Add sequence of successful pings with varying latencies
	// mean = (100 + 150 + 80 + 200) / 4 = 132.5ms
	// variance = ((100-132.5)² + (150-132.5)² + (80-132.5)² + (200-132.5)²) / 4
	//          = (1056.25 + 306.25 + 2756.25 + 4556.25) / 4 = 2168.75
	// stddev = sqrt(2168.75) ≈ 46.57ms
	latencies := []time.Duration{
		100 * time.Millisecond,
		150 * time.Millisecond,
		80 * time.Millisecond,
		200 * time.Millisecond,
	}

	for _, latency := range latencies {
		ph.AddResult(true, latency, "direct")
	}

	_, _, stddev, _, _ := ph.GetStats()
	expectedStdDev := 46 * time.Millisecond // sqrt(2168.75) ≈ 46.57ms
	if stddev < expectedStdDev-2*time.Millisecond || stddev > expectedStdDev+2*time.Millisecond {
		t.Errorf("Expected stddev around %v, got %v", expectedStdDev, stddev)
	}
}

func TestPingHistory_StdDev_RingBufferEviction(t *testing.T) {
	// Use small buffer size of 4 to easily test eviction
	ph := instances.NewPingHistoryWithSize(4)

	// Add [100, 150, 80, 200] - same as TestPingHistory_StdDev
	latencies := []time.Duration{
		100 * time.Millisecond,
		150 * time.Millisecond,
		80 * time.Millisecond,
		200 * time.Millisecond,
	}
	for _, latency := range latencies {
		ph.AddResult(true, latency, "direct")
	}

	// Verify initial state: mean=132.5ms, stddev≈46.57ms
	_, avgLatency, stddev, _, _ := ph.GetStats()
	if avgLatency < 132*time.Millisecond || avgLatency > 133*time.Millisecond {
		t.Errorf("Initial: expected avg latency ~132ms, got %v", avgLatency)
	}
	if stddev < 44*time.Millisecond || stddev > 48*time.Millisecond {
		t.Errorf("Initial: expected stddev ~46ms, got %v", stddev)
	}

	// Add 250ms which evicts 100ms. Buffer now has [250, 150, 80, 200]
	// mean = (250 + 150 + 80 + 200) / 4 = 680 / 4 = 170ms
	// variance = ((250-170)² + (150-170)² + (80-170)² + (200-170)²) / 4
	//          = (6400 + 400 + 8100 + 900) / 4 = 15800 / 4 = 3950
	// stddev = sqrt(3950) ≈ 62.85ms
	ph.AddResult(true, 250*time.Millisecond, "direct")

	_, avgLatency, stddev, _, _ = ph.GetStats()
	expectedAvg := 170 * time.Millisecond
	if avgLatency != expectedAvg {
		t.Errorf("After eviction: expected avg latency %v, got %v", expectedAvg, avgLatency)
	}
	expectedStdDev := 62 * time.Millisecond
	if stddev < expectedStdDev-2*time.Millisecond || stddev > expectedStdDev+2*time.Millisecond {
		t.Errorf("After eviction: expected stddev around %v, got %v", expectedStdDev, stddev)
	}
}

func TestPingHistory_TimeSinceFailure(t *testing.T) {
	ph := instances.NewPingHistory()

	// Add a successful ping
	ph.AddResult(true, 100*time.Millisecond, "direct")

	// Add a failure
	failureTime := time.Now()
	ph.AddResult(false, 0, "")

	// Add another success
	ph.AddResult(true, 100*time.Millisecond, "direct")

	_, _, _, timeSinceFailure, _ := ph.GetStats()

	// Verify time since failure is reasonable
	if timeSinceFailure < 0 {
		t.Error("Time since failure should not be negative")
	}
	if time.Since(failureTime) < timeSinceFailure {
		t.Error("Time since failure should not be greater than actual time passed")
	}
}

func TestPingHistory_ConnectionType(t *testing.T) {
	ph := instances.NewPingHistory()

	// Test connection type changes
	scenarios := []struct {
		success        bool
		latency        time.Duration
		connectionType string
	}{
		{true, 100 * time.Millisecond, "direct"},
		{true, 150 * time.Millisecond, "relayed via sfo"},
		{false, 0, ""},
		{true, 120 * time.Millisecond, "direct"},
	}

	for _, s := range scenarios {
		ph.AddResult(s.success, s.latency, s.connectionType)
	}

	_, _, _, _, connType := ph.GetStats()
	if connType != "direct" { // Should return most recent successful connection type
		t.Errorf("Expected final connection type 'direct', got %q", connType)
	}
}
