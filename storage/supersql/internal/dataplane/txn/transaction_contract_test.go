package txn

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type contractResolver struct {
	normalizers []*RowNormalizer
}

func (resolver contractResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, normalizer := range resolver.normalizers {
		if normalizer.target == target {
			return normalizer, nil
		}
	}
	return nil, storage.ErrInvalidLayout
}

func contractPolicy(t *testing.T, target storage.ObjectRef, columns ...ColumnRule) *RowNormalizer {
	t.Helper()
	normalizer, err := NewRowNormalizer(t.Context(), target, columns, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return normalizer
}

func contractOptions(t *testing.T, normalizers ...*RowNormalizer) TransactionOptions {
	t.Helper()
	return TransactionOptions{Mode: ReadWrite, Resolver: contractResolver{normalizers: normalizers}, KeyEncoder: commitFixtureEncoder{}, MemoryBytes: 1 << 20, SpillBytes: 4 << 20, SpillDirectory: t.TempDir(), MaxViews: 8}
}

func contractBegin(t *testing.T, manager *Manager, options TransactionOptions) *Transaction {
	t.Helper()
	transaction, err := manager.Begin(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transaction.Close(t.Context()); err != nil {
			t.Error(err)
		}
	})
	return transaction
}

func contractBalanceOptions(t *testing.T, fixture commitFixture) TransactionOptions {
	t.Helper()
	return contractOptions(t, contractPolicy(t, fixture.request.Target, ColumnRule{Layout: fixture.request.Layout[0]}))
}

func contractKey(value int64) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(value)^(uint64(1)<<63))
}

func contractKeyLayout() []storage.KeyColumn {
	return []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}
}

func contractSpan(lower, upper int64) storage.KeySpan {
	return storage.KeySpan{Lower: storage.KeyBound{Values: []model.Value{model.Int64Val(lower)}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.Int64Val(upper)}}}
}

