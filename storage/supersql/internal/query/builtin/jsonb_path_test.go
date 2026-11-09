package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestJSONBPathFlattensDimensionsInRowMajorOrder(t *testing.T) {
	path := jsonPathArray("a", "-1", "b", "-1")
	path.Dimensions = []model.ArrayDimension{{Length: 2, LowerBound: -3}, {Length: 2, LowerBound: 7}}
	const input = `{"a":[{"b":[0]},{"b":[1,{"zz":1e2,"x":"\u0061"}]}]}`
	const want = `{"x": "a", "zz": 100}`
	for _, id := range []model.FunctionID{3217, 3940} {
		t.Run(jsonCallable(t, id).Signature().Name, func(t *testing.T) {
			value, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(input), path}, builtin.CallBudget{OutputBytes: 1 << 16, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != want {
				t.Fatalf("multidimensional path = %v, %v; want %q", value, err, want)
			}
		})
	}
}

func TestJSONBPathDiscardsEarlierDuplicatePrefixes(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		path              []string
		null              bool
	}{
		{"missing_last_suffix", `{"a":{"b":1},"a":{}}`, "", []string{"a", "b"}, true},
		{"scalar_last_prefix", `{"a":{"b":1},"a":2}`, "", []string{"a", "b"}, true},
		{"decoded_duplicate_prefix", `{"a":{"b":1},"\u0061":{"b":2}}`, "2", []string{"a", "b"}, false},
		{"nested_duplicate_prefix", `{"a":{"b":{"c":1},"b":{}}}`, "", []string{"a", "b", "c"}, true},
		{"duplicate_array_uses_own_length", `{"a":[1],"a":[2,3]}`, "3", []string{"a", "-1"}, false},
		{"unselected_array_cannot_change_index", `{"z":[8],"a":[2,3]}`, "3", []string{"a", "-1"}, false},
		{"nested_arrays_use_independent_lengths", `[[8],[1,2,3]]`, "3", []string{"-1", "-1"}, false},
		{"last_leaf_is_json_null", `{"a":{"b":1,"b":null}}`, "null", []string{"a", "b"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, 3217).Call(t.Context(), []model.Value{jsonDatum(tc.input), jsonPathArray(tc.path...)}, builtin.CallBudget{OutputBytes: 16, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull != tc.null || value.StrVal != tc.want {
				t.Fatalf("path %v = %v, %v; want %q null=%v", tc.path, value, err, tc.want, tc.null)
			}
		})
	}
}

func TestJSONBPathParsesArraySubscriptsAsDecimalInt4(t *testing.T) {
	for _, tc := range []struct {
		name, index, want string
		null              bool
	}{
		{"leading_c_whitespace_and_sign", "\v\f\t\r\n +01", "20", false},
		{"leading_zero_is_decimal", "010", "110", false},
		{"negative_zero", "-0", "10", false},
		{"negative_length", "-11", "10", false},
		{"nul_terminates_array_index", "1\x00ignored", "20", false},
		{"positive_out_of_range", "11", "", true},
		{"negative_out_of_range", "-12", "", true},
		{"maximum_int4", "2147483647", "", true},
		{"minimum_int4", "-2147483648", "", true},
		{"positive_int4_overflow", "2147483648", "", true},
		{"negative_int4_overflow", "-2147483649", "", true},
		{"trailing_whitespace", "1 ", "", true},
		{"decimal_fraction", "1.0", "", true},
		{"hex_is_not_decimal", "0x1", "", true},
		{"empty_index", "", "", true},
		{"sign_without_digits", "+", "", true},
		{"non_ascii_whitespace", "\u00a01", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, 3940).Call(t.Context(), []model.Value{jsonDatum(`[10,20,30,40,50,60,70,80,90,100,110]`), jsonPathArray(tc.index)}, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull != tc.null || value.StrVal != tc.want {
				t.Fatalf("array index %q = %v, %v; want %q null=%v", tc.index, value, err, tc.want, tc.null)
			}
		})
	}
}

