package series_test

import (
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/exec/series"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var integer = model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}

func query(t *testing.T, ctx context.Context) *runtime.Query {
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
	budget, err := runtime.NewBudget(4<<20, 128)
	if err != nil {
		t.Fatal(err)
	}
	q, err := runtime.NewQuery(ctx, pool, budget, nil, runtime.QueryConfig{WorkBytes: 2 << 20, OutputBytes: 1 << 20, MaxReservations: 128, SpillMode: runtime.SpillDisabled})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Error(err)
		}
	})
	return q
}

type fixture struct {
	t       *testing.T
	builder *ir.Builder
	scope   ir.ScopeID
	layout  ir.LayoutID
	typ     model.TypeLayout
	slot    model.SlotID
	options expr.Options
	calls   map[model.FunctionID]expr.ScalarCall
}

func newFixture(t *testing.T, typ model.TypeLayout) *fixture {
	t.Helper()
	builder, err := ir.NewBuilder(ir.Limits{MaxNodes: 128, MaxOperands: 256, MaxSlots: 16, MaxDepth: 16, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, builder: builder, typ: typ, options: expr.Options{Limits: expr.DefaultLimits()}, calls: make(map[model.FunctionID]expr.ScalarCall)}
	f.scope, err = builder.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.layout, err = builder.AddLayout(t.Context(), typ)
	if err != nil {
		t.Fatal(err)
	}
	f.slot, err = builder.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true, Object: 1, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) scalar(scalar ir.Scalar, value model.Value) ir.ScalarID {
	f.t.Helper()
	scalar.Scope = f.scope
	if scalar.Layout == 0 {
		scalar.Layout = f.layout
	}
	id, err := f.builder.AddScalar(f.t.Context(), ir.ScalarSpec{Scalar: scalar, Value: value})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) constant(value model.Value) ir.ScalarID {
	return f.scalar(ir.Scalar{Kind: ir.Const, Nullable: value.IsNull}, value)
}

func (f *fixture) function(call expr.ScalarCall) ir.ScalarID {
	return f.functionWithLayout(f.layout, call)
}

func (f *fixture) functionWithLayout(layout ir.LayoutID, call expr.ScalarCall) ir.ScalarID {
	f.t.Helper()
	id := model.FunctionID(900000 + len(f.calls))
	f.calls[id] = call
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	signature, err := f.builder.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: id, Result: layout, Nullable: true, Effects: effects}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.scalar(ir.Scalar{Kind: ir.Call, Layout: layout, Function: id, Signature: signature, Nullable: true, Effects: effects}, model.Value{})
}

func (f *fixture) Resolve(fn expr.Function) (expr.ScalarCall, error) {
	return f.calls[fn.ID], nil
}

func (f *fixture) compile(roots ...ir.ScalarID) *expr.Program {
	f.t.Helper()
	root, err := f.builder.AddRel(f.t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: 1, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{f.slot}})
	if err != nil {
		f.t.Fatal(err)
	}
	graph, err := f.builder.Build(f.t.Context(), root)
	if err != nil {
		f.t.Fatal(err)
	}
	f.options.Functions = f
	program, err := expr.Compile(f.t.Context(), graph, roots, f.options)
	if err != nil {
		f.t.Fatal(err)
	}
	return program
}

func constants(t *testing.T, typ model.TypeLayout, values ...model.Value) *expr.Program {
	t.Helper()
	f := newFixture(t, typ)
	roots := make([]ir.ScalarID, len(values))
	for i, value := range values {
		roots[i] = f.constant(value)
	}
	return f.compile(roots...)
}

func options(q *runtime.Query, program *expr.Program) series.Options {
	return series.Options{Arguments: program, Layout: integer, Query: q, BatchRows: 3, BatchBytes: 4096}
}

func open(t *testing.T, ctx context.Context, options series.Options) storage.BatchCursor {
	t.Helper()
	cursor, err := series.New(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
	})
	return cursor
}

func released(t *testing.T, q *runtime.Query) {
	t.Helper()
	usage := q.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("cursor retained query resources: %+v", usage)
	}
}

func values(t *testing.T, out *columnar.Batch) []int64 {
	t.Helper()
	if err := out.Validate(max(1, out.Length)); err != nil {
		t.Fatal(err)
	}
	result := make([]int64, out.Rows())
	for i := range result {
		lane := out.Lane(i)
		if out.Columns[0].IsNull(lane) {
			t.Fatalf("series row %d is NULL", i)
		}
		result[i] = out.Columns[0].Int64[lane]
	}
	return result
}

