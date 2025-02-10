package main

import (
	"testing"
	"time"
)

func TestNewPingHistory(t *testing.T) {
	ph := NewPingHistory()
	if ph == nil {
		t.Fatal("NewPingHistory returned nil")
	}
	if len(ph.history) != historySize {
		t.Errorf("Expected history size %d, got %d", historySize, len(ph.history))
	}
	if ph.successCount != 0 {
		t.Errorf("Expected initial successCount 0, got %d", ph.successCount)
	}
	if ph.position != 0 {
		t.Errorf("Expected initial position 0, got %d", ph.position)
	}
}

func TestPingHistory_AddResult(t *testing.T) {
	tests := []struct {
		name           string
		results        []PingResult
		wantSuccesses  int
		wantAvgLatency time.Duration
	}{
		{
			name: "single success",
			results: []PingResult{
				{Success: true, Latency: 100 * time.Millisecond, ConnectionType: "direct"},
			},
			wantSuccesses:  1,
			wantAvgLatency: 100 * time.Millisecond,
		},
		{
			name: "single failure",
			results: []PingResult{
				{Success: false, Latency: 0, ConnectionType: ""},
			},
			wantSuccesses:  0,
			wantAvgLatency: 0,
		},
		{
			name: "mixed results",
			results: []PingResult{
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
			ph := NewPingHistory()
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

func TestPingHistory_RingBufferBehavior(t *testing.T) {
	ph := NewPingHistory()
	numEntries := historySize + 10 // Test wraparound

	// Add more entries than buffer size
	for i := 0; i < numEntries; i++ {
		success := i%2 == 0
		latency := time.Duration(100*(i+1)) * time.Millisecond
		ph.AddResult(success, latency, "direct")
	}

	// Verify buffer size hasn't grown
	if len(ph.history) != historySize {
		t.Errorf("Buffer grew beyond historySize: got %d, want %d", len(ph.history), historySize)
	}

	// Verify position wrapped around
	if ph.position != numEntries%historySize {
		t.Errorf("Position incorrect after wraparound: got %d, want %d", ph.position, numEntries%historySize)
	}

	// Verify stats are based only on entries in the buffer
	rate, _, _, _, _ := ph.GetStats()
	expectedRate := 0.5 // Due to alternating success/failure
	if rate < expectedRate-0.1 || rate > expectedRate+0.1 {
		t.Errorf("Success rate incorrect after wraparound: got %f, want approximately %f", rate, expectedRate)
	}
}

func TestPingHistory_Jitter(t *testing.T) {
	ph := NewPingHistory()

	// Add sequence of successful pings with varying latencies
	latencies := []time.Duration{
		100 * time.Millisecond,
		150 * time.Millisecond, // +50ms jitter
		80 * time.Millisecond,  // -70ms jitter
		200 * time.Millisecond, // +120ms jitter
	}

	for _, latency := range latencies {
		ph.AddResult(true, latency, "direct")
	}

	_, _, jitter, _, _ := ph.GetStats()
	expectedJitter := 80 * time.Millisecond // (50 + 70 + 120) / 3
	if jitter < expectedJitter-time.Millisecond || jitter > expectedJitter+time.Millisecond {
		t.Errorf("Expected jitter around %v, got %v", expectedJitter, jitter)
	}
}

func TestPingHistory_TimeSinceFailure(t *testing.T) {
	ph := NewPingHistory()

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
	ph := NewPingHistory()

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
