package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestFloatSignDistinguishesNumericNaN(t *testing.T) {
	function := callable(t, "sign", 701)
	for _, test := range []struct{ input, want float64 }{
		{math.Inf(-1), -1}, {-math.SmallestNonzeroFloat64, -1}, {math.Copysign(0, -1), 0},
		{0, 0}, {math.SmallestNonzeroFloat64, 1}, {math.Inf(1), 1}, {math.NaN(), 0},
	} {
		result, err := function.Call(t.Context(), []model.Value{model.Float64Val(test.input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || result.Type != model.TypeFloat64 || result.IsNull || math.Float64bits(result.FloatVal) != math.Float64bits(test.want) {
			t.Fatalf("sign(%g)=%v, %v; want %g", test.input, result, err, test.want)
		}
	}
	result, err := callable(t, "sign", 1700).Call(t.Context(), []model.Value{numericDatum("NaN")}, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
	if err != nil || result.Type != model.TypeNumeric || result.StrVal != "NaN" {
		t.Fatalf("numeric sign(NaN)=%v, %v", result, err)
	}
}
