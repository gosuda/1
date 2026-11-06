package aggregate_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func arrayLayout(t *testing.T, child model.TypeLayout) model.TypeLayout {
	t.Helper()
	typ, ok := builtin.LookupType(child.Type.ID)
	if !ok || typ.Array == 0 {
		t.Fatalf("type %d has no array identity", child.Type.ID)
	}
	return model.TypeLayout{Type: model.Type{ID: typ.Array, Storage: model.TypeArray, Modifier: -1, Collation: child.Type.Collation}, Children: []model.TypeLayout{child}}
}

func arrayAccumulator(t *testing.T, q *runtime.Query, function model.FunctionID, input model.TypeLayout, limit uint64) *aggregate.Accumulator {
	t.Helper()
	output := input
	if function == 2335 {
		output = arrayLayout(t, input)
	}
	output.Type.Modifier = -1
	o := options(t, q, []model.TypeLayout{input}, []model.TypeLayout{output}, nil, nil)
	a, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: function, InputLayouts: o.InputLayouts, OutputLayout: output, Encoder: o.Encoder, Query: q, MaxRecordBytes: limit, WorkBytes: 16 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	return a
}

func arrayResult(t *testing.T, a *aggregate.Accumulator, want model.Value) {
	t.Helper()
	got, err := a.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("array_agg=%+v, want %+v", got, want)
	}
}

func arrayErrorCode(t *testing.T, err error, code string) {
	t.Helper()
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != code {
		t.Fatalf("array_agg error=%v, want SQLSTATE %s", err, code)
	}
}