func contractView(t *testing.T, transaction *Transaction, target storage.ObjectRef, spans ...storage.KeySpan) storage.SnapshotReader {
	t.Helper()
	reader, err := transaction.ReadView(t.Context(), []ReadDependency{{Target: target, KeyLayout: contractKeyLayout(), Spans: spans}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader
}

type contractPointReader interface {
	Get(context.Context, storage.ReadRequest) (model.Row, error)
}

func contractValues(t *testing.T, reader contractPointReader, request storage.ReadRequest, want ...int64) {
	t.Helper()
	row, err := reader.Get(t.Context(), request)
	if err != nil {
		t.Fatalf("read key %x: %v", request.Key, err)
	}
	if len(row.Values) != len(want) {
		t.Fatalf("key %x values = %+v, want %v", request.Key, row.Values, want)
	}
	for i, value := range row.Values {
		if value.Type != model.TypeInt64 || value.IsNull || value.IntVal != want[i] {
			t.Fatalf("key %x column %d = %+v, want %d", request.Key, request.ColumnIDs[i], value, want[i])
		}
	}
}

func contractAbsent(t *testing.T, reader contractPointReader, request storage.ReadRequest) {
	t.Helper()
	if row, err := reader.Get(t.Context(), request); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("key %x = %+v, %v; want absent", request.Key, row, err)
	}
}

func contractApply(t *testing.T, transaction *Transaction, mutations ...storage.Mutation) {
	t.Helper()
	if err := transaction.ApplyMutations(t.Context(), mutations); err != nil {
		t.Fatal(err)
	}
}

func contractCommit(t *testing.T, transaction *Transaction) storage.ApplyResult {
	t.Helper()
	result, err := transaction.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func contractTable(t *testing.T) (*RowNormalizer, storage.ReadRequest) {
	t.Helper()
	target := storage.ObjectRef{ObjectID: 2, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	columns := []ColumnRule{
		{Layout: storage.ColumnLayout{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, KeyPosition: 1},
		{Layout: storage.ColumnLayout{ID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, Default: normalizerConst(model.Int64Val(7))},
		{Layout: storage.ColumnLayout{ID: 3, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, Default: normalizerConst(model.Int64Val(9))},
	}
	normalizer := contractPolicy(t, target, columns...)
	return normalizer, storage.ReadRequest{Target: target, ColumnIDs: []model.ColumnID{1, 2, 3}, Layout: normalizer.Layout().Columns}
}

// SQL affected-row counts and CHECK/FK/index/trigger expansion belong to the query layer.
func TestMutationSemantics(t *testing.T) {
	t.Run("durable_retry", mutationDurableRetry)
	t.Run("submission_isolation", mutationSubmissionIsolation)
	t.Run("complete_update_upsert_replace", func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		policy, request := contractTable(t)
		transaction := contractBegin(t, fixture.manager, contractOptions(t, policy))
		request.Key = contractKey(1)
		contractApply(t, transaction, storage.NewInsertMutation(request.Target, request.Key, request.ColumnIDs, []model.Value{model.Int64Val(1), model.Int64Val(10), model.Int64Val(20)}))
		contractValues(t, transaction, request, 1, 10, 20)
		contractApply(t, transaction, storage.NewUpdateMutation(request.Target, request.Key, []model.ColumnID{2}, []model.Value{model.Int64Val(15)}))
		contractValues(t, transaction, request, 1, 15, 20)
		err := transaction.ApplyMutations(t.Context(), []storage.Mutation{storage.NewInsertMutation(request.Target, request.Key, []model.ColumnID{1}, []model.Value{model.Int64Val(1)})})
		requireNormalizerState(t, err, "23505")
		contractValues(t, transaction, request, 1, 15, 20)
		request.Key = contractKey(2)
		err = transaction.ApplyMutations(t.Context(), []storage.Mutation{storage.NewUpdateMutation(request.Target, request.Key, []model.ColumnID{2}, []model.Value{model.Int64Val(25)})})
		if !errors.Is(err, storage.ErrRowNotFound) {
			t.Fatalf("missing update = %v", err)
		}
		contractApply(t, transaction, storage.NewUpsertMutation(request.Target, request.Key, []model.ColumnID{1}, []model.Value{model.Int64Val(2)}))
		contractValues(t, transaction, request, 2, 7, 9)
		request.Key = contractKey(1)
		contractApply(t, transaction, storage.NewReplaceMutation(request.Target, request.Key, []model.ColumnID{1, 2}, []model.Value{model.Int64Val(1), model.Int64Val(21)}))
		contractValues(t, transaction, request, 1, 21, 9)
		contractCommit(t, transaction)
		reader := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, reader, request, 1, 21, 9)
		request.Key = contractKey(2)
		contractValues(t, reader, request, 2, 7, 9)
	})

	for _, invalid := range []struct {
		name  string
		value model.Value
		state string
	}{{"not_null", model.NullVal(model.TypeInt64), "23502"}, {"type", model.StringVal("not an integer"), "42804"}} {
		t.Run("atomic_batch_"+invalid.name, func(t *testing.T) {
			fixture := newCommitFixture(t, nil)
			policy, request := contractTable(t)
			transaction := contractBegin(t, fixture.manager, contractOptions(t, policy))
			err := transaction.ApplyMutations(t.Context(), []storage.Mutation{
				storage.NewInsertMutation(request.Target, contractKey(10), []model.ColumnID{1}, []model.Value{model.Int64Val(10)}),
				storage.NewInsertMutation(request.Target, contractKey(11), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(11), invalid.value}),
			})
			requireNormalizerState(t, err, invalid.state)
			for _, key := range []int64{10, 11} {
				request.Key = contractKey(key)
				contractAbsent(t, transaction, request)
			}
			contractApply(t, transaction, storage.NewInsertMutation(request.Target, contractKey(12), []model.ColumnID{1}, []model.Value{model.Int64Val(12)}))
			contractCommit(t, transaction)
			reader := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
			for _, key := range []int64{10, 11} {
				request.Key = contractKey(key)
				contractAbsent(t, reader, request)
			}
			request.Key = contractKey(12)
			contractValues(t, reader, request, 12, 7, 9)
		})
	}

	inserted := func(t *testing.T, reader contractPointReader, request storage.ReadRequest) {
		contractValues(t, reader, request, 3, 33, 9)
	}
	for _, scenario := range []struct {
		name       string
		operations []storage.MutationOp
		check      func(*testing.T, contractPointReader, storage.ReadRequest)
	}{
		{"range_only", []storage.MutationOp{storage.MutationDeleteRange}, contractAbsent},
		{"insert_then_range", []storage.MutationOp{storage.MutationDelete, storage.MutationInsert, storage.MutationDeleteRange}, contractAbsent},
		{"range_then_insert", []storage.MutationOp{storage.MutationDeleteRange, storage.MutationInsert}, inserted},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newCommitFixture(t, nil)
			policy, request := contractTable(t)
			options := contractOptions(t, policy)
			seed := contractBegin(t, fixture.manager, options)
			for _, key := range []int64{2, 3, 4} {
				contractApply(t, seed, storage.NewInsertMutation(request.Target, contractKey(key), []model.ColumnID{1}, []model.Value{model.Int64Val(key)}))
			}
			seedResult := contractCommit(t, seed)
			transaction := contractBegin(t, fixture.manager, options)
			operations := map[storage.MutationOp]storage.Mutation{
				storage.MutationDelete:      storage.NewDeleteMutation(request.Target, contractKey(3)),
				storage.MutationDeleteRange: storage.NewDeleteRangeMutation(request.Target, contractKey(2), contractKey(4)),
				storage.MutationInsert:      storage.NewInsertMutation(request.Target, contractKey(3), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(3), model.Int64Val(33)}),
			}
			mutations := make([]storage.Mutation, len(scenario.operations))
			for i, op := range scenario.operations {
				mutations[i] = operations[op]
			}
			contractApply(t, transaction, mutations...)
			request.Key = contractKey(2)
			contractAbsent(t, transaction, request)
			request.Key = contractKey(3)
			scenario.check(t, transaction, request)
			request.Key = contractKey(4)
			contractValues(t, transaction, request, 4, 7, 9)
			contractCommit(t, transaction)
			checkReads := func(t *testing.T) {
				current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
				historical := contractBegin(t, fixture.manager, TransactionOptions{Mode: HistoricalReadOnly, Timestamp: seedResult.CommitTS})
				request.Key = contractKey(2)
				contractAbsent(t, current, request)
				request.Key = contractKey(3)
				scenario.check(t, current, request)
				request.Key = contractKey(4)
				contractValues(t, current, request, 4, 7, 9)
				for _, key := range []int64{2, 3, 4} {
					request.Key = contractKey(key)
					contractValues(t, historical, request, key, 7, 9)
				}
				if err := errors.Join(current.Close(t.Context()), historical.Close(t.Context())); err != nil {
					t.Fatal(err)
				}
			}
			t.Run("before_compaction", checkReads)
			task, err := fixture.backend.ScheduleCompaction(t.Context(), storage.CompactionRequest{Target: request.Target, TargetBlock: 64 << 10})
			if err != nil {
				t.Fatal(err)
			}
			if err := task.Wait(context.WithoutCancel(t.Context())); err != nil {
				t.Fatal(err)
			}
			t.Run("after_compaction", checkReads)
			reopenCommitFixture(t, &fixture)
			t.Run("after_reopen", checkReads)
		})
	}
}

