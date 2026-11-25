# SuperSQL SQL Reference

SuperSQL implements the **PostgreSQL 18.4** SQL and PL/pgSQL dialect (`embedded_pg18`, `server_version_num = 180004`) inside an embedded pure-Go (`CGO_ENABLED=0`) engine.

Rather than duplicating the full PostgreSQL 18 manual, this section documents:
1. **What is supported, partially supported, or excluded (`0A000`)** relative to PostgreSQL 18.4.
2. **Exact syntax synopses and SuperSQL-specific extensions** (such as `AS OF SYSTEM TIME`, pure-Go ICU collations, GIN inverted indexes, native MVCC RLS/RBAC, and `EXPLAIN (ANALYZE, BUFFERS)`).
3. **Executable SQL examples** automatically verified on every test run by [`docs_sql_test.go`](../../docs_sql_test.go).

---

## Navigation

| Document | Contents |
| :--- | :--- |
| **[PG 18 Differences](pg18-differences.md)** | Complete delta reference: behavioral differences, SuperSQL supersets, and `0A000` exclusions |
| **[Queries & DML](queries-and-dml.md)** | `SELECT`, CTEs, window functions, `GROUPING SETS`, row locks, `INSERT`/`ON CONFLICT`, `UPDATE`, `DELETE`, `MERGE`, `COPY`, `AS OF SYSTEM TIME` |
| **[DDL & Indexes](ddl-and-indexes.md)** | Schemas, tables, constraints, views, materialized views, sequences, B-tree & **GIN** indexes (`USING gin`), `ANALYZE`, `REINDEX` |
| **[Security, RBAC & RLS](security-rbac-rls.md)** | Roles, `GRANT`/`REVOKE`, `ALTER ... OWNER TO`, `SECURITY DEFINER`, Row-Level Security (`CREATE POLICY`), custom GUCs |
| **[PL/pgSQL & Triggers](plpgsql-and-triggers.md)** | SQL & `PL/pgSQL` functions, procedures, `DO` blocks, triggers, `%TYPE` superset, `BEGIN ... EXCEPTION` subtransactions |
| **[Types & ICU Collations](collations-and-types.md)** | Scalar/composite/enum/domain/range/JSONB/array/FTS types, pure-Go ICU collations, `CREATE/DROP COLLATION` |
| **[Transactions, `LISTEN`/`NOTIFY` & `EXPLAIN`](transactions-notify.md)** | `SERIALIZABLE` isolation, `SAVEPOINT`, `LISTEN`/`NOTIFY`/`UNLISTEN`/`pg_notify()`, `EXPLAIN (ANALYZE, BUFFERS)`, GUC reference |

For auto-generated exhaustive catalogs, see [`docs/reference/sql-compatibility.md`](../reference/sql-compatibility.md), [`docs/reference/functions.md`](../reference/functions.md), and [`docs/reference/sqlstates.md`](../reference/sqlstates.md).

---

## Statement Support Matrix

