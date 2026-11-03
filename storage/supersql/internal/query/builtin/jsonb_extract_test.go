package builtin_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestJSONBFieldExtractionCanonicalizesLastDecodedKey(t *testing.T) {
	const input = ` { "a": {"old":true}, "nested":{"a":9}, "\u0061": {"zz":1e2,"b":[-0.00,"\u0061\/",{"k":0,"\u006b":2}]} } `
	const want = `{"b": [0.00, "a/", {"k": 2}], "zz": 100}`
	for _, id := range []model.FunctionID{3478, 3214} {
		t.Run(jsonCallable(t, id).Signature().Name, func(t *testing.T) {
			value, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(input), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 1 << 16, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != want {
				t.Fatalf("last decoded key = %v, %v; want %q", value, err, want)
			}
		})
	}
}

func TestJSONBExtractionCanonicalScalarRepresentations(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		path              model.Value
	}{
		{"quoted_string", `{"a":"\u00e9\n\ud83d\ude00\/"}`, "\"é\\n\U0001f600/\"", 3478, model.StringVal("a")},
		{"decoded_string", `{"a":"\u00e9\n\ud83d\ude00\/"}`, "é\n\U0001f600/", 3214, model.StringVal("a")},
		{"number_scale", `{"a":1.2300e2}`, "123.00", 3214, model.StringVal("a")},
		{"negative_first_index", `[-0.000,1,2]`, "0.000", 3215, model.Int64Val(-3)},
		{"negative_nested_container", `[0, {"zz":1,"b":[true,null]}, 2]`, `{"b": [true, null], "zz": 1}`, 3216, model.Int64Val(-2)},
		{"array_quoted_string", `["\u0061"]`, `"a"`, 3215, model.Int64Val(0)},
		{"array_decoded_string", `["\u00e9"]`, "é", 3216, model.Int64Val(0)},
		{"field_null_replaces_container", `{"a":{"x":1},"a":null}`, "null", 3478, model.StringVal("a")},
		{"array_json_null", `[null]`, "null", 3215, model.Int64Val(-1)},
		{"empty_key", `{"":true,"a":false}`, "true", 3214, model.StringVal("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: 1 << 16, MemoryBytes: 64 << 20})
			wantType := model.TypeJSONB
			if tc.id == 3214 || tc.id == 3216 {
				wantType = model.TypeString
			}
			if err != nil || value.IsNull || value.Type != wantType || value.StrVal != tc.want {
				t.Fatalf("extraction = %v, %v; want type %v, %q", value, err, wantType, tc.want)
			}
		})
	}
}

func TestJSONBArrayExtractionIndexesRootScalars(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		index             int64
	}{
		{"number_at_zero", ` 1.2300e2 `, "123.00", 3215, 0},
		{"number_at_minus_one", ` -0.00 `, "0.00", 3216, -1},
		{"string_at_zero", ` "\u0061" `, `"a"`, 3215, 0},
		{"string_at_minus_one", ` "\u00e9" `, "é", 3216, -1},
		{"boolean_at_zero", ` false `, "false", 3215, 0},
		{"null_at_minus_one", ` null `, "null", 3215, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), model.Int64Val(tc.index)}, builtin.CallBudget{OutputBytes: 128, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("root scalar at %d = %v, %v; want %q", tc.index, value, err, tc.want)
			}
		})
	}
}

