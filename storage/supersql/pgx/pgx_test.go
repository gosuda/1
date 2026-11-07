package pgx_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha"
	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/pgx"
)

type testClock struct {
	mu    sync.Mutex
	now   clock.Instant
	scale clock.ScaleDescriptor
	epoch clock.Epoch
}

func newTestClock() *testClock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identifier := sha256.Sum256(history)
	return &testClock{
		now:   1000,
		scale: clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identifier, CanonicalLeapHistory: history},
		epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{1}, LeapHistory: identifier},
	}
}

func (c *testClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now += 1000
	return clock.Reading{Earliest: c.now, Latest: c.now, Status: clock.Synced, Epoch: c.epoch}, nil
}

func (c *testClock) WaitUntilAfter(ctx context.Context, value clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return clock.ErrClockEpochChanged
	}
	if value < 0 || value == math.MaxInt64 {
		return clock.ErrClockRange
	}
	c.now = max(c.now, (value/1000+1)*1000)
	return nil
}

func (c *testClock) Scale() clock.ScaleDescriptor {
	c.mu.Lock()
	defer c.mu.Unlock()
	owned := c.scale
	owned.CanonicalLeapHistory = append([]byte(nil), c.scale.CanonicalLeapHistory...)
	return owned
}

func (c *testClock) FromUTC(value time.Time) (clock.Instant, error) {
	return clock.Instant(value.UnixNano()), nil
}

func (c *testClock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	return clock.UTCProjection{Time: time.Unix(int64(value), 0).UTC()}, nil
}

func openTestPool(t *testing.T) *pgx.Pool {
	t.Helper()
	dir, err := os.MkdirTemp("", "ssql-pgx-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	pool, err := pgx.OpenPool(t.Context(), supersql.OpenOptions{
		Directory:   dir,
		Clock:       newTestClock(),
		Maintenance: supersql.MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenPool: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

type accountRow struct {
	ID      int64    `db:"id"`
	Name    string   `db:"name"`
	Balance int64    `db:"balance"`
	Tags    []string `db:"tags"`
}

func TestPgxCleanroomSuite(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}

	// 1. Exec & CommandTag
	ct, err := pool.Exec(ctx, `CREATE TABLE accounts (
		id bigint PRIMARY KEY,
		name text NOT NULL,
		balance bigint NOT NULL,
		tags text[] NOT NULL
	)`)
	if err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if ct.Empty() {
		t.Fatalf("expected non-empty CommandTag for CREATE TABLE")
	}

	// 2. NamedArgs & native Go slice parameter ([]string)
	ct, err = pool.Exec(ctx, `INSERT INTO accounts (id, name, balance, tags) VALUES (@id, @name, @balance, @tags)`,
		pgx.NamedArgs{
			"id":      1,
			"name":    "Alice",
			"balance": 50000,
			"tags":    []string{"vip", "founder"},
		})
	if err != nil {
		t.Fatalf("INSERT with NamedArgs: %v", err)
	}
	if !ct.Insert() || ct.RowsAffected() != 1 || ct.String() != "INSERT 0 1" {
		t.Fatalf("INSERT CommandTag = %q (rows=%d), want INSERT 0 1", ct.String(), ct.RowsAffected())
	}

	ct, err = pool.Exec(ctx, `INSERT INTO accounts (id, name, balance, tags) VALUES ($1, $2, $3, $4)`,
		2, "Bob", 25000, []string{"standard"})
	if err != nil {
		t.Fatalf("INSERT positional: %v", err)
	}

	// 3. QueryRow & native Go slice scan + ErrNoRows
	var gotName string
	var gotBalance int64
	var gotTags []string
	if err := pool.QueryRow(ctx, `SELECT name, balance, tags FROM accounts WHERE id = @id`, pgx.NamedArgs{"id": 1}).
		Scan(&gotName, &gotBalance, &gotTags); err != nil {
		t.Fatalf("QueryRow Scan: %v", err)
	}
	if gotName != "Alice" || gotBalance != 50000 || len(gotTags) != 2 || gotTags[0] != "vip" || gotTags[1] != "founder" {
		t.Fatalf("QueryRow got (%q, %d, %v)", gotName, gotBalance, gotTags)
	}

	var missing int64
	if err := pool.QueryRow(ctx, `SELECT id FROM accounts WHERE id = 999`).Scan(&missing); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows, got %v", err)
	}

	// 4. ANY(@ids) with []int64 and CollectRows + RowToStructByName
	rows, err := pool.Query(ctx, `SELECT id, name, balance, tags FROM accounts WHERE id = ANY(@ids) ORDER BY id`,
		pgx.NamedArgs{"ids": []int64{1, 2}})
	if err != nil {
		t.Fatalf("Query ANY(@ids): %v", err)
	}
	collected, err := pgx.CollectRows(rows, pgx.RowToStructByName[accountRow])
	if err != nil {
		t.Fatalf("CollectRows(RowToStructByName): %v", err)
	}
	if len(collected) != 2 || collected[0].Name != "Alice" || collected[1].Name != "Bob" {
		t.Fatalf("CollectRows got %+v", collected)
	}

	// 5. CollectOneRow, CollectExactlyOneRow, RowTo, RowToMap, ForEachRow
	rowsScalar, err := pool.Query(ctx, `SELECT balance FROM accounts WHERE id = 1`)
	if err != nil {
		t.Fatalf("Query scalar: %v", err)
	}
	bal, err := pgx.CollectExactlyOneRow(rowsScalar, pgx.RowTo[int64])
	if err != nil || bal != 50000 {
		t.Fatalf("CollectExactlyOneRow(RowTo[int64]) = %d, %v", bal, err)
	}

	rowsMap, err := pool.Query(ctx, `SELECT id, name FROM accounts WHERE id = 2`)
	if err != nil {
		t.Fatalf("Query map: %v", err)
	}
	m, err := pgx.CollectOneRow(rowsMap, pgx.RowToMap)
	if err != nil || m["name"] != "Bob" {
		t.Fatalf("CollectOneRow(RowToMap) = %v, %v", m, err)
	}

	rowsFE, err := pool.Query(ctx, `SELECT balance FROM accounts ORDER BY id`)
	if err != nil {
		t.Fatalf("Query ForEachRow: %v", err)
	}
	var sum int64
	var cur int64
	feTag, err := pgx.ForEachRow(rowsFE, []any{&cur}, func() error {
		sum += cur
		return nil
	})
	if err != nil || sum != 75000 || feTag.RowsAffected() != 2 {
		t.Fatalf("ForEachRow sum=%d tag=%q err=%v", sum, feTag.String(), err)
	}
}

