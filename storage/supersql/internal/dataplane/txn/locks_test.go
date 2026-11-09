package txn

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func testLockRegistry(t *testing.T, maxOwners, maxLocks int, maxBytes uint64) *lockRegistry {
	t.Helper()
	registry, err := newLockRegistry(maxOwners, maxLocks, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func testLockOwner(t *testing.T, registry *lockRegistry, id byte) *lockOwner {
	t.Helper()
	owner, err := registry.register(t.Context(), model.TxnID{id})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.release(owner); err != nil {
			t.Error(err)
		}
	})
	return owner
}

func lockPoint(key byte) lockRequest {
	return lockRequest{
		ObjectID: 1, Generation: 1,
		Lower: lockBound{Key: []byte{key}, Inclusive: true},
		Upper: lockBound{Key: []byte{key}, Inclusive: true},
	}
}

func requireLock(t *testing.T, registry *lockRegistry, owner *lockOwner, request lockRequest, mode lockMode) {
	t.Helper()
	acquired, err := registry.acquire(t.Context(), owner, request, mode, lockNoWait)
	if err != nil || !acquired {
		t.Fatalf("acquire mode %d: acquired=%v err=%v", mode, acquired, err)
	}
}

func requireLockCode(t *testing.T, err error, code string) {
	t.Helper()
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.SQLState() != code {
		t.Fatalf("error=%v, want SQLSTATE %s", err, code)
	}
}

type lockResult struct {
	acquired bool
	err      error
}

func waitForLock(t *testing.T, registry *lockRegistry, owner *lockOwner, request lockRequest, mode lockMode) (<-chan lockResult, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(owner.ctx)
	t.Cleanup(cancel)
	result := make(chan lockResult, 1)
	go func() {
		acquired, err := registry.acquire(ctx, owner, request, mode, lockWait)
		result <- lockResult{acquired: acquired, err: err}
	}()
	return result, cancel
}

func requireLockBlocked(t *testing.T, result <-chan lockResult) {
	t.Helper()
	synctest.Wait()
	select {
	case got := <-result:
		t.Fatalf("lock acquired before holder cleanup: %+v", got)
	default:
	}
}

func requireLockResult(t *testing.T, result <-chan lockResult) {
	t.Helper()
	got := <-result
	if !got.acquired || got.err != nil {
		t.Fatalf("acquire after cleanup: %+v", got)
	}
}

func TestLockRangeBlocksInsertUntilRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := testLockRegistry(t, 3, 4, 1<<20)
		reader := testLockOwner(t, registry, 1)
		writer := testLockOwner(t, registry, 2)
		outside := testLockOwner(t, registry, 3)
		emptyRange := lockRequest{
			ObjectID: 1, Generation: 1,
			Lower: lockBound{Key: []byte{10}, Inclusive: true},
			Upper: lockBound{Key: []byte{100}, Inclusive: true},
		}
		requireLock(t, registry, reader, emptyRange, lockShare)
		insert, _ := waitForLock(t, registry, writer, lockPoint(50), lockUpdate)
		requireLockBlocked(t, insert)
		requireLock(t, registry, outside, lockPoint(101), lockUpdate)
		if err := registry.release(reader); err != nil {
			t.Fatal(err)
		}
		requireLockResult(t, insert)
	})
}

func TestLockWoundWaitRetainsOwnershipUntilRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := testLockRegistry(t, 3, 4, 1<<20)
		older := testLockOwner(t, registry, 1)
		younger := testLockOwner(t, registry, 2)
		probe := testLockOwner(t, registry, 3)
		requireLock(t, registry, younger, lockPoint(50), lockUpdate)
		waiting, _ := waitForLock(t, registry, older, lockPoint(50), lockUpdate)
		<-younger.ctx.Done()
		requireLockCode(t, context.Cause(younger.ctx), "40001")
		requireLockBlocked(t, waiting)
		requireLockCode(t, registry.beginCommit(younger), "40001")
		if acquired, err := registry.acquire(t.Context(), probe, lockPoint(50), lockKeyShare, lockSkip); acquired || err != nil {
			t.Fatalf("wounded holder lost lock before cleanup: acquired=%v err=%v", acquired, err)
		}
		if err := registry.release(younger); err != nil {
			t.Fatal(err)
		}
		requireLockResult(t, waiting)
	})
}