func TestSerializableRangePhantom(t *testing.T) {
	t.Run("insert", func(t *testing.T) { testSerializableRangePhantom(t, false) })
	t.Run("key_move", func(t *testing.T) { testSerializableRangePhantom(t, true) })
}

func testSerializableRangePhantom(t *testing.T, keyMove bool) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		policy, request := contractTable(t)
		balancePolicy := contractPolicy(t, fixture.request.Target, ColumnRule{Layout: fixture.request.Layout[0]})
		options := contractOptions(t, policy, balancePolicy)
		if keyMove {
			seed := contractBegin(t, fixture.manager, options)
			contractApply(t, seed, storage.NewInsertMutation(request.Target, contractKey(150), []model.ColumnID{1}, []model.Value{model.Int64Val(150)}))
			contractCommit(t, seed)
		}
		reader := contractBegin(t, fixture.manager, options)
		span := contractSpan(10, 100)
		span.Upper.Inclusive = true
		view := contractView(t, reader, request.Target, span)
		plan, err := view.PlanScan(t.Context(), storage.PushdownRequest{Target: request.Target, Layout: request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: request.ColumnIDs, Spans: []storage.KeySpan{span}})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := view.ReleasePlan(plan); err != nil {
				t.Error(err)
			}
		}()
		cursor, err := view.VectorizedScan(t.Context(), plan)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := cursor.Close(); err != nil {
				t.Error(err)
			}
		}()
		batch := columnar.Batch{MaxRows: 1}
		if err := cursor.Next(t.Context(), &batch); err != io.EOF {
			t.Fatalf("indexed range [10,100] = %v, want empty", err)
		}
		writer := contractBegin(t, fixture.manager, options)
		mutations := []storage.Mutation{storage.NewInsertMutation(request.Target, contractKey(50), []model.ColumnID{1}, []model.Value{model.Int64Val(50)})}
		if keyMove {
			mutations = append([]storage.Mutation{storage.NewDeleteMutation(request.Target, contractKey(150))}, mutations...)
		}
		done := make(chan error, 1)
		go func() { done <- writer.ApplyMutations(t.Context(), mutations) }()
		synctest.Wait()
		blocked := true
		select {
		case err := <-done:
			requireNormalizerState(t, err, "40001")
			blocked = false
		default:
		}
		// The empty range decides a write outside it; B must observe A before committing.
		contractApply(t, reader, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
		first := contractCommit(t, reader)
		if blocked {
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			contractValues(t, writer, fixture.request, 90)
			second := contractCommit(t, writer)
			if second.CommitTS <= first.CommitTS {
				t.Fatalf("insert commit %d did not follow conditional write %d", second.CommitTS, first.CommitTS)
			}
		} else if err := writer.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, current, fixture.request, 90)
		request.Key = contractKey(50)
		if blocked {
			contractValues(t, current, request, 50, 7, 9)
		} else {
			contractAbsent(t, current, request)
		}
		if keyMove {
			request.Key = contractKey(150)
			if blocked {
				contractAbsent(t, current, request)
			} else {
				contractValues(t, current, request, 150, 7, 9)
			}
		}
	})
}