func TestInclusiveRangesAndSignedOverflow(t *testing.T) {
	cases := []struct {
		name      string
		oid       model.TypeID
		arguments []int64
		want      []int64
	}{
		{"default step", 23, []int64{2, 4}, []int64{2, 3, 4}},
		{"descending", 20, []int64{5, -2, -3}, []int64{5, 2, -1}},
		{"positive wrong direction", 20, []int64{4, 2, 1}, nil},
		{"negative wrong direction", 23, []int64{2, 4, -1}, nil},
		{"equal endpoints", 20, []int64{-3, -3, -2}, []int64{-3}},
		{"int4 maximum endpoint", 23, []int64{math.MaxInt32 - 1, math.MaxInt32}, []int64{math.MaxInt32 - 1, math.MaxInt32}},
		{"int4 positive overflow", 23, []int64{math.MaxInt32 - 1, math.MaxInt32, 2}, []int64{math.MaxInt32 - 1}},
		{"int4 negative overflow", 23, []int64{math.MinInt32 + 1, math.MinInt32, -2}, []int64{math.MinInt32 + 1}},
		{"int8 maximum endpoint", 20, []int64{math.MaxInt64 - 1, math.MaxInt64}, []int64{math.MaxInt64 - 1, math.MaxInt64}},
		{"int8 minimum endpoint", 20, []int64{math.MinInt64 + 1, math.MinInt64, -1}, []int64{math.MinInt64 + 1, math.MinInt64}},
		{"int8 positive overflow", 20, []int64{math.MaxInt64 - 1, math.MaxInt64, 2}, []int64{math.MaxInt64 - 1}},
		{"int8 negative overflow", 20, []int64{math.MinInt64 + 1, math.MinInt64, -2}, []int64{math.MinInt64 + 1}},
		{"int8 wide positive step", 20, []int64{math.MinInt64, math.MaxInt64, math.MaxInt64}, []int64{math.MinInt64, -1, math.MaxInt64 - 1}},
		{"int8 minimum step", 20, []int64{math.MaxInt64, math.MinInt64, math.MinInt64}, []int64{math.MaxInt64, -1}},
		{"int4 minimum step", 23, []int64{math.MaxInt32, math.MinInt32, math.MinInt32}, []int64{math.MaxInt32, -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := query(t, t.Context())
			typ := integer
			typ.Type.ID = tc.oid
			arguments := make([]model.Value, len(tc.arguments))
			for i, value := range tc.arguments {
				arguments[i] = model.Int64Val(value)
			}
			opts := options(q, constants(t, typ, arguments...))
			opts.Layout = typ
			cursor := open(t, t.Context(), opts)
			var got []int64
			out := columnar.Batch{MaxRows: 2}
			for {
				err := cursor.Next(t.Context(), &out)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if out.Rows() == 0 || len(got)+out.Rows() > len(tc.want) {
					t.Fatalf("unexpected output after %v: %v", got, values(t, &out))
				}
				got = append(got, values(t, &out)...)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("series = %v, want %v", got, tc.want)
			}
			released(t, q)
		})
	}
}

func TestStrictNullSuppressesZeroStep(t *testing.T) {
	for nullIndex, name := range []string{"start", "stop", "step"} {
		t.Run(name, func(t *testing.T) {
			q := query(t, t.Context())
			arguments := []model.Value{model.Int64Val(1), model.Int64Val(3), model.Int64Val(0)}
			arguments[nullIndex] = model.NullVal(model.TypeInt64)
			cursor := open(t, t.Context(), options(q, constants(t, integer, arguments...)))
			var out columnar.Batch
			if err := cursor.Next(t.Context(), &out); err != io.EOF {
				t.Fatalf("NULL argument %d with zero step = %v, want EOF", nullIndex, err)
			}
			released(t, q)
		})
	}
}

func TestZeroStepErrorsBeforeDirectionFiltering(t *testing.T) {
	q := query(t, t.Context())
	cursor := open(t, t.Context(), options(q, constants(t, integer, model.Int64Val(4), model.Int64Val(1), model.Int64Val(0))))
	var out columnar.Batch
	err := cursor.Next(t.Context(), &out)
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "22023" {
		t.Fatalf("zero step = %v, want SQLSTATE 22023", err)
	}
	if out.Rows() != 0 {
		t.Fatalf("zero step published %d rows", out.Rows())
	}
	released(t, q)
}

func TestNullStillEvaluatesOtherArguments(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, integer)
	failure := errors.New("argument evaluation failed")
	start := f.constant(model.NullVal(model.TypeInt64))
	stop := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		return model.Value{}, failure
	})
	cursor := open(t, t.Context(), options(q, f.compile(start, stop)))
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, failure) {
		t.Fatalf("NULL hid another argument's error: %v", err)
	}
	released(t, q)
}

