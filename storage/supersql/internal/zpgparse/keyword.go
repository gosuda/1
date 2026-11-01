package zpgparse

import (
	"sort"

	"github.com/gosuda/supersql-alpha/internal/zpgparse/internal/neoparse"
)

// KeywordCategory classifies an exact lower-case spelling using the parser's
// pinned PostgreSQL keyword table. Non-keywords return KeywordKind_NO_KEYWORD.
func KeywordCategory(word string) KeywordKind {
	keyword, ok := neoparse.ScanKeywords[word]
	if !ok {
		return KeywordKind_NO_KEYWORD
	}
	return KeywordKind(keyword.KeywordKind)
}

type Keyword struct {
	Word      string
	Kind      KeywordKind
	BareLabel bool
}

// Keywords returns the parser's complete PostgreSQL keyword inventory in
// lexical order. The returned slice is independently owned.
func Keywords() []Keyword {
	words := make([]string, 0, len(neoparse.ScanKeywords))
	for word := range neoparse.ScanKeywords {
		words = append(words, word)
	}
	sort.Strings(words)
	keywords := make([]Keyword, len(words))
	for i, word := range words {
		keywords[i] = Keyword{Word: word, Kind: KeywordKind(neoparse.ScanKeywords[word].KeywordKind), BareLabel: keywordIsBareLabel(word)}
	}
	return keywords
}

func keywordIsBareLabel(word string) bool {
	switch word {
	case "array", "as", "char", "character", "create", "day", "except",
		"fetch", "filter", "for", "from", "grant", "group", "having", "hour",
		"intersect", "into", "isnull", "limit", "minute", "month", "notnull",
		"offset", "on", "order", "over", "overlaps", "precision", "returning",
		"second", "to", "union", "varying", "where", "window", "with", "within",
		"without", "year":
		return false
	default:
		return true
	}
}
