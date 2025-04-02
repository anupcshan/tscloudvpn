package stats

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// Stats represents the SQLite-based statistics tracker
type Stats struct {
	db       *sql.DB
	mu       sync.Mutex
	dbPath   string
	dbActive bool
}

// PingRecord represents a single ping record
type PingRecord struct {
	Hostname       string
	Timestamp      time.Time
	Success        bool
	Latency        time.Duration
	ConnectionType string
	ProviderName   string
	RegionCode     string
}

// ErrorRecord represents a single error record
type ErrorRecord struct {
	Timestamp    time.Time
	ErrorType    string
	ErrorMessage string
	ProviderName string
	RegionCode   string
	Hostname     string
}

// StatsSummary represents a summary of statistics
type StatsSummary struct {
	TotalPings      int
	SuccessfulPings int
	AverageLatency  time.Duration
	P99Latency      time.Duration
	ErrorCount      int
	ErrorsByType    map[string]int
}

// NewStats creates a new Stats instance
func NewStats(dataDir string) (*Stats, error) {
	// Create data directory if it doesn't exist
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dataDir, "tscloudvpn_stats.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS ping_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		hostname TEXT NOT NULL,
		timestamp INTEGER NOT NULL, -- Unix timestamp in seconds
		success BOOLEAN NOT NULL,
		latency_ms INTEGER,
		connection_type TEXT,
		provider_name TEXT,
		region_code TEXT
	);

	CREATE TABLE IF NOT EXISTS error_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp INTEGER NOT NULL, -- Unix timestamp in seconds
		error_type TEXT NOT NULL,
		error_message TEXT,
		provider_name TEXT,
		region_code TEXT,
		hostname TEXT
	);

	CREATE INDEX IF NOT EXISTS idx_ping_records_hostname ON ping_records(hostname);
	CREATE INDEX IF NOT EXISTS idx_ping_records_timestamp ON ping_records(timestamp);
	CREATE INDEX IF NOT EXISTS idx_error_records_error_type ON error_records(error_type);
	CREATE INDEX IF NOT EXISTS idx_error_records_timestamp ON error_records(timestamp);
	`)
	if err != nil {
		db.Close()
		return nil, err
	}

	return &Stats{
		db:       db,
		dbPath:   dbPath,
		dbActive: true,
	}, nil
}

// Close closes the database connection
func (s *Stats) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dbActive {
		return nil
	}

	s.dbActive = false
	return s.db.Close()
}

// RecordPing records a ping result
func (s *Stats) RecordPing(ctx context.Context, record PingRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dbActive {
		return nil
	}

	latencyMs := int64(0)
	if record.Success {
		latencyMs = int64(record.Latency.Milliseconds())
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ping_records (
			hostname, timestamp, success, latency_ms, connection_type, provider_name, region_code
		) VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		record.Hostname,
		record.Timestamp.UTC().Unix(), // Store Unix timestamp in seconds
		record.Success,
		latencyMs,
		record.ConnectionType,
		record.ProviderName,
		record.RegionCode,
	)

	return err
}

// RecordError records an error
func (s *Stats) RecordError(ctx context.Context, record ErrorRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dbActive {
		return nil
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO error_records (
			timestamp, error_type, error_message, provider_name, region_code, hostname
		) VALUES (?, ?, ?, ?, ?, ?)
	`,
		record.Timestamp.UTC().Unix(), // Store Unix timestamp in seconds
		record.ErrorType,
		record.ErrorMessage,
		record.ProviderName,
		record.RegionCode,
		record.Hostname,
	)

	return err
}

