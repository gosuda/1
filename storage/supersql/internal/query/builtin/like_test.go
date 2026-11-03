package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func requireLikeResult(t *testing.T, function builtin.Callable, source, pattern model.Value, want bool) {
	t.Helper()
	value, err := function.Call(t.Context(), []model.Value{source, pattern}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IsNull || value.Type != model.TypeBoolean || value.IntVal != model.BoolVal(want).IntVal {
		t.Fatalf("function %d LIKE result = %v, %v; want %t", function.Signature().ID, value, err, want)
	}
}

func TestLikeFunctionVariants(t *testing.T) {
	for _, tc := range []struct {
		id          model.FunctionID
		negate      bool
		insensitive bool
	}{
		{850, false, false}, {851, true, false},
		{858, false, false}, {859, true, false},
		{1569, false, false}, {1570, true, false},
		{1571, false, false}, {1572, true, false},
		{1631, false, false}, {1632, true, false},
		{1633, false, true}, {1634, true, true},
		{1635, false, true}, {1636, true, true},
		{1660, false, true}, {1661, true, true},
		{2005, false, false}, {2006, true, false},
		{2007, false, false}, {2008, true, false},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{Collation: 950})
			if err != nil {
				t.Fatal(err)
			}
			for _, input := range []struct {
				name   string
				source string
				match  bool
			}{
				{"matching wildcard", "aBtail", true},
				{"case distinction", "ABtail", tc.insensitive},
				{"mismatching prefix", "bBtail", false},
			} {
				t.Run(input.name, func(t *testing.T) {
					source, pattern := model.StringVal(input.source), model.StringVal("a_%")
					if function.Signature().Arguments[0] == 17 {
						source, pattern = model.BytesVal([]byte(input.source)), model.BytesVal([]byte("a_%"))
					}
					requireLikeResult(t, function, source, pattern, input.match != tc.negate)
				})
			}
		})
	}
}

func TestLikeTextCharacters(t *testing.T) {
	function := callable(t, "textlike", 25, 25)
	for _, tc := range []struct {
		name, source, pattern string
		want                  bool
	}{
		{"empty pattern", "", "", true},
		{"empty text percent", "", "%%%%", true},
		{"empty text underscore", "", "_", false},
		{"nonempty text empty pattern", "a", "", false},
		{"multibyte underscore", "日", "_", true},
		{"two characters", "é日", "__", true},
		{"underscore is not a byte", "é", "__", false},
		{"combining mark is separate", "e\u0301", "_", false},
		{"percent candidate character alignment", "èé日", "%é_", true},
		{"multibyte literal mismatch", "è", "%é", false},
		{"repeated percent", "é日b", "%%_%_%b%%", true},
		{"escaped wildcards", `a_b%c\d`, `a\_b\%c\\d`, true},
		{"escaped multibyte literal", "é日", `\é_`, true},
		{"escaped underscore is literal", "axb", `a\_b`, false},
		{"literal mismatch", "abc", "abd", false},
		{"whole string anchoring", "zabc", "abc", false},
		{"percent retries partial literal", "aaab", "%aab", true},
		{"nested percent", "xxabyyacz", "%ab%a_z", true},
		{"newline underscore", "a\nb", "a_b", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireLikeResult(t, function, model.StringVal(tc.source), model.StringVal(tc.pattern), tc.want)
		})
	}
}

func TestILikeUsesASCIICaseFolding(t *testing.T) {
	for _, collation := range []model.CollationID{950, 951} {
		function, err := builtin.Resolve(1633, builtin.ResolveOptions{Collation: collation})
		if err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, source, pattern string
			want                  bool
		}{
			{"ASCII folding around Unicode", "AéZ", "a_z", true},
			{"nonASCII case remains distinct", "É", "é", false},
			{"Unicode simple fold is excluded", "K", "k", false},
			{"escaped ASCII is folded", "A", `\a`, true},
			{"folded percent candidate", "xxAbC", "%a_c", true},
		} {
			t.Run(fmt.Sprintf("%d/%s", collation, tc.name), func(t *testing.T) {
				requireLikeResult(t, function, model.StringVal(tc.source), model.StringVal(tc.pattern), tc.want)
			})
		}
	}
}

func TestLikeBpcharPreservesPadding(t *testing.T) {
	for _, name := range []string{"bpcharlike", "bpchariclike"} {
		function := callable(t, name, 1042, 25)
		t.Run(name, func(t *testing.T) {
			requireLikeResult(t, function, model.StringVal("a  "), model.StringVal("a"), false)
			requireLikeResult(t, function, model.StringVal("a  "), model.StringVal("a__"), true)
			requireLikeResult(t, function, model.StringVal("a  "), model.StringVal("a%"), true)
		})
	}
}

func TestLikeByteaUsesArbitraryBytes(t *testing.T) {
	function, err := builtin.Resolve(2005, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		source, pattern []byte
		want            bool
	}{
		{"NUL and invalid UTF8", []byte{0, 255, 128}, []byte{'_', 255, '_'}, true},
		{"NUL literal", []byte{0, 255, 0}, []byte{0, '%', 0}, true},
		{"UTF8 bytes counted separately", []byte("é"), []byte("__"), true},
		{"single byte underscore", []byte("é"), []byte("_"), false},
		{"escaped NUL", []byte{0}, []byte{'\\', 0}, true},
		{"escaped percent", []byte{0, '%', 255}, []byte{'_', '\\', '%', 255}, true},
		{"literal mismatch after NUL", []byte{0, 255}, []byte{0, 254}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireLikeResult(t, function, model.BytesVal(tc.source), model.BytesVal(tc.pattern), tc.want)
		})
	}
}

