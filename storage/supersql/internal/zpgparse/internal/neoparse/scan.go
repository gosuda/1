//line scan.rl:1
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

//line scan.go:18
const pg_scanner_start int = 4
const pg_scanner_first_final int = 4
const pg_scanner_error int = -1

const pg_scanner_en_main int = 4

//line scan.rl:428

type Lexer struct {
	data               string
	p                  int
	pe                 int
	eof                int
	cs                 int
	ts                 int
	te                 int
	act                int
	statements         []Statement
	statementLocations []statementLocation
	tokenStart         int
	tokenEnd           int
	initialToken       int
	err                error
	scanMode           bool
	extensions         extensionState

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

//line scan.go:60
	{
		cs = pg_scanner_start
		ts = 0
		te = 0
		act = 0
	}

//line scan.rl:461
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

//line scan.go:90
	{
		if p == pe {
			goto _test_eof
		}
		switch cs {
		case 4:
			goto st_case_4
		case 5:
			goto st_case_5
		case 6:
			goto st_case_6
		case 7:
			goto st_case_7
		case 8:
			goto st_case_8
		case 0:
			goto st_case_0
		case 9:
			goto st_case_9
		case 10:
			goto st_case_10
		case 11:
			goto st_case_11
		case 1:
			goto st_case_1
		case 12:
			goto st_case_12
		case 13:
			goto st_case_13
		case 14:
			goto st_case_14
		case 15:
			goto st_case_15
		case 16:
			goto st_case_16
		case 17:
			goto st_case_17
		case 18:
			goto st_case_18
		case 19:
			goto st_case_19
		case 20:
			goto st_case_20
		case 21:
			goto st_case_21
		case 22:
			goto st_case_22
		case 23:
			goto st_case_23
		case 24:
			goto st_case_24
		case 25:
			goto st_case_25
		case 26:
			goto st_case_26
		case 27:
			goto st_case_27
		case 28:
			goto st_case_28
		case 29:
			goto st_case_29
		case 30:
			goto st_case_30
		case 31:
			goto st_case_31
		case 32:
			goto st_case_32
		case 33:
			goto st_case_33
		case 34:
			goto st_case_34
		case 35:
			goto st_case_35
		case 36:
			goto st_case_36
		case 37:
			goto st_case_37
		case 38:
			goto st_case_38
		case 39:
			goto st_case_39
		case 40:
			goto st_case_40
		case 41:
			goto st_case_41
		case 42:
			goto st_case_42
		case 43:
			goto st_case_43
		case 44:
			goto st_case_44
		case 45:
			goto st_case_45
		case 46:
			goto st_case_46
		case 47:
			goto st_case_47
		case 48:
			goto st_case_48
		case 2:
			goto st_case_2
		case 3:
			goto st_case_3
		case 49:
			goto st_case_49
		}
		goto st_out
	tr0:
//line NONE:1
		switch act {
		case 3:
			{
				p = (te) - 1

				i, closed := nestedCommentEnd(data, ts)
				if !closed {
					l.errorAt("unterminated /* comment", ts)
					{
						p++
						cs = 4
						goto _out
					}
				}
				tok = C_COMMENT
				tokStart = int32(ts)
				tokEnd = int32(i)
				p = i - 1
				keywordKind = KeywordKind_NO_KEYWORD
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 7:
			{
				p = (te) - 1

				tok = EQUALS_GREATER
				tokStart = int32(ts)
				tokEnd = int32(te)
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 8:
			{
				p = (te) - 1

				tok = LESS_EQUALS
				tokStart = int32(ts)
				tokEnd = int32(te)
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 9:
			{
				p = (te) - 1

				tok = GREATER_EQUALS
				tokStart = int32(ts)
				tokEnd = int32(te)
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 10:
			{
				p = (te) - 1

				tok = NOT_EQUALS
				tokStart = int32(ts)
				tokEnd = int32(te)
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 19:
			{
				p = (te) - 1

				l.errorAt("invalid non-decimal integer", ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 20:
			{
				p = (te) - 1

				l.errorAt("trailing junk after numeric literal", ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 26:
			{
				p = (te) - 1

				tok = UIDENT
				tokStart = int32(ts)
				tokEnd = int32(te)
				if lval != nil {
					lval.setstr(unquoteIdent(data[ts:te]))
				}
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 27:
			{
				p = (te) - 1

				value := unquoteIdent(data[ts:te])
				if value == "" {
					l.errorAt("zero-length delimited identifier", ts)
					{
						p++
						cs = 4
						goto _out
					}
				}
				if !utf8.ValidString(value) {
					l.errorAt("invalid UTF-8 identifier", ts)
					{
						p++
						cs = 4
						goto _out
					}
				}
				tok = IDENT
				tokStart = int32(ts)
				tokEnd = int32(te)
				if lval != nil {
					lval.setstr(truncateIdentifier(value))
				}
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 28:
			{
				p = (te) - 1

				word := data[ts:te]
				if !utf8.ValidString(word) {
					l.errorAt("invalid UTF-8 identifier", ts)
					{
						p++
						cs = 4
						goto _out
					}
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
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 29:
			{
				p = (te) - 1

				// Check for embedded comment starts: /* or --
				nchars := te - ts
				rawOp := data[ts:te]

				if strings.HasPrefix(rawOp, "/*") {
					i, closed := nestedCommentEnd(data, ts)
					if !closed {
						l.errorAt("unterminated /* comment", ts)
						{
							p++
							cs = 4
							goto _out
						}
					}
					tok = C_COMMENT
					tokStart = int32(ts)
					tokEnd = int32(i)
					p = i - 1
					keywordKind = KeywordKind_NO_KEYWORD
					{
						p++
						cs = 4
						goto _out
					}
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
					{
						p++
						cs = 4
						goto _out
					}
				}

				rawOp = operatorPrefix(rawOp)
				nchars = len(rawOp)

				if nchars >= 64 {
					l.Error("operator too long")
					{
						p++
						cs = 4
						goto _out
					}
				}
				tokStart = int32(ts)
				tokEnd = int32(ts + nchars)
				p = ts + nchars - 1

				tok = operatorToken(rawOp, lval)
				{
					p++
					cs = 4
					goto _out
				}
			}
		case 30:
			{
				p = (te) - 1

				if data[ts] == '"' {
					l.errorAt("unterminated quoted identifier", ts)
					{
						p++
						cs = 4
						goto _out
					}
				}
				tok = int(data[ts])
				tokStart = int32(ts)
				tokEnd = int32(ts + 1)
				{
					p++
					cs = 4
					goto _out
				}
			}
		}

		goto st4
	tr3:
//line scan.rl:415
		p = (te) - 1
		{
			if data[ts] == '"' {
				l.errorAt("unterminated quoted identifier", ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			tok = int(data[ts])
			tokStart = int32(ts)
			tokEnd = int32(ts + 1)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr4:
//line scan.rl:273
		te = p + 1
		{
			tag := data[ts:te]
			// search for closing tag
			endIdx := strings.Index(data[te:], tag)
			if endIdx >= 0 {
				fullEnd := te + endIdx + len(tag)
				if !utf8.ValidString(data[te : te+endIdx]) {
					l.errorAt("invalid UTF-8 string", ts)
					{
						p++
						cs = 4
						goto _out
					}
				}
				tok = SCONST
				tokStart = int32(ts)
				tokEnd = int32(fullEnd)
				p = fullEnd - 1
				if lval != nil {
					lval.setstr(data[te : te+endIdx])
				}
				{
					p++
					cs = 4
					goto _out
				}
			}
			l.errorAt("unterminated dollar-quoted string", ts)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr6:
//line scan.rl:345
		p = (te) - 1
		{
			word := data[ts:te]
			if !utf8.ValidString(word) {
				l.errorAt("invalid UTF-8 identifier", ts)
				{
					p++
					cs = 4
					goto _out
				}
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
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr8:
//line scan.rl:249
		te = p + 1
		{
			if !l.lexicalPolicy().StandardConformingStrings {
				l.setError("0A000",
					"unsafe use of string constant with Unicode escapes",
					"String constants with Unicode escapes cannot be used when \"standard_conforming_strings\" is off.",
					"", ts, "scanner_yyerror", "scan.l")
				{
					p++
					cs = 4
					goto _out
				}
			}
			value, end, err := l.scanQuotedLiteral(te-1, literalStandard, ts)
			if err != nil {
				l.setLiteralError(err, ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			tok = USCONST
			tokStart = int32(ts)
			tokEnd = int32(end)
			p = end - 1
			if lval != nil {
				lval.setstr(value)
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr10:
//line scan.rl:415
		te = p + 1
		{
			if data[ts] == '"' {
				l.errorAt("unterminated quoted identifier", ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			tok = int(data[ts])
			tokStart = int32(ts)
			tokEnd = int32(ts + 1)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr16:
//line scan.rl:296
		te = p + 1
		{
			mode := literalOrdinary
			if data[ts] != '\'' {
				mode = literalEscape
			}
			value, end, err := l.scanQuotedLiteral(te-1, mode, ts)
			if err != nil {
				l.setLiteralError(err, ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			tok = SCONST
			tokStart = int32(ts)
			tokEnd = int32(end)
			p = end - 1
			if lval != nil {
				lval.setstr(value)
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr31:
//line scan.rl:55
		te = p
		p--
		{
			// whitespace ignored
		}
		goto st4
	tr32:
//line scan.rl:369
		te = p
		p--
		{
			// Check for embedded comment starts: /* or --
			nchars := te - ts
			rawOp := data[ts:te]

			if strings.HasPrefix(rawOp, "/*") {
				i, closed := nestedCommentEnd(data, ts)
				if !closed {
					l.errorAt("unterminated /* comment", ts)
					{
						p++
						cs = 4
						goto _out
					}
				}
				tok = C_COMMENT
				tokStart = int32(ts)
				tokEnd = int32(i)
				p = i - 1
				keywordKind = KeywordKind_NO_KEYWORD
				{
					p++
					cs = 4
					goto _out
				}
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
				{
					p++
					cs = 4
					goto _out
				}
			}

			rawOp = operatorPrefix(rawOp)
			nchars = len(rawOp)

			if nchars >= 64 {
				l.Error("operator too long")
				{
					p++
					cs = 4
					goto _out
				}
			}
			tokStart = int32(ts)
			tokEnd = int32(ts + nchars)
			p = ts + nchars - 1

			tok = operatorToken(rawOp, lval)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr34:
//line scan.rl:415
		te = p
		p--
		{
			if data[ts] == '"' {
				l.errorAt("unterminated quoted identifier", ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			tok = int(data[ts])
			tokStart = int32(ts)
			tokEnd = int32(ts + 1)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr35:
//line scan.rl:326
		te = p
		p--
		{
			value := unquoteIdent(data[ts:te])
			if value == "" {
				l.errorAt("zero-length delimited identifier", ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			if !utf8.ValidString(value) {
				l.errorAt("invalid UTF-8 identifier", ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			tok = IDENT
			tokStart = int32(ts)
			tokEnd = int32(te)
			if lval != nil {
				lval.setstr(truncateIdentifier(value))
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr37:
//line scan.rl:130
		te = p
		p--
		{
			tok = PARAM
			tokStart = int32(ts)
			tokEnd = int32(te)
			numStr := data[ts+1 : te]
			n, err := strconv.ParseInt(numStr, 10, 32)
			if err != nil {
				l.errorAt("parameter number too large", ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			if lval != nil {
				lval.setint(n)
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr39:
//line scan.rl:59
		te = p
		p--
		{
			tok = SQL_COMMENT
			tokStart = int32(ts)
			tokEnd = int32(te)
			keywordKind = KeywordKind_NO_KEYWORD
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr41:
//line scan.rl:88
		te = p + 1
		{
			tok = DOT_DOT
			tokStart = int32(ts)
			tokEnd = int32(te)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr43:
//line scan.rl:166
		te = p
		p--
		{
			tok = FCONST
			tokStart = int32(ts)
			tokEnd = int32(te)
			raw := data[ts:te]
			if lval != nil {
				lval.setstr(raw)
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr47:
//line scan.rl:210
		te = p
		p--
		{
			l.errorAt("trailing junk after numeric literal", ts)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr51:
//line scan.rl:155
		te = p
		p--
		{
			tok = FCONST
			tokStart = int32(ts)
			tokEnd = int32(te)
			raw := data[ts:te]
			if lval != nil {
				lval.setstr(raw)
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr55:
//line scan.rl:198
		te = p
		p--
		{
			tokStart = int32(ts)
			tokEnd = int32(te)
			tok = integerToken(data[ts:te], 10, 0, lval)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr61:
//line scan.rl:146
		te = p + 1
		{
			// Integer followed by '..'; emit integer and rewind 2 bytes
			tokStart = int32(ts)
			tokEnd = int32(te - 2)
			p = te - 3
			tok = integerToken(data[ts:te-2], 10, 0, lval)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr63:
//line scan.rl:205
		te = p
		p--
		{
			l.errorAt("invalid non-decimal integer", ts)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr66:
//line scan.rl:191
		te = p
		p--
		{
			tokStart = int32(ts)
			tokEnd = int32(te)
			tok = integerToken(data[ts:te], 2, 2, lval)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr70:
//line scan.rl:184
		te = p
		p--
		{
			tokStart = int32(ts)
			tokEnd = int32(te)
			tok = integerToken(data[ts:te], 8, 2, lval)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr74:
//line scan.rl:177
		te = p
		p--
		{
			tokStart = int32(ts)
			tokEnd = int32(te)
			tok = integerToken(data[ts:te], 16, 2, lval)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr76:
//line scan.rl:81
		te = p + 1
		{
			tok = TYPECAST
			tokStart = int32(ts)
			tokEnd = int32(te)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr77:
//line scan.rl:95
		te = p + 1
		{
			tok = COLON_EQUALS
			tokStart = int32(ts)
			tokEnd = int32(te)
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr81:
//line scan.rl:345
		te = p
		p--
		{
			word := data[ts:te]
			if !utf8.ValidString(word) {
				l.errorAt("invalid UTF-8 identifier", ts)
				{
					p++
					cs = 4
					goto _out
				}
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
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr82:
//line scan.rl:228
		te = p + 1
		{
			tok = BCONST
			prefix := "b"
			if data[ts] == 'x' || data[ts] == 'X' {
				tok = XCONST
				prefix = "x"
			}
			value, end, err := scanQuotedLiteral(data, te-1, literalBits)
			if err != nil {
				l.errorAt(err.Error(), ts)
				{
					p++
					cs = 4
					goto _out
				}
			}
			tokStart = int32(ts)
			tokEnd = int32(end)
			p = end - 1
			if lval != nil {
				lval.setstr(prefix + value)
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr83:
//line scan.rl:216
		te = p + 1
		{
			tok = NCHAR
			tokStart = int32(ts)
			tokEnd = int32(ts + 1)
			keywordKind = KeywordKind_COL_NAME_KEYWORD
			p = ts
			if lval != nil {
				lval.setstr("nchar")
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	tr85:
//line scan.rl:316
		te = p
		p--
		{
			tok = UIDENT
			tokStart = int32(ts)
			tokEnd = int32(te)
			if lval != nil {
				lval.setstr(unquoteIdent(data[ts:te]))
			}
			{
				p++
				cs = 4
				goto _out
			}
		}
		goto st4
	st4:
//line NONE:1
		ts = 0

		if p++; p == pe {
			goto _test_eof4
		}
	st_case_4:
//line NONE:1
		ts = p

//line scan.go:862
		switch data[p] {
		case 32:
			goto st5
		case 33:
			goto st6
		case 34:
			goto tr13
		case 36:
			goto tr15
		case 39:
			goto tr16
		case 44:
			goto tr10
		case 45:
			goto st12
		case 46:
			goto st15
		case 47:
			goto st25
		case 48:
			goto st26
		case 58:
			goto st40
		case 59:
			goto tr10
		case 60:
			goto st41
		case 61:
			goto st42
		case 62:
			goto st43
		case 66:
			goto st45
		case 69:
			goto st46
		case 78:
			goto st47
		case 85:
			goto tr30
		case 88:
			goto st45
		case 94:
			goto tr14
		case 96:
			goto tr14
		case 98:
			goto st45
		case 101:
			goto st46
		case 110:
			goto st47
		case 117:
			goto tr30
		case 120:
			goto st45
		case 124:
			goto tr14
		case 126:
			goto tr14
		}
		switch {
		case data[p] < 40:
			switch {
			case data[p] < 9:
				if data[p] <= 8 {
					goto tr10
				}
			case data[p] > 13:
				switch {
				case data[p] > 31:
					if 35 <= data[p] && data[p] <= 38 {
						goto tr14
					}
				case data[p] >= 14:
					goto tr10
				}
			default:
				goto st5
			}
		case data[p] > 41:
			switch {
			case data[p] < 63:
				switch {
				case data[p] > 43:
					if 49 <= data[p] && data[p] <= 57 {
						goto st28
					}
				case data[p] >= 42:
					goto tr14
				}
			case data[p] > 64:
				switch {
				case data[p] > 93:
					if 123 <= data[p] && data[p] <= 127 {
						goto tr10
					}
				case data[p] >= 91:
					goto tr10
				}
			default:
				goto tr14
			}
		default:
			goto tr10
		}
		goto st44
	st5:
		if p++; p == pe {
			goto _test_eof5
		}
	st_case_5:
		if data[p] == 32 {
			goto st5
		}
		if 9 <= data[p] && data[p] <= 13 {
			goto st5
		}
		goto tr31
	st6:
		if p++; p == pe {
			goto _test_eof6
		}
	st_case_6:
		switch data[p] {
		case 33:
			goto tr14
		case 35:
			goto tr14
		case 45:
			goto tr14
		case 47:
			goto tr14
		case 61:
			goto tr33
		case 94:
			goto tr14
		case 96:
			goto tr14
		case 124:
			goto tr14
		case 126:
			goto tr14
		}
		switch {
		case data[p] < 42:
			if 37 <= data[p] && data[p] <= 38 {
				goto tr14
			}
		case data[p] > 43:
			if 60 <= data[p] && data[p] <= 64 {
				goto tr14
			}
		default:
			goto tr14
		}
		goto tr32
	tr14:
//line NONE:1
		te = p + 1

//line scan.rl:369
		act = 29
		goto st7
	tr33:
//line NONE:1
		te = p + 1

//line scan.rl:123
		act = 10
		goto st7
	tr54:
//line NONE:1
		te = p + 1

//line scan.rl:67
		act = 3
		goto st7
	tr78:
//line NONE:1
		te = p + 1

//line scan.rl:109
		act = 8
		goto st7
	tr79:
//line NONE:1
		te = p + 1

//line scan.rl:102
		act = 7
		goto st7
	tr80:
//line NONE:1
		te = p + 1

//line scan.rl:116
		act = 9
		goto st7
	st7:
		if p++; p == pe {
			goto _test_eof7
		}
	st_case_7:
//line scan.go:1066
		switch data[p] {
		case 33:
			goto tr14
		case 35:
			goto tr14
		case 45:
			goto tr14
		case 47:
			goto tr14
		case 94:
			goto tr14
		case 96:
			goto tr14
		case 124:
			goto tr14
		case 126:
			goto tr14
		}
		switch {
		case data[p] < 42:
			if 37 <= data[p] && data[p] <= 38 {
				goto tr14
			}
		case data[p] > 43:
			if 60 <= data[p] && data[p] <= 64 {
				goto tr14
			}
		default:
			goto tr14
		}
		goto tr0
	tr13:
//line NONE:1
		te = p + 1

//line scan.rl:415
		act = 30
		goto st8
	st8:
		if p++; p == pe {
			goto _test_eof8
		}
	st_case_8:
//line scan.go:1110
		if data[p] == 34 {
			goto tr2
		}
		goto st0
	st0:
		if p++; p == pe {
			goto _test_eof0
		}
	st_case_0:
		if data[p] == 34 {
			goto tr2
		}
		goto st0
	tr2:
//line NONE:1
		te = p + 1

//line scan.rl:326
		act = 27
		goto st9
	st9:
		if p++; p == pe {
			goto _test_eof9
		}
	st_case_9:
//line scan.go:1136
		if data[p] == 34 {
			goto st0
		}
		goto tr35
	tr15:
//line NONE:1
		te = p + 1

		goto st10
	st10:
		if p++; p == pe {
			goto _test_eof10
		}
	st_case_10:
//line scan.go:1151
		switch data[p] {
		case 36:
			goto tr4
		case 96:
			goto tr34
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st11
				}
			default:
				goto tr34
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr34
				}
			case data[p] >= 91:
				goto tr34
			}
		default:
			goto tr34
		}
		goto st1
	st11:
		if p++; p == pe {
			goto _test_eof11
		}
	st_case_11:
		if 48 <= data[p] && data[p] <= 57 {
			goto st11
		}
		goto tr37
	st1:
		if p++; p == pe {
			goto _test_eof1
		}
	st_case_1:
		switch data[p] {
		case 36:
			goto tr4
		case 96:
			goto tr3
		}
		switch {
		case data[p] < 58:
			if data[p] <= 47 {
				goto tr3
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr3
				}
			case data[p] >= 91:
				goto tr3
			}
		default:
			goto tr3
		}
		goto st1
	st12:
		if p++; p == pe {
			goto _test_eof12
		}
	st_case_12:
		switch data[p] {
		case 33:
			goto tr14
		case 35:
			goto tr14
		case 45:
			goto st13
		case 47:
			goto tr14
		case 94:
			goto tr14
		case 96:
			goto tr14
		case 124:
			goto tr14
		case 126:
			goto tr14
		}
		switch {
		case data[p] < 42:
			if 37 <= data[p] && data[p] <= 38 {
				goto tr14
			}
		case data[p] > 43:
			if 60 <= data[p] && data[p] <= 64 {
				goto tr14
			}
		default:
			goto tr14
		}
		goto tr32
	st13:
		if p++; p == pe {
			goto _test_eof13
		}
	st_case_13:
		switch data[p] {
		case 10:
			goto tr39
		case 13:
			goto tr39
		case 33:
			goto st13
		case 35:
			goto st13
		case 45:
			goto st13
		case 47:
			goto st13
		case 94:
			goto st13
		case 96:
			goto st13
		case 124:
			goto st13
		case 126:
			goto st13
		}
		switch {
		case data[p] < 42:
			if 37 <= data[p] && data[p] <= 38 {
				goto st13
			}
		case data[p] > 43:
			if 60 <= data[p] && data[p] <= 64 {
				goto st13
			}
		default:
			goto st13
		}
		goto st14
	st14:
		if p++; p == pe {
			goto _test_eof14
		}
	st_case_14:
		switch data[p] {
		case 10:
			goto tr39
		case 13:
			goto tr39
		}
		goto st14
	st15:
		if p++; p == pe {
			goto _test_eof15
		}
	st_case_15:
		if data[p] == 46 {
			goto tr41
		}
		if 48 <= data[p] && data[p] <= 57 {
			goto st16
		}
		goto tr34
	st16:
		if p++; p == pe {
			goto _test_eof16
		}
	st_case_16:
		switch data[p] {
		case 69:
			goto st18
		case 95:
			goto st23
		case 101:
			goto st18
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st16
				}
			default:
				goto tr43
			}
		case data[p] > 64:
			switch {
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr43
				}
			case data[p] >= 91:
				goto tr43
			}
		default:
			goto tr43
		}
		goto st17
	st17:
		if p++; p == pe {
			goto _test_eof17
		}
	st_case_17:
		if data[p] == 96 {
			goto tr47
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr47
				}
			default:
				goto tr47
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr47
				}
			case data[p] >= 91:
				goto tr47
			}
		default:
			goto tr47
		}
		goto st17
	st18:
		if p++; p == pe {
			goto _test_eof18
		}
	st_case_18:
		switch data[p] {
		case 43:
			goto st19
		case 45:
			goto st19
		case 96:
			goto tr47
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr47
				}
			default:
				goto tr47
			}
		case data[p] > 57:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr47
				}
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr47
				}
			default:
				goto tr47
			}
		default:
			goto st22
		}
		goto st17
	st19:
		if p++; p == pe {
			goto _test_eof19
		}
	st_case_19:
		if 48 <= data[p] && data[p] <= 57 {
			goto st20
		}
		goto tr47
	st20:
		if p++; p == pe {
			goto _test_eof20
		}
	st_case_20:
		if data[p] == 95 {
			goto st21
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st20
				}
			default:
				goto tr51
			}
		case data[p] > 64:
			switch {
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr51
				}
			case data[p] >= 91:
				goto tr51
			}
		default:
			goto tr51
		}
		goto st17
	st21:
		if p++; p == pe {
			goto _test_eof21
		}
	st_case_21:
		if data[p] == 96 {
			goto tr47
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr47
				}
			default:
				goto tr47
			}
		case data[p] > 57:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr47
				}
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr47
				}
			default:
				goto tr47
			}
		default:
			goto st22
		}
		goto st17
	st22:
		if p++; p == pe {
			goto _test_eof22
		}
	st_case_22:
		if data[p] == 95 {
			goto st21
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr51
				}
			default:
				goto tr51
			}
		case data[p] > 57:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr51
				}
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr51
				}
			default:
				goto tr51
			}
		default:
			goto st22
		}
		goto st17
	st23:
		if p++; p == pe {
			goto _test_eof23
		}
	st_case_23:
		if data[p] == 96 {
			goto tr47
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr47
				}
			default:
				goto tr47
			}
		case data[p] > 57:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr47
				}
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr47
				}
			default:
				goto tr47
			}
		default:
			goto st24
		}
		goto st17
	st24:
		if p++; p == pe {
			goto _test_eof24
		}
	st_case_24:
		switch data[p] {
		case 69:
			goto st18
		case 95:
			goto st23
		case 101:
			goto st18
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr43
				}
			default:
				goto tr43
			}
		case data[p] > 57:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr43
				}
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr43
				}
			default:
				goto tr43
			}
		default:
			goto st24
		}
		goto st17
	st25:
		if p++; p == pe {
			goto _test_eof25
		}
	st_case_25:
		switch data[p] {
		case 33:
			goto tr14
		case 35:
			goto tr14
		case 42:
			goto tr54
		case 43:
			goto tr14
		case 45:
			goto tr14
		case 47:
			goto tr14
		case 94:
			goto tr14
		case 96:
			goto tr14
		case 124:
			goto tr14
		case 126:
			goto tr14
		}
		switch {
		case data[p] > 38:
			if 60 <= data[p] && data[p] <= 64 {
				goto tr14
			}
		case data[p] >= 37:
			goto tr14
		}
		goto tr32
	st26:
		if p++; p == pe {
			goto _test_eof26
		}
	st_case_26:
		switch data[p] {
		case 46:
			goto st27
		case 66:
			goto st31
		case 69:
			goto st18
		case 79:
			goto st34
		case 88:
			goto st37
		case 95:
			goto st29
		case 98:
			goto st31
		case 101:
			goto st18
		case 111:
			goto st34
		case 120:
			goto st37
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st28
				}
			default:
				goto tr55
			}
		case data[p] > 64:
			switch {
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr55
				}
			case data[p] >= 91:
				goto tr55
			}
		default:
			goto tr55
		}
		goto st17
	st27:
		if p++; p == pe {
			goto _test_eof27
		}
	st_case_27:
		switch data[p] {
		case 46:
			goto tr61
		case 69:
			goto st18
		case 96:
			goto tr43
		case 101:
			goto st18
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st16
				}
			default:
				goto tr43
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr43
				}
			case data[p] >= 91:
				goto tr43
			}
		default:
			goto tr43
		}
		goto st17
	st28:
		if p++; p == pe {
			goto _test_eof28
		}
	st_case_28:
		switch data[p] {
		case 46:
			goto st27
		case 69:
			goto st18
		case 95:
			goto st29
		case 101:
			goto st18
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st28
				}
			default:
				goto tr55
			}
		case data[p] > 64:
			switch {
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr55
				}
			case data[p] >= 91:
				goto tr55
			}
		default:
			goto tr55
		}
		goto st17
	st29:
		if p++; p == pe {
			goto _test_eof29
		}
	st_case_29:
		if data[p] == 96 {
			goto tr47
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr47
				}
			default:
				goto tr47
			}
		case data[p] > 57:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr47
				}
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr47
				}
			default:
				goto tr47
			}
		default:
			goto st30
		}
		goto st17
	st30:
		if p++; p == pe {
			goto _test_eof30
		}
	st_case_30:
		switch data[p] {
		case 46:
			goto st27
		case 69:
			goto st18
		case 95:
			goto st29
		case 101:
			goto st18
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr55
				}
			default:
				goto tr55
			}
		case data[p] > 57:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr55
				}
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr55
				}
			default:
				goto tr55
			}
		default:
			goto st30
		}
		goto st17
	st31:
		if p++; p == pe {
			goto _test_eof31
		}
	st_case_31:
		if data[p] == 95 {
			goto tr65
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr63
				}
			default:
				goto tr63
			}
		case data[p] > 49:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr63
				}
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr63
				}
			default:
				goto tr63
			}
		default:
			goto st32
		}
		goto st17
	st32:
		if p++; p == pe {
			goto _test_eof32
		}
	st_case_32:
		if data[p] == 95 {
			goto tr67
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr66
				}
			default:
				goto tr66
			}
		case data[p] > 49:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr66
				}
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr66
				}
			default:
				goto tr66
			}
		default:
			goto st32
		}
		goto st17
	tr65:
