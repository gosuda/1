package builtin

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func arrayPositionTestCallable(t *testing.T, id model.FunctionID, elementID model.TypeID, storage model.DataType, options ResolveOptions) Callable {
	t.Helper()
	info, ok := LookupType(elementID)
	if !ok || info.Array == 0 {
		t.Fatalf("missing array type for element %d", elementID)
	}
	element := model.TypeLayout{Type: model.Type{ID: elementID, Storage: storage, Modifier: -1, Collation: info.Collation}}
	array := model.TypeLayout{Type: model.Type{ID: info.Array, Storage: model.TypeArray, Modifier: -1, Collation: info.Collation}, Children: []model.TypeLayout{element}}
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	arguments := []model.TypeLayout{array, element}
	if id == 3278 {
		arguments = append(arguments, integer)
	}
	result := integer
	if id == 3279 {
		result = model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	}
	function, err := ResolveTyped(t.Context(), id, arguments, result, options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func arrayPositionTestArray(lowerBound int32, values ...model.Value) model.Value {
	array := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		array.Dimensions = []model.ArrayDimension{{LowerBound: lowerBound, Length: int32(len(values))}}
	}
	return array
}

func requireArrayPositionResult(t *testing.T, id model.FunctionID, result model.Value, want ...int64) {
	t.Helper()
	if id != 3279 {
		wrongValue := len(want) != 0 && result.IntVal != want[0]
		if result.Type != model.TypeInt64 || result.IsNull != (len(want) == 0) || wrongValue {
			t.Fatalf("position=%v, want %v", result, want)
		}
		return
	}
	if result.Type != model.TypeArray || result.IsNull || len(result.ArrayVal) != len(want) {
		t.Fatalf("positions=%v, want %v", result, want)
	}
	if len(want) == 0 {
		if len(result.Dimensions) != 0 {
			t.Fatalf("empty positions dimensions=%v", result.Dimensions)
		}
		return
	}
	if !slices.Equal(result.Dimensions, []model.ArrayDimension{{LowerBound: 1, Length: int32(len(want))}}) {
		t.Fatalf("positions dimensions=%v, want [1:%d]", result.Dimensions, len(want))
	}
	for i, position := range want {
		if value := result.ArrayVal[i]; value.Type != model.TypeInt64 || value.IsNull || value.IntVal != position {
			t.Errorf("position %d=%v, want %d", i, value, position)
		}
	}
}

func requireArrayPositionSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != code {
		t.Fatalf("error=%v, want SQLSTATE %s", err, code)
	}
}

func TestArrayPositionUsesRealSubscripts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		id         model.FunctionID
		lowerBound int32
		start      int64
		want       []int64
	}{
		{"minimum lower bound", 3277, math.MinInt32, 0, []int64{math.MinInt32}},
		{"minimum positions", 3279, math.MinInt32, 0, []int64{math.MinInt32, math.MinInt32 + 1}},
		{"maximum positions", 3279, math.MaxInt32 - 1, 0, []int64{math.MaxInt32 - 1, math.MaxInt32}},
		{"last maximum subscript", 3278, math.MaxInt32 - 1, math.MaxInt32, []int64{math.MaxInt32}},
		{"start before lower bound", 3278, -4, math.MinInt32, []int64{-4}},
		{"start within negative bounds", 3278, -4, -3, []int64{-3}},
		{"start after upper bound", 3278, -4, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayPositionTestCallable(t, tc.id, 23, model.TypeInt64, ResolveOptions{})
			arguments := []model.Value{arrayPositionTestArray(tc.lowerBound, model.Int64Val(7), model.Int64Val(7)), model.Int64Val(7)}
			if tc.id == 3278 {
				arguments = append(arguments, model.Int64Val(tc.start))
			}
			result, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireArrayPositionResult(t, tc.id, result, tc.want...)
		})
	}
}

func TestArrayPositionAbsentEqualityPrecedence(t *testing.T) {
	null := model.NullVal(model.TypeString)
	value := model.StringVal("0")
	multidimensional := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}}, ArrayVal: []model.Value{null}}
	for _, id := range []model.FunctionID{3277, 3278, 3279} {
		function := arrayPositionTestCallable(t, id, 114, model.TypeString, ResolveOptions{})
		for _, tc := range []struct {
			name     string
			array    model.Value
			search   model.Value
			code     string
			rootNull bool
		}{
			{"NULL array", model.NullVal(model.TypeArray), null, "", true},
			{"empty array", arrayPositionTestArray(1), value, "", false},
			{"NULL search without NULL slots", arrayPositionTestArray(1, value), null, "", false},
			{"NULL search with NULL slot", arrayPositionTestArray(1, null), null, "42883", false},
			{"nonnull search", arrayPositionTestArray(1, value), value, "42883", false},
			{"multidimensional NULL search", multidimensional, null, "0A000", false},
		} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				arguments := []model.Value{tc.array, tc.search}
				code := tc.code
				if id == 3278 {
					arguments = append(arguments, model.NullVal(model.TypeInt64))
					if code == "42883" {
						code = "22004"
					}
				}
				result, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if code != "" {
					requireArrayPositionSQLState(t, err, code)
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if tc.rootNull {
					if !result.IsNull {
						t.Fatalf("NULL array result=%v", result)
					}
					return
				}
				requireArrayPositionResult(t, id, result)
			})
		}
	}
	function := arrayPositionTestCallable(t, 3278, 114, model.TypeString, ResolveOptions{})
	for _, search := range []model.Value{null, value} {
		_, err := function.Call(t.Context(), []model.Value{arrayPositionTestArray(1, null), search, model.Int64Val(math.MaxInt32)}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireArrayPositionSQLState(t, err, "42883")
	}
}

