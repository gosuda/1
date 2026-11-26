package supersql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

// TestPhase2DeferredConstraintFailedCommitRollsBackAndReleasesLocks verifies
// that a COMMIT failing due to a deferred constraint violation rolls back the
// transaction cleanly (instead of leaving it active/aborted holding locks),
// releases row and advisory locks immediately, and leaves the session ready
// for subsequent statements without hitting SQLSTATE 25P02.
func TestPhase2DeferredConstraintFailedCommitRollsBackAndReleasesLocks(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	db.SetMaxOpenConns(4)

	if _, err := db.ExecContext(ctx, `CREATE TABLE p2_locks (id bigint PRIMARY KEY, val bigint NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE p2_locks: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO p2_locks VALUES (1, 10)`); err != nil {
		t.Fatalf("INSERT p2_locks: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE p2_deferred (id bigint PRIMARY KEY, code bigint UNIQUE DEFERRABLE INITIALLY DEFERRED)`); err != nil {
		t.Fatalf("CREATE TABLE p2_deferred: %v", err)
	}

	t.Run("SQLCommit", func(t *testing.T) {
		connA, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("connA: %v", err)
		}
		defer connA.Close()
		connB, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("connB: %v", err)
		}
		defer connB.Close()

		if _, err := connA.ExecContext(ctx, `BEGIN`); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		if _, err := connA.ExecContext(ctx, `UPDATE p2_locks SET val = 99 WHERE id = 1`); err != nil {
			t.Fatalf("UPDATE p2_locks: %v", err)
		}
		if _, err := connA.ExecContext(ctx, `SELECT pg_advisory_xact_lock(42001)`); err != nil {
			t.Fatalf("pg_advisory_xact_lock: %v", err)
		}
		if _, err := connA.ExecContext(ctx, `INSERT INTO p2_deferred VALUES (1, 777), (2, 777)`); err != nil {
			t.Fatalf("INSERT deferred duplicate: %v", err)
		}
		_, commitErr := connA.ExecContext(ctx, `COMMIT`)
		if commitErr == nil {
			t.Fatal("COMMIT unexpectedly succeeded despite deferred UNIQUE violation")
		}
		if got := SQLState(commitErr); got != "23505" {
			t.Fatalf("COMMIT SQLSTATE = %q (%v), want 23505", got, commitErr)
		}

		// Advisory lock must be released immediately.
		var gotAdvisory bool
		if err := connB.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(42001)`).Scan(&gotAdvisory); err != nil {
			t.Fatalf("connB pg_try_advisory_xact_lock: %v", err)
		}
		if !gotAdvisory {
			t.Fatal("connB failed to acquire advisory lock after connA failed COMMIT")
		}

		// Row lock must be released immediately and uncommitted UPDATE rolled back.
		if _, err := connB.ExecContext(ctx, `SET lock_timeout = '2s'`); err != nil {
			t.Fatalf("connB SET lock_timeout: %v", err)
		}
		var prevVal int64
		if err := connB.QueryRowContext(ctx, `SELECT val FROM p2_locks WHERE id = 1`).Scan(&prevVal); err != nil {
			t.Fatalf("connB SELECT val: %v", err)
		}
		if prevVal != 10 {
			t.Fatalf("connB val = %d, want 10 (rolled back)", prevVal)
		}
		if _, err := connB.ExecContext(ctx, `UPDATE p2_locks SET val = 20 WHERE id = 1`); err != nil {
			t.Fatalf("connB UPDATE after failed SQL COMMIT: %v", err)
		}

		// Deferred rows must have been rolled back.
		var deferredCount int64
		if err := connB.QueryRowContext(ctx, `SELECT count(*) FROM p2_deferred`).Scan(&deferredCount); err != nil {
			t.Fatalf("connB count p2_deferred: %v", err)
		}
		if deferredCount != 0 {
			t.Fatalf("p2_deferred count = %d, want 0", deferredCount)
		}

		// connA must immediately execute a new query without hitting 25P02.
		var one int64
		if err := connA.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil {
			t.Fatalf("connA SELECT 1 after failed COMMIT: %v (SQLSTATE=%q)", err, SQLState(err))
		}
		if one != 1 {
			t.Fatalf("connA SELECT 1 = %d, want 1", one)
		}
	})

	t.Run("DriverTxCommit", func(t *testing.T) {
		connA, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("connA: %v", err)
		}
		defer connA.Close()
		connB, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("connB: %v", err)
		}
		defer connB.Close()

		tx, err := connA.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE p2_locks SET val = 199 WHERE id = 1`); err != nil {
			_ = tx.Rollback()
			t.Fatalf("tx UPDATE p2_locks: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(42002)`); err != nil {
			_ = tx.Rollback()
			t.Fatalf("tx pg_advisory_xact_lock: %v", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO p2_deferred VALUES (10, 888), (11, 888)`); err != nil {
			_ = tx.Rollback()
			t.Fatalf("tx INSERT deferred duplicate: %v", err)
		}
		commitErr := tx.Commit()
		if commitErr == nil {
			t.Fatal("tx.Commit unexpectedly succeeded despite deferred UNIQUE violation")
		}
		if got := SQLState(commitErr); got != "23505" {
			t.Fatalf("tx.Commit SQLSTATE = %q (%v), want 23505", got, commitErr)
		}

		// Advisory lock must be released immediately.
		var gotAdvisory bool
		if err := connB.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(42002)`).Scan(&gotAdvisory); err != nil {
			t.Fatalf("connB pg_try_advisory_xact_lock: %v", err)
		}
		if !gotAdvisory {
			t.Fatal("connB failed to acquire advisory lock after connA failed tx.Commit")
		}

		// Row lock must be released immediately and uncommitted UPDATE rolled back.
		if _, err := connB.ExecContext(ctx, `SET lock_timeout = '2s'`); err != nil {
			t.Fatalf("connB SET lock_timeout: %v", err)
		}
		if _, err := connB.ExecContext(ctx, `UPDATE p2_locks SET val = 30 WHERE id = 1`); err != nil {
			t.Fatalf("connB UPDATE after failed tx.Commit: %v", err)
		}
		var val int64
		if err := connB.QueryRowContext(ctx, `SELECT val FROM p2_locks WHERE id = 1`).Scan(&val); err != nil || val != 30 {
			t.Fatalf("connB val = %d, %v; want 30", val, err)
		}

		// connA must immediately execute a new query without hitting 25P02.
		var check int64
		if err := connA.QueryRowContext(ctx, `SELECT val FROM p2_locks WHERE id = 1`).Scan(&check); err != nil {
			t.Fatalf("connA SELECT after failed tx.Commit: %v (SQLSTATE=%q)", err, SQLState(err))
		}
		if check != 30 {
			t.Fatalf("connA val = %d, want 30", check)
		}
	})
}

