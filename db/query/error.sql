-- name: InsertError :exec
INSERT INTO error_records (
  timestamp, error_type, error_message, provider_name, region_code, hostname
) VALUES (?, ?, ?, ?, ?, ?);