func TestArrayPositionUsesSelectedElementEquality(t *testing.T) {
	for _, tc := range []struct {
		name      string
		elementID model.TypeID
		storage   model.DataType
		values    []model.Value
		search    model.Value
		want      []int64
	}{
		{"float NaNs", 701, model.TypeFloat64, []model.Value{model.Float64Val(0), model.Float64Val(math.NaN()), model.Float64Val(math.Inf(1)), model.Float64Val(math.NaN())}, model.Float64Val(math.NaN()), []int64{0, 2}},
		{"bpchar trailing spaces", 1042, model.TypeString, []model.Value{model.StringVal("x"), model.StringVal("a  "), model.StringVal("b"), model.StringVal("a")}, model.StringVal("a"), []int64{0, 2}},
		{"text trailing spaces", 25, model.TypeString, []model.Value{model.StringVal("x"), model.StringVal("a  "), model.StringVal("b"), model.StringVal("a")}, model.StringVal("a"), []int64{2}},
	} {
		for _, id := range []model.FunctionID{3277, 3279} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				function := arrayPositionTestCallable(t, id, tc.elementID, tc.storage, ResolveOptions{Collation: 950})
				result, err := function.Call(t.Context(), []model.Value{arrayPositionTestArray(-1, tc.values...), tc.search}, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
				if err != nil {
					t.Fatal(err)
				}
				requireArrayPositionResult(t, id, result, tc.want...)
			})
		}
	}
}

func TestArrayPositionCallbacksFollowSearchOrder(t *testing.T) {
	comparisonFailure := errors.New("element comparison failed")
	for _, tc := range []struct {
		id    model.FunctionID
		start []model.Value
		calls [][2]int64
		err   error
	}{
		{3277, nil, [][2]int64{{7, 1}, {7, 0}, {7, 7}}, nil},
		{3278, []model.Value{model.Int64Val(-2)}, [][2]int64{{7, 0}, {7, 7}}, nil},
		{3279, nil, [][2]int64{{7, 1}, {7, 0}, {7, 7}, {7, 8}}, comparisonFailure},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			var calls [][2]int64
			element := Callable{
				signature:       Signature{ArgumentCount: 2, Arguments: [8]model.TypeID{23, 23}, Result: 16, Strict: true},
				argumentStorage: [8]model.DataType{model.TypeInt64, model.TypeInt64}, resultStorage: model.TypeBoolean,
				kernel: func(_ context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
					calls = append(calls, [2]int64{arguments[0].IntVal, arguments[1].IntVal})
					if arguments[1].IntVal == 8 {
						return model.Value{}, comparisonFailure
					}
					return model.BoolVal(arguments[0].IntVal == arguments[1].IntVal), nil
				},
			}
			signature, _ := LookupSignature(tc.id)
			kernel := compileArrayPosition(signature, element, nil)
			array := arrayPositionTestArray(-4, model.NullVal(model.TypeInt64), model.Int64Val(1), model.Int64Val(0), model.Int64Val(7), model.Int64Val(8))
			arguments := append([]model.Value{array, model.Int64Val(7)}, tc.start...)
			result, err := kernel(t.Context(), arguments, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if !errors.Is(err, tc.err) {
				t.Fatalf("comparison error=%v, want %v", err, tc.err)
			}
			if tc.err == nil {
				requireArrayPositionResult(t, tc.id, result, -1)
			}
			if !slices.Equal(calls, tc.calls) {
				t.Fatalf("comparisons=%v, want %v", calls, tc.calls)
			}
		})
	}
}

func TestArrayPositionNullSearchSkipsEquality(t *testing.T) {
	for _, id := range []model.FunctionID{3277, 3278, 3279} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			element := Callable{
				signature:       Signature{ArgumentCount: 2, Arguments: [8]model.TypeID{23, 23}, Result: 16, Strict: true},
				argumentStorage: [8]model.DataType{model.TypeInt64, model.TypeInt64}, resultStorage: model.TypeBoolean,
				kernel: func(context.Context, []model.Value, CallBudget) (model.Value, error) {
					t.Fatal("NULL search invoked equality")
					return model.Value{}, nil
				},
			}
			signature, _ := LookupSignature(id)
			array := arrayPositionTestArray(-4, model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(7))
			arguments := []model.Value{array, model.NullVal(model.TypeInt64)}
			if id == 3278 {
				arguments = append(arguments, model.Int64Val(-4))
			}
			result, err := compileArrayPosition(signature, element, nil)(t.Context(), arguments, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireArrayPositionResult(t, id, result, -3)
		})
	}
}

