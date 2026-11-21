# SuperSQL Architecture

This document describes the internal architecture of SuperSQL: its package layout, the **NeoSepia** columnar storage engine, the certified-clock MVCC transaction engine, controlplane metadata and background maintenance, the Cascades query pipeline, native Row-Level Security (RLS), and bounded resource management.

For exact binary constants, generated Go API signatures, configuration envelopes, SQL compatibility tables, and SQLSTATE references, see [`docs/reference/`](reference/).

---

## 1. System Overview

![SuperSQL System Architecture](assets/architecture.svg)

SuperSQL is an embedded, single-process relational database written in pure Go (`CGO_ENABLED=0`). A single host process owns the data directory lock (`.lock`), the storage engine (`neosepia`), the transaction manager (`txn`), and the query runtime. OLTP point lookups, serializable multi-statement transactions, historical time-travel queries, and vectorized analytical scans all execute over one unified columnar storage layout.

### Package Map

| Layer | Package Path | Core Types & Entry Points | Responsibility |
| :--- | :--- | :--- | :--- |
| **Root Driver API** | `.` | `Open()`, `OpenConnector()`, `Connector`, `Catalog`, `CompactionConfig`, `*supersql.Error` | `database/sql` connector (`sql.OpenDB`), direct catalog execution, hot backup/restore, and structured SQLSTATE errors |
| **Cleanroom `pgx` API** | `pgx/` | `OpenPool()`, `OpenPoolWithConfig()`, `Pool`, `Conn`, `Tx`, `Batch`, `NamedArgs`, `*PgError` | In-process `pgx/v5`-compatible pool, transactions, implicit-tx batching, `pgtype` wrappers, hooks, and generic row collectors |
| **Clock & Time** | `clock/`, `clock/local/`, `clock/gstime/` | `Clock`, `localclock.New()`, `ReadFence`, `Epoch`, `Provider` | Certified monotonic clock contract, standalone single-process local clock (`clock/local`), and GSTime synchronization provider |
| **Model & Columnar** | `internal/model/`, `internal/columnar/` | `DataType`, `Value`, `ObjectRef`, `Batch`, `Vector` | Object identifiers, SQL type layouts, columnar vectors, null bitmaps, and batch budgets |
| **SQL Parser & AST** | `internal/zpgparse/` | `Parse()`, `Split()`, `Fingerprint()`, `Deparse()` | Pure-Go PostgreSQL 18 lexer, parser, AST, and deparser (generated via Ragel + `goyacc`) |
| **Controlplane** | `internal/controlplane/` | `Catalog`, `Namespace`, `SchemaJob`, `Statistics`, `RetentionCoordinator` | Versioned MVCC catalog descriptors, online index/schema jobs, table statistics, and history retention |
| **Transactions** | `internal/dataplane/txn/` | `Manager`, `Transaction`, `lockRegistry`, `WaitGraph`, `Overlay` | Strict 2PL / PCC concurrency control, sharded lock table, deadlock detection, spill-backed overlays, and commit sequencing |
| **Storage Backend** | `internal/dataplane/storage/neosepia/` | `Backend`, `paxBlock`, `sstWriter`, `runSet`, `memtable` | PAX v2 SSTables, Bloom filters, zone maps, TOAST blobs, WAL group commit, active/inactive compaction, and crash recovery |
| **Query Session** | `internal/query/session/` | `Transactions`, `Settings`, `Prepared`, `CatalogAccess` | Session settings (GUC), prepared statements, plan cache, cursors, roles, and `AS OF SYSTEM TIME` routing |
| **Query Binder** | `internal/query/bind/` | `Binder`, `Scope`, `BoundStatement` | Name and type resolution, catalog dependency declarations, RLS policy expansion, and constraint checks |
| **Query Optimizer** | `internal/query/opt/` | `Memo`, `Optimizer`, `PhysicalPlan`, `span` | Cascades cost-based optimizer, join reordering, index span extraction, sort elimination, and predicate pushdown |
| **Query Execution** | `internal/query/exec/` | `Pipeline`, `BatchCursor`, `HashJoin`, `TopK`, `Expr` | Vectorized operators (`BatchRows=1024`), expression VM, window/aggregate spill engines, and ~3,400 built-in functions |

---

## 2. NeoSepia Storage Engine

