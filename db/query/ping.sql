-- name: InsertPing :exec
INSERT INTO ping_records (
  hostname, timestamp, success, latency_ms, connection_type, provider_name, region_code
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: GetRecentPings :many
SELECT
  hostname, timestamp, success, latency_ms, connection_type, provider_name, region_code
FROM ping_records
WHERE hostname = ?
ORDER BY timestamp DESC
LIMIT ?;
