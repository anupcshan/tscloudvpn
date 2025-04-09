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