//line NONE:1
		te = p + 1

//line scan.rl:205
		act = 19
		goto st33
	tr67:
//line NONE:1
		te = p + 1

//line scan.rl:210
		act = 20
		goto st33
	st33:
		if p++; p == pe {
			goto _test_eof33
		}
	st_case_33:
//line scan.go:1939
		if data[p] == 96 {
			goto tr0
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr0
				}
			default:
				goto tr0
			}
		case data[p] > 49:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr0
				}
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr0
				}
			default:
				goto tr0
			}
		default:
			goto st32
		}
		goto st17
	st34:
		if p++; p == pe {
			goto _test_eof34
		}
	st_case_34:
		if data[p] == 95 {
			goto tr69
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr63
				}
			default:
				goto tr63
			}
		case data[p] > 55:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr63
				}
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr63
				}
			default:
				goto tr63
			}
		default:
			goto st35
		}
		goto st17
	st35:
		if p++; p == pe {
			goto _test_eof35
		}
	st_case_35:
		if data[p] == 95 {
			goto tr71
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr70
				}
			default:
				goto tr70
			}
		case data[p] > 55:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr70
				}
			case data[p] > 96:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr70
				}
			default:
				goto tr70
			}
		default:
			goto st35
		}
		goto st17
	tr69:
//line NONE:1
		te = p + 1