func TestArrayPositionRejectsNullEqualityResults(t *testing.T) {
	element := Callable{
		signature:       Signature{ArgumentCount: 2, Arguments: [8]model.TypeID{23, 23}, Result: 16, Strict: true},
		argumentStorage: [8]model.DataType{model.TypeInt64, model.TypeInt64},
		resultStorage:   model.TypeBoolean,
		kernel: func(context.Context, []model.Value, CallBudget) (model.Value, error) {
			return model.NullVal(model.TypeBoolean), nil
		},
	}
	for _, id := range []model.FunctionID{3277, 3278, 3279} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			signature, ok := LookupSignature(id)
			if !ok {
				t.Fatal("missing array position signature")
			}
			kernel := compileArrayPosition(signature, element, nil)
			arguments := []model.Value{arrayPositionTestArray(1, model.Int64Val(7)), model.Int64Val(7)}
			if id == 3278 {
				arguments = append(arguments, model.Int64Val(1))
			}
			_, err := kernel(t.Context(), arguments, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			requireArrayPositionSQLState(t, err, "XX000")
		})
	}
}

func TestArrayPositionsAdmitsOnlyOwnedResult(t *testing.T) {
	function := arrayPositionTestCallable(t, 3279, 17, model.TypeBytes, ResolveOptions{})
	values := make([]model.Value, 4096)
	payload := make([]byte, 8192)
	for i := range values {
		values[i] = model.BytesVal(payload)
	}
	for _, i := range []int{1, 100, 4095} {
		values[i] = model.BytesVal([]byte("hit"))
	}
	input := arrayPositionTestArray(-10, values...)
	search := model.BytesVal([]byte("hit"))
	want := arrayPositionTestArray(1, model.Int64Val(-9), model.Int64Val(90), model.Int64Val(4085))
	budget, err := model.ValueBytes(t.Context(), want, 4096)
	if err != nil {
		t.Fatal(err)
	}
	_, err = function.Call(t.Context(), []model.Value{input, search}, CallBudget{OutputBytes: int64(budget) - 1, MemoryBytes: 64 << 20})
	requireArrayPositionSQLState(t, err, "54000")
	result, err := function.Call(t.Context(), []model.Value{input, search}, CallBudget{OutputBytes: int64(budget), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireArrayPositionResult(t, 3279, result, -9, 90, 4085)
	if input.Dimensions[0].LowerBound != -10 || input.ArrayVal[1].Type != model.TypeBytes || string(input.ArrayVal[1].BytesVal) != "hit" {
		t.Fatal("position search mutated its input")
	}
	if cap(result.ArrayVal) != len(result.ArrayVal) || cap(result.Dimensions) != len(result.Dimensions) {
		t.Fatalf("result retains spare capacity: values=%d/%d, dimensions=%d/%d", len(result.ArrayVal), cap(result.ArrayVal), len(result.Dimensions), cap(result.Dimensions))
	}
	input.Dimensions[0].LowerBound = 100
	input.ArrayVal[1] = model.Int64Val(999)
	requireArrayPositionResult(t, 3279, result, -9, 90, 4085)
	empty, err := function.Call(t.Context(), []model.Value{arrayPositionTestArray(1, model.BytesVal(payload)), search}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireArrayPositionResult(t, 3279, empty)
}

type arrayPositionCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *arrayPositionCancelContext) Err() error {
	c.remaining--
	if c.remaining == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestArrayPositionCancelsScansWithoutEqualityCalls(t *testing.T) {
	for _, tc := range []struct {
		name   string
		value  model.Value
		search model.Value
	}{
		{"NULL presence scan", model.Int64Val(0), model.NullVal(model.TypeInt64)},
		{"NULL element scan", model.NullVal(model.TypeInt64), model.Int64Val(0)},
	} {
		for _, id := range []model.FunctionID{3277, 3279} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				function := arrayPositionTestCallable(t, id, 23, model.TypeInt64, ResolveOptions{})
				values := make([]model.Value, 4096)
				for i := range values {
					values[i] = tc.value
				}
				base, cancel := context.WithCancel(t.Context())
				t.Cleanup(cancel)
				ctx := &arrayPositionCancelContext{Context: base, cancel: cancel, remaining: 20}
				_, err := function.Call(ctx, []model.Value{arrayPositionTestArray(1, values...), tc.search}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("interrupted position scan=%v", err)
				}
			})
		}
	}
}
