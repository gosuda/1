package builtin_test

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func requireWidthBucketRangeResult(t *testing.T, value model.Value, err error, want int64) {
	t.Helper()
	if err != nil || value.IsNull || value.Type != model.TypeInt64 || value.IntVal != want {
		t.Fatalf("width_bucket = %v, %v; want int4 %d", value, err, want)
	}
}

func TestFloatWidthBucketBoundaries(t *testing.T) {
	function, err := builtin.Resolve(320, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name                    string
		operand, bound1, bound2 float64
		count, want             int64
	}{
		{"ascending below", -1, 0, 10, 5, 0},
		{"ascending first bound", 0, 0, 10, 5, 1},
		{"ascending interior boundary", 4, 0, 10, 5, 3},
		{"ascending second bound", 10, 0, 10, 5, 6},
		{"ascending above", 11, 0, 10, 5, 6},
		{"descending above", 11, 10, 0, 5, 0},
		{"descending first bound", 10, 10, 0, 5, 1},
		{"descending interior boundary", 6, 10, 0, 5, 3},
		{"descending second bound", 0, 10, 0, 5, 6},
		{"descending below", -1, 10, 0, 5, 6},
		{"negative bounds", -6, -10, -2, 4, 3},
		{"signed zero first bound", math.Copysign(0, -1), 0, 1, 1, 1},
		{"below internal boundary", math.Nextafter(0.5, 0), 0, 1, 4, 2},
		{"above internal boundary", math.Nextafter(0.5, 1), 0, 1, 4, 3},
		{"descending below internal boundary", math.Nextafter(0.5, 0), 1, 0, 4, 3},
		{"descending above internal boundary", math.Nextafter(0.5, 1), 1, 0, 4, 2},
		{"rounded quotient clamped", math.Nextafter(1, 0), -1, 1, math.MaxInt32, math.MaxInt32},
		{"descending rounded quotient clamped", math.Nextafter(-1, 0), 1, -1, math.MaxInt32, math.MaxInt32},
		{"division before multiplication", 1e308, 0, math.MaxFloat64, 4, 3},
		{"binary decimal boundary", 0.58, 0, 1, 100, 58},
		{"overflowing span midpoint", 0, -math.MaxFloat64, math.MaxFloat64, 4, 3},
		{"descending overflowing span midpoint", 0, math.MaxFloat64, -math.MaxFloat64, 4, 3},
		{"overflowing span next to end", math.Nextafter(math.MaxFloat64, 0), -math.MaxFloat64, math.MaxFloat64, 4, 4},
		{"descending overflowing span next to end", math.Nextafter(-math.MaxFloat64, 0), math.MaxFloat64, -math.MaxFloat64, 4, 4},
		{"subnormal span", math.SmallestNonzeroFloat64, 0, 4 * math.SmallestNonzeroFloat64, 4, 2},
		{"descending subnormal span", 3 * math.SmallestNonzeroFloat64, 4 * math.SmallestNonzeroFloat64, 0, 4, 2},
		{"smallest span first bound", 0, 0, math.SmallestNonzeroFloat64, 1, 1},
		{"smallest span second bound", math.SmallestNonzeroFloat64, 0, math.SmallestNonzeroFloat64, 1, 2},
		{"ascending negative infinity", math.Inf(-1), 0, 1, math.MaxInt32, 0},
		{"ascending positive infinity", math.Inf(1), 0, 1, 5, 6},
		{"descending positive infinity", math.Inf(1), 1, 0, math.MaxInt32, 0},
		{"descending negative infinity", math.Inf(-1), 1, 0, 5, 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := []model.Value{model.Float64Val(test.operand), model.Float64Val(test.bound1), model.Float64Val(test.bound2), model.Int64Val(test.count)}
			value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireWidthBucketRangeResult(t, value, err, test.want)
		})
	}
}