func TestCommittingLockOwnerCannotBeWounded(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := testLockRegistry(t, 2, 3, 1<<20)
		older := testLockOwner(t, registry, 1)
		committing := testLockOwner(t, registry, 2)
		requireLock(t, registry, committing, lockPoint(1), lockUpdate)
		if err := registry.beginCommit(committing); err != nil {
			t.Fatal(err)
		}
		waiting, cancel := waitForLock(t, registry, older, lockPoint(1), lockUpdate)
		requireLockBlocked(t, waiting)
		if err := context.Cause(committing.ctx); err != nil {
			t.Fatalf("committing owner wounded: %v", err)
		}
		cancel()
		if got := <-waiting; got.acquired || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("wait cancellation: %+v", got)
		}
		if acquired, err := registry.acquire(t.Context(), committing, lockPoint(2), lockShare, lockNoWait); acquired || !errors.Is(err, errLockOwnerState) {
			t.Fatalf("committing owner acquired another lock: acquired=%v err=%v", acquired, err)
		}
		if err := registry.release(committing); err != nil {
			t.Fatal(err)
		}
		requireLock(t, registry, older, lockPoint(1), lockUpdate)
	})
}

func TestLockRowStrengthMatrix(t *testing.T) {
	conflicts := [5][5]bool{
		{false, false, false, true, true},
		{false, false, true, true, true},
		{false, true, true, true, true},
		{true, true, true, true, true},
		{true, true, true, true, true},
	}
	for held := lockKeyShare; held <= lockSchema; held++ {
		for requested := lockKeyShare; requested <= lockSchema; requested++ {
			t.Run(fmt.Sprintf("held_%d_requested_%d", held, requested), func(t *testing.T) {
				registry := testLockRegistry(t, 2, 3, 1<<20)
				holder := testLockOwner(t, registry, 1)
				waiter := testLockOwner(t, registry, 2)
				requireLock(t, registry, holder, lockPoint(1), held)
				acquired, err := registry.acquire(t.Context(), waiter, lockPoint(1), requested, lockNoWait)
				if conflicts[held][requested] {
					if acquired {
						t.Fatal("conflicting strength acquired")
					}
					requireLockCode(t, err, "55P03")
				} else if !acquired || err != nil {
					t.Fatalf("compatible strength blocked: acquired=%v err=%v", acquired, err)
				}
			})
		}
	}
}

func TestLockNoWaitAndSkipDoNotWoundOrRetainLocks(t *testing.T) {
	for _, policy := range []lockWaitPolicy{lockNoWait, lockSkip} {
		t.Run(fmt.Sprintf("policy_%d", policy), func(t *testing.T) {
			registry := testLockRegistry(t, 2, 1, 1<<20)
			older := testLockOwner(t, registry, 1)
			younger := testLockOwner(t, registry, 2)
			requireLock(t, registry, younger, lockPoint(1), lockUpdate)
			acquired, err := registry.acquire(t.Context(), older, lockPoint(1), lockUpdate, policy)
			if acquired {
				t.Fatal("conflicting nonblocking request acquired")
			}
			if policy == lockNoWait {
				requireLockCode(t, err, "55P03")
			} else if err != nil {
				t.Fatal(err)
			}
			if err := context.Cause(younger.ctx); err != nil {
				t.Fatalf("nonblocking request wounded holder: %v", err)
			}
			if err := registry.release(younger); err != nil {
				t.Fatal(err)
			}
			requireLock(t, registry, older, lockPoint(2), lockUpdate)
		})
	}
}

