package span_test

import (
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestDateTimeSpansMatchScalarComparisonBoundaries(t *testing.T) {
	for _, tc := range []struct {
		id        model.TypeID
		bound     int64
		functions []model.FunctionID
		values    []int64
	}{
		{1082, 0, []model.FunctionID{1086, 1087, 1088, 1089, 1090}, []int64{math.MinInt32, -1, 0, 1, math.MaxInt32}},
		{1083, 43200000000, []model.FunctionID{1145, 1102, 1103, 1104, 1105}, []int64{0, 43199999999, 43200000000, 43200000001, 86400000000}},
	} {
		for _, function := range tc.functions {
			callable, err := builtin.Resolve(function, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			typ := model.Type{ID: tc.id, Storage: model.TypeTimestamp, Modifier: -1}
			if tc.id == 1083 {
				typ.Modifier = 6
			}
			f := setup(t, typ)
			predicate := f.timestampPredicate(function, tc.bound)
			result := extract(t, f.graph(), predicate, f.keys, nil, limits())
			if result.Residual != 0 {
				t.Fatalf("date/time function%d retained residual", function)
			}
			for _, value := range tc.values {
				datum := model.Value{Type: model.TypeTimestamp, IntVal: value}
				actual, err := callable.Call(t.Context(), []model.Value{datum, {Type: model.TypeTimestamp, IntVal: tc.bound}}, builtin.CallBudget{})
				if err != nil {
					t.Fatal(err)
				}
				got := matches(t, f.keys, result, datum)
				if got != (actual.IntVal == 1) {
					t.Fatalf("function%d value%d key match=%v scalar=%v", function, value, got, actual)
				}
			}
			if matches(t, f.keys, result, model.NullVal(model.TypeTimestamp)) {
				t.Fatal("strict date/time bound included NULL")
			}
		}
	}
}
