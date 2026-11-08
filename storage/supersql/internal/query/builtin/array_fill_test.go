package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func arrayFillCallable(t *testing.T, id model.FunctionID, layout model.TypeLayout) builtin.Callable {
	t.Helper()
	dimensions := arrayComparisonLayout(t, 23, model.TypeInt64)
	arguments := [3]model.TypeLayout{layout.Children[0], dimensions, dimensions}
	signature, ok := builtin.LookupSignature(id)
	if !ok {
		t.Fatalf("missing array fill signature %d", id)
	}
	function, err := builtin.ResolveTyped(t.Context(), id, arguments[:signature.ArgumentCount], layout, builtin.ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func arrayFillVector(values ...int64) model.Value {
	elements := make([]model.Value, len(values))
	for i, value := range values {
		elements[i] = model.Int64Val(value)
	}
	return comparisonArray(elements...)
}

func requireArrayFillShape(t *testing.T, result model.Value, dimensions []model.ArrayDimension, count int) {
	t.Helper()
	if result.Type != model.TypeArray || result.IsNull || !slices.Equal(result.Dimensions, dimensions) || len(result.ArrayVal) != count {
		t.Fatalf("array fill shape=%v, elements=%d, type=%v, NULL=%v; want dimensions=%v, elements=%d", result.Dimensions, len(result.ArrayVal), result.Type, result.IsNull, dimensions, count)
	}
}

func TestArrayFillDefaultMultidimensionalBounds(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 23, model.TypeInt64))
	result, err := function.Call(t.Context(), []model.Value{model.Int64Val(7), arrayFillVector(2, 3)}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireArrayFillShape(t, result, []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 3, LowerBound: 1}}, 6)
	for i, value := range result.ArrayVal {
		if value.Type != model.TypeInt64 || value.IsNull || value.IntVal != 7 {
			t.Errorf("filled element %d=%v, want integer 7", i, value)
		}
	}
}

func TestArrayFillIgnoresDimensionVectorSubscripts(t *testing.T) {
	function := arrayFillCallable(t, 1286, arrayComparisonLayout(t, 23, model.TypeInt64))
	dimensions, bounds := arrayFillVector(2, 3), arrayFillVector(-2, 7)
	dimensions.Dimensions[0].LowerBound = -12
	bounds.Dimensions[0].LowerBound = math.MaxInt32 - 1
	result, err := function.Call(t.Context(), []model.Value{model.Int64Val(11), dimensions, bounds}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	dimensions.ArrayVal[0] = model.Int64Val(9)
	bounds.ArrayVal[0] = model.Int64Val(9)
	dimensions.Dimensions[0].LowerBound = 9
	bounds.Dimensions[0].LowerBound = 9
	requireArrayFillShape(t, result, []model.ArrayDimension{{Length: 2, LowerBound: -2}, {Length: 3, LowerBound: 7}}, 6)
	for i, value := range result.ArrayVal {
		if value.Type != model.TypeInt64 || value.IsNull || value.IntVal != 11 {
			t.Errorf("filled element %d=%v, want integer 11", i, value)
		}
	}
}

func TestArrayFillTypedNullIsAnElement(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 17, model.TypeBytes))
	want := comparisonArray(model.NullVal(model.TypeBytes), model.NullVal(model.TypeBytes))
	size, err := model.ValueBytes(t.Context(), want, 4096)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []model.Value{model.NullVal(model.TypeBytes), arrayFillVector(2)}
	_, err = function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	result, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireArrayFillShape(t, result, want.Dimensions, 2)
	for i, value := range result.ArrayVal {
		if value.Type != model.TypeBytes || !value.IsNull {
			t.Errorf("filled element %d=%v, want bytea NULL", i, value)
		}
	}
}

