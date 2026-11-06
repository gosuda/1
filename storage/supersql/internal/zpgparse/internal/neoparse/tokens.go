package neoparse

import (
	"strings"

	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/pgproto"
)

// ScanResult and ScanToken are aliased to pg_query types for 100% surface compatibility.
type ScanResult = pg_query.ScanResult
type ScanToken = pg_query.ScanToken

type KeywordKind int32

const (
	KeywordKind_NO_KEYWORD KeywordKind = iota
	KeywordKind_UNRESERVED_KEYWORD
	KeywordKind_COL_NAME_KEYWORD
	KeywordKind_TYPE_FUNC_NAME_KEYWORD
	KeywordKind_RESERVED_KEYWORD
)

type KeywordInfo struct {
	Token       int
	KeywordKind KeywordKind
}

func LookupKeyword(word string) (KeywordInfo, bool) {
	keyword, ok := ScanKeywords[strings.ToLower(word)]
	return keyword, ok
}
