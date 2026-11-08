package builtin_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestMakeDateGregorianAndEpochBoundaries(t *testing.T) {
	function := jsonCallable(t, 3846)
	for _, tc := range []struct {
		year, month, day, want int64
		code                   string
	}{
		{2000, 1, 1, 0, ""}, {2000, 2, 29, 59, ""}, {1900, 2, 29, 0, "22008"}, {0, 1, 1, 0, "22008"},
		{-1, 2, 29, -730426, ""}, {-4714, 11, 24, -2451545, ""}, {-4714, 11, 23, 0, "22008"},
		{5874897, 12, 31, 2145031948, ""}, {5874898, 1, 1, 0, "22008"}, {math.MinInt32, 1, 1, 0, "22008"},
	} {
		value, err := function.Call(t.Context(), []model.Value{model.Int64Val(tc.year), model.Int64Val(tc.month), model.Int64Val(tc.day)}, builtin.CallBudget{})
		if tc.code != "" {
			requireSQLState(t, err, tc.code)
			continue
		}
		if err != nil || value.Type != model.TypeTimestamp || value.IntVal != tc.want {
			t.Errorf("make_date(%d,%d,%d)=%v,%v; want %d", tc.year, tc.month, tc.day, value, err, tc.want)
		}
	}
}

func TestDateArithmeticPreservesInfinityAndChecksFiniteRange(t *testing.T) {
	date := func(value int64) model.Value { return model.Value{Type: model.TypeTimestamp, IntVal: value} }
	for _, id := range []model.FunctionID{1141, 1142} {
		function := jsonCallable(t, id)
		value, err := function.Call(t.Context(), []model.Value{date(math.MaxInt32), model.Int64Val(math.MinInt32)}, builtin.CallBudget{})
		if err != nil || value.IntVal != math.MaxInt32 {
			t.Fatalf("infinite date arithmetic=%v,%v", value, err)
		}
	}
	_, err := jsonCallable(t, 1141).Call(t.Context(), []model.Value{date(2145031948), model.Int64Val(1)}, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
	_, err = jsonCallable(t, 1142).Call(t.Context(), []model.Value{date(-2451545), model.Int64Val(1)}, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
	_, err = jsonCallable(t, 1140).Call(t.Context(), []model.Value{date(math.MinInt32), date(0)}, builtin.CallBudget{})
	requireSQLState(t, err, "22008")
	value, err := jsonCallable(t, 1140).Call(t.Context(), []model.Value{date(2145031948), date(-2451545)}, builtin.CallBudget{})
	if err != nil || value.IntVal != 2147483493 {
		t.Fatalf("full supported date span=%v,%v", value, err)
	}
}
