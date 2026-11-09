package neoparse

import (
	"fmt"
	"strings"
	"unicode/utf8"

	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/pgproto"
)

type BooleanOption uint8

const (
	BooleanDefault BooleanOption = iota
	BooleanTrue
	BooleanFalse
)

func BooleanOptionFor(value bool) BooleanOption {
	if value {
		return BooleanTrue
	}
	return BooleanFalse
}

type BackslashQuoteMode uint8

const (
	BackslashQuoteDefault BackslashQuoteMode = iota
	BackslashQuoteOn
	BackslashQuoteOff
	BackslashQuoteSafeEncoding
)

func ParseBackslashQuote(value string) (BackslashQuoteMode, error) {
	switch {
	case strings.EqualFold(value, "on"):
		return BackslashQuoteOn, nil
	case strings.EqualFold(value, "off"):
		return BackslashQuoteOff, nil
	case strings.EqualFold(value, "safe_encoding"):
		return BackslashQuoteSafeEncoding, nil
	default:
		return BackslashQuoteDefault, fmt.Errorf("invalid backslash_quote value %q", value)
	}
}

type ParseOptions struct {
	AllowSystemTime          bool
	AllowOnlineDDL           bool
	StandardConformingStrings BooleanOption
	EscapeStringWarning       BooleanOption
	BackslashQuote            BackslashQuoteMode
}

type LexicalPolicy struct {
	StandardConformingStrings bool
	EscapeStringWarning       bool
	BackslashQuote            BackslashQuoteMode
}

type DiagnosticSeverity uint8

const (
	SeverityWarning DiagnosticSeverity = iota + 1
)

type ParseDiagnostic struct {
	Severity  DiagnosticSeverity
	Code      string
	Message   string
	Detail    string
	Hint      string
	Funcname  string
	Filename  string
	Location  int
	Cursorpos int
}

// TemporalClause locations are zero-based byte offsets into the original input.
// RelationLocation is -1 for SET TRANSACTION; otherwise it identifies a RangeVar.
// StatementIndex indexes ParseResult.Stmts, excluding empty statements.
type TemporalClause struct {
	StatementIndex   int
	RelationLocation int32
	Expression       *pg_query.Node
	Location         int32
}

type OnlineDDLClause struct {
	StatementIndex int
	Location       int32
}

// ExtendedParseResult owns its AST and sidecars. The ParseResult alone is not a
// complete statement identity: consumers must retain the extension expressions.
type ExtendedParseResult struct {
	ParseResult      *pg_query.ParseResult
	TemporalClauses  []TemporalClause
	OnlineDDLClauses []OnlineDDLClause
	LexicalPolicy    LexicalPolicy
	Diagnostics      []ParseDiagnostic
}

type extensionState struct {
	options       ParseOptions
	lexicalPolicy LexicalPolicy
	temporal      []TemporalClause
	online        []OnlineDDLClause
	diagnostics   []ParseDiagnostic
}

func ParseExtended(input string, options ParseOptions) (*ExtendedParseResult, error) {
	policy, err := resolveLexicalPolicy(options)
	if err != nil {
		return nil, err
	}
	lex := newLexer(input)
	lex.extensions.options = options
	lex.extensions.lexicalPolicy = policy
	yyParse(lex)
	if lex.err != nil {
		return nil, lex.err
	}
	return &ExtendedParseResult{
		ParseResult:      lex.parseResult(),
		TemporalClauses:  lex.extensions.temporal,
		OnlineDDLClauses: lex.extensions.online,
		LexicalPolicy:    policy,
		Diagnostics:      append([]ParseDiagnostic(nil), lex.extensions.diagnostics...),
	}, nil
}

func resolveLexicalPolicy(options ParseOptions) (LexicalPolicy, error) {
	standard, err := options.StandardConformingStrings.value(true, "StandardConformingStrings")
	if err != nil {
		return LexicalPolicy{}, err
	}
	warnings, err := options.EscapeStringWarning.value(true, "EscapeStringWarning")
	if err != nil {
		return LexicalPolicy{}, err
	}
	backslashQuote := options.BackslashQuote
	if backslashQuote == BackslashQuoteDefault {
		backslashQuote = BackslashQuoteSafeEncoding
	}
	if backslashQuote < BackslashQuoteOn || backslashQuote > BackslashQuoteSafeEncoding {
		return LexicalPolicy{}, invalidParseOption("BackslashQuote")
	}
	return LexicalPolicy{
		StandardConformingStrings: standard,
		EscapeStringWarning:       warnings,
		BackslashQuote:            backslashQuote,
	}, nil
}

func (option BooleanOption) value(defaultValue bool, name string) (bool, error) {
	switch option {
	case BooleanDefault:
		return defaultValue, nil
	case BooleanTrue:
		return true, nil
	case BooleanFalse:
		return false, nil
	default:
		return false, invalidParseOption(name)
	}
}

func invalidParseOption(name string) error {
	return &Error{Code: "22023", Message: "invalid ParseOptions." + name, Location: -1}
}

func (l *Lexer) lexicalPolicy() LexicalPolicy {
	policy := l.extensions.lexicalPolicy
	if policy.BackslashQuote == BackslashQuoteDefault {
		policy.StandardConformingStrings = true
		policy.EscapeStringWarning = true
		policy.BackslashQuote = BackslashQuoteSafeEncoding
	}
	return policy
}

func (l *Lexer) warningAt(message, hint, funcname string, location int) {
	location = max(0, min(location, len(l.data)))
	l.extensions.diagnostics = append(l.extensions.diagnostics, ParseDiagnostic{
		Severity:  SeverityWarning,
		Code:      "22P06",
		Message:   message,
		Hint:      hint,
		Funcname:  funcname,
		Filename:  "scan.l",
		Location:  location,
		Cursorpos: utf8.RuneCountInString(l.data[:location]) + 1,
	})
}

func (l *Lexer) recordTemporal(relationLocation int32, expression Node, location int) bool {
	if !l.extensions.options.AllowSystemTime {
		l.errorAt("AS OF SYSTEM TIME requires AllowSystemTime", location)
		return false
	}
	l.extensions.temporal = append(l.extensions.temporal, TemporalClause{
		StatementIndex:   len(l.statementLocations),
		RelationLocation: relationLocation,
		Expression:       expression,
		Location:         int32(location),
	})
	return true
}

func (l *Lexer) recordOnline(identifier string, location int) bool {
	// ONLINE stays an IDENT so PostgreSQL identifier uses remain unchanged.
	// Quoted identifiers cannot act as markers, even when their value is online.
	if identifier != "online" || location+len("online") > len(l.data) || !strings.EqualFold(l.data[location:location+len("online")], "online") {
		l.errorAt("syntax error", location)
		return false
	}
	if !l.extensions.options.AllowOnlineDDL {
		l.errorAt("ONLINE requires AllowOnlineDDL", location)
		return false
	}
	l.extensions.online = append(l.extensions.online, OnlineDDLClause{
		StatementIndex: len(l.statementLocations),
		Location:       int32(location),
	})
	return true
}
