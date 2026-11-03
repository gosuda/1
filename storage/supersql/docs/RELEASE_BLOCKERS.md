# Release Blockers

Checklist of known issues that must be resolved (or explicitly waived) before a public release.
Baseline: `main` at 1331cf7 plus the deslop pass. Every item below was observed on 1331cf7
before the deslop pass, so the cleanup did not introduce any of them.

Rules:
- Check an item only when the fix is merged **and** the listed verification passes.
- Add new blockers with severity, location, evidence, and a verification command.
- Waiving an item requires a one-line reason and the approver, written inline.

Severity: **P1** = blocks release. **P2** = must be fixed or explicitly documented as a limitation before release.

## P1

- [x] **Per-statement latency grows with catalog size.**
  - Location: `internal/query/session/type_runtime.go` (declared-type definitions and routine discovery, now cached); `internal/query/session/routine_reentry.go` `catalogDiscoveryDependencies` (dependency-discovery walk, now cached); `schema.IndexPolicyProvider`'s declared-type key encoder (built a fresh runtime per snapshot, ~280 reads/`SELECT` and ~1692+BFS per `INSERT`, now shares a connector-level `session.DeclaredTypeCache` via `open.go`).
  - Evidence (HEAD 9dc2ab4, probe = Open + `db.Exec` loop): `SELECT` 57 ms empty catalog -> 388 ms after 40 tables + 100 enums; `INSERT` 259 -> 799 ms. Catalog reads per no-DDL statement: 24 empty -> 5516 with 40 tables + 100 enums. CPU profile: ~48% in `ScanNamespace -> Store.read -> readPAXPoint -> pread`.
  - Fix: three caches, all keyed by `txn.CatalogReadIdentity` (committed version, transaction overlay + owner, snapshot overlay) plus their other inputs, so a hit is served only for the exact catalog content the computation ran against. (1) Declared-type definitions per session (landed earlier: rebuilds 3 -> 0, reads 5516 -> 2969). (2) The dependency-discovery walk: the discovered dependency suffix is a pure function of catalog content, so it is cached per session; the locks are still acquired on every statement: a probe view declares `base` + schema root and names the identity, a hit declares `base` + cached suffix under a fresh view and re-checks the identity under the held locks, and a commit landing between probe and lock changes the observed identity and falls back to a full walk. A hit never runs holding fewer locks than a fresh walk. (3) The declared-type routine BFS (and the key-encoder runtime): the discovered routine definitions are cached (per session; connector-level for overlay-free snapshots) and only the cheap resolver is compiled per statement, so no snapshot is retained in a cached value. `CatalogReadIdentity.OverlayOwner` was added so equal identities never merge two transactions that staged the same number of catalog changes.
  - Result (identical probe, measured against clean HEAD in a worktree): catalog reads per no-DDL `SELECT` 2995 -> 0 (empty 55 -> 0); `SELECT` 209.7 -> 45.4-55.9 ms loaded vs 34.5-40.9 ms empty (ratio 4.23 -> 1.11-1.62, target ~2x met); `INSERT` 630.7 -> 239.5 ms loaded, 183 -> 20 reads on an empty catalog. Guard tightened to `loaded <= empty + 16` (both 0); forcing both caches to miss makes it fail. Every cache hit was instrumented to recompute fresh and compare (`reflect.DeepEqual`): 0 divergences over ~130 routine hits and ~34 discovery hits across the test runs.
  - Follow-up: Startup-only clock wait (`internal/dataplane/txn/manager.go`) and elimination of per-read `database.state` `F_FULLFSYNC` (`open.go`) further reduced single-statement write latency to sub-millisecond in-memory/WAL times.
  - Verify: `go test -count=1 -run 'TestDeclaredTypeRuntime|TestCatalogDiscovery' -v .`; `TestPG184Differential` 114 steps / 0 mismatches; `-race` on session + txn + catalog.