func TestOrdinalityRespectsChangingDemand(t *testing.T) {
	q := query(t, t.Context())
	opts := options(q, constants(t, integer, model.Int64Val(9), model.Int64Val(1), model.Int64Val(-2)))
	opts.Ordinality = true
	cursor := open(t, t.Context(), opts)
	out := columnar.Batch{MaxRows: 1}
	ordinal := int64(1)
	for _, want := range [][]int64{{9}, {7, 5}, {3, 1}} {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if got := values(t, &out); !reflect.DeepEqual(got, want) {
			t.Fatalf("batch = %v, want %v", got, want)
		}
		if len(out.Columns) != 2 || out.Columns[1].Type != integer.Type {
			t.Fatalf("ordinality layout = %+v", out.Columns)
		}
		for row := range out.Rows() {
			if out.Columns[1].IsNull(row) || out.Columns[1].Int64[row] != ordinal {
				t.Fatalf("ordinality = %v, want %d at row %d", out.Columns[1].Int64, ordinal, row)
			}
			ordinal++
		}
		out.MaxRows = 2
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("exhausted series = %v", err)
	}
	released(t, q)
}

func TestBatchBytesFlushWithoutSkippingValues(t *testing.T) {
	q := query(t, t.Context())
	opts := options(q, constants(t, integer, model.Int64Val(1), model.Int64Val(1000)))
	opts.BatchRows, opts.BatchBytes, opts.Ordinality = 1000, 2048, true
	cursor := open(t, t.Context(), opts)
	var out columnar.Batch
	next := int64(1)
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows() == 0 || out.Rows() >= 1000 {
			t.Fatalf("byte-bounded batch has %d rows", out.Rows())
		}
		if usage := q.Usage().Output.Live; usage > int64(opts.BatchBytes) {
			t.Fatalf("output retained %d bytes, bound %d", usage, opts.BatchBytes)
		}
		for _, value := range values(t, &out) {
			if value != next {
				t.Fatalf("value = %d, want %d", value, next)
			}
			next++
		}
		if next > 1001 {
			t.Fatal("series exceeded stop")
		}
	}
	if next != 1001 {
		t.Fatalf("series stopped before %d", next)
	}
	released(t, q)
}

func TestArgumentsEvaluateOnceAcrossNextContexts(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, integer)
	calls := 0
	start := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		calls++
		return model.Int64Val(int64(calls * 2)), nil
	})
	stop := f.scalar(ir.Scalar{Kind: ir.Param, Parameter: 1}, model.Value{})
	f.options.Parameters = []expr.Parameter{{Number: 1, Layout: integer}}
	opts := options(q, f.compile(start, stop))
	opts.Parameters = []expr.ParameterValue{{Number: 1, Layout: integer, Value: model.Int64Val(4)}}
	cursor := open(t, t.Context(), opts)
	if calls != 0 {
		t.Fatalf("constructor evaluated volatile argument %d times", calls)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if got := values(t, &out); !reflect.DeepEqual(got, []int64{2}) {
		t.Fatalf("first row = %v", got)
	}
	cancel()
	opts.Parameters[0].Value = model.Int64Val(99)
	for _, want := range []int64{3, 4} {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatalf("previous Next context escaped its invocation: %v", err)
		}
		if got := values(t, &out); !reflect.DeepEqual(got, []int64{want}) {
			t.Fatalf("row = %v, want %d", got, want)
		}
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF || calls != 1 {
		t.Fatalf("arguments reevaluated: terminal=%v calls=%d", err, calls)
	}
	released(t, q)
}

func TestCaptureUsesLogicalLane(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, integer)
	start := f.scalar(ir.Scalar{Kind: ir.Slot, Slot: f.slot, Nullable: true}, model.Value{})
	stop := f.constant(model.Int64Val(8))
	f.options.Captures = []expr.Input{{Slot: f.slot, Column: 0}}
	opts := options(q, f.compile(start, stop))
	capture := columnar.Batch{Length: 3, Selection: []uint32{2}, Columns: []columnar.Vector{{Type: integer.Type, Length: 3, Int64: []int64{-50, -60, 7}}}}
	opts.Captures = &capture
	cursor := open(t, t.Context(), opts)
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if got := values(t, &out); !reflect.DeepEqual(got, []int64{7, 8}) {
		t.Fatalf("captured series = %v", got)
	}
	if capture.Rows() != 1 || capture.Columns[0].Int64[2] != 7 {
		t.Fatalf("borrowed capture was modified: %+v", capture)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
}

