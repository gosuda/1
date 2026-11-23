package builtin

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func widthBucketArrayCallable(t *testing.T, elementID model.TypeID, storage model.DataType, options ResolveOptions) Callable {
	t.Helper()
	array := replacementLayout(t, elementID, storage)
	result := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	function, err := ResolveTyped(t.Context(), 3218, []model.TypeLayout{array.Children[0], array}, result, options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireWidthBucketArrayResult(t *testing.T, result model.Value, err error, want int64) {
	t.Helper()
	if err != nil || result.IsNull || result.Type != model.TypeInt64 || result.IntVal != want {
		t.Fatalf("width_bucket = %v, %v; want %d", result, err, want)
	}
}

func widthBucketArrayComparator(id model.TypeID, kernel scalarKernel) Callable {
	return Callable{
		signature:       Signature{Arguments: [8]model.TypeID{id, id}, ArgumentCount: 2, Result: 23},
		argumentStorage: [8]model.DataType{primitiveStorage(id), primitiveStorage(id)},
		resultStorage:   model.TypeInt64,
		kernel:          kernel,
	}
}

func TestWidthBucketArrayCountsThresholdsNotSubscripts(t *testing.T) {
	function := widthBucketArrayCallable(t, 23, model.TypeInt64, ResolveOptions{})
	thresholds := replacementArray(model.Int64Val(1), model.Int64Val(4), model.Int64Val(4), model.Int64Val(9))
	for _, tc := range []struct {
		name       string
		operand    int64
		thresholds model.Value
		want       int64
	}{
		{"below first", 0, thresholds, 0},
		{"equal first", 1, thresholds, 1},
		{"between thresholds", 3, thresholds, 1},
		{"rightmost duplicate", 4, thresholds, 3},
		{"equal last", 9, thresholds, 4},
		{"above last", 10, thresholds, 4},
		{"empty", 4, replacementArray(), 0},
		{"unsorted not validated", 2, replacementArray(model.Int64Val(3), model.Int64Val(1), model.Int64Val(2)), 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := function.Call(t.Context(), []model.Value{model.Int64Val(tc.operand), tc.thresholds}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireWidthBucketArrayResult(t, result, err, tc.want)
		})
	}
}

func TestWidthBucketArrayUsesSelectedComparisonSemantics(t *testing.T) {
	numeric := func(text string) model.Value { return model.Value{Type: model.TypeNumeric, StrVal: text} }
	for _, tc := range []struct {
		name       string
		elementID  model.TypeID
		storage    model.DataType
		operand    model.Value
		thresholds model.Value
		want       int64
	}{
		{"bpchar ignores trailing spaces", 1042, model.TypeString, model.StringVal("a"), replacementArray(model.StringVal("a  "), model.StringVal("b")), 1},
		{"text keeps trailing spaces", 25, model.TypeString, model.StringVal("a"), replacementArray(model.StringVal("a  "), model.StringVal("b")), 0},
		{"numeric scale duplicates", 1700, model.TypeNumeric, numeric("2"), replacementArray(numeric("1"), numeric("2.00"), numeric("2.000"), numeric("10")), 3},
		{"numeric exact precision", 1700, model.TypeNumeric, numeric("9007199254740992"), replacementArray(numeric("9007199254740993")), 0},
		{"numeric NaN operand", 1700, model.TypeNumeric, numeric("NaN"), replacementArray(numeric("Infinity"), numeric("NaN"), numeric("NaN")), 3},
		{"numeric NaN threshold", 1700, model.TypeNumeric, numeric("Infinity"), replacementArray(numeric("-Infinity"), numeric("Infinity"), numeric("NaN")), 2},
		{"float8 NaN operand", 701, model.TypeFloat64, model.Float64Val(math.NaN()), replacementArray(model.Float64Val(math.Inf(1)), model.Float64Val(math.NaN()), model.Float64Val(math.NaN())), 3},
		{"float8 NaN threshold", 701, model.TypeFloat64, model.Float64Val(math.Inf(1)), replacementArray(model.Float64Val(1), model.Float64Val(math.Inf(1)), model.Float64Val(math.NaN())), 2},
		{"float4 NaNs", 700, model.TypeFloat64, model.Float64Val(math.NaN()), replacementArray(model.Float64Val(0), model.Float64Val(math.NaN())), 2},
		{"signed zeros equal", 701, model.TypeFloat64, model.Float64Val(math.Copysign(0, -1)), replacementArray(model.Float64Val(math.Copysign(0, -1)), model.Float64Val(0)), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := widthBucketArrayCallable(t, tc.elementID, tc.storage, ResolveOptions{Collation: 950})
			result, err := function.Call(t.Context(), []model.Value{tc.operand, tc.thresholds}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireWidthBucketArrayResult(t, result, err, tc.want)
		})
	}
}

func TestWidthBucketArrayValidationPrecedesMissingComparison(t *testing.T) {
	function := widthBucketArrayCallable(t, 114, model.TypeString, ResolveOptions{})
	for _, tc := range []struct {
		name       string
		thresholds model.Value
		state      string
	}{
		{"empty still needs comparison", replacementArray(), "42883"},
		{"nonempty missing comparison", replacementArray(model.StringVal("0")), "42883"},
		{"unsearched NULL", replacementArray(model.StringVal("0"), model.StringVal("1"), model.NullVal(model.TypeString)), "22004"},
		{"rank precedes NULL", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}}, ArrayVal: []model.Value{model.NullVal(model.TypeString)}}, "2202E"},
		{"missing dimensions", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal("0")}}, "2202E"},
		{"missing elements", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}}, "2202E"},
		{"length mismatch", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}, ArrayVal: []model.Value{model.StringVal("0")}}, "2202E"},
		{"subscript overflow", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: math.MaxInt32, Length: 2}}, ArrayVal: []model.Value{model.StringVal("0"), model.StringVal("1")}}, "2202E"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{model.StringVal("-1"), tc.thresholds}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, tc.state)
		})
	}
}