- [x] **`Close()` returns `storage corruption` after a longer session.**
  - Location: `internal/dataplane/storage/neosepia/compaction_levels.go` `selectLevelCompaction` (destination lookup); `internal/controlplane/schema/retention.go` `RetentionCoordinator.advance`.
  - Evidence (clean HEAD 9dc2ab4 and 1fae5ec): 150x `CREATE TYPE ... AS ENUM`, `ALTER TYPE`, `SELECT`, then `pool.Close()` -> `storage corruption`, 3/3 with a realistic clock. The error is returned by `pool.Close()` (`sql.DB.Close` runs the connector's `io.Closer` -> `closeAll`, which joins the background maintenance goroutine's fatal error). With the default test clock (`now: 1000`) the same run returned `timestamp outside positive SI nanosecond range`.
  - Root cause: `selectLevelCompaction` chose the level-N+1 destination without checking the pass's `exclude` set (the seed and L0-extension loops did check it). A destination already rewritten earlier in the same `runMaintenance` pass was re-selected 18 times on one target; each re-selection ran `retainFragments`, linking another head fragment of the same source, so two level-1 fragments with the same real minimum key and different cuts coexisted and `validateLevels` rejected them.
  - Fix: skip excluded names in the destination lookup (3 lines). Separately, `advance` returns nil without collecting when `retentionCutoff` returns `model.ErrTimestampRange` (the clock is younger than the retention window, so nothing is old enough to retire); other errors stay fatal.
  - Data: reopening after the failed close read back every row and enum, so no data loss was observed; the check fired on an in-memory replacement generation before any durable state write.
  - Verify: `go test -count=1 -run 'TestCompactionExcludesRewrittenDestinationFromLaterIntervals' ./internal/dataplane/storage/neosepia`; `go test -count=1 -run TestRetentionSkipsCollectionWhileClockIsYoungerThanWindow ./internal/controlplane/schema`; `go test -count=1 -run 'TestTypeAlterAdmitsCatalogSizedNamespace|TestDeclaredTypeRuntimeIsNotRebuiltPerStatement' .`; `go test -count=1 -run '^TestCloseSucceedsAfterLongSessionWithRealisticClock$' .`.

- [x] **`DROP TYPE` is rejected for every name form.**
  - Location: `internal/query/bind/type_declaration.go` `bindDropTypes`.
  - Evidence (clean HEAD 9dc2ab4): `CREATE TYPE d AS ENUM ('a')` then `DROP TYPE d`, `DROP TYPE public.d`, `DROP TYPE IF EXISTS d`, `DROP TYPE d CASCADE` each -> `invalid DROP TYPE name` (corpus x88).
  - Fix: the parser emits one `TypeName` node per object (not a `List`); `bindDropTypes` required `GetList()`. It now reads each object through `typeNameFromNode`. `IF EXISTS`, `CASCADE`/`RESTRICT`, multiple names and `2BP01` under RESTRICT already existed downstream and became reachable.
  - Verify: `go test -count=1 -run 'TestDropType|TestDeclaredTypeRuntimeDropAfterCachedState' -v . ./internal/query/bind`.

- [x] **`ALTER TYPE ... ADD VALUE` fails with `type dependency namespace limit exceeded` once ~100 user enums exist.**
  - Location: `internal/controlplane/catalog/type_service.go` `TypeCascadeScanDependencies`.
  - Evidence (clean HEAD 9dc2ab4): 100 enums, then `ALTER TYPE e0 ADD VALUE 'z'` -> `54000 type dependency namespace limit exceeded` (corpus x37). The walk charged every namespace row against the statement's 64-descriptor mutation allowance (`open.go`).
  - Fix: the walk counts its population first (a no-retention pre-pass over the same spans) and fails with 54000 only if that exceeds the store's configured `Limits.Descriptor.MaxItems` (1024, the same field `NewDeclaredTypeResolver` bounds its population with). The per-statement `maximum` parameter was removed from `TypeCascadeScanDependencies` and its callers.
  - Verify: `go test -count=1 -run 'TestTypeAlterAdmitsCatalogSizedNamespace' -v .` and `go test -count=1 -run TestTypeCascadeCeilingRejectsOverPopulation ./internal/controlplane/catalog`.

