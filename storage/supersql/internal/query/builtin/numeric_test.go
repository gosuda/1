package builtin_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func numericDatum(text string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: text} }

func TestNumericExactArithmeticAndScale(t *testing.T) {
	for _, test := range []struct{ name, operation, left, right, want string }{
		{"decimal addition", "numeric_add", "0.1", "0.2", "0.3"},
		{"wide coefficient", "numeric_add", "999999999999999999999999999999.99", "0.01", "1000000000000000000000000000000.00"},
		{"subtraction scale", "numeric_sub", "1.000", "0.2", "0.800"},
		{"multiplication scale", "numeric_mul", "1.20", "2.00", "2.4000"},
		{"division leading group", "numeric_div", "1", "2", "0.50000000000000000000"},
		{"division exact equal", "numeric_div", "1", "1", "1.00000000000000000000"},
		{"division scale", "numeric_div", "10000", "2", "5000.0000000000000000"},
		{"division rounding", "numeric_div", "2", "3", "0.66666666666666666667"},
		{"negative remainder", "numeric_mod", "-5.50", "2.0", "-1.50"},
		{"truncated quotient", "div", "-5.50", "2.0", "-2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value, err := callable(t, test.operation, 1700, 1700).Call(t.Context(), []model.Value{numericDatum(test.left), numericDatum(test.right)}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != test.want || value.Type != model.TypeNumeric {
				t.Fatalf("got %v, %v; want numeric %s", value, err, test.want)
			}
		})
	}
}

func TestNumericInputNormalizesWithoutLosingScale(t *testing.T) {
	function := callable(t, "numeric_in", 2275, 26, 23)
	for _, test := range []struct{ input, want string }{
		{"  +001.2300e+1  ", "12.300"}, {"-.000", "0.000"},
		{"1_234.5_0e-1", "123.450"}, {".5", "0.5"}, {"1.", "1"},
		{"0x_ff", "255"}, {"-0b1_01", "-5"}, {"0o77", "63"},
		{"nAn", "NaN"}, {"+iNf", "Infinity"}, {"-INFINITY", "-Infinity"},
	} {
		t.Run(test.input, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{model.StringVal(test.input), model.Int64Val(0), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != test.want {
				t.Fatalf("numeric_in(%q) = %v, %v; want %s", test.input, value, err, test.want)
			}
			output, err := callable(t, "numeric_out", 1700).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if err != nil || output.StrVal != test.want {
				t.Fatalf("numeric_out = %v, %v; want %s", output, err, test.want)
			}
		})
	}
}

func TestNumericInputRejectsMalformedText(t *testing.T) {
	function := callable(t, "numeric_in", 2275, 26, 23)
	for _, input := range []string{"", ".", "+NaN", "-NaN", "1..2", "1._2", "1__2", "1e", "1e+", "1e_2", "1e1__2", "0x", "0x__1", "0b2", "1 2", "Infinityx", "1\x00"} {
		t.Run(input, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{model.StringVal(input), model.Int64Val(0), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22P02")
		})
	}
}

