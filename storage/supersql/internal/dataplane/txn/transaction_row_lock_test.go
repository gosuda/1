package txn

import (
	"fmt"
	"io"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestManagedRowLockStrengths(t *testing.T) {
	conflicts := [4][4]bool{
		{false, false, false, true},
		{false, false, true, true},
		{false, true, true, true},
		{true, true, true, true},
	}
	for held := KeyShareLock; held <= UpdateLock; held++ {
		for requested := KeyShareLock; requested <= UpdateLock; requested++ {
			t.Run(fmt.Sprintf("held_%d_requested_%d", held, requested), func(t *testing.T) {
				fixture := newCommitFixture(t, nil)
				options := contractBalanceOptions(t, fixture)
				contender := contractBegin(t, fixture.manager, options)
				holder := contractBegin(t, fixture.manager, options)
				acquired, err := holder.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, held, WaitForRowLock)
				if !acquired || err != nil {
					t.Fatalf("initial row lock: %v %v", acquired, err)
				}
				acquired, err = contender.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, requested, NoWaitForRowLock)
				if conflicts[held][requested] {
					if acquired {
						t.Fatal("conflicting row lock acquired")
					}
					requireNormalizerState(t, err, "55P03")
				} else if !acquired || err != nil {
					t.Fatalf("compatible row lock blocked: %v %v", acquired, err)
				}
				if holder.State() != Active {
					t.Fatalf("NOWAIT wounded holder: %v", holder.State())
				}
			})
		}
	}
}

func TestManagedSkipLockedDoesNotReserveCandidate(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	contender := contractBegin(t, fixture.manager, options)
	holder := contractBegin(t, fixture.manager, options)
	if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, WaitForRowLock); !acquired || err != nil {
		t.Fatalf("initial lock: %v %v", acquired, err)
	}
	if acquired, err := contender.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, SkipLockedRow); acquired || err != nil {
		t.Fatalf("locked candidate was not skipped: %v %v", acquired, err)
	}
	if holder.State() != Active {
		t.Fatalf("SKIP LOCKED wounded holder: %v", holder.State())
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	probe := contractBegin(t, fixture.manager, options)
	if acquired, err := probe.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, NoWaitForRowLock); !acquired || err != nil {
		t.Fatalf("skipping contender retained a conflicting lock: %v %v", acquired, err)
	}
	if err := probe.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if acquired, err := contender.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, NoWaitForRowLock); !acquired || err != nil {
		t.Fatalf("released candidate stayed locked: %v %v", acquired, err)
	}
	readonly := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	_, err := readonly.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, KeyShareLock, SkipLockedRow)
	requireNormalizerState(t, err, "25006")
}

func TestSkipLockedLockSetIsAtomicAndDoesNotWoundHolder(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	contender := contractBegin(t, fixture.manager, options)
	holder := contractBegin(t, fixture.manager, options)
	schemaKey, relationKey := contractKey(1), contractKey(2)
	if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, relationKey, UpdateLock, WaitForRowLock); !acquired || err != nil {
		t.Fatalf("relation holder: acquired=%v err=%v", acquired, err)
	}
	requests := []RowLockRequest{
		{Target: fixture.request.Target, Key: schemaKey, Strength: UpdateLock},
		{Target: fixture.request.Target, Key: relationKey, Strength: UpdateLock},
	}
	if acquired, err := contender.LockRows(t.Context(), requests, SkipLockedRow); acquired || err != nil {
		t.Fatalf("schema/relation pair was not skipped atomically: acquired=%v err=%v", acquired, err)
	}
	if holder.State() != Active {
		t.Fatalf("SKIP LOCKED wounded relation holder: %v", holder.State())
	}
	probe := contractBegin(t, fixture.manager, options)
	if acquired, err := probe.LockRow(t.Context(), fixture.request.Target, schemaKey, UpdateLock, NoWaitForRowLock); !acquired || err != nil {
		t.Fatalf("skipped pair retained schema lock: acquired=%v err=%v", acquired, err)
	}
}

