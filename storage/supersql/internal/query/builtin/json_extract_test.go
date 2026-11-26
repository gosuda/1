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

func TestJSONFieldExtractionUsesLastDecodedKey(t *testing.T) {
	const input = ` { "a": 0, "nested": {"a": 9}, "\u0061" : [ 1e+02 , {"b":"\\n"} ] } `
	const want = `[ 1e+02 , {"b":"\\n"} ]`
	for _, id := range []model.FunctionID{3947, 3948} {
		t.Run(jsonCallable(t, id).Signature().Name, func(t *testing.T) {
			value, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(input), model.StringVal("a")}, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != want {
				t.Fatalf("last decoded key = %v, %v; want %q", value, err, want)
			}
		})
	}
}

func TestJSONFieldExtractionMatchesCStringArgument(t *testing.T) {
	value, err := jsonCallable(t, 3948).Call(t.Context(), []model.Value{jsonDatum(`{"a":1,"a longer":2}`), model.StringVal("a\x00ignored")}, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
	if err != nil || value.IsNull || value.StrVal != "1" {
		t.Fatalf("field argument with NUL = %v, %v; want 1", value, err)
	}
}

func TestJSONExtractionPreservesLexemesOrDecodesText(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.FunctionID
		input string
		path  model.Value
		want  string
	}{
		{"field_raw_string", 3947, `{"a":"\u00e9\n\ud83d\ude00"}`, model.StringVal("a"), `"\u00e9\n\ud83d\ude00"`},
		{"field_decoded_string", 3948, `{"a":"\u00e9\n\ud83d\ude00"}`, model.StringVal("a"), "é\n😀"},
		{"field_null_overrides_value", 3947, `{"a":1,"a":null}`, model.StringVal("a"), "null"},
		{"first_array_element", 3949, ` [ 1e100000, false ] `, model.Int64Val(0), "1e100000"},
		{"negative_array_index", 3949, `[0, { "x" : [true, null] }, 2]`, model.Int64Val(-2), `{ "x" : [true, null] }`},
		{"negative_first_element", 3950, `[ true , 2, 3 ]`, model.Int64Val(-3), "true"},
		{"array_raw_string", 3949, `[0,"\u00e9\n\ud83d\ude00"]`, model.Int64Val(1), `"\u00e9\n\ud83d\ude00"`},
		{"array_decoded_string", 3950, `[0,"\u00e9\n\ud83d\ude00"]`, model.Int64Val(-1), "é\n😀"},
		{"array_text_container", 3950, `[null, [ 1.2300, {"x" : "\u0061"} ]]`, model.Int64Val(1), `[ 1.2300, {"x" : "\u0061"} ]`},
		{"array_raw_null", 3949, `[null]`, model.Int64Val(0), "null"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			wantType := model.TypeJSONB
			if tc.id == 3948 || tc.id == 3950 {
				wantType = model.TypeString
			}
			if err != nil || value.IsNull || value.Type != wantType || value.StrVal != tc.want {
				t.Fatalf("extraction = %v, %v; want type %v, %q", value, err, wantType, tc.want)
			}
		})
	}
}

func TestJSONExtractionNullPathsNeedNoOutputBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.FunctionID
		input model.Value
		path  model.Value
	}{
		{"missing_field", 3947, jsonDatum(`{"present":"value"}`), model.StringVal("absent")},
		{"nested_field_is_not_root_field", 3948, jsonDatum(`{"nested":{"a":1}}`), model.StringVal("a")},
		{"text_key_does_not_index_array", 3947, jsonDatum(`[1]`), model.StringVal("0")},
		{"integer_does_not_match_object_key", 3949, jsonDatum(`{"0":1}`), model.Int64Val(0)},
		{"field_on_scalar", 3948, jsonDatum(`"text"`), model.StringVal("text")},
		{"element_on_scalar", 3950, jsonDatum(`true`), model.Int64Val(0)},
		{"empty_array", 3949, jsonDatum(`[]`), model.Int64Val(-1)},
		{"positive_out_of_range", 3949, jsonDatum(`[1,2]`), model.Int64Val(2)},
		{"negative_out_of_range", 3950, jsonDatum(`[1,2]`), model.Int64Val(-3)},
		{"minimum_index", 3949, jsonDatum(`[1]`), model.Int64Val(math.MinInt32)},
		{"maximum_index", 3950, jsonDatum(`[1]`), model.Int64Val(math.MaxInt32)},
		{"field_json_null", 3948, jsonDatum(`{"a":1,"a":null}`), model.StringVal("a")},
		{"element_json_null", 3950, jsonDatum(`[1,null]`), model.Int64Val(-1)},
		{"null_document", 3947, model.NullVal(model.TypeJSONB), model.StringVal("a")},
		{"null_field", 3948, jsonDatum(`{"a":"\u0000"}`), model.NullVal(model.TypeString)},
		{"null_index", 3949, jsonDatum(`["\u0000"]`), model.NullVal(model.TypeInt64)},
		{"null_document_text_result", 3950, model.NullVal(model.TypeJSONB), model.Int64Val(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{tc.input, tc.path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			wantType := model.TypeJSONB
			if tc.id == 3948 || tc.id == 3950 {
				wantType = model.TypeString
			}
			if err != nil || !value.IsNull || value.Type != wantType {
				t.Fatalf("NULL extraction = %v, %v; want typed NULL %v", value, err, wantType)
			}
		})
	}
}

