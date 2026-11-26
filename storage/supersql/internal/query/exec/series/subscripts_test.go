package series_test

import (
	"context"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
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

var (
	subscriptInteger = model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	subscriptBoolean = model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	subscriptArray   = model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{subscriptInteger}}
)

type subscriptFixture struct {
	*fixture
	dimension, reverse ir.LayoutID
}

func newSubscriptFixture(t *testing.T, layout model.TypeLayout) *subscriptFixture {
	t.Helper()
	f := &subscriptFixture{fixture: newFixture(t, layout)}
	var err error
	f.dimension, err = f.builder.AddLayout(t.Context(), subscriptInteger)
	if err != nil {
		t.Fatal(err)
	}
	f.reverse, err = f.builder.AddLayout(t.Context(), subscriptBoolean)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *subscriptFixture) dimensionValue(value model.Value) ir.ScalarID {
	return f.scalar(ir.Scalar{Kind: ir.Const, Layout: f.dimension, Nullable: value.IsNull}, value)
}

func (f *subscriptFixture) reverseValue(value model.Value) ir.ScalarID {
	return f.scalar(ir.Scalar{Kind: ir.Const, Layout: f.reverse, Nullable: value.IsNull}, value)
}

func subscriptArguments(t *testing.T, array, dimension model.Value, reverse ...model.Value) *expr.Program {
	t.Helper()
	f := newSubscriptFixture(t, subscriptArray)
	roots := []ir.ScalarID{f.constant(array), f.dimensionValue(dimension)}
	for _, value := range reverse {
		roots = append(roots, f.reverseValue(value))
	}
	return f.compile(roots...)
}

func shapedArray(dimensions ...model.ArrayDimension) model.Value {
	count := 0
	if len(dimensions) != 0 {
		count = 1
		for _, dimension := range dimensions {
			count *= int(dimension.Length)
		}
	}
	array := model.Value{Type: model.TypeArray, Dimensions: dimensions, ArrayVal: make([]model.Value, count)}
	for i := range count {
		array.ArrayVal[i] = model.NullVal(model.TypeInt64)
	}
	return array
}

func subscriptOptions(q *runtime.Query, program *expr.Program) series.Options {
	opts := options(q, program)
	opts.Layout = subscriptInteger
	return opts
}

func openSubscripts(t *testing.T, ctx context.Context, opts series.Options) storage.BatchCursor {
	t.Helper()
	cursor, err := series.NewSubscripts(ctx, opts)
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

func TestSubscriptsActualBoundsAndOrdinality(t *testing.T) {
	for _, tc := range []struct {
		name       string
		dimensions []model.ArrayDimension
		dimension  int64
		reverse    []model.Value
		want       []int64
	}{
		{"default forward", []model.ArrayDimension{{Length: 3, LowerBound: -2}}, 1, nil, []int64{-2, -1, 0}},
		{"explicit forward second dimension", []model.ArrayDimension{{Length: 2, LowerBound: 9}, {Length: 3, LowerBound: -4}}, 2, []model.Value{model.BoolVal(false)}, []int64{-4, -3, -2}},
		{"reverse second dimension", []model.ArrayDimension{{Length: 2, LowerBound: 9}, {Length: 3, LowerBound: -4}}, 2, []model.Value{model.BoolVal(true)}, []int64{-2, -3, -4}},
		{"maximum last subscript", []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32 - 1}}, 1, nil, []int64{math.MaxInt32 - 1, math.MaxInt32}},
		{"minimum descending subscript", []model.ArrayDimension{{Length: 2, LowerBound: math.MinInt32}}, 1, []model.Value{model.BoolVal(true)}, []int64{math.MinInt32 + 1, math.MinInt32}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := query(t, t.Context())
			opts := subscriptOptions(q, subscriptArguments(t, shapedArray(tc.dimensions...), model.Int64Val(tc.dimension), tc.reverse...))
			opts.Ordinality = true
			cursor := openSubscripts(t, t.Context(), opts)
			out := columnar.Batch{MaxRows: 1}
			var got []int64
			ordinal := int64(1)
			for {
				err := cursor.Next(t.Context(), &out)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if out.Rows() == 0 || out.Rows() > out.MaxRows || len(got)+out.Rows() > len(tc.want) {
					t.Fatalf("unexpected batch after %v: %v", got, values(t, &out))
				}
				if out.Columns[0].Type != subscriptInteger.Type || len(out.Columns) != 2 || out.Columns[1].Type != integer.Type {
					t.Fatalf("subscripts/ordinality layouts = %+v", out.Columns)
				}
				got = append(got, values(t, &out)...)
				for row := range out.Rows() {
					if out.Columns[1].IsNull(row) || out.Columns[1].Int64[row] != ordinal {
						t.Fatalf("ordinality row %d = %v, want %d", row, out.Columns[1].Int64, ordinal)
					}
					ordinal++
				}
				out.MaxRows = 2
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("subscripts = %v, want %v", got, tc.want)
			}
			released(t, q)
		})
	}
}

