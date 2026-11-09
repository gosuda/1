package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

var arrayComparisonIDs = [...]model.FunctionID{744, 390, 391, 392, 393, 396, 382}

func arrayComparisonLayout(t *testing.T, elementID model.TypeID, storage model.DataType) model.TypeLayout {
	t.Helper()
	info, ok := builtin.LookupType(elementID)
	if !ok || info.Array == 0 {
		t.Fatalf("missing array type for element %d", elementID)
	}
	return model.TypeLayout{
		Type:     model.Type{ID: info.Array, Storage: model.TypeArray, Modifier: -1, Collation: info.Collation},
		Children: []model.TypeLayout{{Type: model.Type{ID: elementID, Storage: storage, Modifier: -1, Collation: info.Collation}}},
	}
}

func arrayComparisonResultLayout(id model.FunctionID) model.TypeLayout {
	if id == 382 {
		return model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	}
	return model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}}
}

func arrayComparisonCallable(t *testing.T, id model.FunctionID, elementID model.TypeID, storage model.DataType, options builtin.ResolveOptions) builtin.Callable {
	t.Helper()
	layout := arrayComparisonLayout(t, elementID, storage)
	function, err := builtin.ResolveTyped(t.Context(), id, []model.TypeLayout{layout, layout}, arrayComparisonResultLayout(id), options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func comparisonArray(values ...model.Value) model.Value {
	array := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		array.Dimensions = []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}
	}
	return array
}

func requireArrayComparisonOrder(t *testing.T, elementID model.TypeID, storage model.DataType, options builtin.ResolveOptions, left, right model.Value, order int64) {
	t.Helper()
	for _, id := range arrayComparisonIDs {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := arrayComparisonCallable(t, id, elementID, storage, options)
			requireArrayComparisonResult(t, function, left, right, order)
		})
	}
}

func requireArrayComparisonResult(t *testing.T, function builtin.Callable, left, right model.Value, order int64) {
	t.Helper()
	var want model.Value
	switch function.Signature().ID {
	case 744:
		want = model.BoolVal(order == 0)
	case 390:
		want = model.BoolVal(order != 0)
	case 391:
		want = model.BoolVal(order < 0)
	case 392:
		want = model.BoolVal(order > 0)
	case 393:
		want = model.BoolVal(order <= 0)
	case 396:
		want = model.BoolVal(order >= 0)
	default:
		want = model.Int64Val(order)
	}
	got, err := function.Call(t.Context(), []model.Value{left, right}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || got.IsNull || got.Type != want.Type || got.IntVal != want.IntVal {
		t.Fatalf("array function %d = %v, %v; want %v", function.Signature().ID, got, err, want)
	}
}

func TestArrayComparisonOrderingPrecedence(t *testing.T) {
	values := []model.Value{model.Int64Val(1), model.Int64Val(2), model.Int64Val(3), model.Int64Val(4)}
	shaped := func(dimensions ...model.ArrayDimension) model.Value {
		return model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: dimensions}
	}
	for _, tc := range []struct {
		name        string
		left, right model.Value
		order       int64
	}{
		{"equal multidimensional", shaped(model.ArrayDimension{Length: 2, LowerBound: -2}, model.ArrayDimension{Length: 2, LowerBound: 7}), shaped(model.ArrayDimension{Length: 2, LowerBound: -2}, model.ArrayDimension{Length: 2, LowerBound: 7}), 0},
		{"elements precede item count", comparisonArray(model.Int64Val(2)), comparisonArray(model.Int64Val(1), model.Int64Val(9)), 1},
		{"item count after equal prefix", comparisonArray(model.Int64Val(1)), comparisonArray(model.Int64Val(1), model.Int64Val(0)), -1},
		{"rank precedes lengths", shaped(model.ArrayDimension{Length: 4, LowerBound: 1}), shaped(model.ArrayDimension{Length: 1, LowerBound: 1}, model.ArrayDimension{Length: 4, LowerBound: 1}), -1},
		{"lengths precede bounds", shaped(model.ArrayDimension{Length: 2, LowerBound: 10}, model.ArrayDimension{Length: 1, LowerBound: 1}, model.ArrayDimension{Length: 2, LowerBound: 1}), shaped(model.ArrayDimension{Length: 2, LowerBound: 0}, model.ArrayDimension{Length: 2, LowerBound: 1}, model.ArrayDimension{Length: 1, LowerBound: 1}), -1},
		{"inner lower bound", shaped(model.ArrayDimension{Length: 2, LowerBound: 1}, model.ArrayDimension{Length: 2, LowerBound: -3}), shaped(model.ArrayDimension{Length: 2, LowerBound: 1}, model.ArrayDimension{Length: 2, LowerBound: 1}), -1},
		{"empty arrays", comparisonArray(), comparisonArray(), 0},
		{"empty before nonempty", comparisonArray(), comparisonArray(model.Int64Val(1)), -1},
		{"null elements equal", comparisonArray(model.NullVal(model.TypeInt64)), comparisonArray(model.NullVal(model.TypeInt64)), 0},
		{"null elements last", comparisonArray(model.NullVal(model.TypeInt64)), comparisonArray(model.Int64Val(math.MaxInt32)), 1},
		{"nonnull before null", comparisonArray(model.Int64Val(math.MinInt32)), comparisonArray(model.NullVal(model.TypeInt64)), -1},
		{"comparison continues after nulls", comparisonArray(model.NullVal(model.TypeInt64), model.Int64Val(2)), comparisonArray(model.NullVal(model.TypeInt64), model.Int64Val(1)), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireArrayComparisonOrder(t, 23, model.TypeInt64, builtin.ResolveOptions{}, tc.left, tc.right, tc.order)
		})
	}
}