// TestPhase2CanceledCommitRollsBackAndReleasesLocks verifies that calling
// Commit on a transaction whose context has been canceled (and is not
// indeterminate) rolls back cleanly, releases held row and advisory locks,
// and leaves the connection usable.
func TestPhase2CanceledCommitRollsBackAndReleasesLocks(t *testing.T) {
	ctx := t.Context()

	t.Run("DriverDirectCanceledCommit", func(t *testing.T) {
		connector, err := OpenConnector(ctx, OpenOptions{
			Directory:   testDirectory(t),
			Clock:       newTestClock(),
			Maintenance: MaintenanceConfig{Disabled: true},
		})
		if err != nil {
			t.Fatalf("OpenConnector: %v", err)
		}
		t.Cleanup(func() {
			if err := connector.Close(); err != nil {
				t.Errorf("connector close: %v", err)
			}
		})

		rawA, err := connector.Connect(ctx)
		if err != nil {
			t.Fatalf("Connect A: %v", err)
		}
		defer rawA.Close()
		connA := rawA.(*connection)

		rawB, err := connector.Connect(ctx)
		if err != nil {
			t.Fatalf("Connect B: %v", err)
		}
		defer rawB.Close()
		connB := rawB.(*connection)

		if _, err := connA.ExecContext(ctx, `CREATE TABLE p2_cancel (id bigint PRIMARY KEY, val bigint NOT NULL)`, nil); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		if _, err := connA.ExecContext(ctx, `INSERT INTO p2_cancel VALUES (1, 10)`, nil); err != nil {
			t.Fatalf("INSERT: %v", err)
		}

		txCtx, txCancel := context.WithCancel(ctx)
		tx, err := connA.BeginTx(txCtx, driver.TxOptions{})
		if err != nil {
			txCancel()
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := connA.ExecContext(txCtx, `UPDATE p2_cancel SET val = 99 WHERE id = 1`, nil); err != nil {
			txCancel()
			_ = tx.Rollback()
			t.Fatalf("UPDATE: %v", err)
		}
		if _, err := connA.ExecContext(txCtx, `SELECT pg_advisory_xact_lock(43001)`, nil); err != nil {
			txCancel()
			_ = tx.Rollback()
			t.Fatalf("pg_advisory_xact_lock: %v", err)
		}

		// Cancel txCtx BEFORE calling driver.Tx.Commit() directly (no database/sql
		// awaitDone goroutine) so transaction.Commit() handles the canceled commit.
		txCancel()
		commitErr := tx.Commit()
		if commitErr == nil {
			t.Fatal("Commit on canceled context unexpectedly succeeded")
		}
		if !errors.Is(commitErr, context.Canceled) && SQLState(commitErr) != "57014" {
			t.Fatalf("Commit error = %v (SQLSTATE=%q), want context.Canceled or 57014", commitErr, SQLState(commitErr))
		}
		if connA.scope.txns.Active() {
			t.Fatal("connA transaction remained active after canceled Commit")
		}

		// connB must immediately acquire the advisory lock and row lock.
		rows, err := connB.QueryContext(ctx, `SELECT pg_try_advisory_xact_lock(43001)`, nil)
		if err != nil {
			t.Fatalf("connB pg_try_advisory_xact_lock: %v", err)
		}
		vals := make([]driver.Value, 1)
		if err := rows.Next(vals); err != nil {
			_ = rows.Close()
			t.Fatalf("connB rows.Next: %v", err)
		}
		_ = rows.Close()
		if got, ok := vals[0].(bool); !ok || !got {
			t.Fatalf("connB pg_try_advisory_xact_lock = %v (%T), want true", vals[0], vals[0])
		}

		if _, err := connB.ExecContext(ctx, `SET lock_timeout = '2s'`, nil); err != nil {
			t.Fatalf("connB SET lock_timeout: %v", err)
		}
		if _, err := connB.ExecContext(ctx, `UPDATE p2_cancel SET val = 20 WHERE id = 1`, nil); err != nil {
			t.Fatalf("connB UPDATE after canceled Commit: %v", err)
		}

		// connA must immediately execute a new query without hitting 25P02.
		rowsA, err := connA.QueryContext(ctx, `SELECT val FROM p2_cancel WHERE id = 1`, nil)
		if err != nil {
			t.Fatalf("connA SELECT after canceled Commit: %v (SQLSTATE=%q)", err, SQLState(err))
		}
		valsA := make([]driver.Value, 1)
		if err := rowsA.Next(valsA); err != nil {
			_ = rowsA.Close()
			t.Fatalf("connA rows.Next: %v", err)
		}
		_ = rowsA.Close()
		if got, ok := valsA[0].(int64); !ok || got != 20 {
			t.Fatalf("connA val = %v (%T), want 20", valsA[0], valsA[0])
		}
	})

	t.Run("SQLTxCanceledCommit", func(t *testing.T) {
		db, _ := openTestDB(t)
		db.SetMaxOpenConns(4)

		if _, err := db.ExecContext(ctx, `CREATE TABLE p2_sql_cancel (id bigint PRIMARY KEY, val bigint NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO p2_sql_cancel VALUES (1, 10)`); err != nil {
			t.Fatalf("INSERT: %v", err)
		}

		connA, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("connA: %v", err)
		}
		defer connA.Close()
		connB, err := db.Conn(ctx)
		if err != nil {
			t.Fatalf("connB: %v", err)
		}
		defer connB.Close()

		txCtx, txCancel := context.WithCancel(ctx)
		tx, err := connA.BeginTx(txCtx, nil)
		if err != nil {
			txCancel()
			t.Fatalf("BeginTx: %v", err)
		}
		if _, err := tx.ExecContext(txCtx, `UPDATE p2_sql_cancel SET val = 88 WHERE id = 1`); err != nil {
			txCancel()
			_ = tx.Rollback()
			t.Fatalf("tx UPDATE: %v", err)
		}
		if _, err := tx.ExecContext(txCtx, `SELECT pg_advisory_xact_lock(43002)`); err != nil {
			txCancel()
			_ = tx.Rollback()
			t.Fatalf("tx pg_advisory_xact_lock: %v", err)
		}

		txCancel()
		if err := tx.Commit(); err == nil {
			t.Fatal("sql.Tx.Commit on canceled context unexpectedly succeeded")
		}

		var gotAdvisory bool
		if err := connB.QueryRowContext(ctx, `SELECT pg_try_advisory_xact_lock(43002)`).Scan(&gotAdvisory); err != nil {
			t.Fatalf("connB pg_try_advisory_xact_lock: %v", err)
		}
		if !gotAdvisory {
			t.Fatal("connB failed to acquire advisory lock after canceled sql.Tx.Commit")
		}

		if _, err := connB.ExecContext(ctx, `SET lock_timeout = '2s'`); err != nil {
			t.Fatalf("connB SET lock_timeout: %v", err)
		}
		if _, err := connB.ExecContext(ctx, `UPDATE p2_sql_cancel SET val = 25 WHERE id = 1`); err != nil {
			t.Fatalf("connB UPDATE after canceled sql.Tx.Commit: %v", err)
		}

		var val int64
		if err := connA.QueryRowContext(ctx, `SELECT val FROM p2_sql_cancel WHERE id = 1`).Scan(&val); err != nil {
			t.Fatalf("connA SELECT after canceled sql.Tx.Commit: %v (SQLSTATE=%q)", err, SQLState(err))
		}
		if val != 25 {
			t.Fatalf("connA val = %d, want 25", val)
		}
	})
}

