package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestJSONBExistsMatchesOnlyTopLevelStrings(t *testing.T) {
	for _, tc := range []struct {
		name, input, key string
		want             bool
	}{
		{"object_key_with_null_value", `{"a":null}`, "a", true},
		{"object_value_is_not_key", `{"a":"value"}`, "value", false},
		{"nested_object_key", `{"outer":{"a":1}}`, "a", false},
		{"nested_array_string", `[["a"],{"a":1}]`, "a", false},
		{"array_string", `[1,false,null,{"x":1},["x"],"a"]`, "a", true},
		{"array_number_is_not_text", `[1]`, "1", false},
		{"array_boolean_is_not_text", `[true]`, "true", false},
		{"array_null_is_not_text", `[null]`, "null", false},
		{"array_null_string", `[null,"null"]`, "null", true},
		{"root_scalar_string", `"a"`, "a", true},
		{"root_scalar_number", `1`, "1", false},
		{"root_scalar_boolean", `true`, "true", false},
		{"root_scalar_null", `null`, "null", false},
		{"empty_key", `{"":1}`, "", true},
		{"empty_array_string", `[""]`, "", true},
		{"empty_root_string", `""`, "", true},
		{"empty_object", `{}`, "", false},
		{"empty_array", `[]`, "", false},
		{"decoded_duplicate_key", `{"a":1,"\u0061":null}`, "a", true},
		{"decoded_unicode_key", `{"\u00e9\ud83d\ude00\/":1}`, "é\U0001f600/", true},
		{"decoded_control_element", `["\n\t\\\""]`, "\n\t\\\"", true},
		{"case_sensitive_key", `{"A":1}`, "a", false},
		{"nul_is_not_string_terminator", `{"a":1}`, "a\x00suffix", false},
		{"nul_is_not_empty_key", `{"":1}`, "\x00", false},
		{"nul_is_not_array_terminator", `["a"]`, "a\x00", false},
		{"nul_is_not_scalar_terminator", `"a"`, "a\x00", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []model.FunctionID{4047, 4048, 4049} {
				key := model.StringVal(tc.key)
				if id != 4047 {
					key = jsonPathArray(tc.key)
				}
				value, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(tc.input), key}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || value.IsNull || value.Type != model.TypeBoolean || value.IntVal != model.BoolVal(tc.want).IntVal {
					t.Errorf("function %d membership = %v, %v; want %v", id, value, err, tc.want)
				}
			}
		})
	}
}

func TestJSONBExistsAnyAllUseRowMajorNonNullKeys(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		keys        model.Value
		any, all    bool
	}{
		{"empty_keys", `null`, jsonPathArray(), false, true},
		{"only_null_keys", `[]`, model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.NullVal(model.TypeString)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -7}}}, false, true},
		{"duplicate_keys_need_one_member", `["a"]`, jsonPathArray("a", "a"), true, true},
		{"missing_before_match", `{"a":null}`, jsonPathArray("absent", "a"), true, false},
		{"missing_after_match", `{"a":null}`, jsonPathArray("a", "absent"), true, false},
		{"scalar_multiple_keys", `"a"`, jsonPathArray("a", "b"), true, false},
		{"all_members", `{"a":1,"b":2,"a":null}`, jsonPathArray("b", "a"), true, true},
		{"nul_key_does_not_match_prefix", `{"a":1}`, jsonPathArray("a", "a\x00"), true, false},
		{
			"multidimensional_null_keys", `{"a":1,"b":null}`,
			model.Value{
				Type:       model.TypeArray,
				ArrayVal:   []model.Value{model.NullVal(model.TypeString), model.StringVal("a"), model.StringVal("b"), model.NullVal(model.TypeString)},
				Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -2}, {Length: 2, LowerBound: 5}},
			},
			true, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, operation := range []struct {
				id   model.FunctionID
				want bool
			}{{4048, tc.any}, {4049, tc.all}} {
				value, err := jsonCallable(t, operation.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.keys}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || value.IsNull || value.Type != model.TypeBoolean || value.IntVal != model.BoolVal(operation.want).IntVal {
					t.Errorf("function %d membership = %v, %v; want %v", operation.id, value, err, operation.want)
				}
			}
		})
	}
}

func TestJSONBExistsStrictNullBypassesInvalidDocument(t *testing.T) {
	for _, id := range []model.FunctionID{4047, 4048, 4049} {
		keyType := model.TypeString
		key := model.StringVal("a")
		if id != 4047 {
			keyType = model.TypeArray
			key = jsonPathArray("a")
		}
		for _, args := range [][]model.Value{
			{jsonDatum(`{"a":"\u0000"}`), model.NullVal(keyType)},
			{model.NullVal(model.TypeJSONB), key},
		} {
			value, err := jsonCallable(t, id).Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != model.TypeBoolean {
				t.Errorf("function %d strict NULL = %v, %v; want boolean NULL", id, value, err)
			}
		}
	}
}

