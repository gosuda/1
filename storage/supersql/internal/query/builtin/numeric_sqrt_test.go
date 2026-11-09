package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/decimalint"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func sqrtWorkspace(t *testing.T) *decimalint.Workspace {
	t.Helper()
	work, err := decimalint.NewWorkspace(t.Context(), nil, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := work.Close(); err != nil {
			t.Error(err)
		}
	})
	return work
}

func sqrtDecimal(t *testing.T, work *decimalint.Workspace, text string) decimal {
	t.Helper()
	value, err := parseNumeric(t.Context(), work, text)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func requireSqrtSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.SQLState() != state {
		t.Fatalf("error = %v, want SQLSTATE %s", err, state)
	}
}

func TestNumericSqrtExactRounding(t *testing.T) {
	for _, test := range []struct {
		name, input string
		scale       int
		want        string
	}{
		{"irrational", "2", 20, "1.41421356237309504880"},
		{"perfect square preserves scale", "4", 3, "2.000"},
		{"wide perfect square", "15241578750190521", 0, "123456789"},
		{"integer radicand rounds up", "3", 0, "2"},
		{"integer radicand rounds down", "2", 0, "1"},
		{"integer midpoint tie", "2.25", 0, "2"},
		{"below integer midpoint", "2.249999999999999999999999999999", 0, "1"},
		{"above integer midpoint", "2.250000000000000000000000000001", 0, "2"},
		{"fractional midpoint tie", "1.5625", 1, "1.3"},
		{"below fractional midpoint", "1.562499999999999999999999999999", 1, "1.2"},
		{"zero midpoint tie", "0.25", 0, "1"},
		{"below zero midpoint", "0.249999999999999999999999999999", 0, "0"},
		{"tenths below midpoint", "0.2", 0, "0"},
		{"tenths above midpoint", "0.3", 0, "1"},
		{"zero caller scale", "-0.000", 7, "0.0000000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			work := sqrtWorkspace(t)
			result, err := numericSqrt(t.Context(), work, sqrtDecimal(t, work, test.input), test.scale)
			if err != nil {
				t.Fatal(err)
			}
			text, err := numericText(t.Context(), result, 1024)
			if err != nil || text != test.want {
				t.Fatalf("sqrt(%s), scale %d = %q, %v; want %s", test.input, test.scale, text, err, test.want)
			}
		})
	}
}

func TestNumericSqrtLargeMagnitudes(t *testing.T) {
	for _, test := range []struct {
		name, input string
		scale       int
		want        string
	}{
		{"huge coefficient", "1e20000", 0, "1" + strings.Repeat("0", 10000)},
		{"tiny exact root", "1e-16000", 8000, "0." + strings.Repeat("0", 7999) + "1"},
		{"tiny rounded zero", "1e-16001", 1000, "0." + strings.Repeat("0", 1000)},
	} {
		t.Run(test.name, func(t *testing.T) {
			work := sqrtWorkspace(t)
			result, err := numericSqrt(t.Context(), work, sqrtDecimal(t, work, test.input), test.scale)
			if err != nil {
				t.Fatal(err)
			}
			text, err := numericText(t.Context(), result, numericInputBytes)
			if err != nil || text != test.want {
				t.Fatalf("sqrt(%s), scale %d did not produce the exact root: %v", test.input, test.scale, err)
			}
		})
	}
}

func TestNumericSqrtSpecialValues(t *testing.T) {
	for _, input := range []string{"NaN", "Infinity"} {
		t.Run(input, func(t *testing.T) {
			work := sqrtWorkspace(t)
			result, err := numericSqrt(t.Context(), work, sqrtDecimal(t, work, input), 20)
			if err != nil {
				t.Fatal(err)
			}
			text, err := numericText(t.Context(), result, 8)
			if err != nil || text != input {
				t.Fatalf("sqrt(%s) = %q, %v", input, text, err)
			}
		})
	}
	for _, input := range []string{"-1", "-1e-16383", "-Infinity"} {
		t.Run(input, func(t *testing.T) {
			work := sqrtWorkspace(t)
			_, err := numericSqrt(t.Context(), work, sqrtDecimal(t, work, input), 20)
			requireSqrtSQLState(t, err, "2201F")
		})
	}
}