func TestJSONBPathMatchesCompleteObjectKeyBytes(t *testing.T) {
	const input = `{"a":1,"":2,"2147483648":3,"-1":4,"\u00e9":5}`
	for _, tc := range []struct {
		name, key, want string
		null            bool
	}{
		{"nul_does_not_terminate_key", "a\x00ignored", "", true},
		{"nul_is_not_empty_key", "\x00", "", true},
		{"empty_key", "", "2", false},
		{"numeric_key_is_not_parsed", "2147483648", "3", false},
		{"negative_key_is_not_an_index", "-1", "4", false},
		{"decoded_utf8_key", "é", "5", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, 3940).Call(t.Context(), []model.Value{jsonDatum(input), jsonPathArray(tc.key)}, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull != tc.null || value.StrVal != tc.want {
				t.Fatalf("object key %q = %v, %v; want %q null=%v", tc.key, value, err, tc.want, tc.null)
			}
		})
	}
}

func TestJSONBPathNonmatchingPathsNeedNoOutputBudget(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		path        []string
	}{
		{"scalar_zero_is_not_array", `1`, []string{"0"}},
		{"scalar_minus_one_is_not_array", `"a"`, []string{"-1"}},
		{"null_scalar_is_not_array", `null`, []string{"0"}},
		{"nested_scalar_stops_path", `{"a":true}`, []string{"a", "0"}},
		{"missing_field", `{"a":1}`, []string{"absent"}},
		{"array_does_not_have_fields", `[1]`, []string{"a"}},
		{"empty_array", `[]`, []string{"-1"}},
	} {
		for _, id := range []model.FunctionID{3217, 3940} {
			t.Run(tc.name+"/"+jsonCallable(t, id).Signature().Name, func(t *testing.T) {
				value, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(tc.input), jsonPathArray(tc.path...)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				wantType := model.TypeJSONB
				if id == 3940 {
					wantType = model.TypeString
				}
				if err != nil || !value.IsNull || value.Type != wantType {
					t.Fatalf("nonmatching path = %v, %v; want typed NULL %v", value, err, wantType)
				}
			})
		}
	}
}