func TestLockSpanBoundariesAndGenerations(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lower, upper lockBound
		point        byte
		generation   uint64
		object       model.ObjectID
		mode         lockMode
		conflict     bool
	}{
		{"inclusive_lower", lockBound{Key: []byte{10}, Inclusive: true}, lockBound{Key: []byte{100}}, 10, 1, 1, lockShare, true},
		{"exclusive_lower", lockBound{Key: []byte{10}}, lockBound{Key: []byte{100}, Inclusive: true}, 10, 1, 1, lockShare, false},
		{"inclusive_upper", lockBound{Key: []byte{10}}, lockBound{Key: []byte{100}, Inclusive: true}, 100, 1, 1, lockShare, true},
		{"exclusive_upper", lockBound{Key: []byte{10}, Inclusive: true}, lockBound{Key: []byte{100}}, 100, 1, 1, lockShare, false},
		{"gap_interior", lockBound{Key: []byte{10}}, lockBound{Key: []byte{100}}, 50, 1, 1, lockShare, true},
		{"unbounded_lower", lockBound{Unbounded: true}, lockBound{Key: []byte{100}}, 0, 1, 1, lockShare, true},
		{"unbounded_upper", lockBound{Key: []byte{10}}, lockBound{Unbounded: true}, 255, 1, 1, lockShare, true},
		{"table", lockBound{Unbounded: true}, lockBound{Unbounded: true}, 50, 1, 1, lockShare, true},
		{"other_generation", lockBound{Unbounded: true}, lockBound{Unbounded: true}, 50, 2, 1, lockShare, false},
		{"schema_other_generation", lockBound{Key: []byte{10}}, lockBound{Key: []byte{20}}, 50, 2, 1, lockSchema, true},
		{"schema_other_object", lockBound{Unbounded: true}, lockBound{Unbounded: true}, 50, 1, 2, lockSchema, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := testLockRegistry(t, 2, 2, 1<<20)
			holder := testLockOwner(t, registry, 1)
			waiter := testLockOwner(t, registry, 2)
			requireLock(t, registry, holder, lockRequest{ObjectID: 1, Generation: 1, Lower: tc.lower, Upper: tc.upper}, tc.mode)
			point := lockPoint(tc.point)
			point.Generation, point.ObjectID = tc.generation, tc.object
			acquired, err := registry.acquire(t.Context(), waiter, point, lockUpdate, lockSkip)
			if err != nil || acquired == tc.conflict {
				t.Fatalf("acquired=%v err=%v, want conflict=%v", acquired, err, tc.conflict)
			}
		})
	}
}

func TestLockSavepointRestoresEarlierStrength(t *testing.T) {
	registry := testLockRegistry(t, 2, 4, 1<<20)
	owner := testLockOwner(t, registry, 1)
	probe := testLockOwner(t, registry, 2)
	requireLock(t, registry, owner, lockPoint(1), lockKeyShare)
	position, err := registry.savepoint(owner)
	if err != nil {
		t.Fatal(err)
	}
	requireLock(t, registry, owner, lockPoint(1), lockUpdate)
	requireLock(t, registry, owner, lockPoint(2), lockUpdate)
	if acquired, err := registry.acquire(t.Context(), probe, lockPoint(1), lockNoKeyUpdate, lockSkip); acquired || err != nil {
		t.Fatalf("upgraded strength not held: acquired=%v err=%v", acquired, err)
	}
	if err := registry.rollback(owner, position); err != nil {
		t.Fatal(err)
	}
	requireLock(t, registry, probe, lockPoint(1), lockNoKeyUpdate)
	requireLock(t, registry, probe, lockPoint(2), lockUpdate)
	if acquired, err := registry.acquire(t.Context(), probe, lockPoint(1), lockUpdate, lockSkip); acquired || err != nil {
		t.Fatalf("rollback removed original key-share: acquired=%v err=%v", acquired, err)
	}
}

