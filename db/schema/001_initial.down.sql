DROP INDEX IF EXISTS idx_ping_records_hostname;
DROP INDEX IF EXISTS idx_ping_records_timestamp;
DROP INDEX IF EXISTS idx_error_records_error_type;
DROP INDEX IF EXISTS idx_error_records_timestamp;

DROP TABLE IF EXISTS ping_records;
DROP TABLE IF EXISTS error_records;