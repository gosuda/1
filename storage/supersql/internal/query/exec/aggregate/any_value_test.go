package aggregate_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func anyValueAccumulator(t *testing.T, q *runtime.Query, layout model.TypeLayout, limit uint64) *aggregate.Accumulator {
	t.Helper()
	output := layout
	output.Type.Modifier = -1
	o := options(t, q, []model.TypeLayout{layout}, []model.TypeLayout{output}, nil, nil)
	a, err := aggregate.NewAccumulator(t.Context(), aggregate.AccumulatorOptions{Function: 6291, InputLayouts: o.InputLayouts, OutputLayout: output, Encoder: o.Encoder, Query: q, MaxRecordBytes: limit, WorkBytes: 16 << 20})
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

func anyValueResult(t *testing.T, a *aggregate.Accumulator, want model.Value) {
	t.Helper()
	got, err := a.Result(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("any_value=%+v, want %+v", got, want)
	}
}

func TestAnyValueSelectsFirstNonNullDatum(t *testing.T) {
	for _, tc := range []struct {
		name         string
		layout       model.TypeLayout
		first, later model.Value
	}{
		{"integer", int8, model.Int64Val(9), model.Int64Val(1)},
		{"boolean", boolean, model.BoolVal(false), model.BoolVal(true)},
		{"float", float8, model.Float64Val(1.25), model.Float64Val(-7)},
		{"numeric", decimal, number("1.2300"), number("99")},
		{"text", text, model.StringVal(""), model.StringVal("later")},
		{"bytea", bytea, model.BytesVal([]byte{0, 255}), model.BytesVal([]byte{1})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			a := anyValueAccumulator(t, q, tc.layout, 1024)
			null := model.NullVal(tc.layout.Type.Storage)
			anyValueResult(t, a, null)
			if err := a.Advance(t.Context(), []model.Value{null}); err != nil {
				t.Fatal(err)
			}
			anyValueResult(t, a, null)
			for _, value := range []model.Value{tc.first, null, tc.later} {
				if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
					t.Fatal(err)
				}
			}
			anyValueResult(t, a, tc.first)
			if err := a.Reset(); err != nil {
				t.Fatal(err)
			}
			anyValueResult(t, a, null)
			if err := a.Advance(t.Context(), []model.Value{tc.later}); err != nil {
				t.Fatal(err)
			}
			anyValueResult(t, a, tc.later)
			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestAnyValueOwnsNestedPayloadAndLayouts(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	array := model.TypeLayout{Type: model.Type{ID: 1001, Storage: model.TypeArray}, Children: []model.TypeLayout{bytea}}
	layout := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: []model.TypeLayout{text, array}}
	payload := []byte{0, 255}
	value := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{
		model.StringVal("chosen"),
		{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -3}}, ArrayVal: []model.Value{model.BytesVal(payload), model.NullVal(model.TypeBytes)}},
	}}
	want := value.Clone()
	a := anyValueAccumulator(t, q, layout, 2048)
	if err := a.Advance(t.Context(), []model.Value{value}); err != nil {
		t.Fatal(err)
	}
	clear(payload)
	value.ArrayVal[0] = model.StringVal("replaced")
	value.ArrayVal[1].Dimensions[0].LowerBound = 1
	layout.Children[0] = int8
	layout.Children[1].Children[0] = int8
	anyValueResult(t, a, want)
	if err := a.Advance(t.Context(), []model.Value{want}); err != nil {
		t.Fatalf("caller layout mutation changed accepted shape: %v", err)
	}
	malformed := want.Clone()
	malformed.ArrayVal[1].ArrayVal[0] = model.Int64Val(1)
	if err := a.Advance(t.Context(), []model.Value{malformed}); !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("late wrong array child=%v", err)
	}
	anyValueResult(t, a, want)
	if err := a.Reset(); err != nil {
		t.Fatal(err)
	}
	if err := a.Advance(t.Context(), []model.Value{want}); err != nil {
		t.Fatal(err)
	}
	anyValueResult(t, a, want)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestAnyValueRejectsOutputShapeBeforePullingInput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*model.TypeLayout)
	}{
		{"child identity", func(l *model.TypeLayout) { l.Children[0].Type.ID = 1043 }},
		{"child collation", func(l *model.TypeLayout) { l.Children[0].Type.Collation = 100 }},
		{"child typmod", func(l *model.TypeLayout) { l.Children[0].Type.Modifier = -1 }},
		{"top collation", func(l *model.TypeLayout) { l.Type.Collation = 100 }},
		{"top typmod changed", func(l *model.TypeLayout) { l.Type.Modifier = 42 }},
		{"missing child", func(l *model.TypeLayout) { l.Children = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			input := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite}, Children: []model.TypeLayout{text}}
			output := model.TypeLayout{Type: input.Type, Children: []model.TypeLayout{text}}
			tc.change(&output)
			o := options(t, q, []model.TypeLayout{input}, []model.TypeLayout{output}, nil, []aggregate.Call{{Function: 6291, Arguments: []int{0}}})
			if err := aggregate.CheckSupport(t.Context(), o); !errors.Is(err, columnar.ErrValueLayout) {
				t.Fatalf("support accepted wrong output layout: %v", err)
			}
			s := newSource(t, o.InputLayouts, nil)
			c, err := aggregate.New(t.Context(), s, o)
			if c != nil {
				t.Cleanup(func() { _ = c.Close() })
			}
			if !errors.Is(err, columnar.ErrValueLayout) || s.pulls != 0 || !s.closed {
				t.Fatalf("invalid output: error=%v pulls=%d closed=%v", err, s.pulls, s.closed)
			}
			released(t, q, dir)
		})
	}
}

