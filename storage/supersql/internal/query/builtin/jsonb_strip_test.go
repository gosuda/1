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

func TestJSONBStripNullsNestedContainers(t *testing.T) {
	const input = `{"z":null,"a":[null,{"x":null,"keep":[null,1.2300e2]},[null],{}],"b":{"gone":null}}`
	for _, tc := range []struct {
		name   string
		arrays bool
		want   string
	}{
		{"default_false", false, `{"a": [null, {"keep": [null, 123.00]}, [null], {}], "b": {}}`},
		{"strip_arrays", true, `{"a": [{"keep": [123.00]}, [], {}], "b": {}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, 3262).Call(t.Context(), []model.Value{jsonDatum(input), model.BoolVal(tc.arrays)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeJSONB || value.StrVal != tc.want {
				t.Fatalf("strip nulls = %v, %v; want %q", value, err, tc.want)
			}
		})
	}
}

func TestJSONBStripNullsCanonicalizesBeforeFiltering(t *testing.T) {
	const input = `{"a":{"keep":1},"a":null,"b":null,"b":{"x":null},"c":1,"\u0063":null,"d":[null,"null",false,-0.00]}`
	const want = `{"b": {}, "d": ["null", false, 0.00]}`
	value, err := jsonCallable(t, 3262).Call(t.Context(), []model.Value{jsonDatum(input), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != want {
		t.Fatalf("duplicate-last stripping = %q, %v; want %q", value.StrVal, err, want)
	}
}

func TestJSONBStripNullsValidatesRemovedAndOverwrittenInput(t *testing.T) {
	for _, tc := range []struct{ input, state string }{
		{`{"a":"\u0000","a":null}`, "22P05"},
		{`{"a":1e131072,"a":null}`, "22003"},
		{`{"a":{"bad":"\ud800"},"a":null}`, "22P02"},
		{`[null,{"bad":1e-16384}]`, "22003"},
		{"[null,\"\xff\"]", "22021"},
		{`[null,]`, "22P02"},
	} {
		_, err := jsonCallable(t, 3262).Call(t.Context(), []model.Value{jsonDatum(tc.input), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, tc.state)
	}
}

func TestJSONBStripNullsPreservesOwnedRootScalars(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`null`, `null`}, {`false`, `false`}, {`"null"`, `"null"`}, {`1.2300e2`, `123.00`}, {`{}`, `{}`}, {`[]`, `[]`},
	} {
		for _, arrays := range []bool{false, true} {
			value, err := jsonCallable(t, 3262).Call(t.Context(), []model.Value{jsonDatum(tc.input), model.BoolVal(arrays)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("strip root %s = %v, %v; want %s", tc.input, value, err, tc.want)
			}
			start := uintptr(unsafe.Pointer(unsafe.StringData(tc.input)))
			result := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
			if result >= start && result < start+uintptr(len(tc.input)) {
				t.Fatal("strip result retains borrowed document storage")
			}
		}
	}
}

func TestJSONBStripNullsRetainsOriginalStateAtExactPeak(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		budget      int64
	}{
		{`null`, `null`, 76},
		{`{"a":null}`, `{}`, 179},
		{`[null,null,null]`, `[]`, 322},
		{`{"a":1e1000,"a":null}`, `{}`, 252},
	} {
		args := []model.Value{jsonDatum(tc.input), model.BoolVal(true)}
		function := jsonCallable(t, 3262)
		value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: tc.budget, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != tc.want {
			t.Fatalf("exact strip admission %s = %v, %v; want %s", tc.input, value, err, tc.want)
		}
		_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: tc.budget - 1, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54000")
	}
}

func TestJSONBStripNullsStrictNullSkipsInvalidDocument(t *testing.T) {
	for _, args := range [][]model.Value{
		{model.NullVal(model.TypeJSONB), model.BoolVal(true)},
		{jsonDatum(`{"bad":"\u0000"}`), model.NullVal(model.TypeBoolean)},
	} {
		value, err := jsonCallable(t, 3262).Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull || value.Type != model.TypeJSONB {
			t.Fatalf("strict strip NULL = %v, %v", value, err)
		}
	}
}

func TestJSONBStripNullsCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: 4}
	value, err := jsonCallable(t, 3262).Call(ctx, []model.Value{jsonDatum("[" + strings.Repeat("null,", 10000) + "null]"), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 1 << 24, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) || value.StrVal != "" {
		t.Fatalf("cancelled strip = %v, %v; want cancellation without output", value, err)
	}
}

func TestJSONBStripNullsDeepNestingPreservesEmptyContainers(t *testing.T) {
	const depth = 6400
	input := strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth)
	want := strings.Repeat("[", depth) + strings.Repeat("]", depth)
	value, err := jsonCallable(t, 3262).Call(t.Context(), []model.Value{jsonDatum(input), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != want {
		t.Fatalf("deep strip length=%d, error=%v; want length=%d", len(value.StrVal), err, len(want))
	}
}