func TestArrayAggregateScalarNullIsAnElementNotEmptyInput(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := arrayAccumulator(t, q, 2335, bytea, 128)
	arrayResult(t, a, model.NullVal(model.TypeArray))
	inactive := model.NullVal(model.TypeInt64)
	inactive.BytesVal = make([]byte, 4096)
	inactive.ArrayVal = []model.Value{model.StringVal("inactive")}
	if err := a.Advance(t.Context(), []model.Value{inactive}); err != nil {
		t.Fatal(err)
	}
	want := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.NullVal(model.TypeBytes)}}
	arrayResult(t, a, want)
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal([]byte{})}); err != nil {
		t.Fatal(err)
	}
	want.Dimensions[0].Length = 2
	want.ArrayVal = append(want.ArrayVal, model.BytesVal([]byte{}))
	arrayResult(t, a, want)
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	arrayResult(t, a, model.NullVal(model.TypeArray))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestArrayAggregateOwnsCompositePayloadAndSchema(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	nested := arrayLayout(t, bytea)
	input := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: []model.TypeLayout{text, nested}}
	payload := []byte{0, 255}
	value := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal("chosen"), {Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -3}}, ArrayVal: []model.Value{model.BytesVal(payload)}}}}
	owned := value.Clone()
	a := arrayAccumulator(t, q, 2335, input, 2048)
	if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
		t.Fatal(err)
	}
	clear(payload)
	value.ArrayVal[0] = model.StringVal("changed")
	value.ArrayVal[1].Dimensions[0].LowerBound = 1
	input.Children[0] = int8
	input.Children[1].Children[0] = int8
	want := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{owned}}
	arrayResult(t, a, want)
	if err := a.Advance(t.Context(), []model.Value{owned}); err != nil {
		t.Fatalf("caller schema mutation changed the accepted layout: %v", err)
	}
	want.Dimensions[0].Length = 2
	want.ArrayVal = append(want.ArrayVal, owned)
	malformed := owned.Clone()
	malformed.ArrayVal[1].ArrayVal[0] = model.Int64Val(1)
	if err := a.Advance(t.Context(), []model.Value{malformed}); !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("wrong nested child=%v", err)
	}
	arrayResult(t, a, want)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestArrayAggregateArrayInputsPreserveShapeAndBounds(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := arrayAccumulator(t, q, 4053, arrayLayout(t, bytea), 2048)
	value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -2}, {Length: 2, LowerBound: 4}}, ArrayVal: []model.Value{model.BytesVal([]byte{7}), model.NullVal(model.TypeBytes)}}
	original := value.Clone()
	arrayResult(t, a, model.NullVal(model.TypeArray))
	if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
		t.Fatal(err)
	}
	value.Dimensions[0].LowerBound = 1
	value.ArrayVal[0].BytesVal[0] = 99
	if err := a.Advance(t.Context(), []model.Value{original}); err != nil {
		t.Fatal(err)
	}
	want := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 1, LowerBound: -2}, {Length: 2, LowerBound: 4}}, ArrayVal: append(original.Clone().ArrayVal, original.Clone().ArrayVal...)}
	arrayResult(t, a, want)
	for _, tc := range []struct {
		name  string
		value model.Value
		code  string
	}{
		{"null", model.NullVal(model.TypeArray), "22004"},
		{"empty", model.Value{Type: model.TypeArray}, "2202E"},
		{"bounds", value, "2202E"},
		{"rank", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}, ArrayVal: original.ArrayVal}, "2202E"},
		{"lengths", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -2}, {Length: 1, LowerBound: 4}}, ArrayVal: original.ArrayVal}, "2202E"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrayErrorCode(t, a.Advance(t.Context(), []model.Value{tc.value}), tc.code)
			arrayResult(t, a, want)
		})
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	arrayErrorCode(t, a.Advance(t.Context(), []model.Value{model.NullVal(model.TypeArray)}), "22004")
	arrayErrorCode(t, a.Advance(t.Context(), []model.Value{{Type: model.TypeArray}}), "2202E")
	arrayResult(t, a, model.NullVal(model.TypeArray))
	if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
		t.Fatalf("reset retained the previous lower bounds: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestArrayAggregateAddsAtMostSixDimensions(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := arrayAccumulator(t, q, 4053, arrayLayout(t, int8), 1024)
	value := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(9)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -5}, {Length: 1, LowerBound: -4}, {Length: 1, LowerBound: -3}, {Length: 1, LowerBound: -2}, {Length: 1, LowerBound: -1}}}
	if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
		t.Fatal(err)
	}
	want := value.Clone()
	want.Dimensions = append([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, want.Dimensions...)
	arrayResult(t, a, want)
	arrayErrorCode(t, a.Advance(t.Context(), []model.Value{want}), "2202E")
	arrayResult(t, a, want)
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	arrayErrorCode(t, a.Advance(t.Context(), []model.Value{want}), "54000")
	arrayResult(t, a, model.NullVal(model.TypeArray))
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestArrayAggregateDefersMaxLastBoundsUntilResult(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := arrayAccumulator(t, q, 4053, arrayLayout(t, int8), 1024)
	value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32 - 1}}, ArrayVal: []model.Value{model.Int64Val(0), model.Int64Val(1)}}
	if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
		t.Fatalf("representable input rejected before finalization: %v", err)
	}
	_, err := a.Result(t.Context())
	arrayErrorCode(t, err, "54000")
	arrayErrorCode(t, a.Advance(t.Context(), []model.Value{model.NullVal(model.TypeArray)}), "22004")
	constructible := value.Clone()
	constructible.Dimensions[0].LowerBound--
	arrayErrorCode(t, a.Advance(t.Context(), []model.Value{constructible}), "2202E")
	if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
		t.Fatalf("failed finalization changed accumulation state: %v", err)
	}
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := a.Advance(t.Context(), []model.Value{constructible}); err != nil {
		t.Fatalf("reset after bounds failure: %v", err)
	}
	want := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 2, LowerBound: math.MaxInt32 - 2}}, ArrayVal: []model.Value{model.Int64Val(0), model.Int64Val(1)}}
	arrayResult(t, a, want)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestArrayAggregateMaxLastFinalizationFailureReleasesResources(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	array := arrayLayout(t, int8)
	value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -3}, {Length: 2, LowerBound: math.MaxInt32 - 1}}, ArrayVal: []model.Value{model.Int64Val(0), model.Int64Val(1)}}
	s := newSource(t, []model.TypeLayout{array}, [][]model.Value{{value}})
	o := options(t, q, []model.TypeLayout{array}, []model.TypeLayout{array}, nil, []aggregate.Call{{Function: 4053, Arguments: []int{0}}})
	c := open(t, s, o)
	out := columnar.Batch{}
	arrayErrorCode(t, c.Next(t.Context(), &out), "54000")
	if out.Rows() != 0 || len(out.Columns) != 0 {
		t.Fatal("bounds failure published an array result")
	}
	if !s.closed {
		t.Fatal("bounds failure retained the source")
	}
	released(t, q, dir)
}

