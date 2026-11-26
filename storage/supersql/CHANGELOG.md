# Changelog

All notable changes to this project are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project is
pre-1.0: the API is unstable and may change between commits.

## [0.1.0] - Unreleased

First private-repo, release-candidate pass over the embedded PostgreSQL-18
engine (NeoSepia columnar storage + the certified-clock session stack).

### Added

- `database/sql` integration: `OpenConnector` returns a `driver.Connector` you
  wrap in `sql.OpenDB`; `Driver.Open`/`Driver.OpenConnector` reject `sql.Open`
  with `ErrClockRequired` (a certified clock is not expressible in a DSN). The
  README quick start is a tested example (`runReadme`, `ExampleOpenConnector`).
- Background maintenance: `Open` starts a retention/compaction loop by default;
  `OpenOptions.Maintenance.Disabled` opts out. The loop is configured by
  `MaintenanceConfig` (`DefaultMaintenanceInterval`, `MinMaintenanceInterval`); the
  most recent loop failure is surfaced via `Catalog.MaintenanceError`. Recovery
  after `SIGKILL` and a bounded `commits/`/`runs/` footprint are covered by crash
  and soak tests.
- Generated reference documentation under `docs/reference/` (API surface, SQL
  compatibility, function/operator/cast catalog, SQLSTATE map, configuration,
  storage format) produced by `go run ./internal/tools/docgen`.
- `THIRD_PARTY_NOTICES.md` + the `internal/tools/licensecheck` gate; generated
  sources carry `Source:`/`License:` headers.
- GitHub Actions CI: build+vet, short and full test, race detector, a
  four-target fuzz matrix, licensecheck, docgen/generate drift, and a
  `postgres:18.4` differential + official-regression job.

### Changed

