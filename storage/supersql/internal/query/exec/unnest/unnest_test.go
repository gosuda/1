package unnest_test

import (
	"context"
	"errors"
	"io"
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
	"github.com/gosuda/supersql-alpha/internal/query/exec/unnest"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var (
	integer = model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	bigint  = model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	text    = model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}}
	bytea   = model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
)

func arrayLayout(element model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: 900100, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{element}}
}

func recordLayout(fields ...model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: fields}
}

func array(values ...model.Value) model.Value {
	value := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		value.Dimensions = []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}
	}
	return value
}

func record(values ...model.Value) model.Value {
	return model.Value{Type: model.TypeComposite, ArrayVal: values}
}

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
	slot    model.SlotID
	options expr.Options
	call    expr.ScalarCall
}

func newFixture(t *testing.T, layout model.TypeLayout) *fixture {
	t.Helper()
	builder, err := ir.NewBuilder(ir.Limits{MaxNodes: 128, MaxOperands: 256, MaxSlots: 16, MaxDepth: 16, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, builder: builder, options: expr.Options{Limits: expr.DefaultLimits()}}
	f.scope, err = builder.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.layout, err = builder.AddLayout(t.Context(), layout)
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
	scalar.Scope, scalar.Layout = f.scope, f.layout
	id, err := f.builder.AddScalar(f.t.Context(), ir.ScalarSpec{Scalar: scalar, Value: value})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) function(call expr.ScalarCall) ir.ScalarID {
	f.t.Helper()
	f.call = call
	effects := ir.Effects{Volatility: ir.Volatile, MayError: true}
	signature, err := f.builder.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 900000, Result: f.layout, Nullable: true, Effects: effects}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.scalar(ir.Scalar{Kind: ir.Call, Function: 900000, Signature: signature, Nullable: true, Effects: effects}, model.Value{})
}

func (f *fixture) Resolve(expr.Function) (expr.ScalarCall, error) { return f.call, nil }

func (f *fixture) compile(root ir.ScalarID) *expr.Program {
	f.t.Helper()
	relation, err := f.builder.AddRel(f.t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: 1, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{f.slot}})
	if err != nil {
		f.t.Fatal(err)
	}
	graph, err := f.builder.Build(f.t.Context(), relation)
	if err != nil {
		f.t.Fatal(err)
	}
	f.options.Functions = f
	program, err := expr.Compile(f.t.Context(), graph, []ir.ScalarID{root}, f.options)
	if err != nil {
		f.t.Fatal(err)
	}
	return program
}

func options(t *testing.T, q *runtime.Query, element model.TypeLayout, value model.Value) unnest.Options {
	t.Helper()
	f := newFixture(t, arrayLayout(element))
	root := f.scalar(ir.Scalar{Kind: ir.Const, Nullable: value.IsNull}, value)
	output := []model.TypeLayout{element}
	if element.Type.Storage == model.TypeComposite {
		output = element.Children
	}
	return unnest.Options{Arguments: f.compile(root), Element: element, Output: output, Query: q, BatchRows: 3, BatchBytes: 16 << 10}
}

func open(t *testing.T, ctx context.Context, options unnest.Options) storage.BatchCursor {
	t.Helper()
	cursor, err := unnest.New(ctx, options)
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

func rows(t *testing.T, batch *columnar.Batch) [][]model.Value {
	t.Helper()
	if err := batch.Validate(max(1, batch.Length)); err != nil {
		t.Fatal(err)
	}
	result := make([][]model.Value, batch.Rows())
	for i := range result {
		result[i] = make([]model.Value, len(batch.Columns))
		for j := range batch.Columns {
			value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[j], batch.Lane(i), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			result[i][j] = value
		}
	}
	return result
}

func requireRows(t *testing.T, batch *columnar.Batch, want [][]model.Value) {
	t.Helper()
	if got := rows(t, batch); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v, want %v", got, want)
	}
}