func TestLikeTrailingEscapeIsCheckedWhenReached(t *testing.T) {
	for _, id := range []model.FunctionID{850, 851, 1633, 1634, 2005, 2006} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{Collation: 950})
		if err != nil {
			t.Fatal(err)
		}
		negate := id == 851 || id == 1634 || id == 2006
		for _, tc := range []struct {
			name, source, pattern string
			invalid               bool
		}{
			{"empty text", "", `\`, false},
			{"literal consumes text", "a", `a\`, false},
			{"literal mismatches", "b", `a\`, false},
			{"percent unreachable", "", `%\`, false},
			{"percent after consumed literal", "a", `a%\`, false},
			{"percent suffix never found", "abc", `%z\`, false},
			{"percent suffix consumes text", "ab", `%b\`, false},
			{"percent suffix reaches escape", "abc", `%b\`, true},
			{"compression reaches escape", "a", `%_\`, true},
			{"compression missing underscore", "a", `%__\`, false},
			{"repeated percent reaches escape", "a", `%%_%%\`, true},
			{"nested percent abort", "aa", `%a%a\`, false},
			{"bare escape reached", "a", `\`, true},
		} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				source, pattern := model.StringVal(tc.source), model.StringVal(tc.pattern)
				if id == 2005 || id == 2006 {
					source, pattern = model.BytesVal([]byte(tc.source)), model.BytesVal([]byte(tc.pattern))
				}
				if tc.invalid {
					_, err := function.Call(t.Context(), []model.Value{source, pattern}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireSQLState(t, err, "22025")
					return
				}
				requireLikeResult(t, function, source, pattern, negate)
			})
		}
	}
}

func TestLikeRejectsInvalidText(t *testing.T) {
	for _, id := range []model.FunctionID{850, 858, 1631, 1633, 1635, 1660} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{Collation: 950})
		if err != nil {
			t.Fatal(err)
		}
		for _, invalid := range []string{"\xff", "\x00", "\xc3"} {
			for _, args := range [][]model.Value{
				{model.StringVal(invalid), model.StringVal("%")},
				{model.StringVal(""), model.StringVal(invalid)},
			} {
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22021")
			}
		}
	}
}

func TestLikeCollationAndStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{850, 858, 1631, 1633, 1635, 1660} {
		for _, options := range []builtin.ResolveOptions{
			{}, {Collation: 100}, {Collation: 100, DefaultCollation: 100}, {Collation: 123456},
		} {
			function, err := builtin.Resolve(id, options)
			if err != nil {
				t.Fatal(err)
			}
			for _, args := range [][]model.Value{
				{model.NullVal(model.TypeString), model.StringVal(`\`)},
				{model.StringVal("\xff"), model.NullVal(model.TypeString)},
			} {
				value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull || value.Type != model.TypeBoolean {
					t.Fatalf("function %d strict NULL = %v, %v", id, value, err)
				}
			}
			_, err = function.Call(t.Context(), []model.Value{model.StringVal("a"), model.StringVal("%")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "42P22")
		}
		for _, concrete := range []model.CollationID{950, 951} {
			function, err := builtin.Resolve(id, builtin.ResolveOptions{Collation: 100, DefaultCollation: concrete})
			if err != nil {
				t.Fatal(err)
			}
			requireLikeResult(t, function, model.StringVal("a"), model.StringVal("%"), true)
		}
	}
	function, err := builtin.Resolve(2006, builtin.ResolveOptions{Collation: 123456})
	if err != nil {
		t.Fatal(err)
	}
	value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeBytes), model.BytesVal([]byte{'\\'})}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull || value.Type != model.TypeBoolean {
		t.Fatalf("bytea NOT LIKE strict NULL = %v, %v", value, err)
	}
	requireLikeResult(t, function, model.BytesVal([]byte{255}), model.BytesVal([]byte{'_'}), false)
}

func TestLikeCancellationDuringMatching(t *testing.T) {
	function := callable(t, "bytealike", 17, 17)
	for _, tc := range []struct {
		name            string
		source, pattern []byte
	}{
		{"literal scan", bytes.Repeat([]byte{'a'}, 20000), bytes.Repeat([]byte{'a'}, 20000)},
		{"percent compression", []byte{'a'}, bytes.Repeat([]byte{'%'}, 20000)},
		{"empty text percent scan", nil, bytes.Repeat([]byte{'%'}, 20000)},
		{"percent candidate retries", bytes.Repeat([]byte{'a'}, 1000), []byte("%" + strings.Repeat("a", 100) + "b")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			// Bytea needs no UTF8 scan, so cancellation occurs in matching work.
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 4}
			_, err := function.Call(interrupted, []model.Value{model.BytesVal(tc.source), model.BytesVal(tc.pattern)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted LIKE = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := function.Call(ctx, []model.Value{model.BytesVal([]byte{'a'}), model.BytesVal([]byte{'\\'})}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled LIKE = %v", err)
	}
}