NeoSepia (`internal/dataplane/storage/neosepia`) is an immutable, log-structured columnar storage engine designed for mixed transactional and analytical workloads.

### 2.1 Directory Layout

See [`docs/reference/storage-format.md`](reference/storage-format.md) for exact magic headers, struct byte sizes, and recovery invariants.

```text
<Data Directory>/
├── database.state                       # Published state (NSDB0002): magic, generation, log position, fence, lease horizon, clock scales
├── database.state.pending               # Staging file for atomic state replacement; removed on recovery
├── fence.secret                         # HMAC-SHA256 secret for ReadFence verification, created atomically on first open
├── tmp/                                 # Spill directory for query operators, transaction overlays, and compaction buffers
├── commits/<020d>.wal                   # Immutable redo WAL segment per committed log position (group-committed)
├── commits/.<020d>.pending              # Staging file for an in-flight WAL segment
├── runs/<020d>.runs                     # Incremental single or coalesced run manifest (NSRUNS01 / NSRUNS02)
├── runs/<020d>-<8hex>.sst               # PAX v2 SSTable run file (NSSST004) with Bloom filters, zone maps, and TOAST blobs
├── runs/base-<020d>.runs                # Base manifest (NSBASE04) with pruned transaction horizon produced by maintenance
├── runs/.base.pending                   # Staging file for a base manifest rewrite
├── .lock                                # Process-exclusive directory lock held while open
└── .checkpoint-<hex>/                   # Hard-linked consistent snapshot directory for hot backups
```

### 2.2 64 KB PAX v2 Block Layout & TOAST (`NSSST004`)

Each SSTable data block (default 64 KB) uses 8-byte aligned little-endian PAX (Partition Attributes Across) columns so vectorized scans slice column arrays directly without per-row deserialization:

![PAX v2 64KB Block Layout](assets/pax-block-layout.svg)

Key `NSSST004` storage features:
- **Out-of-Line TOAST Blobs**: Variable-length column values exceeding 64 KiB are written out-of-line into the SST's blob section and referenced by a compact inline pointer, keeping PAX data blocks dense for scan throughput while supporting multi-megabyte values.
- **Per-SST Bloom Filters**: Each SSTable carries an `xxh3`-hashed Bloom filter over its keys so point lookups skip non-matching runs in $O(1)$ without reading data blocks.
- **SST-Index Zone Maps**: Block index entries store per-column min/max summaries, enabling range scans and predicate pushdown to prune entire 64 KB blocks before decoding.
- **16-Shard LRU Block Cache & Bounded FD Pool**: Decoded PAX blocks are cached in a 16-shard LRU cache, and open SST file handles are managed by a bounded LRU file-descriptor pool (`runSet`) so concurrent readers cannot exhaust OS descriptor limits (`ulimit -n`).
- **Binary-Search Point Lookups**: Point `Get` requests perform $O(\log N)$ binary search over block key arrays inside `activeTier` runs and allow concurrent readers on a shared snapshot.

### 2.3 Write Path: WAL Group Commit & In-Memory Memtable

- **WAL Group Commit**: Concurrent commits batch their WAL writes and `fsync` calls while preserving strictly monotonic commit timestamps and contiguous log positions.
- **Write-Behind Memtable & Coalesced Flushes (`NSRUNS02`)**: Committed mutations are buffered in an in-memory memtable and flushed into coalesced PAX SST runs rather than emitting one tiny SST per small commit, dramatically reducing write amplification and file count.

### 2.4 Dual-Tier Compaction (`activeTier` / `inactiveTier`)

- **`activeTier` ($L_1 \dots L_7$)**: Retains only the latest committed version for each key across leveled runs. Latest-state OLTP reads never walk historical version chains.
- **`inactiveTier`**: Stores superseded historical versions grouped by time window using multi-writer streaming compaction. Once the retention watermark advances past a window's upper bound, the entire inactive file is unlinked in $O(1)$ time.
- **Base Manifest Pruning (`NSBASE04`)**: Maintenance rewrites `runs/base-<pos>.runs`, pruning retired transaction records below the compacted frontier and keeping an $O(1)$ covered-fence index.

### 2.5 Crash Recovery & Hot Checkpoints

