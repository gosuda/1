# DDL & Indexes (B-tree and GIN)

SuperSQL persists all catalog metadata (schemas, tables, constraints, B-tree and GIN indexes, views, sequences, types, collations, roles, ACLs, and RLS policies) inside NeoSepia as **versioned MVCC descriptors**. DDL executes transactionally and rolls back cleanly on `ROLLBACK` or `ROLLBACK TO SAVEPOINT`.

---

## 1. Tables, Constraints, Views & Sequences

### Supported Table Features
- Primary keys, composite `UNIQUE` (including `NULLS NOT DISTINCT`), `FOREIGN KEY` (`ON DELETE/UPDATE CASCADE | SET NULL | SET DEFAULT | RESTRICT`, `DEFERRABLE INITIALLY DEFERRED`), `CHECK`, `DEFAULT`, `NOT NULL`
- Identity columns (`GENERATED { ALWAYS | BY DEFAULT } AS IDENTITY`) and stored generated columns (`GENERATED ALWAYS AS (expr) STORED`)
- `CREATE TABLE ... (LIKE other INCLUDING ALL)`, `CREATE TABLE AS`, `CREATE TEMP TABLE`, `ALTER TABLE` (`ADD/DROP/RENAME COLUMN`, `ALTER COLUMN TYPE/DEFAULT/NOT NULL`, `ADD/DROP CONSTRAINT`, `OWNER TO`, `ENABLE/FORCE ROW LEVEL SECURITY`)
- `CREATE [OR REPLACE] VIEW` (including `WITH (security_barrier = true, check_option = local|cascaded)`), `CREATE / REFRESH MATERIALIZED VIEW`
- `CREATE / ALTER / DROP SEQUENCE`

### Excluded Physical Clauses (`0A000`)
- `UNLOGGED`, `TABLESPACE`, `USING heap`, `WITH (fillfactor = ...)`, column `COMPRESSION` / `STORAGE`

<!-- doctest -->
```sql
CREATE SCHEMA app;

CREATE TABLE app.orders (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    customer_email TEXT NOT NULL,
    unit_price NUMERIC(10, 2) NOT NULL CHECK (unit_price >= 0),
    qty INT NOT NULL CHECK (qty > 0),
    total NUMERIC(12, 2) GENERATED ALWAYS AS (unit_price * qty) STORED,
    status TEXT NOT NULL DEFAULT 'open'
);

CREATE VIEW app.open_orders WITH (security_barrier = true) AS
    SELECT id, customer_email, total
    FROM app.orders
    WHERE status = 'open';

INSERT INTO app.orders (customer_email, unit_price, qty)
VALUES ('alice@example.com', 19.50, 3);

SELECT id, customer_email, total FROM app.orders;
SELECT customer_email, total FROM app.open_orders;
```

---

## 2. Ordered B-tree Indexes (`USING btree`)

### Synopsis
```sql
CREATE [ UNIQUE ] INDEX [ CONCURRENTLY ] [ IF NOT EXISTS ] [ name ]
ON table_name [ USING btree ] (
    { column_name | ( expression ) } [ COLLATE collation ] [ ASC | DESC ] [ NULLS { FIRST | LAST } ] [, ...]
)
[ INCLUDE ( column_name [, ...] ) ]
[ NULLS [ NOT ] DISTINCT ]
[ WHERE predicate ]
```

- Supports covering indexes (`INCLUDE`), partial indexes (`WHERE`), expression indexes, reverse scans (`ORDER BY col DESC` without an extra sort), and ICU/custom collations.

<!-- doctest -->
```sql
CREATE TABLE accounts (
    id INT PRIMARY KEY,
    email TEXT NOT NULL,
    balance INT NOT NULL,
    active BOOLEAN NOT NULL DEFAULT true
);

CREATE UNIQUE INDEX idx_accounts_email_lower
    ON accounts (lower(email))
    INCLUDE (balance)
    WHERE active;

INSERT INTO accounts VALUES (1, 'Alice@Example.com', 500, true);

SELECT balance FROM accounts WHERE lower(email) = 'alice@example.com' AND active;
```

---

## 3. GIN Inverted Indexes (`USING gin`)

