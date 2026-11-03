package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func floatTrigCall(t *testing.T, id model.FunctionID, inputs ...float64) (model.Value, error) {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	args := make([]model.Value, len(inputs))
	for i, input := range inputs {
		args[i] = model.Float64Val(input)
	}
	return function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
}

func floatTrigRequireExact(t *testing.T, id model.FunctionID, want float64, inputs ...float64) {
	t.Helper()
	value, err := floatTrigCall(t, id, inputs...)
	if err != nil {
		t.Fatal(err)
	}
	if value.IsNull || value.Type != model.TypeFloat64 || math.Float64bits(value.FloatVal) != math.Float64bits(want) {
		t.Fatalf("function %d(%v) = %v (%016x), want %v (%016x)", id, inputs, value, math.Float64bits(value.FloatVal), want, math.Float64bits(want))
	}
}

func TestFloatTrigDegreeQuadrants(t *testing.T) {
	negativeZero := math.Copysign(0, -1)
	for _, test := range []struct {
		name                  string
		x, sin, cos, tan, cot float64
	}{
		{"zero", 0, 0, 1, 0, math.Inf(1)},
		{"negative zero", negativeZero, negativeZero, 1, 0, math.Inf(-1)},
		{"quarter turn", 90, 1, 0, math.Inf(1), 0},
		{"half turn", 180, 0, -1, 0, math.Inf(-1)},
		{"three quarter turn", 270, -1, 0, math.Inf(-1), 0},
		{"full turn", 360, 0, 1, 0, math.Inf(1)},
		{"negative quarter turn", -90, -1, 0, math.Inf(-1), 0},
		{"negative half turn", -180, negativeZero, -1, 0, math.Inf(1)},
		{"negative three quarter turn", -270, 1, 0, math.Inf(1), 0},
		{"negative full turn", -360, negativeZero, 1, 0, math.Inf(-1)},
		{"wrapped quarter turn", 450, 1, 0, math.Inf(1), 0},
		{"large wrapped quarter turn", 360*0x1p40 + 90, 1, 0, math.Inf(1), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			floatTrigRequireExact(t, 2735, test.sin, test.x)
			floatTrigRequireExact(t, 2736, test.cos, test.x)
			floatTrigRequireExact(t, 2737, test.tan, test.x)
			floatTrigRequireExact(t, 2738, test.cot, test.x)
		})
	}
}

func TestFloatTrigDegreeCalibratedLandmarks(t *testing.T) {
	for _, test := range []struct {
		name string
		id   model.FunctionID
		x    float64
		want float64
	}{
		{"sind thirty", 2735, 30, 0.5},
		{"sind second quadrant", 2735, 150, 0.5},
		{"sind third quadrant", 2735, 210, -0.5},
		{"sind fourth quadrant", 2735, 330, -0.5},
		{"cosd sixty", 2736, 60, 0.5},
		{"cosd second quadrant", 2736, 120, -0.5},
		{"cosd third quadrant", 2736, 240, -0.5},
		{"cosd fourth quadrant", 2736, 300, 0.5},
		{"tand forty five", 2737, 45, 1},
		{"tand second quadrant", 2737, 135, -1},
		{"tand third quadrant", 2737, 225, 1},
		{"tand fourth quadrant", 2737, 315, -1},
		{"cotd forty five", 2738, 45, 1},
		{"cotd second quadrant", 2738, 135, -1},
		{"cotd third quadrant", 2738, 225, 1},
		{"cotd fourth quadrant", 2738, 315, -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			floatTrigRequireExact(t, test.id, test.want, test.x)
		})
	}
}