//line scan.rl:205
		act = 19
		goto st36
	tr71:
//line NONE:1
		te = p + 1

//line scan.rl:210
		act = 20
		goto st36
	st36:
		if p++; p == pe {
			goto _test_eof36
		}
	st_case_36:
//line scan.go:2059
		if data[p] == 96 {
			goto tr0
		}
		switch {
		case data[p] < 48:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr0
				}
			default:
				goto tr0
			}
		case data[p] > 55:
			switch {
			case data[p] < 91:
				if 58 <= data[p] && data[p] <= 64 {
					goto tr0
				}
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr0
				}
			default:
				goto tr0
			}
		default:
			goto st35
		}
		goto st17
	st37:
		if p++; p == pe {
			goto _test_eof37
		}
	st_case_37:
		if data[p] == 95 {
			goto tr73
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] < 37:
				if data[p] <= 35 {
					goto tr63
				}
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st38
				}
			default:
				goto tr63
			}
		case data[p] > 64:
			switch {
			case data[p] < 91:
				if 65 <= data[p] && data[p] <= 70 {
					goto st38
				}
			case data[p] > 96:
				switch {
				case data[p] > 102:
					if 123 <= data[p] && data[p] <= 127 {
						goto tr63
					}
				case data[p] >= 97:
					goto st38
				}
			default:
				goto tr63
			}
		default:
			goto tr63
		}
		goto st17
	st38:
		if p++; p == pe {
			goto _test_eof38
		}
	st_case_38:
		if data[p] == 95 {
			goto tr75
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] < 37:
				if data[p] <= 35 {
					goto tr74
				}
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st38
				}
			default:
				goto tr74
			}
		case data[p] > 64:
			switch {
			case data[p] < 91:
				if 65 <= data[p] && data[p] <= 70 {
					goto st38
				}
			case data[p] > 96:
				switch {
				case data[p] > 102:
					if 123 <= data[p] && data[p] <= 127 {
						goto tr74
					}
				case data[p] >= 97:
					goto st38
				}
			default:
				goto tr74
			}
		default:
			goto tr74
		}
		goto st17
	tr73:
