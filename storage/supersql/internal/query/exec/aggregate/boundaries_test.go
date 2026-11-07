package aggregate_test

import (
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func TestFloat4TransitionsRoundAndOverflowAsFloat4(t *testing.T) {
	float4 := model.TypeLayout{Type: model.Type{ID: 700, Storage: model.TypeFloat64}}
	for _, test := range []struct {
		name     string
		values   []float64
		function model.FunctionID
		output   model.TypeLayout
		want     float64
		overflow bool
	}{
		{"rounded sum", []float64{16777216, 1}, 2110, float4, 16777216, false},
		{"widened average", []float64{16777216, 1}, 2104, float8, 8388608.5, false},
		{"sum overflow", []float64{math.MaxFloat32, math.MaxFloat32}, 2110, float4, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			var rows [][]model.Value
			for _, v := range test.values {
				rows = append(rows, []model.Value{model.Float64Val(v)})
			}
			o := options(t, q, []model.TypeLayout{float4}, []model.TypeLayout{test.output}, nil, []aggregate.Call{{Function: test.function, Arguments: []int{0}}})
			got, err := drain(t, open(t, newSource(t, []model.TypeLayout{float4}, rows), o))
			if test.overflow {
				var sql *model.Error
				if !errors.As(err, &sql) || sql.Code != "22003" {
					t.Fatalf("error=%v, want float4 overflow", err)
				}
			} else if err != nil || len(got) != 1 || got[0][0].IsNull || got[0][0].FloatVal != test.want {
				t.Fatalf("result=%v error=%v, want %v", got, err, test.want)
			}
			released(t, q, dir)
		})
	}
}

func TestNumericFinalTypmodRoundsBeforeOverflow(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
		overflow          bool
	}{
		{"round", "1.235", "1.24", false}, {"overflow", "999.995", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			out := decimal
			out.Type.Modifier = (5 << 16) + 2 + 4
			o := options(t, q, []model.TypeLayout{decimal}, []model.TypeLayout{out}, nil, []aggregate.Call{{Function: 2114, Arguments: []int{0}}})
			got, err := drain(t, open(t, newSource(t, []model.TypeLayout{decimal}, [][]model.Value{{number(test.value)}}), o))
			if test.overflow {
				var sql *model.Error
				if !errors.As(err, &sql) || sql.Code != "22003" {
					t.Fatalf("final numeric cast=%v, want overflow", err)
				}
			} else {
				if err != nil || len(got) != 1 {
					t.Fatalf("rows=%v error=%v", got, err)
				}
				exact(t, got[0][0], test.want)
			}
			released(t, q, dir)
		})
	}
}

func TestNumericSpecialValuesAndDistinctNaN(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	rows := [][]model.Value{{number("NaN")}, {number("NaN")}, {number("Infinity")}, {number("-Infinity")}, {model.NullVal(model.TypeNumeric)}}
	calls := []aggregate.Call{{Function: 2114, Arguments: []int{0}}, {Function: 2103, Arguments: []int{0}}, {Function: 2146, Arguments: []int{0}}, {Function: 2130, Arguments: []int{0}}, {Function: 2147, Arguments: []int{0}, Distinct: true}}
	o := options(t, q, []model.TypeLayout{decimal}, []model.TypeLayout{decimal, decimal, decimal, decimal, int8}, nil, calls)
	got, err := drain(t, open(t, newSource(t, []model.TypeLayout{decimal}, rows), o))
	if err != nil || len(got) != 1 {
		t.Fatalf("rows=%v error=%v", got, err)
	}
	for _, i := range []int{0, 1, 3} {
		exact(t, got[0][i], "NaN")
	}
	exact(t, got[0][2], "-Infinity")
	if got[0][4].IntVal != 3 {
		t.Fatalf("distinct numeric count=%v, want 3", got[0][4])
	}
	released(t, q, dir)
}

func TestTextExtremaOwnBorrowedBatchPayload(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	rows := [][]model.Value{{model.StringVal("z")}, {model.StringVal("a")}, {model.StringVal("m")}, {model.NullVal(model.TypeString)}}
	o := options(t, q, []model.TypeLayout{text}, []model.TypeLayout{text, text}, nil, []aggregate.Call{{Function: 2145, Arguments: []int{0}}, {Function: 2129, Arguments: []int{0}}})
	o.BatchRows = 1
	got, err := drain(t, open(t, newSource(t, []model.TypeLayout{text}, rows), o))
	if err != nil || len(got) != 1 || got[0][0].StrVal != "a" || got[0][1].StrVal != "z" {
		t.Fatalf("text extrema=%v error=%v", got, err)
	}
	released(t, q, dir)
}

func TestDistinctHotKeySpillsWithoutGrowingTransitionState(t *testing.T) {
	for _, memory := range []bool{false, true} {
		t.Run(map[bool]string{false: "spill", true: "memory"}[memory], func(t *testing.T) {
			q, dir := query(t, false, 32<<20)
			var rows [][]model.Value
			for range 2 {
				for i := range 100 {
					rows = append(rows, []model.Value{model.Int64Val(int64(i))})
				}
			}
			o := options(t, q, []model.TypeLayout{int4}, []model.TypeLayout{int8, int8, decimal}, nil, []aggregate.Call{{Function: 2147, Arguments: []int{0}, Distinct: true}, {Function: 2108, Arguments: []int{0}, Distinct: true}, {Function: 2101, Arguments: []int{0}, Distinct: true}})
			o.GroupBytes = 32 << 10
			if memory {
				o.PartitionMemoryBytes = 1 << 20
			} else {
				o.PartitionMemoryBytes = 1024
			}
			got, err := drain(t, open(t, newSource(t, []model.TypeLayout{int4}, rows), o))
			if err != nil || len(got) != 1 {
				t.Fatalf("rows=%v error=%v", got, err)
			}
			if got[0][0].IntVal != 100 || got[0][1].IntVal != 4950 {
				t.Fatalf("distinct count/sum=%v", got[0])
			}
			exact(t, got[0][2], "49.5")
			if !memory && q.Usage().Spill.Peak == 0 {
				t.Fatal("DISTINCT did not create external runs")
			}
			released(t, q, dir)
		})
	}
}

func TestOutputBudgetFailureClosesInput(t *testing.T) {
	q, dir := query(t, false, 32<<20)
	s := newSource(t, []model.TypeLayout{int8}, [][]model.Value{{model.Int64Val(1)}})
	o := options(t, q, []model.TypeLayout{int8}, []model.TypeLayout{decimal}, nil, []aggregate.Call{{Function: 2107, Arguments: []int{0}}})
	o.BatchBytes = 1
	c, err := aggregate.New(t.Context(), s, o)
	if err == nil {
		defer c.Close()
		out := columnar.Batch{}
		err = c.Next(t.Context(), &out)
	}
	if err == nil {
		t.Fatal("undersized output budget succeeded")
	}
	if !s.closed {
		t.Fatal("output failure retained input")
	}
	released(t, q, dir)
}
