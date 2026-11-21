# SuperSQL

**SuperSQL** is an embedded relational database engine written in pure Go (`CGO_ENABLED=0`). It combines PostgreSQL 18 SQL and PL/pgSQL execution with **NeoSepia**, a local columnar storage engine featuring 64 KB PAX v2 blocks, Bloom filters, zone maps, dual-tier compaction, and certified-timestamp serializable MVCC.

SuperSQL runs in-process over a local directory with no external server, no CGo, and no C library dependencies—serving OLTP point lookups, serializable multi-statement transactions, time-travel queries, and vectorized analytical scans from one unified engine.

> For internal design, NeoSepia on-disk layouts, concurrency control, and query pipeline details, see **[`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)** and the generated specs under **[`docs/reference/`](docs/reference/)**.

---

## Installation

`github.com/gosuda/supersql-alpha` is currently hosted in a private repository:

```sh
GOPRIVATE=github.com/gosuda/supersql-alpha go get github.com/gosuda/supersql-alpha@<commit-sha>
```

Set `GOPRIVATE` (or `GONOSUMDB`) so the Go toolchain fetches the module directly from Git, and ensure Git credentials (`ssh` or `GITHUB_TOKEN`) are configured for `github.com`.

- **Requirements**: Go `1.27.3` or later (`go 1.27.3` in [`go.mod`](go.mod)), pure Go (`CGO_ENABLED=0`).
- **Runtime dependencies**: Production packages have zero `jackc/pgx` runtime dependency (`github.com/jackc/pgx/v5` in `go.mod` is imported only by integration tests).

---

## Quick Start

### Standard `database/sql` API

SuperSQL does not register a global DSN driver (`sql.Open("supersql", ...)` is rejected because a clock cannot be passed in a connection string). Construct a `Connector` with `OpenConnector` and pass it to `sql.OpenDB`:

- Use **`localclock.New()`** (`github.com/gosuda/supersql-alpha/clock/local`) for standalone single-process deployments, or pass a **`clock/gstime`** provider for externally synchronized certified time.
- Tune storage resource envelopes via `OpenOptions.Compaction` (`CompactionConfig`; zero fields inherit `DefaultCompactionConfig()`).
- SQL errors unwrap to `*supersql.Error` with PostgreSQL SQLSTATE codes.
- On shutdown, close `sql.DB` (which also closes the `Connector`), then any external clock provider.

<!-- docgen:begin readme-example -->

```go

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	supersql "github.com/gosuda/supersql-alpha"
	localclock "github.com/gosuda/supersql-alpha/clock/local"
)

// runReadme is the README quick start: open a connector with a local clock,
// wrap it in database/sql, and run DDL, a transaction, and a query.
func runReadme(ctx context.Context, dir string, out io.Writer) error {
	connector, err := supersql.OpenConnector(ctx, supersql.OpenOptions{
		Directory: dir,
		Clock:     localclock.New(),
	})
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector) // db.Close also closes the connector
	defer db.Close()
	if _, err := db.ExecContext(ctx, "CREATE TABLE accounts (id INT PRIMARY KEY, name VARCHAR, balance INT)"); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, "CREATE INDEX idx_acc_name ON accounts(name)"); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, row := range []string{"(1, 'Alice', 50000)", "(2, 'Bob', 85000)"} {
		if _, err := tx.ExecContext(ctx, "INSERT INTO accounts (id, name, balance) VALUES "+row); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, "SELECT id, name, balance FROM accounts WHERE name = 'Alice'")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, balance int64
		var name string
		if err := rows.Scan(&id, &name, &balance); err != nil {
			return err
		}
		fmt.Fprintf(out, "Account: %d | %s | %d\n", id, name, balance)
	}
	return rows.Err()
}
```

<!-- docgen:end readme-example -->

### Cleanroom `pgx/v5`-Compatible API (`pgx`)

Applications written for `pgx/v5` and `pgxpool` can import `github.com/gosuda/supersql-alpha/pgx` directly. It has zero external `jackc/pgx` dependencies and executes in-process:

```go
pool, err := pgx.OpenPool(ctx, supersql.OpenOptions{
	Directory: dir,
	Clock:     localclock.New(),
})
if err != nil {
	return err
}
defer pool.Close()

