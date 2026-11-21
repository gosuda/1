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

func TestPgxPgErrorMapping(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE err_items (id bigint PRIMARY KEY, val bigint CHECK (val > 0))`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO err_items VALUES (1, 10)`); err != nil {
		t.Fatal(err)
	}

	assertPgErr := func(t *testing.T, err error, wantCode string) *pgx.PgError {
		t.Helper()
		if err == nil {
			t.Fatalf("expected error with SQLSTATE %s, got nil", wantCode)
		}
		var pgErr *pgx.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("expected *pgx.PgError, got %T: %v", err, err)
		}
		if pgErr.Code != wantCode || pgErr.SQLState() != wantCode {
			t.Fatalf("PgError.Code = %q (SQLState=%q), want %q (err=%v)", pgErr.Code, pgErr.SQLState(), wantCode, pgErr)
		}
		if pgErr.Severity != "ERROR" {
			t.Fatalf("PgError.Severity = %q, want ERROR", pgErr.Severity)
		}
		var sErr *supersql.Error
		if !errors.As(err, &sErr) {
			t.Fatalf("PgError.Unwrap() should preserve *supersql.Error chain")
		}
		return pgErr
	}

	// Pool.Exec unique violation
	_, err := pool.Exec(ctx, `INSERT INTO err_items VALUES (1, 20)`)
	assertPgErr(t, err, "23505")

	// Pool.Query undefined table
	_, err = pool.Query(ctx, `SELECT * FROM no_such_table`)
	assertPgErr(t, err, "42P01")

	// Pool.QueryRow check violation / division by zero
	var v int64
	err = pool.QueryRow(ctx, `SELECT 1 / 0`).Scan(&v)
	assertPgErr(t, err, "22012")

	// Tx.Exec check constraint violation
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO err_items VALUES (2, -5)`)
	assertPgErr(t, err, "23514")
	_ = tx.Rollback(ctx)

	// CopyFrom duplicate key
	_, err = pool.CopyFrom(ctx, pgx.Identifier{"err_items"}, []string{"id", "val"}, pgx.CopyFromRows([][]any{{1, 5}}))
	assertPgErr(t, err, "23505")

	// SendBatch error
	batch := &pgx.Batch{}
	batch.Queue(`SELECT * FROM nonexistent_batch_tbl`)
	err = pool.SendBatch(ctx, batch).Close()
	assertPgErr(t, err, "42P01")
}

func TestPgxFieldDescriptionsOIDs(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	rows, err := pool.Query(ctx, `SELECT
		true::bool AS c_bool,
		'\xdeadbeef'::bytea AS c_bytea,
		1::int8 AS c_int8,
		2::int2 AS c_int2,
		3::int4 AS c_int4,
		'hi'::text AS c_text,
		42::oid AS c_oid,
		'{"a":1}'::json AS c_json,
		1.5::float4 AS c_float4,
		2.5::float8 AS c_float8,
		'ab'::char(2) AS c_bpchar,
		'cd'::varchar(10) AS c_varchar,
		DATE '2026-01-02' AS c_date,
		TIME '12:34:56' AS c_time,
		TIMESTAMP '2026-01-02 12:34:56' AS c_ts,
		TIMESTAMPTZ '2026-01-02 12:34:56+00' AS c_tstz,
		INTERVAL '1 day' AS c_iv,
		12.34::numeric AS c_num,
		'550e8400-e29b-41d4-a716-446655440000'::uuid AS c_uuid,
		'{"b":2}'::jsonb AS c_jsonb,
		ARRAY[true,false]::bool[] AS c_abool,
		ARRAY[1,2]::int4[] AS c_aint4,
		ARRAY['x','y']::text[] AS c_atext,
		ARRAY[10,20]::int8[] AS c_aint8
	`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	want := []struct {
		name string
		oid  uint32
	}{
		{"c_bool", 16},
		{"c_bytea", 17},
		{"c_int8", 20},
		{"c_int2", 21},
		{"c_int4", 23},
		{"c_text", 25},
		{"c_oid", 26},
		{"c_json", 114},
		{"c_float4", 700},
		{"c_float8", 701},
		{"c_bpchar", 1042},
		{"c_varchar", 1043},
		{"c_date", 1082},
		{"c_time", 1083},
		{"c_ts", 1114},
		{"c_tstz", 1184},
		{"c_iv", 1186},
		{"c_num", 1700},
		{"c_uuid", 2950},
		{"c_jsonb", 3802},
		{"c_abool", 1000},
		{"c_aint4", 1007},
		{"c_atext", 1009},
		{"c_aint8", 1016},
	}

	fds := rows.FieldDescriptions()
	if len(fds) != len(want) {
		t.Fatalf("FieldDescriptions len = %d, want %d", len(fds), len(want))
	}
	for i, w := range want {
		if fds[i].Name != w.name || fds[i].DataTypeOID != w.oid {
			t.Errorf("col %d: got (%q, oid=%d), want (%q, oid=%d)", i, fds[i].Name, fds[i].DataTypeOID, w.name, w.oid)
		}
	}
}

func TestPgxSendBatchImplicitTransaction(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE batch_tx (id bigint PRIMARY KEY, name text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO batch_tx VALUES (1, 'seed')`); err != nil {
		t.Fatal(err)
	}

	// 1. Failing second item in Pool.SendBatch must roll back the first item.
	bFail := &pgx.Batch{}
	bFail.Queue(`INSERT INTO batch_tx VALUES (10, 'should-rollback')`)
	bFail.Queue(`INSERT INTO batch_tx VALUES (1, 'dup-pk-fails')`)
	br := pool.SendBatch(ctx, bFail)
	if _, err := br.Exec(); err != nil {
		t.Fatalf("first batch item unexpectedly failed: %v", err)
	}
	if _, err := br.Exec(); err == nil {
		t.Fatal("expected duplicate PK error on second batch item")
	}
	if err := br.Close(); err == nil {
		t.Fatal("expected Close to return batch error")
	}
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM batch_tx WHERE id = 10`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("Pool.SendBatch did not roll back item 1 on failure: count(id=10)=%d", count)
	}

	// 2. Failing second item on Conn.SendBatch must also roll back.
	if err := pool.AcquireFunc(ctx, func(pc *pgx.PoolConn) error {
		bConnFail := &pgx.Batch{}
		bConnFail.Queue(`INSERT INTO batch_tx VALUES (11, 'conn-rollback')`)
		bConnFail.Queue(`INSERT INTO batch_tx VALUES (1, 'dup')`)
		if err := pc.SendBatch(ctx, bConnFail).Close(); err == nil {
			t.Fatal("expected Conn.SendBatch failure")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM batch_tx WHERE id = 11`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("Conn.SendBatch did not roll back item 1 on failure: count(id=11)=%d", count)
	}

	// 3. Successful Pool.SendBatch commits all items on Close.
	bOK := &pgx.Batch{}
	bOK.Queue(`INSERT INTO batch_tx VALUES (20, 'committed-a')`)
	bOK.Queue(`INSERT INTO batch_tx VALUES (21, 'committed-b')`)
	var seen string
	bOK.Queue(`SELECT name FROM batch_tx WHERE id = 20`).QueryRow(func(r pgx.Row) error {
		return r.Scan(&seen)
	})
	if err := pool.SendBatch(ctx, bOK).Close(); err != nil {
		t.Fatalf("successful batch Close: %v", err)
	}
	if seen != "committed-a" {
		t.Fatalf("batch read-your-own-writes = %q, want committed-a", seen)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM batch_tx WHERE id IN (20, 21)`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 committed rows, got %d", count)
	}

	// 4. Tx.SendBatch participates in caller's transaction (rolling back outer Tx undoes batch).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bTx := &pgx.Batch{}
	bTx.Queue(`INSERT INTO batch_tx VALUES (30, 'in-tx')`)
	if err := tx.SendBatch(ctx, bTx).Close(); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("Tx.SendBatch: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM batch_tx WHERE id = 30`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("Tx.SendBatch committed prematurely: count(id=30)=%d", count)
	}
}