func TestSubscriptsEmptyNullAndInvalidDimensions(t *testing.T) {
	array := shapedArray(model.ArrayDimension{Length: 2, LowerBound: -5})
	for _, tc := range []struct {
		name             string
		array, dimension model.Value
		reverse          []model.Value
	}{
		{"empty array", shapedArray(), model.Int64Val(1), nil},
		{"null array", model.NullVal(model.TypeArray), model.Int64Val(1), nil},
		{"null dimension", array, model.NullVal(model.TypeInt64), nil},
		{"null reverse", array, model.Int64Val(1), []model.Value{model.NullVal(model.TypeBoolean)}},
		{"zero dimension", array, model.Int64Val(0), nil},
		{"negative dimension", array, model.Int64Val(math.MinInt32), nil},
		{"dimension beyond rank", array, model.Int64Val(2), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := query(t, t.Context())
			cursor := openSubscripts(t, t.Context(), subscriptOptions(q, subscriptArguments(t, tc.array, tc.dimension, tc.reverse...)))
			var out columnar.Batch
			if err := cursor.Next(t.Context(), &out); err != io.EOF || out.Rows() != 0 {
				t.Fatalf("empty subscripts = %v, rows=%d", err, out.Rows())
			}
			released(t, q)
		})
	}
}

func TestSubscriptsEvaluatesArgumentsInOrderBeforeStrictNull(t *testing.T) {
	for _, failAt := range []int{-1, 1, 2} {
		t.Run(map[int]string{-1: "all arguments", 1: "dimension error", 2: "reverse error"}[failAt], func(t *testing.T) {
			q := query(t, t.Context())
			f := newSubscriptFixture(t, subscriptArray)
			failure := errors.New("argument failed")
			var calls []int
			layouts := []ir.LayoutID{f.layout, f.dimension, f.reverse}
			results := []model.Value{model.NullVal(model.TypeArray), model.Int64Val(1), model.BoolVal(true)}
			roots := make([]ir.ScalarID, len(layouts))
			for i, layout := range layouts {
				roots[i] = f.functionWithLayout(layout, func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls = append(calls, i)
					if i == failAt {
						return model.Value{}, failure
					}
					return results[i], nil
				})
			}
			cursor := openSubscripts(t, t.Context(), subscriptOptions(q, f.compile(roots...)))
			if len(calls) != 0 {
				t.Fatalf("constructor evaluated arguments: %v", calls)
			}
			var out columnar.Batch
			err := cursor.Next(t.Context(), &out)
			want := []int{0, 1, 2}
			if failAt >= 0 {
				want = want[:failAt+1]
				if !errors.Is(err, failure) {
					t.Fatalf("NULL suppressed later error: %v", err)
				}
			} else if err != io.EOF {
				t.Fatalf("strict NULL = %v, want EOF", err)
			}
			if !reflect.DeepEqual(calls, want) || out.Rows() != 0 {
				t.Fatalf("evaluation order=%v, want %v; rows=%d", calls, want, out.Rows())
			}
			released(t, q)
		})
	}
}