func TestLockCapacityIsReclaimedAfterRollbackAndRelease(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count int
		bytes uint64
	}{
		{"count", 1, 1 << 20},
		{"bytes", 10, uint64(unsafe.Sizeof(lockRecord{})) + 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := testLockRegistry(t, 1, tc.count, tc.bytes)
			owner := testLockOwner(t, registry, 1)
			position, err := registry.savepoint(owner)
			if err != nil {
				t.Fatal(err)
			}
			requireLock(t, registry, owner, lockPoint(1), lockUpdate)
			if acquired, err := registry.acquire(t.Context(), owner, lockPoint(2), lockUpdate, lockWait); acquired || !errors.Is(err, errLockCapacity) {
				t.Fatalf("capacity: acquired=%v err=%v", acquired, err)
			}
			if err := registry.rollback(owner, position); err != nil {
				t.Fatal(err)
			}
			requireLock(t, registry, owner, lockPoint(2), lockUpdate)
			if _, err := registry.register(t.Context(), model.TxnID{2}); !errors.Is(err, errLockOwnerCapacity) {
				t.Fatalf("owner limit: %v", err)
			}
			if err := registry.release(owner); err != nil {
				t.Fatal(err)
			}
			replacement := testLockOwner(t, registry, 1)
			requireLock(t, registry, replacement, lockPoint(2), lockUpdate)
			if err := registry.release(owner); err != nil {
				t.Fatal(err)
			}
			if acquired, err := registry.acquire(t.Context(), replacement, lockPoint(3), lockShare, lockNoWait); acquired || !errors.Is(err, errLockCapacity) {
				t.Fatalf("releasing stale owner removed replacement locks: acquired=%v err=%v", acquired, err)
			}
		})
	}
}

func TestPendingLockCapacityAndOwnerCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := testLockRegistry(t, 3, 2, 1<<20)
		holder := testLockOwner(t, registry, 1)
		waiter := testLockOwner(t, registry, 2)
		probe := testLockOwner(t, registry, 3)
		requireLock(t, registry, holder, lockPoint(1), lockUpdate)
		waiting, cancel := waitForLock(t, registry, waiter, lockPoint(1), lockUpdate)
		requireLockBlocked(t, waiting)
		if err := registry.beginCommit(waiter); !errors.Is(err, errLockOwnerBusy) {
			t.Fatalf("commit while waiting: %v", err)
		}
		if _, err := registry.savepoint(waiter); !errors.Is(err, errLockOwnerBusy) {
			t.Fatalf("savepoint while waiting: %v", err)
		}
		if acquired, err := registry.acquire(t.Context(), probe, lockPoint(2), lockUpdate, lockNoWait); acquired || !errors.Is(err, errLockCapacity) {
			t.Fatalf("pending lock escaped capacity: acquired=%v err=%v", acquired, err)
		}
		cancel()
		if got := <-waiting; got.acquired || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled waiter: %+v", got)
		}
		requireLock(t, registry, probe, lockPoint(2), lockUpdate)
		if acquired, err := registry.acquire(t.Context(), waiter, lockPoint(1), lockUpdate, lockSkip); acquired || err != nil {
			t.Fatalf("wait cancellation removed holder lock: acquired=%v err=%v", acquired, err)
		}
	})
}

func TestLockOwnerCancellationRequiresRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := testLockRegistry(t, 2, 3, 1<<20)
		ctx, cancel := context.WithCancelCause(t.Context())
		t.Cleanup(func() { cancel(context.Canceled) })
		holder, err := registry.register(ctx, model.TxnID{1})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := registry.release(holder); err != nil {
				t.Error(err)
			}
		})
		waiter := testLockOwner(t, registry, 2)
		requireLock(t, registry, holder, lockPoint(1), lockUpdate)
		waiting, _ := waitForLock(t, registry, waiter, lockPoint(1), lockUpdate)
		requireLockBlocked(t, waiting)
		cause := errors.New("transaction canceled")
		cancel(cause)
		requireLockBlocked(t, waiting)
		if err := registry.beginCommit(holder); !errors.Is(err, cause) {
			t.Fatalf("lost transaction cancellation cause: %v", err)
		}
		if err := registry.release(holder); err != nil {
			t.Fatal(err)
		}
		requireLockResult(t, waiting)
	})
}