- **No-Steal Redo Recovery**: Uncommitted writes live only in transaction-private overlays and never touch SSTs. On startup, `neosepia.Open` validates `database.state` (`NSDB0002`), verifies the persisted `ReadFence` and lease horizon (`ClosedTS`), replays any committed `commits/<pos>.wal` segments newer than the run manifest, discards `.pending` staging files, and fails fast (`storage.ErrCorruptStorage`) on any CRC32 mismatch or ordering violation.
- **Streaming Hot Backups**: `Backup` / `RestoreCheckpoint` use hard-linked `.checkpoint-<hex>/` snapshots and streaming copies to avoid holding full snapshots in memory.

---

## 3. Transactions, Certified Clocks & Concurrency Control

![Client Write Transaction Pipeline](assets/transaction-pipeline.svg)

### 3.1 Certified-Clock MVCC & Time Travel

- **Clock Abstraction (`clock/`)**: Every transaction timestamp and snapshot `ReadFence` (`ClosedTS`, `LogPosition`, `Epoch`, HMAC tag) is anchored to a certified monotonic clock:
  - `clock/local`: Zero-dependency monotonic clock for standalone single-process deployments.
  - `clock/gstime`: Externally synchronized certified clock provider with automatic epoch rotation.
- **Serializable Isolation**: SuperSQL enforces serializable isolation via strict pessimistic concurrency control (PCC / Strict 2PL) over certified MVCC snapshots. By default (`supersql.compat_isolation = 'on'`), weaker requested isolation levels automatically escalate to `SERIALIZABLE`; setting `supersql.compat_isolation = 'off'` rejects non-serializable requests with `0A000`.
- **`AS OF SYSTEM TIME <timestamptz>`**: Historical read-only queries execute against a pinned MVCC snapshot verified by a `ReadFence` without acquiring write locks.

### 3.2 Locking, Savepoints & Deadlock Detection

- **Sharded `lockRegistry`**: Key-span locks, row locks (`FOR UPDATE`, `FOR NO KEY UPDATE`, `FOR SHARE`, `FOR KEY SHARE`, `NOWAIT`, `SKIP LOCKED`), and advisory locks are partitioned across a sharded lock registry to reduce contention under high concurrency.
- **Shared $O(V + E)$ `WaitGraph`**: Transaction locks and session advisory locks share one wait-for graph that detects deadlocks across mixed lock types in $O(V + E)$ time and aborts a victim with SQLSTATE `40P01`.
- **Strict 2PL Savepoints & Non-Bricking Statement Rollback**: `ROLLBACK TO SAVEPOINT` and statement-level errors revert only uncommitted overlay mutations while retaining Strict 2PL locks until transaction end, ensuring a failed statement inside an explicit `BEGIN` block does not brick subsequent rollback or savepoint recovery.
- **Spill-Backed Write Overlays**: Transaction write buffers (`Overlay`) track mutations in memory up to their configured byte budget and spill automatically to `tmp/` for large bulk transactions.
- **Atomic Commit Publish**: `Manager.submit` publishes committed state atomically so readers never observe torn transaction visibility.

---

## 4. Controlplane & Background Maintenance

- **Versioned MVCC Catalog (`internal/controlplane/catalog/`)**: Tables, indexes, views, sequences, functions, domains, enums, composite types, roles, ownership metadata, and RLS policies are stored as versioned catalog descriptors inside NeoSepia itself. DDL operations participate in full MVCC transactions and are included automatically in hot backups.
- **Per-Session Sequence State**: Sequence `nextval`/`currval`/`lastval` tracking is isolated per session while durable sequence advances persist transactionally.
- **Schema & Online Index Jobs (`internal/controlplane/schema/`)**: Background index backfills are driven by an event-signaled `IndexJobScheduler` (`Wake()` with exponential backoff) and a `JobStore` that prunes terminal job records to bound catalog growth.
- **Statistics (`internal/controlplane/statistics/`)**: `ANALYZE` collects row counts, NDV, null fractions, and MCV/histogram summaries persisted in the statistics store and consumed directly by the optimizer.
- **Retention Coordinator (`internal/controlplane/retention/`)**: Tracks active snapshot pins and advances the storage history floor safely (closing snapshot readers before triggering storage collection) so `inactiveTier` files can be reclaimed.

---

## 5. Query Processing Pipeline

![Query Processing Pipeline](assets/query-pipeline.svg)

### 5.1 Parsing & Binding (`internal/zpgparse`, `internal/query/bind`)