func TestPgxCommandTagCommentsAndCTEs(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	ct, err := pool.Exec(ctx, "/* header /* nested */ */\n-- line comment\nCREATE TABLE ct_tbl (id bigint PRIMARY KEY, v text)")
	if err != nil {
		t.Fatal(err)
	}
	if ct.String() != "CREATE TABLE" {
		t.Fatalf("CREATE TABLE CommandTag = %q, want CREATE TABLE", ct.String())
	}

	// WITH ... INSERT ... RETURNING -> Rows.CommandTag() must be "INSERT 0 2"
	rows, err := pool.Query(ctx, `
		-- leading comment
		/* block comment */
		WITH src(id, v) AS (VALUES (1, 'a'), (2, 'b'))
		INSERT INTO ct_tbl (id, v) SELECT id, v FROM src RETURNING id
	`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rtag := rows.CommandTag()
	if !rtag.Insert() || rtag.RowsAffected() != 2 || rtag.String() != "INSERT 0 2" {
		t.Fatalf("WITH INSERT RETURNING Rows.CommandTag = %q (rows=%d), want INSERT 0 2", rtag.String(), rtag.RowsAffected())
	}

	// WITH ... UPDATE ... RETURNING -> "UPDATE 1"
	uRows, err := pool.Query(ctx, `WITH target AS (SELECT 1 AS id) UPDATE ct_tbl SET v = 'z' FROM target WHERE ct_tbl.id = target.id RETURNING ct_tbl.id`)
	if err != nil {
		t.Fatal(err)
	}
	for uRows.Next() {
		var id int64
		_ = uRows.Scan(&id)
	}
	uRows.Close()
	if tag := uRows.CommandTag(); !tag.Update() || tag.String() != "UPDATE 1" {
		t.Fatalf("WITH UPDATE RETURNING CommandTag = %q, want UPDATE 1", tag.String())
	}

	// WITH ... DELETE ... RETURNING -> "DELETE 2"
	dRows, err := pool.Query(ctx, `/* del */ WITH doomed AS (SELECT id FROM ct_tbl) DELETE FROM ct_tbl WHERE id IN (SELECT id FROM doomed) RETURNING id`)
	if err != nil {
		t.Fatal(err)
	}
	for dRows.Next() {
		var id int64
		_ = dRows.Scan(&id)
	}
	dRows.Close()
	if tag := dRows.CommandTag(); !tag.Delete() || tag.String() != "DELETE 2" {
		t.Fatalf("WITH DELETE RETURNING CommandTag = %q, want DELETE 2", tag.String())
	}
}

func TestPgxArrayScanningAndUnsignedBounds(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	// 1. NULL array elements + quoted "NULL"
	var ptrStrings []*string
	if err := pool.QueryRow(ctx, `SELECT ARRAY['a', NULL, 'NULL', 'b,c']::text[]`).Scan(&ptrStrings); err != nil {
		t.Fatalf("scan []*string: %v", err)
	}
	if len(ptrStrings) != 4 || ptrStrings[0] == nil || *ptrStrings[0] != "a" ||
		ptrStrings[1] != nil ||
		ptrStrings[2] == nil || *ptrStrings[2] != "NULL" ||
		ptrStrings[3] == nil || *ptrStrings[3] != "b,c" {
		t.Fatalf("unexpected []*string: %+v", ptrStrings)
	}

	var pgTexts []pgx.Text
	if err := pool.QueryRow(ctx, `SELECT ARRAY['x', NULL, 'y']::text[]`).Scan(&pgTexts); err != nil {
		t.Fatalf("scan []pgx.Text: %v", err)
	}
	if len(pgTexts) != 3 || !pgTexts[0].Valid || pgTexts[0].String != "x" || pgTexts[1].Valid || !pgTexts[2].Valid || pgTexts[2].String != "y" {
		t.Fatalf("unexpected []pgx.Text: %+v", pgTexts)
	}

	var ptrInts []*int64
	if err := pool.QueryRow(ctx, `SELECT ARRAY[10, NULL, 30]::bigint[]`).Scan(&ptrInts); err != nil {
		t.Fatalf("scan []*int64: %v", err)
	}
	if len(ptrInts) != 3 || ptrInts[0] == nil || *ptrInts[0] != 10 || ptrInts[1] != nil || ptrInts[2] == nil || *ptrInts[2] != 30 {
		t.Fatalf("unexpected []*int64: %+v", ptrInts)
	}

	// Non-pointer slice must reject NULL element
	var nonNullInts []int64
	if err := pool.QueryRow(ctx, `SELECT ARRAY[1, NULL, 3]::bigint[]`).Scan(&nonNullInts); err == nil {
		t.Fatal("expected error scanning NULL array element into []int64")
	}

	// 2. Multi-dimensional arrays
	var mat [][]int64
	if err := pool.QueryRow(ctx, `SELECT ARRAY[[1, 2], [3, 4]]::bigint[]`).Scan(&mat); err != nil {
		t.Fatalf("scan [][]int64: %v", err)
	}
	if len(mat) != 2 || len(mat[0]) != 2 || mat[0][0] != 1 || mat[0][1] != 2 || mat[1][0] != 3 || mat[1][1] != 4 {
		t.Fatalf("unexpected [][]int64: %+v", mat)
	}

	var smat [][]string
	if err := pool.QueryRow(ctx, `SELECT ARRAY[['a', 'b'], ['c', 'd']]::text[]`).Scan(&smat); err != nil {
		t.Fatalf("scan [][]string: %v", err)
	}
	if len(smat) != 2 || smat[0][0] != "a" || smat[1][1] != "d" {
		t.Fatalf("unexpected [][]string: %+v", smat)
	}

	// 3. []float32 and []int8
	var f32 []float32
	if err := pool.QueryRow(ctx, `SELECT ARRAY[1.25, 2.5]::real[]`).Scan(&f32); err != nil {
		t.Fatalf("scan []float32: %v", err)
	}
	if len(f32) != 2 || f32[0] != 1.25 || f32[1] != 2.5 {
		t.Fatalf("unexpected []float32: %v", f32)
	}
	var i8 []int8
	if err := pool.QueryRow(ctx, `SELECT ARRAY[-5, 0, 127]::smallint[]`).Scan(&i8); err != nil {
		t.Fatalf("scan []int8: %v", err)
	}
	if len(i8) != 3 || i8[0] != -5 || i8[1] != 0 || i8[2] != 127 {
		t.Fatalf("unexpected []int8: %v", i8)
	}

	// 4. Safe unsigned & signed bounds checking
	var u64 uint64
	if err := pool.QueryRow(ctx, `SELECT -1::bigint`).Scan(&u64); err == nil {
		t.Fatalf("expected error scanning -1 into uint64, got %d", u64)
	}
	var u32 uint32
	if err := pool.QueryRow(ctx, `SELECT -42::bigint`).Scan(&u32); err == nil {
		t.Fatalf("expected error scanning -42 into uint32, got %d", u32)
	}
	var u8 uint8
	if err := pool.QueryRow(ctx, `SELECT 256::bigint`).Scan(&u8); err == nil {
		t.Fatalf("expected overflow error scanning 256 into uint8, got %d", u8)
	}
	var s8 int8
	if err := pool.QueryRow(ctx, `SELECT 200::bigint`).Scan(&s8); err == nil {
		t.Fatalf("expected overflow error scanning 200 into int8, got %d", s8)
	}
	if err := pool.QueryRow(ctx, `SELECT 42::bigint`).Scan(&u64); err != nil || u64 != 42 {
		t.Fatalf("scan positive uint64 = %d, %v", u64, err)
	}
	if err := pool.QueryRow(ctx, `SELECT 255::bigint`).Scan(&u8); err != nil || u8 != 255 {
		t.Fatalf("scan positive uint8 = %d, %v", u8, err)
	}
}

func TestPgxDeferrablePoolHooksDeallocateAndAtomicClose(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()

	var beforeConnectCount, afterConnectCount, beforeAcquireCount, afterReleaseCount int
	var mu sync.Mutex
	discardNextRelease := false

	pool, err := pgx.OpenPoolWithConfig(ctx, pgx.PoolConfig{
		OpenOptions: supersql.OpenOptions{
			Directory:   dir,
			Clock:       newTestClock(),
			Maintenance: supersql.MaintenanceConfig{Disabled: true},
		},
		MaxConns: 4,
		BeforeConnect: func(_ context.Context, _ *pgx.PoolConfig) error {
			mu.Lock()
			beforeConnectCount++
			mu.Unlock()
			return nil
		},
		AfterConnect: func(_ context.Context, _ *pgx.Conn) error {
			mu.Lock()
			afterConnectCount++
			mu.Unlock()
			return nil
		},
		BeforeAcquire: func(_ context.Context, _ *pgx.Conn) bool {
			mu.Lock()
			beforeAcquireCount++
			mu.Unlock()
			return true
		},
		AfterRelease: func(_ *pgx.Conn) bool {
			mu.Lock()
			afterReleaseCount++
			keep := !discardNextRelease
			discardNextRelease = false
			mu.Unlock()
			return keep
		},
	})
	if err != nil {
		t.Fatalf("OpenPoolWithConfig: %v", err)
	}
	defer pool.Close()

	// 1. DeferrableMode propagation
	txDef, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:       pgx.Serializable,
		AccessMode:     pgx.ReadOnly,
		DeferrableMode: pgx.Deferrable,
	})
	if err != nil {
		t.Fatalf("BeginTx Deferrable: %v", err)
	}
	var defSetting string
	if err := txDef.QueryRow(ctx, `SHOW transaction_deferrable`).Scan(&defSetting); err != nil {
		_ = txDef.Rollback(ctx)
		t.Fatal(err)
	}
	if defSetting != "on" {
		_ = txDef.Rollback(ctx)
		t.Fatalf("SHOW transaction_deferrable = %q, want on", defSetting)
	}
	if err := txDef.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	txNotDef, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:       pgx.Serializable,
		AccessMode:     pgx.ReadOnly,
		DeferrableMode: pgx.NotDeferrable,
	})
	if err != nil {
		t.Fatalf("BeginTx NotDeferrable: %v", err)
	}
	if err := txNotDef.QueryRow(ctx, `SHOW transaction_deferrable`).Scan(&defSetting); err != nil {
		_ = txNotDef.Rollback(ctx)
		t.Fatal(err)
	}
	if defSetting != "off" {
		_ = txNotDef.Rollback(ctx)
		t.Fatalf("SHOW transaction_deferrable = %q, want off", defSetting)
	}
	if err := txNotDef.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// 2. PoolConfig hooks: discard on AfterRelease forces fresh connect on next acquire
	mu.Lock()
	discardNextRelease = true
	mu.Unlock()
	if err := pool.AcquireFunc(ctx, func(pc *pgx.PoolConn) error {
		return pc.Ping(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.AcquireFunc(ctx, func(pc *pgx.PoolConn) error {
		return pc.Ping(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	bc, ac, ba, ar := beforeConnectCount, afterConnectCount, beforeAcquireCount, afterReleaseCount
	mu.Unlock()
	if bc < 2 || ac < 2 || ba < 4 || ar < 4 {
		t.Fatalf("hook counts too low: beforeConnect=%d afterConnect=%d beforeAcquire=%d afterRelease=%d", bc, ac, ba, ar)
	}

	// 3. Conn.Prepare, Deallocate, DeallocateAll
	if err := pool.AcquireFunc(ctx, func(pc *pgx.PoolConn) error {
		conn := pc.Conn()
		if err := conn.Prepare(ctx, "add_one", `SELECT $1::bigint + 1`); err != nil {
			return err
		}
		if err := conn.Prepare(ctx, "mul_two", `SELECT $1::bigint * 2`); err != nil {
			return err
		}
		var out int64
		if err := conn.QueryRow(ctx, "add_one", 41).Scan(&out); err != nil || out != 42 {
			t.Fatalf("prepared add_one = %d, %v", out, err)
		}
		if err := conn.Deallocate(ctx, "add_one"); err != nil {
			return err
		}
		if err := conn.QueryRow(ctx, "add_one", 41).Scan(&out); err == nil {
			t.Fatal("expected Deallocate to remove prepared statement add_one")
		}
		if err := conn.QueryRow(ctx, "mul_two", 21).Scan(&out); err != nil || out != 42 {
			t.Fatalf("prepared mul_two = %d, %v", out, err)
		}
		if err := conn.DeallocateAll(ctx); err != nil {
			return err
		}
		if err := conn.QueryRow(ctx, "mul_two", 21).Scan(&out); err == nil {
			t.Fatal("expected DeallocateAll to clear all prepared statements")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 4. Concurrent Rows.Close() under -race
	rows, err := pool.Query(ctx, `SELECT 1 UNION ALL SELECT 2`)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows.Close()
		}()
	}
	wg.Wait()
}

func TestPgxPgTypes(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE typed_rows (
		id bigint PRIMARY KEY,
		t text,
		i2 smallint,
		i4 integer,
		i8 bigint,
		f4 real,
		f8 double precision,
		b boolean,
		u uuid,
		num numeric,
		ts timestamp,
		tstz timestamptz,
		d date,
		jb jsonb
	)`); err != nil {
		t.Fatal(err)
	}

	var uIn pgx.UUID
	if err := uIn.Scan("550e8400-e29b-41d4-a716-446655440000"); err != nil {
		t.Fatal(err)
	}
	numIn, err := pgx.NumericFromString("12345678901234567890.123456")
	if err != nil {
		t.Fatal(err)
	}
	tsVal := time.Date(2026, 6, 15, 10, 20, 30, 0, time.UTC)
	dateVal := time.Date(2026, 6, 15, 0, 0, 0, 0, time.UTC)

	// Row 1: all valid
	if _, err := pool.Exec(ctx,
		`INSERT INTO typed_rows VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		int64(1),
		pgx.Text{String: "hello", Valid: true},
		pgx.Int2{Int16: 32000, Valid: true},
		pgx.Int4{Int32: 2000000, Valid: true},
		pgx.Int8{Int64: 9000000000000, Valid: true},
		pgx.Float4{Float32: 1.5, Valid: true},
		pgx.Float8{Float64: 3.14159265, Valid: true},
		pgx.Bool{Bool: true, Valid: true},
		uIn,
		numIn,
		pgx.Timestamp{Time: tsVal, Valid: true},
		pgx.Timestamptz{Time: tsVal, Valid: true},
		pgx.Date{Time: dateVal, Valid: true},
		pgx.JSONB{Bytes: []byte(`{"k":"v"}`), Valid: true},
	); err != nil {
		t.Fatalf("INSERT valid pgtypes: %v", err)
	}

	// Row 2: all NULL
	if _, err := pool.Exec(ctx,
		`INSERT INTO typed_rows VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		int64(2),
		pgx.Text{}, pgx.Int2{}, pgx.Int4{}, pgx.Int8{},
		pgx.Float4{}, pgx.Float8{}, pgx.Bool{}, pgx.UUID{},
		pgx.Numeric{}, pgx.Timestamp{}, pgx.Timestamptz{}, pgx.Date{}, pgx.JSONB{},
	); err != nil {
		t.Fatalf("INSERT NULL pgtypes: %v", err)
	}

	var (
		gotT    pgx.Text
		gotI2   pgx.Int2
		gotI4   pgx.Int4
		gotI8   pgx.Int8
		gotF4   pgx.Float4
		gotF8   pgx.Float8
		gotB    pgx.Bool
		gotU    pgx.UUID
		gotNum  pgx.Numeric
		gotTS   pgx.Timestamp
		gotTSTZ pgx.Timestamptz
		gotD    pgx.Date
		gotJB   pgx.JSONB
	)
	if err := pool.QueryRow(ctx, `SELECT t,i2,i4,i8,f4,f8,b,u,num,ts,tstz,d,jb FROM typed_rows WHERE id = 1`).
		Scan(&gotT, &gotI2, &gotI4, &gotI8, &gotF4, &gotF8, &gotB, &gotU, &gotNum, &gotTS, &gotTSTZ, &gotD, &gotJB); err != nil {
		t.Fatalf("Scan valid row: %v", err)
	}
	if !gotT.Valid || gotT.String != "hello" ||
		!gotI2.Valid || gotI2.Int16 != 32000 ||
		!gotI4.Valid || gotI4.Int32 != 2000000 ||
		!gotI8.Valid || gotI8.Int64 != 9000000000000 ||
		!gotF4.Valid || gotF4.Float32 != 1.5 ||
		!gotF8.Valid || math.Abs(gotF8.Float64-3.14159265) > 1e-9 ||
		!gotB.Valid || !gotB.Bool ||
		!gotU.Valid || gotU.String() != "550e8400-e29b-41d4-a716-446655440000" ||
		!gotNum.Valid || gotNum.String() != "12345678901234567890.123456" ||
		!gotTS.Valid || !gotTS.Time.Equal(tsVal) ||
		!gotTSTZ.Valid || !gotTSTZ.Time.Equal(tsVal) ||
		!gotD.Valid || gotD.Time.Format("2006-01-02") != "2026-06-15" ||
		!gotJB.Valid || len(gotJB.Bytes) == 0 {
		t.Fatalf("valid pgtype mismatch: t=%+v i2=%+v i4=%+v i8=%+v f4=%+v f8=%+v b=%+v u=%q num=%q ts=%+v tstz=%+v d=%+v jb=%q",
			gotT, gotI2, gotI4, gotI8, gotF4, gotF8, gotB, gotU.String(), gotNum.String(), gotTS, gotTSTZ, gotD, string(gotJB.Bytes))
	}

	if err := pool.QueryRow(ctx, `SELECT t,i2,i4,i8,f4,f8,b,u,num,ts,tstz,d,jb FROM typed_rows WHERE id = 2`).
		Scan(&gotT, &gotI2, &gotI4, &gotI8, &gotF4, &gotF8, &gotB, &gotU, &gotNum, &gotTS, &gotTSTZ, &gotD, &gotJB); err != nil {
		t.Fatalf("Scan NULL row: %v", err)
	}
	if gotT.Valid || gotI2.Valid || gotI4.Valid || gotI8.Valid || gotF4.Valid || gotF8.Valid ||
		gotB.Valid || gotU.Valid || gotNum.Valid || gotTS.Valid || gotTSTZ.Valid || gotD.Valid || gotJB.Valid {
		t.Fatalf("expected all NULL pgtypes to have Valid=false")
	}
}

