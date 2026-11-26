package pgx_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/pgx"
)

func assertPhase2PgErr(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected *pgx.PgError with SQLSTATE %s, got nil", wantCode)
	}
	var pgErr *pgx.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected *pgx.PgError with SQLSTATE %s, got %T: %v", wantCode, err, err)
	}
	if pgErr.Code != wantCode {
		t.Fatalf("expected SQLSTATE %s, got %s (%s)", wantCode, pgErr.Code, pgErr.Message)
	}
}

func TestPhase2QueryRowScanCloseBeforeErrMultiRowInsert(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE p2_items (id bigint PRIMARY KEY, label text NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	// Case 1: 2-row INSERT ... RETURNING where the 2nd row triggers a duplicate key violation (23505).
	var gotID int64
	err := pool.QueryRow(ctx, `INSERT INTO p2_items (id, label) VALUES (1, 'first'), (1, 'dup') RETURNING id`).Scan(&gotID)
	assertPhase2PgErr(t, err, "23505")

	// The 1st row must have been rolled back.
	var cnt int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p2_items`).Scan(&cnt); err != nil {
		t.Fatalf("SELECT count after 2-row failure: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 rows after rolled-back 2-row INSERT ... RETURNING, got %d", cnt)
	}

	// Case 2: Multi-batch INSERT ... SELECT ... RETURNING where batch 1 is already streamed
	// and scanned by QueryRow before a later row triggers 23505 during rows.Close().
	err = pool.QueryRow(ctx, `
		INSERT INTO p2_items (id, label)
		SELECT CASE WHEN g = 2000 THEN 1 ELSE g END, 'row-' || g::text
		FROM generate_series(1, 2000) AS g
		RETURNING id
	`).Scan(&gotID)
	assertPhase2PgErr(t, err, "23505")

	if err := pool.QueryRow(ctx, `SELECT count(*) FROM p2_items`).Scan(&cnt); err != nil {
		t.Fatalf("SELECT count after multi-batch failure: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("expected 0 rows after rolled-back multi-batch INSERT ... RETURNING, got %d", cnt)
	}
}

func TestPhase2QueryRowScanDeferredConstraintOnAutocommit(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	t.Run("update_deferred_unique", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p2_uniq (
			id bigint PRIMARY KEY,
			code text NOT NULL,
			CONSTRAINT p2_uniq_code UNIQUE (code) DEFERRABLE INITIALLY DEFERRED
		)`); err != nil {
			t.Fatalf("CREATE p2_uniq: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO p2_uniq VALUES (1, 'a'), (2, 'b')`); err != nil {
			t.Fatalf("INSERT p2_uniq: %v", err)
		}

		// UPDATE ... RETURNING yields row 2, then autocommit COMMIT fails on the deferred UNIQUE (23505).
		var updatedID int64
		err := pool.QueryRow(ctx, `UPDATE p2_uniq SET code = 'a' WHERE id = 2 RETURNING id`).Scan(&updatedID)
		assertPhase2PgErr(t, err, "23505")

		var code2 string
		if err := pool.QueryRow(ctx, `SELECT code FROM p2_uniq WHERE id = 2`).Scan(&code2); err != nil {
			t.Fatalf("SELECT code2: %v", err)
		}
		if code2 != "b" {
			t.Fatalf("expected row 2 code to remain 'b' after deferred unique failure, got %q", code2)
		}
	})

	t.Run("update_deferred_fk", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p2_parents (id bigint PRIMARY KEY)`); err != nil {
			t.Fatalf("CREATE p2_parents: %v", err)
		}
		if _, err := pool.Exec(ctx, `CREATE TABLE p2_children (
			id bigint PRIMARY KEY,
			parent_id bigint NOT NULL REFERENCES p2_parents(id) DEFERRABLE INITIALLY DEFERRED
		)`); err != nil {
			t.Fatalf("CREATE p2_children: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO p2_parents VALUES (1)`); err != nil {
			t.Fatalf("INSERT parent: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO p2_children VALUES (10, 1)`); err != nil {
			t.Fatalf("INSERT child: %v", err)
		}

		// UPDATE ... RETURNING yields row 10, then autocommit COMMIT fails on the deferred FK (23503).
		var updatedChild int64
		err := pool.QueryRow(ctx, `UPDATE p2_children SET parent_id = 999 WHERE id = 10 RETURNING id`).Scan(&updatedChild)
		assertPhase2PgErr(t, err, "23503")

		var parentID int64
		if err := pool.QueryRow(ctx, `SELECT parent_id FROM p2_children WHERE id = 10`).Scan(&parentID); err != nil {
			t.Fatalf("SELECT parent_id: %v", err)
		}
		if parentID != 1 {
			t.Fatalf("expected child parent_id to remain 1 after deferred FK failure, got %d", parentID)
		}
	})

	t.Run("delete_self_ref_deferred_fk", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p2_nodes (
			id bigint PRIMARY KEY,
			parent_id bigint REFERENCES p2_nodes(id) DEFERRABLE INITIALLY DEFERRED
		)`); err != nil {
			t.Fatalf("CREATE p2_nodes: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO p2_nodes VALUES (1, NULL), (2, 1)`); err != nil {
			t.Fatalf("INSERT p2_nodes: %v", err)
		}

		// DELETE ... RETURNING yields row 1, then autocommit COMMIT fails on the deferred self-FK (23503).
		var deletedID int64
		err := pool.QueryRow(ctx, `DELETE FROM p2_nodes WHERE id = 1 RETURNING id`).Scan(&deletedID)
		assertPhase2PgErr(t, err, "23503")

		var nodeCount int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p2_nodes WHERE id = 1`).Scan(&nodeCount); err != nil {
			t.Fatalf("SELECT nodeCount: %v", err)
		}
		if nodeCount != 1 {
			t.Fatalf("expected node 1 to remain after deferred FK failure, got count=%d", nodeCount)
		}
	})
}

func TestPhase2SendBatchInsideManualBegin(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE p2_batch (id bigint PRIMARY KEY, val text NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	c1, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire c1: %v", err)
	}
	defer c1.Release()

	c2, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire c2: %v", err)
	}
	defer c2.Release()

	// Part 1: SendBatch inside manual BEGIN must NOT commit the outer tx; outer ROLLBACK discards it.
	if _, err := c1.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("manual BEGIN: %v", err)
	}
	if _, err := c1.Exec(ctx, `INSERT INTO p2_batch VALUES (1, 'before')`); err != nil {
		t.Fatalf("INSERT before batch: %v", err)
	}

	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO p2_batch VALUES ($1, $2)`, 2, "batch-a")
	batch.Queue(`INSERT INTO p2_batch VALUES ($1, $2)`, 3, "batch-b")
	batch.Queue(`SELECT count(*) FROM p2_batch`)

	br := c1.SendBatch(ctx, batch)
	if _, err := br.Exec(); err != nil {
		t.Fatalf("br.Exec #1: %v", err)
	}
	if _, err := br.Exec(); err != nil {
		t.Fatalf("br.Exec #2: %v", err)
	}
	var inTxCount int64
	if err := br.QueryRow().Scan(&inTxCount); err != nil {
		t.Fatalf("br.QueryRow #3: %v", err)
	}
	if inTxCount != 3 {
		t.Fatalf("expected 3 rows visible inside manual tx, got %d", inTxCount)
	}
	if err := br.Close(); err != nil {
		t.Fatalf("br.Close: %v", err)
	}

	// Another connection must NOT see uncommitted rows after br.Close().
	var otherCount int64
	if err := c2.QueryRow(ctx, `SELECT count(*) FROM p2_batch`).Scan(&otherCount); err != nil {
		t.Fatalf("c2 SELECT count before rollback: %v", err)
	}
	if otherCount != 0 {
		t.Fatalf("br.Close() prematurely committed outer manual BEGIN: c2 saw %d rows", otherCount)
	}

	// Outer ROLLBACK must discard all 3 rows.
	if _, err := c1.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("manual ROLLBACK: %v", err)
	}
	if err := c1.QueryRow(ctx, `SELECT count(*) FROM p2_batch`).Scan(&inTxCount); err != nil {
		t.Fatalf("c1 SELECT count after rollback: %v", err)
	}
	if inTxCount != 0 {
		t.Fatalf("expected 0 rows after outer ROLLBACK, got %d", inTxCount)
	}

	// Part 2: SendBatch inside manual BEGIN becomes visible ONLY after outer COMMIT.
	if _, err := c1.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("manual BEGIN #2: %v", err)
	}
	batch2 := &pgx.Batch{}
	batch2.Queue(`INSERT INTO p2_batch VALUES (10, 'committed-a')`)
	batch2.Queue(`INSERT INTO p2_batch VALUES (11, 'committed-b')`)
	if err := c1.SendBatch(ctx, batch2).Close(); err != nil {
		t.Fatalf("batch2 Close: %v", err)
	}
	if err := c2.QueryRow(ctx, `SELECT count(*) FROM p2_batch`).Scan(&otherCount); err != nil {
		t.Fatalf("c2 SELECT before outer COMMIT: %v", err)
	}
	if otherCount != 0 {
		t.Fatalf("expected 0 rows on c2 before outer COMMIT, got %d", otherCount)
	}
	if _, err := c1.Exec(ctx, `COMMIT`); err != nil {
		t.Fatalf("manual COMMIT: %v", err)
	}
	if err := c2.QueryRow(ctx, `SELECT count(*) FROM p2_batch`).Scan(&otherCount); err != nil {
		t.Fatalf("c2 SELECT after outer COMMIT: %v", err)
	}
	if otherCount != 2 {
		t.Fatalf("expected 2 rows on c2 after outer COMMIT, got %d", otherCount)
	}
}