func TestSubscriptsSnapshotsArgumentsAcrossNextContexts(t *testing.T) {
	q := query(t, t.Context())
	f := newSubscriptFixture(t, subscriptArray)
	calls := 0
	array := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		calls++
		return shapedArray(model.ArrayDimension{Length: 3, LowerBound: int32(-calls)}), nil
	})
	dimension := f.scalar(ir.Scalar{Kind: ir.Param, Layout: f.dimension, Parameter: 1}, model.Value{})
	reverse := f.scalar(ir.Scalar{Kind: ir.Param, Layout: f.reverse, Parameter: 2}, model.Value{})
	f.options.Parameters = []expr.Parameter{{Number: 1, Layout: subscriptInteger}, {Number: 2, Layout: subscriptBoolean}}
	opts := subscriptOptions(q, f.compile(array, dimension, reverse))
	opts.Parameters = []expr.ParameterValue{{Number: 1, Layout: subscriptInteger, Value: model.Int64Val(1)}, {Number: 2, Layout: subscriptBoolean, Value: model.BoolVal(true)}}
	cursor := openSubscripts(t, t.Context(), opts)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(ctx, &out); err != nil {
		t.Fatal(err)
	}
	if got := values(t, &out); !reflect.DeepEqual(got, []int64{1}) {
		t.Fatalf("first subscript = %v", got)
	}
	cancel()
	opts.Parameters[0].Value = model.Int64Val(9)
	opts.Parameters[1].Value = model.BoolVal(false)
	for _, want := range []int64{0, -1} {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatalf("previous Next context escaped invocation: %v", err)
		}
		if got := values(t, &out); !reflect.DeepEqual(got, []int64{want}) {
			t.Fatalf("subscript = %v, want %d", got, want)
		}
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF || calls != 1 {
		t.Fatalf("terminal=%v array evaluations=%d, want EOF and one evaluation", err, calls)
	}
	released(t, q)
}

func TestSubscriptsReleasesArrayBeforeFirstBatch(t *testing.T) {
	q := query(t, t.Context())
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	arrayLayout := model.TypeLayout{Type: model.Type{ID: 1009, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{text}}
	f := newSubscriptFixture(t, arrayLayout)
	array := f.scalar(ir.Scalar{Kind: ir.Param, Parameter: 1}, model.Value{})
	dimension := f.dimensionValue(model.Int64Val(1))
	f.options.Parameters = []expr.Parameter{{Number: 1, Layout: arrayLayout}}
	opts := subscriptOptions(q, f.compile(array, dimension))
	payload := strings.Repeat("x", 64<<10)
	value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: -2}}, ArrayVal: []model.Value{model.StringVal(payload), model.StringVal(payload), model.StringVal(payload)}}
	opts.Parameters = []expr.ParameterValue{{Number: 1, Layout: arrayLayout, Value: value}}
	cursor := openSubscripts(t, t.Context(), opts)
	usage := q.Usage().Work
	blocker, err := q.TryReserve(runtime.WorkMemory, usage.Limit-usage.Live-(32<<10))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatalf("reading dimensions materialized the payload: %v", err)
	}
	if got := values(t, &out); !reflect.DeepEqual(got, []int64{-2}) {
		t.Fatalf("first subscript = %v", got)
	}
	blocker.Release()
	if live := q.Usage().Work.Live; live >= 32<<10 {
		t.Fatalf("first batch retained %d work bytes after consuming array shape", live)
	}
	clear(value.ArrayVal)
	value.Dimensions[0] = model.ArrayDimension{Length: 99, LowerBound: 44}
	opts.Parameters[0].Value = model.NullVal(model.TypeArray)
	for _, want := range []int64{-1, 0} {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if got := values(t, &out); !reflect.DeepEqual(got, []int64{want}) {
			t.Fatalf("released argument changed remaining subscript: %v, want %d", got, want)
		}
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
}