func released(t *testing.T, q *runtime.Query) {
	t.Helper()
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("cursor retained query resources: %+v", usage)
	}
}

func TestScalarRowMajorDemandOrdinality(t *testing.T) {
	q := query(t, t.Context())
	value := array(model.Int64Val(8), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4), model.Int64Val(5), model.Int64Val(6))
	value.Dimensions = []model.ArrayDimension{{Length: 2, LowerBound: -4}, {Length: 3, LowerBound: 17}}
	opts := options(t, q, integer, value)
	opts.Ordinality, opts.Output = true, []model.TypeLayout{integer, bigint}
	cursor := open(t, t.Context(), opts)
	var out columnar.Batch
	want := [][]model.Value{
		{model.Int64Val(8), model.Int64Val(1)}, {model.NullVal(model.TypeInt64), model.Int64Val(2)},
		{model.Int64Val(3), model.Int64Val(3)}, {model.Int64Val(4), model.Int64Val(4)},
		{model.Int64Val(5), model.Int64Val(5)}, {model.Int64Val(6), model.Int64Val(6)},
	}
	start := 0
	for _, demand := range []int{1, 2, 10} {
		out.MaxRows = demand
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		count := min(demand, 3)
		requireRows(t, &out, want[start:start+count])
		start += count
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF || out.Rows() != 0 {
		t.Fatalf("exhausted cursor=%v rows=%d", err, out.Rows())
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatalf("repeated EOF=%v", err)
	}
	released(t, q)
}

func TestRecordSchemaChecksFirstNonnullRecord(t *testing.T) {
	varchar := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 7, Collation: 100, Storage: model.TypeString}}
	otherTypmod := varchar
	otherTypmod.Type.Modifier = 9
	unconstrained := varchar
	unconstrained.Type.Modifier, unconstrained.Type.Collation = -1, 950
	for _, tc := range []struct {
		name     string
		actual   model.TypeLayout
		output   []model.TypeLayout
		value    model.Value
		mismatch bool
	}{
		{"matching", recordLayout(integer), []model.TypeLayout{integer}, record(model.Int64Val(7)), false},
		{"sql identity", recordLayout(integer), []model.TypeLayout{bigint}, record(model.Int64Val(7)), true},
		{"field count", recordLayout(integer), []model.TypeLayout{integer, text}, record(model.Int64Val(7)), true},
		{"typmod", recordLayout(varchar), []model.TypeLayout{otherTypmod}, record(model.StringVal("abc")), true},
		{"unspecified typmod and collation", recordLayout(varchar), []model.TypeLayout{unconstrained}, record(model.StringVal("abc")), false},
		{"nested identity", recordLayout(arrayLayout(integer)), []model.TypeLayout{arrayLayout(bigint)}, record(array(model.Int64Val(7))), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := query(t, t.Context())
			opts := options(t, q, tc.actual, array(model.NullVal(model.TypeComposite), tc.value))
			opts.Output = tc.output
			cursor := open(t, t.Context(), opts)
			out := columnar.Batch{MaxRows: 1}
			err := cursor.Next(t.Context(), &out)
			if tc.mismatch {
				var sqlError *model.Error
				if !errors.As(err, &sqlError) || sqlError.Code != "42804" || out.Rows() != 0 {
					t.Fatalf("record mismatch=%v rows=%d, want 42804 without output", err, out.Rows())
				}
				released(t, q)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			nulls := make([]model.Value, len(tc.output))
			for i := range nulls {
				nulls[i] = model.NullVal(tc.output[i].Type.Storage)
			}
			requireRows(t, &out, [][]model.Value{nulls})
			if err := cursor.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			requireRows(t, &out, [][]model.Value{tc.value.ArrayVal})
		})
	}
}

