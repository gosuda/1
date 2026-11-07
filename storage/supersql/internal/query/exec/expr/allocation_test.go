package expr_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func allocationQuery(t *testing.T) *runtime.Query {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	shared, err := runtime.NewBudget(2<<20, 16)
	if err != nil {
		t.Fatal(err)
	}
	query, err := runtime.NewQuery(t.Context(), pool, shared, nil, runtime.QueryConfig{WorkBytes: 1 << 20, OutputBytes: 1 << 20, SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := query.Close(); err != nil {
			t.Error(err)
		}
	})
	return query
}

func TestStatesShareOutputCapacityAndReleaseOnCancellation(t *testing.T) {
	q := allocationQuery(t)
	f := newFixture(t)
	p := compile(t, f.graph(), []ir.ScalarID{f.x}, f.options())
	options := expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)}
	first, err := expr.NewState(p, options)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := expr.NewState(p, options)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	input := batch(1)
	var firstOut, secondOut columnar.Batch
	if err := first.Eval(t.Context(), &input, nil, &firstOut, expr.Direct); err != nil {
		t.Fatal(err)
	}
	usage := q.Usage()
	if usage.Work.Live != 0 || usage.Work.Reservations != 0 {
		t.Fatalf("successful evaluation retained work: %+v", usage.Work)
	}
	blocker, err := q.TryReserve(runtime.OutputMemory, usage.Output.Limit-2*usage.Output.Live+1)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if err := second.Eval(t.Context(), &input, nil, &secondOut, expr.Direct); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("second state bypassed shared output cap: %v", err)
	}
	requireInt(t, value(t, &firstOut, 0, 0), 0, false)
	blocker.Release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := first.Eval(ctx, &input, nil, &firstOut, expr.Direct); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled evaluation = %v", err)
	}
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("failed evaluations leaked claims: %+v", usage)
	}
	if err := second.Eval(t.Context(), &input, nil, &secondOut, expr.Direct); err != nil {
		t.Fatalf("released output remained unavailable: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if usage := q.Usage().Output; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("close leaked output: %+v", usage)
	}
}

func TestStatesShareLiveEvaluationWork(t *testing.T) {
	q := allocationQuery(t)
	f := newFixture(t)
	root := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile})
	input := columnar.Batch{Length: 1}
	var second *expr.State
	calls := 0
	options := f.options()
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(ctx context.Context, _ []model.Value, _ builtin.CallBudget) (model.Value, error) {
			calls++
			var out columnar.Batch
			if err := second.Eval(ctx, &input, nil, &out, expr.Direct); !errors.Is(err, runtime.ErrBudget) {
				t.Fatalf("nested state bypassed shared work cap: %v", err)
			}
			return model.Int64Val(7), nil
		}, nil
	})
	p := compile(t, f.graph(), []ir.ScalarID{root}, options)
	stateOptions := expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)}
	first, err := expr.NewState(p, stateOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err = expr.NewState(p, stateOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var out columnar.Batch
	if err := first.Eval(t.Context(), &input, nil, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("rejected state invoked function %d times", calls)
	}
	requireInt(t, value(t, &out, 0, 0), 7, false)
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("work claims leaked: %+v", usage)
	}
}

func TestRetainDetachesQueryScratchWithoutMutatingPublishedChildren(t *testing.T) {
	q := allocationQuery(t)
	integer := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	layout := model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	builder, err := columnar.NewBuilder([]model.TypeLayout{layout}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer builder.Close()
	row := []model.Value{{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}, ArrayVal: []model.Value{model.Int64Val(7), model.NullVal(model.TypeInt64)}}}
	if err := builder.Append(t.Context(), row); err != nil {
		t.Fatal(err)
	}
	var input columnar.Batch
	if err := builder.Publish(&input); err != nil {
		t.Fatal(err)
	}
	retained, err := expr.Retain(t.Context(), &input, []model.TypeLayout{layout}, expr.RetainOptions{MaxRows: 1, MaxBytes: 8192, WorkAllocator: q.Allocator(runtime.WorkMemory)})
	if err != nil {
		t.Fatal(err)
	}
	defer retained.Close()
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("retained result kept query scratch: %+v", usage)
	}
	if err := builder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := q.Close(); err != nil {
		t.Fatal(err)
	}
	var out columnar.Batch
	if err := retained.Publish(&out); err != nil {
		t.Fatal(err)
	}
	if err := retained.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(1); err != nil {
		t.Fatalf("close mutated outstanding nested headers: %v", err)
	}
	copied := value(t, &out, 0, 0)
	if len(copied.ArrayVal) != 2 || copied.ArrayVal[0].IntVal != 7 || !copied.ArrayVal[1].IsNull {
		t.Fatalf("retained children changed: %+v", copied)
	}
}
