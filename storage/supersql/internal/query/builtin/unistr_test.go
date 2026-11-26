package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func unicodeStringCallable(t *testing.T) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(6198, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func TestUnicodeStringAcceptsMixedEscapeForms(t *testing.T) {
	function := unicodeStringCallable(t)
	for _, tc := range []struct{ input, want string }{
		{`d\0061t\+000061`, "data"},
		{`\u0041\U00010437\\Z`, "A\U00010437\\Z"},
		{`\D801\+00DC37`, "\U00010437"},
		{`\uD801\U0000DC37`, "\U00010437"},
		{`\FFFF`, "\uffff"},
		{`\U0010FFFF`, "\U0010FFFF"},
		{`\0041xyz`, "Axyz"},
	} {
		result, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input)}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
		if err != nil || result.StrVal != tc.want {
			t.Fatalf("unistr(%q)=%q,%v; want %q", tc.input, result.StrVal, err, tc.want)
		}
		_, err = function.Call(t.Context(), []model.Value{model.StringVal(tc.input)}, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54000")
	}
}

func TestUnicodeStringPreservesSurrogateErrorOrder(t *testing.T) {
	function := unicodeStringCallable(t)
	for _, tc := range []struct{ input, code string }{
		{`\`, "42601"}, {`\u12zz`, "42601"}, {`\0000`, "22023"}, {`\+110000`, "22023"}, {`\UFFFFFFFF`, "22023"},
		{`\DC00`, "42601"}, {`\D800`, "42601"}, {`\D800x`, "42601"}, {`\D800\\`, "42601"}, {`\D800\0041`, "42601"}, {`\D800\0000`, "22023"},
	} {
		_, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, tc.code)
	}
}

func TestUnicodeStringRetainsIndependentResults(t *testing.T) {
	function := unicodeStringCallable(t)
	first, err := function.Call(t.Context(), []model.Value{model.StringVal(`plain\0041`)}, builtin.CallBudget{OutputBytes: 6, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	_, err = function.Call(t.Context(), []model.Value{model.StringVal(`other\0042`)}, builtin.CallBudget{OutputBytes: 6, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if first.StrVal != "plainA" {
		t.Fatalf("retained result=%q", first.StrVal)
	}
}

func TestUnicodeStringCancellationAcrossEscapeBoundaries(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: 4}
	_, err := unicodeStringCallable(t).Call(ctx, []model.Value{model.StringVal(strings.Repeat(`\0041`, 5000))}, builtin.CallBudget{OutputBytes: 5000, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Unicode decoding=%v", err)
	}
}
