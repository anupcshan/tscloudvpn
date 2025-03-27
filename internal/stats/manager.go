package stats

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// StatsManager is responsible for initializing and managing the statistics system
type StatsManager struct {
	stats        *Stats
	dataDir      string
	cleanupTimer *time.Timer
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
}

// DefaultDataDir returns the default data directory
func DefaultDataDir() string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		// Fallback to current directory if we can't get home directory
		return ".tscloudvpn"
	}
	return filepath.Join(homeDir, ".tscloudvpn")
}

// NewStatsManager creates a new StatsManager
func NewStatsManager(dataDir string) (*StatsManager, error) {
	if dataDir == "" {
		dataDir = DefaultDataDir()
	}

	stats, err := NewStats(dataDir)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	manager := &StatsManager{
		stats:   stats,
		dataDir: dataDir,
		ctx:     ctx,
		cancel:  cancel,
	}

	// Start background cleanup routine
	manager.startCleanupRoutine()

	return manager, nil
}

// startCleanupRoutine starts a background routine to periodically clean up old records
func (m *StatsManager) startCleanupRoutine() {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()

		// Clean up old records every 24 hours
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-m.ctx.Done():
				return
			case <-ticker.C:
				// Keep records for 30 days
				if err := m.stats.PruneOldRecords(m.ctx, 30*24*time.Hour); err != nil {
					log.Printf("Error pruning old records: %v", err)
				}
			}
		}
	}()
}

// RecordPing records a ping result
func (m *StatsManager) RecordPing(record PingRecord) {
	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()

		if err := m.stats.RecordPing(ctx, record); err != nil {
			log.Printf("Error recording ping: %v", err)
		}
	}()
}

// RecordError records an error
func (m *StatsManager) RecordError(record ErrorRecord) {
	go func() {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()

		if err := m.stats.RecordError(ctx, record); err != nil {
			log.Printf("Error recording error: %v", err)
		}
	}()
}

// GetPingSummary returns a summary of ping statistics for a given time range
func (m *StatsManager) GetPingSummary(hostname string, from, to time.Time) (StatsSummary, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()

	return m.stats.GetPingSummary(ctx, hostname, from, to)
}

// GetRecentPings returns the most recent ping records for a given hostname
func (m *StatsManager) GetRecentPings(hostname string, limit int) ([]PingRecord, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()

	return m.stats.GetRecentPings(ctx, hostname, limit)
}

// GetAllHostnames returns a list of all unique hostnames that have statistics
func (m *StatsManager) GetAllHostnames() ([]string, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()

	return m.stats.GetAllHostnames(ctx)
}

// Close closes the statistics manager
func (m *StatsManager) Close() error {
	// Cancel context to stop background goroutines
	m.cancel()

	// Wait for all goroutines to complete
	m.wg.Wait()

	// Close the stats database
	return m.stats.Close()
}
