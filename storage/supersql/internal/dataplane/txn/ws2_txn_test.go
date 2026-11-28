package txn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// TestWS2ConcurrentCommitsAndSnapshotsNoSpuriousIndeterminate verifies that
// closing the unlocked pendingInFlight gap in Manager.submit eliminates spurious
// ErrCommitIndeterminate errors on concurrent openTransactionSnapshot calls.
func TestWS2ConcurrentCommitsAndSnapshotsNoSpuriousIndeterminate(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	fixture.manager.mu.Lock()
	fixture.manager.maximum = 64
	fixture.manager.maxTransactions = 64
	fixture.manager.mu.Unlock()
	fixture.manager.locks.mu.Lock()
	fixture.manager.locks.maxOwners = 64
	fixture.manager.locks.mu.Unlock()

	options := contractBalanceOptions(t, fixture)
	const commitCount = 60
	const readerWorkers = 8

	var stopReaders atomic.Bool
	var readerOps atomic.Int64
	errCh := make(chan error, readerWorkers+1)
	var wg sync.WaitGroup

	for w := range readerWorkers {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for !stopReaders.Load() {
				tx, err := fixture.manager.Begin(t.Context(), TransactionOptions{Mode: ReadOnly})
				if err != nil {
					errCh <- fmt.Errorf("reader %d begin: %w", worker, err)
					return
				}
				row, err := tx.Get(t.Context(), fixture.request)
				closeErr := tx.Close(t.Context())
				if err != nil {
					errCh <- fmt.Errorf("reader %d get: %w", worker, err)
					return
				}
				if closeErr != nil {
					errCh <- fmt.Errorf("reader %d close: %w", worker, closeErr)
					return
				}
				if len(row.Values) != 1 || row.Values[0].IsNull {
					errCh <- fmt.Errorf("reader %d invalid row: %+v", worker, row.Values)
					return
				}
				readerOps.Add(1)
			}
		}(w)
	}

	for i := 1; i <= commitCount; i++ {
		writer, err := fixture.manager.Begin(t.Context(), options)
		if err != nil {
			stopReaders.Store(true)
			wg.Wait()
			t.Fatalf("commit %d begin: %v", i, err)
		}
		mut := storage.NewUpdateMutation(
			fixture.request.Target,
			fixture.request.Key,
			fixture.request.ColumnIDs,
			[]model.Value{model.Int64Val(int64(100 + i))},
		)
		if err := writer.ApplyMutations(t.Context(), []storage.Mutation{mut}); err != nil {
			_ = writer.Close(t.Context())
			stopReaders.Store(true)
			wg.Wait()
			t.Fatalf("commit %d apply: %v", i, err)
		}
		if _, err := writer.Commit(t.Context()); err != nil {
			_ = writer.Close(t.Context())
			stopReaders.Store(true)
			wg.Wait()
			t.Fatalf("commit %d commit: %v", i, err)
		}
	}

	stopReaders.Store(true)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	if readerOps.Load() == 0 {
		t.Fatal("expected concurrent snapshot reads to execute during commits")
	}
	ro := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, ro, fixture.request, int64(100+commitCount))
}

