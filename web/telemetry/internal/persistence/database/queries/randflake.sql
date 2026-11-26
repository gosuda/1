-- name: RandflakeLeaseCreate :exec
INSERT INTO randflake_leases(uuid, node_id, created_at, expires_at)
VALUES (?, ?, ?, ?);

-- name: RandflakeLeaseGet :one
SELECT * FROM randflake_leases WHERE uuid = ?;

-- name: RandflakeLeaseExtend :exec
UPDATE randflake_leases SET expires_at = ? WHERE uuid = ?;

-- name: RandflakeGC :exec
DELETE FROM randflake_leases WHERE expires_at < ?;
