package supersql

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

func TestPhase8ConnectorWithClosedCatalog(t *testing.T) {
	ctx := t.Context()

	t.Run("closed_catalog", func(t *testing.T) {
		cat, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := cat.Close(); err != nil {
			t.Fatalf("Catalog.Close: %v", err)
		}

		connector := NewConnectorWithCatalog(cat)
		if got := connector.Catalog(); got != nil {
			t.Fatalf("Connector.Catalog() on closed catalog = %v, want nil", got)
		}
		if _, err := connector.Connect(ctx); !errors.Is(err, ErrDatabaseClosed) {
			t.Fatalf("Connector.Connect on closed catalog = %v, want ErrDatabaseClosed", err)
		}

		db := sql.OpenDB(connector)
		defer func() { _ = db.Close() }()

		if err := db.PingContext(ctx); !errors.Is(err, ErrDatabaseClosed) {
			t.Fatalf("sql.DB.PingContext on closed catalog = %v, want ErrDatabaseClosed", err)
		}
		if _, err := db.ExecContext(ctx, `SET work_mem = '16MB'`); !errors.Is(err, ErrDatabaseClosed) {
			t.Fatalf("sql.DB.ExecContext on closed catalog = %v, want ErrDatabaseClosed", err)
		}
		if _, err := db.QueryContext(ctx, `SHOW work_mem`); !errors.Is(err, ErrDatabaseClosed) {
			t.Fatalf("sql.DB.QueryContext on closed catalog = %v, want ErrDatabaseClosed", err)
		}
		if _, err := db.PrepareContext(ctx, `SELECT 1`); !errors.Is(err, ErrDatabaseClosed) {
			t.Fatalf("sql.DB.PrepareContext on closed catalog = %v, want ErrDatabaseClosed", err)
		}
		if _, err := db.BeginTx(ctx, nil); !errors.Is(err, ErrDatabaseClosed) {
			t.Fatalf("sql.DB.BeginTx on closed catalog = %v, want ErrDatabaseClosed", err)
		}
		if err := connector.Close(); err != nil {
			t.Fatalf("Connector.Close on closed catalog = %v", err)
		}
	})

	t.Run("nil_catalog", func(t *testing.T) {
		connector := NewConnectorWithCatalog(nil)
		if got := connector.Catalog(); got != nil {
			t.Fatalf("Connector.Catalog() on nil catalog = %v, want nil", got)
		}
		if _, err := connector.Connect(ctx); !errors.Is(err, ErrDatabaseClosed) {
			t.Fatalf("Connector.Connect on nil catalog = %v, want ErrDatabaseClosed", err)
		}
		if err := connector.Close(); err != nil {
			t.Fatalf("Connector.Close on nil catalog = %v, want nil", err)
		}
	})
}