func TestFloatTrigInverseEndpoints(t *testing.T) {
	for _, test := range []struct {
		name string
		id   model.FunctionID
		x    float64
		want float64
	}{
		{"asin lower endpoint", 1600, -1, -math.Pi / 2},
		{"asin upper endpoint", 1600, 1, math.Pi / 2},
		{"acos lower endpoint", 1601, -1, math.Pi},
		{"acos upper endpoint", 1601, 1, 0},
		{"atan negative infinity", 1602, math.Inf(-1), -math.Pi / 2},
		{"atan positive infinity", 1602, math.Inf(1), math.Pi / 2},
		{"asind lower endpoint", 2731, -1, -90},
		{"asind negative half", 2731, -0.5, -30},
		{"asind half", 2731, 0.5, 30},
		{"asind upper endpoint", 2731, 1, 90},
		{"acosd lower endpoint", 2732, -1, 180},
		{"acosd negative half", 2732, -0.5, 120},
		{"acosd zero", 2732, 0, 90},
		{"acosd half", 2732, 0.5, 60},
		{"acosd upper endpoint", 2732, 1, 0},
		{"atand negative one", 2733, -1, -45},
		{"atand one", 2733, 1, 45},
		{"atand negative infinity", 2733, math.Inf(-1), -90},
		{"atand positive infinity", 2733, math.Inf(1), 90},
		{"acosh lower endpoint", 2466, 1, 0},
		{"acosh infinity", 2466, math.Inf(1), math.Inf(1)},
		{"atanh lower endpoint", 2467, -1, math.Inf(-1)},
		{"atanh upper endpoint", 2467, 1, math.Inf(1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			floatTrigRequireExact(t, test.id, test.want, test.x)
		})
	}
}

func TestFloatTrigAtan2AxesAndQuadrants(t *testing.T) {
	negativeZero := math.Copysign(0, -1)
	for _, test := range []struct {
		name                   string
		y, x, radians, degrees float64
	}{
		{"positive zeros", 0, 0, 0, 0},
		{"negative y zero", negativeZero, 0, negativeZero, negativeZero},
		{"negative x zero", 0, negativeZero, math.Pi, 180},
		{"negative zeros", negativeZero, negativeZero, -math.Pi, -180},
		{"positive y axis", 1, 0, math.Pi / 2, 90},
		{"negative y axis", -1, 0, -math.Pi / 2, -90},
		{"first quadrant", 1, 1, math.Pi / 4, 45},
		{"second quadrant", 1, -1, 3 * math.Pi / 4, 135},
		{"third quadrant", -1, -1, -3 * math.Pi / 4, -135},
		{"fourth quadrant", -1, 1, -math.Pi / 4, -45},
		{"infinite first quadrant", math.Inf(1), math.Inf(1), math.Pi / 4, 45},
		{"infinite third quadrant", math.Inf(-1), math.Inf(-1), -3 * math.Pi / 4, -135},
	} {
		t.Run(test.name, func(t *testing.T) {
			floatTrigRequireExact(t, 1603, test.radians, test.y, test.x)
			floatTrigRequireExact(t, 2734, test.degrees, test.y, test.x)
		})
	}
}

func TestFloatTrigSignedZero(t *testing.T) {
	negativeZero := math.Copysign(0, -1)
	for _, id := range []model.FunctionID{1600, 1602, 1604, 1606, 1608, 1609, 2462, 2464, 2465, 2467, 2731, 2733} {
		floatTrigRequireExact(t, id, negativeZero, negativeZero)
	}
	floatTrigRequireExact(t, 1607, math.Inf(-1), negativeZero)
	floatTrigRequireExact(t, 1601, math.Pi/2, negativeZero)
	floatTrigRequireExact(t, 1605, 1, negativeZero)
	floatTrigRequireExact(t, 2463, 1, negativeZero)
}

func TestFloatTrigConversions(t *testing.T) {
	floatTrigRequireExact(t, 1610, math.Pi)
	floatTrigRequireExact(t, 1608, 180, math.Pi)
	floatTrigRequireExact(t, 1609, math.Pi, 180)
	floatTrigRequireExact(t, 1608, math.Inf(-1), math.Inf(-1))
	floatTrigRequireExact(t, 1609, math.Inf(1), math.Inf(1))
	_, err := floatTrigCall(t, 1608, math.MaxFloat64)
	requireSQLState(t, err, "22003")
	_, err = floatTrigCall(t, 1609, math.SmallestNonzeroFloat64)
	requireSQLState(t, err, "22003")
}

