package txn

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type preparedSequence struct{ value int64 }

func (s *preparedSequence) NextSequence(ctx context.Context, _ model.OID) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.value++
	return s.value, nil
}

func (s *preparedSequence) CurrentSequence(ctx context.Context, _ model.OID) (int64, error) {
	return s.value, ctx.Err()
}

func (s *preparedSequence) LastSequence(ctx context.Context) (int64, error) {
	return s.value, ctx.Err()
}

func (s *preparedSequence) SetSequence(ctx context.Context, _ model.OID, value int64, called bool) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.value = value
	if !called {
		s.value--
	}
	return value, nil
}

type preparedSequenceDefault struct{}

func (preparedSequenceDefault) Evaluate(ctx context.Context, _ []model.Value, runtime model.ExpressionRuntime) (model.Value, error) {
	if runtime.Sequences == nil {
		return model.Value{}, storage.ErrInvalidMutation
	}
	value, err := runtime.Sequences.NextSequence(ctx, 1)
	return model.Int64Val(value), err
}

func TestPreparedInsertNormalizesDefaultsOnceAndOwnsItsImage(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	target := storage.ObjectRef{ObjectID: 1000, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	sequences := &preparedSequence{value: 6}
	generated := 0
	policy := contractPolicy(t, target,
		ColumnRule{Layout: storage.ColumnLayout{ID: 1, Type: typ}, KeyPosition: 1, Default: preparedSequenceDefault{}},
		ColumnRule{Layout: storage.ColumnLayout{ID: 2, Type: typ}},
		ColumnRule{Layout: storage.ColumnLayout{ID: 3, Type: typ}, Generated: normalizerExpression(func(_ context.Context, values []model.Value) (model.Value, error) {
			generated++
			return model.Int64Val(values[1].IntVal * 2), nil
		})})
	transaction := contractBegin(t, fixture.manager, contractOptions(t, policy))
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	}()
	budget, err := runtime.NewBudget(1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	query, err := runtime.NewQuery(t.Context(), pool, budget, runtime.OSFileSystem{}, runtime.QueryConfig{WorkBytes: 1 << 20, OutputBytes: 1 << 16, MaxReservations: 100, SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
	}()
	candidate, err := transaction.PrepareInsert(t.Context(), InsertPreparation{Target: target, ColumnIDs: []model.ColumnID{2}, Values: []model.Value{model.Int64Val(9)}, Allocator: query.Allocator(runtime.WorkMemory), MaxBytes: 64 << 10, Expressions: model.ExpressionRuntime{Sequences: sequences}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := candidate.Close(); err != nil {
			t.Error(err)
		}
	}()
	copyClaim, err := query.Allocator(runtime.WorkMemory).Reserve(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer copyClaim.Release()
	row, used, err := candidate.CopyRow(t.Context(), 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := copyClaim.Resize(used); err != nil {
		t.Fatal(err)
	}
	row.Values[0].IntVal = 99
	row.Values[2].IntVal = 999
	row.Key[0] ^= 255
	if err := transaction.ApplyPreparedInsert(t.Context(), candidate); err != nil {
		t.Fatal(err)
	}
	if sequences.value != 7 || generated != 1 {
		t.Fatalf("sequence=%d generated evaluations=%d", sequences.value, generated)
	}
	request := storage.ReadRequest{Target: target, Key: contractKey(7), ColumnIDs: []model.ColumnID{1, 2, 3}, Layout: []storage.ColumnLayout{{ID: 1, Type: typ}, {ID: 2, Type: typ}, {ID: 3, Type: typ}}}
	contractValues(t, transaction, request, 7, 9, 18)
	if err := transaction.ApplyPreparedInsert(t.Context(), candidate); !errors.Is(err, storage.ErrInvalidMutation) {
		t.Fatalf("candidate applied twice: %v", err)
	}
	conflictingSequence := &preparedSequence{value: 6}
	conflicting, err := transaction.PrepareInsert(t.Context(), InsertPreparation{Target: target, ColumnIDs: []model.ColumnID{2}, Values: []model.Value{model.Int64Val(20)}, Allocator: query.Allocator(runtime.WorkMemory), MaxBytes: 64 << 10, Expressions: model.ExpressionRuntime{Sequences: conflictingSequence}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conflicting.Close(); err != nil {
			t.Error(err)
		}
	}()
	requireNormalizerState(t, transaction.ApplyPreparedInsert(t.Context(), conflicting), "23505")
	if sequences.value != 7 || conflictingSequence.value != 7 || generated != 2 {
		t.Fatalf("conflict effects: first=%d second=%d generated=%d", sequences.value, conflictingSequence.value, generated)
	}
	contractValues(t, transaction, request, 7, 9, 18)
	if err := conflicting.Close(); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := candidate.CopyRow(t.Context(), 64<<10); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("closed candidate still readable: %v", err)
	}
	row = model.Row{}
	copyClaim.Release()
	if query.Usage().Work.Live != 0 {
		t.Fatalf("candidate retained query memory: %+v", query.Usage())
	}
}
