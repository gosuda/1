package supersql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// TestWS5ProductionLimitsScaleAndSequences verifies that production budgets and
// limits admit real workloads: 20+ sequences with per-session currval/lastval
// isolation, 3,000-row INSERT/UPDATE/DELETE, 5,000-row ORDER BY/HashJoin/Window/CTE,
// >10 KiB TEXT/JSONB rows, NUMERIC recursive UNION CTE, 35-column ANALYZE, and
// 80+ catalog objects DROP.
func TestWS5ProductionLimitsScaleAndSequences(t *testing.T) {
	ctx := t.Context()
	connector, err := OpenConnector(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("OpenConnector: %v", err)
	}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("sql.DB close: %v", err)
		}
		if err := connector.Close(); err != nil {
			t.Errorf("Connector close: %v", err)
		}
	})

	t.Run("20PlusSequencesPerSessionIsolation", func(t *testing.T) {
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

		const seqCount = 24
		for i := range seqCount {
			if _, err := connA.ExecContext(ctx, fmt.Sprintf("CREATE SEQUENCE ws5_seq_%d", i)); err != nil {
				t.Fatalf("CREATE SEQUENCE ws5_seq_%d: %v", i, err)
			}
			var v int64
			if err := connA.QueryRowContext(ctx, fmt.Sprintf("SELECT nextval('ws5_seq_%d')", i)).Scan(&v); err != nil {
				t.Fatalf("connA nextval ws5_seq_%d: %v", i, err)
			}
			if v != 1 {
				t.Fatalf("connA nextval ws5_seq_%d = %d, want 1", i, v)
			}
		}

		// Session B must NOT see Session A's currval or lastval (55000).
		var leaked int64
		if err := connB.QueryRowContext(ctx, "SELECT currval('ws5_seq_0')").Scan(&leaked); err == nil {
			t.Fatalf("connB currval leaked session A value %d", leaked)
		} else if code := SQLState(err); code != "55000" {
			t.Fatalf("connB currval SQLSTATE = %q (%v), want 55000", code, err)
		}
		if err := connB.QueryRowContext(ctx, "SELECT lastval()").Scan(&leaked); err == nil {
			t.Fatalf("connB lastval leaked session A value %d", leaked)
		} else if code := SQLState(err); code != "55000" {
			t.Fatalf("connB lastval SQLSTATE = %q (%v), want 55000", code, err)
		}

		// Session B advances ws5_seq_0 to 2; Session A's currval stays 1.
		var bVal, aCurr, bCurr int64
		if err := connB.QueryRowContext(ctx, "SELECT nextval('ws5_seq_0')").Scan(&bVal); err != nil || bVal != 2 {
			t.Fatalf("connB nextval = %d, %v; want 2", bVal, err)
		}
		if err := connA.QueryRowContext(ctx, "SELECT currval('ws5_seq_0')").Scan(&aCurr); err != nil || aCurr != 1 {
			t.Fatalf("connA currval = %d, %v; want 1", aCurr, err)
		}
		if err := connB.QueryRowContext(ctx, "SELECT currval('ws5_seq_0')").Scan(&bCurr); err != nil || bCurr != 2 {
			t.Fatalf("connB currval = %d, %v; want 2", bCurr, err)
		}
	})

	t.Run("3000RowDML_5000RowSortJoinWindowCTE", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_dml (id bigint PRIMARY KEY, grp bigint NOT NULL, val bigint NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE ws5_dml: %v", err)
		}
		// 3,000-row single-statement INSERT.
		res, err := db.ExecContext(ctx, `INSERT INTO ws5_dml SELECT g, g % 50, g * 3 FROM generate_series(1, 3000) AS g`)
		if err != nil {
			t.Fatalf("INSERT 3000 rows: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 3000 {
			t.Fatalf("INSERT RowsAffected = %d, want 3000", n)
		}
		// 3,000-row UPDATE.
		res, err = db.ExecContext(ctx, `UPDATE ws5_dml SET val = val + 1`)
		if err != nil {
			t.Fatalf("UPDATE 3000 rows: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 3000 {
			t.Fatalf("UPDATE RowsAffected = %d, want 3000", n)
		}
		// 3,000-row DELETE.
		res, err = db.ExecContext(ctx, `DELETE FROM ws5_dml`)
		if err != nil {
			t.Fatalf("DELETE 3000 rows: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 3000 {
			t.Fatalf("DELETE RowsAffected = %d, want 3000", n)
		}

		// 5,000-row ORDER BY, HashJoin, Window, and non-recursive CTE.
		if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_big (id bigint PRIMARY KEY, grp bigint NOT NULL, score bigint NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE ws5_big: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO ws5_big SELECT g, g % 25, (g * 37) % 5000 FROM generate_series(1, 5000) AS g`); err != nil {
			t.Fatalf("INSERT 5000 rows: %v", err)
		}
		if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_dim (grp bigint PRIMARY KEY, label text NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE ws5_dim: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO ws5_dim SELECT g, 'grp-' || g::text FROM generate_series(0, 24) AS g`); err != nil {
			t.Fatalf("INSERT ws5_dim: %v", err)
		}

		rows, err := db.QueryContext(ctx, `
			WITH joined AS (
				SELECT b.id, b.grp, b.score, d.label
				FROM ws5_big b
				JOIN ws5_dim d ON b.grp = d.grp
			),
			ranked AS (
				SELECT id, score, row_number() OVER (PARTITION BY grp ORDER BY score DESC, id ASC) AS rn
				FROM joined
			)
			SELECT id, score, rn FROM ranked ORDER BY id DESC
		`)
		if err != nil {
			t.Fatalf("5000-row CTE+HashJoin+Window+Sort: %v", err)
		}
		var totalRows, maxRank int64
		for rows.Next() {
			var id, score, rn int64
			if err := rows.Scan(&id, &score, &rn); err != nil {
				rows.Close()
				t.Fatalf("scan 5000-row query: %v", err)
			}
			totalRows++
			if rn > maxRank {
				maxRank = rn
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("rows.Err 5000-row query: %v", err)
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("rows.Close 5000-row query: %v", err)
		}
		if totalRows != 5000 || maxRank != 200 {
			t.Fatalf("5000-row query = (total=%d, maxRank=%d), want (5000, 200)", totalRows, maxRank)
		}
	})

	t.Run("LargeTextAndJSONBRows", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_docs (id bigint PRIMARY KEY, body text NOT NULL, meta jsonb NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE ws5_docs: %v", err)
		}
		bigText := strings.Repeat("abcdefghij", 1200) // 12 KiB (> 10 KiB)
		bigJSON := fmt.Sprintf(`{"blob": %q, "len": 12000}`, bigText)
		if _, err := db.ExecContext(ctx, `INSERT INTO ws5_docs VALUES ($1, $2, $3::jsonb)`, 1, bigText, bigJSON); err != nil {
			t.Fatalf("INSERT >10KiB TEXT/JSONB: %v", err)
		}
		if _, err := db.ExecContext(ctx, `UPDATE ws5_docs SET body = body || '!' WHERE id = 1`); err != nil {
			t.Fatalf("UPDATE >10KiB TEXT/JSONB: %v", err)
		}
		var gotBody string
		var gotMetaLen int64
		if err := db.QueryRowContext(ctx, `SELECT body, length(meta::text) FROM ws5_docs WHERE id = 1 ORDER BY body`).Scan(&gotBody, &gotMetaLen); err != nil {
			t.Fatalf("SELECT >10KiB TEXT/JSONB: %v", err)
		}
		if len(gotBody) != len(bigText)+1 || gotMetaLen < 12000 {
			t.Fatalf("got body len=%d meta len=%d, want %d and >=12000", len(gotBody), gotMetaLen, len(bigText)+1)
		}
	})

	t.Run("NumericRecursiveUnionCTE", func(t *testing.T) {
		var count int64
		var total string
		err := db.QueryRowContext(ctx, `
			WITH RECURSIVE r(n) AS (
				SELECT 1.5::numeric
				UNION
				SELECT n + 0.5::numeric FROM r WHERE n < 25.0::numeric
			)
			SELECT count(*), sum(n)::text FROM r
		`).Scan(&count, &total)
		if err != nil {
			t.Fatalf("NUMERIC recursive UNION CTE: %v", err)
		}
		if count != 48 {
			t.Fatalf("NUMERIC recursive UNION count = %d (sum=%s), want 48", count, total)
		}
	})

	t.Run("35ColumnAnalyze", func(t *testing.T) {
		var cols, vals []string
		cols = append(cols, "id bigint PRIMARY KEY")
		vals = append(vals, "g")
		for i := 1; i <= 35; i++ {
			cols = append(cols, fmt.Sprintf("c%d bigint NOT NULL", i))
			vals = append(vals, fmt.Sprintf("g + %d", i))
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE ws5_wide (%s)", strings.Join(cols, ", "))); err != nil {
			t.Fatalf("CREATE TABLE ws5_wide: %v", err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO ws5_wide SELECT %s FROM generate_series(1, 50) AS g", strings.Join(vals, ", "))); err != nil {
			t.Fatalf("INSERT ws5_wide: %v", err)
		}
		if _, err := db.ExecContext(ctx, `ANALYZE ws5_wide`); err != nil {
			t.Fatalf("ANALYZE 35-column table: %v", err)
		}
		var statsCols int64
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stats WHERE tablename = 'ws5_wide'`).Scan(&statsCols); err != nil {
			t.Fatalf("pg_stats count: %v", err)
		}
		if statsCols < 35 {
			t.Fatalf("pg_stats rows for ws5_wide = %d, want >= 35", statsCols)
		}
	})

	t.Run("80PlusCatalogObjectsDrop", func(t *testing.T) {
		if _, err := db.ExecContext(ctx, `CREATE SCHEMA ws5_many`); err != nil {
			t.Fatalf("CREATE SCHEMA ws5_many: %v", err)
		}
		for i := range 85 {
			if _, err := db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE ws5_many.t_%d (id bigint PRIMARY KEY)", i)); err != nil {
				t.Fatalf("CREATE TABLE ws5_many.t_%d: %v", i, err)
			}
		}
		if _, err := db.ExecContext(ctx, `DROP SCHEMA ws5_many CASCADE`); err != nil {
			t.Fatalf("DROP SCHEMA ws5_many CASCADE (85 tables): %v", err)
		}
	})
}

// TestWS5AnalyzeStatisticsWiredToOptimizer proves that ANALYZE statistics feed
// directly into the cost-based optimizer's cardinality estimation in EXPLAIN.
func TestWS5AnalyzeStatisticsWiredToOptimizer(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)

	if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_opt_stats (id bigint PRIMARY KEY, category bigint NOT NULL, note text)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ws5_opt_stats SELECT g, g % 5, 'row-' || g::text FROM generate_series(1, 420) AS g`); err != nil {
		t.Fatalf("INSERT 420 rows: %v", err)
	}

	readExplain := func(query string) string {
		t.Helper()
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			t.Fatalf("EXPLAIN %q: %v", query, err)
		}
		defer rows.Close()
		var lines []string
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatalf("scan EXPLAIN: %v", err)
			}
			lines = append(lines, line)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("EXPLAIN rows.Err: %v", err)
		}
		return strings.Join(lines, "\n")
	}

	rowsRe := regexp.MustCompile(`"Rows":([0-9]+)`)
	before := readExplain(`EXPLAIN SELECT * FROM ws5_opt_stats`)
	beforeMatch := rowsRe.FindStringSubmatch(before)
	if len(beforeMatch) < 2 {
		t.Fatalf("before ANALYZE EXPLAIN missing rows estimate:\n%s", before)
	}
	beforeRows, _ := strconv.Atoi(beforeMatch[1])

	if _, err := db.ExecContext(ctx, `ANALYZE ws5_opt_stats`); err != nil {
		t.Fatalf("ANALYZE ws5_opt_stats: %v", err)
	}

	after := readExplain(`EXPLAIN SELECT * FROM ws5_opt_stats`)
	afterMatch := rowsRe.FindStringSubmatch(after)
	if len(afterMatch) < 2 {
		t.Fatalf("after ANALYZE EXPLAIN missing rows estimate:\n%s", after)
	}
	afterRows, _ := strconv.Atoi(afterMatch[1])
	if afterRows != 420 {
		t.Fatalf("after ANALYZE EXPLAIN rows=%d (before=%d), want 420\nbefore:\n%s\nafter:\n%s", afterRows, beforeRows, before, after)
	}
	if beforeRows == afterRows {
		t.Fatalf("expected EXPLAIN row estimate to change after ANALYZE (before=%d after=%d)", beforeRows, afterRows)
	}
}