func TestPhase8ConnectionOperationsAfterCatalogClose(t *testing.T) {
	ctx := t.Context()

	cat, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	connector := NewConnectorWithCatalog(cat)

	rawConn, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	conn := rawConn.(*connection)
	defer func() { _ = conn.Close() }()

	if err := conn.Ping(ctx); err != nil {
		t.Fatalf("Ping while open: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SET work_mem = '16MB'`, nil); err != nil {
		t.Fatalf("SET while open: %v", err)
	}
	rawStmt, err := conn.PrepareContext(ctx, `SELECT 1`)
	if err != nil {
		t.Fatalf("PrepareContext while open: %v", err)
	}
	stmt := rawStmt.(*statement)
	defer func() { _ = stmt.Close() }()

	// Also open a second raw connection with an active transaction.
	txRawConn, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect (tx): %v", err)
	}
	txConn := txRawConn.(*connection)
	defer func() { _ = txConn.Close() }()
	tx, err := txConn.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatalf("BeginTx while open: %v", err)
	}

	// Also warm a pooled connection inside sql.DB.
	db := sql.OpenDB(connector)
	defer func() { _ = db.Close() }()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("db.PingContext while open: %v", err)
	}

	// Close the underlying Catalog directly without closing the Connector or connections.
	if err := cat.Close(); err != nil {
		t.Fatalf("Catalog.Close: %v", err)
	}

	if got := connector.Catalog(); got != nil {
		t.Fatalf("Connector.Catalog() after Catalog.Close = %v, want nil", got)
	}
	if _, err := cat.newSession(ctx); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("Catalog.newSession after close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := connector.Connect(ctx); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("Connector.Connect after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}

	// All operations on the already-open driver connection must fail with ErrDatabaseClosed.
	if err := conn.Ping(ctx); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("conn.Ping after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := conn.PrepareContext(ctx, `SELECT 1`); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("conn.PrepareContext after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := conn.ExecContext(ctx, `SET work_mem = '32MB'`, nil); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("conn.ExecContext(SET) after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := conn.QueryContext(ctx, `SHOW work_mem`, nil); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("conn.QueryContext(SHOW) after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := conn.BeginTx(ctx, driver.TxOptions{}); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("conn.BeginTx after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, _, _, err := conn.DescribeQuery(ctx, `SELECT 1`, nil); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("conn.DescribeQuery after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if conn.IsValid() {
		t.Fatal("conn.IsValid() after Catalog.Close = true, want false")
	}
	if err := conn.ResetSession(ctx); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("conn.ResetSession after Catalog.Close = %v, want driver.ErrBadConn", err)
	}

	// Previously prepared statement on the open connection must also fail with ErrDatabaseClosed.
	if _, err := stmt.QueryContext(ctx, nil); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("stmt.QueryContext after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := stmt.ExecContext(ctx, nil); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("stmt.ExecContext after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}

	// Active transaction Commit/Rollback must fail cleanly with ErrDatabaseClosed.
	if err := tx.Commit(); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("tx.Commit after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}

	// Pooled sql.DB operations must discard the stale connection and surface ErrDatabaseClosed.
	if err := db.PingContext(ctx); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("db.PingContext after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := db.ExecContext(ctx, `SET work_mem = '32MB'`); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("db.ExecContext(SET) after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := db.QueryContext(ctx, `SHOW work_mem`); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("db.QueryContext(SHOW) after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := db.PrepareContext(ctx, `SELECT 1`); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("db.PrepareContext after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
	if _, err := db.BeginTx(ctx, nil); !errors.Is(err, ErrDatabaseClosed) {
		t.Fatalf("db.BeginTx after Catalog.Close = %v, want ErrDatabaseClosed", err)
	}
}

func readDriverRows(t *testing.T, rows driver.Rows) ([]string, [][]driver.Value) {
	t.Helper()
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("rows.Close: %v", err)
		}
	}()
	cols := rows.Columns()
	var result [][]driver.Value
	for {
		dest := make([]driver.Value, len(cols))
		err := rows.Next(dest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("rows.Next: %v", err)
		}
		result = append(result, dest)
	}
	return cols, result
}

func closedReadViewSnapshot(t *testing.T, ctx context.Context, manager *txn.Manager) (ts int64, identity txn.CatalogReadIdentity) {
	t.Helper()
	view, err := manager.ClosedReadView(ctx)
	if err != nil {
		t.Fatalf("ClosedReadView: %v", err)
	}
	defer func() { _ = view.Close() }()
	identified, ok := view.(interface{ CatalogIdentity() txn.CatalogReadIdentity })
	if !ok {
		t.Fatalf("ClosedReadView does not implement CatalogIdentity")
	}
	return int64(view.Timestamp()), identified.CatalogIdentity()
}

func TestPhase8PreparedPlanCacheSurvivesDML(t *testing.T) {
	ctx := t.Context()
	connector := mustConnector(t, ctx)

	rawA, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect A: %v", err)
	}
	connA := rawA.(*connection)
	defer func() { _ = connA.Close() }()

	rawB, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect B: %v", err)
	}
	connB := rawB.(*connection)
	defer func() { _ = connB.Close() }()

	if _, err := connA.ExecContext(ctx, `CREATE TABLE items (id bigint PRIMARY KEY, val text)`, nil); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	rawSel, err := connA.PrepareContext(ctx, `SELECT id, val FROM items ORDER BY id`)
	if err != nil {
		t.Fatalf("Prepare SELECT: %v", err)
	}
	selStmt := rawSel.(*statement)
	defer func() { _ = selStmt.Close() }()

	rawIns, err := connA.PrepareContext(ctx, `INSERT INTO items VALUES ($1, $2)`)
	if err != nil {
		t.Fatalf("Prepare INSERT: %v", err)
	}
	insStmt := rawIns.(*statement)
	defer func() { _ = insStmt.Close() }()

	// First SELECT populates the cache (1 miss, 0 hits).
	rows, err := selStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("SELECT initial: %v", err)
	}
	_, got := readDriverRows(t, rows)
	if len(got) != 0 {
		t.Fatalf("initial rows = %v, want empty", got)
	}
	if selStmt.planCacheMisses.Load() != 1 || selStmt.planCacheHits.Load() != 0 {
		t.Fatalf("after initial SELECT: hits=%d misses=%d, want hits=0 misses=1",
			selStmt.planCacheHits.Load(), selStmt.planCacheMisses.Load())
	}
	selStmt.mu.Lock()
	firstEntry := selStmt.cached
	selStmt.mu.Unlock()
	if firstEntry == nil || !firstEntry.identity.Live || firstEntry.identity.Fixed != 0 {
		t.Fatalf("expected live CatalogReadIdentity in cache, got %+v", firstEntry)
	}

	tsBefore, idBefore := closedReadViewSnapshot(t, ctx, connector.Catalog().manager)
	if !idBefore.Live || idBefore.Fixed != 0 {
		t.Fatalf("ClosedReadView identity = %+v, want Live:true Fixed:0", idBefore)
	}

	// Repeated DML commits (INSERT, UPDATE, DELETE) across both connections must advance
	// ClosedTS while keeping CatalogReadIdentity stable so the prepared SELECT hits cache.
	dmlSteps := []struct {
		name      string
		run       func() error
		wantRows  [][]driver.Value
	}{
		{
			name: "prepared_insert_1",
			run: func() error {
				_, err := insStmt.ExecContext(ctx, []driver.NamedValue{
					{Ordinal: 1, Value: int64(1)},
					{Ordinal: 2, Value: "alpha"},
				})
				return err
			},
			wantRows: [][]driver.Value{{int64(1), "alpha"}},
		},
		{
			name: "prepared_insert_2",
			run: func() error {
				_, err := insStmt.ExecContext(ctx, []driver.NamedValue{
					{Ordinal: 1, Value: int64(2)},
					{Ordinal: 2, Value: "beta"},
				})
				return err
			},
			wantRows: [][]driver.Value{{int64(1), "alpha"}, {int64(2), "beta"}},
		},
		{
			name: "other_conn_insert_3",
			run: func() error {
				_, err := connB.ExecContext(ctx, `INSERT INTO items VALUES (3, 'gamma')`, nil)
				return err
			},
			wantRows: [][]driver.Value{{int64(1), "alpha"}, {int64(2), "beta"}, {int64(3), "gamma"}},
		},
		{
			name: "other_conn_update",
			run: func() error {
				_, err := connB.ExecContext(ctx, `UPDATE items SET val = 'alpha-updated' WHERE id = 1`, nil)
				return err
			},
			wantRows: [][]driver.Value{{int64(1), "alpha-updated"}, {int64(2), "beta"}, {int64(3), "gamma"}},
		},
		{
			name: "same_conn_delete",
			run: func() error {
				_, err := connA.ExecContext(ctx, `DELETE FROM items WHERE id = 2`, nil)
				return err
			},
			wantRows: [][]driver.Value{{int64(1), "alpha-updated"}, {int64(3), "gamma"}},
		},
	}

	prevTS := tsBefore
	for i, step := range dmlSteps {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		tsAfter, idAfter := closedReadViewSnapshot(t, ctx, connector.Catalog().manager)
		if tsAfter <= prevTS {
			t.Fatalf("%s: expected ClosedTS to advance (%d -> %d)", step.name, prevTS, tsAfter)
		}
		if idAfter != idBefore {
			t.Fatalf("%s: ClosedReadView CatalogIdentity changed across DML: before=%+v after=%+v", step.name, idBefore, idAfter)
		}
		prevTS = tsAfter

		rows, err := selStmt.QueryContext(ctx, nil)
		if err != nil {
			t.Fatalf("%s: prepared SELECT: %v", step.name, err)
		}
		_, gotRows := readDriverRows(t, rows)
		if len(gotRows) != len(step.wantRows) {
			t.Fatalf("%s: got %d rows (%v), want %d (%v)", step.name, len(gotRows), gotRows, len(step.wantRows), step.wantRows)
		}
		for r := range gotRows {
			if !slices.Equal(gotRows[r], step.wantRows[r]) {
				t.Fatalf("%s: row %d = %v, want %v", step.name, r, gotRows[r], step.wantRows[r])
			}
		}

		wantHits := uint64(i + 1)
		if selStmt.planCacheHits.Load() != wantHits || selStmt.planCacheMisses.Load() != 1 {
			t.Fatalf("%s: plan cache hits=%d misses=%d, want hits=%d misses=1",
				step.name, selStmt.planCacheHits.Load(), selStmt.planCacheMisses.Load(), wantHits)
		}
		selStmt.mu.Lock()
		currentEntry := selStmt.cached
		selStmt.mu.Unlock()
		if currentEntry == nil || currentEntry.prepared != firstEntry.prepared {
			t.Fatalf("%s: cached prepared plan pointer changed after DML", step.name)
		}
	}

	// Verify the prepared INSERT also hit its cached plan on the second invocation.
	if insStmt.planCacheMisses.Load() != 1 || insStmt.planCacheHits.Load() != 1 {
		t.Fatalf("prepared INSERT cache: hits=%d misses=%d, want hits=1 misses=1",
			insStmt.planCacheHits.Load(), insStmt.planCacheMisses.Load())
	}
}

