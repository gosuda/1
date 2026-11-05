package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestTimeExtractionExactFractionScales(t *testing.T) {
	value := model.Value{Type: model.TypeTimestamp, IntVal: 45296123456}
	for _, tc := range []struct{ field, want string }{{"hour", "12"}, {"minute", "34"}, {"second", "56.123456"}, {"milliseconds", "56123.456"}, {"microseconds", "56123456"}, {"epoch", "45296.123456"}} {
		result, err := jsonCallable(t, 6200).Call(t.Context(), []model.Value{model.StringVal(tc.field), value}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: uint64(len(tc.want))})
		if err != nil || result.StrVal != tc.want {
			t.Errorf("extract %s=%v,%v; want%q", tc.field, result, err, tc.want)
		}
	}
	result, err := jsonCallable(t, 6200).Call(t.Context(), []model.Value{model.StringVal("hour"), {Type: model.TypeTimestamp, IntVal: 86400000000}}, builtin.CallBudget{OutputBytes: 2, MemoryBytes: 2})
	if err != nil || result.StrVal != "24" {
		t.Fatalf("end-of-day hour=%v,%v", result, err)
	}
}

func TestTimeExtractionDistinguishesUnknownAndUnsupportedFields(t *testing.T) {
	for _, id := range []model.FunctionID{1385, 6200} {
		for _, tc := range []struct{ field, code string }{{"year", "0A000"}, {"timezone", "0A000"}, {"invalid", "22023"}, {"now", "22023"}} {
			_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{model.StringVal(tc.field), {Type: model.TypeTimestamp, IntVal: 0}}, builtin.CallBudget{})
			requireSQLState(t, err, tc.code)
		}
	}
}