// TestWS2ConcurrentRollbackAndStatementRollbackNeverBricksManager verifies that
// racing Transaction.Rollback with a failing Statement's rollbackStatement does
// not poison manager.transactionCloseError or wedge Manager.Begin.
func TestWS2ConcurrentRollbackAndStatementRollbackNeverBricksManager(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	failure := errors.New("statement failure")

	for i := range 100 {
		tx, err := fixture.manager.Begin(t.Context(), options)
		if err != nil {
			t.Fatalf("iteration %d: Manager.Begin bricked: %v", i, err)
		}
		entered := make(chan struct{})
		stmtDone := make(chan error, 1)
		rbDone := make(chan error, 1)

		go func() {
			stmtDone <- tx.Statement(t.Context(), func(ctx context.Context) error {
				mut := storage.NewUpsertMutation(
					fixture.request.Target,
					contractKey(int64(10+i)),
					fixture.request.ColumnIDs,
					[]model.Value{model.Int64Val(int64(i))},
				)
				if err := tx.ApplyMutations(ctx, []storage.Mutation{mut}); err != nil {
					close(entered)
					return err
				}
				close(entered)
				return failure
			})
		}()

		<-entered
		go func() {
			rbDone <- tx.Rollback(t.Context())
		}()

		stmtErr := <-stmtDone
		rbErr := <-rbDone
		if stmtErr == nil {
			t.Fatalf("iteration %d: failing statement returned nil error", i)
		}
		if rbErr != nil {
			t.Fatalf("iteration %d: concurrent Rollback returned error: %v", i, rbErr)
		}
		if tx.State() != Aborted {
			t.Fatalf("iteration %d: state = %v, want Aborted", i, tx.State())
		}
	}

	// Manager must remain healthy and able to begin and commit new transactions.
	tx, err := fixture.manager.Begin(t.Context(), options)
	if err != nil {
		t.Fatalf("post-race Manager.Begin failed: %v", err)
	}
	contractApply(t, tx, statementBalance(fixture, 250))
	contractCommit(t, tx)

	ro := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, ro, fixture.request, 250)
}