func TestPgxTransactionsAndNestedSavepoints(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE ledger (id bigint PRIMARY KEY, amount bigint NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// 1. Default automatic escalation of ReadCommitted and RepeatableRead in BeginTx
	txRC, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		t.Fatalf("BeginTx(ReadCommitted) default escalation failed: %v", err)
	}
	if _, err := txRC.Exec(ctx, `INSERT INTO ledger VALUES (1, 100)`); err != nil {
		_ = txRC.Rollback(ctx)
		t.Fatalf("INSERT in txRC: %v", err)
	}
	// Nested transaction via tx.Begin(ctx) -> SAVEPOINT
	nested, err := txRC.Begin(ctx)
	if err != nil {
		_ = txRC.Rollback(ctx)
		t.Fatalf("nested Begin: %v", err)
	}
	if _, err := nested.Exec(ctx, `INSERT INTO ledger VALUES (2, 200)`); err != nil {
		_ = nested.Rollback(ctx)
		_ = txRC.Rollback(ctx)
		t.Fatalf("INSERT in nested tx: %v", err)
	}
	// Rollback nested savepoint; row 2 should disappear while row 1 remains
	if err := nested.Rollback(ctx); err != nil {
		_ = txRC.Rollback(ctx)
		t.Fatalf("nested Rollback: %v", err)
	}
	// Another nested savepoint that commits
	nested2, err := txRC.Begin(ctx)
	if err != nil {
		_ = txRC.Rollback(ctx)
		t.Fatalf("nested2 Begin: %v", err)
	}
	if _, err := nested2.Exec(ctx, `INSERT INTO ledger VALUES (3, 300)`); err != nil {
		_ = nested2.Rollback(ctx)
		_ = txRC.Rollback(ctx)
		t.Fatalf("INSERT in nested2: %v", err)
	}
	if err := nested2.Commit(ctx); err != nil {
		_ = txRC.Rollback(ctx)
		t.Fatalf("nested2 Commit: %v", err)
	}
	if err := txRC.Commit(ctx); err != nil {
		t.Fatalf("txRC Commit: %v", err)
	}

	rows, err := pool.Query(ctx, `SELECT id FROM ledger ORDER BY id`)
	if err != nil {
		t.Fatalf("SELECT ledger: %v", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil || len(ids) != 2 || ids[0] != 1 || ids[1] != 3 {
		t.Fatalf("expected ledger ids [1, 3], got %v (err=%v)", ids, err)
	}

	// 2. Strict isolation opt-out on an acquired connection
	if err := pool.AcquireFunc(ctx, func(pc *pgx.PoolConn) error {
		if _, err := pc.Exec(ctx, `SET supersql.compat_isolation = 'off'`); err != nil {
			return err
		}
		if tx, err := pc.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted}); err == nil {
			_ = tx.Rollback(ctx)
			t.Fatal("expected BeginTx(ReadCommitted) to fail when compat_isolation=off")
		}
		if _, err := pc.Exec(ctx, `RESET supersql.compat_isolation`); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("AcquireFunc strict isolation check: %v", err)
	}
}

func TestPgxBatchAndCopyFromAndRLS(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE tenant_items (
		id bigint PRIMARY KEY,
		tenant_id text NOT NULL,
		title text NOT NULL
	)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// 1. CopyFrom bulk insert
	copied, err := pool.CopyFrom(
		ctx,
		pgx.Identifier{"tenant_items"},
		[]string{"id", "tenant_id", "title"},
		pgx.CopyFromRows([][]any{
			{1, "t1", "Alpha"},
			{2, "t1", "Beta"},
			{3, "t2", "Gamma"},
		}),
	)
	if err != nil || copied != 3 {
		t.Fatalf("CopyFrom = %d, %v; want 3, nil", copied, err)
	}

	// 2. SendBatch with queued callbacks
	batch := &pgx.Batch{}
	var updatedRows int64
	batch.Queue(`UPDATE tenant_items SET title = @title WHERE id = @id`,
		pgx.NamedArgs{"id": 1, "title": "Alpha Prime"}).Exec(func(ct pgx.CommandTag) error {
		updatedRows = ct.RowsAffected()
		return nil
	})
	var gotTitle string
	batch.Queue(`SELECT title FROM tenant_items WHERE id = $1`, 1).QueryRow(func(row pgx.Row) error {
		return row.Scan(&gotTitle)
	})
	br := pool.SendBatch(ctx, batch)
	if err := br.Close(); err != nil {
		t.Fatalf("SendBatch Close: %v", err)
	}
	if updatedRows != 1 || gotTitle != "Alpha Prime" {
		t.Fatalf("SendBatch results: updatedRows=%d gotTitle=%q", updatedRows, gotTitle)
	}

	// 3. Multi-Tenant Row-Level Security via pgx.Pool.BeginFunc
	for _, ddl := range []string{
		`CREATE ROLE app_tenant`,
		`ALTER TABLE tenant_items ENABLE ROW LEVEL SECURITY`,
		`ALTER TABLE tenant_items FORCE ROW LEVEL SECURITY`,
		`CREATE POLICY tenant_isolation ON tenant_items FOR ALL TO app_tenant USING (tenant_id = current_setting('app.tenant_id', true)) WITH CHECK (tenant_id = current_setting('app.tenant_id', true))`,
	} {
		if _, err := pool.Exec(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	if err := pool.BeginFunc(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE app_tenant`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL app.tenant_id = 't1'`); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT title FROM tenant_items ORDER BY id`)
		if err != nil {
			return err
		}
		titles, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if len(titles) != 2 || titles[0] != "Alpha Prime" || titles[1] != "Beta" {
			t.Fatalf("tenant t1 visible titles = %v, want [Alpha Prime, Beta]", titles)
		}
		// Cross-tenant insert must be rejected by WITH CHECK (42501)
		if _, err := tx.Exec(ctx, `INSERT INTO tenant_items VALUES (4, 't2', 'Forbidden')`); err == nil {
			t.Fatal("expected cross-tenant INSERT to be rejected by RLS WITH CHECK")
		}
		return nil
	}); err != nil {
		t.Fatalf("BeginFunc RLS: %v", err)
	}
}