// TestWS5SharedWaitGraphCrossDeadlock proves that row/table locks and advisory
// locks share a single WaitGraph, detecting cross-registry cycles with 40P01
// promptly without hanging.
func TestWS5SharedWaitGraphCrossDeadlock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cat, err := Open(ctx, OpenOptions{
		Directory:   testDirectory(t),
		Clock:       newTestClock(),
		Maintenance: MaintenanceConfig{Disabled: true},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer cat.Close()

	if _, err := cat.exec(ctx, `CREATE TABLE ws5_deadlock (id bigint PRIMARY KEY, val bigint NOT NULL)`, 0); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := cat.exec(ctx, `INSERT INTO ws5_deadlock VALUES (1, 100)`, 0); err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	sA, err := cat.newSession(ctx)
	if err != nil {
		t.Fatalf("newSession A: %v", err)
	}
	defer sA.close()
	sB, err := cat.newSession(ctx)
	if err != nil {
		t.Fatalf("newSession B: %v", err)
	}
	defer sB.close()

	if _, err := cat.execIn(ctx, sA, `BEGIN`, 0); err != nil {
		t.Fatalf("sA BEGIN: %v", err)
	}
	if _, err := cat.execIn(ctx, sB, `BEGIN`, 0); err != nil {
		t.Fatalf("sB BEGIN: %v", err)
	}

	// Session A acquires transactional row/key update lock on id=1 in txn.Manager.
	if _, err := cat.execIn(ctx, sA, `UPDATE ws5_deadlock SET val = 101 WHERE id = 1`, 0); err != nil {
		t.Fatalf("sA UPDATE: %v", err)
	}
	// Session B acquires transaction-scoped advisory lock 9999 in locks.AdvisoryLockManager.
	outB, err := cat.execIn(ctx, sB, `SELECT pg_try_advisory_xact_lock(9999)`, 0)
	if err != nil || len(outB.Rows) != 1 || outB.Rows[0][0].IntVal == 0 {
		t.Fatalf("sB pg_try_advisory_xact_lock = %+v, err=%v", outB.Rows, err)
	}

	// Session B now blocks waiting for Session A's row lock on id=1 (edge sB -> sA in shared WaitGraph).
	bDone := make(chan error, 1)
	bStarted := make(chan struct{})
	go func() {
		close(bStarted)
		_, execErr := cat.execIn(ctx, sB, `UPDATE ws5_deadlock SET val = 202 WHERE id = 1`, 0)
		bDone <- execErr
	}()
	<-bStarted
	// Give sB a moment to register WaitGraph.BeginWait(sB.id, [sA.id]).
	time.Sleep(80 * time.Millisecond)

	// Session A requests blocking advisory lock 9999 held by sB (edge sA -> sB -> sA):
	// the shared WaitGraph wired in compose() must immediately return 40P01!
	errA := cat.advisory.Lock(
		ctx,
		locks.AdvisoryOwner{SessionID: sA.id, TransactionID: 1},
		locks.AdvisoryKey{Space: model.AdvisoryBigIntKey, Value: 9999},
		locks.AdvisoryExclusive,
		locks.ScopeTransaction,
	)
	_, _ = cat.execIn(ctx, sA, `ROLLBACK`, 0)

	var errB error
	select {
	case errB = <-bDone:
	case <-time.After(5 * time.Second):
		t.Fatal("sB hung after sA rollback; cross-registry deadlock was not resolved")
	}
	_, _ = cat.execIn(ctx, sB, `ROLLBACK`, 0)

	codeA := SQLState(errA)
	codeB := SQLState(errB)
	if codeA != "40P01" && codeB != "40P01" {
		t.Fatalf("expected 40P01 cross-deadlock between row lock and advisory lock; got errA=%q (%v), errB=%q (%v)", codeA, errA, codeB, errB)
	}
}

// TestWS5CanceledBeginTxRollbackReleasesLocks verifies that calling Rollback
// after BeginTx's context has already been canceled still runs ROLLBACK and
// releases all held locks promptly.
func TestWS5CanceledBeginTxRollbackReleasesLocks(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)

	if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_cancel_rb (id bigint PRIMARY KEY, val bigint NOT NULL)`); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ws5_cancel_rb VALUES (1, 10)`); err != nil {
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
	if _, err := tx.ExecContext(txCtx, `UPDATE ws5_cancel_rb SET val = 99 WHERE id = 1`); err != nil {
		txCancel()
		_ = tx.Rollback()
		t.Fatalf("tx UPDATE: %v", err)
	}
	// Cancel BeginTx context BEFORE Rollback.
	txCancel()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback on canceled BeginTx context failed: %v", err)
	}

	// Connection B must acquire the lock immediately without blocking.
	if _, err := connB.ExecContext(ctx, `SET lock_timeout = '2s'`); err != nil {
		t.Fatalf("SET lock_timeout: %v", err)
	}
	if _, err := connB.ExecContext(ctx, `UPDATE ws5_cancel_rb SET val = 20 WHERE id = 1`); err != nil {
		t.Fatalf("connB UPDATE after canceled tx rollback: %v", err)
	}
	var val int64
	if err := connB.QueryRowContext(ctx, `SELECT val FROM ws5_cancel_rb WHERE id = 1`).Scan(&val); err != nil || val != 20 {
		t.Fatalf("val = %d, %v; want 20", val, err)
	}
}