func TestEarlyCloseSkipsEvaluationAndPreservesQuery(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, integer)
	calls := 0
	start := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		calls++
		return model.Int64Val(1), nil
	})
	stop := f.constant(model.Int64Val(math.MaxInt64))
	cursor := open(t, t.Context(), options(q, f.compile(start, stop)))
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("early close evaluated argument %d times", calls)
	}
	released(t, q)
	claim, err := q.TryReserve(runtime.WorkMemory, 1)
	if err != nil {
		t.Fatalf("cursor closed borrowed query: %v", err)
	}
	claim.Release()
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("Next after Close = %v", err)
	}
}

func TestCancellationDuringArgumentEvaluationPreservesCause(t *testing.T) {
	for _, owner := range []string{"next", "cursor", "query"} {
		t.Run(owner, func(t *testing.T) {
			next, cancelNext := context.WithCancelCause(t.Context())
			defer cancelNext(nil)
			life, cancelLife := context.WithCancelCause(t.Context())
			defer cancelLife(nil)
			queryContext, cancelQuery := context.WithCancelCause(t.Context())
			defer cancelQuery(nil)
			q := query(t, queryContext)
			failure := errors.New("canceled " + owner)
			cancel := map[string]context.CancelCauseFunc{"next": cancelNext, "cursor": cancelLife, "query": cancelQuery}[owner]
			f := newFixture(t, integer)
			start := f.function(func(ctx context.Context, _ []model.Value, _ builtin.CallBudget) (model.Value, error) {
				cancel(failure)
				<-ctx.Done()
				return model.Value{}, ctx.Err()
			})
			stop := f.constant(model.Int64Val(3))
			cursor := open(t, life, options(q, f.compile(start, stop)))
			bounded, stopWait := context.WithTimeout(next, 5*time.Second)
			defer stopWait()
			var out columnar.Batch
			if err := cursor.Next(bounded, &out); !errors.Is(err, failure) {
				t.Fatalf("cancellation lost cause: %v", err)
			}
			if out.Rows() != 0 {
				t.Fatalf("canceled source published %d rows", out.Rows())
			}
			released(t, q)
		})
	}
}

func TestOutputAllocationFailureReleasesPartialBatch(t *testing.T) {
	q := query(t, t.Context())
	opts := options(q, constants(t, integer, model.Int64Val(1), model.Int64Val(100)))
	opts.BatchRows = 64
	cursor := open(t, t.Context(), opts)
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	usage := q.Usage().Output
	blocker, err := q.TryReserve(runtime.OutputMemory, usage.Limit-usage.Live)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	out.MaxRows = 64
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("output allocation failure = %v", err)
	}
	if out.Rows() != 0 {
		t.Fatalf("failed output published %d partial rows", out.Rows())
	}
	blocker.Release()
	released(t, q)
}

func TestArgumentAllocationFailureReleasesCursor(t *testing.T) {
	q := query(t, t.Context())
	cursor := open(t, t.Context(), options(q, constants(t, integer, model.Int64Val(1), model.Int64Val(3))))
	usage := q.Usage().Work
	blocker, err := q.TryReserve(runtime.WorkMemory, usage.Limit-usage.Live)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("argument allocation failure = %v", err)
	}
	blocker.Release()
	released(t, q)
}

func TestConstructorFailureReleasesMetadata(t *testing.T) {
	q := query(t, t.Context())
	opts := options(q, constants(t, integer, model.Int64Val(1), model.Int64Val(3)))
	opts.BatchBytes = 1
	cursor, err := series.New(t.Context(), opts)
	if !errors.Is(err, columnar.ErrBatchBudget) || cursor != nil {
		t.Fatalf("unusable batch budget = %v, %v", cursor, err)
	}
	released(t, q)
}

func TestHugeRangeReleasesResourcesOnEarlyClose(t *testing.T) {
	q := query(t, t.Context())
	opts := options(q, constants(t, integer, model.Int64Val(math.MinInt64), model.Int64Val(math.MaxInt64)))
	cursor := open(t, t.Context(), opts)
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if got := values(t, &out); !reflect.DeepEqual(got, []int64{math.MinInt64}) {
		t.Fatalf("first row of huge range = %v", got)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
}

func TestInvalidDemandReleasesCursor(t *testing.T) {
	q := query(t, t.Context())
	cursor := open(t, t.Context(), options(q, constants(t, integer, model.Int64Val(1), model.Int64Val(3))))
	out := columnar.Batch{MaxRows: -1}
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("negative consumer demand = %v", err)
	}
	released(t, q)
}
