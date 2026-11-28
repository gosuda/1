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

func unicodeNormalizationCallable(t testing.TB, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func TestUnicodeNormalizationPreservesPostgresHangulBoundary(t *testing.T) {
	arguments := []model.Value{model.StringVal("\uac00\u11a7"), model.StringVal("NFC")}
	value, err := unicodeNormalizationCallable(t, 4350).Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if value.StrVal != "\uac00" {
		t.Fatalf("Hangul boundary normalization=%X", []rune(value.StrVal))
	}
	checked, err := unicodeNormalizationCallable(t, 4351).Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if checked.IntVal != 1 {
		t.Fatalf("PostgreSQL Hangul quick-check=%v, want true", checked)
	}
}

func TestUnicodeNormalizationForms(t *testing.T) {
	function := unicodeNormalizationCallable(t, 4350)
	for _, tc := range []struct {
		name, form, input, want string
	}{
		{"recursive canonical", "NFD", "\u1e69", "s\u0323\u0307"},
		{"hidden starter reordering", "NFC", "\u00c5\u0323", "\u1ea0\u030a"},
		{"compatibility ligature", "NFKD", "\ufb03", "ffi"},
		{"canonical preserves compatibility", "NFD", "\ufb03", "\ufb03"},
		{"compatibility recomposition", "NFKC", "\u1e9b\u0323", "\u1e69"},
		{"leading marks stable order", "NFD", "\u0301\u0323\u0300", "\u0323\u0301\u0300"},
		{"equal class blocks", "NFC", "A\u0305\u030a", "A\u0305\u030a"},
		{"lower class does not block", "NFC", "A\u0327\u030a", "\u00c5\u0327"},
		{"composition exclusion", "NFC", "\u0344", "\u0308\u0301"},
		{"grapheme joiner boundary", "NFC", "a\u0301\u034f\u0323", "\u00e1\u034f\u0323"},
		{"Hangul decomposition", "NFD", "\uac01", "\u1100\u1161\u11a8"},
		{"Hangul composition", "NFC", "\u1100\u1161\u11a8", "\uac01"},
		{"Hangul blocking", "NFC", "\u1100\u0301\u1161", "\u1100\u0301\u1161"},
		{"Unicode16 recursive decomposition", "NFD", "\U00016126", "\U0001611e\U0001611e\U0001611f"},
		{"Unicode16 zero class composition", "NFC", "\U000113c2\U000113c5\U000113b8", "\U000113c5\U000113c7"},
		{"Unicode16 recursive composition", "NFC", "\U0001611e\U00016123", "\U00016126"},
		{"Unicode16 compatibility", "NFKC", "\U0001ccd6", "A"},
		{"unassigned and private use", "NFKC", "\u0378\ue000\U0010ffff", "\u0378\ue000\U0010ffff"},
		{"mixed case form", "nFkC", "\ufb03", "ffi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arguments := []model.Value{model.StringVal(tc.input), model.StringVal(tc.form)}
			got, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || got.Type != model.TypeString || got.IsNull || got.StrVal != tc.want {
				t.Fatalf("normalize(%q, %s) = %q, %v; want %q", tc.input, tc.form, got.StrVal, err, tc.want)
			}
			_, err = function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestUnicodeIsNormalizedResolvesMaybeWithoutOutputBudget(t *testing.T) {
	function := unicodeNormalizationCallable(t, 4351)
	for _, tc := range []struct {
		name, form, input string
		want              bool
	}{
		{"composable", "NFC", "e\u0301", false},
		{"hidden starter decomposition", "NFC", "\u00c5\u0323", false},
		{"hidden starter already normalized", "NFC", "\u1ea0\u030a", true},
		{"equal class blocking", "NFC", "A\u0305\u030a", true},
		{"lower class permits composition", "NFC", "A\u0327\u030a", false},
		{"leading mark", "NFC", "\u0301", true},
		{"decomposition needed", "NFD", "\u00e9", false},
		{"decomposed", "NFD", "e\u0301", true},
		{"compatibility needed", "NFKC", "\ufb03", false},
		{"compatibility decomposed", "NFKD", "ffi", true},
		{"canonical order violation", "NFC", "q\u0301\u0323", false},
		{"Hangul composition needed", "NFC", "\u1100\u1161", false},
		{"Unicode16 composition needed", "NFC", "\U0001611e\U00016123", false},
		{"long normalized maybe", "NFC", "q" + strings.Repeat("\u0301", 8192), true},
		{"long unblocked composition", "NFC", "A" + strings.Repeat("\u0334", 8192) + "\u030a", false},
		{"empty", "NFKC", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input), model.StringVal(tc.form)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || got.Type != model.TypeBoolean || got.IsNull || (got.IntVal != 0) != tc.want {
				t.Fatalf("is_normalized(%s), input bytes %d = %v, %v; want %t", tc.form, len(tc.input), got, err, tc.want)
			}
		})
	}
}

func TestUnicodeNormalizationOrdersLongRunsStablyAtExactBudget(t *testing.T) {
	function := unicodeNormalizationCallable(t, 4350)
	for _, repetitions := range []int{8, 9, 2048} {
		t.Run(fmt.Sprint(repetitions), func(t *testing.T) {
			input := "q" + strings.Repeat("\u0315\u0301\u0323\u0300", repetitions)
			want := "q" + strings.Repeat("\u0323", repetitions) + strings.Repeat("\u0301\u0300", repetitions) + strings.Repeat("\u0315", repetitions)
			arguments := []model.Value{model.StringVal(input), model.StringVal("NFD")}
			got, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
			if err != nil || got.StrVal != want {
				t.Fatalf("normalizing %d combining groups = %q, %v; want %q", repetitions, got.StrVal, err, want)
			}
			_, err = function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestUnicodeNormalizationOwnsUnchangedAndChangedResults(t *testing.T) {
	function := unicodeNormalizationCallable(t, 4350)
	for _, tc := range []struct {
		name, input, want string
	}{
		{"ASCII", "plain", "plain"},
		{"unchanged nonASCII", "\u00e9", "\u00e9"},
		{"composed", "e\u0301", "\u00e9"},
		{"long run", "q" + strings.Repeat("\u0301", 128), "q" + strings.Repeat("\u0301", 128)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			borrowed := []byte(tc.input)
			text := unsafe.String(unsafe.SliceData(borrowed), len(borrowed))
			arguments := []model.Value{model.StringVal(text), model.StringVal("NFC")}
			got, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			clear(borrowed)
			clear(arguments)
			_, err = function.Call(t.Context(), []model.Value{model.StringVal("other"), model.StringVal("NFC")}, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if got.StrVal != tc.want {
				t.Fatalf("normalization retained borrowed storage or reused its output: %q, want %q", got.StrVal, tc.want)
			}
		})
	}
}

func TestUnicodeNormalizationFormErrorsPrecedeInputValidation(t *testing.T) {
	for _, id := range []model.FunctionID{4350, 4351} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := unicodeNormalizationCallable(t, id)
			for _, form := range []string{"", " NFC", "NFC ", "NFK", "nf\u212aC", "NFC\xff"} {
				_, err := function.Call(t.Context(), []model.Value{model.StringVal("\xff"), model.StringVal(form)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22023")
			}
			for _, input := range []string{"a\x00", "e\u0301\xff", "\xed\xa0\x80", "\xf4\x90\x80\x80"} {
				_, err := function.Call(t.Context(), []model.Value{model.StringVal(input), model.StringVal("NFC")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22021")
			}
			got, err := function.Call(t.Context(), []model.Value{model.StringVal(""), model.StringVal("nFc\x00ignored\xff")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			wrongText := id == 4350 && got.StrVal != ""
			wrongBoolean := id == 4351 && got.IntVal != 1
			if err != nil || got.IsNull || wrongText || wrongBoolean {
				t.Fatalf("C-string normalization form = %v, %v", got, err)
			}
			got, err = function.Call(t.Context(), []model.Value{model.NullVal(model.TypeString), model.StringVal("bad form")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !got.IsNull {
				t.Fatalf("strict NULL normalization = %v, %v; want NULL", got, err)
			}
		})
	}
}

func TestUnicodeNormalizationCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     model.FunctionID
		input  string
		form   string
		budget int64
		after  int
	}{
		{"ASCII validation", 4350, strings.Repeat("x", 1<<17), "NFC", 1 << 20, 4},
		{"ASCII output copy", 4350, strings.Repeat("x", 1<<17), "NFC", 1 << 20, 40},
		{"recursive expansion", 4350, strings.Repeat("\ufdfa", 1<<15), "NFKD", 4 << 20, 100},
		{"long combining replay", 4350, "q" + strings.Repeat("\u0315\u0301\u0323\u0300", 1<<14), "NFD", 1 << 20, 200},
		{"normalized maybe replay", 4351, "q" + strings.Repeat("\u0301", 1<<16), "NFC", 0, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := unicodeNormalizationCallable(t, tc.id)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: tc.after}
			got, err := function.Call(interrupted, []model.Value{model.StringVal(tc.input), model.StringVal(tc.form)}, builtin.CallBudget{OutputBytes: tc.budget, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) || got.StrVal != "" {
				t.Fatalf("interrupted normalization = %v, %v; want cancellation without partial output", got, err)
			}
		})
	}
}
