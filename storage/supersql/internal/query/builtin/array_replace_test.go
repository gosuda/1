package builtin

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var replacementOperations = [...]struct {
	name string
	id   model.FunctionID
}{
	{"remove", 3167},
	{"replace", 3168},
}

func replacementLayout(t *testing.T, elementID model.TypeID, storage model.DataType) model.TypeLayout {
	t.Helper()
	info, ok := LookupType(elementID)
	if !ok || info.Array == 0 {
		t.Fatalf("missing array type for element %d", elementID)
	}
	return model.TypeLayout{
		Type:     model.Type{ID: info.Array, Storage: model.TypeArray, Modifier: -1, Collation: info.Collation},
		Children: []model.TypeLayout{{Type: model.Type{ID: elementID, Storage: storage, Modifier: -1, Collation: info.Collation}}},
	}
}

func replacementCallable(t *testing.T, id model.FunctionID, layout model.TypeLayout) Callable {
	t.Helper()
	arguments := []model.TypeLayout{layout, layout.Children[0]}
	if id == 3168 {
		arguments = append(arguments, layout.Children[0])
	}
	function, err := ResolveTyped(t.Context(), id, arguments, layout, ResolveOptions{Collation: 950}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func replacementArguments(id model.FunctionID, array, search, replace model.Value) []model.Value {
	if id == 3167 {
		return []model.Value{array, search}
	}
	return []model.Value{array, search, replace}
}

func replacementArray(values ...model.Value) model.Value {
	array := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		array.Dimensions = []model.ArrayDimension{{LowerBound: -3, Length: int32(len(values))}}
	}
	return array
}

func requireReplacementSQLState(t *testing.T, err error, state string) {
	t.Helper()
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.SQLState() != state {
		t.Fatalf("error = %v, want SQLSTATE %s", err, state)
	}
}

func requireReplacementIntegers(t *testing.T, got model.Value, want ...model.Value) {
	t.Helper()
	if got.Type != model.TypeArray || got.IsNull || len(got.ArrayVal) != len(want) {
		t.Fatalf("result = %v, want %v", got, want)
	}
	for i, value := range want {
		actual := got.ArrayVal[i]
		wrongValue := !value.IsNull && actual.IntVal != value.IntVal
		if actual.Type != value.Type || actual.IsNull != value.IsNull || wrongValue {
			t.Errorf("element %d = %v, want %v", i, actual, value)
		}
	}
}

func TestArrayReplacementNullMatching(t *testing.T) {
	null := model.NullVal(model.TypeInt64)
	for _, tc := range []struct {
		name            string
		id              model.FunctionID
		search, replace model.Value
		want            []model.Value
	}{
		{"remove null", 3167, null, null, []model.Value{model.Int64Val(1), model.Int64Val(2), model.Int64Val(1)}},
		{"remove nonnull", 3167, model.Int64Val(1), null, []model.Value{null, model.Int64Val(2), null}},
		{"replace null", 3168, null, model.Int64Val(9), []model.Value{model.Int64Val(9), model.Int64Val(1), model.Int64Val(2), model.Int64Val(9), model.Int64Val(1)}},
		{"replace with null", 3168, model.Int64Val(1), null, []model.Value{null, null, model.Int64Val(2), null, null}},
		{"replace null with null", 3168, null, null, []model.Value{null, model.Int64Val(1), model.Int64Val(2), null, model.Int64Val(1)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := replacementCallable(t, tc.id, replacementLayout(t, 23, model.TypeInt64))
			input := replacementArray(null, model.Int64Val(1), model.Int64Val(2), null, model.Int64Val(1))
			result, err := function.Call(t.Context(), replacementArguments(tc.id, input, tc.search, tc.replace), CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireReplacementIntegers(t, result, tc.want...)
			requireReplacementIntegers(t, input, null, model.Int64Val(1), model.Int64Val(2), null, model.Int64Val(1))
			if input.Dimensions[0] != (model.ArrayDimension{LowerBound: -3, Length: 5}) {
				t.Fatalf("replacement mutated input bounds: %v", input.Dimensions)
			}
			if !slices.Equal(result.Dimensions, []model.ArrayDimension{{LowerBound: -3, Length: int32(len(tc.want))}}) {
				t.Fatalf("result bounds = %v", result.Dimensions)
			}
		})
	}
}

func TestArrayReplacementEqualityLookupPhases(t *testing.T) {
	for _, operation := range replacementOperations {
		t.Run(operation.name, func(t *testing.T) {
			function := replacementCallable(t, operation.id, replacementLayout(t, 114, model.TypeString))
			for _, input := range []model.Value{model.NullVal(model.TypeArray), replacementArray()} {
				result, err := function.Call(t.Context(), replacementArguments(operation.id, input, model.NullVal(model.TypeString), model.StringVal("{}")), CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || result.Type != model.TypeArray || result.IsNull != input.IsNull || len(result.ArrayVal) != 0 || len(result.Dimensions) != 0 {
					t.Fatalf("empty/null result = %v, %v", result, err)
				}
			}
			for _, search := range []model.Value{model.NullVal(model.TypeString), model.StringVal("{}")} {
				input := replacementArray(model.NullVal(model.TypeString))
				_, err := function.Call(t.Context(), replacementArguments(operation.id, input, search, model.NullVal(model.TypeString)), CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
				requireReplacementSQLState(t, err, "42883")
			}
			multidimensional := replacementArray(model.NullVal(model.TypeString))
			multidimensional.Dimensions = []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}}
			_, err := function.Call(t.Context(), replacementArguments(operation.id, multidimensional, model.NullVal(model.TypeString), model.NullVal(model.TypeString)), CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			state := "42883"
			if operation.id == 3167 {
				state = "0A000"
			}
			requireReplacementSQLState(t, err, state)
		})
	}
}

func TestArrayReplacePreservesSixDimensionalMaximumSubscripts(t *testing.T) {
	function := replacementCallable(t, 3168, replacementLayout(t, 23, model.TypeInt64))
	input := replacementArray(model.Int64Val(1), model.Int64Val(2))
	input.Dimensions = []model.ArrayDimension{
		{LowerBound: math.MaxInt32 - 1, Length: 2},
		{LowerBound: math.MaxInt32, Length: 1},
		{LowerBound: math.MinInt32, Length: 1},
		{LowerBound: -2, Length: 1},
		{LowerBound: 7, Length: 1},
		{LowerBound: 0, Length: 1},
	}
	result, err := function.Call(t.Context(), []model.Value{input, model.Int64Val(1), model.Int64Val(9)}, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireReplacementIntegers(t, result, model.Int64Val(9), model.Int64Val(2))
	if !slices.Equal(result.Dimensions, input.Dimensions) {
		t.Fatalf("replacement dimensions = %v, want %v", result.Dimensions, input.Dimensions)
	}
	input.Dimensions[1].LowerBound = 0
	if result.Dimensions[1].LowerBound != math.MaxInt32 {
		t.Fatal("replacement aliases input dimensions")
	}
}

func TestArrayRemoveRetainsMaximumLowerBound(t *testing.T) {
	function := replacementCallable(t, 3167, replacementLayout(t, 23, model.TypeInt64))
	input := replacementArray(model.Int64Val(1), model.Int64Val(2))
	input.Dimensions[0].LowerBound = math.MaxInt32 - 1
	result, err := function.Call(t.Context(), []model.Value{input, model.Int64Val(1)}, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireReplacementIntegers(t, result, model.Int64Val(2))
	if !slices.Equal(result.Dimensions, []model.ArrayDimension{{LowerBound: math.MaxInt32 - 1, Length: 1}}) {
		t.Fatalf("removed result bounds = %v", result.Dimensions)
	}
}

func TestArrayReplacementOwnsBytePayloadsAtExactBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     model.FunctionID
		search string
		want   []string
	}{
		{"remove unchanged", 3167, "absent", []string{"keep", "drop", "drop"}},
		{"remove matches", 3167, "drop", []string{"keep"}},
		{"replace unchanged", 3168, "absent", []string{"keep", "drop", "drop"}},
		{"replace matches", 3168, "drop", []string{"keep", "new", "new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := replacementCallable(t, tc.id, replacementLayout(t, 17, model.TypeBytes))
			input := replacementArray(model.BytesVal([]byte("keep")), model.BytesVal([]byte("drop")), model.BytesVal([]byte("drop")))
			input.Dimensions[0].LowerBound = math.MaxInt32 - 2
			replace := model.BytesVal([]byte("new"))
			search := model.BytesVal([]byte(tc.search))
			expected := replacementArray()
			expected.Dimensions = []model.ArrayDimension{{LowerBound: math.MaxInt32 - 2, Length: int32(len(tc.want))}}
			for _, text := range tc.want {
				expected.ArrayVal = append(expected.ArrayVal, model.BytesVal([]byte(text)))
			}
			size, err := model.ValueBytes(t.Context(), expected, 4096)
			if err != nil {
				t.Fatal(err)
			}
			arguments := replacementArguments(tc.id, input, search, replace)
			_, err = function.Call(t.Context(), arguments, CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, "54000")
			result, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			input.ArrayVal[0].BytesVal[0] = '!'
			input.ArrayVal[1].BytesVal[0] = '?'
			input.ArrayVal[2] = model.NullVal(model.TypeBytes)
			input.Dimensions[0].LowerBound = 1
			replace.BytesVal[0] = '#'
			if result.IsNull || !slices.Equal(result.Dimensions, expected.Dimensions) || len(result.ArrayVal) != len(tc.want) {
				t.Fatalf("owned result shape=%v", result)
			}
			for i, text := range tc.want {
				if got := result.ArrayVal[i]; got.IsNull || got.Type != model.TypeBytes || string(got.BytesVal) != text {
					t.Errorf("owned element %d=%v, want %q", i, got, text)
				}
			}
			if cap(result.ArrayVal) != len(result.ArrayVal) {
				t.Fatalf("retained header capacity=%d, want %d", cap(result.ArrayVal), len(result.ArrayVal))
			}
		})
	}
}

func TestArrayReplacementOccurrencesOwnSeparatePayloads(t *testing.T) {
	function := replacementCallable(t, 3168, replacementLayout(t, 17, model.TypeBytes))
	input := replacementArray(model.BytesVal([]byte("old")), model.BytesVal([]byte("old")))
	replacement := model.BytesVal([]byte("new"))
	result, err := function.Call(t.Context(), replacementArguments(3168, input, model.BytesVal([]byte("old")), replacement), CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ArrayVal) != 2 || len(result.ArrayVal[0].BytesVal) != 3 {
		t.Fatalf("replacement result=%v", result)
	}
	result.ArrayVal[0].BytesVal[0] = 'X'
	if string(result.ArrayVal[1].BytesVal) != "new" || string(replacement.BytesVal) != "new" {
		t.Fatal("replacement occurrences share writable payload")
	}
}

func TestArrayReplacementOwnsNestedPayloads(t *testing.T) {
	for _, operation := range replacementOperations {
		t.Run(operation.name, func(t *testing.T) {
			layout := replacementLayout(t, 22, model.TypeArray)
			layout.Children[0].Children = []model.TypeLayout{{Type: model.Type{ID: 21, Storage: model.TypeInt64, Modifier: -1}}}
			function := replacementCallable(t, operation.id, layout)
			retained := replacementArray(model.Int64Val(1), model.Int64Val(2))
			search := replacementArray(model.Int64Val(3))
			replace := replacementArray(model.Int64Val(4), model.Int64Val(5))
			input := replacementArray(retained, search)
			result, err := function.Call(t.Context(), replacementArguments(operation.id, input, search, replace), CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			retained.ArrayVal[0] = model.Int64Val(8)
			retained.Dimensions[0].LowerBound = 20
			replace.ArrayVal[0] = model.Int64Val(9)
			replace.Dimensions[0].LowerBound = 30
			input.ArrayVal[0] = model.NullVal(model.TypeArray)
			want := [][]int64{{1, 2}}
			if operation.id == 3168 {
				want = append(want, []int64{4, 5})
			}
			if result.IsNull || len(result.ArrayVal) != len(want) {
				t.Fatalf("nested result = %v", result)
			}
			for i, values := range want {
				nested := result.ArrayVal[i]
				if nested.Type != model.TypeArray || nested.IsNull || len(nested.ArrayVal) != len(values) || !slices.Equal(nested.Dimensions, []model.ArrayDimension{{LowerBound: -3, Length: int32(len(values))}}) {
					t.Fatalf("nested element %d = %v", i, nested)
				}
				for j, value := range values {
					if nested.ArrayVal[j].IntVal != value {
						t.Errorf("nested element %d/%d = %v, want %d", i, j, nested.ArrayVal[j], value)
					}
				}
			}
		})
	}
}

func TestArrayRemoveAdmitsOnlyRetainedPayload(t *testing.T) {
	function := replacementCallable(t, 3167, replacementLayout(t, 17, model.TypeBytes))
	discarded := model.BytesVal(make([]byte, 1<<20))
	input := replacementArray(discarded, model.BytesVal([]byte("kept")), discarded)
	want := replacementArray(model.BytesVal([]byte("kept")))
	size, err := model.ValueBytes(t.Context(), want, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result, err := function.Call(t.Context(), []model.Value{input, discarded}, CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
	if err != nil || len(result.ArrayVal) != 1 || string(result.ArrayVal[0].BytesVal) != "kept" {
		t.Fatalf("removing oversized input payload = %v, %v", result, err)
	}
	input = replacementArray(discarded, discarded)
	empty, err := function.Call(t.Context(), []model.Value{input, discarded}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || empty.Type != model.TypeArray || empty.IsNull || len(empty.ArrayVal) != 0 || len(empty.Dimensions) != 0 {
		t.Fatalf("all removed at zero budget = %v, %v", empty, err)
	}
}

func TestArrayRemoveDiscardsLargeArrayAtZeroBudget(t *testing.T) {
	function := replacementCallable(t, 3167, replacementLayout(t, 23, model.TypeInt64))
	values := make([]model.Value, 16384)
	for i := range values {
		values[i] = model.NullVal(model.TypeInt64)
	}
	input := replacementArray(values...)
	result, err := function.Call(t.Context(), []model.Value{input, model.NullVal(model.TypeInt64)}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || result.Type != model.TypeArray || result.IsNull || len(result.ArrayVal) != 0 || len(result.Dimensions) != 0 {
		t.Fatalf("discarding large NULL array = %v, %v", result, err)
	}
	if !input.ArrayVal[0].IsNull || !input.ArrayVal[len(input.ArrayVal)-1].IsNull || input.Dimensions[0].Length != 16384 {
		t.Fatal("removal mutated its borrowed input")
	}
}

func TestArrayReplaceAdmitsOnlySelectedPayload(t *testing.T) {
	function := replacementCallable(t, 3168, replacementLayout(t, 17, model.TypeBytes))
	huge := model.BytesVal(make([]byte, 1<<20))
	small := model.BytesVal([]byte("x"))
	want := replacementArray(small, small)
	size, err := model.ValueBytes(t.Context(), want, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                   string
		input, search, replace model.Value
	}{
		{"large replaced input", replacementArray(huge, huge), huge, small},
		{"large unused replacement", replacementArray(small, small), huge, huge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := function.Call(t.Context(), []model.Value{tc.input, tc.search, tc.replace}, CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
			if err != nil || result.IsNull || len(result.ArrayVal) != 2 {
				t.Fatalf("selected payload result = %v, %v", result, err)
			}
			for i, value := range result.ArrayVal {
				if value.IsNull || value.Type != model.TypeBytes || string(value.BytesVal) != "x" {
					t.Errorf("selected payload %d = %v, want x", i, value)
				}
			}
		})
	}
}

func replacementEquality(kernel scalarKernel) Callable {
	return Callable{
		signature:       Signature{Arguments: [8]model.TypeID{23, 23}, ArgumentCount: 2, Result: 16},
		argumentStorage: [8]model.DataType{model.TypeInt64, model.TypeInt64},
		resultStorage:   model.TypeBoolean,
		kernel:          kernel,
	}
}

func TestArrayReplacementComparesEachNonNullPairInOrder(t *testing.T) {
	for _, operation := range replacementOperations {
		t.Run(operation.name, func(t *testing.T) {
			var pairs [][2]int64
			element := replacementEquality(func(_ context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
				pairs = append(pairs, [2]int64{arguments[0].IntVal, arguments[1].IntVal})
				if arguments[0].IntVal == 2 {
					return model.NullVal(model.TypeBoolean), nil
				}
				return model.BoolVal(arguments[0].IntVal == 1), nil
			})
			kernel := compileArrayReplace(Signature{ID: operation.id}, element, nil)
			null := model.NullVal(model.TypeInt64)
			input := replacementArray(model.Int64Val(2), null, model.Int64Val(3), model.Int64Val(1))
			result, err := kernel(t.Context(), replacementArguments(operation.id, input, model.Int64Val(1), model.Int64Val(9)), CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			want := []model.Value{model.Int64Val(2), null, model.Int64Val(3)}
			if operation.id == 3168 {
				want = append(want, model.Int64Val(9))
			}
			requireReplacementIntegers(t, result, want...)
			if !slices.Equal(pairs, [][2]int64{{2, 1}, {3, 1}, {1, 1}}) {
				t.Fatalf("comparison effects = %v", pairs)
			}
		})
	}
}

func TestArrayReplacementNullSearchSkipsEqualityEffects(t *testing.T) {
	for _, operation := range replacementOperations {
		t.Run(operation.name, func(t *testing.T) {
			element := replacementEquality(func(_ context.Context, _ []model.Value, callBudget CallBudget) (model.Value, error) {
				return model.Value{}, &model.Error{Code: "22012", Message: "equality must not be invoked for NULL search"}
			})
			kernel := compileArrayReplace(Signature{ID: operation.id}, element, nil)
			null := model.NullVal(model.TypeInt64)
			input := replacementArray(model.Int64Val(1), null, model.Int64Val(2))
			result, err := kernel(t.Context(), replacementArguments(operation.id, input, null, model.Int64Val(9)), CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			want := []model.Value{model.Int64Val(1), model.Int64Val(2)}
			if operation.id == 3168 {
				want = []model.Value{model.Int64Val(1), model.Int64Val(9), model.Int64Val(2)}
			}
			requireReplacementIntegers(t, result, want...)
		})
	}
}

func TestArrayReplacementStopsAtFirstComparisonError(t *testing.T) {
	for _, operation := range replacementOperations {
		t.Run(operation.name, func(t *testing.T) {
			first := &model.Error{Code: "22003", Message: "first comparison failed"}
			later := &model.Error{Code: "22012", Message: "later comparison failed"}
			var calls []int64
			element := replacementEquality(func(_ context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
				value := arguments[0].IntVal
				calls = append(calls, value)
				switch value {
				case 2:
					return model.Value{}, first
				case 3:
					return model.Value{}, later
				default:
					return model.BoolVal(false), nil
				}
			})
			kernel := compileArrayReplace(Signature{ID: operation.id}, element, nil)
			input := replacementArray(model.Int64Val(1), model.Int64Val(2), model.Int64Val(3))
			arguments := replacementArguments(operation.id, input, model.Int64Val(9), model.Int64Val(8))
			result, err := kernel(t.Context(), arguments, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if !errors.Is(err, first) || !slices.Equal(calls, []int64{1, 2}) || len(result.ArrayVal) != 0 {
				t.Fatalf("comparison failure = %v, %v; effects %v", result, err, calls)
			}
			calls = nil
			_, err = kernel(t.Context(), arguments, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, "54000")
			if !slices.Equal(calls, []int64{1}) {
				t.Fatalf("comparisons continued after output budget failure: %v", calls)
			}
		})
	}
}

type replacementCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *replacementCancelContext) Err() error {
	c.remaining--
	if c.remaining == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestArrayReplacementCancellationDuringNullScan(t *testing.T) {
	for _, operation := range replacementOperations {
		t.Run(operation.name, func(t *testing.T) {
			function := replacementCallable(t, operation.id, replacementLayout(t, 23, model.TypeInt64))
			values := make([]model.Value, 8192)
			for i := range values {
				values[i] = model.NullVal(model.TypeInt64)
			}
			input := replacementArray(values...)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := &replacementCancelContext{Context: ctx, cancel: cancel, remaining: 5}
			budget := int64(0)
			if operation.id == 3168 {
				budget = 2 << 20
			}
			result, err := function.Call(interrupted, replacementArguments(operation.id, input, model.NullVal(model.TypeInt64), model.Int64Val(9)), CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) || len(result.ArrayVal) != 0 || len(result.Dimensions) != 0 {
				t.Fatalf("canceled NULL scan = %v, %v", result, err)
			}
			if !input.ArrayVal[0].IsNull || !input.ArrayVal[len(input.ArrayVal)-1].IsNull || input.Dimensions[0].Length != 8192 {
				t.Fatal("canceled replacement mutated its input")
			}
		})
	}
}