func TestArrayComparisonUsesSQLElementSemantics(t *testing.T) {
	for _, tc := range []struct {
		name        string
		elementID   model.TypeID
		storage     model.DataType
		left, right model.Value
		order       int64
	}{
		{"numeric scale", 1700, model.TypeNumeric, numericDatum("1.000"), numericDatum("1"), 0},
		{"numeric NaNs equal", 1700, model.TypeNumeric, numericDatum("NaN"), numericDatum("NaN"), 0},
		{"numeric NaN above infinity", 1700, model.TypeNumeric, numericDatum("NaN"), numericDatum("Infinity"), 1},
		{"numeric magnitude", 1700, model.TypeNumeric, numericDatum("2"), numericDatum("10.00"), -1},
		{"float4 NaNs equal", 700, model.TypeFloat64, model.Float64Val(math.NaN()), model.Float64Val(math.NaN()), 0},
		{"float8 NaN above infinity", 701, model.TypeFloat64, model.Float64Val(math.NaN()), model.Float64Val(math.Inf(1)), 1},
		{"float8 signed zero", 701, model.TypeFloat64, model.Float64Val(math.Copysign(0, -1)), model.Float64Val(0), 0},
		{"bpchar ignores trailing spaces", 1042, model.TypeString, model.StringVal("a  "), model.StringVal("a"), 0},
		{"varchar preserves trailing spaces", 1043, model.TypeString, model.StringVal("a  "), model.StringVal("a"), 1},
		{"text preserves trailing spaces", 25, model.TypeString, model.StringVal("a  "), model.StringVal("a"), 1},
		{"boolean order", 16, model.TypeBoolean, model.BoolVal(false), model.BoolVal(true), -1},
		{"unsigned OID order", 26, model.TypeInt64, model.Int64Val(math.MaxUint32), model.Int64Val(0), 1},
		{"int8 extremes", 20, model.TypeInt64, model.Int64Val(math.MinInt64), model.Int64Val(math.MaxInt64), -1},
		{"bytea order", 17, model.TypeBytes, model.BytesVal([]byte{0, 255}), model.BytesVal([]byte{1}), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireArrayComparisonOrder(t, tc.elementID, tc.storage, builtin.ResolveOptions{Collation: 950}, comparisonArray(tc.left), comparisonArray(tc.right), tc.order)
		})
	}
}