// TestWS2Strict2PLSavepointsRetainReadLocksAndRefundMetadata verifies that:
//  1. RollbackTo retains lockShare read locks (blocking concurrent writers to
//     prevent non-repeatable reads and write skew) while releasing lockUpdate
//     write locks.
//  2. A lockUpdate that subsumed lockShare after a savepoint downgrades to
//     lockShare on RollbackTo rather than dropping read protection.
//  3. 1,000+ Savepoint + ReleaseSavepoint / RollbackTo iterations never exhaust
//     transaction.metadataUsed.
func TestWS2Strict2PLSavepointsRetainReadLocksAndRefundMetadata(t *testing.T) {
	t.Run("strict_2pl_prevents_write_skew_and_downgrades_subsumed_share", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			fixture := newCommitFixture(t, nil)
			options := contractBalanceOptions(t, fixture)

			write := func(key, val int64) storage.Mutation {
				return storage.NewUpsertMutation(
					fixture.request.Target,
					contractKey(key),
					fixture.request.ColumnIDs,
					[]model.Value{model.Int64Val(val)},
				)
			}

			// Seed keys 2 and 3.
			seed := contractBegin(t, fixture.manager, options)
			contractApply(t, seed, write(2, 200), write(3, 300))
			contractCommit(t, seed)

			tx1 := contractBegin(t, fixture.manager, options)
			sp, err := tx1.Savepoint(t.Context())
			if err != nil {
				t.Fatal(err)
			}

			// After savepoint:
			// - Read key 2 (lockShare)
			// - Write key 3 (lockUpdate) AND read key 3 (subsumed -> retainedShare)
			// - Write key 4 only (lockUpdate, no read)
			req2 := fixture.request
			req2.Key = contractKey(2)
			contractValues(t, tx1, req2, 200)

			contractApply(t, tx1, write(3, 301))
			req3 := fixture.request
			req3.Key = contractKey(3)
			contractValues(t, tx1, req3, 301)

			contractApply(t, tx1, write(4, 401))

			if err := tx1.RollbackTo(t.Context(), sp); err != nil {
				t.Fatal(err)
			}

			// Key 4 write lock MUST be released: tx2 writes key 4 immediately.
			tx2 := contractBegin(t, fixture.manager, options)
			contractApply(t, tx2, write(4, 402))
			contractCommit(t, tx2)

			// Key 3 write lock was downgraded to lockShare: another reader CAN read key 3 immediately!
			txReader := contractBegin(t, fixture.manager, options)
			contractValues(t, txReader, req3, 300)
			if err := txReader.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}

			// Both key 2 (lockShare) and key 3 (downgraded lockShare) MUST block concurrent writers!
			writer2 := contractBegin(t, fixture.manager, options)
			writer3 := contractBegin(t, fixture.manager, options)
			done2, done3 := make(chan error, 1), make(chan error, 1)
			go func() { done2 <- writer2.ApplyMutations(t.Context(), []storage.Mutation{write(2, 299)}) }()
			go func() { done3 <- writer3.ApplyMutations(t.Context(), []storage.Mutation{write(3, 399)}) }()
			synctest.Wait()

			select {
			case err := <-done2:
				t.Fatalf("Strict 2PL violated: writer on read-locked key 2 proceeded after RollbackTo: %v", err)
			default:
			}
			select {
			case err := <-done3:
				t.Fatalf("Strict 2PL violated: writer on downgraded-share key 3 proceeded after RollbackTo: %v", err)
			default:
			}

			// Re-reading keys 2 and 3 inside tx1 MUST observe repeatable values (200 and 300).
			contractValues(t, tx1, req2, 200)
			contractValues(t, tx1, req3, 300)

			// Commit tx1 -> releases retained read locks, unblocking writer2 and writer3.
			contractCommit(t, tx1)
			if err := <-done2; err != nil {
				t.Fatalf("writer2 after tx1 commit: %v", err)
			}
			if err := <-done3; err != nil {
				t.Fatalf("writer3 after tx1 commit: %v", err)
			}
			contractCommit(t, writer2)
			contractCommit(t, writer3)
		})
	})

	t.Run("savepoint_loop_refunds_metadata_budget", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractBalanceOptions(t, fixture)
		// Tight memory budget so monotonic metadata leak would hit 53200 well before 1,500 iterations.
		options.MemoryBytes = 256 << 10
		tx := contractBegin(t, fixture.manager, options)
		initialMetadata := tx.metadataUsed

		for i := range 1500 {
			sp, err := tx.Savepoint(t.Context())
			if err != nil {
				t.Fatalf("iteration %d Savepoint failed (metadataUsed=%d limit=%d): %v", i, tx.metadataUsed, tx.metadataLimit, err)
			}
			if i%2 == 0 {
				if err := tx.RollbackTo(t.Context(), sp); err != nil {
					t.Fatalf("iteration %d RollbackTo failed: %v", i, err)
				}
			}
			if err := tx.ReleaseSavepoint(t.Context(), sp); err != nil {
				t.Fatalf("iteration %d ReleaseSavepoint failed: %v", i, err)
			}
		}
		if tx.metadataUsed != initialMetadata {
			t.Fatalf("metadataUsed leaked: got %d, want %d", tx.metadataUsed, initialMetadata)
		}
		if err := tx.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestWS2ShardedLockRegistryPerObjectWakeupSubsumptionAndWaitGraph verifies:
//  1. Per-object notification channels only wake waiters blocked on the released object.
//  2. Mode-subsumption self-coverage allows lockUpdate to subsume lockShare without duplicate records.
//  3. Shared locks.WaitGraph detects cross-registry deadlocks (40P01) under -race.
func TestWS2ShardedLockRegistryPerObjectWakeupSubsumptionAndWaitGraph(t *testing.T) {
	t.Run("per_object_wakeup_isolation_and_subsumption", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			registry := testLockRegistry(t, 8, 64, 1<<20)
			holderA := testLockOwner(t, registry, 1)
			holderB := testLockOwner(t, registry, 2)
			waiterA := testLockOwner(t, registry, 3)
			waiterB := testLockOwner(t, registry, 4)

			reqA := lockRequest{
				ObjectID: 10, Generation: 1,
				Lower: lockBound{Key: []byte{1}, Inclusive: true},
				Upper: lockBound{Key: []byte{10}, Inclusive: true},
			}
			reqB := lockRequest{
				ObjectID: 20, Generation: 1,
				Lower: lockBound{Key: []byte{1}, Inclusive: true},
				Upper: lockBound{Key: []byte{10}, Inclusive: true},
			}

			requireLock(t, registry, holderA, reqA, lockUpdate)
			requireLock(t, registry, holderB, reqB, lockUpdate)

			// Self-coverage mode subsumption: lockUpdate on [1..10] subsumes lockShare/lockKeyShare/lockNoKeyUpdate on point 5.
			subPointA := lockRequest{
				ObjectID: 10, Generation: 1,
				Lower: lockBound{Key: []byte{5}, Inclusive: true},
				Upper: lockBound{Key: []byte{5}, Inclusive: true},
			}
			for _, mode := range []lockMode{lockKeyShare, lockShare, lockNoKeyUpdate, lockUpdate} {
				requireLock(t, registry, holderA, subPointA, mode)
			}
			if err := registry.acquireNoWaitFor(t.Context(), holderA, subPointA, lockShare, lockUpdate); err != nil {
				t.Fatalf("acquireNoWaitFor subsumed lock failed: %v", err)
			}
			registry.mu.Lock()
			lockCount := registry.lockCount
			registry.mu.Unlock()
			if lockCount != 2 {
				t.Fatalf("mode-subsumption allocated duplicate lock records: lockCount=%d, want 2", lockCount)
			}

			waitA, _ := waitForLock(t, registry, waiterA, subPointA, lockUpdate)
			waitB, _ := waitForLock(t, registry, waiterB, lockRequest{
				ObjectID: 20, Generation: 1,
				Lower: lockBound{Key: []byte{5}, Inclusive: true},
				Upper: lockBound{Key: []byte{5}, Inclusive: true},
			}, lockUpdate)
			requireLockBlocked(t, waitA)
			requireLockBlocked(t, waitB)

			// Capture channel for object 20 before releasing object 10.
			registry.mu.Lock()
			chObj20 := registry.objects[20].changed
			registry.mu.Unlock()

			// Releasing holderA (object 10) MUST wake waiterA and MUST NOT close object 20's wakeup channel.
			if err := registry.release(holderA); err != nil {
				t.Fatal(err)
			}
			requireLockResult(t, waitA)
			select {
			case <-chObj20:
				t.Fatal("releasing lock on object 10 spuriously woke waiters on object 20")
			default:
			}
			requireLockBlocked(t, waitB)

			if err := registry.release(holderB); err != nil {
				t.Fatal(err)
			}
			requireLockResult(t, waitB)
		})
	})

	t.Run("shared_wait_graph_deadlock_detection_under_race", func(t *testing.T) {
		waitGraph, err := locks.NewWaitGraph(locks.DefaultWaitGraphLimits())
		if err != nil {
			t.Fatal(err)
		}
		fixture := newCommitFixture(t, nil)
		// Open a manager wired with the shared WaitGraph.
		state, err := fixture.backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		mgr, err := NewManager(t.Context(), ManagerConfig{
			Backend:       fixture.backend,
			Clock:         fixture.clock,
			Authority:     fixture.authority,
			MaxOperations: 16,
			WaitGraph:     waitGraph,
			Recovered: RecoveredState{
				LogPosition:  state.LogPosition,
				CommitTS:     state.CommitTS,
				ClosedTS:     state.ClosedTS,
				HistoryFloor: state.HistoryFloor,
				Epoch:        state.Epoch,
				LeapHistory:  state.Scale.LeapHistoryID,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := mgr.Close(); err != nil {
				t.Error(err)
			}
		})

		opts1 := contractBalanceOptions(t, fixture)
		opts1.WaitOwner = locks.WaitOwner(101)
		opts2 := contractBalanceOptions(t, fixture)
		opts2.WaitOwner = locks.WaitOwner(202)

		// tx1 (older, owner 101) holds key 1.
		tx1 := contractBegin(t, mgr, opts1)
		contractApply(t, tx1, statementBalance(fixture, 111))

		// Simulate an external advisory-lock wait edge: owner 101 is waiting on owner 202.
		if err := waitGraph.BeginWait(locks.WaitOwner(101), []locks.WaitOwner{locks.WaitOwner(202)}); err != nil {
			t.Fatal(err)
		}
		defer waitGraph.EndWait(locks.WaitOwner(101))

		// tx2 (younger, owner 202) requests key 1 -> blocked by owner 101 -> cycle 202 -> 101 -> 202 -> 40P01!
		tx2 := contractBegin(t, mgr, opts2)
		err = tx2.ApplyMutations(t.Context(), []storage.Mutation{statementBalance(fixture, 222)})
		if !errors.Is(err, locks.ErrDeadlockDetected) {
			t.Fatalf("expected ErrDeadlockDetected (40P01), got: %v", err)
		}
		requireNormalizerState(t, err, "40P01")

		// Concurrent multi-object contention stress with WaitGraph enabled under -race.
		if err := tx2.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := tx1.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		waitGraph.EndWait(locks.WaitOwner(101))

		var wg sync.WaitGroup
		const workers = 6
		for w := range workers {
			wg.Add(1)
			go func(id int) {
				defer wg.Done()
				o := contractBalanceOptions(t, fixture)
				o.WaitOwner = locks.WaitOwner(1000 + id)
				for iter := range 10 {
					tx, err := mgr.Begin(t.Context(), o)
					if err != nil {
						t.Errorf("worker %d iter %d begin: %v", id, iter, err)
						return
					}
					mut := storage.NewUpsertMutation(
						fixture.request.Target,
						contractKey(int64(50+id)),
						fixture.request.ColumnIDs,
						[]model.Value{model.Int64Val(int64(iter))},
					)
					if err := tx.ApplyMutations(t.Context(), []storage.Mutation{mut}); err != nil {
						_ = tx.Rollback(t.Context())
						t.Errorf("worker %d iter %d apply: %v", id, iter, err)
						return
					}
					if _, err := tx.Commit(t.Context()); err != nil {
						_ = tx.Rollback(t.Context())
						t.Errorf("worker %d iter %d commit: %v", id, iter, err)
						return
					}
				}
			}(w)
		}
		wg.Wait()
	})
}