func TestNumericSqrtOwnsResult(t *testing.T) {
	for _, input := range []string{"0.000", "15241578750190521", "1.5625"} {
		t.Run(input, func(t *testing.T) {
			work := sqrtWorkspace(t)
			value := sqrtDecimal(t, work, input)
			before, err := numericText(t.Context(), value, 1024)
			if err != nil {
				t.Fatal(err)
			}
			result, err := numericSqrt(t.Context(), work, value, 1)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := numericText(t.Context(), value, 1024); err != nil || got != before {
				t.Fatalf("borrowed coefficient changed from %s to %s", before, got)
			}
			if err := result.coefficient.Close(); err != nil {
				t.Fatal(err)
			}
			if got, err := numericText(t.Context(), value, 1024); err != nil || got != before {
				t.Fatalf("closing result changed borrowed coefficient from %s to %s", before, got)
			}
		})
	}
}

func TestNumericSqrtWorkLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		scale int
	}{
		{"scaled coefficient", "2", numericWorkDigits / 2},
		{"power", "1", numericWorkDigits},
		{"scale overflow", "1", int(^uint(0) >> 1)},
		{"negative scale", "1", -1},
		{"oversized coefficient", strings.Repeat("9", numericWorkDigits+1), 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			work := sqrtWorkspace(t)
			coefficient, err := work.Parse(t.Context(), test.input, 10)
			if err != nil {
				t.Fatal(err)
			}
			_, err = numericSqrt(t.Context(), work, decimal{coefficient: coefficient}, test.scale)
			requireSqrtSQLState(t, err, "54000")
		})
	}
}

func TestNumericSqrtScalarScale(t *testing.T) {
	for _, id := range []model.FunctionID{1730, 1731} {
		function, err := Resolve(id, ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for _, test := range []struct{ input, want string }{
			{"2", "1.414213562373095"},
			{"10000", "100.0000000000000"},
			{"1e40", "1" + strings.Repeat("0", 20)},
			{"0.1", "0.31622776601683793"},
			{"0.0001", "0.01000000000000000"},
			{"0.00", "0.000000000000000"},
			{"4.00000000000000000000", "2.00000000000000000000"},
			{"1e-2000", "0." + strings.Repeat("0", 999) + "1"},
			{"1e-2002", "0." + strings.Repeat("0", 1000)},
			{"NaN", "NaN"},
			{"Infinity", "Infinity"},
		} {
			value, err := function.Call(t.Context(), []model.Value{{Type: model.TypeNumeric, StrVal: test.input}}, CallBudget{OutputBytes: 2048, MemoryBytes: 64 << 20})
			if err != nil || value.Type != model.TypeNumeric || value.StrVal != test.want {
				t.Fatalf("function %d, sqrt(%s) = %v, %v; want %s", id, test.input, value, err, test.want)
			}
		}
		value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeNumeric)}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull || value.Type != model.TypeNumeric {
			t.Fatalf("function %d, sqrt(NULL) = %v, %v", id, value, err)
		}
		_, err = function.Call(t.Context(), []model.Value{{Type: model.TypeNumeric, StrVal: "2"}}, CallBudget{OutputBytes: 16, MemoryBytes: 64 << 20})
		requireSqrtSQLState(t, err, "54000")
		_, err = function.Call(t.Context(), []model.Value{{Type: model.TypeNumeric, StrVal: "-1"}}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
		requireSqrtSQLState(t, err, "2201F")
	}
}

func TestNumericSqrtCancellation(t *testing.T) {
	work := sqrtWorkspace(t)
	value := sqrtDecimal(t, work, "2")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := numericSqrt(ctx, work, value, 1000)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled square root error = %v", err)
	}
	function, err := Resolve(1730, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = function.Call(ctx, []model.Value{{Type: model.TypeNumeric, StrVal: "2"}}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scalar square root error = %v", err)
	}
}

func TestNumericSqrtAdmitsCoefficientAtWorkLimit(t *testing.T) {
	work := sqrtWorkspace(t)
	value := sqrtDecimal(t, work, strings.Repeat("9", numericWorkDigits))
	result, err := numericSqrt(t.Context(), work, value, 0)
	if err != nil {
		t.Fatal(err)
	}
	text, err := numericText(t.Context(), result, numericWorkDigits)
	want := "1" + strings.Repeat("0", numericWorkDigits/2)
	if err != nil || text != want {
		t.Fatalf("boundary square root has %d digits, error=%v; want rounded power of ten with %d digits", len(text), err, len(want))
	}
}