func TestNumericWidthBucketExactBoundaries(t *testing.T) {
	function, err := builtin.Resolve(2170, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, operand, bound1, bound2 string
		count, want                   int64
	}{
		{"ascending below", "-1", "0", "10", 5, 0},
		{"ascending first bound", "0", "0", "10", 5, 1},
		{"ascending interior boundary", "4", "0", "10", 5, 3},
		{"ascending second bound", "10", "0", "10", 5, 6},
		{"ascending above", "11", "0", "10", 5, 6},
		{"descending above", "11", "10", "0", 5, 0},
		{"descending first bound", "10", "10", "0", 5, 1},
		{"descending interior boundary", "6", "10", "0", 5, 3},
		{"descending second bound", "0", "10", "0", 5, 6},
		{"descending below", "-1", "10", "0", 5, 6},
		{"negative bounds", "-6", "-10", "-2", 4, 3},
		{"mixed display scales", "0.30", "0.1", "0.5000", 4, 3},
		{"below decimal boundary", "0.299999999999999999999999999999", "0", "1", 10, 3},
		{"at decimal boundary", "0.3", "0", "1", 10, 4},
		{"above decimal boundary", "0.300000000000000000000000000001", "0", "1", 10, 4},
		{"descending above decimal boundary", "0.700000000000000000000000000001", "1", "0", 10, 3},
		{"descending at decimal boundary", "0.7", "1", "0", 10, 4},
		{"descending below decimal boundary", "0.699999999999999999999999999999", "1", "0", 10, 4},
		{"exact decimal unlike binary float", "0.58", "0", "1", 100, 59},
		{"beyond binary integer precision", "9007199254740993", "9007199254740992", "9007199254740994", 2, 2},
		{"large integer fractional boundary", "9007199254740993.00000000000000000002", "9007199254740993", "9007199254740993.00000000000000000004", 4, 3},
		{"large exponents", "5e30000", "0", "1e30001", 10, 6},
		{"smallest stored scale", "2e-16383", "1e-16383", "5e-16383", 4, 2},
		{"maximum count final interior", "0.999999999999999999999999999999", "0", "1", math.MaxInt32, math.MaxInt32},
		{"descending maximum count final interior", "0.000000000000000000000000000001", "1", "0", math.MaxInt32, math.MaxInt32},
		{"ascending negative infinity", "-Infinity", "0", "1", math.MaxInt32, 0},
		{"ascending positive infinity", "Infinity", "0", "1", 5, 6},
		{"descending positive infinity", "Infinity", "1", "0", math.MaxInt32, 0},
		{"descending negative infinity", "-Infinity", "1", "0", 5, 6},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := []model.Value{numericDatum(test.operand), numericDatum(test.bound1), numericDatum(test.bound2), model.Int64Val(test.count)}
			value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireWidthBucketRangeResult(t, value, err, test.want)
		})
	}
}

func TestWidthBucketRangeInvalidArguments(t *testing.T) {
	for _, id := range []model.FunctionID{320, 2170} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct {
			name, operand, bound1, bound2 string
			count                         int64
			state                         string
		}{
			{"zero count", "0.5", "0", "1", 0, "2201G"},
			{"negative count", "0.5", "0", "1", -1, "2201G"},
			{"invalid count outside bounds", "Infinity", "0", "1", 0, "2201G"},
			{"NaN operand", "NaN", "0", "1", math.MaxInt32, "2201G"},
			{"NaN first bound", "Infinity", "NaN", "1", math.MaxInt32, "2201G"},
			{"NaN second bound", "Infinity", "0", "NaN", math.MaxInt32, "2201G"},
			{"infinite first bound", "0", "-Infinity", "1", math.MaxInt32, "2201G"},
			{"infinite second bound", "Infinity", "0", "Infinity", math.MaxInt32, "2201G"},
			{"equal finite bounds", "0", "1", "1.0", math.MaxInt32, "2201G"},
			{"equal zero bounds", "Infinity", "-0", "0", math.MaxInt32, "2201G"},
			{"ascending count overflow at endpoint", "1", "0", "1", math.MaxInt32, "22003"},
			{"ascending count overflow at infinity", "Infinity", "0", "1", math.MaxInt32, "22003"},
			{"descending count overflow at endpoint", "0", "1", "0", math.MaxInt32, "22003"},
			{"descending count overflow at infinity", "-Infinity", "1", "0", math.MaxInt32, "22003"},
		} {
			t.Run(strconv.Itoa(int(id))+"/"+test.name, func(t *testing.T) {
				arguments := make([]model.Value, 4)
				for i, text := range []string{test.operand, test.bound1, test.bound2} {
					arguments[i] = numericDatum(text)
					if id == 320 {
						value, err := strconv.ParseFloat(text, 64)
						if err != nil {
							t.Fatal(err)
						}
						arguments[i] = model.Float64Val(value)
					}
				}
				arguments[3] = model.Int64Val(test.count)
				_, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, test.state)
			})
		}
	}
}