func TestNullAndEmptyArraysBypassRecordMismatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value model.Value
		rows  int
	}{
		{"null array", model.NullVal(model.TypeArray), 0},
		{"empty array", array(), 0},
		{"null record", array(model.NullVal(model.TypeComposite)), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := query(t, t.Context())
			opts := options(t, q, recordLayout(integer), tc.value)
			opts.Output = []model.TypeLayout{bigint, text}
			cursor := open(t, t.Context(), opts)
			var out columnar.Batch
			err := cursor.Next(t.Context(), &out)
			if tc.rows == 1 {
				if err != nil {
					t.Fatal(err)
				}
				requireRows(t, &out, [][]model.Value{{model.NullVal(model.TypeInt64), model.NullVal(model.TypeString)}})
				err = cursor.Next(t.Context(), &out)
			}
			if err != io.EOF || out.Rows() != 0 {
				t.Fatalf("empty input=%v rows=%d", err, out.Rows())
			}
			released(t, q)
		})
	}
}

func TestZeroColumnNamedRecords(t *testing.T) {
	for _, ordinality := range []bool{false, true} {
		t.Run(map[bool]string{false: "no columns", true: "ordinal only"}[ordinality], func(t *testing.T) {
			q := query(t, t.Context())
			element := recordLayout()
			element.Type.ID = 900200
			opts := options(t, q, element, array(record(), model.NullVal(model.TypeComposite), record()))
			opts.Ordinality = ordinality
			if ordinality {
				opts.Output = []model.TypeLayout{bigint}
			}
			cursor := open(t, t.Context(), opts)
			out := columnar.Batch{MaxRows: 2}
			want := [][]model.Value{{}, {}, {}}
			if ordinality {
				want = [][]model.Value{{model.Int64Val(1)}, {model.Int64Val(2)}, {model.Int64Val(3)}}
			}
			if err := cursor.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			requireRows(t, &out, want[:2])
			if err := cursor.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			requireRows(t, &out, want[2:])
		})
	}
}

func TestRetainedNestedAndBytePayloadsSurviveCursor(t *testing.T) {
	q := query(t, t.Context())
	nested := arrayLayout(bytea)
	nestedValue := array(model.BytesVal([]byte{0, 255}), model.NullVal(model.TypeBytes))
	nestedValue.Dimensions[0].LowerBound = -7
	for _, element := range []model.TypeLayout{nested, recordLayout(bytea, nested)} {
		value := nestedValue
		if element.Type.Storage == model.TypeComposite {
			value = record(model.BytesVal([]byte{1, 0, 254}), nestedValue)
		}
		opts := options(t, q, element, array(value, model.NullVal(element.Type.Storage), value))
		cursor := open(t, t.Context(), opts)
		out := columnar.Batch{MaxRows: 1}
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		want := [][]model.Value{{value}}
		if element.Type.Storage == model.TypeComposite {
			want = [][]model.Value{value.ArrayVal}
		}
		requireRows(t, &out, want)
		retained, err := expr.Retain(t.Context(), &out, opts.Output, expr.RetainOptions{MaxRows: 1, MaxBytes: 16 << 10})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := retained.Close(); err != nil {
				t.Error(err)
			}
		})
		for range 2 {
			if err := cursor.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
		}
		if err := cursor.Close(); err != nil {
			t.Fatal(err)
		}
		released(t, q)
		if err := retained.Publish(&out); err != nil {
			t.Fatal(err)
		}
		requireRows(t, &out, want)
	}
}