func TestSubscriptsByteBoundedBatches(t *testing.T) {
	q := query(t, t.Context())
	opts := subscriptOptions(q, subscriptArguments(t, shapedArray(model.ArrayDimension{Length: 1000, LowerBound: -20}), model.Int64Val(1)))
	opts.BatchRows, opts.BatchBytes, opts.Ordinality = 1000, 2048, true
	cursor := openSubscripts(t, t.Context(), opts)
	var out columnar.Batch
	next := int64(-20)
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows() == 0 || out.Rows() >= opts.BatchRows {
			t.Fatalf("byte-bounded batch rows=%d", out.Rows())
		}
		if live := q.Usage().Output.Live; live > int64(opts.BatchBytes) {
			t.Fatalf("output retained %d bytes, bound %d", live, opts.BatchBytes)
		}
		for _, value := range values(t, &out) {
			if value != next || value >= 980 {
				t.Fatalf("subscript=%d, want %d below 980", value, next)
			}
			next++
		}
	}
	if next != 980 {
		t.Fatalf("subscripts stopped at %d, want 980", next)
	}
	released(t, q)
}

func TestSubscriptsEarlyCloseSkipsArguments(t *testing.T) {
	q := query(t, t.Context())
	f := newSubscriptFixture(t, subscriptArray)
	calls := 0
	array := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		calls++
		return shapedArray(model.ArrayDimension{Length: 1, LowerBound: 1}), nil
	})
	dimension := f.dimensionValue(model.Int64Val(1))
	cursor := openSubscripts(t, t.Context(), subscriptOptions(q, f.compile(array, dimension)))
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("closing before demand evaluated array %d times", calls)
	}
	released(t, q)
}

func TestSubscriptsCancellationReleasesArguments(t *testing.T) {
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
			f := newSubscriptFixture(t, subscriptArray)
			array := f.function(func(ctx context.Context, _ []model.Value, _ builtin.CallBudget) (model.Value, error) {
				cancel(failure)
				<-ctx.Done()
				return model.Value{}, ctx.Err()
			})
			dimension := f.dimensionValue(model.Int64Val(1))
			cursor := openSubscripts(t, life, subscriptOptions(q, f.compile(array, dimension)))
			bounded, stopWait := context.WithTimeout(next, 5*time.Second)
			defer stopWait()
			var out columnar.Batch
			if err := cursor.Next(bounded, &out); !errors.Is(err, failure) || out.Rows() != 0 {
				t.Fatalf("cancellation=%v rows=%d, want cause and no output", err, out.Rows())
			}
			released(t, q)
		})
	}
}

func TestSubscriptsAllocationFailureReleasesCursor(t *testing.T) {
	for _, memory := range []runtime.MemoryKind{runtime.WorkMemory, runtime.OutputMemory} {
		t.Run(map[runtime.MemoryKind]string{runtime.WorkMemory: "arguments", runtime.OutputMemory: "output"}[memory], func(t *testing.T) {
			q := query(t, t.Context())
			opts := subscriptOptions(q, subscriptArguments(t, shapedArray(model.ArrayDimension{Length: 100, LowerBound: 1}), model.Int64Val(1)))
			opts.BatchRows = 64
			cursor := openSubscripts(t, t.Context(), opts)
			out := columnar.Batch{MaxRows: 1}
			usage := q.Usage().Work
			if memory == runtime.OutputMemory {
				if err := cursor.Next(t.Context(), &out); err != nil {
					t.Fatal(err)
				}
				usage = q.Usage().Output
				out.MaxRows = 64
			}
			blocker, err := q.TryReserve(memory, usage.Limit-usage.Live)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Release()
			if err := cursor.Next(t.Context(), &out); !errors.Is(err, runtime.ErrBudget) || out.Rows() != 0 {
				t.Fatalf("allocation failure=%v rows=%d, want budget error and no output", err, out.Rows())
			}
			blocker.Release()
			released(t, q)
		})
	}
}