_, err = pool.Exec(ctx,
	`INSERT INTO accounts (id, name, balance, tags) VALUES (@id, @name, @balance, @tags)`,
	pgx.NamedArgs{"id": 1, "name": "Alice", "balance": 50000, "tags": []string{"vip", "founder"}},
)

rows, err := pool.Query(ctx,
	`SELECT id, name, balance, tags FROM accounts WHERE id = ANY(@ids)`,
	pgx.NamedArgs{"ids": []int64{1, 2}},
)
accounts, err := pgx.CollectRows(rows, pgx.RowToStructByName[Account])
```

Included `pgx` capabilities:
- `Pool`, `PoolConfig` (`BeforeConnect`, `AfterConnect`, `BeforeAcquire`, `AfterRelease` hooks), `PoolConn`, `Conn` (`Prepare`, `Deallocate`, `DeallocateAll`, `WaitForNotification`), `Tx` (nested `tx.Begin` via `SAVEPOINT`, `DeferrableMode`), `BeginFunc` / `BeginTxFunc`
- `*pgx.PgError` (matching `pgconn.PgError` fields and unwrapping to `*supersql.Error`) and `FieldDescription` with PostgreSQL type OIDs
- `NamedArgs` (`@param`), Go slices (including nullable `[]*T` / `[]pgx.Text`, multi-dimensional `[][]T`, `[]float32`, `[]int8`), UUID (`[16]byte`), JSONB (`map[string]any`, `json.RawMessage`), and nullable `pgtype` wrappers (`Text`, `Int2`, `Int4`, `Int8`, `Float4`, `Float8`, `Bool`, `UUID`, `Numeric`, `Timestamp`, `Timestamptz`, `Date`, `JSONB`)
- Transactional `Batch` / `SendBatch`, `CopyFrom` (`CopyFromRows`, `CopyFromSlice`), and generic collectors (`CollectRows`, `CollectOneRow`, `CollectExactlyOneRow`, `ForEachRow`, `RowToStructByName`, `RowToStructByPos`, `RowToMap`, etc.)

### Optional PostgreSQL Wire Protocol Listener (`pgwire`)

When you want to inspect or query an embedded SuperSQL database with `psql`, GUI clients (DBeaver, DataGrip), or ecosystem drivers (`jackc/pgx/v5`, GORM, Ent, Bun, `sqlc`), attach an opt-in PostgreSQL v3.0 TCP listener over an existing `*supersql.Catalog` or `*supersql.Connector`:

```go
srv, err := pgwire.NewServerFromCatalog(db, pgwire.Config{
	Password: "dev-password", // or AuthFunc / TLSConfig
})
if err != nil {
	return err
}
defer srv.Close()
go srv.ListenAndServe(ctx, "127.0.0.1:5432")
```

Supports Simple & Extended Query (`Parse`/`Bind`/`Describe`/`Execute`/`PortalSuspended`), text & binary parameter/result codecs, `CancelRequest`, TLS (`SSLRequest`), authentication callbacks, `MaxConnections`, and asynchronous `'A'` `NotificationResponse` delivery for `LISTEN`/`NOTIFY`.

---

## Feature Highlights

- **Queries (DQL)**: `UNION`/`INTERSECT`/`EXCEPT`, `INNER`/`LEFT`/`RIGHT`/`FULL`/`CROSS`/`LATERAL` joins, `WITH` / `WITH RECURSIVE` CTEs, window functions, `GROUPING SETS`/`ROLLUP`/`CUBE`, correlated subqueries, row locking (`FOR UPDATE` / `FOR SHARE` / `NOWAIT` / `SKIP LOCKED`), and time-travel queries (`AS OF SYSTEM TIME <timestamptz>`).
- **Mutations (DML)**: Multi-row `INSERT`, `INSERT ... ON CONFLICT DO NOTHING / DO UPDATE`, `UPDATE ... FROM`, `DELETE ... USING`, `MERGE INTO`, `RETURNING`, and streaming `COPY ... FROM STDIN` / `TO STDOUT`.
- **Indexes (B-tree & GIN)**: Composite primary/unique/foreign keys, `CHECK` constraints, secondary/partial/`INCLUDE` covering B-tree indexes, and **GIN inverted indexes** (`USING gin`) for `jsonb` (`jsonb_ops`, `jsonb_path_ops`: `@>`, `?`, `?|`, `?&`), arrays (`array_ops`: `@>`, `&&`), and full-text search `tsvector` (`tsvector_ops`: `@@`).
- **Full-Text Search & Custom `TextSearchAnalyzer`**: `'simple'` plus all 29 built-in pure-Go PostgreSQL Snowball language configurations (`'english'`, `'french'`, `'german'`, `'spanish'`, etc.), plus Go-native custom analyzer registration (`OpenOptions.TextSearchAnalyzers` / `Catalog.RegisterTextSearchAnalyzer`) for Korean/Japanese morphological analyzers, n-gram tokenizers, or domain stemmers.
- **Schema, Procedural & Pure-Go ICU Collations**: Views, materialized views, sequences, domains, enums, composite types, ranges, triggers, functions, procedures, `DO $$ ... $$` blocks in SQL and `PL/pgSQL`, and **pure-Go ICU/locale collations** (`"en-US-x-icu"`, `"ko-KR-x-icu"`, `"ja-JP-x-icu"`, case-insensitive `-u-ks-level2`, numeric `-u-kn-true`, plus `CREATE / DROP COLLATION`).
- **Multi-Tenant Security, RBAC & Session GUC**:
  - **Object-Level Privileges & Ownership**: `GRANT` / `REVOKE` (`WITH GRANT OPTION`, `ALL TABLES/SEQUENCES/FUNCTIONS IN SCHEMA`, `PUBLIC`), `ALTER TABLE/VIEW/SEQUENCE/SCHEMA/FUNCTION ... OWNER TO`, `CREATE SCHEMA ... AUTHORIZATION`, and `SECURITY DEFINER` routines with safe caller-role restoration.
  - **Native MVCC Row-Level Security (RLS)**: `ENABLE / DISABLE / FORCE ROW LEVEL SECURITY` and `CREATE / ALTER / DROP POLICY` (`PERMISSIVE` / `RESTRICTIVE`, `USING`, `WITH CHECK`), persisted transactionally in catalog descriptors and exposed via `pg_policies` and `pg_roles`.
  - **Roles & Session Settings**: `CREATE / ALTER / DROP ROLE [BYPASSRLS | SUPERUSER]`, `GRANT / REVOKE <role>`, `SET [LOCAL] ROLE`, and custom qualified GUCs (`SET [LOCAL] app.tenant_id = '...'`, `current_setting`, `set_config`).
  - **Serializable by Default**: Weaker isolation requests escalate automatically to `SERIALIZABLE` (`SET supersql.compat_isolation = 'off'` switches to strict `0A000` rejection).
- **In-Process `LISTEN` / `NOTIFY`**: Transactional `LISTEN`, `NOTIFY`, `UNLISTEN` (including savepoint rollback suppression and commit-only fanout), `pg_notify()`, `pg_listening_channels()`, Go-native `Catalog.Subscribe(...)`, `pgx.Conn.WaitForNotification`, and `pgwire` `'A'` push frames.
- **Auto-`ANALYZE` & Observability**: Background Auto-`ANALYZE` triggered by per-table mutation thresholds (`MaintenanceConfig`), `EXPLAIN (ANALYZE, BUFFERS)` (TEXT/JSON/YAML/XML), `OpenOptions.Observer` / `OnSlowQuery`, and live `Catalog.Metrics(ctx)`.
- **Encrypted & Incremental Backups, WAL Archiving & PITR**: Online hot backups (`Catalog.Backup`), incremental backup chains (`CreateIncrementalBackup` / `RestoreIncremental`), continuous WAL archiving (`OpenOptions.WALArchiveDir` / `EnableWALArchive`), Point-in-Time Recovery (`RestorePITR` / `RestorePITRFromChain`), and optional pure-Go AES-256-GCM at-rest & backup encryption (`OpenOptions.EncryptionKey` / `BackupOptions.EncryptionKey`).
- **Safe Regex (RE2)**: Regular expression operators use Go's RE2 engine (linear-time evaluation; backreferences and lookarounds are rejected).

---

## Intentional Departures & Known Limitations

SuperSQL omits cluster-wide and heap-specific PostgreSQL features; every excluded feature fails deterministically with SQLSTATE `0A000` (see [`docs/reference/sql-compatibility.md`](docs/reference/sql-compatibility.md)):

- **Embedded single-process storage**: The storage directory is owned exclusively by one process (`.lock`); multi-node clustering, `pg_hba.conf`, and `CREATE DATABASE` / `TABLESPACE` are excluded (use the optional `pgwire` package when external TCP clients need to connect to the host process).
- **No heap maintenance commands**: `VACUUM`, `FREEZE`, `CLUSTER`, `ctid`, and `xmin` are rejected (`0A000`); background compaction, Auto-`ANALYZE`, and retention run automatically inside NeoSepia.
- **No host filesystem / C extensions / XML**: `pg_read_file`, server-side `COPY FROM '/path'` or `PROGRAM`, C extensions, untrusted languages, and XML functions are excluded.
- **Supported index access methods**: Primary storage uses PAX v2 SSTables with Bloom filters and zone maps; secondary indexes support ordered **B-tree** runs and **GIN** inverted posting lists (`GiST`, `SP-GiST`, `BRIN`, and `Hash` are excluded with `0A000`).
- **Catalog lock serialization across uncommitted DDL**: Statements in transaction blocks and writes acquire shared locks on catalog collections until transaction end, so uncommitted DDL or an idle open transaction can block concurrent DDL in another session. Autocommit reads run as snapshot reads without waiting. Bound wait times with `SET lock_timeout` (`55P03`), `SET statement_timeout` (`57014`), `SET idle_in_transaction_session_timeout` (`25P03`), or Go context deadlines.
- **RE2 regex & `%TYPE` superset**: Regular expressions use RE2 rather than Tcl ARE; PL/pgSQL `%TYPE` in routine signatures is supported as a superset of upstream `libpg_query`.

---

## Documentation

| Document | Contents |
| :--- | :--- |
| **[`docs/sql/`](docs/sql/README.md)** | PostgreSQL 18 SQL syntax guide, statement support matrix, [PG 18 differences](docs/sql/pg18-differences.md), GIN indexes, RBAC/RLS, PL/pgSQL, ICU collations, `LISTEN`/`NOTIFY`, and `EXPLAIN (ANALYZE, BUFFERS)` (doctest-verified) |
| **[`docs/guides/pgx-and-orms.md`](docs/guides/pgx-and-orms.md)** | Integration guide for `database/sql`, `supersql/pgx`, `pgwire`, `psql`, `sqlc`, GORM, Ent, and Bun |
| **[`docs/guides/custom-text-search.md`](docs/guides/custom-text-search.md)** | Built-in 29 Snowball FTS configs and custom Go `TextSearchAnalyzer` registration (Korean/Japanese/custom tokenizers + GIN) |
| **[`docs/guides/backup-encryption-pitr.md`](docs/guides/backup-encryption-pitr.md)** | AES-256-GCM at-rest encryption, hot & incremental backup chains, WAL archiving, and PITR |
| **[`docs/guides/observability-tuning.md`](docs/guides/observability-tuning.md)** | `QueryObserver`, `OnSlowQuery`, `Catalog.Metrics`, Auto-`ANALYZE`, and memory/timeout tuning |
| [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) | System diagram, package map, NeoSepia PAX v2 internals, MVCC/locking, optimizer/executor pipeline, resource bounds |
| [`docs/reference/api.md`](docs/reference/api.md) | Generated public Go API reference |
| [`docs/reference/configuration.md`](docs/reference/configuration.md) | Generated `OpenOptions`, `CompactionConfig`, and runtime limits |
| [`docs/reference/sql-compatibility.md`](docs/reference/sql-compatibility.md) | Generated PostgreSQL 18 SQL compatibility & exclusion matrix |
| [`docs/reference/functions.md`](docs/reference/functions.md) | Generated catalog of ~3,400 built-in functions and operators |
| [`docs/reference/sqlstates.md`](docs/reference/sqlstates.md) | Generated SQLSTATE reference |
| [`docs/reference/storage-format.md`](docs/reference/storage-format.md) | Generated on-disk binary format and recovery specification |

---

## License

BSD-3-Clause. See [`LICENSE`](LICENSE), [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md), and [`internal/zpgparse/UPSTREAM`](internal/zpgparse/UPSTREAM).