func TestByteBackpressureDoesNotSkipPendingRow(t *testing.T) {
	q := query(t, t.Context())
	values := make([]model.Value, 40)
	for i := range values {
		values[i] = model.StringVal(strings.Repeat(string(rune('a'+i%26)), 128+i))
	}
	opts := options(t, q, text, array(values...))
	opts.BatchRows, opts.BatchBytes = 40, 2048
	opts.Ordinality, opts.Output = true, []model.TypeLayout{text, bigint}
	cursor := open(t, t.Context(), opts)
	var out columnar.Batch
	next, batches := 0, 0
	for {
		err := cursor.Next(t.Context(), &out)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		batches++
		if out.Rows() == 0 || next+out.Rows() > len(values) {
			t.Fatalf("invalid stream progress rows=%d next=%d", out.Rows(), next)
		}
		if live := q.Usage().Output.Live; live > int64(opts.BatchBytes) {
			t.Fatalf("output bytes=%d exceed %d", live, opts.BatchBytes)
		}
		for _, row := range rows(t, &out) {
			if row[0].StrVal != values[next].StrVal || row[1].IntVal != int64(next+1) {
				t.Fatalf("row %d=%v", next, row)
			}
			next++
		}
	}
	if next != len(values) || batches < 2 {
		t.Fatalf("stream returned %d values in %d batches", next, batches)
	}
	released(t, q)
}

func TestOversizedRowReleasesPartialOutput(t *testing.T) {
	q := query(t, t.Context())
	opts := options(t, q, text, array(model.StringVal("small"), model.StringVal(strings.Repeat("x", 8192))))
	opts.BatchBytes = 2048
	cursor := open(t, t.Context(), opts)
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{model.StringVal("small")}})
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, columnar.ErrBatchBudget) || out.Rows() != 0 {
		t.Fatalf("oversized row=%v rows=%d", err, out.Rows())
	}
	released(t, q)
}

func TestEvaluationIsLazyAndNextContextIsCallScoped(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, arrayLayout(integer))
	calls := 0
	root := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		calls++
		return array(model.Int64Val(7), model.Int64Val(8), model.Int64Val(9)), nil
	})
	opts := unnest.Options{Arguments: f.compile(root), Element: integer, Output: []model.TypeLayout{integer}, Query: q, BatchRows: 3, BatchBytes: 4096}
	life, cancelLife := context.WithCancelCause(t.Context())
	defer cancelLife(nil)
	cursor := open(t, life, opts)
	if calls != 0 {
		t.Fatalf("constructor evaluated argument %d times", calls)
	}
	first, cancelFirst := context.WithCancel(t.Context())
	out := columnar.Batch{MaxRows: 1}
	if err := cursor.Next(first, &out); err != nil {
		t.Fatal(err)
	}
	cancelFirst()
	requireRows(t, &out, [][]model.Value{{model.Int64Val(7)}})
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{model.Int64Val(8)}})
	if calls != 1 {
		t.Fatalf("argument evaluated %d times", calls)
	}
	failure := errors.New("cursor canceled after successful demand")
	cancelLife(failure)
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, failure) || out.Rows() != 0 {
		t.Fatalf("lifetime cancellation=%v rows=%d", err, out.Rows())
	}
	released(t, q)
}

func TestEarlyCloseNeverEvaluatesArguments(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, arrayLayout(integer))
	calls := 0
	root := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		calls++
		return array(model.Int64Val(1)), nil
	})
	cursor := open(t, t.Context(), unnest.Options{Arguments: f.compile(root), Element: integer, Output: []model.TypeLayout{integer}, Query: q, BatchRows: 1, BatchBytes: 4096})
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("early close evaluated argument %d times", calls)
	}
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, runtime.ErrClosed) {
		t.Fatalf("closed cursor=%v", err)
	}
	released(t, q)
	claim, err := q.TryReserve(runtime.WorkMemory, 1)
	if err != nil {
		t.Fatalf("cursor closed borrowed query: %v", err)
	}
	claim.Release()
}

