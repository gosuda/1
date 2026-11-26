package builtin_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func regexpCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	call, err := builtin.Resolve(id, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	if err != nil {
		t.Fatal(err)
	}
	return call
}

func regexpCall(t *testing.T, id model.FunctionID, arguments ...model.Value) model.Value {
	t.Helper()
	result, err := regexpCallable(t, id).Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 8 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRegexpARELeftmostPreferenceAndConstraints(t *testing.T) {
	for _, test := range []struct {
		name    string
		text    string
		pattern string
		want    bool
	}{
		{"alternation is longest", "aa", `^(a|aa)$`, true},
		{"greedy branch", "axxbxxb", `^a.*b$`, true},
		{"non-greedy branch", "axxb", `^a.*?b$`, true},
		{"backreference", "aaabaaa", `^(a+)b\1$`, true},
		{"positive lookahead", "ab", `a(?=b)`, true},
		{"negative lookahead", "ac", `a(?!b)`, true},
		{"positive lookbehind", "ab", `(?<=a)b`, true},
		{"negative lookbehind", "cb", `(?<!a)b`, true},
		{"word constraints", "a word!", `\mword\M`, true},
		{"absolute anchors", "a\nb", `\Ab.*\Z`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := regexpCall(t, 1254, model.StringVal(test.text), model.StringVal(test.pattern))
			if result.Type != model.TypeBoolean || result.IsNull || result.IntVal != model.BoolVal(test.want).IntVal {
				t.Fatalf("match = %v, want %t", result, test.want)
			}
		})
	}
	longest := regexpCall(t, 6265, model.StringVal("aa"), model.StringVal(`a|aa`))
	if longest.StrVal != "aa" {
		t.Fatalf("longest alternative = %q, want aa", longest.StrVal)
	}
	shortest := regexpCall(t, 6265, model.StringVal("axxbxxb"), model.StringVal(`a.*?b`))
	if shortest.StrVal != "axxb" {
		t.Fatalf("shortest match = %q, want axxb", shortest.StrVal)
	}
}

func TestRegexpSyntaxAndNewlineModes(t *testing.T) {
	for _, test := range []struct {
		name, text, pattern, flags string
		want                       bool
	}{
		{"basic", "abab", `\(ab\)\1`, "b", true},
		{"extended", "aaa", `^a+$`, "e", true},
		{"quote", "a+b", `a+b`, "q", true},
		{"expanded", "ab", "a  # ignored\n b", "x", true},
		{"newline dot", "a\nb", `a.b`, "n", false},
		{"newline anchor", "a\nb", `^b$`, "n", true},
		{"partial newline anchor", "a\nb", `^b$`, "p", false},
		{"inverse partial newline anchor", "a\nb", `^b$`, "w", true},
		{"inverse partial dot", "a\nb", `a.b`, "w", true},
		{"case insensitive ASCII", "ABC", `abc`, "i", true},
		{"C class excludes non-ASCII", "é", `[[:alpha:]]`, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := regexpCall(t, 6264, model.StringVal(test.text), model.StringVal(test.pattern), model.StringVal(test.flags))
			if result.Type != model.TypeBoolean || result.IsNull || result.IntVal != model.BoolVal(test.want).IntVal {
				t.Fatalf("match = %v, want %t", result, test.want)
			}
		})
	}
}