func TestArrayFillDimensionValidationPrecedence(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 23, model.TypeInt64))
	multidimensionalNull := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.NullVal(model.TypeInt64)}}
	tooManyWithNull := arrayFillVector(1, 1, 1, 1, 1, 1, 1)
	tooManyWithNull.ArrayVal[6] = model.NullVal(model.TypeInt64)
	for _, tc := range []struct {
		name       string
		dimensions model.Value
		code       string
	}{
		{"NULL dimensions despite NULL fill", model.NullVal(model.TypeArray), "22004"},
		{"rank before NULL entries", multidimensionalNull, "2202E"},
		{"NULL entries before dimension count", tooManyWithNull, "22004"},
		{"too many dimensions", arrayFillVector(1, 1, 1, 1, 1, 1, 1), "54000"},
		{"negative dimension", arrayFillVector(-1), "54000"},
		{"negative after zero", arrayFillVector(0, -1), "54000"},
		{"overflow before later zero", arrayFillVector(math.MaxInt32, 2, 0), "54000"},
		{"PG item count limit", arrayFillVector(134217728), "54000"},
		{"int4 physical range", arrayFillVector(int64(math.MaxInt32) + 1), "22003"},
		{"noninteger dimension", comparisonArray(model.StringVal("2")), "42804"},
		{"missing vector shape", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(2)}}, "2202E"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeInt64), tc.dimensions}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestArrayFillExplicitBoundsValidationPrecedence(t *testing.T) {
	function := arrayFillCallable(t, 1286, arrayComparisonLayout(t, 23, model.TypeInt64))
	multidimensional := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{model.NullVal(model.TypeInt64)}}
	for _, tc := range []struct {
		name               string
		dimensions, bounds model.Value
		code               string
	}{
		{"NULL dimensions before bound rank", model.NullVal(model.TypeArray), multidimensional, "22004"},
		{"NULL bounds before dimension rank", multidimensional, model.NullVal(model.TypeArray), "22004"},
		{"dimension count before bound rank", arrayFillVector(1, 1, 1, 1, 1, 1, 1), multidimensional, "54000"},
		{"dimension count before bound NULL entry", arrayFillVector(1, 1, 1, 1, 1, 1, 1), comparisonArray(model.NullVal(model.TypeInt64)), "54000"},
		{"dimension NULL entry before bound rank", comparisonArray(model.NullVal(model.TypeInt64)), multidimensional, "22004"},
		{"bound rank before bound NULL entry", arrayFillVector(1), multidimensional, "2202E"},
		{"bound NULL entry before length mismatch", arrayFillVector(1, 1), comparisonArray(model.NullVal(model.TypeInt64)), "22004"},
		{"length mismatch before negative dimension", arrayFillVector(-1, 1), arrayFillVector(1), "2202E"},
		{"bound NULL entry before negative dimension", arrayFillVector(-1), comparisonArray(model.NullVal(model.TypeInt64)), "22004"},
		{"empty dimensions with nonempty bounds", arrayFillVector(), arrayFillVector(1), "2202E"},
		{"nonempty dimensions with empty bounds", arrayFillVector(1), arrayFillVector(), "2202E"},
		{"noninteger bound", arrayFillVector(1), comparisonArray(model.StringVal("1")), "42804"},
		{"int4 bound range", arrayFillVector(1), arrayFillVector(int64(math.MinInt32) - 1), "22003"},
		{"last subscript at maximum", arrayFillVector(1), arrayFillVector(math.MaxInt32), "54000"},
		{"bounds checked despite zero count", arrayFillVector(0, 1), arrayFillVector(1, math.MaxInt32), "54000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeInt64), tc.dimensions, tc.bounds}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestArrayFillCanonicalEmptyWithZeroBudget(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 17, model.TypeBytes))
	for _, tc := range []struct {
		name       string
		dimensions model.Value
	}{
		{"zero dimensions", arrayFillVector()},
		{"zero length", arrayFillVector(0)},
		{"zero inner length", arrayFillVector(3, 0)},
		{"zero before large product", arrayFillVector(0, 1<<30, 1<<30)},
		{"item count limit checked after zero", arrayFillVector(134217728, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := function.Call(t.Context(), []model.Value{model.BytesVal([]byte("unused")), tc.dimensions}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireArrayFillShape(t, result, nil, 0)
		})
	}
}

func TestArrayFillEmptyExplicitBounds(t *testing.T) {
	function := arrayFillCallable(t, 1286, arrayComparisonLayout(t, 23, model.TypeInt64))
	for _, tc := range []struct {
		name               string
		dimensions, bounds model.Value
	}{
		{"empty vectors", arrayFillVector(), arrayFillVector()},
		{"maximum bound at zero length", arrayFillVector(0), arrayFillVector(math.MaxInt32)},
		{"maximum dimension followed by zero", arrayFillVector(math.MaxInt32, 0), arrayFillVector(0, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := function.Call(t.Context(), []model.Value{model.Int64Val(1), tc.dimensions, tc.bounds}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireArrayFillShape(t, result, nil, 0)
		})
	}
}

func TestArrayFillAcceptsSixDimensions(t *testing.T) {
	function := arrayFillCallable(t, 1286, arrayComparisonLayout(t, 23, model.TypeInt64))
	result, err := function.Call(t.Context(), []model.Value{model.Int64Val(4), arrayFillVector(1, 1, 1, 1, 1, 1), arrayFillVector(math.MinInt32, math.MaxInt32-1, -1, 0, 1, 2)}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireArrayFillShape(t, result, []model.ArrayDimension{{Length: 1, LowerBound: math.MinInt32}, {Length: 1, LowerBound: math.MaxInt32 - 1}, {Length: 1, LowerBound: -1}, {Length: 1, LowerBound: 0}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 2}}, 1)
	if value := result.ArrayVal[0]; value.IsNull || value.Type != model.TypeInt64 || value.IntVal != 4 {
		t.Fatalf("six-dimensional fill element=%v, want integer 4", value)
	}
}

func TestArrayFillOwnsEveryByteOccurrenceAtExactQuota(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 17, model.TypeBytes))
	fill := model.BytesVal([]byte("abc"))
	want := comparisonArray(fill, fill, fill)
	size, err := model.ValueBytes(t.Context(), want, 4096)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []model.Value{fill, arrayFillVector(3)}
	_, err = function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	result, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireArrayFillShape(t, result, want.Dimensions, 3)
	fill.BytesVal[0] = '!'
	for i, value := range result.ArrayVal {
		if value.Type != model.TypeBytes || value.IsNull || !bytes.Equal(value.BytesVal, []byte("abc")) {
			t.Errorf("owned bytea occurrence %d=%v, want abc", i, value)
		}
	}
	result.ArrayVal[0].BytesVal[0] = '?'
	if !bytes.Equal(result.ArrayVal[1].BytesVal, []byte("abc")) || !bytes.Equal(result.ArrayVal[2].BytesVal, []byte("abc")) {
		t.Fatal("repeated bytea occurrences share mutable storage")
	}
}

func TestArrayFillOwnsEveryNestedOccurrenceAtExactQuota(t *testing.T) {
	layout := arrayComparisonLayout(t, 22, model.TypeArray)
	layout.Children[0].Children = []model.TypeLayout{{Type: model.Type{ID: 21, Storage: model.TypeInt64, Modifier: -1}}}
	function := arrayFillCallable(t, 1286, layout)
	fill := comparisonArray(model.Int64Val(3), model.Int64Val(4))
	fill.Dimensions[0].LowerBound = 0
	want := comparisonArray(fill, fill)
	want.Dimensions[0].LowerBound = -7
	size, err := model.ValueBytes(t.Context(), want, 4096)
	if err != nil {
		t.Fatal(err)
	}
	arguments := []model.Value{fill, arrayFillVector(2), arrayFillVector(-7)}
	_, err = function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(size) - 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	result, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(size), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	fill.ArrayVal[0] = model.Int64Val(9)
	fill.Dimensions[0].LowerBound = 99
	requireArrayFillShape(t, result, want.Dimensions, 2)
	for i, value := range result.ArrayVal {
		requireArrayFillShape(t, value, []model.ArrayDimension{{Length: 2, LowerBound: 0}}, 2)
		if value.ArrayVal[0].IntVal != 3 || value.ArrayVal[1].IntVal != 4 {
			t.Errorf("owned nested occurrence %d=%v, want {3,4}", i, value)
		}
	}
	result.ArrayVal[0].ArrayVal[0] = model.Int64Val(8)
	result.ArrayVal[0].Dimensions[0].LowerBound = 88
	if result.ArrayVal[1].ArrayVal[0].IntVal != 3 || result.ArrayVal[1].Dimensions[0].LowerBound != 0 {
		t.Fatal("repeated nested occurrences share mutable storage")
	}
}

func TestArrayFillEnforcesGlobalAllocationLimitBeforeCopying(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 17, model.TypeBytes))
	_, err := function.Call(t.Context(), []model.Value{model.BytesVal(bytes.Repeat([]byte{'x'}, 1024)), arrayFillVector(1 << 20)}, builtin.CallBudget{OutputBytes: math.MaxInt64, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	if !errors.Is(err, model.ErrValueBudget) {
		t.Fatalf("global array allocation limit error=%v, want value budget cause", err)
	}
}

func TestArrayFillAccountsForOutputRootNesting(t *testing.T) {
	layout := arrayComparisonLayout(t, 22, model.TypeArray)
	layout.Children[0].Children = []model.TypeLayout{{Type: model.Type{ID: 21, Storage: model.TypeInt64, Modifier: -1}}}
	function := arrayFillCallable(t, 1193, layout)
	fill := model.Int64Val(1)
	for range 64 {
		fill = comparisonArray(fill)
	}
	if _, err := model.ValueBytes(t.Context(), fill, 1<<20); err != nil {
		t.Fatalf("fill datum alone must fit nesting bound: %v", err)
	}
	_, err := function.Call(t.Context(), []model.Value{fill, arrayFillVector(1)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	if !errors.Is(err, model.ErrValueDepth) {
		t.Fatalf("filled root nesting error=%v, want value depth cause", err)
	}
}

func TestArrayFillCancelsRepeatedScalarCopies(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 23, model.TypeInt64))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 20}
	_, err := function.Call(interrupted, []model.Value{model.Int64Val(7), arrayFillVector(8192)}, builtin.CallBudget{OutputBytes: 2 << 20, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted repeated fill=%v, want cancellation", err)
	}
}

func TestArrayFillCancelsWithinLargeByteOccurrence(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 17, model.TypeBytes))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 20}
	_, err := function.Call(interrupted, []model.Value{model.BytesVal(make([]byte, 1<<20)), arrayFillVector(1)}, builtin.CallBudget{OutputBytes: 2 << 20, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted large occurrence=%v, want cancellation", err)
	}
}

func TestArrayFillCancelsDimensionNullScan(t *testing.T) {
	function := arrayFillCallable(t, 1193, arrayComparisonLayout(t, 23, model.TypeInt64))
	values := make([]int64, 8192)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 3}
	_, err := function.Call(interrupted, []model.Value{model.Int64Val(7), arrayFillVector(values...)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted dimension scan=%v, want cancellation", err)
	}
}