func TestComputedArgumentsReleaseAtLastRow(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, arrayLayout(text))
	payload := strings.Repeat("x", 64<<10)
	root := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		return array(model.StringVal(payload), model.StringVal(payload), model.StringVal(payload)), nil
	})
	opts := unnest.Options{Arguments: f.compile(root), Element: text, Output: []model.TypeLayout{text}, Query: q, BatchRows: 1, BatchBytes: 128 << 10}
	cursor := open(t, t.Context(), opts)
	out := columnar.Batch{MaxRows: 1}
	for i := range 3 {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatal(err)
		}
		requireRows(t, &out, [][]model.Value{{model.StringVal(payload)}})
		if live := q.Usage().Work.Live; i < 2 && live < 192<<10 {
			t.Fatalf("argument ownership retired before remaining rows: %d bytes", live)
		} else if i == 2 && live >= 32<<10 {
			t.Fatalf("exhausted array retained %d work bytes", live)
		}
	}
	if err := cursor.Next(t.Context(), &out); err != io.EOF {
		t.Fatal(err)
	}
	released(t, q)
}

func TestEarlyCloseReleasesComputedArray(t *testing.T) {
	q := query(t, t.Context())
	f := newFixture(t, arrayLayout(text))
	payload := strings.Repeat("x", 64<<10)
	root := f.function(func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
		return array(model.StringVal(payload), model.StringVal(payload)), nil
	})
	cursor := open(t, t.Context(), unnest.Options{Arguments: f.compile(root), Element: text, Output: []model.TypeLayout{text}, Query: q, BatchRows: 1, BatchBytes: 128 << 10})
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{model.StringVal(payload)}})
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
}

func TestParameterArrayIsNotCopiedToStream(t *testing.T) {
	q := query(t, t.Context())
	layout := arrayLayout(text)
	f := newFixture(t, layout)
	f.options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
	f.options.Limits.MaxOutputBytes = 1
	root := f.scalar(ir.Scalar{Kind: ir.Param, Parameter: 1}, model.Value{})
	payload := strings.Repeat("x", 64<<10)
	parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: array(model.StringVal(payload), model.StringVal(payload), model.StringVal(payload))}}
	opts := unnest.Options{Arguments: f.compile(root), Element: text, Output: []model.TypeLayout{text}, Parameters: parameters, Query: q, BatchRows: 1, BatchBytes: 128 << 10}
	cursor := open(t, t.Context(), opts)
	usage := q.Usage().Work
	blocker, err := q.TryReserve(runtime.WorkMemory, usage.Limit-usage.Live-(32<<10))
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	var out columnar.Batch
	for range 3 {
		if err := cursor.Next(t.Context(), &out); err != nil {
			t.Fatalf("stream copied array payload: %v", err)
		}
		requireRows(t, &out, [][]model.Value{{model.StringVal(payload)}})
	}
	blocker.Release()
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
}

func TestCancellationDuringEvaluationPreservesAllCauses(t *testing.T) {
	for _, owner := range []string{"next", "cursor", "query"} {
		t.Run(owner, func(t *testing.T) {
			next, cancelNext := context.WithCancelCause(t.Context())
			defer cancelNext(nil)
			life, cancelLife := context.WithCancelCause(t.Context())
			defer cancelLife(nil)
			queryContext, cancelQuery := context.WithCancelCause(t.Context())
			defer cancelQuery(nil)
			q := query(t, queryContext)
			failure, callFailure := errors.New("canceled "+owner), errors.New("array evaluation failed")
			cancel := map[string]context.CancelCauseFunc{"next": cancelNext, "cursor": cancelLife, "query": cancelQuery}[owner]
			f := newFixture(t, arrayLayout(integer))
			root := f.function(func(ctx context.Context, _ []model.Value, _ builtin.CallBudget) (model.Value, error) {
				cancel(failure)
				<-ctx.Done()
				return model.Value{}, callFailure
			})
			cursor := open(t, life, unnest.Options{Arguments: f.compile(root), Element: integer, Output: []model.TypeLayout{integer}, Query: q, BatchRows: 1, BatchBytes: 4096})
			bounded, stop := context.WithTimeout(next, 5*time.Second)
			defer stop()
			var out columnar.Batch
			if err := cursor.Next(bounded, &out); !errors.Is(err, failure) || !errors.Is(err, callFailure) || out.Rows() != 0 {
				t.Fatalf("cancellation=%v rows=%d, want both causes and no output", err, out.Rows())
			}
			released(t, q)
		})
	}
}

