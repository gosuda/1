package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func jsonCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}
func jsonDatum(text string) model.Value { return model.Value{Type: model.TypeJSONB, StrVal: text} }

func TestJSONTextCodecsPreserveLexicalRepresentation(t *testing.T) {
	const input = ` { "b":1e999999,"a":"\u0000","a":"\ud800" } `
	value, err := jsonCallable(t, 321).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	output, err := jsonCallable(t, 322).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if output.StrVal != input {
		t.Fatalf("JSON lexical output=%q, want %q", output.StrVal, input)
	}
	wire, err := jsonCallable(t, 324).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if string(wire.BytesVal) != input {
		t.Fatalf("JSON binary text=%q", wire.BytesVal)
	}
	_, err = jsonCallable(t, 321).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: int64(len(input) - 1), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestJSONTextInputRejectsMalformedGrammar(t *testing.T) {
	function := jsonCallable(t, 321)
	for _, input := range []string{"", `[1,]`, `{"a":}`, `{"a":1,}`, `{a:1}`, `truefalse`, `01`, `1.`, `1e+`, `NaN`, `"\u12xz"`, `"line` + "\n" + `break"`, `[}`, `{} []`} {
		_, err := function.Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22P02")
	}
}

func TestJSONTextInspectionDistinguishesNullAndNestedArrayElements(t *testing.T) {
	for _, tc := range []struct{ input, kind string }{
		{`null`, "null"}, {`false`, "boolean"}, {`"null"`, "string"}, {`1e100000`, "number"}, {`{}`, "object"}, {`[]`, "array"},
	} {
		value, err := jsonCallable(t, 3968).Call(t.Context(), []model.Value{jsonDatum(tc.input)}, builtin.CallBudget{OutputBytes: int64(len(tc.kind)), MemoryBytes: 64 << 20})
		if err != nil || value.IsNull || value.StrVal != tc.kind {
			t.Fatalf("json_typeof(%s)=%v,%v", tc.input, value, err)
		}
	}
	value, err := jsonCallable(t, 3956).Call(t.Context(), []model.Value{jsonDatum(`[1,[2,3],{"x":[4,5]},null]`)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IntVal != 4 {
		t.Fatalf("array length=%v,%v; want 4", value, err)
	}
	_, err = jsonCallable(t, 3956).Call(t.Context(), []model.Value{jsonDatum(`null`)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22023")
}

func TestJSONTextNestingHasExplicitBound(t *testing.T) {
	function := jsonCallable(t, 321)
	input := strings.Repeat("[", 6400) + "0" + strings.Repeat("]", 6400)
	if _, err := function.Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20}); err != nil {
		t.Fatal(err)
	}
	input = "[" + input + "]"
	_, err := function.Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54001")
}

func TestJSONArrayInspectionPreservesCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: 4}
	_, err := jsonCallable(t, 3956).Call(ctx, []model.Value{jsonDatum("[" + strings.Repeat(" ", 1<<20) + "1]")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled JSON inspection=%v", err)
	}
}