func TestWidthBucketArrayRootNullBypassesValidationAndLookup(t *testing.T) {
	function := widthBucketArrayCallable(t, 114, model.TypeString, ResolveOptions{})
	invalid := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal("0")}}
	for _, arguments := range [][2]model.Value{
		{model.NullVal(model.TypeString), invalid},
		{model.StringVal("0"), model.NullVal(model.TypeArray)},
	} {
		result, err := function.Call(t.Context(), arguments[:], CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !result.IsNull || result.Type != model.TypeInt64 {
			t.Fatalf("strict NULL width_bucket = %v, %v", result, err)
		}
	}
}

func TestWidthBucketArrayCollationErrorsFollowProbes(t *testing.T) {
	function := widthBucketArrayCallable(t, 25, model.TypeString, ResolveOptions{})
	result, err := function.Call(t.Context(), []model.Value{model.StringVal("a"), replacementArray()}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireWidthBucketArrayResult(t, result, err, 0)
	_, err = function.Call(t.Context(), []model.Value{model.StringVal("a"), replacementArray(model.StringVal("a"))}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "42P22")
	_, err = function.Call(t.Context(), []model.Value{model.StringVal("a"), replacementArray(model.NullVal(model.TypeString))}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "22004")
}

func TestWidthBucketArrayComparisonEffectsFollowPostgresProbes(t *testing.T) {
	thresholds := replacementArray(model.StringVal("a"), model.StringVal("b"), model.StringVal("c"), model.StringVal("d"), model.StringVal("e"), model.StringVal("f"), model.StringVal("g"), model.StringVal("h"))
	for _, tc := range []struct {
		name    string
		operand string
		pairs   [][2]string
		want    int64
	}{
		{"left branch", "0", [][2]string{{"0", "e"}, {"0", "c"}, {"0", "b"}, {"0", "a"}}, 0},
		{"equality goes right", "d", [][2]string{{"d", "e"}, {"d", "c"}, {"d", "d"}}, 4},
		{"right branch", "z", [][2]string{{"z", "e"}, {"z", "g"}, {"z", "h"}}, 8},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pairs [][2]string
			element := widthBucketArrayComparator(25, func(_ context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
				pairs = append(pairs, [2]string{arguments[0].StrVal, arguments[1].StrVal})
				return model.Int64Val(int64(strings.Compare(arguments[0].StrVal, arguments[1].StrVal))), nil
			})
			kernel := compileWidthBucketArray(element, nil)
			result, err := kernel(t.Context(), []model.Value{model.StringVal(tc.operand), thresholds}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireWidthBucketArrayResult(t, result, err, tc.want)
			if !slices.Equal(pairs, tc.pairs) {
				t.Fatalf("comparison effects = %v, want %v", pairs, tc.pairs)
			}
		})
	}
}