func TestJSONFieldExtractionValidatesUnselectedDecodedStrings(t *testing.T) {
	for _, tc := range []struct{ name, input, code string }{
		{"value_before_match", `{"bad":"\u0000","a":1}`, "22P05"},
		{"value_after_match", `{"a":1,"bad":"\u0000"}`, "22P05"},
		{"overwritten_value", `{"a":"\ud800","a":1}`, "22P02"},
		{"mismatched_key_suffix", `{"a":1,"different\u0000":2}`, "22P05"},
		{"nested_key", `{"a":1,"nested":{"\udc00":2}}`, "22P02"},
		{"nested_array", `{"a":1,"nested":[["\u0000"]]}`, "22P05"},
		{"wrong_shape", `[1,"\u0000"]`, "22P05"},
		{"scalar", `"\ud800"`, "22P02"},
		{"invalid_utf8", "{\"a\":1,\"bad\":\"\xff\"}", "22021"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []model.FunctionID{3947, 3948} {
				_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(tc.input), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, tc.code)
			}
		})
	}
}

func TestJSONArrayExtractionValidatesUnselectedDecodedStrings(t *testing.T) {
	for _, tc := range []struct {
		name, input, code string
		index             int64
	}{
		{"after_match", `[1,"\u0000"]`, "22P05", 0},
		{"before_negative_match", `["\ud800",1]`, "22P02", -1},
		{"out_of_range", `[1,"\u0000"]`, "22P05", 10},
		{"negative_out_of_range", `[1,"\u0000"]`, "22P05", -10},
		{"minimum_index_still_validates", `["\u0000"]`, "22P05", math.MinInt32},
		{"wrong_shape", `{"a":"\udc00"}`, "22P02", 0},
		{"nested_key", `[1,{"different\u0000":2}]`, "22P05", 0},
		{"invalid_utf8", "[1,\"\xff\"]", "22021", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []model.FunctionID{3949, 3950} {
				_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(tc.input), model.Int64Val(tc.index)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, tc.code)
			}
		})
	}
}

func TestJSONExtractionPreservesDecodedErrorOrder(t *testing.T) {
	for _, tc := range []struct{ name, input, code string }{
		{"nul_before_surrogate", `["\u0000","\ud800"]`, "22P05"},
		{"surrogate_before_nul", `["\ud800","\u0000"]`, "22P02"},
		{"same_string_nul_before_surrogate", `["\u0000\ud800"]`, "22P05"},
		{"same_string_surrogate_before_nul", `["\ud800\u0000"]`, "22P02"},
		{"low_surrogate_before_nul", `["\udc00\u0000"]`, "22P02"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []model.FunctionID{3947, 3948, 3949, 3950} {
				path := model.StringVal("absent")
				if id == 3949 || id == 3950 {
					path = model.Int64Val(-1)
				}
				_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(tc.input), path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, tc.code)
			}
		})
	}
}

