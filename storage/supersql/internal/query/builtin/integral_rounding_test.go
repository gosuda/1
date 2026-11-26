package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestNumericDirectedIntegralRounding(t *testing.T) {
	for _, tc := range []struct{ input, ceil, floor string }{
		{"1.20", "2", "1"},
		{"-1.20", "-1", "-2"},
		{"-0.000", "0", "0"},
		{"9007199254740993.000000000001", "9007199254740994", "9007199254740993"},
		{"-9007199254740993.000000000001", "-9007199254740993", "-9007199254740994"},
		{"999999999999999999999999999999.9", "1000000000000000000000000000000", "999999999999999999999999999999"},
		{"1e-16383", "1", "0"},
		{"-1e-16383", "0", "-1"},
		{"1e20", "100000000000000000000", "100000000000000000000"},
		{"42.00000", "42", "42"},
		{"NaN", "NaN", "NaN"},
		{"Infinity", "Infinity", "Infinity"},
		{"-Infinity", "-Infinity", "-Infinity"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			for _, operation := range []struct{ name, want string }{{"ceil", tc.ceil}, {"ceiling", tc.ceil}, {"floor", tc.floor}} {
				value, err := callable(t, operation.name, 1700).Call(t.Context(), []model.Value{numericDatum(tc.input)}, builtin.CallBudget{OutputBytes: 128, MemoryBytes: 64 << 20})
				if err != nil || value.IsNull || value.Type != model.TypeNumeric || value.StrVal != operation.want {
					t.Fatalf("%s(%s)=%v, %v; want %s", operation.name, tc.input, value, err, operation.want)
				}
			}
		})
	}
}

func TestFloatDirectedRoundingPreservesSpecialValues(t *testing.T) {
	negativeZero := math.Copysign(0, -1)
	for _, tc := range []struct{ input, ceil, floor float64 }{
		{-1.2, -1, -2}, {1.2, 2, 1}, {-.1, negativeZero, -1},
		{math.SmallestNonzeroFloat64, 1, 0}, {-math.SmallestNonzeroFloat64, negativeZero, -1},
		{negativeZero, negativeZero, negativeZero}, {math.Inf(1), math.Inf(1), math.Inf(1)},
		{math.Inf(-1), math.Inf(-1), math.Inf(-1)},
	} {
		for _, operation := range []struct {
			name string
			want float64
		}{{"ceil", tc.ceil}, {"ceiling", tc.ceil}, {"floor", tc.floor}} {
			value, err := callable(t, operation.name, 701).Call(t.Context(), []model.Value{model.Float64Val(tc.input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || math.Float64bits(value.FloatVal) != math.Float64bits(operation.want) {
				t.Fatalf("%s(%g)=%v, %v; want %g", operation.name, tc.input, value, err, operation.want)
			}
		}
	}
	for _, name := range []string{"ceil", "ceiling", "floor"} {
		value, err := callable(t, name, 701).Call(t.Context(), []model.Value{model.Float64Val(math.NaN())}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !math.IsNaN(value.FloatVal) {
			t.Fatalf("%s(NaN)=%v, %v", name, value, err)
		}
	}
}

func TestNumericIntegralRoundingRespectsOutputBudget(t *testing.T) {
	_, err := callable(t, "floor", 1700).Call(t.Context(), []model.Value{numericDatum("-.1")}, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}