func TestNumericRoundingUsesDecimalTiesAndNegativeScale(t *testing.T) {
	for _, test := range []struct {
		operation, input string
		scale            int64
		want             string
	}{
		{"round", "2.5", 0, "3"}, {"round", "-2.5", 0, "-3"},
		{"round", "1499.5", -3, "1000"}, {"round", "1500", -3, "2000"},
		{"round", "-1500", -3, "-2000"}, {"round", "1.2", 4, "1.2000"},
		{"trunc", "-2.9", 0, "-2"}, {"trunc", "-1999", -3, "-1000"},
		{"round", "9", math.MinInt32, "0"}, {"trunc", "9", math.MinInt32, "0"},
	} {
		t.Run(test.operation+test.input+test.want, func(t *testing.T) {
			value, err := callable(t, test.operation, 1700, 23).Call(t.Context(), []model.Value{numericDatum(test.input), model.Int64Val(test.scale)}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != test.want {
				t.Fatalf("got %v, %v; want %s", value, err, test.want)
			}
		})
	}
}

func TestNumericTypmodRoundsBeforeOverflow(t *testing.T) {
	function := callable(t, "numeric", 1700, 23)
	for _, test := range []struct {
		input            string
		precision, scale int
		want, state      string
	}{
		{"9.994", 3, 2, "9.99", ""}, {"9.995", 3, 2, "", "22003"},
		{"1499", 1, -3, "1000", ""}, {"9500", 1, -3, "", "22003"},
		{"0.0094", 1, 3, "0.009", ""}, {"0.0095", 1, 3, "", "22003"},
		{"Infinity", 3, 2, "", "22003"}, {"NaN", 3, 2, "NaN", ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			typmod := int64((test.precision<<16)|(test.scale&2047)) + 4
			value, err := function.Call(t.Context(), []model.Value{numericDatum(test.input), model.Int64Val(typmod)}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if test.state != "" {
				requireSQLState(t, err, test.state)
				return
			}
			if err != nil || value.StrVal != test.want {
				t.Fatalf("got %v, %v; want %s", value, err, test.want)
			}
		})
	}
}

func TestNumericInputTypmodCanReduceExcessScale(t *testing.T) {
	value, err := callable(t, "numeric_in", 2275, 26, 23).Call(t.Context(), []model.Value{model.StringVal("1e-20000"), model.Int64Val(0), model.Int64Val((3 << 16) + 2 + 4)}, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "0.00" {
		t.Fatalf("typmod input = %v, %v; want 0.00", value, err)
	}
}

func TestNumericUnaryPreservesScaleExceptSign(t *testing.T) {
	for _, test := range []struct{ operation, input, want string }{
		{"numeric_uminus", "1.200", "-1.200"}, {"numeric_uminus", "-Infinity", "Infinity"},
		{"numeric_uplus", "-1.200", "-1.200"}, {"abs", "-1.200", "1.200"},
		{"numeric_abs", "-Infinity", "Infinity"}, {"sign", "-0.001", "-1"},
		{"sign", "0.00", "0"}, {"sign", "Infinity", "1"}, {"sign", "NaN", "NaN"},
	} {
		t.Run(test.operation+test.input, func(t *testing.T) {
			value, err := callable(t, test.operation, 1700).Call(t.Context(), []model.Value{numericDatum(test.input)}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != test.want {
				t.Fatalf("got %v, %v; want %s", value, err, test.want)
			}
		})
	}
}

func TestNumericSpecialArithmetic(t *testing.T) {
	for _, test := range []struct{ operation, left, right, want, state string }{
		{"numeric_add", "Infinity", "-Infinity", "NaN", ""},
		{"numeric_sub", "-Infinity", "Infinity", "-Infinity", ""},
		{"numeric_mul", "Infinity", "0.00", "NaN", ""},
		{"numeric_mul", "-Infinity", "-2", "Infinity", ""},
		{"numeric_div", "1.00", "Infinity", "0", ""},
		{"numeric_div", "Infinity", "Infinity", "NaN", ""},
		{"numeric_div", "NaN", "0", "NaN", ""},
		{"numeric_mod", "2.00", "Infinity", "2.00", ""},
		{"numeric_mod", "Infinity", "2", "NaN", ""},
		{"numeric_div", "Infinity", "0", "", "22012"},
		{"numeric_mod", "Infinity", "0", "", "22012"},
		{"numeric_div", "0", "0", "", "22012"},
	} {
		t.Run(test.operation+test.left+test.right, func(t *testing.T) {
			value, err := callable(t, test.operation, 1700, 1700).Call(t.Context(), []model.Value{numericDatum(test.left), numericDatum(test.right)}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if test.state != "" {
				requireSQLState(t, err, test.state)
				return
			}
			if err != nil || value.StrVal != test.want {
				t.Fatalf("got %v, %v; want %s", value, err, test.want)
			}
		})
	}
}

func TestNumericComparisonTotalOrder(t *testing.T) {
	ordered := []string{"-Infinity", "-999999999999999999999999", "0", "0.1", "Infinity", "NaN"}
	for i, left := range ordered {
		for j, right := range ordered {
			want := int64(0)
			if i < j {
				want = -1
			}
			if i > j {
				want = 1
			}
			value, err := callable(t, "numeric_cmp", 1700, 1700).Call(t.Context(), []model.Value{numericDatum(left), numericDatum(right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.IntVal != want {
				t.Fatalf("cmp(%s,%s) = %v, %v; want %d", left, right, value, err, want)
			}
		}
	}
	value, err := callable(t, "numeric_eq", 1700, 1700).Call(t.Context(), []model.Value{numericDatum("0.10"), numericDatum("0.1")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IntVal != 1 {
		t.Fatalf("scale-independent equality = %v, %v", value, err)
	}
}

func TestNumericPrimitiveCastBoundaries(t *testing.T) {
	for _, test := range []struct {
		target, input string
		want          int64
		state         string
	}{
		{"int2", "32767.4", 32767, ""}, {"int2", "32767.5", 0, "22003"},
		{"int2", "-32768.4", -32768, ""}, {"int2", "-32768.5", 0, "22003"},
		{"int4", "-2.5", -3, ""}, {"int4", "2147483647.5", 0, "22003"},
		{"int8", "9223372036854775807.4", math.MaxInt64, ""},
		{"int8", "9223372036854775807.5", 0, "22003"},
		{"int8", "-9223372036854775808.4", math.MinInt64, ""},
		{"int8", "-9223372036854775808.5", 0, "22003"},
		{"int8", "NaN", 0, "22003"}, {"int4", "Infinity", 0, "22003"},
	} {
		t.Run(test.target+test.input, func(t *testing.T) {
			value, err := callable(t, test.target, 1700).Call(t.Context(), []model.Value{numericDatum(test.input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if test.state != "" {
				requireSQLState(t, err, test.state)
				return
			}
			if err != nil || value.IntVal != test.want {
				t.Fatalf("got %v, %v; want %d", value, err, test.want)
			}
		})
	}
	for _, test := range []struct {
		source model.TypeID
		input  model.Value
		want   string
	}{
		{20, model.Int64Val(math.MaxInt64), "9223372036854775807"},
		{701, model.Float64Val(1.2345678901234567), "1.23456789012346"},
		{700, model.Float64Val(float64(float32(1.2345678))), "1.23457"},
	} {
		value, err := callable(t, "numeric", test.source).Call(t.Context(), []model.Value{test.input}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != test.want {
			t.Fatalf("numeric(%v) = %v, %v; want %s", test.input, value, err, test.want)
		}
	}
	for _, test := range []struct{ target, input string }{{"float4", "1e39"}, {"float8", "1e309"}, {"float4", "1e-100"}, {"float8", "1e-1000"}} {
		_, err := callable(t, test.target, 1700).Call(t.Context(), []model.Value{numericDatum(test.input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22003")
	}
	value, err := callable(t, "float8", 1700).Call(t.Context(), []model.Value{numericDatum("0.1")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.FloatVal != 0.1 {
		t.Fatalf("float8(0.1) = %v, %v", value, err)
	}
}

func TestNumericResourceAndFormatLimits(t *testing.T) {
	input := callable(t, "numeric_in", 2275, 26, 23)
	for _, test := range []struct{ text, state string }{
		{"1e100000", "54000"}, {"1e131072", "22003"},
		{"1e-20000", "22003"}, {"1e99999999999999999", "22003"},
		{strings.Repeat("1", 65537), "54000"},
	} {
		_, err := input.Call(t.Context(), []model.Value{model.StringVal(test.text), model.Int64Val(0), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, test.state)
	}
	_, err := callable(t, "numeric_add", 1700, 1700).Call(t.Context(), []model.Value{numericDatum("0.1"), numericDatum("0.2")}, builtin.CallBudget{OutputBytes: 2, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	_, err = callable(t, "numeric_mul", 1700, 1700).Call(t.Context(), []model.Value{numericDatum(strings.Repeat("9", 20000)), numericDatum(strings.Repeat("9", 20000))}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = input.Call(ctx, []model.Value{model.StringVal("1e100000"), model.Int64Val(0), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled numeric input error = %v", err)
	}
}