SuperSQL implements pure-Go **GIN inverted indexes** backed by `(token, base_pk)` posting lists inside NeoSepia with full MVCC snapshot visibility and residual recheck.

### Synopsis
```sql
CREATE INDEX [ IF NOT EXISTS ] [ name ]
ON table_name USING gin ( column_name [ operator_class ] )
[ WHERE predicate ]
```

### Supported Types, Operator Classes & Indexable Operators

| Column Type | Operator Class | Default? | Indexable Operators | Notes |
| :--- | :--- | :--- | :--- | :--- |
| `jsonb` | `jsonb_ops` | Yes | `@>`, `<@`, `?`, `?\|`, `?&` | Indexes object keys, path/value pairs, top-level strings, and direct string elements of top-level arrays |
| `jsonb` | `jsonb_path_ops` | No | `@>`, `<@` | Compact path-value postings only; `?`/`?\|`/`?&` do not use a `jsonb_path_ops` index |
| `T[]` (arrays) | `array_ops` | Yes | `@>`, `<@`, `&&` | Supports `int[]`, `text[]`, `uuid[]`, etc. (`NULL` elements skipped) |
| `tsvector` | `tsvector_ops` | Yes | `@@` | Supports `tsvector @@ tsquery` and `tsquery @@ tsvector` (built-in `'simple'` + 29 Snowball languages, plus custom Go `TextSearchAnalyzer` — see [`docs/guides/custom-text-search.md`](../guides/custom-text-search.md)) |

### GIN Constraints & Exclusions (`0A000`)
- Single-column only; `UNIQUE`, `INCLUDE`, expression keys, `NULLS NOT DISTINCT`, explicit `ASC`/`DESC`, and custom `opclassopts` are rejected with `0A000`.
- `GiST`, `SP-GiST`, `BRIN`, and `Hash` access methods are excluded (`0A000`).
- Queries that yield no selective tokens (such as `doc @> '{}'::jsonb`, negated `tsquery` like `'!cat'`, or prefix `'run:*'`) fall back safely to a full scan with identical results.

### Example: JSONB, Array & Full-Text GIN Indexes

<!-- doctest -->
```sql
CREATE TABLE articles (
    id INT PRIMARY KEY,
    meta JSONB NOT NULL,
    tags TEXT[] NOT NULL,
    tsv TSVECTOR NOT NULL
);

CREATE INDEX idx_articles_meta_gin ON articles USING gin (meta);
CREATE INDEX idx_articles_meta_path ON articles USING gin (meta jsonb_path_ops);
CREATE INDEX idx_articles_tags_gin ON articles USING gin (tags);
CREATE INDEX idx_articles_tsv_gin  ON articles USING gin (tsv);

INSERT INTO articles VALUES
    (1, '{"author":"alice","featured":true,"labels":["go","db"]}'::jsonb,
        ARRAY['golang', 'postgres'],
        to_tsvector('english', 'Pure Go embedded PostgreSQL database engine')),
    (2, '["go","sqlite"]'::jsonb,
        ARRAY['rust', 'storage'],
        to_tsvector('english', 'Columnar storage with Bloom filters'));

-- JSONB containment (@>) and key/top-level-array existence (?)
SELECT id FROM articles WHERE meta @> '{"author":"alice"}'::jsonb;
SELECT id FROM articles WHERE meta ? 'go' ORDER BY id;

-- Array overlap (&&) and containment (@>)
SELECT id FROM articles WHERE tags && ARRAY['golang', 'zig'];

-- Full-text search (@@) using built-in 'english' Snowball configuration
SELECT id FROM articles WHERE tsv @@ to_tsquery('english', 'embed & databas');

-- Confirm GIN Index Scan in EXPLAIN
EXPLAIN SELECT id FROM articles WHERE tags && ARRAY['golang'];
```

---

## 4. `ANALYZE` & `REINDEX`

- `ANALYZE [ table_name [(col, ...)] ]`: Collects row counts, NDV, null fractions, MCVs, and histograms into the statistics store. Background Auto-`ANALYZE` also triggers automatically as rows mutate.
- `REINDEX { TABLE | INDEX } [ CONCURRENTLY ] name`: Rebuilds B-tree or GIN index generations online (`REINDEX DATABASE / SCHEMA / SYSTEM` are excluded with `0A000`).