func TestPhase8PreparedPlanCacheInvalidatedOnDDL(t *testing.T) {
	ctx := t.Context()
	connector := mustConnector(t, ctx)

	rawA, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect A: %v", err)
	}
	connA := rawA.(*connection)
	defer func() { _ = connA.Close() }()

	rawB, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect B: %v", err)
	}
	connB := rawB.(*connection)
	defer func() { _ = connB.Close() }()

	if _, err := connA.ExecContext(ctx, `CREATE TABLE widgets (id bigint PRIMARY KEY, name text)`, nil); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := connA.ExecContext(ctx, `INSERT INTO widgets VALUES (1, 'sprocket')`, nil); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	rawSel, err := connA.PrepareContext(ctx, `SELECT * FROM widgets ORDER BY id`)
	if err != nil {
		t.Fatalf("Prepare SELECT *: %v", err)
	}
	selStmt := rawSel.(*statement)
	defer func() { _ = selStmt.Close() }()

	// Initial miss populates cache.
	rows, err := selStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	cols, got := readDriverRows(t, rows)
	if !slices.Equal(cols, []string{"id", "name"}) {
		t.Fatalf("initial cols = %v, want [id name]", cols)
	}
	if len(got) != 1 || !slices.Equal(got[0], []driver.Value{int64(1), "sprocket"}) {
		t.Fatalf("initial rows = %v", got)
	}
	selStmt.mu.Lock()
	planV1 := selStmt.cached.prepared
	idV1 := selStmt.cached.identity
	selStmt.mu.Unlock()

	// DML on connB preserves cache hit.
	if _, err := connB.ExecContext(ctx, `INSERT INTO widgets VALUES (2, 'gear')`, nil); err != nil {
		t.Fatalf("INSERT 2: %v", err)
	}
	rows, err = selStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("SELECT 2: %v", err)
	}
	_, got = readDriverRows(t, rows)
	if len(got) != 2 {
		t.Fatalf("rows after DML = %v, want 2 rows", got)
	}
	if selStmt.planCacheHits.Load() != 1 || selStmt.planCacheMisses.Load() != 1 {
		t.Fatalf("after DML: hits=%d misses=%d, want 1/1", selStmt.planCacheHits.Load(), selStmt.planCacheMisses.Load())
	}

	// Cross-connection ALTER TABLE ADD COLUMN on connB must invalidate connA's cached plan
	// even though connA's session-local ddlSeq did not change.
	if _, err := connB.ExecContext(ctx, `ALTER TABLE widgets ADD COLUMN weight text DEFAULT 'standard'`, nil); err != nil {
		t.Fatalf("ALTER TABLE: %v", err)
	}
	rows, err = selStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("SELECT after ALTER TABLE: %v", err)
	}
	cols, got = readDriverRows(t, rows)
	if !slices.Equal(cols, []string{"id", "name", "weight"}) {
		t.Fatalf("cols after ALTER = %v, want [id name weight]", cols)
	}
	if len(got) != 2 || !slices.Equal(got[0], []driver.Value{int64(1), "sprocket", "standard"}) {
		t.Fatalf("rows after ALTER = %v", got)
	}
	if selStmt.planCacheMisses.Load() != 2 || selStmt.planCacheHits.Load() != 1 {
		t.Fatalf("after ALTER: hits=%d misses=%d, want 1/2", selStmt.planCacheHits.Load(), selStmt.planCacheMisses.Load())
	}
	selStmt.mu.Lock()
	planV2 := selStmt.cached.prepared
	idV2 := selStmt.cached.identity
	selStmt.mu.Unlock()
	if planV2 == planV1 {
		t.Fatal("expected new prepared plan after ALTER TABLE")
	}
	if !idV2.Live || idV2.Version <= idV1.Version {
		t.Fatalf("expected CatalogReadIdentity.Version to advance after ALTER: v1=%+v v2=%+v", idV1, idV2)
	}

	// Next execution after ALTER hits cache again.
	rows, err = selStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("SELECT repeat after ALTER: %v", err)
	}
	_, _ = readDriverRows(t, rows)
	if selStmt.planCacheHits.Load() != 2 || selStmt.planCacheMisses.Load() != 2 {
		t.Fatalf("after repeat post-ALTER: hits=%d misses=%d, want 2/2", selStmt.planCacheHits.Load(), selStmt.planCacheMisses.Load())
	}

	// Cross-connection DROP TABLE + CREATE TABLE with new schema must also invalidate.
	if _, err := connB.ExecContext(ctx, `DROP TABLE widgets`, nil); err != nil {
		t.Fatalf("DROP TABLE: %v", err)
	}
	if _, err := connB.ExecContext(ctx, `CREATE TABLE widgets (id bigint PRIMARY KEY, label text, active bool)`, nil); err != nil {
		t.Fatalf("re-CREATE TABLE: %v", err)
	}
	if _, err := connB.ExecContext(ctx, `INSERT INTO widgets VALUES (10, 'bolt', true)`, nil); err != nil {
		t.Fatalf("INSERT after recreate: %v", err)
	}

	rows, err = selStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("SELECT after DROP+CREATE: %v", err)
	}
	cols, got = readDriverRows(t, rows)
	if !slices.Equal(cols, []string{"id", "label", "active"}) {
		t.Fatalf("cols after DROP+CREATE = %v, want [id label active]", cols)
	}
	if len(got) != 1 || !slices.Equal(got[0], []driver.Value{int64(10), "bolt", true}) {
		t.Fatalf("rows after DROP+CREATE = %v", got)
	}
	if selStmt.planCacheMisses.Load() != 3 || selStmt.planCacheHits.Load() != 2 {
		t.Fatalf("after DROP+CREATE: hits=%d misses=%d, want 2/3", selStmt.planCacheHits.Load(), selStmt.planCacheMisses.Load())
	}
	selStmt.mu.Lock()
	planV3 := selStmt.cached.prepared
	idV3 := selStmt.cached.identity
	selStmt.mu.Unlock()
	if planV3 == planV2 || !idV3.Live || idV3.Version <= idV2.Version {
		t.Fatalf("expected invalidated plan and bumped version after DROP+CREATE: v2=%+v v3=%+v", idV2, idV3)
	}
}

