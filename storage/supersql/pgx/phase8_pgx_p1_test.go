package pgx_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha"
	"github.com/gosuda/supersql-alpha/pgx"
)

type failingAfterRowsSource struct {
	total int
	idx   int
	idOff int
	err   error
}

func (s *failingAfterRowsSource) Next() bool {
	if s.idx >= s.total {
		return false
	}
	s.idx++
	return true
}

func (s *failingAfterRowsSource) Values() ([]any, error) {
	id := s.idOff + s.idx
	return []any{int64(id), fmt.Sprintf("v-%d", id)}, nil
}

func (s *failingAfterRowsSource) Err() error {
	if s.idx >= s.total {
		return s.err
	}
	return nil
}

func TestPhase8PgxTxCopyFromMultiBatchFailureReturnsZero(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	t.Run("pk_violation_later_batch", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p8_copy_pk (id bigint PRIMARY KEY, val text NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO p8_copy_pk (id, val) VALUES (1, 'outer')`); err != nil {
			t.Fatalf("outer INSERT: %v", err)
		}

		// Batch size in copyFromExecer is 200; insert 250 rows where row 210 duplicates id 100 from batch 1.
		rows := make([][]any, 250)
		for i := range rows {
			id := int64(100 + i)
			if i == 210 {
				id = 100
			}
			rows[i] = []any{id, fmt.Sprintf("row-%d", i)}
		}

		n, err := tx.CopyFrom(ctx, pgx.Identifier{"p8_copy_pk"}, []string{"id", "val"}, pgx.CopyFromRows(rows))
		if err == nil {
			t.Fatalf("expected error from multi-batch CopyFrom PK violation, got nil (count=%d)", n)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows returned on failed tx.CopyFrom, got %d (err=%v)", n, err)
		}
		assertPhase2PgErr(t, err, "23505")

		// Only the outer row inserted prior to CopyFrom must be visible inside tx.
		var inTxCount int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM p8_copy_pk`).Scan(&inTxCount); err != nil {
			t.Fatalf("SELECT count in tx: %v", err)
		}
		if inTxCount != 1 {
			t.Fatalf("expected 1 row (outer insert) inside tx after failed CopyFrom, got %d", inTxCount)
		}

		// Outer tx must still be healthy and committable.
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("outer Commit: %v", err)
		}

		var persisted int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p8_copy_pk`).Scan(&persisted); err != nil {
			t.Fatalf("SELECT count after commit: %v", err)
		}
		if persisted != 1 {
			t.Fatalf("expected 1 persisted row after commit, got %d", persisted)
		}
	})

	t.Run("check_violation_later_batch", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p8_copy_chk (id bigint PRIMARY KEY, val text NOT NULL CHECK (val <> 'bad'))`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO p8_copy_chk (id, val) VALUES (1, 'outer')`); err != nil {
			t.Fatalf("outer INSERT: %v", err)
		}

		rows := make([][]any, 230)
		for i := range rows {
			val := fmt.Sprintf("ok-%d", i)
			if i == 220 {
				val = "bad"
			}
			rows[i] = []any{int64(100 + i), val}
		}

		n, err := tx.CopyFrom(ctx, pgx.Identifier{"p8_copy_chk"}, []string{"id", "val"}, pgx.CopyFromRows(rows))
		if err == nil {
			t.Fatalf("expected error from multi-batch CopyFrom CHECK violation, got nil (count=%d)", n)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows returned on failed tx.CopyFrom CHECK violation, got %d (err=%v)", n, err)
		}
		assertPhase2PgErr(t, err, "23514")

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("outer Commit: %v", err)
		}

		var persisted int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p8_copy_chk`).Scan(&persisted); err != nil {
			t.Fatalf("SELECT count: %v", err)
		}
		if persisted != 1 {
			t.Fatalf("expected 1 persisted row after commit, got %d", persisted)
		}
	})

	t.Run("rowsrc_err_after_first_batch", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p8_copy_srcerr (id bigint PRIMARY KEY, val text NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO p8_copy_srcerr (id, val) VALUES (1, 'outer')`); err != nil {
			t.Fatalf("outer INSERT: %v", err)
		}

		sentinel := errors.New("stream truncated after batch 1")
		src := &failingAfterRowsSource{total: 205, idOff: 100, err: sentinel}
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"p8_copy_srcerr"}, []string{"id", "val"}, src)
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected sentinel error %v, got %v (count=%d)", sentinel, err, n)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows returned when rowSrc.Err() fails after batch 1, got %d", n)
		}

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("outer Commit: %v", err)
		}

		var persisted int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p8_copy_srcerr`).Scan(&persisted); err != nil {
			t.Fatalf("SELECT count: %v", err)
		}
		if persisted != 1 {
			t.Fatalf("expected 1 persisted row after commit, got %d", persisted)
		}
	})

	t.Run("rowsrc_values_err_after_first_batch", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p8_copy_valerr (id bigint PRIMARY KEY, val text NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO p8_copy_valerr (id, val) VALUES (1, 'outer')`); err != nil {
			t.Fatalf("outer INSERT: %v", err)
		}

		sentinel := errors.New("row generator boom")
		src := pgx.CopyFromSlice(220, func(i int) ([]any, error) {
			if i == 205 {
				return nil, sentinel
			}
			return []any{int64(100 + i), fmt.Sprintf("v-%d", i)}, nil
		})
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"p8_copy_valerr"}, []string{"id", "val"}, src)
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected sentinel error %v, got %v (count=%d)", sentinel, err, n)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows returned when rowSrc.Values() fails after batch 1, got %d", n)
		}

		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("outer Commit: %v", err)
		}

		var persisted int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p8_copy_valerr`).Scan(&persisted); err != nil {
			t.Fatalf("SELECT count: %v", err)
		}
		if persisted != 1 {
			t.Fatalf("expected 1 persisted row after commit, got %d", persisted)
		}
	})

	t.Run("closed_tx_returns_zero", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p8_copy_closed (id bigint PRIMARY KEY)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("Begin: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"p8_copy_closed"}, []string{"id"}, pgx.CopyFromRows([][]any{{int64(1)}}))
		if !errors.Is(err, pgx.ErrTxClosed) {
			t.Fatalf("expected ErrTxClosed, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows on closed tx, got %d", n)
		}
	})
}

