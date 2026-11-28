# Transactions, `LISTEN`/`NOTIFY`, `EXPLAIN` & Session GUCs

---

## 1. Transactions & Savepoints

### Synopsis
```sql
BEGIN [ WORK | TRANSACTION ] [ ISOLATION LEVEL SERIALIZABLE ] [ READ WRITE | READ ONLY ] [ [ NOT ] DEFERRABLE ]
COMMIT [ WORK | TRANSACTION ]
ROLLBACK [ WORK | TRANSACTION ]

SAVEPOINT savepoint_name
RELEASE [ SAVEPOINT ] savepoint_name
ROLLBACK [ WORK | TRANSACTION ] TO [ SAVEPOINT ] savepoint_name
```

- All read-write transactions execute under **Serializable** isolation (Strict 2PL / PCC).
- By default (`supersql.compat_isolation = 'on'`), weaker requested isolation levels (`READ COMMITTED`, `REPEATABLE READ`, `READ UNCOMMITTED`) transparently escalate to `SERIALIZABLE`. Set `SET supersql.compat_isolation = 'off'` to reject them with `0A000`.
- `ROLLBACK TO SAVEPOINT` reverts uncommitted data, DDL, sequence/GUC overrides, and staged `NOTIFY` events while preserving Strict 2PL locks until transaction end.

<!-- doctest -->
```sql
CREATE TABLE tx_demo (id INT PRIMARY KEY);

BEGIN;
INSERT INTO tx_demo VALUES (1);
SAVEPOINT sp1;
INSERT INTO tx_demo VALUES (2);
ROLLBACK TO SAVEPOINT sp1;
INSERT INTO tx_demo VALUES (3);
COMMIT;

SELECT id FROM tx_demo ORDER BY id;
```

---

## 2. In-Process `LISTEN` / `NOTIFY` / `UNLISTEN`

### Synopsis
```sql
LISTEN channel
NOTIFY channel [, 'payload']
UNLISTEN { channel | * }

SELECT pg_notify('channel', 'payload');
SELECT * FROM pg_listening_channels();
```

### Semantics
- **Transactional**: `NOTIFY` and `pg_notify()` are staged in the active transaction, truncated on `ROLLBACK TO SAVEPOINT` and PL/pgSQL `EXCEPTION`, discarded on `ROLLBACK`, and deduplicated `(channel, payload)` per commit batch before non-blocking fanout on `COMMIT`.
- **Limits**: Channel names must be non-empty and `<= 63` bytes (`22023`); payloads must be `<= 8000` bytes (`54000`).
- **Consumption**:
  - Go native: `sub := db.Subscribe("events")` -> `sub.C()` / `sub.Wait(ctx)` / `sub.Close()`
  - `supersql/pgx`: `conn.WaitForNotification(ctx)`
  - `pgwire` TCP clients (`jackc/pgx/v5`, `psql`): automatic async `'A'` (`NotificationResponse`) push frames.

<!-- doctest -->
```sql
LISTEN order_events;
SELECT * FROM pg_listening_channels();

BEGIN;
NOTIFY order_events, 'order-100';
SELECT pg_notify('order_events', 'order-101');
COMMIT;

UNLISTEN order_events;
UNLISTEN *;
```

---

## 3. `EXPLAIN` & `EXPLAIN (ANALYZE, BUFFERS)`

### Synopsis
```sql
EXPLAIN [ ( option [, ...] ) ] statement

-- Supported options:
--   ANALYZE [ boolean ]
--   VERBOSE [ boolean ]
--   COSTS   [ boolean ]
--   BUFFERS [ boolean ]
--   TIMING  [ boolean ]
--   SUMMARY [ boolean ]
--   FORMAT  { TEXT | XML | JSON | YAML }
```

- `BUFFERS` reports NeoSepia shared block-cache hits/reads and 8 KiB temporary spill read/write blocks (`Buffers: shared hit=N read=M, temp read=X written=Y` in TEXT, and structured fields in JSON/YAML/XML).
- `WAL` and `MEMORY` options are excluded (`0A000`).

<!-- doctest -->
```sql
CREATE TABLE metrics_sample (id INT PRIMARY KEY, grp INT NOT NULL, val INT NOT NULL);
CREATE INDEX idx_metrics_grp ON metrics_sample (grp);
INSERT INTO metrics_sample SELECT g, g % 10, g * 2 FROM generate_series(1, 50) AS g;
ANALYZE metrics_sample;

EXPLAIN (ANALYZE, BUFFERS)
SELECT grp, sum(val) FROM metrics_sample WHERE grp = 3 GROUP BY grp;

EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)
SELECT id, val FROM metrics_sample WHERE grp = 3 ORDER BY id;
```

---

## 4. Supported Session GUCs (`SET` / `SHOW` / `RESET`)

| Parameter | Default | Notes |
| :--- | :--- | :--- |
| `search_path` | `"$user", public` | Schema resolution path |
| `TimeZone` | `GMT` | IANA time zone name or UTC offset |
| `DateStyle` | `ISO, MDY` | Date input/output formatting |
| `IntervalStyle` | `postgres` | Interval formatting |
| `bytea_output` | `hex` | `hex` or `escape` |
| `extra_float_digits` | `1` | Range `-15..3` |
| `default_text_search_config` | `pg_catalog.simple` | Default FTS dictionary/configuration |
| `statement_timeout` | `0` (disabled) | Cancels statement with `57014` |
| `lock_timeout` | `0` (disabled) | Aborts lock wait with `55P03` |
| `idle_in_transaction_session_timeout` | `0` (disabled) | Aborts idle open tx with `25P03` |
| `transaction_timeout` | `0` (disabled) | Bounds total transaction duration (`57014`) |
| `work_mem` | `64MB` (`65536` kB) | Operator spill threshold (min `64kB`) |
| `maintenance_work_mem` | `64MB` (`65536` kB) | Maintenance/index build memory |
| `temp_file_limit` | `-1` (unlimited) | Cap on spill bytes (`53100`) |
| `default_transaction_isolation` / `transaction_isolation` | `serializable` | Serializable only |
| `default_transaction_read_only` / `transaction_read_only` | `off` | Read-only mode |
| `default_transaction_deferrable` / `transaction_deferrable` | `off` | Read-only deferrable hint |
| `application_name` | `""` | Client metadata |
| `check_function_bodies` | `on` | Validate routine bodies at `CREATE` |
| `role` | `none` | Active role (`SET [LOCAL] ROLE`) |
| `supersql.compat_isolation` | `on` | Auto-escalate weaker isolation requests (`on`/`off`) |
| `<namespace>.<name>` (custom GUCs) | unset | Arbitrary dotted session/transaction GUCs (e.g. `SET LOCAL app.tenant_id = 't1'`) |

Read-only server metadata parameters: `server_encoding` (`UTF8`), `server_version`, `server_version_num` (`180004`), `integer_datetimes` (`on`), `max_identifier_length` (`63`).
