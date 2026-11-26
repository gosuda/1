package supersql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestPhase7MaintenanceBootstrapTransientClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "sqlstate 40001 model error",
			err:  &model.Error{Code: "40001", Message: "serialization failure"},
			want: true,
		},
		{
			name: "sqlstate 40001 wrapped model error",
			err:  fmt.Errorf("maintenance analyze: %w", &model.Error{Code: "40001", Message: "transaction wounded by an older lock waiter"}),
			want: true,
		},
		{
			name: "sqlstate 40001 exported error",
			err:  &Error{Code: "40001", Message: "could not serialize access"},
			want: true,
		},
		{
			name: "sqlstate 40P01 model error",
			err:  &model.Error{Code: "40P01", Message: "deadlock detected"},
			want: true,
		},
		{
			name: "deadlock sentinel",
			err:  locks.ErrDeadlockDetected,
			want: true,
		},
		{
			name: "wrapped deadlock sentinel",
			err:  fmt.Errorf("maintenance pass: %w", locks.ErrDeadlockDetected),
			want: true,
		},
		{
			name: "sqlstate 55P03 model error",
			err:  &model.Error{Code: "55P03", Message: "could not obtain lock"},
			want: true,
		},
		{
			name: "lock timeout sentinel",
			err:  txn.ErrLockTimeout,
			want: true,
		},
		{
			name: "sqlstate 53300 model error",
			err:  &model.Error{Code: "53300", Message: "too many active transactions"},
			want: true,
		},
		{
			name: "txn manager capacity sentinel",
			err:  txn.ErrManagerCapacity,
			want: true,
		},
		{
			name: "wrapped txn manager capacity sentinel",
			err:  fmt.Errorf("begin maintenance txn: %w", txn.ErrManagerCapacity),
			want: true,
		},
		{
			name: "deadline exceeded remains transient",
			err:  context.DeadlineExceeded,
			want: true,
		},
		{
			name: "snapshot pinned remains transient",
			err:  storage.ErrSnapshotPinned,
			want: true,
		},
		{
			name: "nil error is not transient",
			err:  nil,
			want: false,
		},
		{
			name: "context canceled is fatal",
			err:  context.Canceled,
			want: false,
		},
		{
			name: "wrapped context canceled is fatal",
			err:  fmt.Errorf("maintenance interrupted: %w", context.Canceled),
			want: false,
		},
		{
			name: "corrupt storage is fatal",
			err:  storage.ErrCorruptStorage,
			want: false,
		},
		{
			name: "wrapped corrupt storage is fatal",
			err:  fmt.Errorf("verify run: %w", storage.ErrCorruptStorage),
			want: false,
		},
		{
			name: "invalid encryption key is fatal",
			err:  neosepia.ErrInvalidEncryptionKey,
			want: false,
		},
		{
			name: "closed storage is fatal",
			err:  storage.ErrClosed,
			want: false,
		},
		{
			name: "generic fatal error is not transient",
			err:  errors.New("unrecoverable invariant failure"),
			want: false,
		},
		{
			name: "internal SQLSTATE XX000 is not transient",
			err:  &model.Error{Code: "XX000", Message: "internal error"},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := transientMaintenance(tc.err); got != tc.want {
				t.Fatalf("transientMaintenance(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestPhase7AutoAnalyzeBenignContention(t *testing.T) {
	wounded := errors.Join(
		&model.Error{Code: "40001", Message: "transaction wounded by an older lock waiter"},
		&model.Error{Code: "40001", Message: "transaction wounded by an older lock waiter"},
	)
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "joined 40001 wound-wait", err: wounded, want: true},
		{name: "40P01 deadlock", err: locks.ErrDeadlockDetected, want: true},
		{name: "55P03 lock timeout", err: txn.ErrLockTimeout, want: true},
		{name: "53300 capacity", err: txn.ErrManagerCapacity, want: true},
		{name: "snapshot pinned stays propagated", err: storage.ErrSnapshotPinned, want: false},
		{name: "deadline exceeded stays propagated", err: context.DeadlineExceeded, want: false},
		{name: "corrupt storage not benign", err: storage.ErrCorruptStorage, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoAnalyzeBenignContention(tc.err); got != tc.want {
				t.Fatalf("autoAnalyzeBenignContention(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestPhase7MaintenanceBootstrapConcurrentOpenQueriesAndCursors(t *testing.T) {
	ctx := t.Context()
	catalog, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Parallelism: 1,
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db := sql.OpenDB(NewConnectorWithCatalog(catalog))
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Errorf("db.Close: %v", closeErr)
		}
	})
	db.SetMaxOpenConns(64)
	db.SetMaxIdleConns(64)

	if _, err := db.ExecContext(ctx, `CREATE TABLE p7_items (id bigint PRIMARY KEY, val text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	for id := 1; id <= 10; id++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO p7_items (id, val) VALUES ($1, $2)`, id, fmt.Sprintf("v-%d", id)); err != nil {
			t.Fatalf("INSERT %d: %v", id, err)
		}
	}

	const concurrent = 24
	txs := make([]*sql.Tx, 0, concurrent)
	defer func() {
		for _, tx := range txs {
			_ = tx.Rollback()
		}
	}()

	// Hold 24 concurrent open transactions with active cursors simultaneously.
	// Each cursor allocates runtime query task groups; with Parallelism=1 the
	// old max(16, parallelism*4) cap rejected admission with SQLSTATE 53200.
	for i := range concurrent {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx[%d]: %v (SQLSTATE=%q)", i, err, SQLState(err))
		}
		txs = append(txs, tx)

		cursorName := fmt.Sprintf("p7_cur_%d", i)
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DECLARE %s CURSOR FOR SELECT id, val FROM p7_items ORDER BY id`, cursorName)); err != nil {
			t.Fatalf("DECLARE %s: %v (SQLSTATE=%q)", cursorName, err, SQLState(err))
		}
		var id int64
		var val string
		if err := tx.QueryRowContext(ctx, fmt.Sprintf(`FETCH 1 FROM %s`, cursorName)).Scan(&id, &val); err != nil {
			t.Fatalf("FETCH %s: %v (SQLSTATE=%q)", cursorName, err, SQLState(err))
		}
		if id != 1 || val != "v-1" {
			t.Fatalf("FETCH %s = (%d, %q), want (1, %q)", cursorName, id, val, "v-1")
		}
	}

	// While all 24 cursors stay open, also open 24 concurrent streaming queries.
	openRows := make([]*sql.Rows, 0, concurrent)
	defer func() {
		for _, rows := range openRows {
			_ = rows.Close()
		}
	}()
	for i := range concurrent {
		rows, err := db.QueryContext(ctx, `SELECT id, val FROM p7_items ORDER BY id`)
		if err != nil {
			t.Fatalf("QueryContext[%d]: %v (SQLSTATE=%q)", i, err, SQLState(err))
		}
		openRows = append(openRows, rows)
		if !rows.Next() {
			t.Fatalf("QueryContext[%d] returned no first row: %v", i, rows.Err())
		}
		var id int64
		var val string
		if err := rows.Scan(&id, &val); err != nil {
			t.Fatalf("Scan[%d]: %v", i, err)
		}
		if id != 1 || val != "v-1" {
			t.Fatalf("QueryContext[%d] first row = (%d, %q), want (1, %q)", i, id, val, "v-1")
		}
	}

	// Drain and close all open result sets and cursors cleanly.
	for i, rows := range openRows {
		count := 1
		for rows.Next() {
			count++
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows[%d].Err: %v", i, err)
		}
		if count != 10 {
			t.Fatalf("rows[%d] count = %d, want 10", i, count)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("rows[%d].Close: %v", i, err)
		}
	}
	openRows = nil

	for i, tx := range txs {
		if err := tx.Commit(); err != nil {
			t.Fatalf("tx[%d].Commit: %v", i, err)
		}
	}
	txs = nil
}

func TestPhase7MaintenanceBootstrapMaxGroupsScalesWithMaxTransactions(t *testing.T) {
	ctx := t.Context()
	catalog, err := Open(ctx, OpenOptions{
		Directory:       testDirectory(t),
		Clock:           newTestClock(),
		Parallelism:     1,
		MaxTransactions: 300,
		Maintenance:     MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if closeErr := catalog.Close(); closeErr != nil {
			t.Errorf("catalog.Close: %v", closeErr)
		}
	})

	// MaxTransactions=300 => maxGroups >= max(1024, 300*4, 1*4) = 1200.
	// Verify more than 1024 concurrent task groups are admitted without 53200.
	const wantGroups = 1100
	groups := make([]*runtime.Group, 0, wantGroups)
	defer func() {
		for _, g := range groups {
			_ = g.Close()
		}
	}()
	for i := range wantGroups {
		g, err := catalog.pool.NewGroup(ctx)
		if err != nil {
			t.Fatalf("NewGroup[%d]: %v (SQLSTATE=%q)", i, err, SQLState(err))
		}
		groups = append(groups, g)
	}
}