func TestPhase8PoolAndConnCopyFromFailureReturnsZero(t *testing.T) {
	ctx := t.Context()
	pool := openTestPool(t)

	t.Run("pool_copyfrom_multibatch_pk_failure", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p8_pool_copy (id bigint PRIMARY KEY, val text NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}

		rows := make([][]any, 240)
		for i := range rows {
			id := int64(1 + i)
			if i == 215 {
				id = 1
			}
			rows[i] = []any{id, fmt.Sprintf("v-%d", i)}
		}

		n, err := pool.CopyFrom(ctx, pgx.Identifier{"p8_pool_copy"}, []string{"id", "val"}, pgx.CopyFromRows(rows))
		if err == nil {
			t.Fatalf("expected error from Pool.CopyFrom, got nil (count=%d)", n)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows returned from failed Pool.CopyFrom, got %d (err=%v)", n, err)
		}
		assertPhase2PgErr(t, err, "23505")

		var persisted int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p8_pool_copy`).Scan(&persisted); err != nil {
			t.Fatalf("SELECT count: %v", err)
		}
		if persisted != 0 {
			t.Fatalf("expected 0 persisted rows after failed Pool.CopyFrom, got %d", persisted)
		}
	})

	t.Run("pool_copyfrom_nil_source", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p8_pool_nilsrc (id bigint PRIMARY KEY)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		n, err := pool.CopyFrom(ctx, pgx.Identifier{"p8_pool_nilsrc"}, []string{"id"}, nil)
		if err == nil {
			t.Fatalf("expected error for nil CopyFromSource, got nil")
		}
		if n != 0 {
			t.Fatalf("expected 0 rows for nil CopyFromSource, got %d", n)
		}
	})

	t.Run("conn_copyfrom_autocommit_and_manual_begin_savepoint", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `CREATE TABLE p8_conn_copy (id bigint PRIMARY KEY, val text NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}

		pc, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer pc.Release()
		conn := pc.Conn()

		// 1. Autocommit Conn.CopyFrom multi-batch failure -> (0, err) and 0 rows persisted.
		sentinel := errors.New("conn copy stream error")
		n, err := conn.CopyFrom(ctx, pgx.Identifier{"p8_conn_copy"}, []string{"id", "val"}, &failingAfterRowsSource{total: 205, idOff: 10, err: sentinel})
		if !errors.Is(err, sentinel) {
			t.Fatalf("expected sentinel error from Conn.CopyFrom, got %v", err)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows returned from failed Conn.CopyFrom, got %d", n)
		}
		var cnt int64
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM p8_conn_copy`).Scan(&cnt); err != nil {
			t.Fatalf("SELECT count: %v", err)
		}
		if cnt != 0 {
			t.Fatalf("expected 0 persisted rows after failed autocommit Conn.CopyFrom, got %d", cnt)
		}

		// 2. Manual BEGIN block on Conn -> CopyFrom uses savepoint; failure returns (0, err)
		//    and preserves earlier row inserted in the manual transaction.
		if _, err := conn.Exec(ctx, `BEGIN`); err != nil {
			t.Fatalf("BEGIN: %v", err)
		}
		if _, err := conn.Exec(ctx, `INSERT INTO p8_conn_copy (id, val) VALUES (1, 'manual-outer')`); err != nil {
			t.Fatalf("outer INSERT: %v", err)
		}
		rows := make([][]any, 225)
		for i := range rows {
			id := int64(500 + i)
			if i == 210 {
				id = 500
			}
			rows[i] = []any{id, fmt.Sprintf("v-%d", i)}
		}
		n, err = conn.CopyFrom(ctx, pgx.Identifier{"p8_conn_copy"}, []string{"id", "val"}, pgx.CopyFromRows(rows))
		if err == nil {
			t.Fatalf("expected error from savepoint Conn.CopyFrom, got nil (count=%d)", n)
		}
		if n != 0 {
			t.Fatalf("expected 0 rows from failed savepoint Conn.CopyFrom, got %d", n)
		}
		assertPhase2PgErr(t, err, "23505")

		if _, err := conn.Exec(ctx, `COMMIT`); err != nil {
			t.Fatalf("COMMIT: %v", err)
		}
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM p8_conn_copy`).Scan(&cnt); err != nil {
			t.Fatalf("SELECT count after manual commit: %v", err)
		}
		if cnt != 1 {
			t.Fatalf("expected 1 persisted row after manual COMMIT, got %d", cnt)
		}
	})
}