func TestWoundWaitHolderAbort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		releaseWork := sync.OnceFunc(func() { close(release) })
		defer releaseWork()
		target := storage.ObjectRef{ObjectID: 2, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
		policy := contractPolicy(t, target,
			ColumnRule{Layout: storage.ColumnLayout{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, KeyPosition: 1},
			ColumnRule{Layout: storage.ColumnLayout{ID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, Default: normalizerExpression(func(ctx context.Context, _ []model.Value) (model.Value, error) {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-release
				return model.Value{}, context.Cause(ctx)
			})},
		)
		balancePolicy := contractPolicy(t, fixture.request.Target, ColumnRule{Layout: fixture.request.Layout[0]})
		options := contractOptions(t, policy, balancePolicy)
		older := contractBegin(t, fixture.manager, options)
		younger := contractBegin(t, fixture.manager, options)
		view := contractView(t, younger, fixture.request.Target, contractSpan(1, 2))
		contractValues(t, view, fixture.request, 100)
		contractApply(t, younger, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(80)}))
		youngerDone := make(chan error, 1)
		go func() {
			youngerDone <- younger.ApplyMutations(t.Context(), []storage.Mutation{storage.NewInsertMutation(target, contractKey(2), []model.ColumnID{1}, []model.Value{model.Int64Val(2)})})
		}()
		<-entered
		olderDone := make(chan error, 1)
		go func() {
			olderDone <- older.ApplyMutations(t.Context(), []storage.Mutation{storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)})})
		}()
		<-canceled
		synctest.Wait()
		select {
		case err := <-olderDone:
			t.Fatalf("requester finished before canceled holder stopped: %v", err)
		default:
		}
		releaseWork()
		requireNormalizerState(t, <-youngerDone, "40001")
		if err := <-olderDone; err != nil {
			t.Fatal(err)
		}
		if younger.State() != Aborted {
			t.Fatalf("wounded holder state = %v, want Aborted before requester grant", younger.State())
		}
		if _, err := younger.Commit(t.Context()); err == nil {
			t.Fatal("wounded holder committed")
		}
		if _, err := view.Get(t.Context(), fixture.request); err == nil {
			t.Fatal("wounded transaction retained a usable read view")
		}
		contractCommit(t, older)
		current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, current, fixture.request, 90)
		contractAbsent(t, current, storage.ReadRequest{Target: target, Key: contractKey(2), ColumnIDs: []model.ColumnID{1, 2}, Layout: policy.Layout().Columns})
	})
}

