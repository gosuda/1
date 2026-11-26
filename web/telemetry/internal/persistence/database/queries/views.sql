-- name: ViewInsert :exec
INSERT INTO views (id, url_id, client_id, created_at)
VALUES (?, ?, ?, ?);

-- name: ViewCountLookup :one
SELECT * FROM view_counts WHERE url_id = ?;

-- name: ViewCountInsert :exec
INSERT INTO view_counts (id, url_id, count, updated_at)
VALUES (?, ?, 1, ?);

-- name: ViewCountUpdate :exec
UPDATE view_counts SET count = count + 1, updated_at = ? WHERE url_id = ?;

-- name: UrlLookupByUrl :one
SELECT * FROM urls WHERE url = ?;

-- name: UrlInsert :exec
INSERT INTO urls (id, url, created_at)
VALUES (?, ?, ?);