func TestPhase2CopyFromInsideManualBegin(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `CREATE TABLE p2_copy (id bigint PRIMARY KEY, val text NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}

	c1, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire c1: %v", err)
	}
	defer c1.Release()

	c2, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire c2: %v", err)
	}
	defer c2.Release()

	// Part 1: Successful CopyFrom inside manual BEGIN does NOT commit outer tx; outer ROLLBACK discards it.
	if _, err := c1.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("manual BEGIN #1: %v", err)
	}
	if _, err := c1.Exec(ctx, `INSERT INTO p2_copy VALUES (1, 'pre-copy')`); err != nil {
		t.Fatalf("INSERT pre-copy: %v", err)
	}
	copied, err := c1.CopyFrom(
		ctx,
		pgx.Identifier{"p2_copy"},
		[]string{"id", "val"},
		pgx.CopyFromRows([][]any{
			{2, "copy-a"},
			{3, "copy-b"},
			{4, "copy-c"},
		}),
	)
	if err != nil || copied != 3 {
		t.Fatalf("CopyFrom inside BEGIN = %d, %v; want 3, nil", copied, err)
	}

	var otherCount int64
	if err := c2.QueryRow(ctx, `SELECT count(*) FROM p2_copy`).Scan(&otherCount); err != nil {
		t.Fatalf("c2 SELECT before rollback: %v", err)
	}
	if otherCount != 0 {
		t.Fatalf("CopyFrom prematurely committed outer manual BEGIN: c2 saw %d rows", otherCount)
	}

	if _, err := c1.Exec(ctx, `ROLLBACK`); err != nil {
		t.Fatalf("manual ROLLBACK: %v", err)
	}
	var c1Count int64
	if err := c1.QueryRow(ctx, `SELECT count(*) FROM p2_copy`).Scan(&c1Count); err != nil {
		t.Fatalf("c1 SELECT after rollback: %v", err)
	}
	if c1Count != 0 {
		t.Fatalf("expected 0 rows after outer ROLLBACK, got %d", c1Count)
	}

	// Part 2: Failed multi-batch CopyFrom inside manual BEGIN rolls back ONLY its savepoint,
	// leaving the outer transaction healthy so earlier and subsequent rows can COMMIT.
	if _, err := c1.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("manual BEGIN #2: %v", err)
	}
	if _, err := c1.Exec(ctx, `INSERT INTO p2_copy VALUES (10, 'earlier')`); err != nil {
		t.Fatalf("INSERT earlier: %v", err)
	}

	// 205 rows forces >1 internal batch (maxRowsPerBatch=200), with the final row duplicating id=10.
	rows := make([][]any, 205)
	for i := 0; i < 204; i++ {
		rows[i] = []any{int64(1000 + i), "bulk"}
	}
	rows[204] = []any{int64(10), "dup-earlier"}

	copied, err = c1.CopyFrom(ctx, pgx.Identifier{"p2_copy"}, []string{"id", "val"}, pgx.CopyFromRows(rows))
	assertPhase2PgErr(t, err, "23505")
	if copied != 0 {
		t.Fatalf("expected copied=0 on failed CopyFrom, got %d", copied)
	}

	// Outer tx must still be healthy (not 25P02) and still uncommitted.
	if _, err := c1.Exec(ctx, `INSERT INTO p2_copy VALUES (11, 'after-failed-copy')`); err != nil {
		t.Fatalf("outer tx broken after failed CopyFrom: %v", err)
	}
	if err := c2.QueryRow(ctx, `SELECT count(*) FROM p2_copy`).Scan(&otherCount); err != nil {
		t.Fatalf("c2 SELECT before outer COMMIT: %v", err)
	}
	if otherCount != 0 {
		t.Fatalf("expected 0 rows on c2 before outer COMMIT, got %d", otherCount)
	}

	if _, err := c1.Exec(ctx, `COMMIT`); err != nil {
		t.Fatalf("outer COMMIT after failed CopyFrom: %v", err)
	}
	if err := c2.QueryRow(ctx, `SELECT count(*) FROM p2_copy`).Scan(&otherCount); err != nil {
		t.Fatalf("c2 SELECT after outer COMMIT: %v", err)
	}
	if otherCount != 2 {
		t.Fatalf("expected exactly 2 committed rows (id 10 and 11), got %d", otherCount)
	}
}

func TestPhase2BeginTxRejectsActiveTxOnAllPaths(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	c1, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer c1.Release()

	if _, err := c1.Exec(ctx, `BEGIN`); err != nil {
		t.Fatalf("manual BEGIN: %v", err)
	}
	defer func() { _, _ = c1.Exec(ctx, `ROLLBACK`) }()

	// Standard Begin
	if _, err := c1.Begin(ctx); err == nil {
		t.Fatalf("expected Begin inside manual BEGIN to fail with 25001, got nil")
	} else {
		assertPhase2PgErr(t, err, "25001")
	}

	// Raw BeginQuery path
	if _, err := c1.BeginTx(ctx, pgx.TxOptions{BeginQuery: "BEGIN ISOLATION LEVEL SERIALIZABLE"}); err == nil {
		t.Fatalf("expected BeginTx(BeginQuery) inside manual BEGIN to fail with 25001, got nil")
	} else {
		assertPhase2PgErr(t, err, "25001")
	}

	// Raw DeferrableMode path
	if _, err := c1.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:       pgx.Serializable,
		AccessMode:     pgx.ReadOnly,
		DeferrableMode: pgx.Deferrable,
	}); err == nil {
		t.Fatalf("expected BeginTx(DeferrableMode) inside manual BEGIN to fail with 25001, got nil")
	} else {
		assertPhase2PgErr(t, err, "25001")
	}

	// Outer manual transaction must still be healthy and usable.
	var one int64
	if err := c1.QueryRow(ctx, `SELECT 1`).Scan(&one); err != nil || one != 1 {
		t.Fatalf("outer manual tx damaged after rejected BeginTx calls: one=%d err=%v", one, err)
	}
}

func TestPhase2TxCommitFailureLeavesConnectionUsable(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	if _, err := pool.Exec(ctx, `
		CREATE TABLE p2_commit_uniq (
			id bigint PRIMARY KEY,
			code text NOT NULL,
			CONSTRAINT p2_commit_uniq_code UNIQUE (code) DEFERRABLE INITIALLY DEFERRED
		);
		INSERT INTO p2_commit_uniq VALUES (1, 'x');
	`); err != nil {
		t.Fatalf("setup: %v", err)
	}

	c1, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer c1.Release()

	cases := []struct {
		name string
		opts pgx.TxOptions
	}{
		{name: "standard_sqlTx", opts: pgx.TxOptions{}},
		{name: "rawBegin_BeginQuery", opts: pgx.TxOptions{BeginQuery: "BEGIN"}},
		{name: "rawBegin_DeferrableMode", opts: pgx.TxOptions{IsoLevel: pgx.Serializable, DeferrableMode: pgx.NotDeferrable}},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := c1.BeginTx(ctx, tc.opts)
			if err != nil {
				t.Fatalf("BeginTx: %v", err)
			}
			// Violates deferred UNIQUE constraint on COMMIT.
			if _, err := tx.Exec(ctx, `INSERT INTO p2_commit_uniq VALUES ($1, 'x')`, int64(100+i)); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("INSERT deferred dup: %v", err)
			}
			err = tx.Commit(ctx)
			assertPhase2PgErr(t, err, "23505")

			// Connection must be usable immediately (not stuck in 25P02 or an active tx).
			var cnt int64
			if err := c1.QueryRow(ctx, `SELECT count(*) FROM p2_commit_uniq`).Scan(&cnt); err != nil {
				t.Fatalf("connection unusable after failed Commit: %v", err)
			}
			// Must also be able to start a new transaction immediately (no lingering active tx).
			nextTx, err := c1.Begin(ctx)
			if err != nil {
				t.Fatalf("Begin after failed Commit: %v", err)
			}
			if _, err := nextTx.Exec(ctx, `INSERT INTO p2_commit_uniq VALUES ($1, $2)`, int64(200+i), tc.name); err != nil {
				_ = nextTx.Rollback(ctx)
				t.Fatalf("INSERT in nextTx: %v", err)
			}
			if err := nextTx.Commit(ctx); err != nil {
				t.Fatalf("Commit nextTx: %v", err)
			}
		})
	}

	// Also verify rawBegin Commit with a pre-canceled context rolls back cleanly and leaves conn usable.
	tx, err := c1.BeginTx(ctx, pgx.TxOptions{BeginQuery: "BEGIN"})
	if err != nil {
		t.Fatalf("BeginTx(BeginQuery) for canceled commit: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO p2_commit_uniq VALUES (999, 'canceled')`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("INSERT before canceled commit: %v", err)
	}
	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	if err := tx.Commit(canceledCtx); err == nil {
		t.Fatalf("expected error from Commit(canceledCtx), got nil")
	}

	// Connection must NOT be stuck in an active transaction and row 999 must be rolled back.
	var row999 int64
	if err := c1.QueryRow(ctx, `SELECT count(*) FROM p2_commit_uniq WHERE id = 999`).Scan(&row999); err != nil {
		t.Fatalf("SELECT after canceled rawBegin Commit: %v", err)
	}
	if row999 != 0 {
		t.Fatalf("expected row 999 to be rolled back after canceled rawBegin Commit, got %d", row999)
	}
	txAfter, err := c1.Begin(ctx)
	if err != nil {
		t.Fatalf("Begin after canceled rawBegin Commit failed (tx leaked?): %v", err)
	}
	if err := txAfter.Rollback(ctx); err != nil {
		t.Fatalf("Rollback txAfter: %v", err)
	}
}
