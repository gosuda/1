// Package sqlite is a zero-cgo database/sql driver for SQLite served over HQQ
// shared memory, built on the Go client that hqq-bindgen generates from
// sqlite3/sqlite3.idl. The SQLite engine runs in its own OS process
// (hqq-sqlite-server in sqlite3/server). Every database/sql connection is an
// isolated generated-client session with its own shared-memory ring, dedicated
// server thread, and sqlite3 connection handle.
//
// The driver registers itself as "sqlite3" when the package is imported:
//
//	import (
//		"database/sql"
//
//		_ "gosuda.org/sqlite3" // registers "sqlite3"
//	)
//
//	db, err := sql.Open("sqlite3", "hqq://sqlite-svc?db=main&_journal=WAL&_busy_timeout=5000")
//
// The DSN format is hqq://<listener>?<options>, where <listener> is the HQQ
// acceptor name of the server. Supported options include db (required alias or
// relative path), mode (rwc, rw, or ro), timeout, fetch (rows per batch),
// busy / _busy_timeout / _timeout (lock wait duration or milliseconds),
// txlock / _txlock (deferred, immediate, or exclusive), loc / _loc (auto or IANA
// timezone name), standard underscore-prefixed PRAGMA control knobs (_fk,
// _journal_mode, _synchronous, _cache_size, _locking_mode, _auto_vacuum,
// _temp_store, _mmap_size, _page_size, _wal_autocheckpoint, _secure_delete,
// _query_only, _recursive_triggers, _case_sensitive_like, _defer_foreign_keys,
// _ignore_check_constraints, _writable_schema), and arbitrary pragma.<name>=value
// entries. Unknown, duplicate, or conflicting options return an error. See
// docs/DSN.md for the full control-knob reference.
//
// To construct a connector directly with structured configuration:
//
//	c, err := sqlite.NewConnector("sqlite-svc", sqlite.Config{
//		Database:    "main",
//		Timeout:     5 * time.Second,
//		FetchSize:   256,
//		BusyTimeout: 5 * time.Second,
//	})
//	db := sql.OpenDB(c)
//
// Errors. A SQLite engine error from the server is returned as *Error with Code
// (the primary result code, such as CodeConstraint or CodeBusy) and ExtendedCode
// (such as CodeConstraintUnique; equal to Code when SQLite provides none). Use
// errors.As to inspect *Error, the helper predicates IsBusy, IsLocked,
// IsConstraint, IsReadOnly, and IsInterrupted, or errors.Is with the sentinels
// ErrBusy, ErrLocked, ErrConstraint, ErrReadOnly, and ErrInterrupted. A protocol
// or policy violation returns *ProtocolError. A lost link before a request is
// sent, or during a query, Begin, Commit, or Rollback, wraps driver.ErrBadConn
// so database/sql discards the connection and retries idempotent operations. A
// lost link during Exec or ExecStmt wraps ErrOutcomeUnknown (not
// driver.ErrBadConn) because the statement may have already committed on the
// server.
//
// Cancellation. When a call's context expires while the server is executing it,
// the driver sends an out-of-band cancellation over the control link (invoking
// the @cancel(sqlite3_interrupt) hook on the session's Db handle) and closes the
// session link, returning the context error immediately and discarding the
// connection.
//
// Values. Arguments: nil is NULL; signed integers and uint64 values up to
// math.MaxInt64 are INTEGER; float32 and float64 are REAL (NaN is rejected with
// an error, +Inf and -Inf are preserved); bool is 0 or 1; string is TEXT; []byte
// and json.RawMessage are BLOB (convert json.RawMessage to string to store
// TEXT); time.Time is formatted as "2006-01-02 15:04:05.999999999-07:00" (after
// converting to Config.Location when set); driver.Valuer (including sql.Null*)
// is evaluated. Named arguments use sql.Named with or without a prefix (":id",
// "@id", "$id"). Results: INTEGER is int64, REAL is float64, TEXT is string,
// BLOB is []byte, and NULL is nil. A TEXT cell in a column declared DATE,
// DATETIME, or TIMESTAMP is parsed into time.Time (using Config.Location or UTC
// for offset-free strings) when valid, or returned as string otherwise. Cells
// that exceed the 64 KiB batch buffer are streamed in chunks via column_chunk up
// to SQLite's 1 GiB limit.
//
// Transactions. BeginTx accepts sql.LevelDefault and sql.LevelSerializable and
// rejects other isolation levels. ReadOnly transactions begin DEFERRED with
// PRAGMA query_only = 1. Read-write transactions use Config.TxMode or a
// per-transaction override from WithTxMode.
//
// Each Exec, Query, or Prepare call accepts exactly one SQL statement; trailing
// SQL statements fail with a BAD_REQUEST ProtocolError ("multiple statements").
package sqlite