func TestArrayAggregateBudgetFailurePreservesState(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := arrayAccumulator(t, q, 2335, bytea, 128)
	prefix := model.BytesVal([]byte("prefix"))
	if err := a.Advance(t.Context(), []model.Value{prefix}); err != nil {
		t.Fatal(err)
	}
	want := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{prefix}}
	for _, size := range []int{1024, 800} {
		err := a.Advance(t.Context(), []model.Value{model.BytesVal(make([]byte, size))})
		arrayErrorCode(t, err, "53200")
		if !errors.Is(err, model.ErrValueBudget) || !errors.Is(err, runtime.ErrBudget) {
			t.Fatalf("budget failure lost cause: %v", err)
		}
		arrayResult(t, a, want)
	}
	// This fits only if spare headers can give way to the new payload.
	last := model.BytesVal(bytes.Repeat([]byte{'x'}, 200))
	if err := a.Advance(t.Context(), []model.Value{last}); err != nil {
		t.Fatal(err)
	}
	want.Dimensions[0].Length = 2
	want.ArrayVal = append(want.ArrayVal, last)
	arrayResult(t, a, want)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestArrayAggregateCancellationAtCopyCheckpointsIsAtomic(t *testing.T) {
	for _, size := range []int{2, 8} {
		t.Run(map[int]string{2: "reuse", 8: "growth"}[size], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			record := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: []model.TypeLayout{bytea, text}}
			a := arrayAccumulator(t, q, 4053, arrayLayout(t, record), 2048)
			row := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: int32(size), LowerBound: -1}}, ArrayVal: make([]model.Value, size)}
			for i := range row.ArrayVal {
				row.ArrayVal[i] = model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.BytesVal([]byte{byte(i)}), model.StringVal("owned")}}
			}
			for checkpoint := 1; ; checkpoint++ {
				if checkpoint > 256 {
					t.Fatal("copy did not complete after all cancellation checkpoints")
				}
				if err := a.Reset(); err != nil {
					t.Fatal(err)
				}
				if err := a.Advance(t.Context(), []model.Value{row}); err != nil {
					t.Fatal(err)
				}
				want := row.Clone()
				want.Dimensions = append([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, want.Dimensions...)
				ctx, cancel := context.WithCancel(context.Background())
				interrupt := &stringAggregateCancelContext{Context: ctx, cancel: cancel, checks: checkpoint}
				err := a.Advance(interrupt, []model.Value{row})
				cancel()
				if err != nil {
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("checkpoint %d: %v", checkpoint, err)
					}
					arrayResult(t, a, want)
					if err := a.Advance(t.Context(), []model.Value{row}); err != nil {
						t.Fatalf("recovery at checkpoint %d: %v", checkpoint, err)
					}
				}
				want.Dimensions[0].Length = 2
				want.ArrayVal = append(want.ArrayVal, row.Clone().ArrayVal...)
				arrayResult(t, a, want)
				if err == nil {
					break
				}
			}
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestArrayAggregateDistinctRetainsOneScalarNull(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{int8, boolean}
	rows := [][]model.Value{{model.Int64Val(9), model.BoolVal(false)}, {model.NullVal(model.TypeInt64), model.BoolVal(false)}, {model.NullVal(model.TypeInt64), model.BoolVal(true)}, {model.Int64Val(2), model.BoolVal(true)}, {model.NullVal(model.TypeInt64), model.BoolVal(true)}, {model.Int64Val(2), model.BoolVal(true)}, {model.Int64Val(1), model.BoolVal(true)}}
	calls := []aggregate.Call{{Function: 2335, Arguments: []int{0}, Distinct: true, FilterColumn: new(1), OrderBy: []aggregate.OrderKey{{Column: 0, NullsFirst: true}}}}
	got, err := drain(t, open(t, newSource(t, layouts, rows), options(t, q, layouts, []model.TypeLayout{arrayLayout(t, int8)}, nil, calls)))
	if err != nil || len(got) != 1 {
		t.Fatalf("DISTINCT array_agg=%v, error=%v", got, err)
	}
	value := got[0][0]
	if value.IsNull || len(value.ArrayVal) != 3 || !value.ArrayVal[0].IsNull || value.ArrayVal[1].IntVal != 1 || value.ArrayVal[2].IntVal != 2 {
		t.Fatalf("DISTINCT array_agg=%+v, want {NULL,1,2}", value)
	}
	released(t, q, dir)
}

func TestArrayAggregateFlattensGroupsThroughSpill(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	array := arrayLayout(t, text)
	layouts := []model.TypeLayout{int8, array, boolean}
	var rows [][]model.Value
	for round := range 3 {
		for id := range 12 {
			value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -3}}, ArrayVal: []model.Value{model.StringVal("first")}}
			if round == 0 {
				value = model.NullVal(model.TypeArray)
			} else if round == 2 {
				value.ArrayVal[0] = model.StringVal("later")
			}
			rows = append(rows, []model.Value{model.Int64Val(int64(id)), value, model.BoolVal(round != 0)})
		}
	}
	o := options(t, q, layouts, []model.TypeLayout{int8, array}, []int{0}, []aggregate.Call{{Function: 4053, Arguments: []int{1}, FilterColumn: new(2)}})
	o.GroupBytes = 32 << 10
	got, err := drain(t, open(t, newSource(t, layouts, rows), o))
	if err != nil || len(got) != 12 {
		t.Fatalf("spilled array groups=%v, error=%v", got, err)
	}
	seen := make(map[int64]bool)
	for _, row := range got {
		id, value := row[0].IntVal, row[1]
		invalidGroup := id < 0 || id >= 12 || seen[id]
		invalidShape := value.IsNull || len(value.ArrayVal) != 2 || len(value.Dimensions) != 2
		if invalidGroup || invalidShape {
			t.Fatalf("spilled array group=%v", row)
		}
		if value.ArrayVal[0].StrVal != "first" || value.ArrayVal[1].StrVal != "later" || value.Dimensions[0] != (model.ArrayDimension{Length: 2, LowerBound: 1}) || value.Dimensions[1] != (model.ArrayDimension{Length: 1, LowerBound: -3}) {
			t.Fatalf("spilled array contents=%+v", value)
		}
		seen[id] = true
	}
	if q.Usage().Spill.Peak == 0 {
		t.Fatal("array groups did not spill")
	}
	released(t, q, dir)
}

