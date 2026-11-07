package builtin_test

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestNumericDivisorsExactScale(t *testing.T) {
	for _, test := range []struct {
		name, left, right, gcd, lcm string
	}{
		{"fractional mixed scales", "1.20", "0.045", "0.015", "3.600"},
		{"opposite signs", "-12.50", "7.5", "2.50", "37.50"},
		{"both negative", "-0.12", "-0.18", "0.06", "0.36"},
		{"equal coefficients different scales", "1.2", "-0.012", "0.012", "1.200"},
		{"equal magnitudes different scales", "12.00", "-12.0", "12.00", "12.00"},
		{"zero with greater scale", "0.000", "-5.5", "5.500", "0.000"},
		{"zero with lower scale", "0", "0.125", "0.125", "0.000"},
		{"both zero", "-0.00", "0.000", "0.000", "0.000"},
		{"beyond binary float precision", "9007199254740993", "9007199254740995", "1", "81129638414606717724586024108035"},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, operation := range []struct {
				id   model.FunctionID
				want string
			}{{5048, test.gcd}, {5049, test.lcm}} {
				function, err := builtin.Resolve(operation.id, builtin.ResolveOptions{})
				if err != nil {
					t.Fatal(err)
				}
				arguments := []model.Value{numericDatum(test.left), numericDatum(test.right)}
				for range 2 {
					value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
					if err != nil || value.IsNull || value.Type != model.TypeNumeric || value.StrVal != operation.want {
						t.Fatalf("function %d(%s, %s) = %v, %v; want numeric %s", operation.id, arguments[0].StrVal, arguments[1].StrVal, value, err, operation.want)
					}
					arguments[0], arguments[1] = arguments[1], arguments[0]
				}
			}
		})
	}
}

func TestNumericDivisorsSpecialPrecedence(t *testing.T) {
	for _, id := range []model.FunctionID{5048, 5049} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, pair := range [][2]string{
			{"NaN", "0.00"},
			{"Infinity", "0.00"},
			{"-Infinity", "1.25"},
			{"Infinity", "-Infinity"},
		} {
			arguments := []model.Value{numericDatum(pair[0]), numericDatum(pair[1])}
			for range 2 {
				value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
				if err != nil || value.IsNull || value.Type != model.TypeNumeric || value.StrVal != "NaN" {
					t.Fatalf("function %d(%s, %s) = %v, %v; want numeric NaN", id, arguments[0].StrVal, arguments[1].StrVal, value, err)
				}
				arguments[0], arguments[1] = arguments[1], arguments[0]
			}
		}
		value, err := function.Call(t.Context(), []model.Value{numericDatum("Infinity"), model.NullVal(model.TypeNumeric)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull || value.Type != model.TypeNumeric {
			t.Fatalf("function %d(Infinity, NULL) = %v, %v; want numeric NULL", id, value, err)
		}
	}
}

func TestNumericDivisorsOutputBudget(t *testing.T) {
	for _, test := range []struct {
		id                model.FunctionID
		left, right, want string
	}{
		{5048, "1.20", "0.045", "0.015"},
		{5049, "1.20", "0.045", "3.600"},
		{5049, "0.000", "12", "0.000"},
		{5048, "Infinity", "0", "NaN"},
		{5049, "NaN", "0", "NaN"},
	} {
		function, err := builtin.Resolve(test.id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		arguments := []model.Value{numericDatum(test.left), numericDatum(test.right)}
		_, err = function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(test.want) - 1), MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54000")
		value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(test.want)), MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != test.want {
			t.Fatalf("function %d with exact output budget = %v, %v; want %s", test.id, value, err, test.want)
		}
	}
}

func TestNumericLCMDividesBeforeProductAdmission(t *testing.T) {
	function, err := builtin.Resolve(5049, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "1" + strings.Repeat("0", 20000)
	arguments := []model.Value{numericDatum("1e20000"), numericDatum("1e19999")}
	for range 2 {
		value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != want {
			t.Fatalf("LCM of overlapping powers did not return the larger power: %v", err)
		}
		arguments[0], arguments[1] = arguments[1], arguments[0]
	}
	_, err = function.Call(t.Context(), []model.Value{numericDatum("1e20000"), numericDatum(strings.Repeat("9", 20000))}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestNumericDivisorsWorkBoundaryShortcuts(t *testing.T) {
	want := "1" + strings.Repeat("0", 32767)
	for _, id := range []model.FunctionID{5048, 5049} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		value, err := function.Call(t.Context(), []model.Value{numericDatum("1e32767"), numericDatum("-1e32767")}, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != want {
			t.Fatalf("function %d with equal magnitudes at the work boundary failed: %v", id, err)
		}
	}
	function, err := builtin.Resolve(5049, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	arguments := []model.Value{numericDatum("1e32767"), numericDatum("0.000")}
	for range 2 {
		value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != "0.000" {
			t.Fatalf("LCM with zero unnecessarily aligned the other operand: %v, %v", value, err)
		}
		arguments[0], arguments[1] = arguments[1], arguments[0]
	}
	function, err = builtin.Resolve(5048, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = function.Call(t.Context(), []model.Value{numericDatum("1e20000"), numericDatum("1e-16383")}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestNumericDivisorsRepeatedCallsPreserveArguments(t *testing.T) {
	gcd, err := builtin.Resolve(5048, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lcm, err := builtin.Resolve(5049, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	arguments := []model.Value{numericDatum("-12.50"), numericDatum("7.5")}
	first, err := gcd.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	multiple, err := lcm.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
	if err != nil || multiple.StrVal != "37.50" {
		t.Fatalf("LCM after GCD = %v, %v; want 37.50", multiple, err)
	}
	again, err := gcd.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
	if err != nil || again.StrVal != "2.50" || first.StrVal != "2.50" || arguments[0].StrVal != "-12.50" || arguments[1].StrVal != "7.5" {
		t.Fatalf("repeated calls changed operands or prior result: args=%v, first=%v, again=%v, error=%v", arguments, first, again, err)
	}
}

func TestNumericDivisorsCancellationDuringEuclideanWork(t *testing.T) {
	left, right := big.NewInt(1), big.NewInt(1)
	for range 256 {
		left, right = right, new(big.Int).Add(left, right)
	}
	arguments := []model.Value{numericDatum(left.String()), numericDatum(right.String())}
	for _, id := range []model.FunctionID{5048, 5049} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		// Short inputs leave the 64th checkpoint inside the long remainder chain.
		interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 64}
		_, err = function.Call(interrupted, arguments, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("function %d did not cancel during Euclidean work: %v", id, err)
		}
	}
}