func TestReadViewSkipLockedRollsBackEarlierReadDependency(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	contender := contractBegin(t, fixture.manager, options)
	holder := contractBegin(t, fixture.manager, options)
	if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, contractKey(2), UpdateLock, WaitForRowLock); !acquired || err != nil {
		t.Fatalf("relation holder: acquired=%v err=%v", acquired, err)
	}
	point := func(value int64) storage.KeySpan {
		bound := storage.KeyBound{Values: []model.Value{model.Int64Val(value)}, Inclusive: true}
		return storage.KeySpan{Lower: bound, Upper: bound}
	}
	dependencies := []ReadDependency{{Target: fixture.request.Target, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{point(1), point(2)}}}
	view, acquired, err := contender.ReadViewSkipLocked(t.Context(), dependencies)
	if err != nil || acquired || view != nil {
		t.Fatalf("locked read view was not skipped: view=%v acquired=%v err=%v", view, acquired, err)
	}
	probe := contractBegin(t, fixture.manager, options)
	if acquired, err := probe.LockRow(t.Context(), fixture.request.Target, contractKey(1), UpdateLock, NoWaitForRowLock); !acquired || err != nil {
		t.Fatalf("skipped read view retained earlier dependency: acquired=%v err=%v", acquired, err)
	}
	if holder.State() != Active {
		t.Fatalf("skipped read view wounded holder: %v", holder.State())
	}
}

func TestRowLockReadViewLocksEligibleSubspansBeforeSnapshotScan(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	seed := contractBegin(t, fixture.manager, options)
	second := storage.NewUpsertMutation(fixture.request.Target, contractKey(2), fixture.request.ColumnIDs, []model.Value{model.Int64Val(200)})
	third := storage.NewUpsertMutation(fixture.request.Target, contractKey(3), fixture.request.ColumnIDs, []model.Value{model.Int64Val(300)})
	contractApply(t, seed, second, third)
	if _, err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	holder := contractBegin(t, fixture.manager, options)
	holderWrite := storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(190)})
	contractApply(t, holder, holderWrite)
	if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, WaitForRowLock); !acquired || err != nil {
		t.Fatalf("holder lock: acquired=%v err=%v", acquired, err)
	}
	point := func(value int64) storage.KeySpan {
		bound := storage.KeyBound{Values: []model.Value{model.Int64Val(value)}, Inclusive: true}
		return storage.KeySpan{Lower: bound, Upper: bound}
	}
	all := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
	dependencies := []ReadDependency{
		{Target: fixture.request.Target, Occurrence: 1, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{all}},
		{Target: fixture.request.Target, Occurrence: 1, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{point(4)}},
	}
	nowait := contractBegin(t, fixture.manager, options)
	if view, err := nowait.ReadViewForRowLocks(t.Context(), dependencies, []RowLockPolicy{{Occurrence: 1, Strength: UpdateLock, Wait: NoWaitForRowLock}}); view != nil || err == nil {
		t.Fatalf("NOWAIT read view crossed held row: view=%v err=%v", view, err)
	}
	if holder.State() != Active {
		t.Fatalf("NOWAIT read view wounded holder: %v", holder.State())
	}
	if err := nowait.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	contender := contractBegin(t, fixture.manager, options)
	ownWrite := storage.NewUpdateMutation(fixture.request.Target, contractKey(2), fixture.request.ColumnIDs, []model.Value{model.Int64Val(290)})
	contractApply(t, contender, ownWrite)
	view, err := contender.ReadViewForRowLocks(t.Context(), dependencies, []RowLockPolicy{{Occurrence: 1, Strength: UpdateLock, Wait: SkipLockedRow}})
	if err != nil {
		t.Fatal(err)
	}
	request := storage.PushdownRequest{
		Target: fixture.request.Target, Occurrence: 1, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(),
		ColumnIDs: fixture.request.ColumnIDs, Spans: []storage.KeySpan{all},
	}
	plan, err := view.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := view.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	var batch columnar.Batch
	batch.MaxRows = 8
	var accepted [2]int64
	count := 0
	for {
		err := cursor.Next(t.Context(), &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := range batch.Rows() {
			lane := batch.Lane(row)
			value := batch.Columns[0].Int64[batch.Columns[0].ValueIndex(lane)]
			var key int64
			switch value {
			case 290:
				key = 2
			case 300:
				key = 3
			case 100:
				t.Fatal("blocked row was read before downstream row-lock admission")
			default:
				t.Fatalf("unexpected snapshot value %d", value)
			}
			acquired, err := contender.LockRow(t.Context(), fixture.request.Target, contractKey(key), UpdateLock, SkipLockedRow)
			if err != nil {
				t.Fatal(err)
			}
			if acquired {
				if count == len(accepted) {
					t.Fatalf("admitted too many rows: %v and %d", accepted, value)
				}
				accepted[count] = value
				count++
			}
		}
	}
	if count != 2 || accepted != [2]int64{290, 300} {
		t.Fatalf("SKIP LOCKED admitted rows = %v, want own row 290 and snapshot row 300", accepted[:count])
	}
	if holder.State() != Active {
		t.Fatalf("SKIP LOCKED read view wounded holder: %v", holder.State())
	}
	if _, err := holder.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	probe := contractBegin(t, fixture.manager, options)
	if acquired, err := probe.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, NoWaitForRowLock); !acquired || err != nil {
		t.Fatalf("skipped point retained a lock: acquired=%v err=%v", acquired, err)
	}
	for _, key := range []int64{0, 4} {
		if acquired, err := probe.LockRow(t.Context(), fixture.request.Target, contractKey(key), UpdateLock, NoWaitForRowLock); acquired || err == nil {
			t.Fatalf("eligible empty gap %d lost predicate protection: acquired=%v err=%v", key, acquired, err)
		}
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := probe.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := contender.Commit(t.Context()); err != nil {
		t.Fatalf("strict-PCC SKIP LOCKED transaction commit: %v", err)
	}
}