// TestWS2OverlaySpillsBeyondMemoryQuarterAndReclaimsMemory verifies that:
//  1. A transaction with MemoryBytes=8 MiB and SpillBytes=32 MiB cleanly stages,
//     reads back (via Get and ReadView scan), and commits 6 MiB of row payloads
//     (3x the old 2 MiB MemoryBytes/4 ceiling!).
//  2. Repeatedly overwriting the same key or rolling back savepoints reclaims
//     overlay memory instead of growing monotonically.
func TestWS2OverlaySpillsBeyondMemoryQuarterAndReclaimsMemory(t *testing.T) {
	t.Run("stages_reads_and_commits_6MiB_with_8MiB_memory_budget", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)

		// Create a table with an int64 key (col 1) and a bytes payload (col 2).
		target := storage.ObjectRef{ObjectID: 77, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
		layout := []storage.ColumnLayout{
			{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}},
			{ID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}},
		}
		keyLayout := []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}
		normalizer, err := NewRowNormalizer(t.Context(), target, []ColumnRule{
			{Layout: layout[0], KeyPosition: 1},
			{Layout: layout[1]},
		}, 1<<20)
		if err != nil {
			t.Fatal(err)
		}

		opts := TransactionOptions{
			Mode:           ReadWrite,
			Resolver:       contractResolver{normalizers: []*RowNormalizer{normalizer}},
			KeyEncoder:     commitFixtureEncoder{},
			MemoryBytes:    8 << 20,  // 8 MiB -> old overlay limit was 2 MiB
			SpillBytes:     32 << 20, // 32 MiB spill budget
			SpillDirectory: t.TempDir(),
			MaxViews:       8,
		}
		tx := contractBegin(t, fixture.manager, opts)

		const (
			rowCount    = 384 // 384 * 16 KiB = 6 MiB (> 3x MemoryBytes/4)
			payloadSize = 16 << 10
		)
		makePayload := func(i int) []byte {
			buf := bytes.Repeat([]byte{byte(i%251 + 1)}, payloadSize)
			buf[0] = byte(i >> 8)
			buf[1] = byte(i)
			return buf
		}

		for i := 1; i <= rowCount; i++ {
			mut := storage.NewInsertMutation(
				target,
				contractKey(int64(i)),
				[]model.ColumnID{1, 2},
				[]model.Value{model.Int64Val(int64(i)), model.BytesVal(makePayload(i))},
			)
			if err := tx.ApplyMutations(t.Context(), []storage.Mutation{mut}); err != nil {
				t.Fatalf("row %d (total ~%d KiB) failed: %v", i, i*16, err)
			}
		}

		// Verify read-your-own-writes for early (inline), middle (spilled), and late (spilled) rows via Get.
		for _, idx := range []int{1, 50, 200, rowCount} {
			row, err := tx.Get(t.Context(), storage.ReadRequest{
				Target:    target,
				Key:       contractKey(int64(idx)),
				Layout:    layout,
				ColumnIDs: []model.ColumnID{1, 2},
			})
			if err != nil {
				t.Fatalf("tx.Get row %d: %v", idx, err)
			}
			want := makePayload(idx)
			if len(row.Values) != 2 || row.Values[0].IntVal != int64(idx) || !bytes.Equal(row.Values[1].BytesVal, want) {
				t.Fatalf("tx.Get row %d mismatch: id=%d len=%d", idx, row.Values[0].IntVal, len(row.Values[1].BytesVal))
			}
		}

		// Verify full ReadView scan over all 384 inline + spilled rows.
		span := storage.KeySpan{
			Lower: storage.KeyBound{Values: []model.Value{model.Int64Val(1)}, Inclusive: true},
			Upper: storage.KeyBound{Values: []model.Value{model.Int64Val(rowCount)}, Inclusive: true},
		}
		view, err := tx.ReadView(t.Context(), []ReadDependency{{Target: target, KeyLayout: keyLayout, Spans: []storage.KeySpan{span}}})
		if err != nil {
			t.Fatalf("tx.ReadView: %v", err)
		}
		plan, err := view.PlanScan(t.Context(), storage.PushdownRequest{
			Target:    target,
			Layout:    layout,
			KeyLayout: keyLayout,
			ColumnIDs: []model.ColumnID{1, 2},
			Spans:     []storage.KeySpan{span},
		})
		if err != nil {
			t.Fatalf("view.PlanScan: %v", err)
		}
		cursor, err := view.VectorizedScan(t.Context(), plan)
		if err != nil {
			t.Fatalf("view.VectorizedScan: %v", err)
		}
		batch := columnar.Batch{MaxRows: 16}
		seen := 0
		for {
			err := cursor.Next(t.Context(), &batch)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("cursor.Next: %v", err)
			}
			seen += batch.Rows()
		}
		if err := cursor.Close(); err != nil {
			t.Fatal(err)
		}
		if err := view.Close(); err != nil {
			t.Fatal(err)
		}
		if seen != rowCount {
			t.Fatalf("ReadView scan saw %d rows, want %d", seen, rowCount)
		}

		// Commit all 6 MiB and verify durable read from a fresh ReadOnly transaction.
		contractCommit(t, tx)

		ro := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		for _, idx := range []int{1, 192, rowCount} {
			row, err := ro.Get(t.Context(), storage.ReadRequest{
				Target:    target,
				Key:       contractKey(int64(idx)),
				Layout:    layout,
				ColumnIDs: []model.ColumnID{1, 2},
			})
			if err != nil {
				t.Fatalf("post-commit Get row %d: %v", idx, err)
			}
			if !bytes.Equal(row.Values[1].BytesVal, makePayload(idx)) {
				t.Fatalf("post-commit row %d payload mismatch", idx)
			}
		}
	})

	t.Run("overwrites_and_savepoint_rollbacks_reclaim_overlay_memory", func(t *testing.T) {
		overlay, err := NewOverlay(8192)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = overlay.Close() })

		target := storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
		// Overwrite the same key 500 times with 1 KiB payloads (500 KiB total >> 8 KiB budget).
		for i := uint64(1); i <= 500; i++ {
			m := storage.NewUpsertMutation(target, []byte("same-key"), []model.ColumnID{1}, []model.Value{model.BytesVal(bytes.Repeat([]byte{byte(i)}, 1024))})
			m.Ordinal = i
			if err := overlay.StageOwned(t.Context(), []storage.Mutation{m}); err != nil {
				t.Fatalf("overwrite %d exhausted 8 KiB overlay budget: %v", i, err)
			}
		}

		// Repeated checkpoint -> stage 3 KiB -> restore 200 times (600 KiB total >> 8 KiB budget).
		cp, err := overlay.checkpoint()
		if err != nil {
			t.Fatal(err)
		}
		for i := range 200 {
			m := storage.NewUpsertMutation(target, []byte(fmt.Sprintf("temp-%d", i)), []model.ColumnID{1}, []model.Value{model.BytesVal(bytes.Repeat([]byte{'x'}, 3072))})
			m.Ordinal = 501 + uint64(i)
			if err := overlay.StageOwned(t.Context(), []storage.Mutation{m}); err != nil {
				t.Fatalf("savepoint cycle %d stage failed: %v", i, err)
			}
			if err := overlay.restore(cp); err != nil {
				t.Fatalf("savepoint cycle %d restore failed: %v", i, err)
			}
		}
	})

	t.Run("savepoint_rollback_with_spilled_entries", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		target := storage.ObjectRef{ObjectID: 78, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
		layout := []storage.ColumnLayout{
			{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}},
			{ID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}},
		}
		normalizer, err := NewRowNormalizer(t.Context(), target, []ColumnRule{
			{Layout: layout[0], KeyPosition: 1},
			{Layout: layout[1]},
		}, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		opts := TransactionOptions{
			Mode:           ReadWrite,
			Resolver:       contractResolver{normalizers: []*RowNormalizer{normalizer}},
			KeyEncoder:     commitFixtureEncoder{},
			MemoryBytes:    8 << 20,
			SpillBytes:     32 << 20,
			SpillDirectory: t.TempDir(),
			MaxViews:       8,
		}
		tx := contractBegin(t, fixture.manager, opts)
		payload := bytes.Repeat([]byte{'a'}, 16<<10)

		// Stage 2.5 MiB (spills beyond 2 MiB MemoryBytes/4).
		for i := 1; i <= 160; i++ {
			mut := storage.NewInsertMutation(target, contractKey(int64(i)), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(int64(i)), model.BytesVal(payload)})
			if err := tx.ApplyMutations(t.Context(), []storage.Mutation{mut}); err != nil {
				t.Fatalf("pre-savepoint row %d: %v", i, err)
			}
		}
		sp, err := tx.Savepoint(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		// Stage another 2.5 MiB after savepoint, then roll back.
		for i := 161; i <= 320; i++ {
			mut := storage.NewInsertMutation(target, contractKey(int64(i)), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(int64(i)), model.BytesVal(payload)})
			if err := tx.ApplyMutations(t.Context(), []storage.Mutation{mut}); err != nil {
				t.Fatalf("post-savepoint row %d: %v", i, err)
			}
		}
		if err := tx.RollbackTo(t.Context(), sp); err != nil {
			t.Fatalf("RollbackTo on spilled overlay: %v", err)
		}
		// Pre-savepoint spilled row 160 must still be readable; post-savepoint row 161 must be absent.
		row160, err := tx.Get(t.Context(), storage.ReadRequest{Target: target, Key: contractKey(160), Layout: layout, ColumnIDs: []model.ColumnID{1, 2}})
		if err != nil || row160.Values[0].IntVal != 160 || !bytes.Equal(row160.Values[1].BytesVal, payload) {
			t.Fatalf("pre-savepoint spilled row 160: row=%+v err=%v", row160, err)
		}
		if _, err := tx.Get(t.Context(), storage.ReadRequest{Target: target, Key: contractKey(161), Layout: layout, ColumnIDs: []model.ColumnID{1, 2}}); !errors.Is(err, storage.ErrRowNotFound) {
			t.Fatalf("post-savepoint rolled-back row 161 still visible: %v", err)
		}
		contractCommit(t, tx)
	})
}