| Category | Statements | Status | Notes |
| :--- | :--- | :--- | :--- |
| **Queries (DQL)** | `SELECT`, `VALUES`, `TABLE`, `WITH [RECURSIVE]`, joins (`INNER`/`LEFT`/`RIGHT`/`FULL`/`CROSS`/`LATERAL`), `GROUP BY` / `GROUPING SETS` / `ROLLUP` / `CUBE`, `WINDOW`, `ORDER BY`, `LIMIT`/`OFFSET`/`FETCH`, `DISTINCT [ON]`, `UNION`/`INTERSECT`/`EXCEPT` | **Supported** | Vectorized execution with external spill |
| **Row Locking** | `FOR UPDATE`, `FOR NO KEY UPDATE`, `FOR SHARE`, `FOR KEY SHARE`, `NOWAIT`, `SKIP LOCKED` | **Supported** | Strict 2PL + shared `WaitGraph` (`40P01`) |
| **Time Travel** | `SELECT ... AS OF SYSTEM TIME <timestamptz>`, `SET TRANSACTION AS OF SYSTEM TIME` | **Extension** | Reads retained MVCC snapshot without write locks |
| **Mutations (DML)** | `INSERT` (`DEFAULT VALUES`, `ON CONFLICT DO NOTHING / DO UPDATE`), `UPDATE [FROM]`, `DELETE [USING]`, `MERGE INTO`, `TRUNCATE`, `RETURNING` | **Supported** | Full constraint, trigger, RLS, and index maintenance |
| **Bulk I/O** | `COPY ... FROM STDIN`, `COPY ... TO STDOUT` (text, CSV, binary) | **Supported** | Server filesystem paths & `PROGRAM` excluded (`0A000`) |
| **Schemas & Tables** | `CREATE / ALTER / DROP SCHEMA`, `CREATE / ALTER / DROP TABLE`, `CREATE TABLE AS`, `CREATE TEMP TABLE`, generated columns, identity columns, constraints (`PK`, `UNIQUE`, `FK`, `CHECK`, `EXCLUDE USING btree`) | **Supported** | `UNLOGGED`, `TABLESPACE`, `WITH (fillfactor=...)` excluded (`0A000`) |
| **Indexes** | `CREATE / ALTER / DROP INDEX`, `USING btree` (unique, partial, expression, `INCLUDE`), `USING gin` (`jsonb`, arrays, `tsvector`), `REINDEX TABLE / INDEX` | **Supported** | `GiST`, `SP-GiST`, `BRIN`, `Hash` excluded (`0A000`) |
| **Views & Sequences** | `CREATE [OR REPLACE] / ALTER / DROP VIEW`, `CREATE / REFRESH / DROP MATERIALIZED VIEW`, `CREATE / ALTER / DROP SEQUENCE`, `nextval`/`currval`/`lastval`/`setval` | **Supported** | `security_barrier` & `check_option` supported |
| **Types & Collations** | `CREATE / ALTER / DROP TYPE` (enum, composite, range), `CREATE / ALTER / DROP DOMAIN`, `CREATE / DROP COLLATION` (pure-Go ICU) | **Supported** | `provider = libc`, `ALTER COLLATION`, `CREATE CONVERSION` excluded (`0A000`) |
| **Procedural & Triggers** | `CREATE [OR REPLACE] / ALTER / DROP FUNCTION`, `PROCEDURE`, `CALL`, `DO $$ ... $$`, `CREATE / ALTER / DROP TRIGGER`, `SECURITY INVOKER / DEFINER` | **Supported** | Languages: `SQL` and `PL/pgSQL`; external C/PL languages excluded (`0A000`) |
| **Security & RLS** | `CREATE / ALTER / DROP ROLE`, `GRANT / REVOKE <role>`, object `GRANT / REVOKE` (`WITH GRANT OPTION`, `ALL IN SCHEMA`, `PUBLIC`), `ALTER ... OWNER TO`, `ENABLE / FORCE ROW LEVEL SECURITY`, `CREATE / ALTER / DROP POLICY` | **Supported** | `ALTER DEFAULT PRIVILEGES`, `SET SESSION AUTHORIZATION`, `SECURITY LABEL` excluded (`0A000`) |
| **Transactions** | `BEGIN` / `START TRANSACTION`, `COMMIT`, `ROLLBACK`, `SAVEPOINT`, `RELEASE SAVEPOINT`, `ROLLBACK TO SAVEPOINT`, `SET TRANSACTION` | **Supported** | `SERIALIZABLE` only (weaker levels auto-escalate by default); `PREPARE TRANSACTION` excluded (`0A000`) |
| **Pub/Sub** | `LISTEN`, `NOTIFY`, `UNLISTEN`, `pg_notify()`, `pg_listening_channels()` | **Supported** | Transactional commit-only fanout; `pg_notification_queue_usage()` excluded (`0A000`) |
| **Session & Planning** | `PREPARE` / `EXECUTE` / `DEALLOCATE`, `DECLARE` / `FETCH` / `MOVE` / `CLOSE` cursors, `SET [LOCAL]` / `SHOW` / `RESET`, `ANALYZE`, `EXPLAIN [(ANALYZE, BUFFERS, COSTS, VERBOSE, FORMAT ...)]` | **Supported** | `EXPLAIN (WAL, MEMORY)` and `DISCARD SEQUENCES/ALL` excluded (`0A000`) |
| **Cluster & Heap Admin** | `CREATE DATABASE`, `TABLESPACE`, `VACUUM`, `CLUSTER`, `CHECKPOINT`, `CREATE EXTENSION`, `CREATE PUBLICATION/SUBSCRIPTION`, `CREATE FOREIGN TABLE`, `CREATE RULE`, `CREATE EVENT TRIGGER`, `XML` | **Excluded (`0A000`)** | Deterministic SQLSTATE `0A000` with feature ID |
