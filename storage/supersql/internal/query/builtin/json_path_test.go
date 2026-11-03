package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func jsonPathArray(parts ...string) model.Value {
	values := make([]model.Value, len(parts))
	for i, part := range parts {
		values[i] = model.StringVal(part)
	}
	value := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(parts) > 0 {
		value.Dimensions = []model.ArrayDimension{{Length: int32(len(parts)), LowerBound: 1}}
	}
	return value
}

func TestJSONPathPreservesSourceOrderAcrossDuplicatePrefixes(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		path        []string
		want        string
	}{
		{"last full match survives missing suffix", `{"a":{"b":1},"a":{}}`, []string{"a", "b"}, "1"},
		{"negative index mutates across duplicate arrays", `{"a":[1],"a":[2,3]}`, []string{"a", "-1"}, "2"},
		{"unselected array establishes negative index", `{"z":[8],"a":[2,3]}`, []string{"a", "-1"}, "2"},
		{"nested raw span", `{"a":[0,{"b": [ 1e+2, 3 ]}]}`, []string{"a", "-1", "b"}, `[ 1e+2, 3 ]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, 3951).Call(t.Context(), []model.Value{jsonDatum(tc.input), jsonPathArray(tc.path...)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("path %v = %v, %v; want %q", tc.path, value, err, tc.want)
			}
		})
	}
}

func TestJSONPathNullElementBypassesDecodedDocument(t *testing.T) {
	path := jsonPathArray("a", "b")
	path.ArrayVal[1] = model.NullVal(model.TypeString)
	value, err := jsonCallable(t, 3953).Call(t.Context(), []model.Value{jsonDatum(`{"a":"\u0000"}`), path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull {
		t.Fatalf("NULL path = %v, %v; want SQL NULL", value, err)
	}
}

func TestJSONEmptyPathNormalizesOnlyTextScalars(t *testing.T) {
	for _, tc := range []struct {
		id          model.FunctionID
		input, want string
		null        bool
	}{
		{3951, `  "\u0061"  `, `"\u0061"`, false},
		{3953, `  "\u0061"  `, "a", false},
		{3951, ` null `, "null", false},
		{3953, ` null `, "", true},
	} {
		value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), jsonPathArray()}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
		if err != nil || value.IsNull != tc.null || value.StrVal != tc.want {
			t.Errorf("function %d input %q = %v, %v; want %q null=%v", tc.id, tc.input, value, err, tc.want, tc.null)
		}
	}
}

func TestJSONPathValidatesUnselectedEscapes(t *testing.T) {
	_, err := jsonCallable(t, 3953).Call(t.Context(), []model.Value{jsonDatum(`{"a":1,"z":"\u0000"}`), jsonPathArray("a")}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22P05")
}