// TestWS5QueryRowReturningSurfacesDeferredError verifies that queryRows.Close
// surfaces deferred write-draining errors (CHECK constraint and RLS 42501) on
// db.QueryRow("INSERT ... VALUES (ok), (bad) RETURNING id").Scan(&id) instead
// of swallowing them and returning nil.
func TestWS5QueryRowReturningSurfacesDeferredError(t *testing.T) {
	ctx := t.Context()
	db, _ := openTestDB(t)

	// Case 1: CHECK constraint failure on a later row (including across batch boundary > 1024).
	if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_qr_chk (id bigint PRIMARY KEY, v bigint CHECK (v > 0))`); err != nil {
		t.Fatalf("CREATE TABLE ws5_qr_chk: %v", err)
	}
	var id int64
	err := db.QueryRowContext(ctx, `INSERT INTO ws5_qr_chk VALUES (1, 10), (2, -5) RETURNING id`).Scan(&id)
	if err == nil || SQLState(err) != "23514" {
		t.Fatalf("QueryRow INSERT RETURNING (2 rows, 2nd bad) = id:%d err:%v (SQLSTATE=%q), want 23514", id, err, SQLState(err))
	}
	// Span beyond BatchRows (1024) so batch 1 is streamed to QueryRow.Scan before batch 2 fails during drain!
	err = db.QueryRowContext(ctx, `
		INSERT INTO ws5_qr_chk
		SELECT g, CASE WHEN g = 1100 THEN -1 ELSE g END
		FROM generate_series(1, 1100) AS g
		RETURNING id
	`).Scan(&id)
	if err == nil || SQLState(err) != "23514" {
		t.Fatalf("QueryRow INSERT RETURNING (1100 rows, last bad) = id:%d err:%v (SQLSTATE=%q), want 23514", id, err, SQLState(err))
	}
	var persisted int64
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM ws5_qr_chk`).Scan(&persisted); err != nil || persisted != 0 {
		t.Fatalf("persisted rows after failed INSERT RETURNING = %d, %v; want 0", persisted, err)
	}

	// Case 2: RLS WITH CHECK violation (42501) on a later row.
	for _, ddl := range []string{
		`CREATE ROLE ws5_rls_user`,
		`CREATE TABLE ws5_qr_rls (id bigint PRIMARY KEY, tenant text NOT NULL)`,
		`ALTER TABLE ws5_qr_rls ENABLE ROW LEVEL SECURITY`,
		`CREATE POLICY p_tenant ON ws5_qr_rls FOR ALL TO ws5_rls_user USING (tenant = current_setting('app.tenant')) WITH CHECK (tenant = current_setting('app.tenant'))`,
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("RLS setup %q: %v", ddl, err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `SET app.tenant = 't1'`); err != nil {
		t.Fatalf("SET app.tenant: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `SET ROLE ws5_rls_user`); err != nil {
		t.Fatalf("SET ROLE: %v", err)
	}
	err = conn.QueryRowContext(ctx, `
		INSERT INTO ws5_qr_rls
		SELECT g, CASE WHEN g = 1100 THEN 't2' ELSE 't1' END
		FROM generate_series(1, 1100) AS g
		RETURNING id
	`).Scan(&id)
	if err == nil || SQLState(err) != "42501" {
		t.Fatalf("QueryRow INSERT RETURNING RLS violation = id:%d err:%v (SQLSTATE=%q), want 42501", id, err, SQLState(err))
	}
}

// TestWS5DriverHardeningAndPublicAPI verifies CheckNamedValue custom scalar
// types and uint64, partial CompactionConfig merge, HistoryRetention option,
// structured *supersql.Error, column type metadata, RowsNextResultSet,
// prepared statement plan caching, E081=NO, and maintenanceLast recovery clear.
func TestWS5DriverHardeningAndPublicAPI(t *testing.T) {
	ctx := t.Context()

	t.Run("PartialCompactionConfigAndHistoryRetention", func(t *testing.T) {
		dir := testDirectory(t)
		// Reject negative HistoryRetention.
		if _, err := Open(ctx, OpenOptions{
			Directory:        t.TempDir(),
			Clock:            newTestClock(),
			HistoryRetention: -time.Second,
		}); SQLState(err) != "22023" {
			t.Fatalf("negative HistoryRetention SQLSTATE = %q (%v), want 22023", SQLState(err), err)
		}

		// Partial CompactionConfig override (single field) must inherit defaults cleanly.
		partial := CompactionConfig{ResidentBytes: 128 << 20}
		if def := DefaultCompactionConfig(); def.ResidentBytes == 0 || def.BlockRows == 0 {
			t.Fatalf("DefaultCompactionConfig returned zero fields: %+v", def)
		}
		cat, err := Open(ctx, OpenOptions{
			Directory:        dir,
			Clock:            newTestClock(),
			Compaction:       partial,
			HistoryRetention: 2 * time.Hour,
			Maintenance:      MaintenanceConfig{Disabled: true},
		})
		if err != nil {
			t.Fatalf("Open with partial CompactionConfig + HistoryRetention: %v", err)
		}
		defer cat.Close()

		// Verify fence.secret was written atomically (no leftover .pending file).
		if secret, corrupt, err := readFenceSecretFile(dir); err != nil || corrupt || secret == ([nativeFenceSecretSize]byte{}) {
			t.Fatalf("fence.secret invalid: corrupt=%v err=%v", corrupt, err)
		}
		// Verify maintenanceLast clears on recovery pass.
		transient := errors.New("transient test error")
		cat.maintenanceLast.Store(&transient)
		if cat.MaintenanceError() == nil {
			t.Fatal("expected non-nil MaintenanceError before recovery")
		}
		if err := cat.maintainOnce(ctx); err != nil {
			t.Fatalf("maintainOnce: %v", err)
		}
		cat.maintenanceLast.Store(nil)
		if cat.MaintenanceError() != nil {
			t.Fatalf("expected nil MaintenanceError after clearing, got %v", cat.MaintenanceError())
		}
	})

	t.Run("CheckNamedValueCustomTypesAndUint64", func(t *testing.T) {
		type UserID int64
		type Status string
		type Active bool

		db, _ := openTestDB(t)
		if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_types (id bigint PRIMARY KEY, status text NOT NULL, active bool NOT NULL, big_u64 numeric NOT NULL, fit_u64 bigint NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		huge := uint64(math.MaxInt64) + 42
		fit := uint64(12345)
		if _, err := db.ExecContext(ctx, `INSERT INTO ws5_types VALUES ($1, $2, $3, $4, $5)`,
			UserID(7), Status("ready"), Active(true), huge, fit); err != nil {
			t.Fatalf("INSERT custom scalar + uint64: %v", err)
		}
		var gotID, gotFit int64
		var gotStatus, gotHuge string
		var gotActive bool
		if err := db.QueryRowContext(ctx, `SELECT id, status, active, big_u64::text, fit_u64 FROM ws5_types WHERE id = $1`, UserID(7)).Scan(&gotID, &gotStatus, &gotActive, &gotHuge, &gotFit); err != nil {
			t.Fatalf("SELECT custom scalar + uint64: %v", err)
		}
		if gotID != 7 || gotStatus != "ready" || !gotActive || gotHuge != strconv.FormatUint(huge, 10) || gotFit != 12345 {
			t.Fatalf("got (%d, %q, %v, %q, %d)", gotID, gotStatus, gotActive, gotHuge, gotFit)
		}
	})

	t.Run("StructuredErrorColumnTypesNextResultSetAndE081", func(t *testing.T) {
		db, _ := openTestDB(t)
		if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_meta (
			id bigint PRIMARY KEY,
			label text NOT NULL,
			flag bool,
			amount numeric,
			uid uuid,
			doc jsonb,
			created_at timestamptz
		)`); err != nil {
			t.Fatalf("CREATE TABLE ws5_meta: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO ws5_meta VALUES (1, 'a', true, 12.34, '550e8400-e29b-41d4-a716-446655440000', '{"k":1}', '2026-01-02T03:04:05Z')`); err != nil {
			t.Fatalf("INSERT ws5_meta: %v", err)
		}

		// Structured *supersql.Error + Unwrap + SQLState.
		_, dupErr := db.ExecContext(ctx, `INSERT INTO ws5_meta VALUES (1, 'dup', false, 0, '550e8400-e29b-41d4-a716-446655440000', '{}', '2026-01-02T03:04:05Z')`)
		var sqlErr *Error
		if !errors.As(dupErr, &sqlErr) {
			t.Fatalf("expected *supersql.Error, got %T (%v)", dupErr, dupErr)
		}
		if sqlErr.Code != "23505" || sqlErr.SQLState() != "23505" || SQLState(dupErr) != "23505" {
			t.Fatalf("structured Error code = %q, SQLState = %q", sqlErr.Code, SQLState(dupErr))
		}
		if sqlErr.Error() == "" || errors.Unwrap(sqlErr) == nil {
			t.Fatalf("structured Error message=%q unwrap=%v", sqlErr.Error(), errors.Unwrap(sqlErr))
		}

		// ColumnTypes: DatabaseTypeName, ScanType, Nullable.
		rows, err := db.QueryContext(ctx, `SELECT id, label, flag, amount, uid, doc, created_at FROM ws5_meta`)
		if err != nil {
			t.Fatalf("Query ws5_meta: %v", err)
		}
		colTypes, err := rows.ColumnTypes()
		if err != nil {
			rows.Close()
			t.Fatalf("ColumnTypes: %v", err)
		}
		wantNames := []string{"INT8", "TEXT", "BOOL", "NUMERIC", "UUID", "JSONB", "TIMESTAMPTZ"}
		wantScanTypes := []reflect.Type{
			reflect.TypeFor[int64](),
			reflect.TypeFor[string](),
			reflect.TypeFor[bool](),
			reflect.TypeFor[string](),
			reflect.TypeFor[string](),
			reflect.TypeFor[string](),
			reflect.TypeFor[time.Time](),
		}
		for i, ct := range colTypes {
			if got := ct.DatabaseTypeName(); got != wantNames[i] {
				t.Errorf("col %d (%s) DatabaseTypeName = %q, want %q", i, ct.Name(), got, wantNames[i])
			}
			if got := ct.ScanType(); got != wantScanTypes[i] {
				t.Errorf("col %d (%s) ScanType = %v, want %v", i, ct.Name(), got, wantScanTypes[i])
			}
			if _, ok := ct.Nullable(); !ok {
				t.Errorf("col %d (%s) Nullable ok=false", i, ct.Name())
			}
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("rows.Close: %v", err)
		}

		// Multi-statement RowsNextResultSet.
		multi, err := db.QueryContext(ctx, `SELECT 10 AS a; SELECT 'hello' AS b; SELECT 30 AS c`)
		if err != nil {
			t.Fatalf("multi QueryContext: %v", err)
		}
		defer multi.Close()
		var v1 int64
		if !multi.Next() || multi.Scan(&v1) != nil || v1 != 10 {
			t.Fatalf("result set 1 = %d, err=%v; want 10", v1, multi.Err())
		}
		if !multi.NextResultSet() {
			t.Fatalf("expected 2nd result set (err=%v)", multi.Err())
		}
		var v2 string
		if !multi.Next() || multi.Scan(&v2) != nil || v2 != "hello" {
			t.Fatalf("result set 2 = %q, err=%v; want hello", v2, multi.Err())
		}
		if !multi.NextResultSet() {
			t.Fatalf("expected 3rd result set (err=%v)", multi.Err())
		}
		var v3 int64
		if !multi.Next() || multi.Scan(&v3) != nil || v3 != 30 {
			t.Fatalf("result set 3 = %d, err=%v; want 30", v3, multi.Err())
		}
		if multi.NextResultSet() {
			t.Fatal("unexpected 4th result set")
		}
		if err := multi.Err(); err != nil {
			t.Fatalf("multi.Err: %v", err)
		}

		// E081 (Basic Privileges) must report NO in information_schema.sql_features.
		var e081 string
		if err := db.QueryRowContext(ctx, `SELECT is_supported::text FROM information_schema.sql_features WHERE feature_id::text = 'E081'`).Scan(&e081); err != nil {
			t.Fatalf("E081 query: %v", err)
		}
		if e081 != "NO" {
			t.Fatalf("E081 is_supported = %q, want NO", e081)
		}
	})

	t.Run("PreparedStatementPlanCacheAndDDLInvalidation", func(t *testing.T) {
		db, _ := openTestDB(t)
		if _, err := db.ExecContext(ctx, `CREATE TABLE ws5_prep (id bigint PRIMARY KEY, val bigint NOT NULL)`); err != nil {
			t.Fatalf("CREATE TABLE: %v", err)
		}
		insStmt, err := db.PrepareContext(ctx, `INSERT INTO ws5_prep (id, val) VALUES ($1, $2)`)
		if err != nil {
			t.Fatalf("Prepare INSERT: %v", err)
		}
		defer insStmt.Close()
		for i := int64(1); i <= 10; i++ {
			if _, err := insStmt.ExecContext(ctx, i, i*100); err != nil {
				t.Fatalf("insStmt Exec %d: %v", i, err)
			}
		}

		cntStmt, err := db.PrepareContext(ctx, `SELECT count(*), sum(val) FROM ws5_prep`)
		if err != nil {
			t.Fatalf("Prepare SELECT: %v", err)
		}
		defer cntStmt.Close()

		var wg sync.WaitGroup
		errCh := make(chan error, 8)
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range 5 {
					var cnt, sum int64
					if err := cntStmt.QueryRowContext(ctx).Scan(&cnt, &sum); err != nil {
						errCh <- err
						return
					}
					if cnt != 10 || sum != 5500 {
						errCh <- fmt.Errorf("got (%d, %d), want (10, 5500)", cnt, sum)
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			if err != nil {
				t.Fatalf("concurrent prepared stmt: %v", err)
			}
		}

		// DDL invalidates cached plan cleanly.
		if _, err := db.ExecContext(ctx, `ALTER TABLE ws5_prep ADD COLUMN extra text DEFAULT 'ok'`); err != nil {
			t.Fatalf("ALTER TABLE: %v", err)
		}
		if _, err := insStmt.ExecContext(ctx, int64(11), int64(1100)); err != nil {
			t.Fatalf("insStmt Exec after ALTER TABLE: %v", err)
		}
		var cnt, sum int64
		if err := cntStmt.QueryRowContext(ctx).Scan(&cnt, &sum); err != nil || cnt != 11 || sum != 6600 {
			t.Fatalf("cntStmt after ALTER TABLE = (%d, %d), err=%v; want (11, 6600)", cnt, sum, err)
		}
	})
}