- **Pure-Go PostgreSQL 18 Parser**: `internal/zpgparse` implements a complete Ragel + `goyacc` PostgreSQL 18 grammar, statement splitter, query fingerprinter, and AST deparser with zero CGo (`libpg_query` is not linked).
- **Binder & Native RLS**: `bind` resolves identifiers, types, implicit casts, constraints, and declared catalog dependencies. When Row-Level Security is enabled on a target relation, the binder injects `PERMISSIVE`/`RESTRICTIVE` `USING` predicates wrapped in a `SecurityBarrier` (preventing leaky-operator pushdown bypasses) and attaches per-branch `WITH CHECK` enforcement for `INSERT`, `UPDATE`, and `MERGE`.

### 5.2 Cascades Cost-Based Optimizer (`internal/query/opt`)

- **Span Extraction**: Extracts tight index bounds for built-in types, `UUID`, `VARCHAR`/`BPCHAR`, `NUMERIC`, user-defined domains and enums, `LIKE 'prefix%'`, `= ANY($1)`, and parameterized (`$1`) predicates. User-defined domain/enum spans are encoded through the session's declared-type `KeyEncoder` so secondary indexes and primary keys prune directly in NeoSepia.
- **Statistics-Driven Costing & Join Reordering**: Wires persisted `ANALYZE` statistics into Cascades cardinality estimation, supports join key commutation, pushes filters through joins (respecting `SecurityBarrier` boundaries), and reorders joins beneath `Aggregate`, `Limit`, `Distinct`, `Window`, and `SetOp`.
- **Order Exploitation**: Recognizes reverse index scans (`ScanReverse`) and eliminates redundant `Sort` operators when an access path already satisfies the required `ORDER BY`.
- **Plan Caching**: Prepared and repeated statements reuse cached physical plans validated against catalog descriptor versions and RLS/role context.

### 5.3 Vectorized Execution Engine (`internal/query/exec`)

- **Columnar Vectorization**: Operators exchange 1,024-row (`BatchRows = 1024`) columnar batches (`internal/columnar`) with reusable builders and expression VM registers to minimize per-row allocations.
- **External Spill Operators**:
  - **Recursive Partitioned `HashJoin`**: Spills skewed or oversized build/probe partitions recursively to `tmp/`.
  - **Bounded-$K$ Heap `TopK`**: `ORDER BY ... LIMIT K` maintains a bounded heap instead of sorting the full input.
  - **External Merge `Sort`, `Aggregate` (`DISTINCT`/`ORDER BY`/Grouping Sets), `Window`, and `SetOp`**: Respect `work_mem` bidirectionally and spill intermediate partitions to `tmp/` while keeping `OutputMemory` batches sized for large TOAST values.
- **Built-in Catalog & PL/pgSQL**: Provides ~3,400 PostgreSQL built-in functions/operators, RE2 linear-time regular expressions, and a cancellable PL/pgSQL runtime (including interruptible loops).

---

## 6. Resource Bounds

Every layer enforces explicit memory, spill, and descriptor budgets rather than relying on unbounded heap growth:

- **Query Memory & `work_mem`**: `runtime.Query` tracks `WorkMemory`, `OutputMemory`, and `SpillBytes` independently. Exceeding configured bounds fails cleanly with SQLSTATE `53200` / `53100` instead of OOM-crashing the host process.
- **Storage Envelope (`CompactionConfig` / `neosepia.Limits`)**: Bounds open file handles, pinned run generations, decoded block cache bytes, WAL segment sizes, and scan plan memory.
- **Timeouts**: Supports `statement_timeout` (`57014`), `lock_timeout` (`55P03`), `idle_in_transaction_session_timeout` (`25P03`), and Go `context.Context` cancellation throughout parsing, locking, scanning, and PL/pgSQL execution.

---

## 7. Regenerating Generated Sources

### SQL Parser (`internal/zpgparse`)

With Ragel installed on `PATH` (`goyacc` is pinned via `tool` in `go.mod`):

```sh
go generate ./internal/zpgparse/...
CGO_ENABLED=0 go test ./internal/zpgparse/...
```

Edit `gram.y` and `scan.rl` rather than the generated `y.go` or `scan.go`.

### Built-in Tables & Reference Documentation

```sh
go generate ./internal/query/bind ./internal/query/builtin
go run ./internal/tools/docgen
```
