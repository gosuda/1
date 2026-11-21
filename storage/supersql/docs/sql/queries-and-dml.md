# Queries (DQL) & Data Manipulation (DML)

SuperSQL supports standard PostgreSQL 18 query and mutation syntax, executing reads through a Cascades cost-based optimizer and 1,024-row vectorized pipelines.

---

## 1. `SELECT`, CTEs, Window Functions & Grouping Sets

### Synopsis
```sql
[ WITH [ RECURSIVE ] cte_name [ (col, ...) ] AS [ [ NOT ] MATERIALIZED ] ( query ) [, ...] ]
SELECT [ ALL | DISTINCT [ ON ( expr [, ...] ) ] ]
    select_list
[ FROM from_item [, ...] ]
[ WHERE condition ]
[ GROUP BY { expr | GROUPING SETS (...) | ROLLUP (...) | CUBE (...) } [, ...] ]
[ HAVING condition ]
[ WINDOW window_name AS ( window_definition ) [, ...] ]
[ { UNION | INTERSECT | EXCEPT } [ ALL | DISTINCT ] select ]
[ ORDER BY expr [ ASC | DESC ] [ NULLS { FIRST | LAST } ] [, ...] ]
[ LIMIT { count | ALL } ] [ OFFSET start ]
[ FETCH { FIRST | NEXT } [ count ] { ROW | ROWS } { ONLY | WITH TIES } ]
[ FOR { UPDATE | NO KEY UPDATE | SHARE | KEY SHARE } [ OF table [, ...] ] [ NOWAIT | SKIP LOCKED ] ]
```

### Example: Recursive CTE, Window Functions & `ROLLUP`

<!-- doctest -->
```sql
CREATE TABLE sales (
    id INT PRIMARY KEY,
    region TEXT NOT NULL,
    category TEXT NOT NULL,
    amount INT NOT NULL
);

INSERT INTO sales VALUES
    (1, 'apac', 'books', 120),
    (2, 'apac', 'books', 80),
    (3, 'apac', 'games', 300),
    (4, 'emea', 'books', 150),
    (5, 'emea', 'games', 250);

-- Window ranking per region
SELECT region, category, amount,
       sum(amount) OVER (PARTITION BY region ORDER BY amount DESC, id) AS running_total,
       dense_rank() OVER (PARTITION BY region ORDER BY amount DESC) AS rnk
FROM sales
ORDER BY region, rnk, id;

-- ROLLUP subtotals
SELECT coalesce(region, 'ALL') AS region,
       coalesce(category, 'ALL') AS category,
       sum(amount) AS total
FROM sales
GROUP BY ROLLUP (region, category)
ORDER BY region, category;

-- Recursive CTE
WITH RECURSIVE seq(n) AS (
    SELECT 1
    UNION ALL
    SELECT n + 1 FROM seq WHERE n < 5
)
SELECT sum(n) FROM seq;
```

---

## 2. `INSERT` & `ON CONFLICT` (Upsert)

### Synopsis
```sql
INSERT INTO table_name [ AS alias ] [ ( col [, ...] ) ]
{ DEFAULT VALUES | VALUES ( expr [, ...] ) [, ...] | query }
[ ON CONFLICT [ ( conflict_col [, ...] ) [ WHERE index_predicate ] | ON CONSTRAINT constraint_name ]
  DO { NOTHING | UPDATE SET col = expr [, ...] [ WHERE condition ] } ]
[ RETURNING * | output_expr [ [ AS ] output_name ] [, ...] ]
```

<!-- doctest -->
```sql
CREATE TABLE kv (
    k TEXT PRIMARY KEY,
    v INT NOT NULL,
    hits INT NOT NULL DEFAULT 1
);

INSERT INTO kv (k, v) VALUES ('alpha', 10), ('beta', 20)
ON CONFLICT (k) DO UPDATE
    SET v = EXCLUDED.v,
        hits = kv.hits + 1
RETURNING k, v, hits;

INSERT INTO kv (k, v) VALUES ('alpha', 99)
ON CONFLICT (k) DO UPDATE
    SET v = EXCLUDED.v,
        hits = kv.hits + 1
RETURNING k, v, hits;
```

