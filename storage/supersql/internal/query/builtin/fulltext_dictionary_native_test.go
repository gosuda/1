package builtin

import (
	"context"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// TestNativeSynonymDictionaryLexize checks the pinned synonym_sample.syn
// against ts_lexize's contract: a nil result for an unknown word, and exactly
// one lexeme for a known word. PostgreSQL's own oracle lives in
// testdata/postgres184/expected/tsdicts.out.
func TestNativeSynonymDictionaryLexize(t *testing.T) {
	ctx := context.Background()
	dictionary, err := newNativeSynonymDictionary(ctx, "synonym_sample", false)
	if err != nil {
		t.Fatalf("newNativeSynonymDictionary: %v", err)
	}
	cases := []struct {
		token string
		want  []string
	}{
		{token: "PoStGrEs", want: []string{"pgsql"}},
		{token: "Gogle", want: []string{"googl"}},
		{token: "indices", want: []string{"index"}},
		{token: "unknown_token", want: nil},
	}
	for _, testCase := range cases {
		got, err := dictionary.Lexize(ctx, testCase.token)
		if err != nil {
			t.Fatalf("Lexize(%q): %v", testCase.token, err)
		}
		if len(got) != len(testCase.want) {
			t.Fatalf("Lexize(%q) = %v, want %v", testCase.token, got, testCase.want)
		}
		for i := range got {
			if got[i] != testCase.want[i] {
				t.Fatalf("Lexize(%q) = %v, want %v", testCase.token, got, testCase.want)
			}
		}
	}
}

// TestNativeSynonymDictionaryCaseSensitive checks that the case_sensitive
// option stops the lower-casing dsynonym_init applies by default.
func TestNativeSynonymDictionaryCaseSensitive(t *testing.T) {
	ctx := context.Background()
	dictionary, err := newNativeSynonymDictionary(ctx, "synonym_sample", true)
	if err != nil {
		t.Fatalf("newNativeSynonymDictionary: %v", err)
	}
	got, err := dictionary.Lexize(ctx, "PoStGrEs")
	if err != nil {
		t.Fatalf("Lexize: %v", err)
	}
	if got != nil {
		t.Fatalf("case-sensitive Lexize(\"PoStGrEs\") = %v, want nil", got)
	}
}

// TestNativeThesaurusDictionaryLexize checks the thesaurus_sample.ths rules
// against an accept-all sub-dictionary, mirroring the single-word path
// ts_lexize exposes when PostgreSQL passes a NULL sub-state.
func TestNativeThesaurusDictionaryLexize(t *testing.T) {
	ctx := context.Background()
	thesaurus, err := newNativeThesaurusDictionary(ctx, "thesaurus_sample", nativeAcceptAllDictionary{})
	if err != nil {
		t.Fatalf("newNativeThesaurusDictionary: %v", err)
	}
	got, err := thesaurus.Lexize(ctx, "one")
	if err != nil {
		t.Fatalf("Lexize: %v", err)
	}
	if len(got) != 1 || got[0] != "1" {
		t.Fatalf("Lexize(\"one\") = %v, want [1]", got)
	}
}

// nativeAcceptAllDictionary is the test double for PostgreSQL's simple
// dictionary: every word is its own lexeme, so a thesaurus rule's sample words
// match themselves.
type nativeAcceptAllDictionary struct{}

func (nativeAcceptAllDictionary) Lexize(_ context.Context, token string) ([]string, error) {
	return []string{token}, nil
}

var _ model.TextSearchDictionaryKernel = nativeAcceptAllDictionary{}

// ispellLexizeCases is the frozen lexize contract for the pinned ispell and
// hunspell dictionaries, transcribed from
// testdata/postgres184/expected/tsdicts.out lines 10-300. All four dictionary
// files share the same rows, and the ordering is significant: it is the order
// PostgreSQL's NINormalizeWord emits, including the whole word first for a
// compound-capable base form.
var ispellLexizeCases = []struct {
	token string
	want  []string
}{
	{"skies", []string{"sky"}},
	{"bookings", []string{"booking", "book"}},
	{"booking", []string{"booking", "book"}},
	{"foot", []string{"foot"}},
	{"foots", []string{"foot"}},
	{"rebookings", []string{"booking", "book"}},
	{"rebooking", []string{"booking", "book"}},
	{"rebook", nil},
	{"unbookings", []string{"book"}},
	{"unbooking", []string{"book"}},
	{"unbook", []string{"book"}},
	{"footklubber", []string{"foot", "klubber"}},
	{"footballklubber", []string{"footballklubber", "foot", "ball", "klubber", "football", "klubber"}},
	{"ballyklubber", []string{"ball", "klubber"}},
	{"footballyklubber", []string{"foot", "ball", "klubber"}},
}

// TestNativeIspellDictionaryLexize checks the old Ispell affix format, the
// Hunspell PFX/SFX format, and both Hunspell flag encodings against the frozen
// corpus rows. unbook is deliberately a hit while rebook is deliberately empty,
// so a blanket prefix stripper fails here.
func TestNativeIspellDictionaryLexize(t *testing.T) {
	ctx := context.Background()
	for _, fixture := range []struct{ name, dict, affix string }{
		{"ispell", "ispell_sample", "ispell_sample"},
		{"hunspell", "ispell_sample", "hunspell_sample"},
		{"hunspell_long", "hunspell_sample_long", "hunspell_sample_long"},
		{"hunspell_num", "hunspell_sample_num", "hunspell_sample_num"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dictionary, err := newNativeIspellDictionary(ctx, fixture.dict, fixture.affix)
			if err != nil {
				t.Fatalf("newNativeIspellDictionary: %v", err)
			}
			for _, testCase := range ispellLexizeCases {
				got, err := dictionary.Lexize(ctx, testCase.token)
				if err != nil {
					t.Fatalf("Lexize(%q): %v", testCase.token, err)
				}
				if !slices.Equal(got, testCase.want) {
					t.Errorf("Lexize(%q) = %v, want %v", testCase.token, got, testCase.want)
				}
			}
		})
	}
}

