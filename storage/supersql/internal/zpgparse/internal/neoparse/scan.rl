package neoparse

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	pg_query "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/pgproto"
)

%%{
    machine pg_scanner;
    alphtype byte;

    newline     = '\r'? '\n';
    whitespace  = [ \t\r\n\f\v]+;
    linecomment = '--' [^\r\n]*;

    ident_start = [a-zA-Z_] | ( 0x80..0xFF );
    ident_cont  = [a-zA-Z_0-9] | 0x24 | ( 0x80..0xFF );
    ident       = ident_start ident_cont*;

    qident      = '"' ( [^"] | '""' )* '"';
    uqident     = [uU] '&"' ( [^"] | '""' )* '"';

    param       = 0x24 [0-9]+;

    decdigit    = [0-9];
    hexdigit    = [0-9a-fA-F];
    octdigit    = [0-7];
    bindigit    = [0-1];

    decinteger  = decdigit ('_'? decdigit)*;
    hexinteger  = '0' [xX] ('_'? hexdigit)+;
    octinteger  = '0' [oO] ('_'? octdigit)+;
    bininteger  = '0' [bB] ('_'? bindigit)+;

    numeric     = (decinteger '.' decinteger?) | ('.' decinteger);
    numericfail = decinteger '..';
    real        = (decinteger | numeric) [eE] [+\-]? decinteger;
    realfail    = (decinteger | numeric) [eE] [+\-];
    numericjunk = (decinteger | numeric | real) ident;

    bconst      = [bB] '\'';
    xconst      = [xX] '\'';
    usconst     = [uU] '&' '\'';

    op_chars    = [\~\!\@\#\^\&\|\`\?\+\-\*\/\%\<\>\=];
    operator    = op_chars+;

    main := |*
        whitespace => {
            // whitespace ignored
        };

        linecomment => {
            tok = SQL_COMMENT
            tokStart = int32(ts)
            tokEnd = int32(te)
            keywordKind = KeywordKind_NO_KEYWORD
            fbreak;
        };

        '/*' => {
            i, closed := nestedCommentEnd(data, ts)
            if !closed {
                l.errorAt("unterminated /* comment", ts)
                fbreak;
            }
            tok = C_COMMENT
            tokStart = int32(ts)
            tokEnd = int32(i)
            p = i - 1
            keywordKind = KeywordKind_NO_KEYWORD
            fbreak;
        };

        '::' => {
            tok = TYPECAST
            tokStart = int32(ts)
            tokEnd = int32(te)
            fbreak;
        };

        '..' => {
            tok = DOT_DOT
            tokStart = int32(ts)
            tokEnd = int32(te)
            fbreak;
        };

        ':=' => {
            tok = COLON_EQUALS
            tokStart = int32(ts)
            tokEnd = int32(te)
            fbreak;
        };

        '=>' => {
            tok = EQUALS_GREATER
            tokStart = int32(ts)
            tokEnd = int32(te)
            fbreak;
        };

        '<=' => {
            tok = LESS_EQUALS
            tokStart = int32(ts)
            tokEnd = int32(te)
            fbreak;
        };

        '>=' => {
            tok = GREATER_EQUALS
            tokStart = int32(ts)
            tokEnd = int32(te)
            fbreak;
        };

        '!=' | '<>' => {
            tok = NOT_EQUALS
            tokStart = int32(ts)
            tokEnd = int32(te)
            fbreak;
        };

        param => {
            tok = PARAM
            tokStart = int32(ts)
            tokEnd = int32(te)
            numStr := data[ts+1 : te]
            n, err := strconv.ParseInt(numStr, 10, 32)
            if err != nil {
                l.errorAt("parameter number too large", ts)
                fbreak;
            }
            if lval != nil {
                lval.setint(n)
            }
            fbreak;
        };

        numericfail => {
            // Integer followed by '..'; emit integer and rewind 2 bytes
            tokStart = int32(ts)
            tokEnd = int32(te - 2)
            p = te - 3
            tok = integerToken(data[ts:te-2], 10, 0, lval)
            fbreak;
        };

        real => {
            tok = FCONST
            tokStart = int32(ts)
            tokEnd = int32(te)
            raw := data[ts:te]
            if lval != nil {
                lval.setstr(raw)
            }
            fbreak;
        };

        numeric => {
            tok = FCONST
            tokStart = int32(ts)
            tokEnd = int32(te)
            raw := data[ts:te]
            if lval != nil {
                lval.setstr(raw)
            }
            fbreak;
        };

        hexinteger => {
            tokStart = int32(ts)
            tokEnd = int32(te)
            tok = integerToken(data[ts:te], 16, 2, lval)
            fbreak;
        };

        octinteger => {
            tokStart = int32(ts)
            tokEnd = int32(te)
            tok = integerToken(data[ts:te], 8, 2, lval)
            fbreak;
        };

        bininteger => {
            tokStart = int32(ts)
            tokEnd = int32(te)
            tok = integerToken(data[ts:te], 2, 2, lval)
            fbreak;
        };

        decinteger => {
            tokStart = int32(ts)
            tokEnd = int32(te)
            tok = integerToken(data[ts:te], 10, 0, lval)
            fbreak;
        };

        '0' [xX] '_'? | '0' [oO] '_'? | '0' [bB] '_'? => {
            l.errorAt("invalid non-decimal integer", ts)
            fbreak;
        };

        numericjunk | realfail => {
            l.errorAt("trailing junk after numeric literal", ts)
            fbreak;
        };


        [nN] '\'' => {
            tok = NCHAR
            tokStart = int32(ts)
            tokEnd = int32(ts + 1)
            keywordKind = KeywordKind_COL_NAME_KEYWORD
            p = ts
            if lval != nil {
                lval.setstr("nchar")
            }
            fbreak;
        };

        bconst | xconst => {
            tok = BCONST
            prefix := "b"
            if data[ts] == 'x' || data[ts] == 'X' {
                tok = XCONST
                prefix = "x"
            }
            value, end, err := scanQuotedLiteral(data, te-1, literalBits)
            if err != nil {
                l.errorAt(err.Error(), ts)
                fbreak;
            }
            tokStart = int32(ts)
            tokEnd = int32(end)
            p = end - 1
            if lval != nil {
                lval.setstr(prefix + value)
            }
            fbreak;
        };

        usconst => {
            if !l.lexicalPolicy().StandardConformingStrings {
                l.setError("0A000",
                    "unsafe use of string constant with Unicode escapes",
                    "String constants with Unicode escapes cannot be used when \"standard_conforming_strings\" is off.",
                    "", ts, "scanner_yyerror", "scan.l")
                fbreak;
            }
            value, end, err := l.scanQuotedLiteral(te-1, literalStandard, ts)
            if err != nil {
                l.setLiteralError(err, ts)
                fbreak;
            }
            tok = USCONST
            tokStart = int32(ts)
            tokEnd = int32(end)
            p = end - 1
            if lval != nil {
                lval.setstr(value)
            }
            fbreak;
        };

        # Dollar-quoted strings: $tag$...$tag$ or $$...$$
        0x24 ( ident_start (ident_start | [0-9])* )? 0x24 => {
            tag := data[ts:te]
            // search for closing tag
            endIdx := strings.Index(data[te:], tag)
            if endIdx >= 0 {
                fullEnd := te + endIdx + len(tag)
                if !utf8.ValidString(data[te:te+endIdx]) {
                    l.errorAt("invalid UTF-8 string", ts)
                    fbreak;
                }
                tok = SCONST
                tokStart = int32(ts)
                tokEnd = int32(fullEnd)
                p = fullEnd - 1
                if lval != nil {
                    lval.setstr(data[te : te+endIdx])
                }
                fbreak;
            }
            l.errorAt("unterminated dollar-quoted string", ts)
            fbreak;
        };

        [eE]? '\'' => {
            mode := literalOrdinary
            if data[ts] != '\'' {
                mode = literalEscape
            }
            value, end, err := l.scanQuotedLiteral(te-1, mode, ts)
            if err != nil {
                l.setLiteralError(err, ts)
                fbreak;
            }
            tok = SCONST
            tokStart = int32(ts)
            tokEnd = int32(end)
            p = end - 1
            if lval != nil {
                lval.setstr(value)
            }
            fbreak;
        };

        uqident => {
            tok = UIDENT
            tokStart = int32(ts)
            tokEnd = int32(te)
            if lval != nil {
                lval.setstr(unquoteIdent(data[ts:te]))
            }
            fbreak;
        };

        qident => {
            value := unquoteIdent(data[ts:te])
            if value == "" {
                l.errorAt("zero-length delimited identifier", ts)
                fbreak;
            }
            if !utf8.ValidString(value) {
                l.errorAt("invalid UTF-8 identifier", ts)
                fbreak;
            }
            tok = IDENT
            tokStart = int32(ts)
            tokEnd = int32(te)
            if lval != nil {
                lval.setstr(truncateIdentifier(value))
            }
            fbreak;
        };

        ident => {
            word := data[ts:te]
            if !utf8.ValidString(word) {
                l.errorAt("invalid UTF-8 identifier", ts)
                fbreak;
            }
            tokStart = int32(ts)
            tokEnd = int32(te)
            if kw, ok := LookupKeyword(word); ok {
                tok = kw.Token
                keywordKind = kw.KeywordKind
                if lval != nil {
                    lval.setstr(strings.ToLower(word))
                }
            } else {
                tok = IDENT
                keywordKind = KeywordKind_NO_KEYWORD
                if lval != nil {
                    lval.setstr(truncateIdentifier(asciiDowncase(word)))
                }
            }
            fbreak;
        };

        operator => {
            // Check for embedded comment starts: /* or --
            nchars := te - ts
            rawOp := data[ts:te]

            if strings.HasPrefix(rawOp, "/*") {
                i, closed := nestedCommentEnd(data, ts)
                if !closed {
                    l.errorAt("unterminated /* comment", ts)
                    fbreak;
                }
                tok = C_COMMENT
                tokStart = int32(ts)
                tokEnd = int32(i)
                p = i - 1
                keywordKind = KeywordKind_NO_KEYWORD
                fbreak;
            }
            if strings.HasPrefix(rawOp, "--") {
                i := ts + 2
                for i < len(data) && data[i] != '\n' && data[i] != '\r' {
                    i++
                }
                tok = SQL_COMMENT
                tokStart = int32(ts)
                tokEnd = int32(i)
                p = i - 1
                keywordKind = KeywordKind_NO_KEYWORD
                fbreak;
            }

            rawOp = operatorPrefix(rawOp)
            nchars = len(rawOp)

            if nchars >= 64 {
                l.Error("operator too long")
                fbreak;
            }
            tokStart = int32(ts)
            tokEnd = int32(ts + nchars)
            p = ts + nchars - 1

            tok = operatorToken(rawOp, lval)
            fbreak;
        };

        any => {
            if data[ts] == '"' {
                l.errorAt("unterminated quoted identifier", ts)
                fbreak;
            }
            tok = int(data[ts])
            tokStart = int32(ts)
            tokEnd = int32(ts + 1)
            fbreak;
        };
    *|;

    write data;
}%%

type Lexer struct {
	data        string
	p           int
	pe          int
	eof         int
	cs          int
	ts          int
	te          int
	act         int
	statements  []Statement
	statementLocations []statementLocation
	tokenStart int
	tokenEnd int
	initialToken int
	err         error
	scanMode    bool
	extensions extensionState

	haveLookahead bool
	lookaheadTok  int
	lookaheadVal  yySymType
}

func NewLexer(input string) *Lexer {
	l := &Lexer{
		data: input,
		pe:   len(input),
		eof:  len(input),
	}
	var cs, ts, te, act int
	%% write init;
	l.cs = cs
	l.ts = ts
	l.te = te
	l.act = act
	return l
}

var newLexer = NewLexer
type lexer = Lexer

func (l *Lexer) scanToken(lval *yySymType) (tok int, tokStart, tokEnd int32, keywordKind KeywordKind) {
	data := l.data
	p := l.p
	pe := l.pe
	eof := l.eof
	cs := l.cs
	ts := l.ts
	te := l.te
	act := l.act

	%% write exec;

	l.p = p
	l.cs = cs
	l.ts = ts
	l.te = te
	l.act = act
	return
}

func (l *Lexer) rawLex(lval *yySymType) int {
	if l.err != nil {
		return 0
	}
	for l.p < l.pe {
		oldP := l.p
		tok, tokStart, _, _ := l.scanToken(lval)
		if l.err != nil {
			return 0
		}
		if tok != 0 {
			lval.yyloc = int(tokStart)
			if !l.scanMode && (tok == SQL_COMMENT || tok == C_COMMENT) {
				continue
			}
			return tok
		}
		if l.p <= oldP {
			l.p = oldP + 1
		}
	}
	lval.yyloc = len(l.data)
	return 0
}

func (l *Lexer) Lex(lval *yySymType) int {
	if l.initialToken != 0 {
		token := l.initialToken
		l.initialToken = 0
		lval.yyloc = 0
		return token
	}
	var curTok int
	if l.haveLookahead {
		curTok = l.lookaheadTok
		if lval != nil {
			*lval = l.lookaheadVal
		}
		l.haveLookahead = false
	} else {
		curTok = l.rawLex(lval)
	}
	l.tokenStart = lval.yyloc
	l.tokenEnd = l.te

	if curTok == 0 {
		return 0
	}

	if l.scanMode {
		return curTok
	}

	switch curTok {
	case NOT:
		var nextVal yySymType
		nextTok := l.rawLex(&nextVal)
		l.haveLookahead = true
		l.lookaheadTok = nextTok
		l.lookaheadVal = nextVal

		switch nextTok {
		case BETWEEN, IN_P, LIKE, ILIKE, SIMILAR:
			return NOT_LA
		}

	case NULLS_P:
		var nextVal yySymType
		nextTok := l.rawLex(&nextVal)
		l.haveLookahead = true
		l.lookaheadTok = nextTok
		l.lookaheadVal = nextVal

		switch nextTok {
		case FIRST_P, LAST_P:
			return NULLS_LA
		}

	case WITH:
		var nextVal yySymType
		nextTok := l.rawLex(&nextVal)
		l.haveLookahead = true
		l.lookaheadTok = nextTok
		l.lookaheadVal = nextVal

		switch nextTok {
		case TIME, ORDINALITY:
			return WITH_LA
		}

	case WITHOUT:
		var nextVal yySymType
		nextTok := l.rawLex(&nextVal)
		l.haveLookahead = true
		l.lookaheadTok = nextTok
		l.lookaheadVal = nextVal

		switch nextTok {
		case TIME:
			return WITHOUT_LA
		}

	case FORMAT:
		var nextVal yySymType
		nextTok := l.rawLex(&nextVal)
		l.haveLookahead = true
		l.lookaheadTok = nextTok
		l.lookaheadVal = nextVal

		switch nextTok {
		case JSON:
			return FORMAT_LA
		}

	case USCONST, UIDENT:
		escape := byte('\\')
		var nextVal yySymType
		nextTok := l.rawLex(&nextVal)
		if nextTok == UESCAPE {
			var escVal yySymType
			if l.rawLex(&escVal) != SCONST {
				l.Error("UESCAPE must be followed by a simple string literal")
				return 0
			}
			esc := escVal.str()
			if len(esc) != 1 || isUnicodeHexDigit(esc[0]) || strings.ContainsRune("+\"' \t\n\r\f\v", rune(esc[0])) {
				l.Error("invalid Unicode escape character")
				return 0
			}
			escape = esc[0]
		} else {
			l.haveLookahead = true
			l.lookaheadTok = nextTok
			l.lookaheadVal = nextVal
		}
		decoded, err := decodeUnicodeEscapesAt(lval.str(), escape, l.tokenStart)
		if err != nil {
			l.setLiteralError(err, l.tokenStart)
			return 0
		}
		if curTok == UIDENT && decoded == "" {
			l.Error("zero-length delimited identifier")
			return 0
		}
		if curTok == UIDENT {
			decoded = truncateIdentifier(decoded)
		}
		lval.setstr(decoded)
		if curTok == UIDENT {
			return IDENT
		}
		return SCONST
	}

	return curTok
}

func (l *Lexer) Error(s string) {
	l.errorAt(s, l.tokenStart)
}

func (l *Lexer) errorAt(message string, location int) {
	filename, funcname := "gram.y", "parser_yyerror"
	if message == "syntax error" || strings.HasPrefix(message, "syntax error:") {
		filename, funcname = "scan.l", "scanner_yyerror"
		location = max(0, min(location, len(l.data)))
		if location == len(l.data) {
			message = "syntax error at end of input"
		} else {
			end := max(location+1, min(l.tokenEnd, len(l.data)))
			message = fmt.Sprintf("syntax error at or near %q", l.data[location:end])
		}
	}
	l.setError("42601", message, "", "", location, funcname, filename)
}

func (l *Lexer) setError(code, message, detail, hint string, location int, funcname, filename string) {
	if l.err != nil {
		return
	}
	location = max(0, min(location, len(l.data)))
	l.err = &Error{
		Code:      code,
		Message:   message,
		Detail:    detail,
		Hint:      hint,
		Funcname:  funcname,
		Filename:  filename,
		Cursorpos: utf8.RuneCountInString(l.data[:location]) + 1,
		Location:  location,
	}
}

func (l *Lexer) setLiteralError(err error, fallbackLocation int) {
	issue, ok := err.(*literalError)
	if !ok {
		l.errorAt(err.Error(), fallbackLocation)
		return
	}
	location := issue.location
	if location < 0 {
		location = fallbackLocation
	}
	l.setError(issue.code, issue.message, issue.detail, issue.hint, location, "scanner_yyerror", "scan.l")
}

// Scan parses the SQL string into tokens matching PostgreSQL's scan.l and pg_query.Scan.
func Scan(input string) (*ScanResult, error) {
	l := NewLexer(input)
	l.scanMode = true

	var tokens []*ScanToken
	for l.p < l.pe {
		tok, tokStart, tokEnd, keywordKind := l.scanToken(nil)

		if l.err != nil {
			return nil, l.err
		}
		if tok != 0 {
			tokens = append(tokens, &ScanToken{
				Start:       tokStart,
				End:         tokEnd,
				Token:       pg_query.Token(tok),
				KeywordKind: pg_query.KeywordKind(keywordKind),
			})
		}
	}

	return &ScanResult{
		Version: 180004,
		Tokens:  tokens,
	}, nil
}

func unquoteIdent(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return strings.ReplaceAll(s[1:len(s)-1], `""`, `"`)
	}
	if unicodeQuotedIdentifier(s) {
		return strings.ReplaceAll(s[3:len(s)-1], `""`, `"`)
	}
	return s
}

var (
	errInvalidUnicodeEscape = errors.New("invalid Unicode escape")
	errInvalidUnicodeValue = errors.New("invalid Unicode escape value")
	errInvalidUnicodePair = errors.New("invalid Unicode surrogate pair")
)

func isUnicodeHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func decodeUnicodeEscapes(s string, escape byte) (string, error) {
	return decodeUnicodeEscapesAt(s, escape, 0)
}

func decodeUnicodeEscapesAt(s string, escape byte, tokenStart int) (string, error) {
	if issue := invalidStringEncoding(s, tokenStart); issue != nil {
		return "", issue
	}
	if strings.IndexByte(s, escape) < 0 {
		return s, nil
	}
	var decoded strings.Builder
	decoded.Grow(len(s))
	var highSurrogate rune
	for i := 0; i < len(s); {
		if s[i] != escape {
			if highSurrogate != 0 {
				return "", &literalError{
					code: "42601", message: "invalid Unicode surrogate pair",
					location: tokenStart + 3 + i,
				}
			}
			decoded.WriteByte(s[i])
			i++
			continue
		}
		escapeLocation := tokenStart + 3 + i
		i++
		if i < len(s) && s[i] == escape {
			if highSurrogate != 0 {
				return "", &literalError{
					code: "42601", message: "invalid Unicode surrogate pair",
					location: escapeLocation,
				}
			}
			decoded.WriteByte(escape)
			i++
			continue
		}
		digits := 4
		if i < len(s) && s[i] == '+' {
			digits = 6
			i++
		}
		if len(s)-i < digits {
			return "", unicodeQuotedEscapeError(errInvalidUnicodeEscape, escapeLocation)
		}
		var value rune
		for j := range digits {
			c := s[i+j]
			if !isUnicodeHexDigit(c) {
				return "", unicodeQuotedEscapeError(errInvalidUnicodeEscape, escapeLocation)
			}
			value <<= 4
			switch {
			case c <= '9':
				value += rune(c - '0')
			case c <= 'F':
				value += rune(c - 'A' + 10)
			default:
				value += rune(c - 'a' + 10)
			}
		}
		i += digits
		if value == 0 || value > utf8.MaxRune {
			return "", unicodeQuotedEscapeError(errInvalidUnicodeValue, escapeLocation)
		}
		if highSurrogate != 0 {
			if value < 0xDC00 || value > 0xDFFF {
				return "", unicodeQuotedEscapeError(errInvalidUnicodePair, escapeLocation)
			}
			value = utf16.DecodeRune(highSurrogate, value)
			highSurrogate = 0
		} else if value >= 0xD800 && value <= 0xDBFF {
			highSurrogate = value
			continue
		}
		if value >= 0xDC00 && value <= 0xDFFF {
			return "", unicodeQuotedEscapeError(errInvalidUnicodePair, escapeLocation)
		}
		decoded.WriteRune(value)
	}
	if highSurrogate != 0 {
		return "", unicodeQuotedEscapeError(errInvalidUnicodePair, tokenStart+3+len(s))
	}
	return decoded.String(), nil
}

func unicodeQuotedEscapeError(err error, location int) *literalError {
	if err == errInvalidUnicodeEscape {
		return &literalError{
			code:     "42601",
			message:  err.Error(),
			hint:     "Unicode escapes must be \\XXXX or \\+XXXXXX.",
			location: location,
		}
	}
	return &literalError{code: "42601", message: err.Error(), location: location}
}

type literalMode uint8

const (
	literalStandard literalMode = iota
	literalEscape
	literalBits
	literalOrdinary
)

type literalError struct {
	code     string
	message  string
	detail   string
	hint     string
	location int
}

func (e *literalError) Error() string {
	return e.message
}

func scanQuotedLiteral(data string, quote int, mode literalMode) (string, int, error) {
	policy := LexicalPolicy{
		StandardConformingStrings: true,
		EscapeStringWarning:       true,
		BackslashQuote:            BackslashQuoteSafeEncoding,
	}
	return scanQuotedLiteralWithPolicy(nil, data, quote, mode, quote, policy)
}

func (l *Lexer) scanQuotedLiteral(quote int, mode literalMode, tokenStart int) (string, int, error) {
	return scanQuotedLiteralWithPolicy(l, l.data, quote, mode, tokenStart, l.lexicalPolicy())
}

func scanQuotedLiteralWithPolicy(l *Lexer, data string, quote int, mode literalMode, tokenStart int, policy LexicalPolicy) (string, int, error) {
	escapeMode := mode == literalEscape
	warnEscapes := false
	if mode == literalOrdinary {
		escapeMode = !policy.StandardConformingStrings
		warnEscapes = escapeMode && policy.EscapeStringWarning
	}

	var value strings.Builder
	segment := quote + 1
	warned := false
	for i := segment; i < len(data); {
		if data[i] == '\'' {
			if mode != literalBits && i+1 < len(data) && data[i+1] == '\'' {
				value.WriteString(data[segment:i+1])
				i += 2
				segment = i
				continue
			}
			if next := continuedQuote(data, i+1); next >= 0 {
				value.WriteString(data[segment:i])
				i = next + 1
				segment = i
				continue
			}
			text := data[segment:i]
			if value.Len() != 0 {
				value.WriteString(text)
				text = value.String()
			}
			if issue := invalidStringEncoding(text, tokenStart); issue != nil {
				return "", i + 1, issue
			}
			return text, i + 1, nil
		}
		if escapeMode && data[i] == '\\' {
			value.WriteString(data[segment:i])
			// UTF8 is not a PostgreSQL client-only encoding, so safe_encoding permits this escape.
			if i+1 < len(data) && data[i+1] == '\'' && policy.BackslashQuote == BackslashQuoteOff {
				return "", i + 2, &literalError{
					code:     "22P06",
					message:  "unsafe use of \\' in a string literal",
					hint:     "Use '' to write quotes in strings. \\' is insecure in client-only encodings.",
					location: tokenStart,
				}
			}
			if warnEscapes && !warned && i+1 < len(data) {
				l.warnStringEscape(data[i+1], tokenStart)
				warned = true
			}
			next, err := appendStringEscape(&value, data, i)
			if err != nil {
				return "", next, err
			}
			i = next
			segment = i
			continue
		}
		i++
	}
	return "", len(data), &literalError{code: "42601", message: "unterminated quoted string", location: tokenStart}
}

func (l *Lexer) warnStringEscape(escaped byte, tokenStart int) {
	if l == nil {
		return
	}
	switch escaped {
	case '\'':
		l.warningAt(
			"nonstandard use of \\' in a string literal",
			"Use '' to write quotes in strings, or use the escape string syntax (E'...').",
			"check_string_escape_warning", tokenStart)
	case '\\':
		l.warningAt(
			"nonstandard use of \\\\ in a string literal",
			"Use the escape string syntax for backslashes, e.g., E'\\\\'.",
			"check_string_escape_warning", tokenStart)
	default:
		l.warningAt(
			"nonstandard use of escape in a string literal",
			"Use the escape string syntax for escapes, e.g., E'\\r\\n'.",
			"check_escape_warning", tokenStart)
	}
}

func invalidStringEncoding(value string, tokenStart int) *literalError {
	for position := 0; position < len(value); {
		if value[position] == 0 {
			return invalidUTF8Error(value[position:position+1], tokenStart)
		}
		r, size := utf8.DecodeRuneInString(value[position:])
		if r == utf8.RuneError && size == 1 {
			width := invalidUTF8SequenceWidth(value[position])
			end := min(len(value), position+width)
			return invalidUTF8Error(value[position:end], tokenStart)
		}
		position += size
	}
	return nil
}

func invalidUTF8SequenceWidth(first byte) int {
	switch {
	case first >= 0xC0 && first < 0xE0:
		return 2
	case first >= 0xE0 && first < 0xF0:
		return 3
	case first >= 0xF0 && first < 0xF8:
		return 4
	default:
		return 1
	}
}

func invalidUTF8Error(sequence string, tokenStart int) *literalError {
	encoded := make([]string, len(sequence))
	for i := range sequence {
		encoded[i] = fmt.Sprintf("0x%02x", sequence[i])
	}
	return &literalError{
		code:     "22021",
		message:  "invalid byte sequence for encoding \"UTF8\": " + strings.Join(encoded, " "),
		location: tokenStart,
	}
}

func continuedQuote(data string, pos int) int {
	hasNewline := false
	for pos < len(data) {
		switch data[pos] {
		case ' ', '\t', '\f', '\v':
			pos++
		case '\n', '\r':
			hasNewline = true
			pos++
		case '-':
			if pos+1 >= len(data) || data[pos+1] != '-' {
				return -1
			}
			pos += 2
			for pos < len(data) && data[pos] != '\n' && data[pos] != '\r' {
				pos++
			}
		default:
			if hasNewline && data[pos] == '\'' {
				return pos
			}
			return -1
		}
	}
	return -1
}

func appendStringEscape(dst *strings.Builder, data string, slash int) (int, error) {
	pos := slash + 1
	if pos >= len(data) {
		return pos, &literalError{code: "42601", message: "unterminated quoted string", location: -1}
	}
	c := data[pos]
	pos++
	if c == 'u' || c == 'U' {
		value, next, err := stringUnicodeEscape(data, slash)
		if err != nil {
			return next, stringEscapeError(err, slash)
		}
		if value >= 0xD800 && value <= 0xDBFF {
			if !unicodeEscapeAt(data, next) {
				return next, &literalError{code: "42601", message: "invalid Unicode surrogate pair", location: next}
			}
			second, end, err := stringUnicodeEscape(data, next)
			if err != nil {
				return end, stringEscapeError(err, next)
			}
			if second < 0xDC00 || second > 0xDFFF {
				return end, &literalError{code: "42601", message: "invalid Unicode surrogate pair", location: next}
			}
			value = utf16.DecodeRune(value, second)
			next = end
		}
		if value >= 0xDC00 && value <= 0xDFFF {
			return next, &literalError{code: "42601", message: "invalid Unicode surrogate pair", location: slash}
		}
		dst.WriteRune(value)
		return next, nil
	}
	if c >= '0' && c <= '7' {
		value := c - '0'
		for count := 1; count < 3 && pos < len(data) && data[pos] >= '0' && data[pos] <= '7'; count++ {
			value = value*8 + data[pos] - '0'
			pos++
		}
		dst.WriteByte(value)
		return pos, nil
	}
	if c == 'x' && pos < len(data) && isUnicodeHexDigit(data[pos]) {
		start := pos
		pos++
		if pos < len(data) && isUnicodeHexDigit(data[pos]) {
			pos++
		}
		value, _ := strconv.ParseUint(data[start:pos], 16, 8)
		dst.WriteByte(byte(value))
		return pos, nil
	}
	switch c {
	case 'b':
		c = '\b'
	case 'f':
		c = '\f'
	case 'n':
		c = '\n'
	case 'r':
		c = '\r'
	case 't':
		c = '\t'
	case 'v':
		c = '\v'
	}
	dst.WriteByte(c)
	return pos, nil
}

func stringEscapeError(err error, location int) *literalError {
	switch err {
	case errInvalidUnicodeEscape:
		return &literalError{
			code:     "22025",
			message:  "invalid Unicode escape",
			hint:     "Unicode escapes must be \\uXXXX or \\UXXXXXXXX.",
			location: location,
		}
	case errInvalidUnicodeValue:
		return &literalError{code: "42601", message: err.Error(), location: location}
	case errInvalidUnicodePair:
		return &literalError{code: "42601", message: err.Error(), location: location}
	default:
		return &literalError{code: "42601", message: err.Error(), location: location}
	}
}

func stringUnicodeEscape(data string, slash int) (rune, int, error) {
	digits := 4
	if data[slash+1] == 'U' {
		digits = 8
	}
	end := slash + 2 + digits
	if end > len(data) {
		return 0, len(data), errInvalidUnicodeEscape
	}
	for i := slash + 2; i < end; i++ {
		if !isUnicodeHexDigit(data[i]) {
			return 0, i, errInvalidUnicodeEscape
		}
	}
	value, err := strconv.ParseUint(data[slash+2:end], 16, 32)
	if err != nil || value == 0 || value > utf8.MaxRune {
		return 0, end, errInvalidUnicodeValue
	}
	return rune(value), end, nil
}

func truncateIdentifier(value string) string {
	if len(value) <= 63 {
		return value
	}
	end := 63
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func asciiDowncase(value string) string {
	first := strings.IndexFunc(value, func(c rune) bool { return c >= 'A' && c <= 'Z' })
	if first < 0 {
		return value
	}
	lower := []byte(value)
	for i := first; i < len(lower); i++ {
		if lower[i] >= 'A' && lower[i] <= 'Z' {
			lower[i] += 'a' - 'A'
		}
	}
	return string(lower)
}