func TestArrayComparisonStrictRootNull(t *testing.T) {
	for _, id := range arrayComparisonIDs {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := arrayComparisonCallable(t, id, 23, model.TypeInt64, builtin.ResolveOptions{})
			invalid := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal("wrong storage")}}
			for _, args := range [][2]model.Value{{model.NullVal(model.TypeArray), invalid}, {invalid, model.NullVal(model.TypeArray)}} {
				got, err := function.Call(t.Context(), args[:], builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !got.IsNull || got.Type != arrayComparisonResultLayout(id).Type.Storage {
					t.Fatalf("strict NULL comparison = %v, %v", got, err)
				}
			}
		})
	}
}

func TestArrayComparisonRejectsInvalidShapes(t *testing.T) {
	for _, id := range []model.FunctionID{744, 382} {
		function := arrayComparisonCallable(t, id, 23, model.TypeInt64, builtin.ResolveOptions{})
		for _, invalid := range []model.Value{
			{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}},
			{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}},
			{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}},
			{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32}}},
		} {
			for _, args := range [][2]model.Value{{invalid, comparisonArray()}, {comparisonArray(), invalid}} {
				_, err := function.Call(t.Context(), args[:], builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "2202E")
			}
		}
	}
}

func TestArrayComparisonMasksOnlyUnreachedElementErrors(t *testing.T) {
	left := comparisonArray(model.Int64Val(1), model.StringVal("invalid int4"))
	right := comparisonArray(model.Int64Val(2), model.Int64Val(0))
	for _, id := range arrayComparisonIDs {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := arrayComparisonCallable(t, id, 23, model.TypeInt64, builtin.ResolveOptions{})
			requireArrayComparisonResult(t, function, left, right, -1)
			equalPrefix := comparisonArray(model.Int64Val(1), model.Int64Val(0))
			_, err := function.Call(t.Context(), []model.Value{left, equalPrefix}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "42804")
			_, err = function.Call(t.Context(), []model.Value{comparisonArray(model.Int64Val(math.MaxInt32 + 1)), comparisonArray(model.Int64Val(0))}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22003")
		})
	}
	invalid := comparisonArray(model.StringVal("invalid int4"))
	for _, id := range []model.FunctionID{744, 390} {
		function := arrayComparisonCallable(t, id, 23, model.TypeInt64, builtin.ResolveOptions{})
		requireArrayComparisonResult(t, function, invalid, right, -1)
	}
	function := arrayComparisonCallable(t, 382, 23, model.TypeInt64, builtin.ResolveOptions{})
	_, err := function.Call(t.Context(), []model.Value{invalid, right}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42804")
}

func TestArrayComparisonCollationErrorsFollowElementCalls(t *testing.T) {
	for _, elementID := range []model.TypeID{25, 1042, 1043} {
		for _, id := range []model.FunctionID{744, 382} {
			t.Run(fmt.Sprintf("%d/%d", elementID, id), func(t *testing.T) {
				function := arrayComparisonCallable(t, id, elementID, model.TypeString, builtin.ResolveOptions{})
				nonnull := comparisonArray(model.StringVal("a"))
				_, err := function.Call(t.Context(), []model.Value{nonnull, nonnull}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "42P22")
				requireArrayComparisonResult(t, function, comparisonArray(), comparisonArray(), 0)
				null := comparisonArray(model.NullVal(model.TypeString))
				requireArrayComparisonResult(t, function, null, null, 0)
				requireArrayComparisonResult(t, function, null, nonnull, 1)
				if id == 744 {
					requireArrayComparisonResult(t, function, nonnull, comparisonArray(model.StringVal("a"), model.StringVal("b")), -1)
				}
			})
		}
	}
}