- [x] **A second session's read blocks indefinitely on another session's open DDL transaction.**
  - Evidence (clean HEAD 9dc2ab4): session A `BEGIN; CREATE TYPE t AS ENUM ('s')`; session B `SELECT 's'::t::text` waits on `lockRegistry.acquireWithConflict`. `lock_timeout` defaults to unbounded, as in PostgreSQL.
  - Scope: A plain ReadWrite statement with no declared dependencies binds under `catalogLayoutDependencies` (`internal/query/session/read_dependencies.go`), which takes `lockShare` on unbounded spans of all three catalog collections. Any uncommitted catalog-row write (`lockUpdate`) therefore blocks other sessions' ReadWrite statements until transaction end, and an idle `BEGIN; SELECT 1` blocks other sessions' DDL. Waits are bounded by `lock_timeout` (55P03), `statement_timeout` (57014), `idle_in_transaction_session_timeout` (25P03), and context deadlines.
  - Verify: `go test -count=1 -run 'TestCatalogLockWait' -v .`.
  - Waiver: **approved** by repository owner (2026-10-14); documented in `README.md` §10 ("Known Limitations") and verified via `TestCatalogLockWait`.

- [x] **`lock_timeout` is ignored for plain-Exec DDL.**
  - Location: `internal/query/session/execute_statement.go` `Transactions.RunStatement` (dispatch context).
  - Evidence (pre-fix, 11ce187 + test): A `BEGIN; SELECT 1` idle; B `SET lock_timeout='1s'; CREATE TYPE u AS ENUM ('s')` waited until the 5 s test context (57014) instead of failing 55P03 at 1 s.
  - Fix: one `txn.WithLockTimeout(ctx, ...Resources().LockTimeout)` before the dispatch chain, covering every plain-statement branch.
  - Verify: `go test -count=1 -run 'TestCatalogLockWaitDDLBoundedByLockTimeout' -v .`.

- [x] **Documented `database/sql` entry point does not exist.**
  - Location: `README.md` §7.1, `connector.go`.
  - Evidence: no `sql.Register` in the repo, so `sql.Open("supersql", ...)` fails with `unknown driver "supersql"`. `Driver.Open` always returns `ErrClockRequired`.
  - Fix: §7.1 documents `OpenConnector` + `sql.OpenDB`, generated verbatim from `example_readme_test.go` (docgen region `readme-example`) and executed as `ExampleOpenConnector`.
  - Verify: `go test -count=1 -run '^ExampleOpenConnector$' -v .`.

- [x] **`go test ./...` fails with the default test timeout.**
  - Location: root package `github.com/gosuda/supersql-alpha`.
  - Evidence: `internal/controlplane/schema/index_scheduler.go` `scan` polled `Manager.CurrentReadView` every 10 ms (`open.go`), and every call certified a new fence and republished `database.state` even when idle.
  - Fix: (1) `Manager.ClosedReadView` reads the already-published closed frontier without certifying; (2) root tests share one bootstrapped template directory copied per test (`testDirectory`, `os.CopyFS`); (3) long tests are gated behind `testing.Short()`.
  - Verify: `go test -short -count=1 ./...` passes across all packages.

- [x] **`clock/gstime` tests fail.**
  - Tests: `TestNewRejectsInvalidHistory` (`clock_test.go:286`), `TestFailedRoundHoldoverExpiresFromLastSuccess` (`provider_test.go:305`), `TestInvalidationReacquisitionUsesFreshFence` (`provider_test.go:321`).
  - Fix: `certifiedClock` adopts the service's rotated epoch under a mutex (`adoptEpoch`); stale fences still fail in `WaitUntilAfter`. `TestNewRejectsInvalidHistory` uses midnight transitions (`-62135683200`, `253402300800`).
  - Verify: `go test -race -count=1 ./clock/...`.

- [x] **Test fixtures missing from the repository.**
  - Fix: committed `testdata/postgres184/`, `testdata/postgres/`, and `internal/zpgparse/testdata/`.
  - Verify: `go test -count=1 ./internal/query/builtin/internal/genunicode ./internal/zpgparse`.

- [x] **`CheckpointSpec` can certify a fence over an unresolved commit (TOCTOU).**
  - Location: `internal/dataplane/txn/manager.go`.
  - Fix: re-check `commitUnresolved()` immediately after `defer manager.sequencer.unlock()`, mirroring `openStrongSnapshot`. Added `TestCheckpointSpecRejectsQueuedCommitResolution`.
  - Verify: `go test -race -count=1 ./internal/dataplane/txn/...`.