---

## 3. `UPDATE`, `DELETE` & `MERGE`

### Synopsis
```sql
UPDATE table_name [ [ AS ] alias ]
SET col = expr [, ...]
[ FROM from_item [, ...] ]
[ WHERE condition ]
[ RETURNING * | output_expr [, ...] ]

DELETE FROM table_name [ [ AS ] alias ]
[ USING from_item [, ...] ]
[ WHERE condition ]
[ RETURNING * | output_expr [, ...] ]

MERGE INTO target_table [ [ AS ] target_alias ]
USING data_source [ [ AS ] source_alias ]
ON join_condition
{ WHEN MATCHED [ AND condition ] THEN { UPDATE SET ... | DELETE | DO NOTHING }
| WHEN NOT MATCHED [ AND condition ] THEN { INSERT ... | DO NOTHING } } ...
```

<!-- doctest -->
```sql
CREATE TABLE inventory (
    sku TEXT PRIMARY KEY,
    qty INT NOT NULL
);

INSERT INTO inventory VALUES ('A1', 10), ('B2', 5);

MERGE INTO inventory AS dst
USING (VALUES ('A1', 3), ('C3', 7)) AS src(sku, delta)
ON dst.sku = src.sku
WHEN MATCHED THEN
    UPDATE SET qty = dst.qty + src.delta
WHEN NOT MATCHED THEN
    INSERT (sku, qty) VALUES (src.sku, src.delta);

SELECT sku, qty FROM inventory ORDER BY sku;
```

---

## 4. Row-Level Locking (`FOR UPDATE` / `SKIP LOCKED` / `NOWAIT`)

Row locks participate in SuperSQL's Strict 2PL sharded lock registry and shared `WaitGraph` (deadlocks raise `40P01`, `NOWAIT` contention raises `55P03`).

<!-- doctest -->
```sql
CREATE TABLE jobs (
    id INT PRIMARY KEY,
    status TEXT NOT NULL
);

INSERT INTO jobs VALUES (1, 'queued'), (2, 'queued');

BEGIN;
SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED;
UPDATE jobs SET status = 'running' WHERE id = 1;
COMMIT;
```

---

## 5. Streaming `COPY` (`FROM STDIN` / `TO STDOUT`)

SuperSQL supports PostgreSQL `COPY` over client streams (`text`, `csv`, `binary`) via `database/sql` / `supersql.Catalog.CopyFrom` / `CopyTo` and `supersql/pgx.CopyFrom`.

- **Supported**: `COPY table [(cols)] FROM STDIN [WITH (FORMAT text|csv|binary, DELIMITER ..., HEADER ..., NULL ...)]` and `COPY { table | (query) } TO STDOUT [...]`.
- **Excluded (`0A000`)**: Host filesystem paths (`COPY t FROM '/path'`) and shell commands (`COPY t TO PROGRAM '...'`).

---

## 6. Time-Travel Queries (`AS OF SYSTEM TIME`)

SuperSQL extends PostgreSQL `SELECT` and `SET TRANSACTION` with certified-timestamp historical reads within `OpenOptions.HistoryRetention` (default `24h`).

### Synopsis
```sql
SELECT ... FROM table_name AS OF SYSTEM TIME '<timestamptz>' [ WHERE ... ]

BEGIN READ ONLY;
SET TRANSACTION AS OF SYSTEM TIME '<timestamptz>';
SELECT ...;
COMMIT;
```

- Historical reads run lock-free against the pinned MVCC snapshot verified by a certified `ReadFence`.
- Timestamps older than the retained history floor fail cleanly instead of returning silently truncated history.
- Use `OpenOptions{HistoricalReadOnly: true}` to inspect historical snapshots even without an active certified write clock.
