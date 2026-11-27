package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestJSONBCanonicalizesDecodedKeysAndNestedValues(t *testing.T) {
	const input = ` {"é":1,"aa":2,"z":3,"😀":4,"bbb":5,"\u0061":6,"a":{"z":0,"b":[-0.00,1.2300e2,"\u0061\/\b\f\n\r\t\u001f\ud83d\ude00"]}} `
	const want = `{"a": {"b": [0.00, 123.00, "a/\b\f\n\r\t\u001f😀"], "z": 0}, "z": 3, "aa": 2, "é": 1, "bbb": 5, "😀": 4}`
	value, err := jsonCallable(t, 3806).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || value.Type != model.TypeJSONB || value.StrVal != want {
		t.Fatalf("jsonb input = %q, %v; want %q", value.StrVal, err, want)
	}
	output, err := jsonCallable(t, 3804).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || output.StrVal != want {
		t.Fatalf("jsonb output = %q, %v; want %q", output.StrVal, err, want)
	}
}

func TestJSONBLastDuplicateCanReplaceContainerWithNull(t *testing.T) {
	const input = `{"a":{"nested":1},"b":[1,2],"\u0061":null,"b":{"x":1,"x":2}}`
	const want = `{"a": null, "b": {"x": 2}}`
	value, err := jsonCallable(t, 3806).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != want {
		t.Fatalf("duplicate JSONB keys = %q, %v; want %q", value.StrVal, err, want)
	}
}

func TestJSONBValidatesOverwrittenValues(t *testing.T) {
	for _, tc := range []struct {
		name, input, state string
	}{
		{"nul", `{"a":"\u0000","a":1}`, "22P05"},
		{"surrogate", `{"a":"\ud800","a":1}`, "22P02"},
		{"numeric", `{"a":1e131072,"a":1}`, "22003"},
		{"nested", `{"a":{"bad":"\u0000"},"a":null}`, "22P05"},
		{"invalid UTF8", "{\"a\":\"\xff\",\"a\":1}", "22021"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := jsonCallable(t, 3806).Call(t.Context(), []model.Value{model.StringVal(tc.input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.state)
		})
	}
}

func TestJSONBDecodedErrorsPrecedeLaterGrammarErrors(t *testing.T) {
	for _, input := range []string{`"\u0000\q"`, `"\u0000`, `{"\u0000\q":1}`, `{"bad":"\u0000","later":}`, `{"a" "\u0000"}`, `1 "\u0000"`} {
		_, err := jsonCallable(t, 3806).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22P05")
	}
}

func TestJSONBNumericPreservesScaleAndFullPostgresRange(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
	}{
		{"negative zero", "-0.00e-2", "0.0000"},
		{"decimal exponent", "123.4500e-2", "1.234500"},
		{"leading fractional zeroes", "0.00012300e2", "0.012300"},
		{"integer upper boundary", "1e131071", "1" + strings.Repeat("0", 131071)},
		{"fraction upper boundary", "1e-16383", "0." + strings.Repeat("0", 16382) + "1"},
		{"full coefficient", strings.Repeat("1", 131072), strings.Repeat("1", 131072)},
		{"long mantissa reduces", "0." + strings.Repeat("0", 32768) + "1e32769", "1"},
		{"zero exponent upper boundary", "0e1073741823", "0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, 3806).Call(t.Context(), []model.Value{model.StringVal(tc.input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != tc.want {
				t.Fatalf("canonical numeric length=%d, error=%v; want length=%d (equal=%v)", len(value.StrVal), err, len(tc.want), value.StrVal == tc.want)
			}
		})
	}
}

func TestJSONBNumericRejectsStorageOverflow(t *testing.T) {
	for _, input := range []string{"1e131072", "1e-16384", "0e-16384", "0e1073741824", strings.Repeat("1", 131073)} {
		_, err := jsonCallable(t, 3806).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22003")
	}
}