func TestJSONBExistsValidatesUnselectedInputBeforeReturning(t *testing.T) {
	for _, tc := range []struct {
		name, input, code string
	}{
		{"after_matched_key", `{"a":1,"bad":"\u0000"}`, "22P05"},
		{"overwritten_value", `{"a":"\u0000","a":1}`, "22P05"},
		{"unselected_surrogate", `{"a":1,"bad":["\ud800"]}`, "22P02"},
		{"unselected_utf8", "{\"a\":1,\"bad\":\"\xff\"}", "22021"},
		{"unselected_numeric_overflow", `{"a":1,"bad":1e131072}`, "22003"},
		{"unselected_numeric_underflow", `{"a":1,"bad":1e-16384}`, "22003"},
		{"number_error_before_unicode", `{"a":1e131072,"bad":"\u0000"}`, "22003"},
		{"unicode_error_before_number", `{"a":"\u0000","bad":1e131072}`, "22P05"},
		{"unicode_error_before_syntax", `{"a":1,"bad":"\u0000\q"}`, "22P05"},
		{"after_array_match", `["a","\u0000"]`, "22P05"},
		{"trailing_input", `{"a":1} []`, "22P02"},
		{"trailing_nul", "{\"a\":1}\x00", "22P02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []model.FunctionID{4047, 4048, 4049} {
				key := model.StringVal("a")
				if id != 4047 {
					key = jsonPathArray("a")
				}
				_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(tc.input), key}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, tc.code)
			}
		})
	}
	for _, id := range []model.FunctionID{4048, 4049} {
		for _, keys := range []model.Value{
			jsonPathArray(),
			{Type: model.TypeArray, ArrayVal: []model.Value{model.NullVal(model.TypeString)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}},
			jsonPathArray("absent"),
		} {
			_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(`{"bad":"\u0000"}`), keys}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22P05")
		}
	}
}

func TestJSONBExistsRejectsMalformedKeyArrays(t *testing.T) {
	for _, id := range []model.FunctionID{4048, 4049} {
		keys := jsonPathArray("a")
		keys.Dimensions[0].Length = 2
		_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(`{"a":1}`), keys}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "2202E")
		keys = jsonPathArray("a", "b")
		keys.ArrayVal[1] = model.Int64Val(1)
		_, err = jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(`{"a":1}`), keys}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "42804")
	}
}

func TestJSONBExistsBoundsTraversalWithoutOutputBudget(t *testing.T) {
	large := strings.Repeat("x", 1<<16)
	nested := strings.Repeat("[", 6399) + "0" + strings.Repeat("]", 6399)
	for _, id := range []model.FunctionID{4047, 4048, 4049} {
		key := model.StringVal(large)
		if id != 4047 {
			key = jsonPathArray(large)
		}
		input := `{"` + large + `":` + nested + `}`
		value, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(input), key}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || value.IsNull || value.IntVal != 1 {
			t.Fatalf("function %d bounded membership = %v, %v; want true", id, value, err)
		}
		_, err = jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(`{"a":[` + nested + `]}`), key}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54001")
	}
}

func TestJSONBExistsPreservesCancellation(t *testing.T) {
	large := strings.Repeat("x", 1<<20)
	manyKeys := jsonPathArray()
	manyKeys.ArrayVal = make([]model.Value, 8192)
	manyKeys.Dimensions = []model.ArrayDimension{{Length: 8192, LowerBound: 1}}
	for i := range manyKeys.ArrayVal {
		manyKeys.ArrayVal[i] = model.NullVal(model.TypeString)
	}
	for _, tc := range []struct {
		name, input string
		id          model.FunctionID
		keys        model.Value
		remaining   int
	}{
		{"unselected_document", `{"a":1,"bad":"` + large + `"}`, 4047, model.StringVal("a"), 12},
		{"large_matching_string", `["` + large + `"]`, 4048, jsonPathArray(large), 12},
		{"large_search_key", `{"a":1}`, 4049, jsonPathArray(large), 64},
		{"null_key_scan", `{}`, 4048, manyKeys, 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: tc.remaining}
			value, err := jsonCallable(t, tc.id).Call(ctx, []model.Value{jsonDatum(tc.input), tc.keys}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) || value.StrVal != "" || value.ArrayVal != nil {
				t.Fatalf("cancelled membership = %v, %v; want cancellation without payload", value, err)
			}
		})
	}
}

func TestJSONBExistsResultsRetainNoBorrowedPayload(t *testing.T) {
	large := strings.Repeat("x", 1<<16)
	for _, id := range []model.FunctionID{4047, 4048, 4049} {
		function := jsonCallable(t, id)
		key := model.StringVal(large)
		if id != 4047 {
			key = jsonPathArray(large)
		}
		args := []model.Value{jsonDatum(`{"` + large + `":null}`), key}
		first, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || first.IsNull || first.IntVal != 1 {
			t.Fatalf("function %d first membership = %v, %v; want true", id, first, err)
		}
		args[0] = jsonDatum(`{"different":1}`)
		second, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || second.IsNull || second.IntVal != 0 || first.IntVal != 1 {
			t.Fatalf("function %d reused membership = %v, %v; want false with unchanged first result", id, second, err)
		}
		for _, result := range []model.Value{first, second} {
			textPayload := result.StrVal != "" || result.BytesVal != nil
			collectionPayload := result.VectorVal != nil || result.ArrayVal != nil || result.Dimensions != nil
			if result.Type != model.TypeBoolean || textPayload || collectionPayload {
				t.Fatalf("function %d boolean retains variable-width payload: %v", id, result)
			}
		}
	}
}
