-- name: GetPingSummary :one
SELECT
  COUNT(*),
  SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END),
  AVG(CASE WHEN success = 1 THEN latency_ms ELSE 0 END)
FROM ping_records
WHERE
  hostname = ? AND
  timestamp >= ? AND
  timestamp < ?;

-- name: GetP99Latency :one
SELECT latency_ms
FROM ping_records
WHERE hostname = ?
ORDER BY latency_ms
LIMIT 1 OFFSET ?;

-- name: GetErrorCounts :many
SELECT error_type, COUNT(*)
FROM error_records
WHERE
  hostname = ? AND
  timestamp >= ? AND
  timestamp < ?
GROUP BY error_type;

-- name: GetAllHostnames :many
SELECT DISTINCT hostname
FROM ping_records
ORDER BY hostname;
