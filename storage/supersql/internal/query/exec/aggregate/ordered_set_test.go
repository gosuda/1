package aggregate_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

type fixedDirect struct {
	values []model.Value
	calls  int
}

func (d *fixedDirect) Evaluate(_ context.Context, _ []model.Value, _ []int, out []model.Value) error {
	d.calls++
	if len(out) != len(d.values) {
		return errors.New("unexpected direct argument count")
	}
	copy(out, d.values)
	return nil
}
func (d *fixedDirect) Close() error { return nil }

func TestOrderedSetPercentilesAndMode(t *testing.T) {
	interval := model.TypeLayout{Type: model.Type{ID: 1186, Storage: model.TypeInterval}}
	int4Array := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{int4}}
	float8Array := model.TypeLayout{Type: model.Type{ID: 1022, Storage: model.TypeArray}, Children: []model.TypeLayout{float8}}
	intervalArray := model.TypeLayout{Type: model.Type{ID: 1187, Storage: model.TypeArray}, Children: []model.TypeLayout{interval}}
	fractionArray := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 4, LowerBound: -2}}, ArrayVal: []model.Value{model.Float64Val(1), model.NullVal(model.TypeFloat64), model.Float64Val(0), model.Float64Val(.5)}}
	for _, scenario := range []struct {
		name     string
		function model.FunctionID
		input    model.TypeLayout
		output   model.TypeLayout
		rows     []model.Value
		direct   []model.Value
		check    func(*testing.T, model.Value)
	}{
		{"discrete scalar", 3972, int4, int4, []model.Value{model.Int64Val(3), model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(2)}, []model.Value{model.Float64Val(.5)}, func(t *testing.T, value model.Value) {
			if value.IsNull || value.IntVal != 2 { t.Fatalf("percentile_disc=%+v, want 2", value) }
		}},
		{"continuous float", 3974, float8, float8, []model.Value{model.Float64Val(20), model.Float64Val(0), model.Float64Val(10)}, []model.Value{model.Float64Val(.25)}, func(t *testing.T, value model.Value) {
			if value.IsNull || value.FloatVal != 5 { t.Fatalf("percentile_cont=%+v, want 5", value) }
		}},
		{"continuous interval", 3976, interval, interval, []model.Value{{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: 10}}, {Type: model.TypeInterval}}, []model.Value{model.Float64Val(.5)}, func(t *testing.T, value model.Value) {
			if value.IsNull || value.IntervalVal.Microseconds != 5 { t.Fatalf("interval percentile=%+v, want 5us", value) }
		}},
		{"discrete array", 3978, int4, int4Array, []model.Value{model.Int64Val(3), model.Int64Val(1), model.Int64Val(2)}, []model.Value{fractionArray}, func(t *testing.T, value model.Value) {
			if value.IsNull || len(value.Dimensions) != 1 || value.Dimensions[0].LowerBound != -2 || len(value.ArrayVal) != 4 || value.ArrayVal[0].IntVal != 3 || !value.ArrayVal[1].IsNull || value.ArrayVal[2].IntVal != 1 || value.ArrayVal[3].IntVal != 2 { t.Fatalf("array percentile_disc=%+v", value) }
		}},
		{"continuous float array", 3980, float8, float8Array, []model.Value{model.Float64Val(20), model.Float64Val(0), model.Float64Val(10)}, []model.Value{fractionArray}, func(t *testing.T, value model.Value) {
			if value.IsNull || value.ArrayVal[0].FloatVal != 20 || !value.ArrayVal[1].IsNull || value.ArrayVal[2].FloatVal != 0 || value.ArrayVal[3].FloatVal != 10 { t.Fatalf("float array percentile=%+v", value) }
		}},
		{"continuous interval array", 3982, interval, intervalArray, []model.Value{{Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: 20}}, {Type: model.TypeInterval}, {Type: model.TypeInterval, IntervalVal: model.Interval{Microseconds: 10}}}, []model.Value{fractionArray}, func(t *testing.T, value model.Value) {
			if value.IsNull || value.ArrayVal[0].IntervalVal.Microseconds != 20 || !value.ArrayVal[1].IsNull || value.ArrayVal[2].IntervalVal.Microseconds != 0 || value.ArrayVal[3].IntervalVal.Microseconds != 10 { t.Fatalf("interval array percentile=%+v", value) }
		}},
		{"mode sorted tie", 3984, text, text, []model.Value{model.StringVal("b"), model.StringVal("a"), model.StringVal("b"), model.StringVal("a"), model.StringVal("c"), model.NullVal(model.TypeString)}, nil, func(t *testing.T, value model.Value) {
			if value.IsNull || value.StrVal != "a" { t.Fatalf("mode=%+v, want a", value) }
		}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			q, dir := query(t, false, 8<<20)
			rows := make([][]model.Value, len(scenario.rows))
			for i, value := range scenario.rows {
				rows[i] = []model.Value{value}
			}
			var evaluator *fixedDirect
			call := aggregate.Call{Function: scenario.function, Arguments: []int{0}, OrderBy: []aggregate.OrderKey{{Column: 0}}}
			if scenario.direct != nil {
				evaluator = &fixedDirect{values: scenario.direct}
				call.DirectLayouts = []model.TypeLayout{float8}
				if scenario.direct[0].Type == model.TypeArray {
					call.DirectLayouts[0] = float8Array
				}
				call.DirectEvaluator = evaluator
			}
			cursor := open(t, newSource(t, []model.TypeLayout{scenario.input}, rows), options(t, q, []model.TypeLayout{scenario.input}, []model.TypeLayout{scenario.output}, nil, []aggregate.Call{call}))
			result, err := drain(t, cursor)
			if err != nil {
				t.Fatal(err)
			}
			if len(result) != 1 {
				t.Fatalf("rows=%d, want 1", len(result))
			}
			scenario.check(t, result[0][0])
			if evaluator != nil && evaluator.calls != 1 {
				t.Fatalf("direct evaluations=%d, want 1", evaluator.calls)
			}
			if err := cursor.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestOrderedSetFilterIsAppliedBeforeSorting(t *testing.T) {
	q, dir := query(t, false, 8<<20)
	filter := 1
	rows := [][]model.Value{
		{model.Int64Val(1), model.BoolVal(false)},
		{model.Int64Val(2), model.BoolVal(true)},
		{model.Int64Val(2), model.BoolVal(true)},
		{model.Int64Val(1), model.BoolVal(true)},
	}
	call := aggregate.Call{Function: 3984, Arguments: []int{0}, FilterColumn: &filter, OrderBy: []aggregate.OrderKey{{Column: 0}}}
	cursor := open(t, newSource(t, []model.TypeLayout{int4, boolean}, rows), options(t, q, []model.TypeLayout{int4, boolean}, []model.TypeLayout{int4}, nil, []aggregate.Call{call}))
	result, err := drain(t, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0][0].IsNull || result[0][0].IntVal != 2 {
		t.Fatalf("mode with filter=%+v, want 2", result)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestHypotheticalSetMulticolumnRanks(t *testing.T) {
	q, dir := query(t, false, 8<<20)
	rows := [][]model.Value{{model.Int64Val(1), model.StringVal("b")}, {model.Int64Val(2), model.StringVal("a")}, {model.Int64Val(1), model.StringVal("b")}, {model.NullVal(model.TypeInt64), model.StringVal("z")}}
	outputs := []model.TypeLayout{int8, float8, float8, int8}
	functions := []model.FunctionID{3986, 3988, 3990, 3992}
	calls := make([]aggregate.Call, len(functions))
	for i, function := range functions {
		direct := &fixedDirect{values: []model.Value{model.Int64Val(1), model.StringVal("a")}}
		calls[i] = aggregate.Call{Function: function, Arguments: []int{0, 1}, DirectLayouts: []model.TypeLayout{int4, text}, DirectEvaluator: direct, OrderBy: []aggregate.OrderKey{{Column: 0, NullsFirst: false}, {Column: 1, Descending: true, NullsFirst: false}}}
	}
	cursor := open(t, newSource(t, []model.TypeLayout{int4, text}, rows), options(t, q, []model.TypeLayout{int4, text}, outputs, nil, calls))
	result, err := drain(t, cursor)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("rows=%d, want 1", len(result))
	}
	row := result[0]
	if row[0].IntVal != 3 || row[1].FloatVal != .5 || math.Abs(row[2].FloatVal-.6) > 1e-12 || row[3].IntVal != 2 {
		t.Fatalf("hypothetical results=%+v, want rank=3 percent_rank=.5 cume_dist=.6 dense_rank=2", row)
	}
	for i, call := range calls {
		if call.DirectEvaluator.(*fixedDirect).calls != 1 {
			t.Fatalf("call %d direct evaluations=%d, want 1", i, call.DirectEvaluator.(*fixedDirect).calls)
		}
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}

func TestPercentileDirectEvaluationOnEmptyInput(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		function model.FunctionID
		layout   model.TypeLayout
		value    model.Value
		code     string
	}{
		{"scalar validates fraction", 3972, float8, model.Float64Val(math.NaN()), "22003"},
		{"array defers element validation", 3978, model.TypeLayout{Type: model.Type{ID: 1022, Storage: model.TypeArray}, Children: []model.TypeLayout{float8}}, model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.Float64Val(math.NaN())}}, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			q, dir := query(t, false, 8<<20)
			direct := &fixedDirect{values: []model.Value{scenario.value}}
			output := int4
			if scenario.function == 3978 {
				output = model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{int4}}
			}
			call := aggregate.Call{Function: scenario.function, Arguments: []int{0}, DirectLayouts: []model.TypeLayout{scenario.layout}, DirectEvaluator: direct, OrderBy: []aggregate.OrderKey{{Column: 0}}}
			cursor := open(t, newSource(t, []model.TypeLayout{int4}, nil), options(t, q, []model.TypeLayout{int4}, []model.TypeLayout{output}, nil, []aggregate.Call{call}))
			rows, err := drain(t, cursor)
			if direct.calls != 1 {
				t.Fatalf("direct evaluations=%d, want 1", direct.calls)
			}
			if scenario.code != "" {
				var diagnostic *model.Error
				if !errors.As(err, &diagnostic) || diagnostic.Code != scenario.code {
					t.Fatalf("error=%v, want SQLSTATE %s", err, scenario.code)
				}
			} else if err != nil || len(rows) != 1 || !rows[0][0].IsNull {
				t.Fatalf("empty array percentile rows=%+v error=%v, want one NULL", rows, err)
			}
			if err := cursor.Close(); err != nil {
				t.Fatal(err)
			}
			released(t, q, dir)
		})
	}
}

func TestArrayPercentileRejectsInvalidFractionWithInput(t *testing.T) {
	q, dir := query(t, false, 8<<20)
	arrayLayout := model.TypeLayout{Type: model.Type{ID: 1022, Storage: model.TypeArray}, Children: []model.TypeLayout{float8}}
	outputLayout := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{int4}}
	direct := &fixedDirect{values: []model.Value{{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.Float64Val(math.NaN())}}}}
	call := aggregate.Call{Function: 3978, Arguments: []int{0}, DirectLayouts: []model.TypeLayout{arrayLayout}, DirectEvaluator: direct, OrderBy: []aggregate.OrderKey{{Column: 0}}}
	cursor := open(t, newSource(t, []model.TypeLayout{int4}, [][]model.Value{{model.Int64Val(1)}}), options(t, q, []model.TypeLayout{int4}, []model.TypeLayout{outputLayout}, nil, []aggregate.Call{call}))
	_, err := drain(t, cursor)
	var diagnostic *model.Error
	if !errors.As(err, &diagnostic) || diagnostic.Code != "22003" {
		t.Fatalf("error=%v, want SQLSTATE 22003", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	released(t, q, dir)
}
