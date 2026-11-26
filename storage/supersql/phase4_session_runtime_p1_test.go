package supersql

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestPhase4SessionRuntimeTempTableOnCommitDeleteRows(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	mustExec := func(sql string) outcome {
		t.Helper()
		outcome, err := db.exec(ctx, sql, 1000)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return outcome
	}

	mustExec(`CREATE TEMP TABLE tmp_del (id int PRIMARY KEY, k text) ON COMMIT DELETE ROWS`)
	mustExec(`CREATE INDEX tmp_del_k_idx ON tmp_del (k)`)

	// Commit round 1: rows visible before commit, cleared on commit.
	mustExec(`BEGIN`)
	mustExec(`INSERT INTO tmp_del VALUES (1, 'alpha'), (2, 'beta')`)
	out := mustExec(`SELECT count(*) FROM tmp_del`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 2 {
		t.Fatalf("before commit 1: got %v, want count=2", out.Rows)
	}
	mustExec(`COMMIT`)

	out = mustExec(`SELECT count(*) FROM tmp_del`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 0 {
		t.Fatalf("after commit 1: got %v, want count=0", out.Rows)
	}

	// Commit round 2: table and secondary index must remain writable and readable
	// after the generation bump from ON COMMIT DELETE ROWS.
	mustExec(`BEGIN`)
	mustExec(`INSERT INTO tmp_del VALUES (1, 'gamma'), (3, 'delta')`)
	out = mustExec(`SELECT id, k FROM tmp_del WHERE k = 'gamma'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 1 || out.Rows[0][1].StrVal != "gamma" {
		t.Fatalf("round 2 secondary index lookup: got %v, want (1, 'gamma')", out.Rows)
	}
	out = mustExec(`SELECT count(*) FROM tmp_del`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 2 {
		t.Fatalf("before commit 2: got %v, want count=2", out.Rows)
	}
	mustExec(`COMMIT`)

	out = mustExec(`SELECT count(*) FROM tmp_del`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 0 {
		t.Fatalf("after commit 2: got %v, want count=0", out.Rows)
	}

	// Commit round 3: verify repeated commits keep refreshing mutation targets.
	mustExec(`BEGIN`)
	mustExec(`INSERT INTO tmp_del VALUES (10, 'epsilon')`)
	out = mustExec(`SELECT k FROM tmp_del WHERE id = 10`)
	if len(out.Rows) != 1 || out.Rows[0][0].StrVal != "epsilon" {
		t.Fatalf("round 3 lookup: got %v, want 'epsilon'", out.Rows)
	}
	mustExec(`COMMIT`)

	out = mustExec(`SELECT count(*) FROM tmp_del`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 0 {
		t.Fatalf("after commit 3: got %v, want count=0", out.Rows)
	}
}

func TestPhase4SessionRuntimeTempTableDropRollback(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	mustExec := func(sql string) outcome {
		t.Helper()
		outcome, err := db.exec(ctx, sql, 1000)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return outcome
	}

	mustExec(`CREATE TEMP TABLE tmp_rb (id int PRIMARY KEY, val text)`)
	mustExec(`CREATE INDEX tmp_rb_val_idx ON tmp_rb (val)`)
	mustExec(`INSERT INTO tmp_rb VALUES (1, 'one')`)

	// Full transaction ROLLBACK after DROP TABLE must restore temporaryTargets and temporaryRows.
	mustExec(`BEGIN`)
	mustExec(`DROP TABLE tmp_rb`)
	mustExec(`ROLLBACK`)

	mustExec(`INSERT INTO tmp_rb VALUES (2, 'two')`)
	out := mustExec(`SELECT id, val FROM tmp_rb ORDER BY id`)
	if len(out.Rows) != 2 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 2 {
		t.Fatalf("after DROP+ROLLBACK: got %v, want rows 1 and 2", out.Rows)
	}
	out = mustExec(`SELECT id FROM tmp_rb WHERE val = 'two'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 2 {
		t.Fatalf("secondary index after DROP+ROLLBACK: got %v, want id=2", out.Rows)
	}

	// Savepoint ROLLBACK TO after DROP TABLE must also restore temporaryTargets and temporaryRows.
	mustExec(`BEGIN`)
	mustExec(`INSERT INTO tmp_rb VALUES (3, 'three')`)
	mustExec(`SAVEPOINT sp_drop`)
	mustExec(`DROP TABLE tmp_rb`)
	mustExec(`ROLLBACK TO SAVEPOINT sp_drop`)
	mustExec(`INSERT INTO tmp_rb VALUES (4, 'four')`)
	mustExec(`COMMIT`)

	out = mustExec(`SELECT id FROM tmp_rb ORDER BY id`)
	if len(out.Rows) != 4 || out.Rows[0][0].IntVal != 1 || out.Rows[1][0].IntVal != 2 || out.Rows[2][0].IntVal != 3 || out.Rows[3][0].IntVal != 4 {
		t.Fatalf("after savepoint DROP+ROLLBACK TO: got %v, want ids [1 2 3 4]", out.Rows)
	}
	out = mustExec(`SELECT id FROM tmp_rb WHERE val = 'four'`)
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 4 {
		t.Fatalf("secondary index after savepoint DROP+ROLLBACK TO: got %v, want id=4", out.Rows)
	}
}

func TestPhase4SessionRuntimeConcurrentRunAndDescribe(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	for _, sql := range []string{
		`CREATE DOMAIN pos_int AS int CHECK (value > 0)`,
		`CREATE TABLE race_tbl (id pos_int PRIMARY KEY, label text)`,
		`INSERT INTO race_tbl VALUES (1, 'init')`,
		`CREATE FUNCTION insert_and_count(v int) RETURNS int LANGUAGE plpgsql AS $$ DECLARE c int; BEGIN INSERT INTO race_tbl VALUES (v, 'from_fn'); SELECT count(*) INTO c FROM race_tbl; RETURN c; END; $$`,
	} {
		if _, err := db.exec(ctx, sql, 1000); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// Routine reentry through RunStatement must work without deadlocking and
	// preserve outer statementTypes.
	out, err := db.exec(ctx, `SELECT insert_and_count(2)`, 1000)
	if err != nil {
		t.Fatalf("routine reentry: %v", err)
	}
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 2 {
		t.Fatalf("routine reentry result: got %v, want 2", out.Rows)
	}

	// Concurrent RunStatement, DescribeStatement, and pre-dispatch validation
	// rejection (markFailedIn) on the same session must be race-free.
	scope := db.embeddedScope()
	parsed, err := zpgparse.Parse(`SELECT id, label FROM race_tbl WHERE id = 1`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if parsed == nil || len(parsed.Stmts) != 1 {
		t.Fatalf("unexpected parse result")
	}
	describeStmt := parsed.Stmts[0]

	const iterations = 20
	var wg sync.WaitGroup
	errCh := make(chan error, 4*iterations)

	// Worker 1: RunStatement SELECT
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			outcome, err := db.exec(ctx, `SELECT id, label FROM race_tbl WHERE id = 1`, 100)
			if err != nil {
				errCh <- err
				return
			}
			if len(outcome.Rows) != 1 || outcome.Rows[0][0].IntVal != 1 {
				errCh <- errors.New("unexpected SELECT result")
				return
			}
		}
	}()

	// Worker 2: DescribeStatement on the same session
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			builtins := scope.builtins()
			execOpts := session.PreparedExecution{
				Catalog:     db.catalog,
				Statistics:  ptrTo(scope.statisticsCommandOptions(builtins)),
				TypeRuntime: scope.typeRuntimeOptions(builtins),
				Routines:    scope.routineExecutionOptions(builtins),
			}
			execOpts.Environment.Builtin = builtins
			cols, err := scope.txns.DescribeStatement(ctx, describeStmt, session.StatementExecution{
				Execution: execOpts,
				Planning: func(ctx context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (session.PrepareOptions, error) {
					return scope.prepareOptions(ctx, snapshot, reader, builtins)
				},
			})
			if err != nil {
				errCh <- err
				return
			}
			if len(cols) != 2 {
				errCh <- errors.New("unexpected DescribeStatement column count")
				return
			}
		}
	}()

	// Worker 3: RunStatement with invalid/nil statement hitting ValidateStatementFeatures -> markFailedIn
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			_, err := scope.txns.RunStatement(ctx, &zpgparse.RawStmt{}, session.StatementExecution{}, nil)
			if err == nil {
				errCh <- errors.New("expected error for nil statement")
				return
			}
		}
	}()

	// Worker 4: Concurrent RunStatement calling routine reentry
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range iterations {
			outcome, err := db.exec(ctx, `SELECT count(*) FROM race_tbl`, 100)
			if err != nil {
				errCh <- err
				return
			}
			if len(outcome.Rows) != 1 || outcome.Rows[0][0].IntVal != 2 {
				errCh <- errors.New("unexpected count result")
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent session operation failed: %v", err)
	}
}

func TestPhase4SessionRuntimeGroupRunPanicCleanup(t *testing.T) {
	ctx := t.Context()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 2, QueueCapacity: 4, MaxGroups: 4})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	runWithRecover := func(fn func()) (recovered any) {
		defer func() {
			recovered = recover()
		}()
		fn()
		return nil
	}

	closeWithTimeout := func(name string, fn func() error) error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			done <- fn()
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(2 * time.Second):
			t.Fatalf("%s hung after task panic", name)
			return nil
		}
	}

	// Case 1: Panic inside Task.Run; Cleanup must still run once, and Group.Close returns promptly.
	g1, err := pool.NewGroup(ctx)
	if err != nil {
		t.Fatalf("NewGroup g1: %v", err)
	}
	cleanupCount1 := 0
	rec1 := runWithRecover(func() {
		_ = g1.Run(ctx, runtime.Task{
			Run: func(context.Context) error {
				panic("boom-in-run")
			},
			Cleanup: func() error {
				cleanupCount1++
				return nil
			},
		})
	})
	if rec1 != "boom-in-run" {
		t.Fatalf("case 1 recovered %v, want boom-in-run", rec1)
	}
	if cleanupCount1 != 1 {
		t.Fatalf("case 1 cleanup count = %d, want 1", cleanupCount1)
	}
	if err := closeWithTimeout("g1.Close", g1.Close); !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("g1.Close() = %v, want ErrClosed", err)
	}

	// Case 2: Panic inside Task.Cleanup; Cleanup must run only once, and Group.Close returns promptly.
	g2, err := pool.NewGroup(ctx)
	if err != nil {
		t.Fatalf("NewGroup g2: %v", err)
	}
	cleanupCount2 := 0
	rec2 := runWithRecover(func() {
		_ = g2.Run(ctx, runtime.Task{
			Run: func(context.Context) error {
				return nil
			},
			Cleanup: func() error {
				cleanupCount2++
				panic("boom-in-cleanup")
			},
		})
	})
	if rec2 != "boom-in-cleanup" {
		t.Fatalf("case 2 recovered %v, want boom-in-cleanup", rec2)
	}
	if cleanupCount2 != 1 {
		t.Fatalf("case 2 cleanup count = %d, want 1", cleanupCount2)
	}
	if err := closeWithTimeout("g2.Close", g2.Close); !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("g2.Close() = %v, want ErrClosed", err)
	}

	// Case 3: Panic in both Task.Run and Task.Cleanup; Group.Close still returns promptly.
	g3, err := pool.NewGroup(ctx)
	if err != nil {
		t.Fatalf("NewGroup g3: %v", err)
	}
	cleanupCount3 := 0
	rec3 := runWithRecover(func() {
		_ = g3.Run(ctx, runtime.Task{
			Run: func(context.Context) error {
				panic("boom-run-3")
			},
			Cleanup: func() error {
				cleanupCount3++
				panic("boom-cleanup-3")
			},
		})
	})
	if rec3 != "boom-cleanup-3" {
		t.Fatalf("case 3 recovered %v, want boom-cleanup-3", rec3)
	}
	if cleanupCount3 != 1 {
		t.Fatalf("case 3 cleanup count = %d, want 1", cleanupCount3)
	}
	if err := closeWithTimeout("g3.Close", g3.Close); !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("g3.Close() = %v, want ErrClosed", err)
	}

	// Pool.Close must also return promptly.
	if err := closeWithTimeout("pool.Close", pool.Close); err != nil {
		t.Fatalf("pool.Close() = %v, want nil", err)
	}
}