// TestPhase2BeginTxActiveTransactionReturns25001 verifies that calling
// sql.Conn.BeginTx while an explicit SQL transaction is already active returns
// SQLSTATE 25001 without committing, rolling back, or poisoning the outer
// transaction.
func TestPhase2BeginTxActiveTransactionReturns25001(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)
	db.SetMaxOpenConns(4)

	if _, err := db.ExecContext(ctx, `CREATE TABLE p2_active_tx (id bigint PRIMARY KEY, note text NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	connA, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("connA: %v", err)
	}
	defer connA.Close()
	connB, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("connB: %v", err)
	}
	defer connB.Close()

	// Start explicit outer transaction and insert one row.
	if _, err := connA.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("BEGIN: %v", err)
	}
	if _, err := connA.ExecContext(ctx, `INSERT INTO p2_active_tx VALUES (1, 'outer-before')`); err != nil {
		t.Fatalf("INSERT outer-before: %v", err)
	}

	// Nested BeginTx must fail with SQLSTATE 25001.
	nestedTx, err := connA.BeginTx(ctx, nil)
	if err == nil {
		_ = nestedTx.Rollback()
		t.Fatal("connA.BeginTx while in explicit BEGIN unexpectedly succeeded")
	}
	if got := SQLState(err); got != "25001" {
		t.Fatalf("connA.BeginTx SQLSTATE = %q (%v), want 25001", got, err)
	}

	// Second nested BeginTx with options must also fail with 25001.
	nestedRO, err := connA.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err == nil {
		_ = nestedRO.Rollback()
		t.Fatal("connA.BeginTx(ReadOnly) while in explicit BEGIN unexpectedly succeeded")
	}
	if got := SQLState(err); got != "25001" {
		t.Fatalf("connA.BeginTx(ReadOnly) SQLSTATE = %q (%v), want 25001", got, err)
	}

	// Outer transaction must NOT have been committed yet.
	var visibleBefore int64
	if err := connB.QueryRowContext(ctx, `SELECT count(*) FROM p2_active_tx`).Scan(&visibleBefore); err != nil {
		t.Fatalf("connB count before commit: %v", err)
	}
	if visibleBefore != 0 {
		t.Fatalf("connB saw %d rows before outer COMMIT; BeginTx prematurely committed outer tx", visibleBefore)
	}

	// Outer transaction must NOT have been rolled back or aborted (25P02).
	if _, err := connA.ExecContext(ctx, `INSERT INTO p2_active_tx VALUES (2, 'outer-after')`); err != nil {
		t.Fatalf("INSERT outer-after failed (SQLSTATE=%q): %v", SQLState(err), err)
	}
	if _, err := connA.ExecContext(ctx, `COMMIT`); err != nil {
		t.Fatalf("outer COMMIT: %v", err)
	}

	// Both rows from the outer transaction must now be committed.
	var visibleAfter int64
	if err := connB.QueryRowContext(ctx, `SELECT count(*) FROM p2_active_tx`).Scan(&visibleAfter); err != nil {
		t.Fatalf("connB count after commit: %v", err)
	}
	if visibleAfter != 2 {
		t.Fatalf("connB saw %d rows after outer COMMIT, want 2", visibleAfter)
	}

	// Verify rollback path as well: BeginTx error does not commit outer tx, and
	// explicit ROLLBACK discards outer writes cleanly.
	if _, err := connA.ExecContext(ctx, `BEGIN`); err != nil {
		t.Fatalf("second BEGIN: %v", err)
	}
	if _, err := connA.ExecContext(ctx, `INSERT INTO p2_active_tx VALUES (3, 'outer-rollback')`); err != nil {
		t.Fatalf("INSERT outer-rollback: %v", err)
	}
	if nestedTx, err := connA.BeginTx(ctx, nil); err == nil {
		_ = nestedTx.Rollback()
		t.Fatal("second nested BeginTx unexpectedly succeeded")
	} else if got := SQLState(err); got != "25001" {
		t.Fatalf("second nested BeginTx SQLSTATE = %q (%v), want 25001", got, err)
	}
	if _, err := connA.ExecContext(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("outer ROLLBACK: %v", err)
	}
	var countAfterRollback int64
	if err := connB.QueryRowContext(ctx, `SELECT count(*) FROM p2_active_tx`).Scan(&countAfterRollback); err != nil {
		t.Fatalf("connB count after rollback: %v", err)
	}
	if countAfterRollback != 2 {
		t.Fatalf("connB saw %d rows after outer ROLLBACK, want 2", countAfterRollback)
	}
}