func TestJSONExtractionLexesBeforeReportingGrammarErrors(t *testing.T) {
	for _, tc := range []struct {
		name, input, code string
		id                model.FunctionID
		path              model.Value
	}{
		{"missing_comma", `{"a":1 "\u0000"}`, "22P05", 3947, model.StringVal("a")},
		{"missing_colon", `{"a" "\u0000":1}`, "22P05", 3948, model.StringVal("a")},
		{"primitive_followed_by_string", `1"\u0000"`, "22P05", 3947, model.StringVal("a")},
		{"nul_before_bad_escape", `["\u0000\q"]`, "22P05", 3949, model.Int64Val(0)},
		{"bad_escape_before_nul", `["\q\u0000"]`, "22P02", 3950, model.Int64Val(0)},
		{"nul_before_unterminated_string", `["\u0000`, "22P05", 3950, model.Int64Val(0)},
		{"nul_before_invalid_utf8", "[\"\\u0000\xff\"]", "22P05", 3949, model.Int64Val(0)},
		{"invalid_utf8_before_nul", "[\"\xff\\u0000\"]", "22021", 3949, model.Int64Val(0)},
		{"positive_index_decodes_before_trailing_comma", `["\u0000",]`, "22P05", 3949, model.Int64Val(0)},
		{"negative_index_counts_before_decoding", `["\u0000",]`, "22P02", 3949, model.Int64Val(-1)},
		{"negative_count_lexes_following_token", `["\u0000"] "\q"`, "22P02", 3950, model.Int64Val(-1)},
		{"negative_count_does_not_decode_following_token", `["\u0000"] "\ud800"`, "22P05", 3950, model.Int64Val(-1)},
		{"negative_count_stops_after_one_lookahead", `["\u0000"] true false`, "22P05", 3950, model.Int64Val(-1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestJSONExtractionAdmitsOnlyFinalOwnedOutput(t *testing.T) {
	large := strings.Repeat("x", 1<<18)
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		path              model.Value
	}{
		{"overwritten_large_match", `{"a":"` + large + `","a":1}`, "1", 3947, model.StringVal("a")},
		{"unselected_large_key", `{"` + large + `":0,"a":"\u00e9"}`, "é", 3948, model.StringVal("a")},
		{"negative_index_large_sibling", `["` + large + `",true]`, "true", 3949, model.Int64Val(-1)},
		{"decoded_output_not_lexical_size", `["\u00e9\ud83d\ude00"]`, "é😀", 3950, model.Int64Val(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := jsonCallable(t, tc.id)
			args := []model.Value{jsonDatum(tc.input), tc.path}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("exact budget extraction = %v, %v; want %q", value, err, tc.want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
	value, err := jsonCallable(t, 3948).Call(t.Context(), []model.Value{jsonDatum(`{"a":"` + large + `","a":null}`), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull {
		t.Fatalf("overwritten output at zero budget = %v, %v; want NULL", value, err)
	}
	value, err = jsonCallable(t, 3950).Call(t.Context(), []model.Value{jsonDatum(`[""]`), model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IsNull || value.StrVal != "" {
		t.Fatalf("empty decoded string at zero budget = %v, %v; want empty text", value, err)
	}
}

func TestJSONExtractionResultsDoNotRetainInputStorage(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		path              model.Value
	}{
		{"raw_field", `{"a":[1, 2],"unused":"` + strings.Repeat("x", 1<<16) + `"}`, `[1, 2]`, 3947, model.StringVal("a")},
		{"text_field", `{"a":"plain"}`, "plain", 3948, model.StringVal("a")},
		{"raw_element", `["raw",2]`, `"raw"`, 3949, model.Int64Val(0)},
		{"decoded_element", `["\u00e9",2]`, "é", 3950, model.Int64Val(0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := jsonCallable(t, tc.id)
			value, err := function.Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("extraction = %v, %v; want %q", value, err, tc.want)
			}
			start := uintptr(unsafe.Pointer(unsafe.StringData(tc.input)))
			result := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
			if result >= start && result < start+uintptr(len(tc.input)) {
				t.Fatal("result retains borrowed document storage")
			}
			other, err := function.Call(t.Context(), []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || other.StrVal != tc.want || value.StrVal != tc.want {
				t.Fatalf("repeated extraction changed output: first=%q, next=%v, error=%v", value.StrVal, other, err)
			}
		})
	}
}

func TestJSONExtractionBoundsUnselectedNesting(t *testing.T) {
	function := jsonCallable(t, 3947)
	nested := strings.Repeat("[", 6399) + "0" + strings.Repeat("]", 6399)
	value, err := function.Call(t.Context(), []model.Value{jsonDatum(`{"a":` + nested + `}`), model.StringVal("missing")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull {
		t.Fatalf("nesting at limit = %v, %v; want NULL", value, err)
	}
	_, err = function.Call(t.Context(), []model.Value{jsonDatum(`{"a":[` + nested + `]}`), model.StringVal("missing")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54001")
}

func TestJSONExtractionPreservesTraversalCancellation(t *testing.T) {
	large := strings.Repeat("x", 1<<20)
	for _, tc := range []struct {
		name, input string
		id          model.FunctionID
		path        model.Value
	}{
		{"unselected_value_after_match", `{"a":1,"other":"` + large + `"}`, 3947, model.StringVal("a")},
		{"mismatched_large_key", `{"` + large + `":1}`, 3948, model.StringVal("a")},
		{"negative_index_count", `["` + large + `",1]`, 3949, model.Int64Val(-1)},
		{"out_of_range_scan", `[0,` + strings.Repeat(" ", 1<<20) + `1]`, 3950, model.Int64Val(10)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: 12}
			value, err := jsonCallable(t, tc.id).Call(ctx, []model.Value{jsonDatum(tc.input), tc.path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) || value.StrVal != "" {
				t.Fatalf("cancelled extraction = %v, %v; want cancellation without output", value, err)
			}
		})
	}
}