func TestArrayAggregateSpillCloseAndCancellationReleaseResources(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel"}[cancelled], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			layouts := []model.TypeLayout{int8, bytea}
			var rows [][]model.Value
			for id := range 12 {
				rows = append(rows, []model.Value{model.Int64Val(int64(id)), model.BytesVal([]byte{0, 255})})
			}
			s := newSource(t, layouts, rows)
			o := options(t, q, layouts, []model.TypeLayout{int8, arrayLayout(t, bytea)}, []int{0}, []aggregate.Call{{Function: 2335, Arguments: []int{1}}})
			o.GroupBytes = 32 << 10
			c, err := aggregate.New(ctx, s, o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if q.Usage().Spill.Live == 0 {
				t.Fatal("early-close case did not retain spill")
			}
			if cancelled {
				cancel()
				if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled array_agg=%v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !s.closed {
				t.Fatal("array_agg retained source")
			}
			released(t, q, dir)
		})
	}
}

func TestArrayAggregateRejectsChangedResultLayoutsBeforeInputPull(t *testing.T) {
	for _, function := range []model.FunctionID{2335, 4053} {
		for _, change := range []struct {
			name  string
			apply func(*model.TypeLayout)
		}{
			{"array identity", func(l *model.TypeLayout) { l.Type.ID = 1001 }},
			{"array collation", func(l *model.TypeLayout) { l.Type.Collation = 100 }},
			{"array typmod", func(l *model.TypeLayout) { l.Type.Modifier = 42 }},
			{"element identity", func(l *model.TypeLayout) { l.Children[0].Type.ID = 1043 }},
			{"element collation", func(l *model.TypeLayout) { l.Children[0].Type.Collation = 100 }},
			{"element typmod", func(l *model.TypeLayout) { l.Children[0].Type.Modifier = -1 }},
		} {
			t.Run(map[model.FunctionID]string{2335: "scalar", 4053: "array"}[function]+"/"+change.name, func(t *testing.T) {
				q, dir := query(t, false, 32<<20)
				input := text
				if function == 4053 {
					input = arrayLayout(t, text)
				}
				output := arrayLayout(t, text)
				change.apply(&output)
				o := options(t, q, []model.TypeLayout{input}, []model.TypeLayout{output}, nil, []aggregate.Call{{Function: function, Arguments: []int{0}}})
				if err := aggregate.CheckSupport(t.Context(), o); !errors.Is(err, columnar.ErrValueLayout) {
					t.Fatalf("support accepted changed result: %v", err)
				}
				s := newSource(t, o.InputLayouts, nil)
				c, err := aggregate.New(t.Context(), s, o)
				if c != nil {
					t.Cleanup(func() { _ = c.Close() })
				}
				if !errors.Is(err, columnar.ErrValueLayout) || s.pulls != 0 || !s.closed {
					t.Fatalf("invalid result: error=%v pulls=%d closed=%v", err, s.pulls, s.closed)
				}
				released(t, q, dir)
			})
		}
	}
}

func TestArrayAggregateResourceErrorsReleaseUnpublishedResults(t *testing.T) {
	for _, admission := range []bool{false, true} {
		t.Run(map[bool]string{false: "growing result", true: "group admission"}[admission], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			rows := make([][]model.Value, 24)
			for i := range rows {
				rows[i] = []model.Value{model.BytesVal(bytes.Repeat([]byte{'x'}, 400))}
			}
			s := newSource(t, []model.TypeLayout{bytea}, rows)
			o := options(t, q, []model.TypeLayout{bytea}, []model.TypeLayout{arrayLayout(t, bytea)}, nil, []aggregate.Call{{Function: 2335, Arguments: []int{0}}})
			if admission {
				o.GroupBytes = 12 << 10
			}
			c, err := aggregate.New(t.Context(), s, o)
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
				out := columnar.Batch{}
				err = c.Next(t.Context(), &out)
				if out.Rows() != 0 {
					t.Fatalf("resource failure published %d rows", out.Rows())
				}
			}
			arrayErrorCode(t, err, "53200")
			if !s.closed || admission && s.pulls != 0 {
				t.Fatalf("resource failure: source closed=%v, pulls=%d", s.closed, s.pulls)
			}
			released(t, q, dir)
		})
	}
}