func TestRegexpCapturesReplacementSplitAndPositions(t *testing.T) {
	matched := regexpCall(t, 3396, model.StringVal("foobarbeque"), model.StringVal(`(bar)(beque)`))
	wantMatch := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}, ArrayVal: []model.Value{model.StringVal("bar"), model.StringVal("beque")}}
	if diff := cmp.Diff(wantMatch, matched); diff != "" {
		t.Fatalf("regexp_match (-want +got):\n%s", diff)
	}
	replaced := regexpCall(t, 2285, model.StringVal("abc"), model.StringVal(`x*`), model.StringVal("X"), model.StringVal("g"))
	if replaced.StrVal != "XaXbXcX" {
		t.Fatalf("empty replacement = %q", replaced.StrVal)
	}
	// PostgreSQL substitutes the whole match only for \&, never for a bare &:
	// appendStringInfoRegexpSubstr treats & specially only immediately after a
	// backslash (src/backend/utils/adt/varlena.c), and the corpus pins that
	// spelling in strings.sql:225 (E'X\\&Y' -> fXooYbaXrrYbaXzzY).
	replaced = regexpCall(t, 2284, model.StringVal("abc123"), model.StringVal(`([a-z]+)([0-9]+)`), model.StringVal(`\2-\1-\&`))
	if replaced.StrVal != "123-abc-abc123" {
		t.Fatalf("capture replacement = %q", replaced.StrVal)
	}
	replaced = regexpCall(t, 2284, model.StringVal("abc123"), model.StringVal(`([a-z]+)([0-9]+)`), model.StringVal(`\2-\1-&`))
	if replaced.StrVal != "123-abc-&" {
		t.Fatalf("literal ampersand replacement = %q, want 123-abc-&", replaced.StrVal)
	}
	replaced = regexpCall(t, 6253, model.StringVal("aaa"), model.StringVal("a"), model.StringVal("x"), model.Int64Val(1))
	if replaced.StrVal != "xaa" {
		t.Fatalf("extended replacement default n = %q, want xaa", replaced.StrVal)
	}
	replaced = regexpCall(t, 6252, model.StringVal("aaa"), model.StringVal("a"), model.StringVal("x"), model.Int64Val(1), model.Int64Val(0))
	if replaced.StrVal != "xxx" {
		t.Fatalf("extended replacement n zero = %q, want xxx", replaced.StrVal)
	}
	replaced = regexpCall(t, 2284, model.StringVal("a"), model.StringVal("a"), model.StringVal(`\\`))
	if replaced.StrVal != `\` {
		t.Fatalf("escaped replacement backslash = %q, want one backslash", replaced.StrVal)
	}
	split := regexpCall(t, 2767, model.StringVal("é日x"), model.StringVal(""))
	wantSplit := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: 1}}, ArrayVal: []model.Value{model.StringVal("é"), model.StringVal("日"), model.StringVal("x")}}
	if diff := cmp.Diff(wantSplit, split); diff != "" {
		t.Fatalf("empty split (-want +got):\n%s", diff)
	}
	position := regexpCall(t, 6257, model.StringVal("é日x"), model.StringVal("日"))
	if position.IntVal != 2 {
		t.Fatalf("regexp_instr = %d, want 2", position.IntVal)
	}
}

func TestRegexpSimilarTranslationAndSubstring(t *testing.T) {
	translated := regexpCall(t, 1986, model.StringVal(`a%_b`), model.StringVal(`\`))
	if translated.StrVal != `^(?:a.*.b)$` {
		t.Fatalf("similar translation = %q", translated.StrVal)
	}
	middle := regexpCall(t, 2074, model.StringVal("foobar"), model.StringVal(`%#"o_b#"%`), model.StringVal("#"))
	if middle.StrVal != "oob" {
		t.Fatalf("SIMILAR substring = %q, want oob", middle.StrVal)
	}
}

func TestRegexpErrorsCancellationAndLimits(t *testing.T) {
	for _, test := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
		code string
	}{
		{"invalid option", 6264, []model.Value{model.StringVal("a"), model.StringVal("a"), model.StringVal("z")}, "22023"},
		{"invalid expression", 6263, []model.Value{model.StringVal("a"), model.StringVal("(")}, "2201B"},
		{"invalid start", 6255, []model.Value{model.StringVal("a"), model.StringVal("a"), model.Int64Val(0)}, "22023"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := regexpCallable(t, test.id).Call(t.Context(), test.args, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 1 << 20})
			requireSQLState(t, err, test.code)
		})
	}
	call, err := builtin.Resolve(6263, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = call.Call(t.Context(), []model.Value{model.StringVal("a"), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 1 << 20})
	requireSQLState(t, err, "42P22")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = regexpCallable(t, 6263).Call(ctx, []model.Value{model.StringVal("a"), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 4096, MemoryBytes: 1 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled match = %v", err)
	}
	_, err = regexpCallable(t, 6263).Call(t.Context(), []model.Value{model.StringVal("a"), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 128, MemoryBytes: 128})
	requireSQLState(t, err, "54000")
}

func TestRegexpRowSources(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 950, Storage: model.TypeString}}
	textArray := model.TypeLayout{Type: model.Type{ID: 1009, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{text}}
	matches, err := builtin.ResolveRegexpRows(t.Context(), 2764, []model.TypeLayout{text, text, text}, textArray, builtin.ResolveOptions{Collation: 950}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	iterator, err := matches.Open(t.Context(), []model.Value{model.StringVal("ab ac"), model.StringVal(`a(.)`), model.StringVal("g")}, builtin.RowSourceOptions{MaxInputBytes: 4096, MaxRowBytes: 4096, MaxMemoryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = iterator.Close() }()
	for index, want := range []string{"b", "c"} {
		row, ok, err := iterator.Next(t.Context())
		if err != nil || !ok || len(row) != 1 || len(row[0].ArrayVal) != 1 || row[0].ArrayVal[0].StrVal != want {
			t.Fatalf("row %d = %v, %t, %v", index, row, ok, err)
		}
	}
	if _, ok, err := iterator.Next(t.Context()); err != nil || ok {
		t.Fatalf("end = %t, %v", ok, err)
	}
}