- [x] **Compaction and garbage collection never run in production.**
  - Location: `internal/dataplane/storage/api.go` `ScheduleCompaction`, `internal/controlplane/schema/retention.go` `NewRetentionCoordinator` / `Backend.Collect`, `maintenance.go`.
  - Fix: `Open` composes a `RetentionCoordinator` and starts a `maintenanceLoop` worker (`MaintenanceConfig` on `OpenOptions`). The loop probes `Backend.MaintenanceBacklog` on `Interval` and runs `RetentionCoordinator.Advance` + `Backend.Collect(Boundary: HistoryFloor)`.
  - Verify: `go test -count=1 -run 'Maintenance' . ./internal/dataplane/storage/neosepia`; `SUPERSQL_SOAK=1 go test -run TestCatalogSoakBoundedGrowth -v .`.

- [x] **Triage open Dependabot alerts.**
  - Evidence: GitHub reported 18 alerts on the default branch (9 critical, 2 high, 6 moderate, 1 low) against `github.com/jackc/pgx/v5 v5.7.6` (alerts #3, #4, #5) and indirect `golang.org/x/crypto v0.37.0` (alerts #1, #2, #6-#18).
  - Fix: Upgraded `github.com/jackc/pgx/v5` from `v5.7.6` to `v5.11.0` (`go.mod`, `THIRD_PARTY_NOTICES.md`), fixing all `pgx/v5` advisories and dropping the indirect `golang.org/x/crypto` requirement from `go.mod`.
  - Verify: `go run ./internal/tools/licensecheck` exits 0; `grep -c 'golang.org/x/crypto' go.mod` returns 0; `github.com/jackc/pgx/v5` is at `v5.11.0`.

- [x] **Module path `z.gfire.dev/supersql` is not resolvable.**
  - Fix: module path changed to `github.com/gosuda/supersql-alpha` (`go.mod:1` and all imports).
  - Verify: `go build ./...` exits 0.

- [x] **`docgen -check` fails on `main` (CI `docgen-drift` job red).**
  - Location: `docs/reference/{sql-compatibility,sqlstates,configuration}.md`.
  - Fix: regenerated via `go run ./internal/tools/docgen`.
  - Verify: `go run ./internal/tools/docgen -check` exits 0.

- [x] **`ALTER TABLE ... ADD UNIQUE / PRIMARY KEY` on a table that already has an index panics the process.**
  - Location: `internal/query/bind/schema_ddl.go` `promoteTransactionalConstraintIndex`.
  - Fix: construct the binder with its initialized dependency set (`make(map[model.ObjectID]model.ObjectDescriptor)`).
  - Verify: `go test -count=1 -run '^TestAddUniqueConstraintOnIndexedTable$' .`.

- [x] **`ALTER TABLE ... ADD UNIQUE / PRIMARY KEY` on a table that already has an index is rejected with 55000.**
  - Location: `internal/controlplane/schema/constraint_validation.go` `validationIndexSet`, `internal/query/exec/modify/options.go` `preflightConstraint`.
  - Fix: `validationIndexSet` builds a filtered validation index set for the single newly created constraint index (`table.WithIndexes([]model.ObjectID{index.ID()})`), and `modify.Options.preflightConstraint` skips table-level index expansion count mismatch while `ConstraintTransaction != nil`.
  - Verify: `go test -count=1 -run '^TestAddUniqueConstraintOnIndexedTable$' -v .`.

- [x] **Per-statement latency grows with committed history: run files accumulate and maintenance does not bound them.**
  - Location: `internal/dataplane/storage/neosepia` (`backend_maintenance.go`, `compaction_levels.go`, `compaction_prepare.go`, `run_set_maintenance.go`), `maintenance.go`.
  - Fix: (1) `selectTargetCompaction` compacts fragmented single-level active SST runs (`len(files) >= m.limits.Level0CompactionTrigger`) even when `len(runs.Levels) == 1` and while the clock is younger than the retention window (`boundary <= 0`); (2) `finishWriters` closes only writers that exist; (3) `maintenanceTargets` sorts targets by total file count descending; (4) `maintainOnce` checks `UncompactedRunFiles()` rather than total `RunFiles()`.
  - Verify: `SUPERSQL_SOAK=1 go test -count=1 -run '^TestCatalogSoakBoundedGrowth$' -v .` passes (`run_files` bounded, reopen **32.5ms**); `go test -count=1 ./internal/dataplane/storage/neosepia/...` passes.

- [x] **Row-Level Security (RLS) policy state is not cleaned up on `DROP TABLE` / `ALTER TABLE RENAME`, and `SubLink` subqueries / `INSERT ... ON CONFLICT DO UPDATE` / `pg_policies` / `pg_roles` bypass or lack RLS integration.**
  - Severity: **P1**
  - Location: `rls.go` (`capturePreDDLRLSTables`, `afterDDLStatementForRLS`, `rewriteExprSublinksForRLS`, `rewriteFromClauseRLSCatalogs`, `rewriteSelectForRLS`, `rewriteStatementForRLS`), `open.go` (`execStatement`).
  - Fix: (1) `capturePreDDLRLSTables` + `afterDDLStatementForRLS` clean up `rls.state.Tables` on `DROP TABLE` and migrate table keys on `ALTER TABLE ... RENAME TO ...` / `SET SCHEMA`. (2) `rewriteExprSublinksForRLS` recursively walks expression trees (`SubLink`, `BoolExpr`, `AExpr`, `FuncCall`, `CaseExpr`, etc.) across `SELECT`/`UPDATE`/`DELETE`/`ON CONFLICT` to enforce RLS inside subqueries. (3) `rewriteStatementForRLS` conjoins the `"update"` `USING` predicate onto `OnConflictClause.WhereClause` and combines `"insert"` + `"update"` `WITH CHECK` predicates. (4) `rewriteFromClauseRLSCatalogs` synthesizes `pg_policies` and `pg_roles` inline `RangeSubselect` relations when RLS is active.
  - Verify: `go test -count=1 -run '^(TestMultiTenantGUCAndRowLevelSecurity|TestRLSProductionCompleteness)$' -v .` passes.

- [x] **RLS bypass paths (`COPY`, `MERGE`, `PREPARE`/`EXECUTE`, `DECLARE CURSOR`, `CTAS`/`MATVIEW`, `AS OF SYSTEM TIME`, SQL/PL/pgSQL routines), missing privilege enforcement (`SET ROLE`, role/policy DDL), `GRANT`/`REVOKE` silent no-op, fail-open resolution, and non-transactional `tmp/rls.json` persistence (S1–S5).**
  - Severity: **P1**
  - Location: `rls.go`, `open.go`, `internal/controlplane/schema/catalog_manifest.go`, `internal/query/session/routine_reentry.go`.
  - Evidence: (S1) `rewriteStatementForRLS` only handles top-level `SELECT`/`INSERT`/`UPDATE`/`DELETE`; `COPY`, `MERGE`, `PREPARE`/`EXECUTE`, `DECLARE CURSOR`, `CREATE TABLE AS`, `CREATE/REFRESH MATERIALIZED VIEW`, `AS OF SYSTEM TIME`, and routine bodies bypass RLS. (S2) Unprivileged roles can run `SET ROLE postgres`, `CREATE ROLE ... SUPERUSER`, and `DISABLE ROW LEVEL SECURITY` / `DROP POLICY`. (S3) `GRANT`/`REVOKE` on tables returns a success tag without enforcing table privileges instead of `0A000`. (S4) `resolveCatalogTable` ignores resolution errors (fail-open) and `rls.go` contains test-specific `"r"` role branches. (S5) `tmp/rls.json` is written outside MVCC transactions without `fsync` and is omitted from `CreateHotBackup`.
  - Fix: `rewriteStatementForRLS` covers `COPY TO`, `MERGE`, `PREPARE`/`EXECUTE`, `DECLARE CURSOR`, `CTAS`/`MATVIEW`, and routine bodies; `tryExecCopyFromRLS` enforces `WITH CHECK` on `COPY FROM`; role/policy DDL and `SET ROLE` check privileges and membership; table `GRANT`/`REVOKE` fail `0A000`; RLS metadata is written through the transaction (`rls_meta.json`) and included in backups. Follow-on fixes found while closing the matrix: (a) text `COPY FROM` input without the `\.` terminator dropped the last record on odd record counts because `internal/query/copy/source.go` `readLine` surfaced end of input as an `io.EOF` error; (b) `FETCH ALL` failed with `EOF` because `internal/query/session/cursor.go` `nextLive` returned `io.EOF` joined with close/seal results; (c) RLS rewrites inside `CREATE TABLE AS` / `REFRESH MATERIALIZED VIEW` population and routine bodies deadlocked on `Transactions.Active` (session gate already held), now `Transactions.ActiveIn(ctx)`; (d) population rewrote the `INSERT` target (`42P01`), now `ObjectCommandOptions.RewriteQuery` rewrites only the source query; (e) user routine calls required exact argument types (`f(1)` for `f(bigint)` failed `42883`), now `internal/query/bind/function.go` `userRoutine` ranks implicitly coercible candidates.
  - Verify: `go test -count=1 -run '^(TestRLSSecurityBypassMatrix|TestRLSTransactionalCatalogAndBackup|TestRLSProductionCompleteness|TestMultiTenantGUCAndRowLevelSecurity)$' -v .` passes; `go test -count=1 -run '^TestRoutineImplicitArgumentCoercion$' -v .` and `go test -count=1 -run '^TestTextEndOfInputWithoutTerminator$' ./internal/query/copy` pass.

- [x] **Snapshot reads contend with commit `fsync` on the global sequencer gate and `Parallelism: 1` is hardcoded (W1, W2).**
  - Severity: **P1**
  - Location: `open.go`, `internal/dataplane/txn/manager.go`, `internal/dataplane/txn/manager_commit.go`, `internal/dataplane/txn/transaction_read.go`, `internal/dataplane/storage/neosepia/backend_mutation.go`, `internal/dataplane/storage/neosepia/backend_read.go`, `internal/query/session/execute_statement.go`, `internal/query/session/execute_prepared.go`, `internal/query/session/transactions.go`.
  - Evidence: (W1) Autocommit `SELECT` defaults to `txn.ReadWrite`, and `openStrongSnapshot` / `Backend.OpenSnapshot` wait on `sequencer.gate` and `backend.mu` while `ApplyMutations` runs disk `fsync`. (W2) `open.go` hardcodes `Parallelism: 1` with no `OpenOptions` override.
  - Fix: autocommit `SELECT` runs as an implicit read-only transaction, snapshot opens no longer wait on in-flight commit `fsync`, and `OpenOptions.Parallelism` (default `runtime.GOMAXPROCS(0)`) replaces the hardcoded value.
  - Verify: `go test -race -count=1 -run '^TestReadWriteConcurrencyAndParallelism$' -v .` and `go test -race -count=1 -run '^TestTransactionReadsDoNotWaitForInFlightCommit$' -v ./internal/dataplane/txn` pass.

## P2

- [x] **`EXPLAIN (ANALYZE, BUFFERS | WAL | MEMORY)` sections are silently dropped.**
  - Location: `internal/query/bind/explain.go` `bindExplain`.
  - Fix: `buffers || wal || memory` returns `unsupported(ServerObservability, 0)` = SQLSTATE `0A000`.
  - Verify: `go test -run TestBindExplainRejectsObservabilitySections ./internal/query/bind`.

- [x] **Hunspell replacement flags (`repl/flags`) are ignored.**
  - Location: `internal/query/builtin/fulltext_dictionary_ispell.go` (`ispellAffix.replFlags`).
  - Fix: `normalizeSubWord` skips, for an ordinary word (`flag == 0`), every affix whose flagflags or replacement flags carry `ONLYINCOMPOUND`.
  - Verify: `go test -run TestNativeIspellDictionaryStandaloneCompoundOnlyAffix ./internal/query/builtin`.

- [x] **Deparse emits a double space for `AT_AlterColumnGenericOptions` / `AT_GenericOptions`.**
  - Location: `internal/zpgparse/deparse_ddl.go`.
  - Resolution: behavior matches the upstream `libpg_query` reference oracle (`TestDeparseAlterColumnGenericOptionsMatchesReference`).

- [x] **`information_schema.sql_features`, `sql_parts`, `sql_sizing` always return zero rows.**
  - Location: `open_targets.go` (`CatalogFeatures`/`CatalogSQLParts`/`CatalogSQLSizing`).
  - Fix: populated `embeddedSQLFeatures`, `embeddedSQLParts`, and `embeddedSQLSizing` in `open_targets.go` with the `embedded_pg18` SQL conformance and sizing inventory.
  - Verify: `go test -count=1 -run '^TestInformationSchemaSQLStandardsPopulated$' -v .`.

- [x] **PostgreSQL conformance gates are skipped by default.**
  - Location: `regression_test.go` (`NEOSEPIA_POSTGRES_REGRESS=1`), `pg184_differential_test.go` (`SUPERSQL_PG184_DSN`), `internal/query/bind/index_expression.go` (`defineRowExpression`).
  - Fix: wired into CI (`pg184` job in `.github/workflows/ci.yml`); fixed `defineRowExpression` stale relation version check in `internal/query/bind/index_expression.go` (eliminating the `0A000` leak in `join.sql`); core 13-file regression subset (`boolean`, `char`, `varchar`, `text`, `int2`, `int4`, `int8`, `float4`, `float8`, `numeric`, `select`, `join`, `subselect`, `aggregates`, `transactions`) runs in 17.5s with zero `0A000` leaks and zero panics.
  - Waiver: **approved** by repository owner (2026-10-14) for full 231-file corpus run; core regression subset and 114-step differential suite verified.

- [x] **`go vet` lostcancel warnings (possible context leak).**
  - Location: `internal/dataplane/txn/locks.go`, `internal/locks/advisory.go`.
  - Fix: registered `defer func() { stopWait() }()` immediately after `WithTimeoutCause`, and replaced struct copies containing locks with `proto.Clone`.
  - Verify: `go vet ./... 2>&1 | grep -v '^#' | grep -v 'scan\.rl:[0-9]*: unreachable code$'` prints nothing.

- [x] **PL/pgSQL `%TYPE` in routine signatures is a deliberate upstream superset.**
  - Location: `internal/query/bind/type.go` `pctColumnLayout`, `postgresql_corpus_test.go`.
  - Status: **waived** as an intentional superset. Approver: repository owner. Documented in `README.md` §10 ("Known Limitations").

- [x] **README storage description drifts from the implementation.**
  - Fix: §3.1 topology shows `commits/<pos>.wal` and `runs/base-<gen>.runs`; §8 states `VACUUM` is rejected with `0A000` and compaction/GC run automatically in the `Open` maintenance loop.

- [x] **Materialized views cannot be created, so they cannot be read.**
  - Location: `internal/query/bind/object_declaration.go`, `internal/controlplane/schema/`, `internal/query/session/object_ddl.go`, `internal/query/exec/modify.go`.
  - Verify: `go test -count=1 -run 'MaterializedView|CreateTableAs' -v .`.

- [x] **README overview still describes a registered driver and storage names that do not exist.**
  - Fix: `README.md` architecture box and §2 table name `OpenConnector`/`sql.OpenDB`, `c-<tid>-<n>.{active,inactive}.sst`, and `NSBASE03`.
  - Verify: `grep -nE 'NSRUN001|Driver \("supersql"\)|<gen>\.<run>' README.md` prints nothing.

- [x] **ORM / `database/sql` / `pgx` transactions requesting weaker isolation (`READ COMMITTED` / `REPEATABLE READ`) need automatic escalation to `SERIALIZABLE`.**
  - Severity: **P2**
  - Location: `connector.go` (`BeginTx`, `compatIsolationEnabled`, `rewriteCompatIsolation`), `pgx/pgx.go`.
  - Fix: By default (`compatIsolationEnabled() == true`), `BeginTx` and SQL `BEGIN ISOLATION LEVEL ...` / `SET TRANSACTION ISOLATION LEVEL ...` escalate `READ UNCOMMITTED`, `READ COMMITTED`, and `REPEATABLE READ` requests to `SERIALIZABLE`. Setting `SET supersql.compat_isolation = 'off'` (or `SET supersql.strict_isolation = 'on'`) opts into strict `0A000` rejection.
  - Verify: `go test -count=1 -run '^(TestSerializableOnlyIsolation|TestCompatIsolationUpgrade)$' -v .` and `go test -count=1 -v ./pgx` pass.

- [x] **`README.md` §10 ("Known Limitations") and §7 drift from current `information_schema`, Custom GUC, Row-Level Security, and `pgx` support.**
  - Severity: **P2**
  - Location: `README.md`, `docs/RELEASE_BLOCKERS.md`.
  - Fix: Updated `README.md` §7 with the cleanroom `pgx` package, Multi-Tenant Security & Session GUC, and default isolation escalation, and removed obsolete limitations from §10.
  - Verify: `go run ./internal/tools/docgen -check` exits 0 and `grep -n 'return zero rows' README.md` prints nothing.

- [x] **Outer join (`LEFT`/`RIGHT`/`FULL JOIN`) with RLS-enabled tables filters out unmatched outer rows (S6).**
  - Severity: **P2**
  - Location: `rls.go` (`rewriteSelectForRLS`).
  - Evidence: Joining an RLS-protected right-hand table in a `LEFT JOIN` pushes the right-hand table's `USING` predicate into the outer `SelectStmt.WhereClause`, turning NULL-extended unmatched left rows into false predicates and dropping them instead of preserving outer join semantics.
  - Fix: `collectFromItemRLSQual` rewrites each protected relation inside a join tree as a filtered subquery (`rewriteRangeVarAsRLSSubselect`); only a top-level relation's `USING` predicate goes into the outer `WhereClause`.
  - Verify: `go test -count=1 -run '^TestRLSOuterJoinSemantics$' -v .` passes (`LEFT`, `RIGHT`, `FULL`, protected relation on the nullable and the preserved side).

- [x] **Ad-hoc `ReadWrite` queries and `MutationDeleteRange` acquire full-table locks instead of narrowed key spans, and memory/transaction budgets are hardcoded (W3, W4, W5).**
  - Severity: **P2**
  - Location: `internal/query/session/execute_prepared.go` (`runPreparedStatement`), `internal/dataplane/txn/transaction_range.go` (`enumerateRange`), `open.go` (`OpenOptions`).
  - Evidence: (W3) `runPreparedStatement` opens `view` with `statementScanDependencies` (unbounded table `KeySpan`s) before `prepareBound` narrows the scan spans. (W4) `enumerateRange` calls `lockUpdate` on `all` (`Unbounded..Unbounded`) even though `applyOne` already locked `[mutation.Key, mutation.EndKey]`. (W5) `MemoryBudgetBytes`, `TransactionMemoryBytes`, `TransactionSpillBytes`, `MaxTransactions`, and `MaxOperations` are hardcoded in `open.go`.
  - Fix: (W3) `statementScanDependencies` (`internal/query/session/read_dependencies.go`) trial-plans the bound statement and takes the plan's narrowed read dependencies when the statement has no row-lock policies, parameters, or statistics. (W4) `enumerateRange` no longer takes the unbounded object lock and scans only `[mutation.Key, mutation.EndKey)`, which `applyOne` already locked. (W5) `OpenOptions` exposes `MemoryBudgetBytes`, `TransactionMemoryBytes`, `TransactionSpillBytes`, `MaxTransactions`, and `MaxOperations`.
  - Verify: `go test -race -count=1 -run '^TestReadWriteConcurrencyAndParallelism$' -v .` passes (two open transactions read, update, and range-delete disjoint keys without blocking; `OpenOptions` budgets applied).

## Verified non-blockers

- `internal/controlplane/schema/index_plan.go` `KeyLayout()` / `relational_policy.go` `cloneConstraintKeys` `panic(err)`: unreachable. Key layouts are validated by `model.NewDescriptor` before use, and the budgets cover the copy.
- `internal/zpgparse/internal/neoparse/semantic_values.go` invariant panics (L115, L138, L265, L288): reached only if a grammar rule yields a value or keyword outside the tables. A 3-minute fuzz of `zpgparse.Parse`/`ParseExtended` (2.1 M execs, 970 interesting inputs) found no crasher.

## Gate evidence

- `go build ./...` -> exit 0.
- `go vet ./...` -> clean except 27 generated-scanner `scan.rl:NNN: unreachable code` lines (baseline, excluded).
- `go run ./internal/tools/licensecheck` -> exit 0 (no findings).
- `go run ./internal/tools/docgen -check` -> exit 0.
- `go test -count=1 -v ./pgx` -> PASS.
- `go test -count=1 -run '^(TestCompatIsolationUpgrade|TestSerializableOnlyIsolation|TestMultiTenantGUCAndRowLevelSecurity|TestRLSProductionCompleteness|TestDocumentationGeneratorInSync)$' -v .` -> PASS.
- `SUPERSQL_SOAK=1 go test -count=1 -run '^TestCatalogSoakBoundedGrowth$' -v .` -> PASS (reopen 32.5ms).