func TestManagedReadOnlySnapshotDoesNotBlockWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		fixed := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, fixed, fixture.request, 100)
		writer := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
		contractApply(t, writer, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
		result := contractCommit(t, writer)
		contractValues(t, fixed, fixture.request, 100)
		current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, current, fixture.request, 90)
		err := fixed.ApplyMutations(t.Context(), []storage.Mutation{storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(70)})})
		requireNormalizerState(t, err, "25006")
		fixture.clock.mu.Lock()
		fixture.clock.reading.Status = 0
		fixture.clock.mu.Unlock()
		contractValues(t, fixed, fixture.request, 100)
		historical := contractBegin(t, fixture.manager, TransactionOptions{Mode: HistoricalReadOnly, Timestamp: result.CommitTS})
		contractValues(t, historical, fixture.request, 90)
		err = historical.ApplyMutations(t.Context(), []storage.Mutation{storage.NewDeleteMutation(fixture.request.Target, fixture.request.Key)})
		requireNormalizerState(t, err, "25006")
		unfixed := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		if _, err := unfixed.Get(t.Context(), fixture.request); !errors.Is(err, clock.ErrClockUnavailable) {
			t.Fatalf("first strong data access without clock = %v", err)
		}
	})
}

func TestManagedViewRejectsUnlockedReads(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	view := contractView(t, transaction, fixture.request.Target, contractSpan(10, 100))
	_, err := view.Get(t.Context(), fixture.request)
	requireNormalizerState(t, err, "55000")
	request := storage.PushdownRequest{Target: fixture.request.Target, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: fixture.request.ColumnIDs, Spans: []storage.KeySpan{contractSpan(1, 2)}}
	_, err = view.PlanScan(t.Context(), request)
	requireNormalizerState(t, err, "55000")
	request.Spans = []storage.KeySpan{contractSpan(10, 100)}
	plan, err := view.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := view.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	batch := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("declared empty span scan = %v, want EOF", err)
	}
}

func TestManagedSavepointRetainsEarlierBranch(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	update := func(value int64) storage.Mutation {
		return storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(value)})
	}
	contractApply(t, transaction, update(90))
	earlier, err := transaction.Savepoint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	contractApply(t, transaction, update(80))
	discarded, err := transaction.Savepoint(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	contractApply(t, transaction, update(70))
	if err := transaction.RollbackTo(t.Context(), earlier); err != nil {
		t.Fatal(err)
	}
	contractValues(t, transaction, fixture.request, 90)
	contractApply(t, transaction, update(60))
	requireNormalizerState(t, transaction.RollbackTo(t.Context(), discarded), "3B001")
	contractValues(t, transaction, fixture.request, 60)
	if err := transaction.RollbackTo(t.Context(), earlier); err != nil {
		t.Fatalf("reusing retained earlier mark: %v", err)
	}
	contractValues(t, transaction, fixture.request, 90)
	contractCommit(t, transaction)
	current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
	contractValues(t, current, fixture.request, 90)
}