func assertPoolClosedOps(t *testing.T, ctx context.Context, pool *pgx.Pool) {
	t.Helper()

	if err := pool.Ping(ctx); !errors.Is(err, supersql.ErrDatabaseClosed) {
		t.Errorf("Ping: expected ErrDatabaseClosed, got %v", err)
	}
	if pc, err := pool.Acquire(ctx); !errors.Is(err, supersql.ErrDatabaseClosed) {
		if pc != nil {
			pc.Release()
		}
		t.Errorf("Acquire: expected ErrDatabaseClosed, got %v", err)
	}
	if _, err := pool.Exec(ctx, `SELECT 1`); !errors.Is(err, supersql.ErrDatabaseClosed) {
		t.Errorf("Exec: expected ErrDatabaseClosed, got %v", err)
	}
	if rows, err := pool.Query(ctx, `SELECT 1`); !errors.Is(err, supersql.ErrDatabaseClosed) {
		if rows != nil {
			rows.Close()
		}
		t.Errorf("Query: expected ErrDatabaseClosed, got %v", err)
	}
	var one int64
	if err := pool.QueryRow(ctx, `SELECT 1`).Scan(&one); !errors.Is(err, supersql.ErrDatabaseClosed) {
		t.Errorf("QueryRow.Scan: expected ErrDatabaseClosed, got %v", err)
	}
	if tx, err := pool.Begin(ctx); !errors.Is(err, supersql.ErrDatabaseClosed) {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
		t.Errorf("Begin: expected ErrDatabaseClosed, got %v", err)
	}
	if tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable}); !errors.Is(err, supersql.ErrDatabaseClosed) {
		if tx != nil {
			_ = tx.Rollback(ctx)
		}
		t.Errorf("BeginTx: expected ErrDatabaseClosed, got %v", err)
	}
	n, err := pool.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"id"}, pgx.CopyFromRows([][]any{{int64(1)}}))
	if !errors.Is(err, supersql.ErrDatabaseClosed) || n != 0 {
		t.Errorf("CopyFrom: expected (0, ErrDatabaseClosed), got (%d, %v)", n, err)
	}
	b := &pgx.Batch{}
	b.Queue(`SELECT 1`)
	if err := pool.SendBatch(ctx, b).Close(); !errors.Is(err, supersql.ErrDatabaseClosed) {
		t.Errorf("SendBatch.Close: expected ErrDatabaseClosed, got %v", err)
	}
}