func TestAnyValueFailedSelectionAndLateErrorsPreserveState(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	a := anyValueAccumulator(t, q, bytea, 128)
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal(make([]byte, 129))}); !errors.Is(err, model.ErrValueBudget) {
		t.Fatalf("oversized initial selection=%v", err)
	}
	anyValueResult(t, a, model.NullVal(model.TypeBytes))
	payload := bytes.Repeat([]byte{'x'}, 128)
	want := model.BytesVal(bytes.Clone(payload))
	if err := a.Advance(t.Context(), []model.Value{model.BytesVal(payload)}); err != nil {
		t.Fatal(err)
	}
	clear(payload)
	if err := a.Advance(t.Context(), []model.Value{model.Int64Val(1)}); !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("late type error=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := a.Advance(ctx, []model.Value{model.BytesVal(nil)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("late cancellation=%v", err)
	}
	anyValueResult(t, a, want)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestAnyValueConsumesLateSourceErrors(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	rows := make([][]model.Value, 33)
	for i := range rows {
		rows[i] = []model.Value{model.Int64Val(9)}
	}
	rows[32][0] = model.StringVal("wrong storage")
	s := newSource(t, []model.TypeLayout{int8}, rows)
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{int8}, nil, []aggregate.Call{{Function: 6291, Arguments: []int{0}}})
	if _, err := drain(t, open(t, s, o)); !errors.Is(err, columnar.ErrValueLayout) {
		t.Fatalf("late source error=%v", err)
	}
	if !s.closed {
		t.Fatal("failed any_value retained source")
	}
	released(t, q, dir)
}

func TestAnyValueOrdersFilteredDistinctInputs(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	layouts := []model.TypeLayout{text, boolean}
	rows := [][]model.Value{
		{model.StringVal("z"), model.BoolVal(false)},
		{model.StringVal("a"), model.BoolVal(true)},
		{model.NullVal(model.TypeString), model.BoolVal(true)},
		{model.StringVal("b"), model.BoolVal(true)},
		{model.StringVal("b"), model.BoolVal(true)},
	}
	calls := []aggregate.Call{{Function: 6291, Arguments: []int{0}, Distinct: true, FilterColumn: new(1), OrderBy: []aggregate.OrderKey{{Column: 0, Descending: true, NullsFirst: true}}}}
	got, err := drain(t, open(t, newSource(t, layouts, rows), options(t, q, layouts, []model.TypeLayout{text}, nil, calls)))
	if err != nil || len(got) != 1 {
		t.Fatalf("ordered DISTINCT any_value=%v, error=%v", got, err)
	}
	if got[0][0].IsNull || got[0][0].StrVal != "b" {
		t.Fatalf("ordered DISTINCT any_value=%v, want b", got)
	}
	released(t, q, dir)
}

