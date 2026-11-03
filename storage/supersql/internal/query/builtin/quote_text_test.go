package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func textQuoteCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireQuotedText(t *testing.T, function builtin.Callable, text, want string) {
	t.Helper()
	args := []model.Value{model.StringVal(text)}
	value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || value.Type != model.TypeString || value.IsNull || value.StrVal != want {
		t.Fatalf("quote(%q) = %v, %v; want %q", text, value, err, want)
	}
	if text != "" && unsafe.StringData(value.StrVal) == unsafe.StringData(args[0].StrVal) {
		t.Fatal("quoted result retains borrowed input storage")
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestQuoteIdentPostgreSQLIdentifierRules(t *testing.T) {
	function := textQuoteCallable(t, 1282)
	for _, tc := range []struct {
		name, text, want string
	}{
		{"empty", "", `""`},
		{"lowercase and digits", "a_09", "a_09"},
		{"leading underscore", "_09", "_09"},
		{"leading digit", "9a", `"9a"`},
		{"uppercase", "Mixed", `"Mixed"`},
		{"reserved keyword", "select", `"select"`},
		{"unreserved keyword", "abort", "abort"},
		{"column name keyword", "between", `"between"`},
		{"type function keyword", "authorization", `"authorization"`},
		{"keyword prefix", "select1", "select1"},
		{"dollar in identifier", "a$b", `"a$b"`},
		{"Unicode", "é界", `"é界"`},
		{"double quotes", `a""b`, `"a""""b"`},
		{"literal backslash", `a\b`, `"a\b"`},
		{"single quote", "a'b", `"a'b"`},
		{"long identifier is not truncated", strings.Repeat("a", 64), strings.Repeat("a", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireQuotedText(t, function, tc.text, tc.want)
		})
	}
}

func TestQuoteLiteralEscaping(t *testing.T) {
	for _, id := range []model.FunctionID{1283, 1289} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := textQuoteCallable(t, id)
			for _, tc := range []struct {
				name, text, want string
			}{
				{"empty", "", "''"},
				{"literal NULL", "NULL", "'NULL'"},
				{"single quotes", "a''b", "'a''''b'"},
				{"backslash enables escape syntax", `a\b`, `E'a\\b'`},
				{"backslash and quote", `\'`, `E'\\'''`},
				{"double quote is literal", `a"b`, `'a"b'`},
				{"Unicode and newline", "é界\n�", "'é界\n�'"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					requireQuotedText(t, function, tc.text, tc.want)
				})
			}
		})
	}
}

func TestTextQuotingCrossesCopyBoundaries(t *testing.T) {
	prefix := strings.Repeat("x", 4094)
	for _, tc := range []struct {
		name       string
		id         model.FunctionID
		text, want string
	}{
		{"identifier", 1282, prefix + "\"é", `"` + prefix + `""é"`},
		{"literal", 1283, prefix + "\\é'", `E'` + prefix + `\\é'''`},
		{"nullable", 1289, prefix + "\\é'", `E'` + prefix + `\\é'''`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireQuotedText(t, textQuoteCallable(t, tc.id), tc.text, tc.want)
		})
	}
}

func TestStrictTextQuotesPreserveNull(t *testing.T) {
	for _, id := range []model.FunctionID{1282, 1283} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := textQuoteCallable(t, id)
			argument := model.Value{Type: model.TypeString, IsNull: true, StrVal: "\xff\x00"}
			value, err := function.Call(t.Context(), []model.Value{argument}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.Type != model.TypeString || !value.IsNull {
				t.Fatalf("quote(NULL) = %v, %v; want text NULL", value, err)
			}
		})
	}
}

func TestQuoteNullableOwnsNullText(t *testing.T) {
	function := textQuoteCallable(t, 1289)
	args := []model.Value{{Type: model.TypeString, IsNull: true, StrVal: "\xff\x00"}}
	first, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
	if err != nil || first.Type != model.TypeString || first.IsNull || first.StrVal != "NULL" {
		t.Fatalf("quote_nullable(NULL) = %v, %v; want owned NULL text", first, err)
	}
	second, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
	if err != nil || second.Type != model.TypeString || second.IsNull || second.StrVal != "NULL" {
		t.Fatalf("second quote_nullable(NULL) = %v, %v; want owned NULL text", second, err)
	}
	if unsafe.StringData(first.StrVal) == unsafe.StringData(second.StrVal) {
		t.Fatal("separate calls share NULL output storage")
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestTextQuotesRejectInvalidTextDatums(t *testing.T) {
	for _, id := range []model.FunctionID{1282, 1283, 1289} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := textQuoteCallable(t, id)
			for _, tc := range []struct {
				name, text string
			}{
				{"invalid UTF8", "a\xffb"},
				{"embedded NUL", "a\x00b"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					_, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.text)}, builtin.CallBudget{OutputBytes: 100, MemoryBytes: 64 << 20})
					requireSQLState(t, err, "22021")
				})
			}
		})
	}
}

func TestTextQuotesHonorCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		text string
	}{
		{"unquoted identifier", 1282, strings.Repeat("a", 65536)},
		{"quoted identifier", 1282, strings.Repeat("a\"'\\", 16384)},
		{"literal", 1283, strings.Repeat("a\"'\\", 16384)},
		{"nullable", 1289, strings.Repeat("a\"'\\", 16384)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := textQuoteCallable(t, tc.id)
			args := []model.Value{model.StringVal(tc.text)}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := function.Call(ctx, args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-canceled quote = %v", err)
			}
			for _, phase := range []struct {
				name      string
				remaining int
			}{
				{"validation", 3},
				{"sizing", 20},
				{"copying", 40},
			} {
				t.Run(phase.name, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: phase.remaining}
					_, err := function.Call(interrupted, args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("interrupted quote = %v", err)
					}
				})
			}
		})
	}
}