func TestJSONBPathNullArgumentsBypassRawDocumentValidation(t *testing.T) {
	nullElement := jsonPathArray("a", "b")
	nullElement.ArrayVal[1] = model.NullVal(model.TypeString)
	for _, tc := range []struct {
		name           string
		document, path model.Value
	}{
		{"null_document", model.NullVal(model.TypeJSONB), jsonPathArray("a")},
		{"null_array", jsonDatum(`{"a":"\u0000"}`), model.NullVal(model.TypeArray)},
		{"null_element", jsonDatum(`{"a":1e131072}`), nullElement},
	} {
		for _, id := range []model.FunctionID{3217, 3940} {
			t.Run(tc.name+"/"+jsonCallable(t, id).Signature().Name, func(t *testing.T) {
				value, err := jsonCallable(t, id).Call(t.Context(), []model.Value{tc.document, tc.path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull {
					t.Fatalf("NULL argument = %v, %v; want SQL NULL", value, err)
				}
			})
		}
	}
}

func TestJSONBEmptyPathCanonicalizesOwnedResult(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		null              bool
	}{
		{"whole_object", ` {"zz":1e2,"b":[-0.00,"\u0061"],"zz":2} `, `{"b": [0.00, "a"], "zz": 2}`, 3217, false},
		{"text_container", ` [ {"bb":1,"a":2}, 3 ] `, `[{"a": 2, "bb": 1}, 3]`, 3940, false},
		{"quoted_scalar", ` "\u00e9\n\ud83d\ude00\/" `, "\"é\\n\U0001f600/\"", 3217, false},
		{"text_scalar", ` "\u00e9\n\ud83d\ude00\/" `, "é\n\U0001f600/", 3940, false},
		{"number_scale", ` 1.2300e2 `, "123.00", 3217, false},
		{"json_null", ` null `, "null", 3217, false},
		{"text_null", ` null `, "", 3940, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), jsonPathArray()}, builtin.CallBudget{OutputBytes: 1 << 16, MemoryBytes: 64 << 20})
			wantType := model.TypeJSONB
			if tc.id == 3940 {
				wantType = model.TypeString
			}
			if err != nil || value.IsNull != tc.null || value.Type != wantType || value.StrVal != tc.want {
				t.Fatalf("empty path = %v, %v; want type %v, %q null=%v", value, err, wantType, tc.want, tc.null)
			}
		})
	}
	value, err := jsonCallable(t, 3940).Call(t.Context(), []model.Value{jsonDatum(`{"a":[null]}`), jsonPathArray("a", "0")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull || value.Type != model.TypeString {
		t.Fatalf("text leaf null = %v, %v; want SQL NULL text", value, err)
	}
}

func TestJSONBPathValidatesOverwrittenAndUnselectedValuesInOrder(t *testing.T) {
	for _, tc := range []struct {
		name, input, code string
		path              []string
	}{
		{"overwritten_prefix", `{"a":{"bad":"\u0000"},"a":{"b":1}}`, "22P05", []string{"a", "b"}},
		{"overwritten_number", `{"a":{"b":1e131072,"b":1}}`, "22003", []string{"a", "b"}},
		{"unselected_numeric_range", `{"a":{"b":1},"z":1e-16384}`, "22003", []string{"a", "b"}},
		{"numeric_before_decoded_error", `{"a":1e131072,"z":"\u0000"}`, "22003", []string{"missing"}},
		{"decoded_before_numeric_error", `{"a":"\u0000","z":1e131072}`, "22P05", []string{"missing"}},
		{"decoded_before_later_syntax", `{"a":1,"z":"\u0000\q"}`, "22P05", []string{"a"}},
		{"mismatched_key_suffix", `{"a":1,"other\u0000":2}`, "22P05", []string{"a"}},
		{"bad_index_still_validates", `[1e131072]`, "22003", []string{"2147483648"}},
		{"nul_key_still_validates", `{"a":"\u0000"}`, "22P05", []string{"a\x00"}},
		{"scalar_path_still_validates", `"\ud800"`, "22P02", []string{"0"}},
		{"invalid_utf8", "[1,\"\xff\"]", "22021", []string{"0"}},
		{"trailing_input", `{"a":1} []`, "22P02", []string{"a"}},
		{"empty_path_still_validates", `{"a":"\u0000"}`, "22P05", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jsonCallable(t, 3217).Call(t.Context(), []model.Value{jsonDatum(tc.input), jsonPathArray(tc.path...)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestJSONBPathAdmitsOnlyOwnedScalarOutputAndLiveDecode(t *testing.T) {
	large := strings.Repeat("x", 1<<18)
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		path              []string
		budget            int64
	}{
		{"discarded_large_prefix", `{"a":["` + large + `"],"a":{"b":1}}`, "1", 3217, []string{"a", "b"}, 1},
		{"negative_index_large_sibling", `{"a":["` + large + `",true]}`, "true", 3217, []string{"a", "-1"}, 4},
		{"empty_path_expanded_number", ` 1e1000 `, "1" + strings.Repeat("0", 1000), 3940, nil, 1001},
		{"empty_path_owned_literal", ` false `, "false", 3217, nil, 5},
		{"empty_path_quoted_utf8_overlap", ` "\u00e9" `, `"é"`, 3217, nil, 6},
		{"quoted_control_overlap", `{"a":{"b":"\u0001"}}`, `"\u0001"`, 3217, []string{"a", "b"}, 9},
		{"decoded_utf8_not_lexical_size", `{"a":"\u00e9"}`, "é", 3940, []string{"a"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := jsonCallable(t, tc.id)
			args := []model.Value{jsonDatum(tc.input), jsonPathArray(tc.path...)}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: tc.budget, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("exact budget = %v, %v; want %q", value, err, tc.want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: tc.budget - 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
	value, err := jsonCallable(t, 3940).Call(t.Context(), []model.Value{jsonDatum(`""`), jsonPathArray()}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IsNull || value.StrVal != "" {
		t.Fatalf("empty decoded text at zero budget = %v, %v", value, err)
	}
}

func TestJSONBPathAccountsContainerStateWithOutput(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		path        []string
	}{
		{"empty_path", `{"a":1,"a":2}`, nil},
		{"selected_container", `{"x":{"a":1,"a":2},"ignored":"` + strings.Repeat("x", 1<<18) + `"}`, []string{"x"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := jsonCallable(t, 3217)
			args := []model.Value{jsonDatum(tc.input), jsonPathArray(tc.path...)}
			const want = `{"a": 2}`
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != want {
				t.Fatalf("bounded container state = %v, %v; want %q", value, err, want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestJSONBPathResultsNeverRetainBorrowedDocumentStorage(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		id                model.FunctionID
		path              []string
	}{
		{"empty_path_container", `{"b":1,"a":2}`, `{"a": 2, "b": 1}`, 3217, nil},
		{"empty_path_literal", ` true `, "true", 3940, nil},
		{"empty_path_number", ` 1e2 `, "100", 3217, nil},
		{"empty_path_quoted_string", ` "plain" `, `"plain"`, 3217, nil},
		{"empty_path_decoded_string", ` "plain" `, "plain", 3940, nil},
		{"selected_container", `{"a":[1,2],"other":"` + strings.Repeat("x", 1<<16) + `"}`, `[1, 2]`, 3217, []string{"a"}},
		{"selected_decoded_string", `{"a":{"b":"plain"}}`, "plain", 3940, []string{"a", "b"}},
		{"selected_json_null", `{"a":null}`, "null", 3217, []string{"a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, tc.id).Call(t.Context(), []model.Value{jsonDatum(tc.input), jsonPathArray(tc.path...)}, builtin.CallBudget{OutputBytes: 1 << 16, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("owned path result = %v, %v; want %q", value, err, tc.want)
			}
			start := uintptr(unsafe.Pointer(unsafe.StringData(tc.input)))
			result := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
			if result >= start && result < start+uintptr(len(tc.input)) {
				t.Fatal("path result retains borrowed document storage")
			}
		})
	}
}

func TestJSONBPathPreservesCancellation(t *testing.T) {
	large := strings.Repeat("x", 1<<20)
	longPath := make([]string, 4096)
	for _, tc := range []struct {
		name, input string
		path        []string
		remaining   int
		budget      int64
	}{
		{"path_null_scan", `{"a":1}`, longPath, 12, 0},
		{"unselected_value_validation", `{"a":1,"z":"` + large + `"}`, []string{"a"}, 12, 1},
		{"overwritten_prefix_validation", `{"a":"` + large + `","a":{"b":1}}`, []string{"a", "b"}, 12, 1},
		{"large_subscript", `[1]`, []string{strings.Repeat(" ", 1<<20) + "0"}, 24, 1},
		{"empty_path_numeric_expansion", `1e131071`, nil, 24, 131072},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: tc.remaining}
			value, err := jsonCallable(t, 3940).Call(ctx, []model.Value{jsonDatum(tc.input), jsonPathArray(tc.path...)}, builtin.CallBudget{OutputBytes: tc.budget, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) || value.StrVal != "" {
				t.Fatalf("cancelled path = %v, %v; want cancellation without output", value, err)
			}
		})
	}
}

func TestJSONBPathBoundsUnselectedNestingWithoutPathAllocation(t *testing.T) {
	function := jsonCallable(t, 3217)
	nested := strings.Repeat("[", 6399) + "0" + strings.Repeat("]", 6399)
	path := jsonPathArray("missing")
	value, err := function.Call(t.Context(), []model.Value{jsonDatum(`{"a":` + nested + `}`), path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull {
		t.Fatalf("unselected nesting at limit = %v, %v; want NULL", value, err)
	}
	_, err = function.Call(t.Context(), []model.Value{jsonDatum(`{"a":[` + nested + `]}`), path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54001")
	longPath := make([]string, 6401)
	value, err = function.Call(t.Context(), []model.Value{jsonDatum(`{}`), jsonPathArray(longPath...)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull {
		t.Fatalf("long nonmatching path at zero budget = %v, %v; want NULL", value, err)
	}
}

func TestJSONBPathRejectsMalformedPhysicalArrays(t *testing.T) {
	function := jsonCallable(t, 3217)
	path := jsonPathArray("a")
	path.Dimensions[0].Length = 2
	_, err := function.Call(t.Context(), []model.Value{jsonDatum(`{"a":1}`), path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "2202E")
	path = jsonPathArray("a")
	path.ArrayVal[0] = model.Int64Val(0)
	_, err = function.Call(t.Context(), []model.Value{jsonDatum(`[1]`), path}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42804")
}
