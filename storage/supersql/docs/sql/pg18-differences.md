# PostgreSQL 18.4 Differences & Extensions

SuperSQL targets **PostgreSQL 18.4** (`server_version_num = 180004`) as its SQL baseline. Every intentional departure falls into one of three categories:

1. **SuperSQL Extensions (Supersets)** — capabilities added for embedded, columnar, or time-travel workloads.
2. **Behavioral Differences** — subtle runtime differences compared to stock PostgreSQL 18.4.
3. **Deterministic Exclusions (`0A000`)** — server-only, heap-specific, or C-extension features that always fail with SQLSTATE `0A000`.

---

## 1. SuperSQL Extensions (Supersets)

| Extension | Syntax / API | Behavior |
| :--- | :--- | :--- |
| **Time-Travel Queries** | `SELECT ... FROM t AS OF SYSTEM TIME '<timestamptz>'` | Evaluates a read-only query against a certified historical MVCC snapshot within `OpenOptions.HistoryRetention` (default 24h). |
| **Isolation Auto-Escalation** | `SET supersql.compat_isolation = 'on' \| 'off'` | Default `'on'` transparently escalates `READ COMMITTED` / `REPEATABLE READ` / `READ UNCOMMITTED` requests to `SERIALIZABLE` so ORMs and drivers work out of the box. Set `'off'` for strict `0A000` rejection. |
| **PL/pgSQL `%TYPE` in Signatures** | `CREATE FUNCTION f(x accounts.balance%TYPE) ...` | Supported directly in function/procedure parameter and return signatures as a superset of upstream `libpg_query`. |
| **Pure-Go Predefined ICU Collations** | `COLLATE "en-US-x-icu"`, `"ko-KR-x-icu"`, `"en-US-u-ks-level2"`, `"en-US-u-kn-true"` | Works identically across Linux/macOS/Windows with `CGO_ENABLED=0` and zero system `libicu` installation. |
| **Custom Go `TextSearchAnalyzer`** | `OpenOptions.TextSearchAnalyzers` / `Catalog.RegisterTextSearchAnalyzer` | Registers Go tokenizers/morphological analyzers as PostgreSQL `regconfig` names usable in `to_tsvector`, `to_tsquery`, and GIN `tsvector_ops` indexes (alongside 29 built-in Snowball languages). |
| **Optional In-Process Wire Listener** | `pgwire.NewServerFromCatalog(db, cfg)` | Exposes an embedded database over PostgreSQL v3.0 TCP without running a separate database cluster. |

---

## 2. Behavioral Differences from PostgreSQL 18.4

### Serializable-Only Isolation (Strict 2PL / PCC)
- SuperSQL executes all read-write transactions under **Serializable** isolation using Strict 2PL / Pessimistic Concurrency Control over certified MVCC snapshots.
- Unlike PostgreSQL SSI (which can abort with `40001` at commit time on read-write cycles), SuperSQL uses pessimistic row/key-span locks + a shared $O(V + E)$ `WaitGraph` (deadlocks abort with `40P01`).
- Uncommitted DDL or an open transaction holding catalog shared locks can serialize concurrent DDL in another session. Autocommit reads run lock-free against a snapshot. Use `SET lock_timeout`, `SET statement_timeout`, or `SET idle_in_transaction_session_timeout` to bound waits.

### RE2 Regular Expressions
- All regex operators (`~`, `~*`, `!~`, `!~*`, `regexp_match`, `regexp_replace`, `regexp_split_to_table`, etc.) run on Go's **RE2** engine to guarantee linear-time evaluation and prevent ReDoS attacks.
- Backreferences (`\1`) and lookahead/lookbehind assertions (`(?=...)`, `(?<=...)`) are not supported by RE2 and return a regex syntax error (`2201B`).

### Background Maintenance Replaces `VACUUM` / `CLUSTER`
- NeoSepia is a columnar PAX v2 LSM engine (`activeTier` / `inactiveTier`), not a PostgreSQL heap.
- Space reclamation, dead-version compaction, history pruning, and **Auto-`ANALYZE`** run automatically in the background (`OpenOptions.Maintenance`).
- Heap-specific system columns (`ctid`, `xmin`, `xmax`, `cmin`, `cmax`) and commands (`VACUUM`, `CLUSTER`, `CHECKPOINT`) return `0A000`.

### `EXPLAIN (ANALYZE, BUFFERS)`
- `BUFFERS` reports NeoSepia shared block-cache hits/reads and 8 KiB temp spill read/write blocks across `TEXT`, `JSON`, `YAML`, and `XML` formats.
- `EXPLAIN (WAL)` and `EXPLAIN (MEMORY)` are excluded (`0A000`).

### `GIN` Index Scope
- `CREATE INDEX ... USING gin` supports single-column indexes on `jsonb` (`jsonb_ops`, `jsonb_path_ops`), arrays `T[]` (`array_ops`), and `tsvector` (`tsvector_ops`), with optional partial `WHERE` predicates.
- Multi-column GIN, expression GIN, `UNIQUE`, `INCLUDE`, explicit `ASC`/`DESC`, and custom operator classes (`opclassopts`) return `0A000`.

### `NOTIFY` / `LISTEN`
- `NOTIFY` and `pg_notify()` are transactional (staged per transaction, truncated on `ROLLBACK TO SAVEPOINT` and PL/pgSQL `EXCEPTION`, published atomically on `COMMIT`, discarded on `ROLLBACK`).
- Channel names are limited to 63 bytes (`22023`) and payloads to 8,000 bytes (`54000`), matching PostgreSQL.
- `pg_notification_queue_usage()` is excluded (`0A000`).

---

## 3. Deterministic `0A000` Exclusions

Every excluded PostgreSQL feature fails deterministically with SQLSTATE `0A000` (`feature_not_supported`) and a machine-readable `FeatureID`. For the complete auto-generated catalog, see [`docs/reference/sql-compatibility.md`](../reference/sql-compatibility.md).

<!-- doctest:expect-error 0A000 -->
```sql
VACUUM;
```

<!-- doctest:expect-error 0A000 -->
```sql
CREATE EXTENSION hstore;
```

<!-- doctest:expect-error 0A000 -->
```sql
CREATE TABLE t_gist (r int4range);
CREATE INDEX idx_gist ON t_gist USING gist (r);
```

<!-- doctest:expect-error 0A000 -->
```sql
PREPARE TRANSACTION 'tx_2pc';
```
