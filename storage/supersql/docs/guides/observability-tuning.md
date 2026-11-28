# Observability, Auto-`ANALYZE` & Resource Tuning

SuperSQL exposes structured per-query observability hooks, live runtime metrics, background Auto-`ANALYZE`, and explicit memory/storage envelopes so embedded deployments stay predictable under load.

---

## 1. Per-Query Observer & Slow-Query Logging

Configure `Observer`, `SlowQueryThreshold`, and `OnSlowQuery` in `OpenOptions`:

```go
db, err := supersql.Open(ctx, supersql.OpenOptions{
	Directory:          dir,
	Clock:              localclock.New(),
	SlowQueryThreshold: 100 * time.Millisecond,
	OnSlowQuery: func(ctx context.Context, ev supersql.QueryEvent) {
		slog.WarnContext(ctx, "slow query",
			"sql", ev.SQL,
			"duration", ev.Duration,
			"planning", ev.PlanningDuration,
			"rows", ev.RowsAffected,
			"shared_hit", ev.SharedHitBlocks,
			"shared_read", ev.SharedReadBlocks,
			"spill_bytes", ev.SpillBytes,
			"err", ev.Err,
		)
	},
	Observer: supersql.QueryObserverFunc(func(ctx context.Context, ev supersql.QueryEvent) {
		// Export to Prometheus / OpenTelemetry
	}),
})
```

- Internal bootstrap DDL and background Auto-`ANALYZE` statements are isolated from user query counters and observers.

---

## 2. Live Runtime Metrics (`Catalog.Metrics`)

Poll `db.Metrics(ctx)` to export a point-in-time `DatabaseMetrics` snapshot (suitable for `/metrics` or periodic health logs):

```go
m := db.Metrics(ctx)
// m.QueriesTotal, m.QueriesFailed, m.SlowQueriesTotal
// m.AutoAnalyzeRuns
// m.BlockCacheHits, m.BlockCacheMisses, m.BlockCacheEntries
// m.UncompactedRunFiles, m.WALBacklogBytes
```

---

## 3. Background Auto-`ANALYZE`

The Cascades optimizer relies on table statistics (row counts, NDV, null fractions, MCVs, histograms). SuperSQL tracks per-table row mutations across `INSERT`, `UPDATE`, `DELETE`, `MERGE`, and `COPY FROM`, and automatically runs `ANALYZE` in the background maintenance loop once:

```text
modified_rows >= AutoAnalyzeMinRows + AutoAnalyzeScaleFactor * live_rows
```

Default thresholds (`MaintenanceConfig`):
- `AutoAnalyzeMinRows = 50`
- `AutoAnalyzeScaleFactor = 0.2`
- `AutoAnalyzeDisabled = false`

You can customize thresholds in `OpenOptions.Maintenance` or trigger a pass synchronously:

```go
analyzedTables, err := db.RunAutoAnalyze(ctx)
```

---

## 4. Memory, Storage & Timeout Tuning

| Layer | Configuration | Default | Behavior When Exceeded |
| :--- | :--- | :--- | :--- |
| **Query Work Memory** | `SET work_mem = '64MB'` | `64 MB` | Sort, HashJoin, Aggregate, Window, SetOp spill cleanly to `tmp/` |
| **Spill Cap** | `SET temp_file_limit = '512MB'` | Unlimited (`-1`) | Aborts statement cleanly with `53100` |
| **Shared Query Budget** | `OpenOptions.MemoryBudgetBytes` | `256 MiB` | Rejects allocation with `53200` (no process OOM) |
| **Transaction Overlay** | `OpenOptions.TransactionMemoryBytes` / `TransactionSpillBytes` | `8 MiB` / `32 MiB` | Spills large uncommitted transactions to `tmp/` |
| **Storage Envelope** | `OpenOptions.Compaction` (`CompactionConfig`) | `DefaultCompactionConfig()` | Bounds resident cache, open FDs (`MaxFiles`), WAL & run sizes |
| **Concurrency Limits** | `OpenOptions.MaxTransactions` / `MaxOperations` | `64` / `64` | Bounds active concurrent transactions and read ops |
| **SQL Timeouts** | `statement_timeout`, `lock_timeout`, `idle_in_transaction_session_timeout`, `transaction_timeout` | `0` (disabled) | Cancels with `57014`, `55P03`, or `25P03` |
| **History Retention** | `OpenOptions.HistoryRetention` | `24h` | Controls `AS OF SYSTEM TIME` window & `inactiveTier` reclamation |

See [`docs/reference/configuration.md`](../reference/configuration.md) for the complete generated field reference of `OpenOptions`, `CompactionConfig`, and `MaintenanceConfig`.