func TestJSONBExtractionNullPathsNeedNoOutputBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.FunctionID
		input model.Value
		path  model.Value
	}{
		{"missing_field", 3478, jsonDatum(`{"present":1}`), model.StringVal("absent")},
		{"nested_field_is_not_root_field", 3214, jsonDatum(`{"nested":{"a":1}}`), model.StringVal("a")},
		{"text_key_does_not_index_array", 3478, jsonDatum(`[1]`), model.StringVal("0")},
		{"integer_does_not_match_object_key", 3215, jsonDatum(`{"0":1}`), model.Int64Val(0)},
		{"field_on_scalar", 3214, jsonDatum(`"text"`), model.StringVal("text")},
		{"empty_array", 3215, jsonDatum(`[]`), model.Int64Val(-1)},
		{"positive_out_of_range", 3215, jsonDatum(`[1,2]`), model.Int64Val(2)},
		{"negative_out_of_range", 3216, jsonDatum(`[1,2]`), model.Int64Val(-3)},
		{"minimum_index", 3215, jsonDatum(`[1]`), model.Int64Val(math.MinInt32)},
		{"maximum_index", 3216, jsonDatum(`[1]`), model.Int64Val(math.MaxInt32)},
		{"scalar_positive_out_of_range", 3215, jsonDatum(`true`), model.Int64Val(1)},
		{"scalar_negative_out_of_range", 3216, jsonDatum(`1`), model.Int64Val(-2)},
		{"field_text_null", 3214, jsonDatum(`{"a":1,"a":null}`), model.StringVal("a")},
		{"element_text_null", 3216, jsonDatum(`[1,null]`), model.Int64Val(-1)},
		{"scalar_text_null", 3216, jsonDatum(`null`), model.Int64Val(0)},
		{"nul_key_is_not_cstring", 3478, jsonDatum(`{"a":1}`), model.StringVal("a\x00ignored")},
		{"nul_key_is_not_empty_key", 3214, jsonDatum(`{"":1}`), model.StringVal("\x00")},
		{"null_document", 3478, model.NullVal(model.TypeJSONB), model.StringVal("a")},
		{"null_field", 3214, jsonDatum(`{"a":"\u0000"}`), model.NullVal(model.TypeString)},
		{"null_index", 3215, jsonDatum(`["\u0000"]`), model.NullVal(model.TypeInt64)},
		{"null_document_text_result", 3216, model.NullVal(model.TypeJSONB), model.Int64Val(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{tc.input, tc.path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			wantType := model.TypeJSONB
			if tc.id == 3214 || tc.id == 3216 {
				wantType = model.TypeString
			}
			if err != nil || !value.IsNull || value.Type != wantType {
				t.Fatalf("NULL extraction = %v, %v; want typed NULL %v", value, err, wantType)
			}
		})
	}
}

