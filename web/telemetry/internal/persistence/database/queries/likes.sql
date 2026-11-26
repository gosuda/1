-- name: LikeInsert :exec
INSERT INTO likes (id, url_id, client_id, created_at)
VALUES (?, ?, ?, ?);

-- name: LikeCountInsert :exec
INSERT INTO like_counts (id, url_id, count, updated_at)
VALUES (?, ?, 1, ?);

-- name: LikeCountLookup :one
SELECT id, url_id, count, updated_at FROM like_counts WHERE url_id = ?;

-- name: LikeCountUpdate :exec
UPDATE like_counts SET count = count + 1, updated_at = ? WHERE url_id = ?;