func TestPhase8NewPoolFromCatalogClosedAndNil(t *testing.T) {
	ctx := t.Context()

	t.Run("nil_catalog", func(t *testing.T) {
		pool := pgx.NewPoolFromCatalog(nil)
		if pool.Catalog() != nil {
			t.Fatalf("expected nil Catalog, got %v", pool.Catalog())
		}
		_ = pool.Stat()
		assertPoolClosedOps(t, ctx, pool)
		pool.Close()
		pool.Close()
	})

	t.Run("nil_connector", func(t *testing.T) {
		pool := pgx.NewPoolFromConnector(nil)
		if pool.Catalog() != nil {
			t.Fatalf("expected nil Catalog, got %v", pool.Catalog())
		}
		_ = pool.Stat()
		assertPoolClosedOps(t, ctx, pool)
		pool.Close()
		pool.Close()
	})

	t.Run("closed_catalog_before_pool", func(t *testing.T) {
		cat, err := supersql.Open(ctx, supersql.OpenOptions{
			Directory: t.TempDir(),
			Clock:     newTestClock(),
		})
		if err != nil {
			t.Fatalf("supersql.Open: %v", err)
		}
		if err := cat.Close(); err != nil {
			t.Fatalf("cat.Close: %v", err)
		}

		pool := pgx.NewPoolFromCatalog(cat)
		defer pool.Close()

		assertPoolClosedOps(t, ctx, pool)
		pool.Close()
		pool.Close()
	})

	t.Run("closed_catalog_with_idle_pooled_conn", func(t *testing.T) {
		cat, err := supersql.Open(ctx, supersql.OpenOptions{
			Directory: t.TempDir(),
			Clock:     newTestClock(),
		})
		if err != nil {
			t.Fatalf("supersql.Open: %v", err)
		}
		pool := pgx.NewPoolFromCatalog(cat)
		defer pool.Close()

		// Prime sql.DB with an idle connection.
		if _, err := pool.Exec(ctx, `SELECT 1`); err != nil {
			t.Fatalf("priming SELECT 1: %v", err)
		}
		if err := cat.Close(); err != nil {
			t.Fatalf("cat.Close: %v", err)
		}

		assertPoolClosedOps(t, ctx, pool)
	})
}