func TestNumericWidthBucketStoredInputPrecedence(t *testing.T) {
	function, err := builtin.Resolve(2170, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, operand, bound1, bound2 string
		count                         int64
		state                         string
	}{
		{"operand parse before bounds", "broken", "1e-16384", "1", 1, "22P02"},
		{"operand scale before bound parse", "1e-16384", "broken", "1", 1, "22003"},
		{"first bound parse before second scale", "0", "broken", "1e-16384", 1, "22P02"},
		{"first bound scale before second parse", "0", "1e-16384", "broken", 1, "22003"},
		{"second bound parse before count", "0", "0", "broken", 0, "22P02"},
		{"second bound scale before NaN", "NaN", "0", "1e-16384", 1, "22003"},
		{"bound parse before operand NaN", "NaN", "broken", "1", 1, "22P02"},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := []model.Value{numericDatum(test.operand), numericDatum(test.bound1), numericDatum(test.bound2), model.Int64Val(test.count)}
			_, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, test.state)
		})
	}
}

func TestWidthBucketRangeStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{320, 2170} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for nullIndex := range 4 {
			arguments := []model.Value{numericDatum("NaN"), numericDatum("0"), numericDatum("1"), model.Int64Val(0)}
			if id == 320 {
				arguments[0], arguments[1], arguments[2] = model.Float64Val(math.NaN()), model.Float64Val(0), model.Float64Val(1)
			}
			arguments[nullIndex] = model.NullVal(arguments[nullIndex].Type)
			value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != model.TypeInt64 {
				t.Fatalf("function %d NULL argument %d = %v, %v; want int4 NULL", id, nullIndex, value, err)
			}
		}
	}
}

func TestNumericWidthBucketWorkLimits(t *testing.T) {
	function, err := builtin.Resolve(2170, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, operand, bound1, bound2 string
		count                         int64
	}{
		{"input byte limit", strings.Repeat("0", 65537), "0", "1", 1},
		{"expanded coefficient limit", "1e32768", "0", "1", 1},
		{"alignment limit", "1", "1e-16383", "1e20000", 1},
		{"subtraction growth limit", "0", "-1e32767", "1e32767", 1},
		{"multiplication growth limit", "1e32760", "0", "1e32761", math.MaxInt32},
	} {
		t.Run(test.name, func(t *testing.T) {
			arguments := []model.Value{numericDatum(test.operand), numericDatum(test.bound1), numericDatum(test.bound2), model.Int64Val(test.count)}
			_, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestNumericWidthBucketCancellation(t *testing.T) {
	function, err := builtin.Resolve(2170, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 27}
	arguments := []model.Value{numericDatum("1.5"), numericDatum("0"), numericDatum("3"), model.Int64Val(10)}
	_, err = function.Call(interrupted, arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("numeric width_bucket did not cancel during arithmetic: %v", err)
	}
}