func TestRowLockReadViewScopesSkipAdmissionByScanOccurrence(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	seed := contractBegin(t, fixture.manager, options)
	contractApply(t, seed, storage.NewUpsertMutation(fixture.request.Target, contractKey(2), fixture.request.ColumnIDs, []model.Value{model.Int64Val(200)}))
	if _, err := seed.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	holder := contractBegin(t, fixture.manager, options)
	if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, WaitForRowLock); !acquired || err != nil {
		t.Fatalf("holder lock: acquired=%v err=%v", acquired, err)
	}
	point := func(value int64) storage.KeySpan {
		bound := storage.KeyBound{Values: []model.Value{model.Int64Val(value)}, Inclusive: true}
		return storage.KeySpan{Lower: bound, Upper: bound}
	}
	all := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
	contender := contractBegin(t, fixture.manager, options)
	view, err := contender.ReadViewForRowLocks(t.Context(), []ReadDependency{
		{Target: fixture.request.Target, Occurrence: 1, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{all}},
		{Target: fixture.request.Target, Occurrence: 2, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{point(2)}},
	}, []RowLockPolicy{{Occurrence: 1, Strength: UpdateLock, Wait: SkipLockedRow}})
	if err != nil {
		t.Fatal(err)
	}
	skipRequest := storage.PushdownRequest{Target: fixture.request.Target, Occurrence: 1, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: fixture.request.ColumnIDs, Spans: []storage.KeySpan{all}}
	skipPlan, err := view.PlanScan(t.Context(), skipRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipPlan.Request.Spans) != 0 || len(skipPlan.Request.EncodedSpans) == 0 {
		t.Fatalf("targeted occurrence admission = typed %d encoded %d", len(skipPlan.Request.Spans), len(skipPlan.Request.EncodedSpans))
	}
	waitRequest := storage.PushdownRequest{Target: fixture.request.Target, Occurrence: 2, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: fixture.request.ColumnIDs, Spans: []storage.KeySpan{point(2)}}
	waitPlan, err := view.PlanScan(t.Context(), waitRequest)
	if err != nil {
		t.Fatal(err)
	}
	if len(waitPlan.Request.Spans) != 1 || len(waitPlan.Request.EncodedSpans) != 0 {
		t.Fatalf("untargeted occurrence admission = typed %d encoded %d", len(waitPlan.Request.Spans), len(waitPlan.Request.EncodedSpans))
	}
	waitRequest.Spans = []storage.KeySpan{all}
	if plan, err := view.PlanScan(t.Context(), waitRequest); err == nil {
		t.Fatalf("untargeted occurrence used targeted authorization: %#v", plan.Request)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := contender.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRowLockReadViewFailureRollsBackEarlierUntargetedLocks(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	holder := contractBegin(t, fixture.manager, options)
	if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, UpdateLock, WaitForRowLock); !acquired || err != nil {
		t.Fatalf("holder lock: acquired=%v err=%v", acquired, err)
	}
	point := func(value int64) storage.KeySpan {
		bound := storage.KeyBound{Values: []model.Value{model.Int64Val(value)}, Inclusive: true}
		return storage.KeySpan{Lower: bound, Upper: bound}
	}
	contender := contractBegin(t, fixture.manager, options)
	view, err := contender.ReadViewForRowLocks(t.Context(), []ReadDependency{
		{Target: fixture.request.Target, Occurrence: 2, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{point(2)}},
		{Target: fixture.request.Target, Occurrence: 1, KeyLayout: contractKeyLayout(), Spans: []storage.KeySpan{point(1)}},
	}, []RowLockPolicy{{Occurrence: 1, Strength: UpdateLock, Wait: NoWaitForRowLock}})
	if view != nil || err == nil {
		t.Fatalf("NOWAIT mixed view = %v, %v", view, err)
	}
	probe := contractBegin(t, fixture.manager, options)
	if acquired, err := probe.LockRow(t.Context(), fixture.request.Target, contractKey(2), UpdateLock, NoWaitForRowLock); !acquired || err != nil {
		t.Fatalf("failed view retained untargeted share lock: acquired=%v err=%v", acquired, err)
	}
	if err := probe.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := contender.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRowLockReadViewRejectsPolicyWithoutADeclaredOccurrence(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	contender := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	bound := storage.KeyBound{Values: []model.Value{model.Int64Val(1)}, Inclusive: true}
	dependency := ReadDependency{
		Target: fixture.request.Target, Occurrence: 1, KeyLayout: contractKeyLayout(),
		Spans: []storage.KeySpan{{Lower: bound, Upper: bound}},
	}
	view, err := contender.ReadViewForRowLocks(t.Context(), []ReadDependency{dependency}, []RowLockPolicy{{Occurrence: 2, Strength: UpdateLock, Wait: NoWaitForRowLock}})
	if view != nil || err == nil {
		t.Fatalf("unmatched row-lock policy opened a view: view=%v err=%v", view, err)
	}
	if err := contender.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRowLockReadViewUsesPerOccurrenceStrengthWithPCCShareFloor(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	options := contractBalanceOptions(t, fixture)
	holder := contractBegin(t, fixture.manager, options)
	if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, KeyShareLock, WaitForRowLock); !acquired || err != nil {
		t.Fatalf("key-share holder: acquired=%v err=%v", acquired, err)
	}
	dependency := ReadDependency{
		Target: fixture.request.Target, Occurrence: 1, KeyLayout: contractKeyLayout(),
		Spans: []storage.KeySpan{{
			Lower: storage.KeyBound{Values: []model.Value{model.Int64Val(1)}, Inclusive: true},
			Upper: storage.KeyBound{Values: []model.Value{model.Int64Val(1)}, Inclusive: true},
		}},
	}
	noKey := contractBegin(t, fixture.manager, options)
	view, err := noKey.ReadViewForRowLocks(t.Context(), []ReadDependency{dependency}, []RowLockPolicy{{Occurrence: 1, Strength: NoKeyUpdateLock, Wait: NoWaitForRowLock}})
	if err != nil || view == nil {
		t.Fatalf("compatible no-key-update policy: view=%v err=%v", view, err)
	}
	probe := contractBegin(t, fixture.manager, options)
	if acquired, err := probe.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, NoKeyUpdateLock, NoWaitForRowLock); acquired || err == nil {
		t.Fatalf("pre-view PCC share floor did not protect the occurrence: acquired=%v err=%v", acquired, err)
	}
	if err := probe.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if err := noKey.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	update := contractBegin(t, fixture.manager, options)
	if view, err := update.ReadViewForRowLocks(t.Context(), []ReadDependency{dependency}, []RowLockPolicy{{Occurrence: 1, Strength: UpdateLock, Wait: NoWaitForRowLock}}); view != nil || err == nil {
		t.Fatalf("update policy ignored key-share conflict: view=%v err=%v", view, err)
	}
	if holder.State() != Active {
		t.Fatalf("NOWAIT admission wounded holder: %v", holder.State())
	}
	if err := update.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := holder.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestRowLockReadViewWaitsAtTheOccurrenceStrength(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractBalanceOptions(t, fixture)
		holder := contractBegin(t, fixture.manager, options)
		if acquired, err := holder.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, KeyShareLock, WaitForRowLock); !acquired || err != nil {
			t.Fatalf("key-share holder: acquired=%v err=%v", acquired, err)
		}
		bound := storage.KeyBound{Values: []model.Value{model.Int64Val(1)}, Inclusive: true}
		dependency := ReadDependency{
			Target: fixture.request.Target, Occurrence: 1, KeyLayout: contractKeyLayout(),
			Spans: []storage.KeySpan{{Lower: bound, Upper: bound}},
		}
		waiter := contractBegin(t, fixture.manager, options)
		result := make(chan lockResult, 1)
		go func() {
			view, err := waiter.ReadViewForRowLocks(t.Context(), []ReadDependency{dependency}, []RowLockPolicy{{Occurrence: 1, Strength: UpdateLock, Wait: WaitForRowLock}})
			acquired := view != nil
			if view != nil {
				if closeErr := view.Close(); err == nil {
					err = closeErr
				}
			}
			result <- lockResult{acquired: acquired, err: err}
		}()
		requireLockBlocked(t, result)
		if err := holder.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		requireLockResult(t, result)
		probe := contractBegin(t, fixture.manager, options)
		if acquired, err := probe.LockRow(t.Context(), fixture.request.Target, fixture.request.Key, KeyShareLock, NoWaitForRowLock); !acquired || err != nil {
			t.Fatalf("pre-view admission retained the row strength instead of the PCC share floor: acquired=%v err=%v", acquired, err)
		}
		if err := probe.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := waiter.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
