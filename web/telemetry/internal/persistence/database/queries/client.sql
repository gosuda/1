-- name: ClientRegister :exec
INSERT INTO client_identifiers (id, token, created_at)
VALUES (?, ?, ?);

-- name: ClientLookupByID :one
SELECT *
FROM client_identifiers
WHERE id = ?;

-- name: ClientLookupByToken :one
SELECT *
FROM client_identifiers
WHERE token = ?;

-- name: ClientRegisterFingerprint :exec
INSERT INTO client_fingerprints (id, client_id, user_agent, user_agent_data, fpversion, fphash, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ClientVerifyToken :one
SELECT 1 FROM client_identifiers WHERE id = ? AND token = ?;
