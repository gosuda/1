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

func membershipCallable(t *testing.T, id model.FunctionID, layout model.TypeLayout, options ResolveOptions) Callable {
	t.Helper()
	result := model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}}
	function, err := ResolveTyped(t.Context(), id, []model.TypeLayout{layout, layout}, result, options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireMembershipBoolean(t *testing.T, result model.Value, want bool) {
	t.Helper()
	if result.IsNull || result.Type != model.TypeBoolean || result.IntVal != model.BoolVal(want).IntVal {
		t.Fatalf("membership result = %v, want %t", result, want)
	}
}

func requireMembershipCall(t *testing.T, function Callable, left, right model.Value, want bool) {
	t.Helper()
	result, err := function.Call(t.Context(), []model.Value{left, right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	requireMembershipBoolean(t, result, want)
}

func TestArrayMembershipIgnoresShapeAndBounds(t *testing.T) {
	matrix := replacementArray(model.Int64Val(1), model.Int64Val(2), model.Int64Val(3), model.Int64Val(4))
	matrix.Dimensions = []model.ArrayDimension{{LowerBound: -8, Length: 2}, {LowerBound: 5, Length: 2}}
	maximum := replacementArray(model.Int64Val(1), model.Int64Val(2))
	maximum.Dimensions = []model.ArrayDimension{
		{LowerBound: math.MaxInt32, Length: 1},
		{LowerBound: math.MinInt32, Length: 1},
		{LowerBound: 0, Length: 1},
		{LowerBound: -3, Length: 1},
		{LowerBound: 8, Length: 1},
		{LowerBound: math.MaxInt32 - 1, Length: 2},
	}
	for _, tc := range []struct {
		name        string
		left, right model.Value
	}{
		{"different rank and order", matrix, replacementArray(model.Int64Val(4), model.Int64Val(2), model.Int64Val(1), model.Int64Val(3))},
		{"six dimensions at maximum subscripts", maximum, replacementArray(model.Int64Val(2), model.Int64Val(1))},
	} {
		for _, id := range []model.FunctionID{2747, 2748, 2749} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				function := membershipCallable(t, id, replacementLayout(t, 23, model.TypeInt64), ResolveOptions{})
				requireMembershipCall(t, function, tc.left, tc.right, true)
			})
		}
	}
}

func TestArrayMembershipNullEmptyAndDuplicateSemantics(t *testing.T) {
	layout := replacementLayout(t, 23, model.TypeInt64)
	overlap := membershipCallable(t, 2747, layout, ResolveOptions{})
	contains := membershipCallable(t, 2748, layout, ResolveOptions{})
	contained := membershipCallable(t, 2749, layout, ResolveOptions{})
	empty := replacementArray()
	null := replacementArray(model.NullVal(model.TypeInt64))
	one := replacementArray(model.Int64Val(1))
	for _, tc := range []struct {
		name                         string
		left, right                  model.Value
		overlap, contains, contained bool
	}{
		{"both empty", empty, empty, false, true, true},
		{"empty versus NULL element", empty, null, false, false, true},
		{"NULL element versus empty", null, empty, false, true, false},
		{"NULL never matches NULL", null, null, false, false, false},
		{"empty versus nonnull", empty, one, false, false, true},
		{"nonnull versus empty", one, empty, false, true, false},
		{"duplicate multiplicity ignored", one, replacementArray(model.Int64Val(1), model.Int64Val(1)), true, true, true},
		{"proper subset", one, replacementArray(model.Int64Val(1), model.Int64Val(2)), true, false, true},
		{"proper superset", replacementArray(model.Int64Val(2), model.Int64Val(1)), one, true, true, false},
		{"disjoint members", one, replacementArray(model.Int64Val(2)), false, false, false},
		{"required NULL fails containment", one, replacementArray(model.NullVal(model.TypeInt64), model.Int64Val(1)), true, false, true},
		{"candidate NULL does not hide a match", replacementArray(model.NullVal(model.TypeInt64), model.Int64Val(1)), one, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireMembershipCall(t, overlap, tc.left, tc.right, tc.overlap)
			requireMembershipCall(t, contains, tc.left, tc.right, tc.contains)
			requireMembershipCall(t, contained, tc.left, tc.right, tc.contained)
		})
	}
}

func TestArrayMembershipUsesSelectedEquality(t *testing.T) {
	for _, tc := range []struct {
		name        string
		elementID   model.TypeID
		storage     model.DataType
		left, right model.Value
		want        bool
	}{
		{"float NaNs", 701, model.TypeFloat64, model.Float64Val(math.NaN()), model.Float64Val(math.NaN()), true},
		{"bpchar trailing spaces", 1042, model.TypeString, model.StringVal("a  "), model.StringVal("a"), true},
		{"text trailing spaces", 25, model.TypeString, model.StringVal("a  "), model.StringVal("a"), false},
	} {
		for _, id := range []model.FunctionID{2747, 2748, 2749} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				function := membershipCallable(t, id, replacementLayout(t, tc.elementID, tc.storage), ResolveOptions{Collation: 950})
				requireMembershipCall(t, function, replacementArray(tc.left), replacementArray(tc.right), tc.want)
			})
		}
	}
}

