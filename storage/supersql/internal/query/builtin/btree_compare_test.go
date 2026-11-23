package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func btreeComparisonCallable(t *testing.T, id model.FunctionID, options builtin.ResolveOptions) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, options)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func TestBtreeIntegerComparisonBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name       string
		id         model.FunctionID
		low, high  int64
		difference int64
		storage    model.DataType
	}{
		{"int2", 350, math.MinInt16, math.MaxInt16, 65535, model.TypeInt64},
		{"int4", 351, math.MinInt32, math.MaxInt32, 1, model.TypeInt64},
		{"int8", 842, math.MinInt64, math.MaxInt64, 1, model.TypeInt64},
		{"oid", 356, 0, math.MaxUint32, 1, model.TypeInt64},
		{"oid_signed_boundary", 356, math.MaxInt32, math.MaxInt32 + 1, 1, model.TypeInt64},
		{"bool", 1693, 0, 1, 1, model.TypeBoolean},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := btreeComparisonCallable(t, tc.id, builtin.ResolveOptions{})
			for _, pair := range []struct{ left, right, want int64 }{
				{tc.low, tc.high, -tc.difference},
				{tc.high, tc.low, tc.difference},
				{tc.high, tc.high, 0},
			} {
				args := []model.Value{{Type: tc.storage, IntVal: pair.left}, {Type: tc.storage, IntVal: pair.right}}
				got, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || got.IsNull || got.Type != model.TypeInt64 || got.IntVal != pair.want {
					t.Fatalf("compare(%d, %d) = %v, %v; want %d", pair.left, pair.right, got, err, pair.want)
				}
			}
		})
	}
}