func TestManagedSavepointRestoresReadAndWriteLocks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractBalanceOptions(t, fixture)
		transaction := contractBegin(t, fixture.manager, options)
		contractValues(t, transaction, fixture.request, 100)
		write := func(key, value int64) storage.Mutation {
			return storage.NewUpsertMutation(fixture.request.Target, contractKey(key), fixture.request.ColumnIDs, []model.Value{model.Int64Val(value)})
		}
		contractApply(t, transaction, write(2, 20))
		mark, err := transaction.Savepoint(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		request := fixture.request
		request.Key = contractKey(3)
		contractAbsent(t, transaction, request)
		contractApply(t, transaction, write(4, 40))
		if err := transaction.RollbackTo(t.Context(), mark); err != nil {
			t.Fatal(err)
		}
		later := contractBegin(t, fixture.manager, options)
		contractApply(t, later, write(3, 33), write(4, 44))
		contractCommit(t, later)
		request.Key = contractKey(2)
		contractValues(t, transaction, request, 20)
		for _, key := range []int64{3, 4} {
			request.Key = contractKey(key)
			contractValues(t, transaction, request, key*11)
		}
		readCompetitor := contractBegin(t, fixture.manager, options)
		writeCompetitor := contractBegin(t, fixture.manager, options)
		readDone, writeDone := make(chan error, 1), make(chan error, 1)
		go func() { readDone <- readCompetitor.ApplyMutations(t.Context(), []storage.Mutation{write(1, 91)}) }()
		go func() { writeDone <- writeCompetitor.ApplyMutations(t.Context(), []storage.Mutation{write(2, 22)}) }()
		synctest.Wait()
		for _, pending := range []<-chan error{readDone, writeDone} {
			select {
			case err := <-pending:
				t.Fatalf("rollback released a pre-savepoint lock: %v", err)
			default:
			}
		}
		if err := transaction.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-readDone; err != nil {
			t.Fatal(err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		contractCommit(t, readCompetitor)
		contractCommit(t, writeCompetitor)
		current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, current, fixture.request, 91)
		request.Key = contractKey(2)
		contractValues(t, current, request, 22)
	})
}

func TestManagedCloseReleasesViewsAndCursorPins(t *testing.T) {
	for _, closeManager := range []bool{false, true} {
		name := "transaction"
		if closeManager {
			name = "manager"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newCommitFixture(t, nil)
				transaction := contractBegin(t, fixture.manager, TransactionOptions{Mode: HistoricalReadOnly, Timestamp: 20, KeyEncoder: commitFixtureEncoder{}})
				view := contractView(t, transaction, fixture.request.Target, contractSpan(1, 2))
				plan, err := view.PlanScan(t.Context(), storage.PushdownRequest{Target: fixture.request.Target, Layout: fixture.request.Layout, KeyLayout: contractKeyLayout(), ColumnIDs: fixture.request.ColumnIDs, Spans: []storage.KeySpan{contractSpan(1, 2)}})
				if err != nil {
					t.Fatal(err)
				}
				cursor, err := view.VectorizedScan(t.Context(), plan)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := cursor.Close(); err != nil {
						t.Error(err)
					}
				})
				batch := columnar.Batch{MaxRows: 1}
				if err := cursor.Next(t.Context(), &batch); err != nil {
					t.Fatal(err)
				}
				writer := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
				contractApply(t, writer, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
				contractCommit(t, writer)
				if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 21}); !errors.Is(err, storage.ErrSnapshotPinned) {
					t.Fatalf("collection crossed a live fixed view: %v", err)
				}
				closeOwner := func() error { return transaction.Close(t.Context()) }
				if closeManager {
					closeOwner = fixture.manager.Close
				}
				start, done := make(chan struct{}), make(chan error, 4)
				for range 4 {
					go func() { <-start; done <- closeOwner() }()
				}
				close(start)
				for range 4 {
					if err := <-done; err != nil {
						t.Fatal(err)
					}
				}
				if _, err := view.Get(t.Context(), fixture.request); err == nil {
					t.Fatal("closed owner left a usable read view")
				}
				if err := cursor.Next(t.Context(), &batch); !errors.Is(err, storage.ErrClosed) {
					t.Fatalf("closed owner cursor = %v, want ErrClosed", err)
				}
				if err := view.ReleasePlan(plan); err != nil {
					t.Fatalf("closed owner plan cleanup: %v", err)
				}
				if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 21}); err != nil {
					t.Fatalf("closed owner retained a snapshot or cursor pin: %v", err)
				}
			})
		})
	}
}

