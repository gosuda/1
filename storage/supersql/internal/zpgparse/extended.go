package zpgparse

import "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/neoparse"

// ParseOptions configures SuperSQL extensions and request-local PostgreSQL
// lexical settings. Its zero value accepts only PostgreSQL syntax and uses
// PostgreSQL 18.4 lexical defaults.
type ParseOptions = neoparse.ParseOptions

// BooleanOption distinguishes an omitted parser setting from explicit false.
type BooleanOption = neoparse.BooleanOption

const (
	BooleanDefault = neoparse.BooleanDefault
	BooleanTrue    = neoparse.BooleanTrue
	BooleanFalse   = neoparse.BooleanFalse
)

func BooleanOptionFor(value bool) BooleanOption {
	return neoparse.BooleanOptionFor(value)
}

// BackslashQuoteMode follows PostgreSQL's backslash_quote values. Under the
// parser's fixed UTF8 encoding, SafeEncoding permits escaped quotes.
type BackslashQuoteMode = neoparse.BackslashQuoteMode

const (
	BackslashQuoteDefault      = neoparse.BackslashQuoteDefault
	BackslashQuoteOn           = neoparse.BackslashQuoteOn
	BackslashQuoteOff          = neoparse.BackslashQuoteOff
	BackslashQuoteSafeEncoding = neoparse.BackslashQuoteSafeEncoding
)

func ParseBackslashQuote(value string) (BackslashQuoteMode, error) {
	return neoparse.ParseBackslashQuote(value)
}

// LexicalPolicy is the resolved parser policy and forms part of statement identity.
type LexicalPolicy = neoparse.LexicalPolicy

type DiagnosticSeverity = neoparse.DiagnosticSeverity

const SeverityWarning = neoparse.SeverityWarning

// ParseDiagnostic is an owned, request-local non-fatal parser diagnostic.
type ParseDiagnostic = neoparse.ParseDiagnostic

// TemporalClause associates an unmodified expression AST with its top-level
// statement. Locations are zero-based UTF-8 byte offsets into the original SQL.
// RelationLocation is -1 for SET TRANSACTION, otherwise the RangeVar location.
// StatementIndex indexes ParseResult.Stmts, excluding empty statements.
type TemporalClause = neoparse.TemporalClause

// OnlineDDLClause marks a terminal ONLINE on ALTER TABLE. StatementIndex indexes
// ParseResult.Stmts; Location is the marker's zero-based UTF-8 byte offset.
// Whether an ALTER action can run online is a semantic, not grammatical, check.
type OnlineDDLClause = neoparse.OnlineDDLClause

// ExtendedParseResult owns the PostgreSQL AST, extension sidecars, resolved
// lexical policy, and diagnostics. Consumers must include the sidecars and
// LexicalPolicy in statement identity; ParseResult alone can be misinterpreted
// if cached independently.
type ExtendedParseResult = neoparse.ExtendedParseResult

func ParseExtended(input string, options ParseOptions) (*ExtendedParseResult, error) {
	result, err := neoparse.ParseExtended(input, options)
	return result, publicParseError(err)
}