type epochTrackingClock struct {
	mu          sync.Mutex
	earliest    clock.Instant
	latest      clock.Instant
	scale       clock.ScaleDescriptor
	epoch       clock.Epoch
	waitCalls   int
	lastTarget  clock.Instant
	lastEpoch   clock.Epoch
	stallOnWait bool
}

func newEpochTrackingClock() *epochTrackingClock {
	base := newTestClock()
	return &epochTrackingClock{
		earliest: 1000,
		latest:   1500,
		scale:    base.scale,
		epoch:    base.epoch,
	}
}

func (c *epochTrackingClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return clock.Reading{
		Earliest: c.earliest,
		Latest:   c.latest,
		Status:   clock.Synced,
		Epoch:    c.epoch,
	}, nil
}

func (c *epochTrackingClock) WaitUntilAfter(ctx context.Context, value clock.Instant, epoch clock.Epoch) error {
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
	c.waitCalls++
	c.lastTarget = value
	c.lastEpoch = epoch
	if c.stallOnWait {
		// Deliberately fail to advance Earliest past value so checkFence rejects.
		return nil
	}
	c.earliest = max(c.earliest, value+10)
	c.latest = max(c.latest, c.earliest+500)
	return nil
}

func (c *epochTrackingClock) Scale() clock.ScaleDescriptor {
	c.mu.Lock()
	defer c.mu.Unlock()
	owned := c.scale
	owned.CanonicalLeapHistory = append([]byte(nil), c.scale.CanonicalLeapHistory...)
	return owned
}

