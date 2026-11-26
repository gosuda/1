-- name: BulkCountsByUrls :many
SELECT
  u.url AS url,
  COALESCE(vc.count, 0) AS view_count,
  COALESCE(lc.count, 0) AS like_count
FROM urls u
LEFT JOIN view_counts vc ON vc.url_id = u.id
LEFT JOIN like_counts lc ON lc.url_id = u.id
WHERE u.url IN (sqlc.slice('urls'));
