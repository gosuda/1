package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type floatCoreUnaryCase struct {
	name        string
	input, want float64
}

func floatCoreCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireFloatCoreResult(t *testing.T, value model.Value, err error, want float64) {
	t.Helper()
	if err != nil || value.IsNull || value.Type != model.TypeFloat64 {
		t.Fatalf("result = %v, %v; want float8 %g", value, err, want)
	}
	if math.IsNaN(want) {
		if !math.IsNaN(value.FloatVal) {
			t.Fatalf("result = %g, want NaN", value.FloatVal)
		}
		return
	}
	if math.Float64bits(value.FloatVal) != math.Float64bits(want) {
		t.Fatalf("result = %.17g (%016x), want %.17g (%016x)", value.FloatVal, math.Float64bits(value.FloatVal), want, math.Float64bits(want))
	}
}

func floatCoreUnaryCases(t *testing.T, ids []model.FunctionID, cases []floatCoreUnaryCase) {
	t.Helper()
	for _, id := range ids {
		function := floatCoreCallable(t, id)
		t.Run(function.Signature().Name, func(t *testing.T) {
			for _, test := range cases {
				t.Run(test.name, func(t *testing.T) {
					value, err := function.Call(t.Context(), []model.Value{model.Float64Val(test.input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireFloatCoreResult(t, value, err, test.want)
				})
			}
		})
	}
}

func TestFloatCoreSquareRootBoundaries(t *testing.T) {
	floatCoreUnaryCases(t, []model.FunctionID{230, 1344}, []floatCoreUnaryCase{
		{"positive zero", 0, 0},
		{"negative zero", math.Copysign(0, -1), math.Copysign(0, -1)},
		{"smallest subnormal", math.SmallestNonzeroFloat64, 0x1p-537},
		{"largest finite", math.MaxFloat64, 0x1.fffffffffffffp511},
		{"positive infinity", math.Inf(1), math.Inf(1)},
		{"NaN", math.NaN(), math.NaN()},
	})
}

func TestFloatCoreCubeRootBoundaries(t *testing.T) {
	floatCoreUnaryCases(t, []model.FunctionID{231, 1345}, []floatCoreUnaryCase{
		{"negative cube", -27, -3},
		{"positive cube", 8, 2},
		{"negative zero", math.Copysign(0, -1), math.Copysign(0, -1)},
		{"smallest positive subnormal", math.SmallestNonzeroFloat64, 0x1p-358},
		{"smallest negative subnormal", -math.SmallestNonzeroFloat64, -0x1p-358},
		{"positive infinity", math.Inf(1), math.Inf(1)},
		{"negative infinity", math.Inf(-1), math.Inf(-1)},
		{"NaN", math.NaN(), math.NaN()},
	})
}

func TestFloatCoreExponentialBoundaries(t *testing.T) {
	floatCoreUnaryCases(t, []model.FunctionID{233, 1347}, []floatCoreUnaryCase{
		{"negative zero", math.Copysign(0, -1), 1},
		{"unit exponent", 1, math.E},
		{"subnormal result", -744, 0x1p-1073},
		{"last nonzero exponent", -745.1332191019411, math.SmallestNonzeroFloat64},
		{"negative infinity", math.Inf(-1), 0},
		{"positive infinity", math.Inf(1), math.Inf(1)},
		{"NaN", math.NaN(), math.NaN()},
	})
	for _, id := range []model.FunctionID{233, 1347} {
		function := floatCoreCallable(t, id)
		value, err := function.Call(t.Context(), []model.Value{model.Float64Val(709.782712893384)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		invalidValue := value.IsNull || value.Type != model.TypeFloat64 || math.IsInf(value.FloatVal, 0) || math.IsNaN(value.FloatVal)
		if err != nil || invalidValue || value.FloatVal < 0x1p1023 {
			t.Fatalf("%s at last finite exponent = %v, %v; want finite result in the highest binade", function.Signature().Name, value, err)
		}
	}
}

func TestFloatCoreLogarithmBoundaries(t *testing.T) {
	floatCoreUnaryCases(t, []model.FunctionID{234, 1341}, []floatCoreUnaryCase{
		{"unit", 1, 0},
		{"nearest above unit", 0x1.0000000000001p0, 0x1.fffffffffffffp-53},
		{"nearest below unit", 0x1.fffffffffffffp-1, -0x1p-53},
		{"smallest subnormal", math.SmallestNonzeroFloat64, -744.4400719213812},
		{"positive infinity", math.Inf(1), math.Inf(1)},
		{"NaN", math.NaN(), math.NaN()},
	})
	floatCoreUnaryCases(t, []model.FunctionID{1194, 1339, 1340}, []floatCoreUnaryCase{
		{"unit", 1, 0},
		{"positive power of ten", 100, 2},
		{"negative power of ten", 0.001, -3},
		{"positive infinity", math.Inf(1), math.Inf(1)},
		{"NaN", math.NaN(), math.NaN()},
	})
}

func TestFloatCoreSubnormalDecimalLogarithmRemainsFinite(t *testing.T) {
	for _, id := range []model.FunctionID{1194, 1339, 1340} {
		function := floatCoreCallable(t, id)
		value, err := function.Call(t.Context(), []model.Value{model.Float64Val(math.SmallestNonzeroFloat64)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		invalidValue := value.IsNull || value.Type != model.TypeFloat64 || math.IsInf(value.FloatVal, 0) || math.IsNaN(value.FloatVal)
		decimalExponent := value.FloatVal > -324 && value.FloatVal < -323
		if err != nil || invalidValue || !decimalExponent {
			t.Fatalf("%s of smallest subnormal = %v, %v; want finite decimal exponent between -324 and -323", function.Signature().Name, value, err)
		}
	}
}

func TestFloatCorePowerSpecialValues(t *testing.T) {
	negativeZero := math.Copysign(0, -1)
	for _, id := range []model.FunctionID{232, 1346, 1368} {
		function := floatCoreCallable(t, id)
		t.Run(function.Signature().Name, func(t *testing.T) {
			for _, test := range []struct {
				name                 string
				base, exponent, want float64
			}{
				{"negative base odd power", -2, 3, -8},
				{"negative base even power", -2, 4, 16},
				{"negative base reciprocal", -2, -3, -0.125},
				{"positive base fractional power", 9, 0.5, 3},
				{"zero to zero", 0, 0, 1},
				{"negative zero odd power", negativeZero, 3, negativeZero},
				{"negative zero even power", negativeZero, 2, 0},
				{"negative zero fractional power", negativeZero, 0.5, 0},
				{"largest finite", math.MaxFloat64, 1, math.MaxFloat64},
				{"smallest result", 2, -1074, math.SmallestNonzeroFloat64},
				{"negative subnormal result", -2, -1073, -0x1p-1073},
				{"subnormal base", math.SmallestNonzeroFloat64, 1, math.SmallestNonzeroFloat64},
				{"NaN to zero", math.NaN(), 0, 1},
				{"NaN to nonzero", math.NaN(), 2, math.NaN()},
				{"one to NaN", 1, math.NaN(), 1},
				{"negative one to NaN", -1, math.NaN(), math.NaN()},
				{"zero to NaN", 0, math.NaN(), math.NaN()},
				{"negative unit to infinity", -1, math.Inf(1), 1},
				{"large magnitude to infinity", -2, math.Inf(1), math.Inf(1)},
				{"large magnitude to negative infinity", -2, math.Inf(-1), 0},
				{"small magnitude to infinity", -0.5, math.Inf(1), 0},
				{"small magnitude to negative infinity", -0.5, math.Inf(-1), math.Inf(1)},
				{"zero to infinity", 0, math.Inf(1), 0},
				{"infinity to zero", math.Inf(1), 0, 1},
				{"infinity to positive fraction", math.Inf(1), 0.5, math.Inf(1)},
				{"infinity to negative power", math.Inf(1), -3, 0},
				{"negative infinity odd power", math.Inf(-1), 3, math.Inf(-1)},
				{"negative infinity even power", math.Inf(-1), 2, math.Inf(1)},
				{"negative infinity odd reciprocal", math.Inf(-1), -3, negativeZero},
				{"negative infinity even reciprocal", math.Inf(-1), -2, 0},
				{"negative infinity largest odd power", math.Inf(-1), 0x1.fffffffffffffp52, math.Inf(-1)},
				{"negative infinity huge even power", math.Inf(-1), math.MaxFloat64, math.Inf(1)},
				{"negative unit huge even power", -1, math.MaxFloat64, 1},
			} {
				t.Run(test.name, func(t *testing.T) {
					value, err := function.Call(t.Context(), []model.Value{model.Float64Val(test.base), model.Float64Val(test.exponent)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireFloatCoreResult(t, value, err, test.want)
				})
			}
		})
	}
}

func TestFloatCoreUnaryDomainErrors(t *testing.T) {
	for _, family := range []struct {
		ids    []model.FunctionID
		inputs []float64
		state  string
	}{
		{[]model.FunctionID{230, 1344}, []float64{-math.SmallestNonzeroFloat64, math.Inf(-1)}, "2201F"},
		{[]model.FunctionID{234, 1341, 1194, 1339, 1340}, []float64{0, math.Copysign(0, -1), -math.SmallestNonzeroFloat64, math.Inf(-1)}, "2201E"},
	} {
		for _, id := range family.ids {
			function := floatCoreCallable(t, id)
			t.Run(function.Signature().Name, func(t *testing.T) {
				for _, input := range family.inputs {
					_, err := function.Call(t.Context(), []model.Value{model.Float64Val(input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireSQLState(t, err, family.state)
				}
			})
		}
	}
}

func TestFloatCorePowerDomainErrors(t *testing.T) {
	for _, id := range []model.FunctionID{232, 1346, 1368} {
		function := floatCoreCallable(t, id)
		t.Run(function.Signature().Name, func(t *testing.T) {
			for _, test := range []struct {
				name           string
				base, exponent float64
			}{
				{"zero reciprocal", 0, -1},
				{"negative zero reciprocal", math.Copysign(0, -1), -1},
				{"zero to negative infinity", 0, math.Inf(-1)},
				{"negative fractional power", -2, 0.5},
				{"negative base subnormal exponent", -2, math.SmallestNonzeroFloat64},
				{"negative infinity fractional power", math.Inf(-1), 0.5},
			} {
				t.Run(test.name, func(t *testing.T) {
					_, err := function.Call(t.Context(), []model.Value{model.Float64Val(test.base), model.Float64Val(test.exponent)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireSQLState(t, err, "2201F")
				})
			}
		})
	}
}

func TestFloatCoreRangeErrors(t *testing.T) {
	for _, id := range []model.FunctionID{233, 1347} {
		function := floatCoreCallable(t, id)
		for _, input := range []float64{709.7827128933841, -745.1332191019412} {
			_, err := function.Call(t.Context(), []model.Value{model.Float64Val(input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22003")
		}
	}
	for _, id := range []model.FunctionID{232, 1346, 1368} {
		function := floatCoreCallable(t, id)
		for _, test := range []struct{ base, exponent float64 }{
			{2, 1024}, {-2, 1025}, {2, -1075}, {-2, -1075},
			{math.SmallestNonzeroFloat64, -1}, {0.5, math.MaxFloat64},
		} {
			_, err := function.Call(t.Context(), []model.Value{model.Float64Val(test.base), model.Float64Val(test.exponent)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22003")
		}
	}
}

func TestFloatCoreStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{230, 231, 232, 233, 234, 1194, 1339, 1340, 1341, 1344, 1345, 1346, 1347, 1368} {
		function := floatCoreCallable(t, id)
		t.Run(function.Signature().Name, func(t *testing.T) {
			arguments := []model.Value{model.NullVal(model.TypeFloat64)}
			if function.Signature().ArgumentCount == 2 {
				arguments = append(arguments, model.Float64Val(-0.5))
			}
			value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != model.TypeFloat64 {
				t.Fatalf("NULL argument result = %v, %v; want NULL float8", value, err)
			}
			if len(arguments) == 2 {
				arguments[0] = model.Float64Val(-2)
				arguments[1] = model.NullVal(model.TypeFloat64)
				value, err = function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull || value.Type != model.TypeFloat64 {
					t.Fatalf("NULL exponent result = %v, %v; want NULL float8", value, err)
				}
			}
		})
	}
}