func TestLockReleaseWakesOwnPendingAcquisition(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := testLockRegistry(t, 2, 2, 1<<20)
		holder := testLockOwner(t, registry, 1)
		waiter := testLockOwner(t, registry, 2)
		requireLock(t, registry, holder, lockPoint(1), lockUpdate)
		waiting, _ := waitForLock(t, registry, waiter, lockPoint(1), lockUpdate)
		requireLockBlocked(t, waiting)
		if err := registry.release(waiter); err != nil {
			t.Fatal(err)
		}
		if got := <-waiting; got.acquired || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("released waiter remained blocked: %+v", got)
		}
		replacement := testLockOwner(t, registry, 2)
		requireLock(t, registry, replacement, lockPoint(2), lockUpdate)
	})
}

func TestLockRequestsOwnRetainedKeys(t *testing.T) {
	registry := testLockRegistry(t, 2, 2, 1<<20)
	holder := testLockOwner(t, registry, 1)
	probe := testLockOwner(t, registry, 2)
	request := lockPoint(50)
	requireLock(t, registry, holder, request, lockUpdate)
	request.Lower.Key[0], request.Upper.Key[0] = 100, 100
	if acquired, err := registry.acquire(t.Context(), probe, lockPoint(50), lockShare, lockSkip); acquired || err != nil {
		t.Fatalf("caller mutation changed retained lock: acquired=%v err=%v", acquired, err)
	}
	requireLock(t, registry, probe, lockPoint(100), lockUpdate)
}

func TestLockRejectsInvalidRequestsAndOwners(t *testing.T) {
	registry := testLockRegistry(t, 2, 2, 1<<20)
	other := testLockRegistry(t, 1, 1, 1<<20)
	owner := testLockOwner(t, registry, 1)
	foreign := testLockOwner(t, other, 1)
	if _, err := registry.register(t.Context(), model.TxnID{1}); !errors.Is(err, errLockOwner) {
		t.Fatalf("duplicate transaction ID: %v", err)
	}
	for _, invalid := range []lockRequest{
		{ObjectID: 1, Generation: 1, Lower: lockBound{Key: []byte{2}}, Upper: lockBound{Key: []byte{1}}},
		{ObjectID: 1, Generation: 1, Lower: lockBound{Key: []byte{1}}, Upper: lockBound{Key: []byte{1}}},
		{ObjectID: 1, Generation: 1, Lower: lockBound{Unbounded: true, Key: []byte{1}}, Upper: lockBound{Unbounded: true}},
		{ObjectID: 1, Generation: 1, Lower: lockBound{Unbounded: true}, Upper: lockBound{Unbounded: true, Inclusive: true}},
	} {
		if acquired, err := registry.acquire(t.Context(), owner, invalid, lockShare, lockWait); acquired || !errors.Is(err, errLockRequest) {
			t.Fatalf("invalid interval: acquired=%v err=%v", acquired, err)
		}
	}
	if acquired, err := registry.acquire(t.Context(), foreign, lockPoint(1), lockUpdate, lockWait); acquired || !errors.Is(err, errLockOwner) {
		t.Fatalf("foreign owner: acquired=%v err=%v", acquired, err)
	}
	if err := registry.release(foreign); !errors.Is(err, errLockOwner) {
		t.Fatalf("foreign release: %v", err)
	}
	if err := registry.rollback(owner, lockPosition{owner: foreign}); !errors.Is(err, errLockPosition) {
		t.Fatalf("foreign savepoint: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := registry.register(ctx, model.TxnID{2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled registration: %v", err)
	}
	if acquired, err := registry.acquire(ctx, owner, lockPoint(1), lockUpdate, lockWait); acquired || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquisition: acquired=%v err=%v", acquired, err)
	}
	requireLock(t, registry, owner, lockPoint(1), lockUpdate)
}

func TestLockRejectsZeroIdentities(t *testing.T) {
	registry := testLockRegistry(t, 1, 1, 1<<20)
	if owner, err := registry.register(t.Context(), model.TxnID{}); owner != nil || !errors.Is(err, errLockOwner) {
		t.Fatalf("zero transaction ID: owner=%v err=%v", owner, err)
	}
	owner := testLockOwner(t, registry, 1)
	for _, mode := range []lockMode{lockShare, lockSchema} {
		for _, target := range []struct {
			object     model.ObjectID
			generation uint64
		}{{0, 1}, {1, 0}} {
			request := lockPoint(1)
			request.ObjectID, request.Generation = target.object, target.generation
			acquired, err := registry.acquire(t.Context(), owner, request, mode, lockWait)
			if acquired || !errors.Is(err, errLockRequest) {
				t.Fatalf("mode=%d object=%d generation=%d: acquired=%v err=%v", mode, target.object, target.generation, acquired, err)
			}
		}
	}
	requireLock(t, registry, owner, lockPoint(1), lockSchema)
}