func TestManagedIndeterminateCommitResolvesWithoutClock(t *testing.T) {
	var injected *cancelAfterCommitBackend
	fixture := newCommitFixture(t, func(backend storage.Backend) storage.Backend {
		injected = &cancelAfterCommitBackend{Backend: backend}
		return injected
	})
	transaction := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	contractApply(t, transaction, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	injected.cancel = cancel
	_, err := transaction.Commit(ctx)
	if !errors.Is(err, storage.ErrCommitIndeterminate) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel after durable decision = %v", err)
	}
	if transaction.State() != Indeterminate {
		t.Fatalf("durably ambiguous transaction state = %v", transaction.State())
	}
	fixture.clock.mu.Lock()
	fixture.clock.reading.Status = 0
	fixture.clock.mu.Unlock()
	resolution, err := transaction.Resolve(t.Context())
	if err != nil || resolution.State != storage.CommitCommitted {
		t.Fatalf("clock-free transaction resolution = %+v, %v", resolution, err)
	}
	if transaction.State() != Committed {
		t.Fatalf("resolved transaction state = %v", transaction.State())
	}
	repeated, err := transaction.Resolve(t.Context())
	if err != nil || repeated != resolution {
		t.Fatalf("repeated resolution = %+v, %v; want %+v", repeated, err, resolution)
	}
	historical := contractBegin(t, fixture.manager, TransactionOptions{Mode: HistoricalReadOnly, Timestamp: resolution.Result.CommitTS})
	contractValues(t, historical, fixture.request, 90)
}

func TestManagedCommitSurvivesBackendReopen(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	policy, request := contractTable(t)
	transaction := contractBegin(t, fixture.manager, contractOptions(t, policy))
	request.Key = contractKey(2)
	contractApply(t, transaction, storage.NewInsertMutation(request.Target, request.Key, []model.ColumnID{1}, []model.Value{model.Int64Val(2)}))
	committed := contractCommit(t, transaction)
	reopenCommitFixture(t, &fixture)
	reader := contractBegin(t, fixture.manager, TransactionOptions{Mode: HistoricalReadOnly, Timestamp: committed.CommitTS})
	contractValues(t, reader, request, 2, 7, 9)
	resolution, err := fixture.manager.ResolveCommit(t.Context(), transaction.ID())
	if err != nil || resolution.State != storage.CommitCommitted || resolution.Result != committed {
		t.Fatalf("recovered managed commit = %+v, %v; want %+v", resolution, err, committed)
	}
}

func TestManagedCloseCancelsPendingLockRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newCommitFixture(t, nil)
		options := contractBalanceOptions(t, fixture)
		holder := contractBegin(t, fixture.manager, options)
		contractValues(t, holder, fixture.request, 100)
		waiter := contractBegin(t, fixture.manager, options)
		pending := make(chan error, 1)
		go func() {
			pending <- waiter.ApplyMutations(t.Context(), []storage.Mutation{storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)})})
		}()
		synctest.Wait()
		select {
		case err := <-pending:
			t.Fatalf("writer bypassed holder before close: %v", err)
		default:
		}
		if err := waiter.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		requireNormalizerState(t, <-pending, "57014")
		if waiter.State() != Aborted {
			t.Fatalf("closed lock waiter state = %v", waiter.State())
		}
		contractValues(t, holder, fixture.request, 100)
		if err := holder.Rollback(t.Context()); err != nil {
			t.Fatal(err)
		}
		writer := contractBegin(t, fixture.manager, options)
		contractApply(t, writer, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(80)}))
		contractCommit(t, writer)
		current := contractBegin(t, fixture.manager, TransactionOptions{Mode: ReadOnly})
		contractValues(t, current, fixture.request, 80)
	})
}