func TestArrayMembershipAbsentEqualityPrecedesMembershipResults(t *testing.T) {
	empty := replacementArray()
	null := replacementArray(model.NullVal(model.TypeString))
	for _, id := range []model.FunctionID{2747, 2748, 2749} {
		function := membershipCallable(t, id, replacementLayout(t, 114, model.TypeString), ResolveOptions{})
		for _, tc := range []struct {
			name        string
			left, right model.Value
		}{
			{"both empty", empty, empty},
			{"empty left", empty, null},
			{"empty right", null, empty},
			{"both NULL elements", null, null},
			{"disjoint values", replacementArray(model.StringVal("0")), replacementArray(model.StringVal("1"), model.StringVal("2"))},
		} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				_, err := function.Call(t.Context(), []model.Value{tc.left, tc.right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireReplacementSQLState(t, err, "42883")
			})
		}
	}
}

func TestArrayMembershipRootNullBypassesShapeAndEquality(t *testing.T) {
	invalid := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal("0")}}
	for _, id := range []model.FunctionID{2747, 2748, 2749} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := membershipCallable(t, id, replacementLayout(t, 114, model.TypeString), ResolveOptions{})
			for _, arguments := range [][2]model.Value{{model.NullVal(model.TypeArray), invalid}, {invalid, model.NullVal(model.TypeArray)}} {
				result, err := function.Call(t.Context(), arguments[:], CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !result.IsNull || result.Type != model.TypeBoolean {
					t.Fatalf("strict NULL membership = %v, %v", result, err)
				}
			}
		})
	}
}

func TestArrayMembershipRejectsInvalidShapesBeforeEmptyResults(t *testing.T) {
	for _, id := range []model.FunctionID{2747, 2748, 2749} {
		function := membershipCallable(t, id, replacementLayout(t, 23, model.TypeInt64), ResolveOptions{})
		for _, tc := range []struct {
			name  string
			array model.Value
		}{
			{"missing dimensions", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}}},
			{"missing elements", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}}},
			{"count mismatch", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(1)}}},
			{"subscript overflow", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: math.MaxInt32, Length: 2}}, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}}},
		} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				for _, arguments := range [][2]model.Value{{tc.array, replacementArray()}, {replacementArray(), tc.array}} {
					_, err := function.Call(t.Context(), arguments[:], CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireReplacementSQLState(t, err, "2202E")
				}
			})
		}
	}
}

func TestArrayMembershipComparisonEffectsFollowPostgresOrder(t *testing.T) {
	null := model.NullVal(model.TypeInt64)
	candidates := replacementArray(null, model.Int64Val(0), model.Int64Val(1), model.Int64Val(2), model.Int64Val(99))
	required := replacementArray(model.Int64Val(2), model.Int64Val(1), model.Int64Val(2))
	for _, tc := range []struct {
		name        string
		id          model.FunctionID
		left, right model.Value
		want        bool
		pairs       [][2]int64
	}{
		{"overlap stops at first match", 2747, replacementArray(null, model.Int64Val(3), model.Int64Val(1), model.Int64Val(99)), replacementArray(null, model.Int64Val(0), model.Int64Val(2), model.Int64Val(1)), true, [][2]int64{{3, 0}, {3, 2}, {3, 1}, {1, 0}, {1, 2}, {1, 1}}},
		{"contains searches RHS in LHS", 2748, candidates, required, true, [][2]int64{{2, 0}, {2, 1}, {2, 2}, {1, 0}, {1, 1}, {2, 0}, {2, 1}, {2, 2}}},
		{"contained searches LHS in RHS", 2749, required, candidates, true, [][2]int64{{2, 0}, {2, 1}, {2, 2}, {1, 0}, {1, 1}, {2, 0}, {2, 1}, {2, 2}}},
		{"contains stops at missing member", 2748, replacementArray(null, model.Int64Val(0), model.Int64Val(2)), replacementArray(model.Int64Val(1), model.Int64Val(99)), false, [][2]int64{{1, 0}, {1, 2}}},
		{"contained stops at missing member", 2749, replacementArray(model.Int64Val(1), model.Int64Val(99)), replacementArray(null, model.Int64Val(0), model.Int64Val(2)), false, [][2]int64{{1, 0}, {1, 2}}},
		{"contains stops at required NULL", 2748, replacementArray(model.Int64Val(99)), replacementArray(null, model.Int64Val(99)), false, nil},
		{"contained stops at required NULL", 2749, replacementArray(null, model.Int64Val(99)), replacementArray(model.Int64Val(99)), false, nil},
		{"overlap skips NULL members", 2747, replacementArray(null, null), replacementArray(model.Int64Val(99)), false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var pairs [][2]int64
			element := replacementEquality(func(_ context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
				if arguments[0].IsNull || arguments[1].IsNull {
					t.Fatal("membership passed NULL to non-strict equality")
				}
				left, right := arguments[0].IntVal, arguments[1].IntVal
				pairs = append(pairs, [2]int64{left, right})
				if left == 99 || right == 99 {
					return model.Value{}, &model.Error{Code: "22003", Message: "unreached comparison"}
				}
				if right == 0 {
					return model.NullVal(model.TypeBoolean), nil
				}
				return model.BoolVal(left == right), nil
			})
			kernel := compileArrayMembership(Signature{ID: tc.id}, element, nil)
			result, err := kernel(t.Context(), []model.Value{tc.left, tc.right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireMembershipBoolean(t, result, tc.want)
			if !slices.Equal(pairs, tc.pairs) {
				t.Fatalf("comparison effects = %v, want %v", pairs, tc.pairs)
			}
		})
	}
}

func TestArrayMembershipNullEqualityIsNotAMatch(t *testing.T) {
	element := replacementEquality(func(context.Context, []model.Value, CallBudget) (model.Value, error) {
		return model.NullVal(model.TypeBoolean), nil
	})
	for _, id := range []model.FunctionID{2747, 2748, 2749} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			kernel := compileArrayMembership(Signature{ID: id}, element, nil)
			value := replacementArray(model.Int64Val(1))
			result, err := kernel(t.Context(), []model.Value{value, value}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireMembershipBoolean(t, result, false)
		})
	}
}