func TestWidthBucketArrayRejectsMalformedComparatorResults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		result model.Value
	}{
		{"NULL", model.NullVal(model.TypeInt64)},
		{"boolean", model.BoolVal(true)},
		{"below int4", model.Int64Val(math.MinInt32 - 1)},
		{"above int4", model.Int64Val(math.MaxInt32 + 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			element := widthBucketArrayComparator(23, func(context.Context, []model.Value, CallBudget) (model.Value, error) {
				calls++
				return tc.result, nil
			})
			kernel := compileWidthBucketArray(element, nil)
			_, err := kernel(t.Context(), []model.Value{model.Int64Val(0), replacementArray(model.Int64Val(1), model.Int64Val(2), model.Int64Val(3))}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, "XX000")
			if calls != 1 {
				t.Fatalf("comparison effects continued after malformed result: %d calls", calls)
			}
		})
	}
}

func TestWidthBucketArrayStopsAtFirstComparisonError(t *testing.T) {
	first := &model.Error{Code: "22003", Message: "selected comparison failed"}
	var pairs [][2]int64
	var scratch []model.Value
	element := widthBucketArrayComparator(23, func(_ context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		scratch = arguments
		left, right := arguments[0].IntVal, arguments[1].IntVal
		pairs = append(pairs, [2]int64{left, right})
		if right == 1 {
			return model.Value{}, first
		}
		return model.Int64Val(int64(cmp.Compare(left, right))), nil
	})
	kernel := compileWidthBucketArray(element, nil)
	thresholds := replacementArray(model.Int64Val(1), model.Int64Val(2), model.Int64Val(3))
	_, err := kernel(t.Context(), []model.Value{model.Int64Val(0), thresholds}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, first) {
		t.Fatalf("comparison error = %v, want %v", err, first)
	}
	if !slices.Equal(pairs, [][2]int64{{0, 2}, {0, 1}}) {
		t.Fatalf("comparison effects = %v, want first failing probe to stop", pairs)
	}
	if scratch[0].IntVal != 0 || scratch[1].IntVal != 0 {
		t.Fatal("failed comparison retains argument scratch")
	}
}

func TestWidthBucketArrayRejectsUnsearchedNullBeforeComparisonEffects(t *testing.T) {
	calls := 0
	element := widthBucketArrayComparator(23, func(context.Context, []model.Value, CallBudget) (model.Value, error) {
		calls++
		return model.Int64Val(-1), nil
	})
	kernel := compileWidthBucketArray(element, nil)
	thresholds := replacementArray(model.Int64Val(1), model.Int64Val(2), model.NullVal(model.TypeInt64))
	_, err := kernel(t.Context(), []model.Value{model.Int64Val(0), thresholds}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "22004")
	if calls != 0 {
		t.Fatalf("NULL thresholds invoked comparison %d times", calls)
	}
}

func TestWidthBucketArrayCancellationDuringNullScan(t *testing.T) {
	values := make([]model.Value, 4096)
	for i := range len(values) {
		values[i] = model.Int64Val(int64(i))
	}
	function := widthBucketArrayCallable(t, 23, model.TypeInt64, ResolveOptions{})
	base, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	ctx := &arrayPositionCancelContext{Context: base, cancel: cancel, remaining: 20}
	_, err := function.Call(ctx, []model.Value{model.Int64Val(-1), replacementArray(values...)}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted threshold scan = %v, want cancellation", err)
	}
}

func TestWidthBucketArrayCancellationStopsComparisonEffects(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	calls := 0
	element := widthBucketArrayComparator(23, func(context.Context, []model.Value, CallBudget) (model.Value, error) {
		calls++
		cancel()
		return model.Int64Val(-1), nil
	})
	kernel := compileWidthBucketArray(element, nil)
	thresholds := replacementArray(model.Int64Val(1), model.Int64Val(2), model.Int64Val(3))
	_, err := kernel(ctx, []model.Value{model.Int64Val(0), thresholds}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("interrupted search = %v after %d comparisons, want cancellation after first", err, calls)
	}
}

func TestWidthBucketArrayBorrowsLargeThresholdsAtZeroOutputBudget(t *testing.T) {
	payload := make([]byte, 8192)
	payload[0], payload[len(payload)-1] = 1, 2
	values := make([]model.Value, 4096)
	for i := range len(values) {
		values[i] = model.BytesVal(payload)
	}
	thresholds := replacementArray(values...)
	function := widthBucketArrayCallable(t, 17, model.TypeBytes, ResolveOptions{})
	result, err := function.Call(t.Context(), []model.Value{model.BytesVal(payload), thresholds}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireWidthBucketArrayResult(t, result, err, 4096)
	if thresholds.Dimensions[0].LowerBound != -3 || thresholds.Dimensions[0].Length != 4096 || payload[0] != 1 || payload[len(payload)-1] != 2 {
		t.Fatal("width_bucket mutated borrowed thresholds")
	}
}
