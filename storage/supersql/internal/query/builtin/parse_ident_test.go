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

func identifierCallable(t *testing.T) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(1268, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func TestParseIdentifierPreservesQuotedNamesAndUntruncatedUnicode(t *testing.T) {
	input := `  Foo . "B""C" . ÄBC . ` + strings.Repeat("LONG", 32)
	result, err := identifierCallable(t).Call(t.Context(), []model.Value{model.StringVal(input), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"foo", `B"C`, "Äbc", strings.Repeat("long", 32)}
	if len(result.ArrayVal) != len(want) {
		t.Fatalf("identifiers=%v", result.ArrayVal)
	}
	for i, name := range want {
		if result.ArrayVal[i].StrVal != name {
			t.Fatalf("identifier %d=%q, want %q", i, result.ArrayVal[i].StrVal, name)
		}
	}
	if len(result.Dimensions) != 1 || result.Dimensions[0].LowerBound != 1 || result.Dimensions[0].Length != 4 {
		t.Fatalf("identifier array bounds=%v", result.Dimensions)
	}
}

func TestParseIdentifierNonStrictStopsOnlyAfterCompletePart(t *testing.T) {
	function := identifierCallable(t)
	result, err := function.Call(t.Context(), []model.Value{model.StringVal(`Schema.Table(text) trailing`), model.BoolVal(false)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ArrayVal) != 2 || result.ArrayVal[0].StrVal != "schema" || result.ArrayVal[1].StrVal != "table" {
		t.Fatalf("parsed prefix=%v", result.ArrayVal)
	}
	for _, input := range []string{`Schema.`, `.Table`, `Schema..Table`, `""`, `"unclosed`} {
		_, err := function.Call(t.Context(), []model.Value{model.StringVal(input), model.BoolVal(false)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22023")
	}
	_, err = function.Call(t.Context(), []model.Value{model.StringVal(`Schema.Table(text)`), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22023")
}

func TestParseIdentifierAdmitsExactOwnedResult(t *testing.T) {
	function := identifierCallable(t)
	input := strings.Clone(`A."b""c"`)
	arguments := []model.Value{model.StringVal(input), model.BoolVal(true)}
	budget := 2*int64(unsafe.Sizeof(model.Value{})) + int64(unsafe.Sizeof(model.ArrayDimension{})) + 4
	_, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: budget - 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	arguments[0] = model.StringVal("changed")
	if _, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20}); err != nil {
		t.Fatal(err)
	}
	if len(value.ArrayVal) != 2 || value.ArrayVal[0].StrVal != "a" || value.ArrayVal[1].StrVal != `b"c` {
		t.Fatalf("owned identifiers=%v", value.ArrayVal)
	}
}

func TestParseIdentifierCancellationDuringLeadingWhitespace(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: 4}
	_, err := identifierCallable(t).Call(ctx, []model.Value{model.StringVal(strings.Repeat(" ", 1<<20) + "name"), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled identifier scan=%v", err)
	}
}