func TestLockTimeoutStartsAtConflictAndPreservesWoundCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		registry := testLockRegistry(t, 3, 4, 1<<20)
		older := testLockOwner(t, registry, 1)
		younger := testLockOwner(t, registry, 2)
		probe := testLockOwner(t, registry, 3)
		requireLock(t, registry, younger, lockPoint(1), lockUpdate)

		waitContext := WithLockTimeout(t.Context(), time.Hour)
		beforeWait, cancelBeforeWait := context.WithTimeout(t.Context(), 2*time.Hour)
		<-beforeWait.Done()
		cancelBeforeWait()

		result := make(chan lockResult, 1)
		go func() {
			acquired, err := registry.acquire(waitContext, older, lockPoint(1), lockUpdate, lockWait)
			result <- lockResult{acquired: acquired, err: err}
		}()
		<-younger.ctx.Done()
		requireLockCode(t, context.Cause(younger.ctx), "40001")
		requireLockBlocked(t, result)
		got := <-result
		if got.acquired || !errors.Is(got.err, ErrLockTimeout) || !errors.Is(got.err, context.DeadlineExceeded) {
			t.Fatalf("lock timeout: %+v", got)
		}
		requireLockCode(t, got.err, "55P03")
		registry.mu.Lock()
		pending, lockCount := older.pending, registry.lockCount
		registry.mu.Unlock()
		if pending != 0 || lockCount != 1 {
			t.Fatalf("timed-out admission retained state: pending=%d locks=%d", pending, lockCount)
		}
		if acquired, err := registry.acquire(t.Context(), probe, lockPoint(1), lockKeyShare, lockSkip); acquired || err != nil {
			t.Fatalf("timeout bypassed wounded holder cleanup: acquired=%v err=%v", acquired, err)
		}
		if err := registry.release(younger); err != nil {
			t.Fatal(err)
		}
		requireLock(t, registry, probe, lockPoint(1), lockUpdate)
		synctest.Wait()
	})
}

func TestEligibleRangeSplitIsAtomicAndBounded(t *testing.T) {
	registry := testLockRegistry(t, 3, 8, 1<<20)
	holder := testLockOwner(t, registry, 1)
	reader := testLockOwner(t, registry, 2)
	requireLock(t, registry, holder, lockPoint('m'), lockUpdate)
	full := lockRequest{ObjectID: 1, Generation: 1, Lower: lockBound{Unbounded: true}, Upper: lockBound{Unbounded: true}}
	if parts, _, err := registry.acquireEligible(t.Context(), reader, full, lockShare, lockUpdate, 1); parts != nil || err == nil {
		t.Fatalf("bounded split: parts=%v err=%v", parts, err)
	} else {
		requireLockCode(t, err, "53200")
	}
	if reader.locks != nil {
		t.Fatal("failed bounded split published a partial lock set")
	}
	parts, _, err := registry.acquireEligible(t.Context(), reader, full, lockShare, lockUpdate, 2)
	if err != nil || len(parts) != 2 {
		t.Fatalf("eligible split: parts=%v err=%v", parts, err)
	}
	if err := registry.release(holder); err != nil {
		t.Fatal(err)
	}
	probe := testLockOwner(t, registry, 3)
	if acquired, err := registry.acquire(t.Context(), probe, lockPoint('m'), lockUpdate, lockNoWait); !acquired || err != nil {
		t.Fatalf("excluded point remained protected: acquired=%v err=%v", acquired, err)
	}
	if acquired, err := registry.acquire(t.Context(), probe, lockPoint('a'), lockUpdate, lockNoWait); acquired || err == nil {
		t.Fatalf("eligible lower gap lacked protection: acquired=%v err=%v", acquired, err)
	}
}
