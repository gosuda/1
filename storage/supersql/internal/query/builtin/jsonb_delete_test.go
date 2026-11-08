package builtin_test

import (
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestJSONBDeletePathValidatesOnlyReachedSteps(t *testing.T) {
	for _, tc := range []struct {
		name, input, want, state string
		path                     model.Value
	}{
		{"empty_root_skips_null", `{}`, `{}`, "", jsonPathArray("ignored", "ignored")},
		{"missing_prefix_skips_null", `{"b":1}`, `{"b": 1}`, "", jsonPathArray("a", "ignored")},
		{"scalar_prefix_reaches_null", `{"a":1}`, "", "22004", jsonPathArray("a", "ignored")},
		{"empty_object_reaches_null", `{"a":{}}`, "", "22004", jsonPathArray("a", "ignored")},
		{"empty_array_reaches_null", `{"a":[]}`, "", "22004", jsonPathArray("a", "ignored")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.path.ArrayVal[len(tc.path.ArrayVal)-1] = model.NullVal(model.TypeString)
			value, err := jsonCallable(t, 3304).Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if tc.state != "" {
				requireSQLState(t, err, tc.state)
				return
			}
			if err != nil || value.StrVal != tc.want {
				t.Fatalf("delete path = %q, %v; want %q", value.StrVal, err, tc.want)
			}
		})
	}
}

func TestJSONBDeletePathIndexParsingDependsOnReachedContainer(t *testing.T) {
	for _, tc := range []struct {
		name, input, index, want, state string
	}{
		{"empty_root_skips_bad_index", `[]`, "bad", `[]`, ""},
		{"empty_nested_array_parses_index", `{"a":[]}`, "bad", "", "22P02"},
		{"empty_nested_object_uses_key", `{"a":{}}`, "bad", `{"a": {}}`, ""},
		{"scalar_skips_bad_index", `{"a":1}`, "bad", `{"a": 1}`, ""},
		{"int4_minimum_is_valid", `{"a":[1,2]}`, "-2147483648", `{"a": [1, 2]}`, ""},
		{"int4_underflow_is_invalid", `{"a":[1,2]}`, "-2147483649", "", "22P02"},
		{"int4_overflow_is_invalid", `{"a":[1,2]}`, "2147483648", "", "22P02"},
		{"leading_whitespace_and_sign", `{"a":[1,2]}`, "\v\f +01", `{"a": [1]}`, ""},
		{"trailing_whitespace_is_invalid", `{"a":[1,2]}`, "1 ", "", "22P02"},
		{"nul_terminates_index", `{"a":[1,2]}`, "1\x00ignored", `{"a": [1]}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, 3304).Call(t.Context(), []model.Value{jsonDatum(tc.input), jsonPathArray("a", tc.index)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if tc.state != "" {
				requireSQLState(t, err, tc.state)
				return
			}
			if err != nil || value.StrVal != tc.want {
				t.Fatalf("delete index %q = %q, %v; want %q", tc.index, value.StrVal, err, tc.want)
			}
		})
	}
}

func TestJSONBDeleteTextArrayIgnoresNullsWithoutRecursing(t *testing.T) {
	keys := jsonPathArray("a", "unused", "a")
	keys.ArrayVal[1] = model.NullVal(model.TypeString)
	keys.Dimensions[0].LowerBound = -7
	for _, tc := range []struct{ input, want string }{
		{`["a",["a"],{"a":null},"a",1,null,"b"]`, `[["a"], {"a": null}, 1, null, "b"]`},
		{`{"a":1,"b":{"a":2},"a":3}`, `{"b": {"a": 2}}`},
	} {
		value, err := jsonCallable(t, 3343).Call(t.Context(), []model.Value{jsonDatum(tc.input), keys}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != tc.want {
			t.Fatalf("delete keys = %q, %v; want %q", value.StrVal, err, tc.want)
		}
	}
}

func TestJSONBDeleteRejectsDimensionsBeforeScalarOrEmptyRoot(t *testing.T) {
	keys := jsonPathArray("a", "b")
	keys.Dimensions = []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 2, LowerBound: 1}}
	for _, id := range []model.FunctionID{3304, 3343} {
		for _, input := range []string{`1`, `{}`} {
			_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(input), keys}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "2202E")
		}
	}
}

func TestJSONBDeleteNoopSurvivesBorrowedInputReuse(t *testing.T) {
	const input = `{"a": null}`
	for _, tc := range []struct {
		id  model.FunctionID
		arg model.Value
	}{
		{3302, model.Value{Type: model.TypeString, StrVal: "missing"}},
		{3304, jsonPathArray("missing")},
		{3343, jsonPathArray()},
	} {
		function := jsonCallable(t, tc.id)
		buffer := []byte(input)
		args := []model.Value{jsonDatum(unsafe.String(unsafe.SliceData(buffer), len(buffer))), tc.arg}
		value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		args[0] = model.Value{}
		for i := range buffer {
			buffer[i] = 'x'
		}
		if value.StrVal != input {
			t.Fatalf("noop delete %d changed after input reuse: %q", tc.id, value.StrVal)
		}
	}
}

func TestJSONBDeleteIntegerCountsFromEndWithoutDescending(t *testing.T) {
	for _, tc := range []struct {
		input, want, state string
		index              int64
	}{
		{`[[1],2,3]`, `[[1], 2]`, "", -1},
		{`[[1],2,3]`, `[2, 3]`, "", -3},
		{`[[1],2,3]`, `[[1], 2, 3]`, "", -4},
		{`[1]`, `[1]`, "", -2147483648},
		{`[1]`, `[1]`, "", 2147483647},
		{`{}`, "", "22023", 0},
		{`null`, "", "22023", 0},
	} {
		value, err := jsonCallable(t, 3303).Call(t.Context(), []model.Value{jsonDatum(tc.input), model.Int64Val(tc.index)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		if tc.state != "" {
			requireSQLState(t, err, tc.state)
			continue
		}
		if err != nil || value.StrVal != tc.want {
			t.Fatalf("delete index %d from %s = %q, %v; want %q", tc.index, tc.input, value.StrVal, err, tc.want)
		}
	}
}

func TestJSONBDeleteTextPreservesNestedValuesAndExactNumbers(t *testing.T) {
	for _, tc := range []struct{ input, key, want string }{
		{`["a",["a"],{"a":null},"a",1.2300e2]`, "a", `[["a"], {"a": null}, 123.00]`},
		{`{"a":1,"\u0061":2,"b":1.2300e2}`, "a", `{"b": 123.00}`},
		{`{"a":1}`, "a\x00ignored", `{"a": 1}`},
	} {
		value, err := jsonCallable(t, 3302).Call(t.Context(), []model.Value{jsonDatum(tc.input), model.StringVal(tc.key)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != tc.want {
			t.Fatalf("delete key %q = %q, %v; want %q", tc.key, value.StrVal, err, tc.want)
		}
	}
}