func TestArrayComparisonAbsentOperatorsPreserveLookupTiming(t *testing.T) {
	for _, id := range arrayComparisonIDs {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := arrayComparisonCallable(t, id, 114, model.TypeString, builtin.ResolveOptions{})
			empty := comparisonArray()
			nullElement := comparisonArray(model.NullVal(model.TypeString))
			if id == 744 || id == 390 {
				requireArrayComparisonResult(t, function, empty, nullElement, -1)
			} else {
				_, err := function.Call(t.Context(), []model.Value{empty, nullElement}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "42883")
			}
			for _, array := range []model.Value{empty, nullElement} {
				_, err := function.Call(t.Context(), []model.Value{array, array}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "42883")
			}
			got, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeArray), empty}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !got.IsNull {
				t.Fatalf("NULL masks absent element operator: %v, %v", got, err)
			}
		})
	}
}

func TestArrayComparisonRejectsInvalidBoundTypes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*model.TypeLayout, *model.TypeLayout, *model.TypeLayout)
	}{
		{"different array identities", func(_, right, _ *model.TypeLayout) { *right = arrayComparisonLayout(t, 20, model.TypeInt64) }},
		{"catalog element mismatch", func(left, _, _ *model.TypeLayout) { left.Children[0].Type.ID = 20 }},
		{"wrong element storage", func(left, _, _ *model.TypeLayout) { left.Children[0].Type.Storage = model.TypeString }},
		{"nonarray argument", func(left, _, _ *model.TypeLayout) {
			*left = model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
		}},
		{"missing element layout", func(left, _, _ *model.TypeLayout) { left.Children = nil }},
		{"wrong result identity", func(_, _, result *model.TypeLayout) { result.Type.ID = 25 }},
		{"wrong result storage", func(_, _, result *model.TypeLayout) { result.Type.Storage = model.TypeString }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []model.FunctionID{744, 382} {
				left := arrayComparisonLayout(t, 23, model.TypeInt64)
				right := arrayComparisonLayout(t, 23, model.TypeInt64)
				result := arrayComparisonResultLayout(id)
				tc.mutate(&left, &right, &result)
				_, err := builtin.ResolveTyped(t.Context(), id, []model.TypeLayout{left, right}, result, builtin.ResolveOptions{}, 1<<20)
				requireSQLState(t, err, "42804")
			}
		})
	}
}

func TestArrayComparisonCancellationDuringNullScan(t *testing.T) {
	values := make([]model.Value, 4096)
	for i := range values {
		values[i] = model.NullVal(model.TypeInt64)
	}
	array := comparisonArray(values...)
	for _, id := range []model.FunctionID{744, 382} {
		function := arrayComparisonCallable(t, id, 23, model.TypeInt64, builtin.ResolveOptions{})
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := function.Call(ctx, []model.Value{array, array}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("pre-canceled array comparison = %v", err)
		}
		ctx, cancel = context.WithCancel(t.Context())
		interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 20}
		_, err = function.Call(interrupted, []model.Value{array, array}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interrupted NULL element scan = %v", err)
		}
	}
}

func TestArrayComparisonRepeatedCallsDoNotRetainElementState(t *testing.T) {
	for _, id := range arrayComparisonIDs {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := arrayComparisonCallable(t, id, 23, model.TypeInt64, builtin.ResolveOptions{})
			left := comparisonArray(model.Int64Val(9), model.Int64Val(1))
			right := comparisonArray(model.Int64Val(1), model.Int64Val(1))
			requireArrayComparisonResult(t, function, left, right, 1)
			left.ArrayVal[0] = model.Int64Val(1)
			requireArrayComparisonResult(t, function, left, right, 0)
			left.ArrayVal[1] = model.StringVal("invalid int4")
			_, err := function.Call(t.Context(), []model.Value{left, right}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "42804")
			left.ArrayVal[1] = model.Int64Val(0)
			requireArrayComparisonResult(t, function, left, right, -1)
			requireArrayComparisonResult(t, function, comparisonArray(), comparisonArray(), 0)
		})
	}
}