func TestAnyValueNestedGroupsSurviveSpill(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	array := model.TypeLayout{Type: model.Type{ID: 1009, Storage: model.TypeArray}, Children: []model.TypeLayout{text}}
	layouts := []model.TypeLayout{int8, array}
	var rows [][]model.Value
	for round := range 3 {
		for id := range 12 {
			value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -1}}, ArrayVal: []model.Value{model.StringVal("first")}}
			if round == 0 {
				value = model.NullVal(model.TypeArray)
			} else if round == 2 {
				value.ArrayVal[0] = model.StringVal("later")
			}
			rows = append(rows, []model.Value{model.Int64Val(int64(id)), value})
		}
	}
	o := options(t, q, layouts, layouts, []int{0}, []aggregate.Call{{Function: 6291, Arguments: []int{1}}})
	o.GroupBytes = 32 << 10
	got, err := drain(t, open(t, newSource(t, layouts, rows), o))
	if err != nil || len(got) != 12 {
		t.Fatalf("spilled nested groups=%v, error=%v", got, err)
	}
	seen := make(map[int64]bool)
	for _, row := range got {
		id, value := row[0].IntVal, row[1]
		invalidGroup := id < 0 || id >= 12 || seen[id]
		invalidShape := value.IsNull || len(value.ArrayVal) != 1 || len(value.Dimensions) != 1
		if invalidGroup || invalidShape {
			t.Fatalf("spilled group shape=%v", row)
		}
		if value.ArrayVal[0].StrVal != "first" || value.Dimensions[0].LowerBound != -1 {
			t.Fatalf("spilled group=%v", row)
		}
		seen[id] = true
	}
	if q.Usage().Spill.Peak == 0 {
		t.Fatal("nested groups did not spill")
	}
	released(t, q, dir)
}

func TestAnyValueOutputFailureReleasesState(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	s := newSource(t, []model.TypeLayout{bytea}, [][]model.Value{{model.BytesVal(make([]byte, 1024))}})
	o := options(t, q, []model.TypeLayout{bytea}, []model.TypeLayout{bytea}, nil, []aggregate.Call{{Function: 6291, Arguments: []int{0}}})
	o.BatchBytes = 1024
	if _, err := drain(t, open(t, s, o)); !errors.Is(err, runtime.ErrBudget) {
		t.Fatalf("output budget failure=%v", err)
	}
	if !s.closed {
		t.Fatal("output failure retained source")
	}
	released(t, q, dir)
}

func TestAnyValueSpillCloseAndCancellationReleaseResources(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "cancel"}[cancelled], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			layouts := []model.TypeLayout{int8, bytea}
			var rows [][]model.Value
			for id := range 40 {
				rows = append(rows, []model.Value{model.Int64Val(int64(id)), model.BytesVal([]byte{0, 255})})
			}
			s := newSource(t, layouts, rows)
			o := options(t, q, layouts, layouts, []int{0}, []aggregate.Call{{Function: 6291, Arguments: []int{1}}})
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
					t.Fatalf("cancelled any_value=%v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !s.closed {
				t.Fatal("any_value retained source")
			}
			released(t, q, dir)
		})
	}
}

func TestAnyValueRejectsCyclicLayoutBeforeExecution(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	children := make([]model.TypeLayout, 1)
	cyclic := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: children}
	children[0] = cyclic
	o := options(t, q, []model.TypeLayout{cyclic}, []model.TypeLayout{cyclic}, nil, []aggregate.Call{{Function: 6291, Arguments: []int{0}}})
	if err := aggregate.CheckSupport(t.Context(), o); !errors.Is(err, model.ErrInvalidTypeLayout) {
		t.Fatalf("cyclic layout error=%v, want invalid layout", err)
	}
	released(t, q, dir)
}