- Module path renamed to `github.com/gosuda/supersql-alpha` (see README "Using
  this module" for `GOPRIVATE` consumption).
- `EXPLAIN (… BUFFERS | WAL | MEMORY)` is now rejected with SQLSTATE `0A000`
  `feature is outside embedded_pg18: server_observability` instead of silently
  dropping the sections. `22023` "requires ANALYZE" still takes precedence.
- `CREATE/REFRESH MATERIALIZED VIEW` works end-to-end: a matview is a
  physical relation backed by its own table (populated or `WITH NO DATA`),
  an unpopulated matview rejects scans with `55000`/`REFRESH MATERIALIZED
  VIEW`, `REFRESH [CONCURRENTLY]` picks up changes, and a filtered scan can
  use an index created on the matview. `CREATE TABLE AS` is fixed by the
  same population-path rework.
- First `Open` writes one extra commit for the history-retention policy.

### Fixed

- `ALTER TABLE … ADD UNIQUE / PRIMARY KEY` on a table that already has an
  index no longer panics the process (`assignment to entry in nil map` in
  `promoteTransactionalConstraintIndex`); it now returns a SQLSTATE error. The
  panic aborted every PostgreSQL regression-corpus run at `index_including`.
- The PostgreSQL regression harness runs each schedule file in its own session,
  as pg_regress does, so transactions, settings, prepared statements, cursors
  and temporary objects no longer leak between files. It runs only the pinned
  18.4 reference corpus by default (`SUPERSQL_REGRESS_CORPORA=postgres184,postgres`
  adds the REL_18_STABLE auxiliary corpus, which CI runs nightly), can be narrowed
  with `SUPERSQL_REGRESS_FILES`, logs per-file elapsed time, statements slower
  than 2 s and the live run-file count, and reports background maintenance and
  `Close` errors.
- The columnar `Batch.Validate` row cap now applies to the logical row count
  (`Rows()`), not the physical vector extent (`Length`): a predicate-filtered
  batch can hold `Length` > `maxRows` lanes under a shorter `Selection`.
  Fixes `invalid columnar batch` on filtered scans and materialized-view
  population.

- Idle `Open` no longer republishes `database.state` on a timer (the schema
  index-scheduler poll now uses `ClosedReadView`, removing the per-database
  `fsync` storm); the root test suite dropped from ~524 s to ~208 s.
- `txn.CheckpointSpec` re-checks `commitUnresolved()` inside the sequencer gate,
  closing the TOCTOU against an indeterminate commit (mirrors
  `openStrongSnapshot`).
- `gstime` clock: epoch adoption on `Invalidate`/re-anchor; `WaitUntilAfter`
  returns `ErrClockEpochChanged` for a stale epoch.
- Ispell/Hunspell: an affix marked `ONLYINCOMPOUND` (`repl/flags`) is no longer
  applied to a standalone word, so `ts_lexize('hunspell','bally')` returns NULL
  instead of `{ball}`; compound lexize unchanged.
- `go vet` `lostcancel`/`copylocks` findings resolved; PL/pgSQL `%ROWTYPE` and
  whole-row assignment emit the upstream `PLpgSQL_var`+`PLpgSQL_row` shape.

- Conformance corpus fixes (against the official PostgreSQL 18.4
  `parallel_schedule`):
  - `TestPG184Differential` now runs 114 steps with **0 mismatches** vs a live
    `postgres:18.4` reference — `pg_typeof` `any`-param specialization, numeric
    `format_type` typmod scale (signed 11-bit), spool `MetadataBytes`/modify
    `MaxRecordBytes` budgets, domain→base `::text` coercion + resolver
    propagation, and `ANALYZE` reservoir clamp + `Nullable`/`MissingValue`
    layout emission.
  - Statements against **session-temporary** (`pg_temp`) relations no longer
    fail `55000 read is outside declared transaction dependencies` or
    `40001 catalog descriptor version changed` — binding, dependency
    declaration, search-path, routine-definition loading, and the write policy
    now reach temp objects through the session overlay.
  - `53200 transaction resource limit exceeded` no longer fires at catalog
    scale: the `MemoryBytes/4` read-view allowance is shared by actually-open
    views (a `viewBudget`) instead of being divided by `MaxViews` (which gave a
    16× smaller per-view share that catalog-sized dependency declarations
    overflowed). Long `BEGIN` blocks no longer hit a fixed 2 MiB overlay —
    the transaction memory budget scales to `work_mem`.
  - `CREATE OR REPLACE AGGREGATE` and `DISCARD ALL`/`DISCARD SEQUENCES`
    rejections now carry `aggregate_or_replace`/`session_state` FeatureIDs so
    the conformance manifest classifies them as intentional exclusions.
  - 38 further `0A000` rejections (`write_descriptor_expansion`,
    `datetime_fields`, `encoding_conversion`) carry FeatureIDs; the profile now
    lists 25 features.
  - The declared-type metadata allowance scales with the number of declared
    types instead of being a single descriptor's `MaxBytes`; with a few hundred
    user types every statement used to fail `descriptor exceeds metadata limits`
    or `54000 declared type metadata budget exceeded`.

- Per-statement catalog work no longer scales with catalog size. Declared-type
  definitions are cached per session by catalog content, session overlay and
  descriptor limits (rebuilds per statement 3 → 0), and `Manager.catalogVersion`
  now advances only for commits that staged catalog rows. The per-statement
  dependency-discovery walk and the declared-type routine scan (including the
  one the index key encoder rebuilt per snapshot, the dominant source) are
  cached under the same exact catalog identity: the discovered dependency set is
  reused only while a view still reports the identity it was discovered under,
  and every lock in that set is still acquired on every statement, so a
  concurrent commit always moves the identity and falls back to a full walk.
  With 40 tables + 100 enums, catalog reads per no-DDL `SELECT` dropped
  5516 → 0 and `SELECT` latency 388 → ~50 ms (was 388 ms, ~55 ms on an empty
  catalog); `INSERT` 799 → ~240 ms.

- `DROP TYPE` no longer fails `invalid DROP TYPE name` for every name form; the
  binder now reads the parser's per-object `TypeName` nodes. `IF EXISTS`,
  `CASCADE`/`RESTRICT`, multiple names and `2BP01` under RESTRICT work.
- `ALTER TYPE … ADD VALUE` no longer fails `type dependency namespace limit
  exceeded` once a schema holds more than 64 objects: the dependency walk is
  bounded by its counted population and the configured descriptor item limit
  instead of a statement's 64-descriptor mutation allowance.
- `Close()` no longer fails with `storage corruption` after a longer session.
  Background compaction re-selected a destination file it had already rewritten
  in the same pass, linking a second head fragment of one source at the same
  level, which `validateLevels` correctly rejected; the maintenance goroutine's
  fatal error surfaced from `sql.DB.Close`. The destination lookup now honours
  the pass's exclusion set. Retention collection is skipped (instead of failing)
  while the clock is younger than the retention window. No data was lost: the
  check fires on an in-memory generation before any durable state write.
- `SET lock_timeout` now bounds every plain statement, including DDL; DDL waiting on another session's catalog locks fails with `55P03` instead of waiting until the context ends.
- The conformance corpus fixtures (`testdata/postgres`, `testdata/postgres184`, `internal/zpgparse/testdata`) are committed, and the `testdata/` ignore rules are gone from `.gitignore`.
- `COPY … FROM STDIN` text input that ends without the `\.` terminator no longer drops the final record when the record count is odd: the source reported end of input as an `io.EOF` error, and the consumer discarded the pending row. A single-row `CopyFrom` now inserts its row.
- `FETCH ALL` (and any fetch that reaches the end of a live cursor) no longer fails with `EOF` and aborts the transaction: the end sentinel was returned joined with close/seal results instead of as `io.EOF`.
- User-defined function and procedure calls admit implicitly coercible arguments, as PostgreSQL does: `f(1)` resolves `f(bigint)`. An exact signature still outranks one that needs coercion; equally ranked candidates fail with `42725`.
- `CREATE TABLE AS` / `REFRESH MATERIALIZED VIEW` under row-level security no longer deadlock the session (the RLS rewrite re-entered the session gate) or fail with `42P01` (the population target was rewritten as an RLS relation); only the source query is rewritten.

### Deprecated

- N/A

### Removed

- N/A

### Known limitations

See README "Known limitations" (`information_schema.sql_features`/`sql_parts`/`sql_sizing` return zero rows; `%TYPE` superset; RE2 regexp; embedded single-process).
Catalog locks couple sessions: until a transaction ends, its catalog locks make other sessions' transaction blocks and writes wait, so `BEGIN; SELECT 1` in one session waits behind uncommitted DDL in another; autocommit reads are snapshot reads and do not wait. Bound the waits with `lock_timeout` (`55P03`), `statement_timeout` (`57014`) and `idle_in_transaction_session_timeout` (`25P03`); all default to 0. The official PostgreSQL 18.4 regression corpus does not yet pass in full; README "Known limitations" states the conformance status.