func TestSubscriptsCapturedArrayUsesLogicalLane(t *testing.T) {
	q := query(t, t.Context())
	f := newSubscriptFixture(t, subscriptArray)
	array := f.scalar(ir.Scalar{Kind: ir.Slot, Slot: f.slot, Nullable: true}, model.Value{})
	dimension := f.dimensionValue(model.Int64Val(1))
	f.options.Captures = []expr.Input{{Slot: f.slot, Column: 0}}
	opts := subscriptOptions(q, f.compile(array, dimension))
	builder, err := columnar.NewBuilder([]model.TypeLayout{subscriptArray}, columnar.BuilderOptions{MaxRows: 2, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer builder.Close()
	for _, lower := range []int32{-10, 7} {
		if err := builder.Append(t.Context(), []model.Value{shapedArray(model.ArrayDimension{Length: 3, LowerBound: lower})}); err != nil {
			t.Fatal(err)
		}
	}
	var capture columnar.Batch
	if err := builder.Publish(&capture); err != nil {
		t.Fatal(err)
	}
	capture.Selection = []uint32{1}
	opts.Captures = &capture
	cursor := openSubscripts(t, t.Context(), opts)
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if got := values(t, &out); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("captured subscript=%v, want selected lane lower bound 7", got)
	}
	if err := builder.Close(); err != nil {
		t.Fatal(err)
	}
	capture.Reset()
	for _, want := range []int64{8, 9} {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		if got := values(t, &out); !reflect.DeepEqual(got, []int64{want}) {
			t.Fatalf("released capture changed remaining subscript: %v, want %d", got, want)
		}
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
}

func TestSubscriptsLifetimeCancellationAfterFirstBatch(t *testing.T) {
	q := query(t, t.Context())
	life, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	opts := subscriptOptions(q, subscriptArguments(t, shapedArray(model.ArrayDimension{Length: 4, LowerBound: -3}), model.Int64Val(1)))
	cursor := openSubscripts(t, life, opts)
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("cursor lifetime ended")
	cancel(failure)
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, failure) || out.Rows() != 0 {
		t.Fatalf("expired cursor lifetime=%v rows=%d", err, out.Rows())
	}
	released(t, q)
}

func TestSubscriptsRejectsWrongCompiledTypes(t *testing.T) {
	for _, mismatch := range []string{"array", "dimension", "reverse", "output"} {
		t.Run(mismatch, func(t *testing.T) {
			q := query(t, t.Context())
			f := newSubscriptFixture(t, subscriptArray)
			array := f.constant(shapedArray(model.ArrayDimension{Length: 1, LowerBound: 1}))
			dimension := f.dimensionValue(model.Int64Val(1))
			reverse := f.reverseValue(model.BoolVal(false))
			roots := []ir.ScalarID{array, dimension, reverse}
			switch mismatch {
			case "array":
				roots[0] = dimension
			case "dimension":
				layout, err := f.builder.AddLayout(t.Context(), integer)
				if err != nil {
					t.Fatal(err)
				}
				roots[1] = f.scalar(ir.Scalar{Kind: ir.Const, Layout: layout}, model.Int64Val(1))
			case "reverse":
				roots[2] = dimension
			}
			opts := subscriptOptions(q, f.compile(roots...))
			if mismatch == "output" {
				opts.Layout = integer
			}
			cursor, err := series.NewSubscripts(t.Context(), opts)
			if cursor != nil {
				if closeErr := cursor.Close(); closeErr != nil {
					t.Error(closeErr)
				}
				t.Fatal("constructor accepted mismatched compiled types")
			}
			if !errors.Is(err, columnar.ErrValueLayout) {
				t.Fatalf("type mismatch=%v, want layout error", err)
			}
			released(t, q)
		})
	}
}

func TestSubscriptsReleasesComputedArrayBeforeFirstBatch(t *testing.T) {
	q := query(t, t.Context())
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}}
	arrayLayout := model.TypeLayout{Type: model.Type{ID: 1009, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{text}}
	f := newSubscriptFixture(t, arrayLayout)
	array := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		payload := strings.Repeat("x", 64<<10)
		return model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: 5}}, ArrayVal: []model.Value{model.StringVal(payload), model.StringVal(payload), model.StringVal(payload)}}, nil
	})
	dimension := f.dimensionValue(model.Int64Val(1))
	cursor := openSubscripts(t, t.Context(), subscriptOptions(q, f.compile(array, dimension)))
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	if got := values(t, &out); !reflect.DeepEqual(got, []int64{5}) {
		t.Fatalf("first subscript=%v, want 5", got)
	}
	if live := q.Usage().Work.Live; live >= 32<<10 {
		t.Fatalf("stream retained %d work bytes after consuming computed array shape", live)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
	claim, err := q.TryReserve(runtime.WorkMemory, 1)
	if err != nil {
		t.Fatalf("early close closed borrowed query: %v", err)
	}
	claim.Release()
}
