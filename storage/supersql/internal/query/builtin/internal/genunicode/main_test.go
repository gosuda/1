package main

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func pinnedSources(t *testing.T) map[string]string {
	t.Helper()
	sources, err := readSources("../../../../../testdata/postgres184/unicode")
	if err != nil {
		t.Fatal(err)
	}
	return sources
}

func pinnedData(t *testing.T) *unicodeData {
	t.Helper()
	data, err := parseData(pinnedSources(t))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPinnedDecompositionsPreserveInlineAndSupplementaryMappings(t *testing.T) {
	data := pinnedData(t)
	for _, test := range []struct {
		code uint32
		want []uint32
	}{
		{0x00a0, []uint32{0x0020}},
		{0x00c5, []uint32{0x0041, 0x030a}},
		{0x212b, []uint32{0x00c5}},
		{0x105c9, []uint32{0x105d2, 0x0307}},
		{0x2fa1d, []uint32{0x2a600}},
	} {
		var got []uint32
		for _, entry := range data.norm {
			if entry.code != test.code {
				continue
			}
			for i := range int(entry.flags & decompSize) {
				got = append(got, data.decompositionAt(entry, i))
			}
			break
		}
		if !slices.Equal(got, test.want) {
			t.Errorf("U+%X decomposition = %X, want %X", test.code, got, test.want)
		}
	}
}

func TestCompositionExclusionsAreNotInverted(t *testing.T) {
	data := pinnedData(t)
	for _, test := range []struct {
		left, right, want uint32
	}{
		{0x0041, 0x030a, 0x00c5},
		{0x105d2, 0x0307, 0x105c9},
		{0x0308, 0x0301, 0}, // Nonstarter decomposition, U+0344.
		{0x0915, 0x093c, 0}, // Composition exclusion, U+0958.
		{0x0020, 0x0308, 0}, // Compatibility decomposition, U+00A8.
	} {
		var got uint32
		for _, pair := range data.compositions {
			if pair.key == uint64(test.left)<<21|uint64(test.right) {
				got = pair.code
				break
			}
		}
		if got != test.want {
			t.Errorf("compose(U+%X, U+%X) = U+%X, want U+%X", test.left, test.right, got, test.want)
		}
	}
}

func TestCompressedQuickChecksPreserveEveryCodepoint(t *testing.T) {
	data := pinnedData(t)
	ranges := combineQuickChecks(data.quickChecks)
	var indices [2]int
	rangeIndex := 0
	for code := uint32(0); code <= maxCodepoint; code++ {
		var want uint8
		for form, entries := range data.quickChecks {
			if indices[form] < len(entries) && entries[indices[form]].code == code {
				want |= entries[indices[form]].value << (2 * form)
				indices[form]++
			}
		}
		for rangeIndex < len(ranges) && code > ranges[rangeIndex].last {
			rangeIndex++
		}
		var got uint8
		if rangeIndex < len(ranges) && code >= ranges[rangeIndex].first {
			got = ranges[rangeIndex].flags
		}
		if got != want {
			t.Fatalf("quick checks at U+%X = 0x%X, source = 0x%X", code, got, want)
		}
	}
}

func TestAssignedRangeCompressionPreservesCategoryGaps(t *testing.T) {
	data := pinnedData(t)
	ranges := assignedRanges(data.categories)
	categoryIndex, rangeIndex := 0, 0
	for code := uint32(0); code <= maxCodepoint; code++ {
		for categoryIndex < len(data.categories) && data.categories[categoryIndex].last < code {
			categoryIndex++
		}
		want := categoryIndex < len(data.categories) && data.categories[categoryIndex].first <= code
		for rangeIndex < len(ranges) && ranges[rangeIndex].last < code {
			rangeIndex++
		}
		got := rangeIndex < len(ranges) && ranges[rangeIndex].first <= code
		if got != want {
			t.Fatalf("assigned U+%X = %v, source categories = %v", code, got, want)
		}
	}
	for _, test := range []struct {
		code     uint32
		assigned bool
	}{
		{0x0377, true}, {0x0378, false},
		{0x105c0, true}, {0x105f3, true}, {0x105f4, false},
		{0xd800, true}, {0xdfff, true}, // PostgreSQL categories include surrogates.
		{0x10fffd, true}, {0x10fffe, false}, {0x10ffff, false},
	} {
		if got := categoryAt(data.categories, test.code) != "PG_U_UNASSIGNED"; got != test.assigned {
			t.Errorf("assigned U+%X = %v, want %v", test.code, got, test.assigned)
		}
	}
}

func TestRecursiveDecompositionBoundIncludesSharedDescendants(t *testing.T) {
	data := &unicodeData{
		norm: []normEntry{
			{code: 0xa0, flags: decompCompat | 3, index: 0},
			{code: 0xc0, flags: decompCompat | 2, index: 3},
		},
		decompositions: []uint32{0x41, 0x42, 0x43, 0xa0, 0xa0},
		categories:     []categoryRange{{codeRange: codeRange{first: 0, last: 0xff}, category: "PG_U_OTHER_LETTER"}},
	}
	if err := data.validate(); err != nil {
		t.Fatal(err)
	}
	if data.maxDecomposition != 6 {
		t.Fatalf("recursive decomposition bound = %d, want 6", data.maxDecomposition)
	}
}

func TestDecompositionBoundIncludesUntabledHangul(t *testing.T) {
	data := &unicodeData{
		norm:       []normEntry{{code: 0x300, class: 230}},
		categories: []categoryRange{{codeRange: codeRange{first: 0x300, last: 0x300}, category: "PG_U_NONSPACING_MARK"}},
	}
	if err := data.validate(); err != nil {
		t.Fatal(err)
	}
	if data.maxDecomposition != 3 {
		t.Fatalf("Hangul-inclusive bound = %d, want 3", data.maxDecomposition)
	}
}

func TestMalformedSourceCannotSilentlyLoseMappings(t *testing.T) {
	sources := pinnedSources(t)
	for _, test := range []struct {
		name, file, before, after string
	}{
		{"declared count", "unicode_norm_table.h", "UnicodeDecompMain[6843]", "UnicodeDecompMain[6842]"},
		{"unknown flag", "unicode_norm_table.h", "1 | DECOMP_COMPAT | DECOMP_INLINE, 0x0020", "1 | DECOMP_UNKNOWN | DECOMP_INLINE, 0x0020"},
		{"class overflow", "unicode_norm_table.h", "{0x00A0, 0,", "{0x00A0, 256,"},
		{"length overflow", "unicode_norm_table.h", "18 | DECOMP_COMPAT, 4702", "32 | DECOMP_COMPAT, 4702"},
		{"inline length", "unicode_norm_table.h", "1 | DECOMP_COMPAT | DECOMP_INLINE, 0x0020", "2 | DECOMP_COMPAT | DECOMP_INLINE, 0x0020"},
		{"pool bounds", "unicode_norm_table.h", "{0x2FA1D, 0, 1, 5137}", "{0x2FA1D, 0, 1, 5138}"},
		{"supplementary overflow", "unicode_norm_table.h", "/* 5137 */ 0x2A600", "/* 5137 */ 0x110000"},
		{"duplicate codepoint", "unicode_norm_table.h", "{0x00A8, 0,", "{0x00A0, 0,"},
		{"malformed row", "unicode_norm_table.h", "{0x00A8, 0,", "malformed {0x00A8, 0,"},
		{"decomposition cycle", "unicode_norm_table.h", "1 | DECOMP_COMPAT | DECOMP_INLINE, 0x0020", "1 | DECOMP_COMPAT | DECOMP_INLINE, 0x00A0"},
		{"quick check count", "unicode_normprops_table.h", "\n\t1252\n", "\n\t1251\n"},
		{"quick check state", "unicode_normprops_table.h", "{0x0300, UNICODE_NORM_QC_MAYBE}", "{0x0300, UNICODE_NORM_QC_YES}"},
		{"category overlap", "unicode_category_table.h", "{0x000020, 0x000020,", "{0x00001f, 0x000020,"},
		{"ASCII disagreement", "unicode_category_table.h", ".category = PG_U_CONTROL", ".category = PG_U_UNASSIGNED"},
		{"version mismatch", "unicode_version.h", "\"16.0\"", "\"17.0\""},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := make(map[string]string, len(sources))
			for name, source := range sources {
				changed[name] = source
			}
			if !strings.Contains(changed[test.file], test.before) {
				t.Fatalf("source fixture %s does not contain mutation target %q", test.file, test.before)
			}
			changed[test.file] = strings.Replace(changed[test.file], test.before, test.after, 1)
			if _, err := parseData(changed); !errors.Is(err, errInvalidData) {
				t.Fatalf("malformed source error = %v, want invalid Unicode data", err)
			}
		})
	}
}