func TestBtreeOIDEqualityUsesUnsignedDomain(t *testing.T) {
	for _, tc := range []struct {
		id          model.FunctionID
		left, right int64
		want        bool
	}{
		{184, math.MaxUint32, math.MaxUint32, true},
		{184, math.MaxInt32, math.MaxInt32 + 1, false},
		{185, math.MaxUint32, math.MaxUint32, false},
		{185, 0, math.MaxUint32, true},
	} {
		t.Run(fmt.Sprintf("%d_%d_%d", tc.id, tc.left, tc.right), func(t *testing.T) {
			function := btreeComparisonCallable(t, tc.id, builtin.ResolveOptions{})
			got, err := function.Call(t.Context(), []model.Value{model.Int64Val(tc.left), model.Int64Val(tc.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.Type != model.TypeBoolean || got.IntVal != model.BoolVal(tc.want).IntVal {
				t.Fatalf("OID equality = %v, %v; want %t", got, err, tc.want)
			}
		})
	}
}

func TestBtreeFloatComparisonPostgresOrder(t *testing.T) {
	for _, width := range []struct {
		id                model.FunctionID
		maximum, smallest float64
	}{
		{354, math.MaxFloat32, math.SmallestNonzeroFloat32},
		{355, math.MaxFloat64, math.SmallestNonzeroFloat64},
	} {
		t.Run(fmt.Sprint(width.id), func(t *testing.T) {
			function := btreeComparisonCallable(t, width.id, builtin.ResolveOptions{})
			for _, tc := range []struct {
				name        string
				left, right float64
				want        int64
			}{
				{"distinct_nan_payloads", math.Float64frombits(0x7ff8000000000001), math.Float64frombits(0xfff8000000000002), 0},
				{"nan_above_infinity", math.NaN(), math.Inf(1), 1},
				{"infinity_below_nan", math.Inf(1), math.NaN(), -1},
				{"positive_infinity", math.Inf(1), width.maximum, 1},
				{"negative_infinity", math.Inf(-1), -width.maximum, -1},
				{"equal_infinities", math.Inf(-1), math.Inf(-1), 0},
				{"signed_zero", math.Copysign(0, -1), 0, 0},
				{"positive_subnormal", width.smallest, 0, 1},
				{"negative_subnormal", -width.smallest, math.Copysign(0, -1), -1},
				{"opposite_extremes", -width.maximum, width.maximum, -1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got, err := function.Call(t.Context(), []model.Value{model.Float64Val(tc.left), model.Float64Val(tc.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					if err != nil || got.IsNull || got.Type != model.TypeInt64 || got.IntVal != tc.want {
						t.Fatalf("compare(%g, %g) = %v, %v; want %d", tc.left, tc.right, got, err, tc.want)
					}
				})
			}
		})
	}
}

func TestBtreeNameComparisonUsesCByteOrder(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right string
		want        int64
	}{
		{"empty", "", "", 0},
		{"case_sensitive", "Z", "a", -1},
		{"non_ascii", "é", "z", 1},
		{"trailing_space", "name ", "name", 1},
		{"prefix", "nam", "name", -1},
		{"unicode_equal", "日本語", "日本語", 0},
		{"no_unicode_normalization", "é", "e\u0301", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []model.FunctionID{62, 246, 253, 359} {
				function := btreeComparisonCallable(t, id, builtin.ResolveOptions{Collation: 950})
				want, storage := tc.want, model.TypeInt64
				if id == 62 {
					want, storage = model.BoolVal(tc.want == 0).IntVal, model.TypeBoolean
				}
				got, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.left), model.StringVal(tc.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || got.IsNull || got.Type != storage || got.IntVal != want {
					t.Fatalf("function %d(%q, %q) = %v, %v; want %d", id, tc.left, tc.right, got, err, want)
				}
			}
		})
	}
}

func TestBtreeNameCollationErrorsFollowStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{62, 246, 253, 359} {
		for _, options := range []builtin.ResolveOptions{
			{}, {Collation: 100}, {Collation: 100, DefaultCollation: 100}, {Collation: 123456}, {Collation: 0, DefaultCollation: 950},
		} {
			function := btreeComparisonCallable(t, id, options)
			_, err := function.Call(t.Context(), []model.Value{model.StringVal("same"), model.StringVal("same")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "42P22")
			for index := range 2 {
				args := []model.Value{model.StringVal("\xff"), model.StringVal("\xff")}
				args[index] = model.NullVal(model.TypeString)
				got, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !got.IsNull {
					t.Fatalf("function %d NULL argument %d with collation %+v = %v, %v", id, index, options, got, err)
				}
			}
		}
		for _, options := range []builtin.ResolveOptions{{Collation: 950}, {Collation: 951}, {Collation: 100, DefaultCollation: 951}} {
			function := btreeComparisonCallable(t, id, options)
			want := int64(-1)
			if id == 62 {
				want = 0
			}
			got, err := function.Call(t.Context(), []model.Value{model.StringVal("Z"), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.IntVal != want {
				t.Fatalf("function %d with collation %+v = %v, %v; want %d", id, options, got, err, want)
			}
		}
	}
}

func TestBtreeComparisonRejectsWrongDomains(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		bad  model.Value
		good model.Value
		code string
	}{
		{"int2_below_minimum", 350, model.Int64Val(math.MinInt16 - 1), model.Int64Val(0), "22003"},
		{"int2_above_maximum", 350, model.Int64Val(math.MaxInt16 + 1), model.Int64Val(0), "22003"},
		{"int4_below_minimum", 351, model.Int64Val(math.MinInt32 - 1), model.Int64Val(0), "22003"},
		{"int4_above_maximum", 351, model.Int64Val(math.MaxInt32 + 1), model.Int64Val(0), "22003"},
		{"int8_float_storage", 842, model.Float64Val(1), model.Int64Val(0), "42804"},
		{"oid_negative", 356, model.Int64Val(-1), model.Int64Val(0), "22003"},
		{"oid_above_maximum", 356, model.Int64Val(math.MaxUint32 + 1), model.Int64Val(0), "22003"},
		{"oideq_negative", 184, model.Int64Val(-1), model.Int64Val(0), "22003"},
		{"oidne_above_maximum", 185, model.Int64Val(math.MaxUint32 + 1), model.Int64Val(0), "22003"},
		{"oid_boolean_storage", 356, model.BoolVal(true), model.Int64Val(0), "42804"},
		{"bool_noncanonical", 1693, model.Value{Type: model.TypeBoolean, IntVal: 2}, model.BoolVal(false), "42804"},
		{"float4_unrounded", 354, model.Float64Val(1.1), model.Float64Val(0), "42804"},
		{"float4_overflow", 354, model.Float64Val(math.MaxFloat64), model.Float64Val(0), "42804"},
		{"float4_underflow", 354, model.Float64Val(math.SmallestNonzeroFloat64), model.Float64Val(0), "42804"},
		{"float8_integer_storage", 355, model.Int64Val(0), model.Float64Val(0), "42804"},
		{"nameeq_invalid_utf8", 62, model.StringVal("\xff"), model.StringVal("a"), "22021"},
		{"namecmp_invalid_utf8", 359, model.StringVal("\xff"), model.StringVal("a"), "22021"},
		{"namecmp_nul", 359, model.StringVal("a\x00b"), model.StringVal("a"), "22021"},
		{"nameeq_integer_storage", 62, model.Int64Val(0), model.StringVal("a"), "42804"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := btreeComparisonCallable(t, tc.id, builtin.ResolveOptions{Collation: 950})
			for index := range 2 {
				args := []model.Value{tc.good, tc.good}
				args[index] = tc.bad
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, tc.code)
			}
		})
	}
}

func TestBtreeComparisonStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{350, 351, 354, 355, 356, 359, 842, 1693, 62, 184, 185, 2188, 2189, 2190, 2191, 2192, 2193, 2194, 2195, 246, 253} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := btreeComparisonCallable(t, id, builtin.ResolveOptions{})
			argument, result := model.Int64Val(0), model.TypeInt64
			switch id {
			case 354, 355, 2194, 2195:
				argument = model.Float64Val(0)
			case 359, 62, 246, 253:
				argument = model.StringVal("name")
			case 1693:
				argument = model.BoolVal(false)
			}
			if id == 62 || id == 184 || id == 185 {
				result = model.TypeBoolean
			}
			for index := range 2 {
				args := []model.Value{argument, argument}
				args[index] = model.NullVal(argument.Type)
				got, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !got.IsNull || got.Type != result {
					t.Fatalf("NULL argument %d = %v, %v; want NULL storage %v", index, got, err, result)
				}
			}
		})
	}
}

func TestBtreeComparisonCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, id := range []model.FunctionID{350, 351, 354, 355, 356, 359, 842, 1693, 62, 184, 185, 2188, 2189, 2190, 2191, 2192, 2193, 2194, 2195, 246, 253} {
		function := btreeComparisonCallable(t, id, builtin.ResolveOptions{Collation: 950})
		argument := model.Int64Val(0)
		switch id {
		case 354, 355, 2194, 2195:
			argument = model.Float64Val(0)
		case 359, 62, 246, 253:
			argument = model.StringVal("name")
		case 1693:
			argument = model.BoolVal(false)
		}
		_, err := function.Call(ctx, []model.Value{argument, argument}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("function %d canceled call = %v", id, err)
		}
	}
	for _, id := range []model.FunctionID{62, 246, 253, 359} {
		function := btreeComparisonCallable(t, id, builtin.ResolveOptions{Collation: 950})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 4}
		input := model.StringVal(strings.Repeat("name", 1<<16))
		_, err := function.Call(interrupted, []model.Value{input, input}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("function %d interrupted comparison = %v", id, err)
		}
	}
}

func TestBtreeCrossIntegerComparisonPreservesOperandWidths(t *testing.T) {
	for _, tc := range []struct {
		id                                   model.FunctionID
		leftMin, leftMax, rightMin, rightMax int64
	}{
		{2188, math.MinInt32, math.MaxInt32, math.MinInt64, math.MaxInt64},
		{2189, math.MinInt64, math.MaxInt64, math.MinInt32, math.MaxInt32},
		{2190, math.MinInt16, math.MaxInt16, math.MinInt32, math.MaxInt32},
		{2191, math.MinInt32, math.MaxInt32, math.MinInt16, math.MaxInt16},
		{2192, math.MinInt16, math.MaxInt16, math.MinInt64, math.MaxInt64},
		{2193, math.MinInt64, math.MaxInt64, math.MinInt16, math.MaxInt16},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			function := btreeComparisonCallable(t, tc.id, builtin.ResolveOptions{})
			for _, pair := range []struct{ left, right, want int64 }{
				{tc.leftMin, tc.rightMax, -1},
				{tc.leftMax, tc.rightMin, 1},
				{-1, -1, 0},
				{-1, 0, -1},
			} {
				got, err := function.Call(t.Context(), []model.Value{model.Int64Val(pair.left), model.Int64Val(pair.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || got.IsNull || got.Type != model.TypeInt64 || got.IntVal != pair.want {
					t.Fatalf("compare(%d, %d) = %v, %v; want %d", pair.left, pair.right, got, err, pair.want)
				}
			}
			for i, bound := range []int64{tc.leftMax, tc.rightMax} {
				if bound == math.MaxInt64 {
					continue
				}
				args := []model.Value{model.Int64Val(0), model.Int64Val(0)}
				args[i] = model.Int64Val(bound + 1)
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22003")
			}
		})
	}
}

func TestBtreeCrossFloatComparisonWidensOnlyFloat4(t *testing.T) {
	for _, id := range []model.FunctionID{2194, 2195} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := btreeComparisonCallable(t, id, builtin.ResolveOptions{})
			for _, tc := range []struct {
				name           string
				single, double float64
				want           int64
			}{
				{"double_precision_retained", 16777216, 16777217, -1},
				{"double_subnormal_retained", 0, math.SmallestNonzeroFloat64, -1},
				{"double_range_retained", math.MaxFloat32, math.MaxFloat64, -1},
				{"nan_above_infinity", math.NaN(), math.Inf(1), 1},
				{"infinity_below_nan", math.Inf(1), math.NaN(), -1},
				{"distinct_nan_payloads", math.Float64frombits(0x7ff8000000000001), math.Float64frombits(0xfff8000000000002), 0},
				{"signed_zero", math.Copysign(0, -1), 0, 0},
				{"equal_finite", math.SmallestNonzeroFloat32, math.SmallestNonzeroFloat32, 0},
			} {
				t.Run(tc.name, func(t *testing.T) {
					left, right, want := tc.single, tc.double, tc.want
					if id == 2195 {
						left, right, want = right, left, -want
					}
					got, err := function.Call(t.Context(), []model.Value{model.Float64Val(left), model.Float64Val(right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					if err != nil || got.IsNull || got.Type != model.TypeInt64 || got.IntVal != want {
						t.Fatalf("compare(%g, %g) = %v, %v; want %d", left, right, got, err, want)
					}
				})
			}
			args := []model.Value{model.Float64Val(0), model.Float64Val(0)}
			index := 0
			if id == 2195 {
				index = 1
			}
			args[index] = model.Float64Val(1.1)
			_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "42804")
		})
	}
}

func TestBtreeNameTextComparisonDoesNotTruncateText(t *testing.T) {
	name := strings.Repeat("n", 63)
	for _, id := range []model.FunctionID{246, 253} {
		function := btreeComparisonCallable(t, id, builtin.ResolveOptions{Collation: 950})
		left, right, want := name, name+" ", int64(-1)
		if id == 253 {
			left, right, want = right, left, -want
		}
		got, err := function.Call(t.Context(), []model.Value{model.StringVal(left), model.StringVal(right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || got.IsNull || got.Type != model.TypeInt64 || got.IntVal != want {
			t.Fatalf("function %d compared truncated text: %v, %v; want %d", id, got, err, want)
		}
		for i := range 2 {
			args := []model.Value{model.StringVal("a"), model.StringVal("a")}
			args[i] = model.StringVal("a\x00b")
			_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22021")
		}
	}
}