// GetPingSummary returns a summary of ping statistics for a given time range
func (s *Stats) GetPingSummary(ctx context.Context, hostname string, from, to time.Time) (StatsSummary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var summary StatsSummary = StatsSummary{
		ErrorsByType: make(map[string]int),
	}

	if !s.dbActive {
		return summary, nil
	}

	// Get ping statistics
	row := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END),
			AVG(CASE WHEN success = 1 THEN latency_ms ELSE 0 END)
		FROM ping_records
		WHERE
			hostname = ? AND
			timestamp BETWEEN ? AND ?
	`, hostname, from.UTC().Unix(), to.UTC().Unix())

	var successfulPings sql.NullInt64
	var avgLatencyMs sql.NullFloat64

	err := row.Scan(
		&summary.TotalPings,
		&successfulPings,
		&avgLatencyMs,
	)
	if err != nil {
		return summary, err
	}

	if successfulPings.Valid && successfulPings.Int64 > 0 {
		summary.SuccessfulPings = int(successfulPings.Int64)
		// Calculate p99 latency
		p99Row := s.db.QueryRowContext(ctx, `
			SELECT latency_ms
			FROM ping_records
			WHERE hostname = ? AND
			      timestamp BETWEEN ? AND ? AND
			      success = 1
			ORDER BY latency_ms
			LIMIT 1 OFFSET ?`,
			hostname,
			from.UTC().Unix(),
			to.UTC().Unix(),
			int64(float64(successfulPings.Int64-1)*0.99),
		)

		var p99LatencyMs sql.NullInt64
		if err := p99Row.Scan(&p99LatencyMs); err != nil {
			return summary, err
		}
		if p99LatencyMs.Valid {
			summary.P99Latency = time.Duration(p99LatencyMs.Int64) * time.Millisecond
		}
	}

	if avgLatencyMs.Valid {
		summary.AverageLatency = time.Duration(avgLatencyMs.Float64) * time.Millisecond
	}

	// Get error counts
	rows, err := s.db.QueryContext(ctx, `
		SELECT error_type, COUNT(*)
		FROM error_records
		WHERE
			hostname = ? AND
			timestamp BETWEEN ? AND ?
		GROUP BY error_type
	`, hostname, from.UTC().Unix(), to.UTC().Unix())
	if err != nil {
		return summary, err
	}
	defer rows.Close()

	summary.ErrorCount = 0
	for rows.Next() {
		var errorType string
		var count int
		if err := rows.Scan(&errorType, &count); err != nil {
			return summary, err
		}
		summary.ErrorsByType[errorType] = count
		summary.ErrorCount += count
	}

	return summary, rows.Err()
}

// GetRecentPings returns the most recent ping records for a given hostname
func (s *Stats) GetRecentPings(ctx context.Context, hostname string, limit int) ([]PingRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var records []PingRecord

	if !s.dbActive {
		return records, nil
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			hostname, timestamp, success, latency_ms, connection_type, provider_name, region_code
		FROM ping_records
		WHERE hostname = ?
		ORDER BY timestamp DESC
		LIMIT ?
	`, hostname, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var record PingRecord
		var latencyMs int64
		var unixTimestamp int64

		err := rows.Scan(
			&record.Hostname,
			&unixTimestamp,
			&record.Success,
			&latencyMs,
			&record.ConnectionType,
			&record.ProviderName,
			&record.RegionCode,
		)
		if err != nil {
			return records, err
		}

		// Convert Unix timestamp to time.Time
		record.Timestamp = time.Unix(unixTimestamp, 0).UTC()
		record.Latency = time.Duration(latencyMs) * time.Millisecond
		records = append(records, record)
	}

	return records, rows.Err()
}

// PruneOldRecords removes records older than the specified duration
func (s *Stats) PruneOldRecords(ctx context.Context, olderThan time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dbActive {
		return nil
	}

	cutoffTime := time.Now().UTC().Add(-olderThan).Unix()

	// Delete old ping records
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM ping_records
		WHERE timestamp < ?
	`, cutoffTime)
	if err != nil {
		return err
	}

	// Delete old error records
	_, err = s.db.ExecContext(ctx, `
		DELETE FROM error_records
		WHERE timestamp < ?
	`, cutoffTime)
	return err
}

// GetAllHostnames returns a list of all unique hostnames in the database
func (s *Stats) GetAllHostnames(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var hostnames []string

	if !s.dbActive {
		return hostnames, nil
	}

	// Get unique hostnames from ping_records
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT hostname
		FROM ping_records
		ORDER BY hostname
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var hostname string
		if err := rows.Scan(&hostname); err != nil {
			return hostnames, err
		}
		hostnames = append(hostnames, hostname)
	}

	return hostnames, rows.Err()
}