func (c *epochTrackingClock) FromUTC(value time.Time) (clock.Instant, error) {
	return clock.Instant(value.UnixNano()), nil
}

func (c *epochTrackingClock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	return clock.UTCProjection{Time: time.Unix(0, int64(value)).UTC()}, nil
}

func (c *epochTrackingClock) setEpoch(assuranceByte byte, stall bool) clock.Epoch {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.epoch.Assurance = [16]byte{assuranceByte}
	c.stallOnWait = stall
	// Keep an uncertainty window where Earliest <= Latest so commit timestamp > Earliest.
	c.latest = max(c.latest, c.earliest+500)
	return c.epoch
}

func (c *epochTrackingClock) stats() (waits int, target clock.Instant, epoch clock.Epoch, earliest clock.Instant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waitCalls, c.lastTarget, c.lastEpoch, c.earliest
}

func TestPhase4SessionRuntimeCommitSequencerEpochResync(t *testing.T) {
	ctx := t.Context()
	clk := newEpochTrackingClock()
	db, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: clk})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()

	// Commit 1 in epoch 1 anchors the sequencer.
	if _, err := db.exec(ctx, `CREATE TABLE seq_epoch_tbl (id int PRIMARY KEY)`, 100); err != nil {
		t.Fatalf("CREATE TABLE: %v", err)
	}
	waits1, _, epoch1, _ := clk.stats()
	if waits1 == 0 {
		t.Fatalf("expected initial WaitUntilAfter to anchor epoch 1")
	}

	// Commit 2 still in epoch 1 reuses the anchor without another WaitUntilAfter.
	if _, err := db.exec(ctx, `INSERT INTO seq_epoch_tbl VALUES (1)`, 100); err != nil {
		t.Fatalf("INSERT 1: %v", err)
	}
	waits2, _, _, _ := clk.stats()
	if waits2 != waits1 {
		t.Fatalf("same-epoch commit called WaitUntilAfter unexpectedly: got %d, want %d", waits2, waits1)
	}

	// Switch to epoch 2: commit sequencer must detect epoch change and call
	// WaitUntilAfter strictly past timestamp in the new epoch.
	epoch2 := clk.setEpoch(2, false)
	if epoch2 == epoch1 {
		t.Fatalf("epoch did not change")
	}
	if _, err := db.exec(ctx, `INSERT INTO seq_epoch_tbl VALUES (2)`, 100); err != nil {
		t.Fatalf("INSERT 2 in epoch 2: %v", err)
	}
	waits3, target3, waitEpoch3, earliest3 := clk.stats()
	if waits3 <= waits2 {
		t.Fatalf("epoch change did not trigger WaitUntilAfter: before=%d after=%d", waits2, waits3)
	}
	if waitEpoch3 != epoch2 {
		t.Fatalf("WaitUntilAfter epoch = %v, want %v", waitEpoch3, epoch2)
	}
	if earliest3 <= target3 {
		t.Fatalf("clock earliest %d was not strictly past commit timestamp %d", earliest3, target3)
	}

	// Switch to epoch 3 with a stalled clock that does not advance Earliest past
	// timestamp: checkFence must reject because anchored was reset before waiting.
	epoch3 := clk.setEpoch(3, true)
	if _, err := db.exec(ctx, `INSERT INTO seq_epoch_tbl VALUES (3)`, 100); !errors.Is(err, clock.ErrClockUnavailable) {
		t.Fatalf("stalled new epoch commit: got %v, want ErrClockUnavailable", err)
	}

	// Unstall epoch 3: sequencer must still be unanchored and re-anchor cleanly.
	waitsBeforeFix, _, _, _ := clk.stats()
	clk.setEpoch(3, false)
	if _, err := db.exec(ctx, `INSERT INTO seq_epoch_tbl VALUES (3)`, 100); err != nil {
		t.Fatalf("INSERT 3 after unstalling epoch 3: %v", err)
	}
	waitsAfterFix, targetFix, waitEpochFix, earliestFix := clk.stats()
	if waitsAfterFix <= waitsBeforeFix {
		t.Fatalf("expected WaitUntilAfter retry after failed epoch re-anchor: before=%d after=%d", waitsBeforeFix, waitsAfterFix)
	}
	if waitEpochFix != epoch3 {
		t.Fatalf("retry WaitUntilAfter epoch = %v, want %v", waitEpochFix, epoch3)
	}
	if earliestFix <= targetFix {
		t.Fatalf("retry earliest %d not strictly past target %d", earliestFix, targetFix)
	}

	out, err := db.exec(ctx, `SELECT count(*) FROM seq_epoch_tbl`, 100)
	if err != nil {
		t.Fatalf("SELECT count: %v", err)
	}
	if len(out.Rows) != 1 || out.Rows[0][0].IntVal != 3 {
		t.Fatalf("final row count = %v, want 3", out.Rows)
	}
}