//line NONE:1
		te = p + 1

//line scan.rl:205
		act = 19
		goto st39
	tr75:
//line NONE:1
		te = p + 1

//line scan.rl:210
		act = 20
		goto st39
	st39:
		if p++; p == pe {
			goto _test_eof39
		}
	st_case_39:
//line scan.go:2197
		if data[p] == 96 {
			goto tr0
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] < 37:
				if data[p] <= 35 {
					goto tr0
				}
			case data[p] > 47:
				if 48 <= data[p] && data[p] <= 57 {
					goto st38
				}
			default:
				goto tr0
			}
		case data[p] > 64:
			switch {
			case data[p] < 91:
				if 65 <= data[p] && data[p] <= 70 {
					goto st38
				}
			case data[p] > 94:
				switch {
				case data[p] > 102:
					if 123 <= data[p] && data[p] <= 127 {
						goto tr0
					}
				case data[p] >= 97:
					goto st38
				}
			default:
				goto tr0
			}
		default:
			goto tr0
		}
		goto st17
	st40:
		if p++; p == pe {
			goto _test_eof40
		}
	st_case_40:
		switch data[p] {
		case 58:
			goto tr76
		case 61:
			goto tr77
		}
		goto tr34
	st41:
		if p++; p == pe {
			goto _test_eof41
		}
	st_case_41:
		switch data[p] {
		case 33:
			goto tr14
		case 35:
			goto tr14
		case 45:
			goto tr14
		case 47:
			goto tr14
		case 61:
			goto tr78
		case 62:
			goto tr33
		case 94:
			goto tr14
		case 96:
			goto tr14
		case 124:
			goto tr14
		case 126:
			goto tr14
		}
		switch {
		case data[p] < 42:
			if 37 <= data[p] && data[p] <= 38 {
				goto tr14
			}
		case data[p] > 43:
			if 60 <= data[p] && data[p] <= 64 {
				goto tr14
			}
		default:
			goto tr14
		}
		goto tr32
	st42:
		if p++; p == pe {
			goto _test_eof42
		}
	st_case_42:
		switch data[p] {
		case 33:
			goto tr14
		case 35:
			goto tr14
		case 45:
			goto tr14
		case 47:
			goto tr14
		case 62:
			goto tr79
		case 94:
			goto tr14
		case 96:
			goto tr14
		case 124:
			goto tr14
		case 126:
			goto tr14
		}
		switch {
		case data[p] < 42:
			if 37 <= data[p] && data[p] <= 38 {
				goto tr14
			}
		case data[p] > 43:
			if 60 <= data[p] && data[p] <= 64 {
				goto tr14
			}
		default:
			goto tr14
		}
		goto tr32
	st43:
		if p++; p == pe {
			goto _test_eof43
		}
	st_case_43:
		switch data[p] {
		case 33:
			goto tr14
		case 35:
			goto tr14
		case 45:
			goto tr14
		case 47:
			goto tr14
		case 61:
			goto tr80
		case 94:
			goto tr14
		case 96:
			goto tr14
		case 124:
			goto tr14
		case 126:
			goto tr14
		}
		switch {
		case data[p] < 42:
			if 37 <= data[p] && data[p] <= 38 {
				goto tr14
			}
		case data[p] > 43:
			if 60 <= data[p] && data[p] <= 64 {
				goto tr14
			}
		default:
			goto tr14
		}
		goto tr32
	st44:
		if p++; p == pe {
			goto _test_eof44
		}
	st_case_44:
		if data[p] == 96 {
			goto tr81
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr81
				}
			default:
				goto tr81
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr81
				}
			case data[p] >= 91:
				goto tr81
			}
		default:
			goto tr81
		}
		goto st44
	st45:
		if p++; p == pe {
			goto _test_eof45
		}
	st_case_45:
		switch data[p] {
		case 39:
			goto tr82
		case 96:
			goto tr81
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr81
				}
			default:
				goto tr81
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr81
				}
			case data[p] >= 91:
				goto tr81
			}
		default:
			goto tr81
		}
		goto st44
	st46:
		if p++; p == pe {
			goto _test_eof46
		}
	st_case_46:
		switch data[p] {
		case 39:
			goto tr16
		case 96:
			goto tr81
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr81
				}
			default:
				goto tr81
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr81
				}
			case data[p] >= 91:
				goto tr81
			}
		default:
			goto tr81
		}
		goto st44
	st47:
		if p++; p == pe {
			goto _test_eof47
		}
	st_case_47:
		switch data[p] {
		case 39:
			goto tr83
		case 96:
			goto tr81
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr81
				}
			default:
				goto tr81
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr81
				}
			case data[p] >= 91:
				goto tr81
			}
		default:
			goto tr81
		}
		goto st44
	tr30:
//line NONE:1
		te = p + 1

//line scan.rl:345
		act = 28
		goto st48
	st48:
		if p++; p == pe {
			goto _test_eof48
		}
	st_case_48:
//line scan.go:2510
		switch data[p] {
		case 38:
			goto st2
		case 96:
			goto tr81
		}
		switch {
		case data[p] < 58:
			switch {
			case data[p] > 35:
				if 37 <= data[p] && data[p] <= 47 {
					goto tr81
				}
			default:
				goto tr81
			}
		case data[p] > 64:
			switch {
			case data[p] > 94:
				if 123 <= data[p] && data[p] <= 127 {
					goto tr81
				}
			case data[p] >= 91:
				goto tr81
			}
		default:
			goto tr81
		}
		goto st44
	st2:
		if p++; p == pe {
			goto _test_eof2
		}
	st_case_2:
		switch data[p] {
		case 34:
			goto st3
		case 39:
			goto tr8
		}
		goto tr6
	st3:
		if p++; p == pe {
			goto _test_eof3
		}
	st_case_3:
		if data[p] == 34 {
			goto tr9
		}
		goto st3
	tr9:
//line NONE:1
		te = p + 1

//line scan.rl:316
		act = 26
		goto st49
	st49:
		if p++; p == pe {
			goto _test_eof49
		}
	st_case_49:
//line scan.go:2573
		if data[p] == 34 {
			goto st3
		}
		goto tr85
	st_out:
	_test_eof4:
		cs = 4
		goto _test_eof
	_test_eof5:
		cs = 5
		goto _test_eof
	_test_eof6:
		cs = 6
		goto _test_eof
	_test_eof7:
		cs = 7
		goto _test_eof
	_test_eof8:
		cs = 8
		goto _test_eof
	_test_eof0:
		cs = 0
		goto _test_eof
	_test_eof9:
		cs = 9
		goto _test_eof
	_test_eof10:
		cs = 10
		goto _test_eof
	_test_eof11:
		cs = 11
		goto _test_eof
	_test_eof1:
		cs = 1
		goto _test_eof
	_test_eof12:
		cs = 12
		goto _test_eof
	_test_eof13:
		cs = 13
		goto _test_eof
	_test_eof14:
		cs = 14
		goto _test_eof
	_test_eof15:
		cs = 15
		goto _test_eof
	_test_eof16:
		cs = 16
		goto _test_eof
	_test_eof17:
		cs = 17
		goto _test_eof
	_test_eof18:
		cs = 18
		goto _test_eof
	_test_eof19:
		cs = 19
		goto _test_eof
	_test_eof20:
		cs = 20
		goto _test_eof
	_test_eof21:
		cs = 21
		goto _test_eof
	_test_eof22:
		cs = 22
		goto _test_eof
	_test_eof23:
		cs = 23
		goto _test_eof
	_test_eof24:
		cs = 24
		goto _test_eof
	_test_eof25:
		cs = 25
		goto _test_eof
	_test_eof26:
		cs = 26
		goto _test_eof
	_test_eof27:
		cs = 27
		goto _test_eof
	_test_eof28:
		cs = 28
		goto _test_eof
	_test_eof29:
		cs = 29
		goto _test_eof
	_test_eof30:
		cs = 30
		goto _test_eof
	_test_eof31:
		cs = 31
		goto _test_eof
	_test_eof32:
		cs = 32
		goto _test_eof
	_test_eof33:
		cs = 33
		goto _test_eof
	_test_eof34:
		cs = 34
		goto _test_eof
	_test_eof35:
		cs = 35
		goto _test_eof
	_test_eof36:
		cs = 36
		goto _test_eof
	_test_eof37:
		cs = 37
		goto _test_eof
	_test_eof38:
		cs = 38
		goto _test_eof
	_test_eof39:
		cs = 39
		goto _test_eof
	_test_eof40:
		cs = 40
		goto _test_eof
	_test_eof41:
		cs = 41
		goto _test_eof
	_test_eof42:
		cs = 42
		goto _test_eof
	_test_eof43:
		cs = 43
		goto _test_eof
	_test_eof44:
		cs = 44
		goto _test_eof
	_test_eof45:
		cs = 45
		goto _test_eof
	_test_eof46:
		cs = 46
		goto _test_eof
	_test_eof47:
		cs = 47
		goto _test_eof
	_test_eof48:
		cs = 48
		goto _test_eof
	_test_eof2:
		cs = 2
		goto _test_eof
	_test_eof3:
		cs = 3
		goto _test_eof
	_test_eof49:
		cs = 49
		goto _test_eof

	_test_eof:
		{
		}
		if p == eof {
			switch cs {
			case 5:
				goto tr31
			case 6:
				goto tr32
			case 7:
				goto tr0
			case 8:
				goto tr34
			case 0:
				goto tr0
			case 9:
				goto tr35
			case 10:
				goto tr34
			case 11:
				goto tr37
			case 1:
				goto tr3
			case 12:
				goto tr32
			case 13:
				goto tr39
			case 14:
				goto tr39
			case 15:
				goto tr34
			case 16:
				goto tr43
			case 17:
				goto tr47
			case 18:
				goto tr47
			case 19:
				goto tr47
			case 20:
				goto tr51
			case 21:
				goto tr47
			case 22:
				goto tr51
			case 23:
				goto tr47
			case 24:
				goto tr43
			case 25:
				goto tr32
			case 26:
				goto tr55
			case 27:
				goto tr43
			case 28:
				goto tr55
			case 29:
				goto tr47
			case 30:
				goto tr55
			case 31:
				goto tr63
			case 32:
				goto tr66
			case 33:
				goto tr0
			case 34:
				goto tr63
			case 35:
				goto tr70
			case 36:
				goto tr0
			case 37:
				goto tr63
			case 38:
				goto tr74
			case 39:
				goto tr0
			case 40:
				goto tr34
			case 41:
				goto tr32
			case 42:
				goto tr32
			case 43:
				goto tr32
			case 44:
				goto tr81
			case 45:
				goto tr81
			case 46:
				goto tr81
			case 47:
				goto tr81
			case 48:
				goto tr81
			case 2:
				goto tr6
			case 3:
				goto tr0
			case 49:
				goto tr85
			}
		}

	_out:
		{
		}
	}

//line scan.rl:482

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
	errInvalidUnicodeValue  = errors.New("invalid Unicode escape value")
	errInvalidUnicodePair   = errors.New("invalid Unicode surrogate pair")
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
				value.WriteString(data[segment : i+1])
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