func TestPhase8HistoricalReadDoesNotPollutePlanCache(t *testing.T) {
	ctx := t.Context()
	testClock := newTestClock()
	cat, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: testClock})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	connector := NewConnectorWithCatalog(cat)
	t.Cleanup(func() {
		if err := connector.Close(); err != nil {
			t.Errorf("connector.Close: %v", err)
		}
	})

	rawConn, err := connector.Connect(ctx)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	conn := rawConn.(*connection)
	defer func() { _ = conn.Close() }()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE hist (id bigint PRIMARY KEY, val text)`, nil); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO hist VALUES (1, 'before')`, nil); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// Warm the live prepared plan cache on the pre-DDL schema.
	rawLiveStmt, err := conn.PrepareContext(ctx, `SELECT * FROM hist ORDER BY id`)
	if err != nil {
		t.Fatalf("Prepare live SELECT: %v", err)
	}
	liveStmt := rawLiveStmt.(*statement)
	defer func() { _ = liveStmt.Close() }()

	rows, err := liveStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("live SELECT before DDL: %v", err)
	}
	cols, got := readDriverRows(t, rows)
	if !slices.Equal(cols, []string{"id", "val"}) || len(got) != 1 || !slices.Equal(got[0], []driver.Value{int64(1), "before"}) {
		t.Fatalf("live pre-DDL = cols=%v rows=%v", cols, got)
	}

	// Capture a certified timestamp before updating data and altering schema.
	reading, err := testClock.Read(ctx)
	if err != nil {
		t.Fatalf("clock.Read: %v", err)
	}
	beforeInstant := time.Unix(0, int64(reading.Latest)).UTC()
	beforeTS := int64(reading.Latest)

	if _, err := conn.ExecContext(ctx, `UPDATE hist SET val = 'after' WHERE id = 1`, nil); err != nil {
		t.Fatalf("UPDATE: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE hist ADD COLUMN tag text DEFAULT 'v2'`, nil); err != nil {
		t.Fatalf("ALTER TABLE: %v", err)
	}

	// Historical AS OF SYSTEM TIME query (both prepared and direct) must see the
	// historical 2-column schema and 'before' value, and must not populate statement.cached.
	histSQL := fmt.Sprintf(`SELECT * FROM hist AS OF SYSTEM TIME '%s'::timestamptz ORDER BY id`,
		beforeInstant.Format("2006-01-02 15:04:05.999999999-07:00"))
	rawHistStmt, err := conn.PrepareContext(ctx, histSQL)
	if err != nil {
		t.Fatalf("Prepare historical SELECT: %v", err)
	}
	histStmt := rawHistStmt.(*statement)
	defer func() { _ = histStmt.Close() }()

	rows, err = histStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("historical SELECT: %v", err)
	}
	cols, got = readDriverRows(t, rows)
	if !slices.Equal(cols, []string{"id", "val"}) {
		t.Fatalf("historical cols = %v, want [id val]", cols)
	}
	if len(got) != 1 || !slices.Equal(got[0], []driver.Value{int64(1), "before"}) {
		t.Fatalf("historical rows = %v, want [[1 before]]", got)
	}
	histStmt.mu.Lock()
	histCached := histStmt.cached
	histStmt.mu.Unlock()
	if histCached != nil {
		t.Fatalf("historical AS OF SYSTEM TIME statement should not populate plan cache, got %+v", histCached)
	}

	// Verify ClosedReadView uses Live:true while historical reads stay disjoint (Fixed:ts).
	liveTS, liveIdentity := closedReadViewSnapshot(t, ctx, cat.manager)
	if !liveIdentity.Live || liveIdentity.Fixed != 0 || liveTS <= beforeTS {
		t.Fatalf("ClosedReadView after DDL = ts:%d id:%+v (beforeTS=%d)", liveTS, liveIdentity, beforeTS)
	}

	// Live prepared statement must re-plan for the post-DDL schema and then hit cache across DML.
	rows, err = liveStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("live SELECT after DDL: %v", err)
	}
	cols, got = readDriverRows(t, rows)
	if !slices.Equal(cols, []string{"id", "val", "tag"}) {
		t.Fatalf("live post-DDL cols = %v, want [id val tag]", cols)
	}
	if len(got) != 1 || !slices.Equal(got[0], []driver.Value{int64(1), "after", "v2"}) {
		t.Fatalf("live post-DDL rows = %v, want [[1 after v2]]", got)
	}
	if liveStmt.planCacheMisses.Load() != 2 {
		t.Fatalf("liveStmt misses after DDL = %d, want 2", liveStmt.planCacheMisses.Load())
	}

	if _, err := conn.ExecContext(ctx, `INSERT INTO hist VALUES (2, 'latest', 'v2')`, nil); err != nil {
		t.Fatalf("INSERT post-DDL: %v", err)
	}
	rows, err = liveStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("live SELECT after post-DDL DML: %v", err)
	}
	_, got = readDriverRows(t, rows)
	if len(got) != 2 {
		t.Fatalf("live rows after post-DDL DML = %v, want 2 rows", got)
	}
	if liveStmt.planCacheHits.Load() != 1 || liveStmt.planCacheMisses.Load() != 2 {
		t.Fatalf("liveStmt after post-DDL DML: hits=%d misses=%d, want 1/2",
			liveStmt.planCacheHits.Load(), liveStmt.planCacheMisses.Load())
	}

	// Re-run historical SELECT again to prove the live cache updates never affect it.
	rows, err = histStmt.QueryContext(ctx, nil)
	if err != nil {
		t.Fatalf("historical SELECT repeat: %v", err)
	}
	cols, got = readDriverRows(t, rows)
	if !slices.Equal(cols, []string{"id", "val"}) || len(got) != 1 || !slices.Equal(got[0], []driver.Value{int64(1), "before"}) {
		t.Fatalf("historical repeat = cols=%v rows=%v, want [id val] [[1 before]]", cols, got)
	}
}