func TestJSONBExtractionValidatesCompleteRawRepresentation(t *testing.T) {
	for _, tc := range []struct {
		name, input, code string
		id                model.FunctionID
		path              model.Value
	}{
		{"overwritten_value", `{"a":"\u0000","a":1}`, "22P05", 3478, model.StringVal("a")},
		{"overwritten_number", `{"a":1e131072,"a":1}`, "22003", 3214, model.StringVal("a")},
		{"unselected_numeric_range", `{"a":1,"other":1e-16384}`, "22003", 3478, model.StringVal("a")},
		{"numeric_error_before_decoded_error", `{"a":1e131072,"other":"\u0000"}`, "22003", 3214, model.StringVal("missing")},
		{"decoded_error_before_numeric_error", `{"a":"\u0000","other":1e131072}`, "22P05", 3478, model.StringVal("missing")},
		{"decoded_error_before_syntax", `{"a":1,"bad":"\u0000\q"}`, "22P05", 3214, model.StringVal("a")},
		{"mismatched_key_suffix", `{"a":1,"different\u0000":2}`, "22P05", 3478, model.StringVal("a")},
		{"wrong_shape", `[1,"\ud800"]`, "22P02", 3214, model.StringVal("a")},
		{"negative_index_validation_order", `["\u0000",]`, "22P05", 3215, model.Int64Val(-1)},
		{"after_selected_element", `[1,"\u0000"]`, "22P05", 3216, model.Int64Val(0)},
		{"minimum_index_still_validates", `[1e131072]`, "22003", 3215, model.Int64Val(math.MinInt32)},
		{"wrong_shape_still_validates", `{"a":"\udc00"}`, "22P02", 3216, model.Int64Val(0)},
		{"invalid_utf8", "[1,\"\xff\"]", "22021", 3215, model.Int64Val(0)},
		{"trailing_input", `{"a":1} []`, "22P02", 3478, model.StringVal("a")},
		{"stored_nul_is_not_cstring", "[1]\x00", "22P02", 3216, model.Int64Val(0)},
		{"nul_key_still_validates", `{"a":"\u0000"}`, "22P05", 3214, model.StringVal("a\x00")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestJSONBExtractionAdmitsExactScalarOutputOverlap(t *testing.T) {
	large := strings.Repeat("x", 1<<18)
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		path              model.Value
		budget            int64
	}{
		{"overwritten_large_container", `{"a":["` + large + `"],"a":1}`, "1", 3478, model.StringVal("a"), 1},
		{"negative_index_large_sibling", `["` + large + `",true]`, "true", 3215, model.Int64Val(-1), 4},
		{"expanded_number", `{"a":1e1000}`, "1" + strings.Repeat("0", 1000), 3214, model.StringVal("a"), 1001},
		{"decoded_output_not_lexical_size", `["\u00e9"]`, "é", 3216, model.Int64Val(0), 2},
		{"quoted_control_overlap", `{"a":"\u0001"}`, `"\u0001"`, 3478, model.StringVal("a"), 9},
		{"quoted_utf8_overlap", `["\u00e9"]`, `"é"`, 3215, model.Int64Val(0), 6},
		{"empty_quoted_string", `[""]`, `""`, 3215, model.Int64Val(0), 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := jsonCallable(t, tc.id)
			args := []model.Value{jsonDatum(tc.input), tc.path}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: tc.budget, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("exact budget extraction = %v, %v; want %q", value, err, tc.want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: tc.budget - 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
	value, err := jsonCallable(t, 3214).Call(t.Context(), []model.Value{jsonDatum(`{"a":""}`), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IsNull || value.StrVal != "" {
		t.Fatalf("empty decoded output at zero budget = %v, %v; want empty text", value, err)
	}
}

func TestJSONBExtractionAdmitsOnlySelectedContainerState(t *testing.T) {
	input := `{"a":{"x":1,"x":2},"other":"` + strings.Repeat("x", 1<<18) + `"}`
	const want = `{"x": 2}`
	for _, id := range []model.FunctionID{3478, 3214} {
		t.Run(jsonCallable(t, id).Signature().Name, func(t *testing.T) {
			function := jsonCallable(t, id)
			args := []model.Value{jsonDatum(input), model.StringVal("a")}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != want {
				t.Fatalf("selected container budget = %v, %v; want %q", value, err, want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestJSONBExtractionResultsDoNotRetainInputStorage(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		path              model.Value
	}{
		{"container", `{"a":[1,2],"other":"` + strings.Repeat("x", 1<<16) + `"}`, `[1, 2]`, 3478, model.StringVal("a")},
		{"decoded_field", `{"a":"plain"}`, "plain", 3214, model.StringVal("a")},
		{"number", `[1e2]`, "100", 3215, model.Int64Val(0)},
		{"literal", `[true]`, "true", 3216, model.Int64Val(0)},
		{"quoted_string", `["plain"]`, `"plain"`, 3215, model.Int64Val(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: 1 << 16, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("extraction = %v, %v; want %q", value, err, tc.want)
			}
			start := uintptr(unsafe.Pointer(unsafe.StringData(tc.input)))
			result := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
			if result >= start && result < start+uintptr(len(tc.input)) {
				t.Fatal("result retains borrowed document storage")
			}
		})
	}
}

func TestJSONBExtractionBoundsUnselectedNesting(t *testing.T) {
	function := jsonCallable(t, 3478)
	nested := strings.Repeat("[", 6399) + "0" + strings.Repeat("]", 6399)
	value, err := function.Call(t.Context(), []model.Value{jsonDatum(`{"a":` + nested + `}`), model.StringVal("missing")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull {
		t.Fatalf("nesting at limit = %v, %v; want NULL", value, err)
	}
	_, err = function.Call(t.Context(), []model.Value{jsonDatum(`{"a":[` + nested + `]}`), model.StringVal("missing")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54001")
}

func TestJSONBExtractionPreservesCancellation(t *testing.T) {
	large := strings.Repeat("x", 1<<20)
	for _, tc := range []struct {
		name, input string
		id          model.FunctionID
		path        model.Value
		remaining   int
		budget      int64
	}{
		{"unselected_value", `{"a":1,"other":"` + large + `"}`, 3478, model.StringVal("a"), 12, 1},
		{"mismatched_large_key", `{"` + large + `":1}`, 3214, model.StringVal("a"), 12, 0},
		{"negative_index_scan", `["` + large + `",1]`, 3215, model.Int64Val(-1), 12, 1},
		{"numeric_output_expansion", `[1e131071]`, 3216, model.Int64Val(0), 24, 131072},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: tc.remaining}
			value, err := jsonCallable(t, tc.id).Call(ctx, []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: tc.budget, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) || value.StrVal != "" {
				t.Fatalf("cancelled extraction = %v, %v; want cancellation without output", value, err)
			}
		})
	}
}
