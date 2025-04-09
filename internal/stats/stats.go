package stats

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anupcshan/tscloudvpn/db"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/sqlite"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "modernc.org/sqlite"
)

// Stats represents the SQLite-based statistics tracker
type Stats struct {
	queries  *db.Queries
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
	sqlDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	migrationSource, err := iofs.New(db.FS, "schema")
	if err != nil {
		return nil, err
	}

	sqliteDB, err := sqlite.WithInstance(sqlDB, &sqlite.Config{})
	if err != nil {
		return nil, err
	}

	migrator, err := migrate.NewWithInstance("iofs", migrationSource, "sqlite", sqliteDB)
	if err != nil {
		return nil, err
	}

	if err := migrator.Up(); err != nil && err != migrate.ErrNoChange {
		return nil, err
	}

	queries := db.New(sqlDB)

	return &Stats{
		queries:  queries,
		db:       sqlDB,
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

	params := db.InsertPingParams{
		Hostname:       record.Hostname,
		Timestamp:      record.Timestamp.UTC().Unix(), // Store Unix timestamp in seconds
		Success:        record.Success,
		LatencyMs:      sql.NullInt64{Int64: latencyMs, Valid: record.Success},
		ConnectionType: sql.NullString{String: record.ConnectionType, Valid: record.ConnectionType != ""},
		ProviderName:   sql.NullString{String: record.ProviderName, Valid: record.ProviderName != ""},
		RegionCode:     sql.NullString{String: record.RegionCode, Valid: record.RegionCode != ""},
	}

	return s.queries.InsertPing(ctx, params)
}

// RecordError records an error
func (s *Stats) RecordError(ctx context.Context, record ErrorRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dbActive {
		return nil
	}

	params := db.InsertErrorParams{
		Timestamp:    record.Timestamp.Unix(), // Store Unix timestamp in seconds
		ErrorType:    record.ErrorType,
		ErrorMessage: sql.NullString{String: record.ErrorMessage, Valid: record.ErrorMessage != ""},
		ProviderName: sql.NullString{String: record.ProviderName, Valid: record.ProviderName != ""},
		RegionCode:   sql.NullString{String: record.RegionCode, Valid: record.RegionCode != ""},
		Hostname:     sql.NullString{String: record.Hostname, Valid: record.Hostname != ""},
	}

	return s.queries.InsertError(ctx, params)
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
	params := db.GetPingSummaryParams{
		Hostname:    hostname,
		Timestamp:   from.Unix(),
		Timestamp_2: to.Unix(),
	}

	result, err := s.queries.GetPingSummary(ctx, params)
	if err != nil {
		return summary, err
	}

	summary.TotalPings = int(result.Count)
	if result.Sum.Valid {
		summary.SuccessfulPings = int(result.Count)

		// Calculate p99 latency
		p99Params := db.GetP99LatencyParams{
			Hostname: hostname,
			Offset:   int64(float64(result.Count-1) * 0.99),
		}

		p99Latency, err := s.queries.GetP99Latency(ctx, p99Params)
		if err != nil {
			return summary, err
		}

		if p99Latency.Valid {
			summary.P99Latency = time.Duration(p99Latency.Int64) * time.Millisecond
		}
	}

	if result.Avg.Valid {
		summary.AverageLatency = time.Duration(result.Avg.Float64) * time.Millisecond
	}

	// Get ping statistics
	errorCountParams := db.GetErrorCountsParams{
		Hostname:    sql.NullString{String: hostname, Valid: true},
		Timestamp:   from.Unix(),
		Timestamp_2: to.Unix(),
	}

	errorCounts, err := s.queries.GetErrorCounts(ctx, errorCountParams)
	if err != nil {
		return summary, err
	}

	summary.ErrorCount = 0
	for _, errorCount := range errorCounts {
		summary.ErrorsByType[errorCount.ErrorType] = int(errorCount.Count)
		summary.ErrorCount += int(errorCount.Count)
	}

	return summary, nil
}

// GetRecentPings returns the most recent ping records for a given hostname
func (s *Stats) GetRecentPings(ctx context.Context, hostname string, limit int) ([]PingRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var records []PingRecord

	if !s.dbActive {
		return records, nil
	}

	recentPings, err := s.queries.GetRecentPings(ctx, db.GetRecentPingsParams{
		Hostname: hostname,
		Limit:    int64(limit),
	})
	if err != nil {
		return nil, err
	}

	records = make([]PingRecord, len(recentPings))
	for i, record := range recentPings {
		records[i] = PingRecord{
			Hostname:       record.Hostname,
			Timestamp:      time.Unix(record.Timestamp, 0).UTC(),
			Success:        record.Success,
			Latency:        time.Duration(record.LatencyMs.Int64) * time.Millisecond,
			ConnectionType: record.ConnectionType.String,
			ProviderName:   record.ProviderName.String,
			RegionCode:     record.RegionCode.String,
		}
	}

	return records, nil
}

// PruneOldRecords removes records older than the specified duration
func (s *Stats) PruneOldRecords(ctx context.Context, olderThan time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.dbActive {
		return nil
	}

	cutoffTime := time.Now().UTC().Add(-olderThan).Unix()

	// No generated code for delete queries, so using raw sql
	_, err := s.db.ExecContext(ctx, `
DELETE FROM ping_records
WHERE timestamp < ?
`, cutoffTime)
	if err != nil {
		return err
	}

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
	hostnamesResult, err := s.queries.GetAllHostnames(ctx)
	if err != nil {
		return nil, err
	}

	hostnames = make([]string, len(hostnamesResult))
	for i, hostname := range hostnamesResult {
		hostnames[i] = hostname
	}

	return hostnames, nil
}