func TestArrayMembershipStopsAtFirstEqualityError(t *testing.T) {
	searched := replacementArray(model.Int64Val(1), model.Int64Val(2))
	candidates := replacementArray(model.Int64Val(3), model.Int64Val(4))
	for _, tc := range []struct {
		id          model.FunctionID
		left, right model.Value
	}{
		{2747, searched, candidates},
		{2748, candidates, searched},
		{2749, searched, candidates},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			first := &model.Error{Code: "22003", Message: "first comparison failed"}
			later := &model.Error{Code: "22012", Message: "later comparison failed"}
			var pairs [][2]int64
			element := replacementEquality(func(_ context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
				left, right := arguments[0].IntVal, arguments[1].IntVal
				pairs = append(pairs, [2]int64{left, right})
				if left == 2 {
					return model.Value{}, later
				}
				if right == 4 {
					return model.Value{}, first
				}
				return model.BoolVal(false), nil
			})
			kernel := compileArrayMembership(Signature{ID: tc.id}, element, nil)
			_, err := kernel(t.Context(), []model.Value{tc.left, tc.right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if !errors.Is(err, first) {
				t.Fatalf("membership error = %v, want %v", err, first)
			}
			if !slices.Equal(pairs, [][2]int64{{1, 3}, {1, 4}}) {
				t.Fatalf("comparisons continued past first error: %v", pairs)
			}
		})
	}
}

func TestArrayMembershipCancelsNullOnlyScans(t *testing.T) {
	values := make([]model.Value, 4096)
	for i := range values {
		values[i] = model.NullVal(model.TypeInt64)
	}
	nulls := replacementArray(values...)
	one := replacementArray(model.Int64Val(1))
	for _, tc := range []struct {
		name        string
		id          model.FunctionID
		left, right model.Value
	}{
		{"overlap outer NULL scan", 2747, nulls, one},
		{"overlap inner NULL scan", 2747, one, nulls},
		{"contains inner NULL scan", 2748, nulls, one},
		{"contained inner NULL scan", 2749, one, nulls},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := membershipCallable(t, tc.id, replacementLayout(t, 23, model.TypeInt64), ResolveOptions{})
			base, cancel := context.WithCancel(t.Context())
			t.Cleanup(cancel)
			ctx := &arrayPositionCancelContext{Context: base, cancel: cancel, remaining: 20}
			_, err := function.Call(ctx, []model.Value{tc.left, tc.right}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted membership scan = %v, want cancellation", err)
			}
		})
	}
}

func TestArrayMembershipBorrowsLargeInputsAtZeroOutputBudget(t *testing.T) {
	payload := make([]byte, 8192)
	payload[0], payload[len(payload)-1] = 1, 2
	values := make([]model.Value, 4096)
	for i := range values {
		values[i] = model.BytesVal(payload)
	}
	large := replacementArray(values...)
	one := replacementArray(model.BytesVal(slices.Clone(payload)))
	for _, id := range []model.FunctionID{2747, 2748, 2749} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := membershipCallable(t, id, replacementLayout(t, 17, model.TypeBytes), ResolveOptions{})
			requireMembershipCall(t, function, large, one, true)
			if large.Dimensions[0].LowerBound != -3 || large.Dimensions[0].Length != 4096 || payload[0] != 1 || payload[len(payload)-1] != 2 {
				t.Fatal("membership mutated its borrowed input")
			}
		})
	}
}