// ispellCompoundOnlyCases pins CheckAffix's `flagflags == 0` branch for the
// four pinned affix files. Every one of them marks the Y suffix ONLYINCOMPOUND
// — hunspell_sample.affix through the replacement flags `Y/L`,
// hunspell_sample_long.affix through the alias `Y/2`, hunspell_sample_num.affix
// through `Y/102`, and ispell_sample.affix through the old-format `flag ~\`
// form — so that suffix may only build a compound member. A standalone token
// that would need it (ball + Y) therefore has no lexeme, while the same token
// still splits inside a compound: testdata/postgres184/expected/tsdicts.out
// lines 182-193 hold ballyklubber -> {ball,klubber}.
var ispellCompoundOnlyCases = []struct {
	token string
	want  []string
}{
	{"ball", []string{"ball"}},
	{"bally", nil},
	{"ballyklubber", []string{"ball", "klubber"}},
	{"footballyklubber", []string{"foot", "ball", "klubber"}},
}

// TestNativeIspellDictionaryStandaloneCompoundOnlyAffix checks that an affix
// marked ONLYINCOMPOUND is refused for an ordinary word. Before the fix the
// lexizer applied the suffix unconditionally, so Lexize("bally") returned
// [ball] instead of nil for all four dictionaries.
func TestNativeIspellDictionaryStandaloneCompoundOnlyAffix(t *testing.T) {
	ctx := context.Background()
	for _, fixture := range []struct{ name, dict, affix string }{
		{"ispell", "ispell_sample", "ispell_sample"},
		{"hunspell", "ispell_sample", "hunspell_sample"},
		{"hunspell_long", "hunspell_sample_long", "hunspell_sample_long"},
		{"hunspell_num", "hunspell_sample_num", "hunspell_sample_num"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			dictionary, err := newNativeIspellDictionary(ctx, fixture.dict, fixture.affix)
			if err != nil {
				t.Fatalf("newNativeIspellDictionary: %v", err)
			}
			for _, testCase := range ispellCompoundOnlyCases {
				got, err := dictionary.Lexize(ctx, testCase.token)
				if err != nil {
					t.Fatalf("Lexize(%q): %v", testCase.token, err)
				}
				if !slices.Equal(got, testCase.want) {
					t.Errorf("Lexize(%q) = %v, want %v", testCase.token, got, testCase.want)
				}
			}
		})
	}
}

// TestCompileTextSearchDictionaryKernelIspell pins the dispatch a real
// ts_lexize call takes: a descriptor with Kind ispell must reach the native
// lexizer, so the constructor being correct is not by itself sufficient.
func TestCompileTextSearchDictionaryKernelIspell(t *testing.T) {
	ctx := context.Background()
	kernel, err := CompileTextSearchDictionaryKernel(ctx, model.TextSearchDictionaryMetadata{
		Kind: model.TextSearchIspellDictionary, DictionaryFile: "hunspell_sample_num", AffixFile: "hunspell_sample_num",
	}, nil)
	if err != nil {
		t.Fatalf("CompileTextSearchDictionaryKernel: %v", err)
	}
	if kernel == nil {
		t.Fatal("ispell descriptor compiled to a nil kernel")
	}
	for _, testCase := range ispellLexizeCases {
		got, err := kernel.Lexize(ctx, testCase.token)
		if err != nil {
			t.Fatalf("Lexize(%q): %v", testCase.token, err)
		}
		if !slices.Equal(got, testCase.want) {
			t.Errorf("Lexize(%q) = %v, want %v", testCase.token, got, testCase.want)
		}
	}
}

// TestNativeIspellDictionaryRejectsMismatchedAffixFiles checks that a dictionary
// whose flag encoding cannot be read by the affix file is rejected at compile
// time, as PostgreSQL's NISortDictionary does. testdata/postgres184/sql/tsdicts.sql
// lines 100-140 pin these three combinations as errors.
func TestNativeIspellDictionaryRejectsMismatchedAffixFiles(t *testing.T) {
	ctx := context.Background()
	for _, mismatch := range []struct{ name, dict, affix string }{
		{"char flags with alias table", "ispell_sample", "hunspell_sample_long"},
		{"char flags with numeric flags", "ispell_sample", "hunspell_sample_num"},
		{"numeric flags with alias table", "hunspell_sample_num", "hunspell_sample_long"},
	} {
		t.Run(mismatch.name, func(t *testing.T) {
			if _, err := newNativeIspellDictionary(ctx, mismatch.dict, mismatch.affix); err == nil {
				t.Fatal("mismatched dictionary and affix files were accepted")
			}
		})
	}
}