func TestFloatTrigHyperbolicOverflow(t *testing.T) {
	for _, id := range []model.FunctionID{2462, 2463} {
		value, err := floatTrigCall(t, id, 710)
		if err != nil || value.IsNull || math.IsNaN(value.FloatVal) || math.IsInf(value.FloatVal, 0) || value.FloatVal <= math.MaxFloat64/2 {
			t.Fatalf("function %d(710) = %v, %v; want a finite value above MaxFloat64/2", id, value, err)
		}
		floatTrigRequireExact(t, id, math.Inf(1), 711)
	}
	floatTrigRequireExact(t, 2462, math.Inf(-1), -711)
	floatTrigRequireExact(t, 2463, math.Inf(1), -711)
	floatTrigRequireExact(t, 2464, -1, math.Inf(-1))
	floatTrigRequireExact(t, 2464, 1, math.Inf(1))
	floatTrigRequireExact(t, 2465, math.Inf(-1), math.Inf(-1))
	floatTrigRequireExact(t, 2465, math.Inf(1), math.Inf(1))
}

func TestFloatTrigSubnormalArguments(t *testing.T) {
	for _, id := range []model.FunctionID{1600, 1602, 1604, 1606, 2462, 2464, 2465, 2467} {
		floatTrigRequireExact(t, id, math.SmallestNonzeroFloat64, math.SmallestNonzeroFloat64)
	}
	floatTrigRequireExact(t, 1607, math.Inf(1), math.SmallestNonzeroFloat64)
	floatTrigRequireExact(t, 2735, 0, math.SmallestNonzeroFloat64)
}

func TestFloatTrigRejectsOutsideDomain(t *testing.T) {
	for _, id := range []model.FunctionID{1600, 1601, 2467, 2731, 2732} {
		for _, x := range []float64{math.Nextafter(1, 2), math.Nextafter(-1, -2), math.Inf(1), math.Inf(-1)} {
			_, err := floatTrigCall(t, id, x)
			requireSQLState(t, err, "22003")
		}
	}
	for _, id := range []model.FunctionID{1604, 1605, 1606, 1607, 2735, 2736, 2737, 2738} {
		for _, x := range []float64{math.Inf(1), math.Inf(-1)} {
			_, err := floatTrigCall(t, id, x)
			requireSQLState(t, err, "22003")
		}
	}
	_, err := floatTrigCall(t, 2466, math.Nextafter(1, 0))
	requireSQLState(t, err, "22003")
}

func TestFloatTrigNaNAndStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{
		1600, 1601, 1602, 1604, 1605, 1606, 1607, 1608, 1609,
		2462, 2463, 2464, 2465, 2466, 2467,
		2731, 2732, 2733, 2735, 2736, 2737, 2738,
	} {
		value, err := floatTrigCall(t, id, math.NaN())
		if err != nil || value.IsNull || !math.IsNaN(value.FloatVal) {
			t.Fatalf("function %d(NaN) = %v, %v", id, value, err)
		}
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		value, err = function.Call(t.Context(), []model.Value{model.NullVal(model.TypeFloat64)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull || value.Type != model.TypeFloat64 {
			t.Fatalf("function %d(NULL) = %v, %v", id, value, err)
		}
	}
	for _, id := range []model.FunctionID{1603, 2734} {
		for _, args := range [][2]float64{{math.NaN(), math.Inf(1)}, {math.Inf(1), math.NaN()}} {
			value, err := floatTrigCall(t, id, args[:]...)
			if err != nil || value.IsNull || !math.IsNaN(value.FloatVal) {
				t.Fatalf("function %d(%v) = %v, %v", id, args, value, err)
			}
		}
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]model.Value{
			{model.NullVal(model.TypeFloat64), model.Float64Val(math.Inf(1))},
			{model.Float64Val(math.Inf(1)), model.NullVal(model.TypeFloat64)},
		} {
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != model.TypeFloat64 {
				t.Fatalf("function %d(%v) = %v, %v", id, args, value, err)
			}
		}
	}
}