func TestAllocationFailuresReleaseCursor(t *testing.T) {
	for _, memory := range []runtime.MemoryKind{runtime.WorkMemory, runtime.OutputMemory} {
		t.Run(map[runtime.MemoryKind]string{runtime.WorkMemory: "arguments", runtime.OutputMemory: "output"}[memory], func(t *testing.T) {
			q := query(t, t.Context())
			opts := options(t, q, integer, array(model.Int64Val(1), model.Int64Val(2)))
			cursor := open(t, t.Context(), opts)
			usage := q.Usage().Work
			if memory == runtime.OutputMemory {
				usage = q.Usage().Output
			}
			blocker, err := q.TryReserve(memory, usage.Limit-usage.Live)
			if err != nil {
				t.Fatal(err)
			}
			defer blocker.Release()
			var out columnar.Batch
			if err := cursor.Next(t.Context(), &out); !errors.Is(err, runtime.ErrBudget) || out.Rows() != 0 {
				t.Fatalf("allocation failure=%v rows=%d", err, out.Rows())
			}
			blocker.Release()
			released(t, q)
		})
	}
}

func TestConstructorFailureAndInvalidDemandReleaseResources(t *testing.T) {
	q := query(t, t.Context())
	opts := options(t, q, integer, array(model.Int64Val(1)))
	opts.BatchBytes = 1
	if cursor, err := unnest.New(t.Context(), opts); !errors.Is(err, columnar.ErrBatchBudget) || cursor != nil {
		t.Fatalf("undersized output metadata=%v cursor=%v", err, cursor)
	}
	released(t, q)
	opts.BatchBytes = 4096
	cursor := open(t, t.Context(), opts)
	out := columnar.Batch{MaxRows: -1}
	if err := cursor.Next(t.Context(), &out); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("negative demand=%v", err)
	}
	released(t, q)
}

func TestCapturedArrayUsesSelectedLogicalRow(t *testing.T) {
	q := query(t, t.Context())
	layout := arrayLayout(integer)
	f := newFixture(t, layout)
	root := f.scalar(ir.Scalar{Kind: ir.Slot, Slot: f.slot, Nullable: true}, model.Value{})
	f.options.Captures = []expr.Input{{Slot: f.slot, Column: 0}}
	builder, err := columnar.NewBuilder([]model.TypeLayout{layout}, columnar.BuilderOptions{MaxRows: 2, MaxBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer builder.Close()
	for _, value := range []model.Value{array(model.Int64Val(1)), array(model.Int64Val(8), model.NullVal(model.TypeInt64), model.Int64Val(9))} {
		if err := builder.Append(t.Context(), []model.Value{value}); err != nil {
			t.Fatal(err)
		}
	}
	var captures columnar.Batch
	if err := builder.Publish(&captures); err != nil {
		t.Fatal(err)
	}
	captures.Selection = []uint32{1}
	cursor := open(t, t.Context(), unnest.Options{Arguments: f.compile(root), Element: integer, Output: []model.TypeLayout{integer}, Captures: &captures, Query: q, BatchRows: 2, BatchBytes: 4096})
	var out columnar.Batch
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{model.Int64Val(8)}, {model.NullVal(model.TypeInt64)}})
	if err := cursor.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	requireRows(t, &out, [][]model.Value{{model.Int64Val(9)}})
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q)
}

func TestConstructorRejectsMismatchedElementDescriptor(t *testing.T) {
	q := query(t, t.Context())
	config := options(t, q, integer, array(model.Int64Val(7)))
	config.Element, config.Output = bigint, []model.TypeLayout{bigint}
	cursor, err := unnest.New(t.Context(), config)
	if cursor != nil {
		if closeErr := cursor.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("substituted array element descriptor: %v", err)
	}
	released(t, q)
}