func TestJSONBCodecsValidateRawStoredRepresentation(t *testing.T) {
	const input = ` {"b":1e2,"a":1,"b":2} `
	const want = `{"a": 1, "b": 2}`
	value, err := jsonCallable(t, 3804).Call(t.Context(), []model.Value{jsonDatum(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != want {
		t.Fatalf("raw JSONB output = %q, %v; want %q", value.StrVal, err, want)
	}
	first, err := jsonCallable(t, 3803).Call(t.Context(), []model.Value{jsonDatum(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || string(first.BytesVal) != "\x01"+want {
		t.Fatalf("jsonb send = %q, %v", first.BytesVal, err)
	}
	second, err := jsonCallable(t, 3803).Call(t.Context(), []model.Value{jsonDatum(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	first.BytesVal[1] = '!'
	if string(second.BytesVal) != "\x01"+want || value.StrVal != want {
		t.Fatal("mutating one wire payload changed another owned result")
	}
	for _, id := range []model.FunctionID{3804, 3803, 3210, 3207} {
		_, err := jsonCallable(t, id).Call(t.Context(), []model.Value{jsonDatum(`["\u0000"]`)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22P05")
	}
}

func TestJSONBMetadataDistinguishesScalarAndContainerRoots(t *testing.T) {
	for _, tc := range []struct{ input, kind string }{
		{`null`, "null"}, {`false`, "boolean"}, {`"null"`, "string"}, {`1e131071`, "number"}, {`{}`, "object"}, {`[]`, "array"},
	} {
		value, err := jsonCallable(t, 3210).Call(t.Context(), []model.Value{jsonDatum(tc.input)}, builtin.CallBudget{OutputBytes: int64(len(tc.kind)), MemoryBytes: 64 << 20})
		if err != nil || value.IsNull || value.StrVal != tc.kind {
			t.Fatalf("jsonb_typeof(%s) = %v, %v; want %s", tc.input, value, err, tc.kind)
		}
	}
	value, err := jsonCallable(t, 3207).Call(t.Context(), []model.Value{jsonDatum(`[1,[2,3],{"x":[4,5]},null]`)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IntVal != 4 {
		t.Fatalf("jsonb array length = %v, %v; want 4", value, err)
	}
	for _, input := range []string{`{}`, `null`, `"array"`, `0`} {
		_, err := jsonCallable(t, 3207).Call(t.Context(), []model.Value{jsonDatum(input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22023")
	}
}

func TestJSONBInputUsesCStringTerminator(t *testing.T) {
	value, err := jsonCallable(t, 3806).Call(t.Context(), []model.Value{model.StringVal(" [1] \x00\xff not JSON")}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "[1]" {
		t.Fatalf("NUL-terminated jsonb input = %q, %v; want [1]", value.StrVal, err)
	}
}

func TestJSONBRejectsMalformedContainers(t *testing.T) {
	for _, input := range []string{"", `[1,]`, `{"a":}`, `{"a":1,}`, `{a:1}`, `truefalse`, `01`, `1.`, `1e+`, `NaN`, `[}`, `{} []`} {
		_, err := jsonCallable(t, 3806).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22P02")
	}
}

func TestJSONBTemporaryAndExpandedOutputBudgets(t *testing.T) {
	function := jsonCallable(t, 3806)
	for _, tc := range []struct {
		input  string
		budget int64
	}{
		{`[1,2,3,4,5]`, 16},
		{`1e1000`, 1000},
		{`"\u0001"`, 7},
		{`{"a":1,"a":2}`, 13},
	} {
		_, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input)}, builtin.CallBudget{OutputBytes: tc.budget, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54000")
	}
	const input = ` {"a":1,"a":2} `
	value, err := function.Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != `{"a": 2}` {
		t.Fatalf("admitted canonical JSONB = %q, %v", value.StrVal, err)
	}
	_, err = jsonCallable(t, 3803).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: int64(len(value.StrVal)), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestJSONBStrictNullNeedsNoOutputBudget(t *testing.T) {
	for _, id := range []model.FunctionID{3806, 3804, 3803, 3210, 3207} {
		input := model.NullVal(model.TypeJSONB)
		if id == 3806 {
			input = model.NullVal(model.TypeString)
		}
		value, err := jsonCallable(t, id).Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull {
			t.Fatalf("strict NULL for %d = %v, %v", id, value, err)
		}
	}
}

func TestJSONBNestingHasExplicitBound(t *testing.T) {
	function := jsonCallable(t, 3806)
	input := strings.Repeat("[", 6400) + "0" + strings.Repeat("]", 6400)
	value, err := function.Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != input {
		t.Fatalf("bounded nested JSONB length=%d, error=%v; want %d", len(value.StrVal), err, len(input))
	}
	_, err = function.Call(t.Context(), []model.Value{model.StringVal("[" + input + "]")}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54001")
}

func TestJSONBPreservesCancellationAcrossLargeInputs(t *testing.T) {
	for _, id := range []model.FunctionID{3806, 3804, 3803, 3210, 3207} {
		parent, cancel := context.WithCancel(t.Context())
		ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: 4}
		input := jsonDatum("[\"" + strings.Repeat("a", 1<<20) + "\"]")
		if id == 3806 {
			input.Type = model.TypeString
		}
		_, err := jsonCallable(t, id).Call(ctx, []model.Value{input}, builtin.CallBudget{OutputBytes: 1 << 24, MemoryBytes: 64 << 20})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled JSONB %d = %v", id, err)
		}
	}
}
