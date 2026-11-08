package supersql

import (
	"database/sql"
	"testing"
)

func TestCompatIsolationUpgrade(t *testing.T) {
	ctx := t.Context()
	connector := mustConnector(t, ctx)
	defer connector.Close()
	database := sql.OpenDB(connector)
	defer database.Close()

	conn, err := database.Conn(ctx)
	if err != nil {
		t.Fatalf("database.Conn: %v", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE compat_iso_items (id bigint PRIMARY KEY, val text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// 1. Default behavior automatically escalates weaker isolation levels to SERIALIZABLE.
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("BeginTx(LevelReadCommitted) by default: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO compat_iso_items VALUES (1, 'first')`); err != nil {
		_ = tx.Rollback()
		t.Fatalf("INSERT inside default escalated ReadCommitted tx: %v", err)
	}
	var gotVal string
	if err := tx.QueryRowContext(ctx, `SELECT val FROM compat_iso_items WHERE id = 1`).Scan(&gotVal); err != nil {
		_ = tx.Rollback()
		t.Fatalf("SELECT inside default escalated ReadCommitted tx: %v", err)
	}
	if gotVal != "first" {
		_ = tx.Rollback()
		t.Fatalf("SELECT val = %q, want %q", gotVal, "first")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit default escalated ReadCommitted tx: %v", err)
	}

	// Verify ReadOnly + LevelRepeatableRead via BeginTx by default.
	roTx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		t.Fatalf("BeginTx(LevelRepeatableRead, ReadOnly) by default: %v", err)
	}
	if err := roTx.QueryRowContext(ctx, `SELECT val FROM compat_iso_items WHERE id = 1`).Scan(&gotVal); err != nil {
		_ = roTx.Rollback()
		t.Fatalf("SELECT inside default RepeatableRead RO tx: %v", err)
	}
	if _, err := roTx.ExecContext(ctx, `INSERT INTO compat_iso_items VALUES (2, 'second')`); err == nil {
		_ = roTx.Rollback()
		t.Fatal("expected INSERT inside ReadOnly tx to fail, got nil")
	}
	if err := roTx.Rollback(); err != nil {
		t.Fatalf("Rollback default RepeatableRead RO tx: %v", err)
	}

	// Verify SQL-level BEGIN ISOLATION LEVEL READ COMMITTED and SET TRANSACTION ISOLATION LEVEL REPEATABLE READ by default.
	if _, err := conn.ExecContext(ctx, `BEGIN ISOLATION LEVEL READ COMMITTED`); err != nil {
		t.Fatalf("BEGIN ISOLATION LEVEL READ COMMITTED by default: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		t.Fatalf("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ by default: %v", err)
	}
	var iso string
	if err := conn.QueryRowContext(ctx, `SHOW transaction_isolation`).Scan(&iso); err != nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		t.Fatalf("SHOW transaction_isolation: %v", err)
	}
	if iso != "serializable" {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		t.Fatalf("SHOW transaction_isolation = %q, want serializable", iso)
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("COMMIT SQL tx: %v", err)
	}

	// Verify SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED by default.
	if _, err := conn.ExecContext(ctx, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED`); err != nil {
		t.Fatalf("SET SESSION CHARACTERISTICS by default: %v", err)
	}

	// 2. Setting supersql.compat_isolation = 'off' enforces strict 0A000 rejection.
	if _, err := conn.ExecContext(ctx, `SET supersql.compat_isolation = 'off'`); err != nil {
		t.Fatalf("SET supersql.compat_isolation = 'off': %v", err)
	}
	if tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted}); err == nil {
		_ = tx.Rollback()
		t.Fatal("expected BeginTx(LevelReadCommitted) with compat_isolation=off to fail with 0A000, got nil")
	} else if got := sqlStateOf(err); got != "0A000" {
		t.Fatalf("BeginTx(LevelReadCommitted) with compat_isolation=off SQLSTATE = %q, want 0A000 (%v)", got, err)
	}
	if _, err := conn.ExecContext(ctx, `BEGIN ISOLATION LEVEL READ COMMITTED`); err == nil {
		_, _ = conn.ExecContext(ctx, `ROLLBACK`)
		t.Fatal("expected BEGIN ISOLATION LEVEL READ COMMITTED with compat_isolation=off to fail, got nil")
	} else if got := sqlStateOf(err); got != "0A000" {
		t.Fatalf("BEGIN ISOLATION LEVEL READ COMMITTED with compat_isolation=off SQLSTATE = %q, want 0A000 (%v)", got, err)
	}

	// 3. After RESET supersql.compat_isolation, default automatic escalation is restored.
	if _, err := conn.ExecContext(ctx, `RESET supersql.compat_isolation`); err != nil {
		t.Fatalf("RESET supersql.compat_isolation: %v", err)
	}
	tx3, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("BeginTx(LevelReadCommitted) after RESET: %v", err)
	}
	if err := tx3.Rollback(); err != nil {
		t.Fatalf("Rollback after RESET: %v", err)
	}
}
