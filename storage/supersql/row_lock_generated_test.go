package supersql_test

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	jackpgx "github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	supersql "github.com/gosuda/supersql-alpha"
	clocklocal "github.com/gosuda/supersql-alpha/clock/local"
	"github.com/gosuda/supersql-alpha/pgwire"
	ssqlpgx "github.com/gosuda/supersql-alpha/pgx"
)

func requireSupersqlCode(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected SQLSTATE %s, got nil error", want)
	}
	var sqlErr *supersql.Error
	if errors.As(err, &sqlErr) && sqlErr.Code == want {
		return
	}
	var pgErr *ssqlpgx.PgError
	if errors.As(err, &pgErr) && pgErr.Code == want {
		return
	}
	var wireErr *pgconn.PgError
	if errors.As(err, &wireErr) && wireErr.Code == want {
		return
	}
	t.Fatalf("got error %v, want SQLSTATE %s", err, want)
}

func startTestPgwirePool(t *testing.T, ctx context.Context, db *supersql.Catalog) *pgxpool.Pool {
	t.Helper()
	srv, err := pgwire.NewServerFromCatalog(db, pgwire.Config{
		Password: "lock-test-password",
	})
	if err != nil {
		t.Fatalf("pgwire.NewServerFromCatalog: %v", err)
	}
	go func() { _ = srv.ListenAndServe(ctx, "127.0.0.1:0") }()
	t.Cleanup(func() { _ = srv.Close() })

	var addr string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if a := srv.Addr(); a != nil {
			addr = a.String()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("pgwire server failed to bind address")
	}

	poolCfg, err := pgxpool.ParseConfig("postgres://supersql:lock-test-password@" + addr + "/postgres?sslmode=disable")
	if err != nil {
		t.Fatalf("pgxpool.ParseConfig: %v", err)
	}
	poolCfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		t.Fatalf("pgxpool.NewWithConfig: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestSelectForUpdateNowaitAndSkipLocked verifies SELECT ... FOR UPDATE /
// FOR NO KEY UPDATE / FOR SHARE / FOR KEY SHARE with NOWAIT and SKIP LOCKED
// across database/sql, supersql/pgx, and pgwire, both single-session and across
// concurrent transactions, with and without ORDER BY, LIMIT, WHERE, and
// secondary indexes.
func TestSelectForUpdateNowaitAndSkipLocked(t *testing.T) {
	ctx := t.Context()
	db, err := supersql.Open(ctx, supersql.OpenOptions{
		Directory: t.TempDir(),
		Clock:     clocklocal.New(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sqlDB := sql.OpenDB(supersql.NewConnectorWithCatalog(db))
	defer func() { _ = sqlDB.Close() }()

	for _, stmt := range []string{
		`CREATE TABLE jobs (id INT PRIMARY KEY, status TEXT NOT NULL, priority INT NOT NULL DEFAULT 0)`,
		`CREATE INDEX idx_jobs_status ON jobs (status)`,
		`INSERT INTO jobs VALUES (1, 'queued', 20), (2, 'queued', 10), (3, 'queued', 30), (4, 'done', 5)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	// 1. Single-session: all four lock strengths + SKIP LOCKED with ORDER BY,
	//    LIMIT, WHERE, and secondary index present.
	t.Run("SingleSessionAllStrengthsAndShapes", func(t *testing.T) {
		tx, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("BeginTx: %v", err)
		}
		defer func() { _ = tx.Rollback() }()

		for _, tc := range []struct {
			name string
			sql  string
			want []int
		}{
			{
				name: "for_update_skip_locked_order_limit",
				sql:  `SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`,
				want: []int{1},
			},
			{
				name: "for_update_skip_locked_non_pk_order_limit",
				sql:  `SELECT id FROM jobs WHERE status = 'queued' ORDER BY priority ASC LIMIT 1 FOR UPDATE SKIP LOCKED`,
				want: []int{2},
			},
			{
				name: "for_update_no_where_no_limit",
				sql:  `SELECT id FROM jobs ORDER BY id FOR UPDATE`,
				want: []int{1, 2, 3, 4},
			},
			{
				name: "for_no_key_update_nowait",
				sql:  `SELECT id FROM jobs WHERE id = 2 FOR NO KEY UPDATE NOWAIT`,
				want: []int{2},
			},
			{
				name: "for_share_skip_locked",
				sql:  `SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 2 FOR SHARE SKIP LOCKED`,
				want: []int{1, 2},
			},
			{
				name: "for_key_share",
				sql:  `SELECT id FROM jobs WHERE id = 3 FOR KEY SHARE`,
				want: []int{3},
			},
		} {
			rows, err := tx.QueryContext(ctx, tc.sql)
			if err != nil {
				t.Fatalf("%s QueryContext: %v", tc.name, err)
			}
			var got []int
			for rows.Next() {
				var id int
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					t.Fatalf("%s Scan: %v", tc.name, err)
				}
				got = append(got, id)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				t.Fatalf("%s rows.Err: %v", tc.name, err)
			}
			if err := rows.Close(); err != nil {
				t.Fatalf("%s rows.Close: %v", tc.name, err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("%s got %v, want %v", tc.name, got, tc.want)
			}
		}

		// Prepared statement path with parameter + SKIP LOCKED.
		stmt, err := tx.PrepareContext(ctx, `SELECT id FROM jobs WHERE status = $1 ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`)
		if err != nil {
			t.Fatalf("PrepareContext: %v", err)
		}
		var claimed int
		if err := stmt.QueryRowContext(ctx, "queued").Scan(&claimed); err != nil {
			_ = stmt.Close()
			t.Fatalf("prepared QueryRowContext: %v", err)
		}
		if err := stmt.Close(); err != nil {
			t.Fatalf("stmt.Close: %v", err)
		}
		if claimed != 1 {
			t.Fatalf("prepared claimed = %d, want 1", claimed)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE jobs SET status = 'running' WHERE id = $1`, claimed); err != nil {
			t.Fatalf("update claimed: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		// Reset row 1 back to queued for subsequent subtests.
		if _, err := sqlDB.ExecContext(ctx, `UPDATE jobs SET status = 'queued' WHERE id = 1`); err != nil {
			t.Fatalf("reset job 1: %v", err)
		}
	})

	// 2. Concurrent FOR UPDATE NOWAIT (55P03 on contention, success on unlocked row)
	//    and FOR UPDATE SKIP LOCKED (skips locked row 1, claims row 2; returns 0
	//    rows cleanly when the only targeted row is locked).
	t.Run("ConcurrentNowaitAndSkipLocked", func(t *testing.T) {
		tx1, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("tx1 BeginTx: %v", err)
		}
		defer func() { _ = tx1.Rollback() }()

		var lockedID int
		if err := tx1.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE`).Scan(&lockedID); err != nil {
			t.Fatalf("tx1 lock row 1: %v", err)
		}
		if lockedID != 1 {
			t.Fatalf("tx1 lockedID = %d, want 1", lockedID)
		}

		// Concurrent NOWAIT on locked row 1 must fail immediately with 55P03.
		txNowait, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("txNowait BeginTx: %v", err)
		}
		var dummy int
		err = txNowait.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE NOWAIT`).Scan(&dummy)
		requireSupersqlCode(t, err, "55P03")
		_ = txNowait.Rollback()

		// Concurrent NOWAIT on unlocked row 3 must succeed immediately.
		txUnlocked, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("txUnlocked BeginTx: %v", err)
		}
		var unlockedID int
		if err := txUnlocked.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id = 3 FOR UPDATE NOWAIT`).Scan(&unlockedID); err != nil {
			_ = txUnlocked.Rollback()
			t.Fatalf("txUnlocked row 3 NOWAIT: %v", err)
		}
		if unlockedID != 3 {
			_ = txUnlocked.Rollback()
			t.Fatalf("txUnlocked id = %d, want 3", unlockedID)
		}
		if err := txUnlocked.Rollback(); err != nil {
			t.Fatalf("txUnlocked Rollback: %v", err)
		}

		// Concurrent SKIP LOCKED on point row 1 (all matching rows locked) must
		// succeed with 0 rows rather than erroring.
		txEmpty, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("txEmpty BeginTx: %v", err)
		}
		err = txEmpty.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id = 1 FOR UPDATE SKIP LOCKED`).Scan(&dummy)
		if !errors.Is(err, sql.ErrNoRows) {
			_ = txEmpty.Rollback()
			t.Fatalf("SKIP LOCKED on locked row 1: got err=%v, want sql.ErrNoRows", err)
		}
		_ = txEmpty.Rollback()

		// Concurrent SKIP LOCKED on queue must skip locked row 1 and claim row 2.
		tx2, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("tx2 BeginTx: %v", err)
		}
		defer func() { _ = tx2.Rollback() }()

		var skippedToID int
		if err := tx2.QueryRowContext(ctx,
			`SELECT id FROM jobs WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`,
		).Scan(&skippedToID); err != nil {
			t.Fatalf("tx2 SKIP LOCKED: %v", err)
		}
		if skippedToID != 2 {
			t.Fatalf("tx2 claimed id = %d, want 2 (skipping locked row 1)", skippedToID)
		}

		// Both transactions update their respective claimed rows and commit cleanly.
		if _, err := tx1.ExecContext(ctx, `UPDATE jobs SET status = 'running' WHERE id = 1`); err != nil {
			t.Fatalf("tx1 update row 1: %v", err)
		}
		if err := tx1.Commit(); err != nil {
			t.Fatalf("tx1 Commit: %v", err)
		}
		if _, err := tx2.ExecContext(ctx, `UPDATE jobs SET status = 'running' WHERE id = 2`); err != nil {
			t.Fatalf("tx2 update row 2: %v", err)
		}
		if err := tx2.Commit(); err != nil {
			t.Fatalf("tx2 Commit: %v", err)
		}

		var runningCount int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE status = 'running'`).Scan(&runningCount); err != nil {
			t.Fatalf("count running: %v", err)
		}
		if runningCount != 2 {
			t.Fatalf("runningCount = %d, want 2", runningCount)
		}
	})

	// 3. Concurrent FOR SHARE allows another FOR SHARE while blocking FOR UPDATE NOWAIT (55P03).
	t.Run("ConcurrentForShareCompatibility", func(t *testing.T) {
		tx1, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("tx1 BeginTx: %v", err)
		}
		defer func() { _ = tx1.Rollback() }()

		var id1 int
		if err := tx1.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id = 3 FOR SHARE`).Scan(&id1); err != nil {
			t.Fatalf("tx1 FOR SHARE: %v", err)
		}
		if id1 != 3 {
			t.Fatalf("tx1 id = %d, want 3", id1)
		}

		// Concurrent FOR SHARE must succeed immediately.
		tx2, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("tx2 BeginTx: %v", err)
		}
		defer func() { _ = tx2.Rollback() }()

		var id2 int
		if err := tx2.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id = 3 FOR SHARE NOWAIT`).Scan(&id2); err != nil {
			t.Fatalf("tx2 FOR SHARE NOWAIT: %v", err)
		}
		if id2 != 3 {
			t.Fatalf("tx2 id = %d, want 3", id2)
		}

		// Concurrent FOR UPDATE NOWAIT must fail with 55P03 while share locks are held.
		tx3, err := sqlDB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("tx3 BeginTx: %v", err)
		}
		var dummy int
		err = tx3.QueryRowContext(ctx, `SELECT id FROM jobs WHERE id = 3 FOR UPDATE NOWAIT`).Scan(&dummy)
		requireSupersqlCode(t, err, "55P03")
		_ = tx3.Rollback()

		if err := tx2.Commit(); err != nil {
			t.Fatalf("tx2 Commit: %v", err)
		}
		if err := tx1.Commit(); err != nil {
			t.Fatalf("tx1 Commit: %v", err)
		}
	})

	// 4. End-to-end verification through supersql/pgx and pgwire (jackc/pgx/v5).
	t.Run("PgxAndPgwire", func(t *testing.T) {
		// supersql/pgx native adapter.
		spool := ssqlpgx.NewPoolFromCatalog(db)
		defer spool.Close()

		pgxTx1, err := spool.Begin(ctx)
		if err != nil {
			t.Fatalf("ssqlpgx Begin: %v", err)
		}
		defer func() { _ = pgxTx1.Rollback(ctx) }()

		var locked int
		if err := pgxTx1.QueryRow(ctx, `SELECT id FROM jobs WHERE id = 3 FOR UPDATE`).Scan(&locked); err != nil {
			t.Fatalf("ssqlpgx FOR UPDATE: %v", err)
		}
		if locked != 3 {
			t.Fatalf("ssqlpgx locked = %d, want 3", locked)
		}

		pgxTx2, err := spool.Begin(ctx)
		if err != nil {
			t.Fatalf("ssqlpgx Begin tx2: %v", err)
		}
		var dummy int
		err = pgxTx2.QueryRow(ctx, `SELECT id FROM jobs WHERE id = 3 FOR UPDATE NOWAIT`).Scan(&dummy)
		requireSupersqlCode(t, err, "55P03")
		_ = pgxTx2.Rollback(ctx)

		// pgwire via upstream jackc/pgx/v5 while ssqlpgx holds the lock on row 3.
		wirePool := startTestPgwirePool(t, ctx, db)
		wireTx, err := wirePool.BeginTx(ctx, jackpgx.TxOptions{})
		if err != nil {
			t.Fatalf("pgwire BeginTx: %v", err)
		}
		defer func() { _ = wireTx.Rollback(ctx) }()

		var claimed int
		if err := wireTx.QueryRow(ctx,
			`SELECT id FROM jobs WHERE status = 'done' OR id = 3 ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED`,
		).Scan(&claimed); err != nil {
			t.Fatalf("pgwire SKIP LOCKED: %v", err)
		}
		if claimed != 4 {
			t.Fatalf("pgwire SKIP LOCKED claimed = %d, want 4 (skipping locked row 3)", claimed)
		}
		if err := wireTx.Commit(ctx); err != nil {
			t.Fatalf("pgwire Commit: %v", err)
		}
		if err := pgxTx1.Commit(ctx); err != nil {
			t.Fatalf("ssqlpgx Commit: %v", err)
		}
	})
}

// TestGeneratedStoredColumnsEndToEnd verifies GENERATED ALWAYS AS (...) STORED
// columns with IDENTITY, DEFAULT, CHECK, RETURNING, UPDATE recomputation,
// ON CONFLICT DO UPDATE, MERGE, BEFORE triggers, and 428C9 rejection of
// explicit non-DEFAULT values.
func TestGeneratedStoredColumnsEndToEnd(t *testing.T) {
	ctx := t.Context()
	db, err := supersql.Open(ctx, supersql.OpenOptions{
		Directory: t.TempDir(),
		Clock:     clocklocal.New(),
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	sqlDB := sql.OpenDB(supersql.NewConnectorWithCatalog(db))
	defer func() { _ = sqlDB.Close() }()

	for _, stmt := range []string{
		`CREATE SCHEMA app`,
		`CREATE TABLE app.orders (
			id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
			customer_email TEXT NOT NULL UNIQUE,
			unit_price NUMERIC(10, 2) NOT NULL CHECK (unit_price >= 0),
			qty INT NOT NULL CHECK (qty > 0),
			total NUMERIC(12, 2) GENERATED ALWAYS AS (unit_price * qty) STORED,
			status TEXT NOT NULL DEFAULT 'open'
		)`,
	} {
		if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("setup %q: %v", stmt, err)
		}
	}

	// 1. INSERT omitting generated column + IDENTITY + DEFAULT + RETURNING total.
	var id1 int64
	var total1 string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO app.orders (customer_email, unit_price, qty)
		 VALUES ('alice@example.com', 19.50, 3)
		 RETURNING id, total`,
	).Scan(&id1, &total1); err != nil {
		t.Fatalf("INSERT omitting generated column: %v", err)
	}
	if id1 != 1 || total1 != "58.50" {
		t.Fatalf("INSERT RETURNING = (%d, %q), want (1, \"58.50\")", id1, total1)
	}

	// Verify persisted value via SELECT.
	var email, gotTotal, status string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT customer_email, total, status FROM app.orders WHERE id = 1`,
	).Scan(&email, &gotTotal, &status); err != nil {
		t.Fatalf("SELECT persisted order: %v", err)
	}
	if email != "alice@example.com" || gotTotal != "58.50" || status != "open" {
		t.Fatalf("persisted row = (%q, %q, %q), want (alice@example.com, 58.50, open)", email, gotTotal, status)
	}

	// 2. INSERT with explicit DEFAULT for generated column (PG 18 semantics).
	var id2 int64
	var total2 string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO app.orders (customer_email, unit_price, qty, total)
		 VALUES ('bob@example.com', 12.25, 4, DEFAULT)
		 RETURNING id, total`,
	).Scan(&id2, &total2); err != nil {
		t.Fatalf("INSERT explicit DEFAULT: %v", err)
	}
	if id2 != 2 || total2 != "49.00" {
		t.Fatalf("INSERT DEFAULT RETURNING = (%d, %q), want (2, \"49.00\")", id2, total2)
	}

	// 3. UPDATE recomputes stored generated column and supports SET total = DEFAULT.
	var updatedTotal string
	if err := sqlDB.QueryRowContext(ctx,
		`UPDATE app.orders SET qty = 5 WHERE customer_email = 'alice@example.com' RETURNING total`,
	).Scan(&updatedTotal); err != nil {
		t.Fatalf("UPDATE recompute: %v", err)
	}
	if updatedTotal != "97.50" {
		t.Fatalf("UPDATE recomputed total = %q, want \"97.50\"", updatedTotal)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`UPDATE app.orders SET unit_price = 20.00, total = DEFAULT WHERE customer_email = 'alice@example.com' RETURNING total`,
	).Scan(&updatedTotal); err != nil {
		t.Fatalf("UPDATE with total = DEFAULT: %v", err)
	}
	if updatedTotal != "100.00" {
		t.Fatalf("UPDATE total = DEFAULT recomputed = %q, want \"100.00\"", updatedTotal)
	}

	// 4. INSERT ... ON CONFLICT DO UPDATE recomputes generated column and exposes EXCLUDED.total.
	var upsertTotal string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO app.orders (customer_email, unit_price, qty)
		 VALUES ('alice@example.com', 15.00, 4)
		 ON CONFLICT (customer_email) DO UPDATE
		     SET unit_price = EXCLUDED.unit_price,
		         qty = EXCLUDED.qty,
		         status = CASE WHEN EXCLUDED.total = 60.00 THEN 'recalculated' ELSE 'wrong' END
		 RETURNING total`,
	).Scan(&upsertTotal); err != nil {
		t.Fatalf("ON CONFLICT DO UPDATE: %v", err)
	}
	if upsertTotal != "60.00" {
		t.Fatalf("ON CONFLICT DO UPDATE total = %q, want \"60.00\"", upsertTotal)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status FROM app.orders WHERE customer_email = 'alice@example.com'`,
	).Scan(&status); err != nil {
		t.Fatalf("SELECT status after upsert: %v", err)
	}
	if status != "recalculated" {
		t.Fatalf("EXCLUDED.total status = %q, want \"recalculated\"", status)
	}

	// 5. MERGE (WHEN MATCHED THEN UPDATE + WHEN NOT MATCHED THEN INSERT).
	if _, err := sqlDB.ExecContext(ctx,
		`MERGE INTO app.orders AS dst
		 USING (VALUES
		     ('bob@example.com', 10.00::numeric(10,2), 6),
		     ('carol@example.com', 7.50::numeric(10,2), 2)
		 ) AS src(email, price, qty)
		 ON dst.customer_email = src.email
		 WHEN MATCHED THEN
		     UPDATE SET unit_price = src.price, qty = src.qty
		 WHEN NOT MATCHED THEN
		     INSERT (customer_email, unit_price, qty, total)
		     VALUES (src.email, src.price, src.qty, DEFAULT)`,
	); err != nil {
		t.Fatalf("MERGE: %v", err)
	}
	var bobTotal, carolTotal string
	if err := sqlDB.QueryRowContext(ctx, `SELECT total FROM app.orders WHERE customer_email = 'bob@example.com'`).Scan(&bobTotal); err != nil {
		t.Fatalf("SELECT bob total: %v", err)
	}
	if bobTotal != "60.00" {
		t.Fatalf("MERGE updated bob total = %q, want \"60.00\"", bobTotal)
	}
	if err := sqlDB.QueryRowContext(ctx, `SELECT total FROM app.orders WHERE customer_email = 'carol@example.com'`).Scan(&carolTotal); err != nil {
		t.Fatalf("SELECT carol total: %v", err)
	}
	if carolTotal != "15.00" {
		t.Fatalf("MERGE inserted carol total = %q, want \"15.00\"", carolTotal)
	}

	// 6. BEFORE INSERT / UPDATE trigger modifying base columns recomputes generated column.
	for _, stmt := range []string{
		`CREATE FUNCTION app.double_qty() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			NEW.qty := NEW.qty * 2;
			RETURN NEW;
		END;
		$$`,
		`CREATE TRIGGER trg_double_qty BEFORE INSERT ON app.orders
		 FOR EACH ROW EXECUTE FUNCTION app.double_qty()`,
	} {
		if _, err := sqlDB.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("trigger setup %q: %v", stmt, err)
		}
	}
	var trigQty int
	var trigTotal string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO app.orders (customer_email, unit_price, qty)
		 VALUES ('dave@example.com', 5.00, 3)
		 RETURNING qty, total`,
	).Scan(&trigQty, &trigTotal); err != nil {
		t.Fatalf("INSERT with BEFORE trigger: %v", err)
	}
	if trigQty != 6 || trigTotal != "30.00" {
		t.Fatalf("BEFORE trigger RETURNING = (%d, %q), want (6, \"30.00\")", trigQty, trigTotal)
	}
	if _, err := sqlDB.ExecContext(ctx, `DROP TRIGGER trg_double_qty ON app.orders`); err != nil {
		t.Fatalf("DROP TRIGGER: %v", err)
	}

	// 7. Explicitly supplying non-DEFAULT values to generated column MUST fail with 428C9.
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			name: "insert_values_non_default",
			sql:  `INSERT INTO app.orders (customer_email, unit_price, qty, total) VALUES ('eve@example.com', 10.00, 2, 100.00)`,
		},
		{
			name: "insert_select_non_default",
			sql:  `INSERT INTO app.orders (customer_email, unit_price, qty, total) SELECT 'eve@example.com', 10.00::numeric(10,2), 2, 100.00::numeric(12,2)`,
		},
		{
			name: "update_non_default",
			sql:  `UPDATE app.orders SET total = 100.00 WHERE customer_email = 'alice@example.com'`,
		},
		{
			name: "on_conflict_update_non_default",
			sql:  `INSERT INTO app.orders (customer_email, unit_price, qty) VALUES ('alice@example.com', 10.00, 2) ON CONFLICT (customer_email) DO UPDATE SET total = 100.00`,
		},
		{
			name: "merge_insert_non_default",
			sql:  `MERGE INTO app.orders AS dst USING (VALUES ('eve@example.com', 10.00::numeric(10,2), 2)) AS src(email, price, qty) ON dst.customer_email = src.email WHEN NOT MATCHED THEN INSERT (customer_email, unit_price, qty, total) VALUES (src.email, src.price, src.qty, 100.00)`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := sqlDB.ExecContext(ctx, tc.sql)
			requireSupersqlCode(t, err, "428C9")
		})
	}
}
