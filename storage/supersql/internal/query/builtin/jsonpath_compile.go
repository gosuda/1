package builtin

import (
	"context"
	"errors"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const jsonPathWireVersion = 1
const jsonPathCompileDepth = 256

type jsonPathKind uint8

const (
	jpNull jsonPathKind = iota + 1
	jpString
	jpNumber
	jpBoolean
	jpRoot
	jpCurrent
	jpVariable
	jpLast
	jpMember
	jpAnyMember
	jpAnyArray
	jpSubscript
	jpRecursive
	jpFilter
	jpMethod
	jpExists
	jpNot
	jpIsUnknown
	jpAnd
	jpOr
	jpEqual
	jpNotEqual
	jpLess
	jpGreater
	jpLessEqual
	jpGreaterEqual
	jpStartsWith
	jpLikeRegex
	jpAdd
	jpSubtract
	jpMultiply
	jpDivide
	jpModulo
	jpPlus
	jpMinus
)

type jsonPathSubscript struct {
	from *jsonPathExpr
	to   *jsonPathExpr
}

type jsonPathExpr struct {
	kind       jsonPathKind
	left       *jsonPathExpr
	right      *jsonPathExpr
	args       []*jsonPathExpr
	subscripts []jsonPathSubscript
	text       string
	boolean    bool
	first      int
	last       int
	bounds     bool
	regexFlags    regexpFlags
	regexFlagText string
	regex         regexpProgram
}

// JSONPathPlan is immutable and safe to share. Execute calls own all mutable
// traversal state; the plan retains no source document or variable values.
type JSONPathPlan struct {
	expression *jsonPathExpr
	canonical  string
	lax        bool
	ownedBytes uint64
}

func (plan *JSONPathPlan) Lax() bool          { return plan != nil && plan.lax }
func (plan *JSONPathPlan) String() string     { if plan == nil { return "" }; return plan.canonical }
func (plan *JSONPathPlan) OwnedBytes() uint64 { if plan == nil { return 0 }; return plan.ownedBytes }

type jsonPathTemporalStatus uint8

const (
	jsonPathNonTemporal jsonPathTemporalStatus = iota
	jsonPathUnknownTemporal
	jsonPathZonedTemporal
	jsonPathUnzonedTemporal
)

// Mutable reports whether PostgreSQL's timezone-enabled SQL/JSON execution
// can depend on session timezone for the supplied PASSING types.
func (plan *JSONPathPlan) Mutable(variables map[string]model.TypeID) bool {
	if plan == nil || plan.expression == nil {
		return true
	}
	mutable := false
	jsonPathMutableStatus(plan.expression, jsonPathNonTemporal, plan.lax, variables, &mutable)
	return mutable
}

func jsonPathMutableStatus(node *jsonPathExpr, current jsonPathTemporalStatus, lax bool, variables map[string]model.TypeID, mutable *bool) jsonPathTemporalStatus {
	if node == nil || *mutable {
		return jsonPathNonTemporal
	}
	switch node.kind {
	case jpVariable:
		switch variables[node.text] {
		case 1082, 1083, 1114:
			return jsonPathUnzonedTemporal
		case 1184, 1266:
			return jsonPathZonedTemporal
		default:
			return jsonPathNonTemporal
		}
	case jpCurrent:
		return current
	case jpFilter:
		status := jsonPathMutableStatus(node.left, current, lax, variables, mutable)
		jsonPathMutableStatus(node.right, status, lax, variables, mutable)
		return status
	case jpEqual, jpNotEqual, jpLess, jpGreater, jpLessEqual, jpGreaterEqual:
		left := jsonPathMutableStatus(node.left, current, lax, variables, mutable)
		right := jsonPathMutableStatus(node.right, current, lax, variables, mutable)
		if left != jsonPathNonTemporal && right != jsonPathNonTemporal &&
			(left == jsonPathUnknownTemporal || right == jsonPathUnknownTemporal || left != right) {
			*mutable = true
		}
		return jsonPathNonTemporal
	case jpMethod:
		jsonPathMutableStatus(node.left, current, lax, variables, mutable)
		switch node.text {
		case "date", "time", "time_tz", "timestamp", "timestamp_tz":
			*mutable = true
			if node.text == "time_tz" || node.text == "timestamp_tz" {
				return jsonPathZonedTemporal
			}
			return jsonPathUnzonedTemporal
		case "datetime":
			if len(node.args) == 0 {
				return jsonPathUnknownTemporal
			}
			if jsonPathDatetimeTemplateHasZone(node.args[0].text) {
				return jsonPathZonedTemporal
			}
			return jsonPathUnzonedTemporal
		default:
			for _, argument := range node.args {
				jsonPathMutableStatus(argument, current, lax, variables, mutable)
			}
			return jsonPathNonTemporal
		}
	case jpSubscript:
		status := jsonPathMutableStatus(node.left, current, lax, variables, mutable)
		for _, subscript := range node.subscripts {
			jsonPathMutableStatus(subscript.from, current, lax, variables, mutable)
			jsonPathMutableStatus(subscript.to, current, lax, variables, mutable)
		}
		if !lax {
			return jsonPathNonTemporal
		}
		return status
	case jpAnyArray:
		status := jsonPathMutableStatus(node.left, current, lax, variables, mutable)
		if !lax {
			return jsonPathNonTemporal
		}
		return status
	case jpRecursive:
		status := jsonPathMutableStatus(node.left, current, lax, variables, mutable)
		if node.first > 0 {
			return jsonPathNonTemporal
		}
		return status
	default:
		jsonPathMutableStatus(node.left, current, lax, variables, mutable)
		jsonPathMutableStatus(node.right, current, lax, variables, mutable)
		for _, argument := range node.args {
			jsonPathMutableStatus(argument, current, lax, variables, mutable)
		}
		for _, subscript := range node.subscripts {
			jsonPathMutableStatus(subscript.from, current, lax, variables, mutable)
			jsonPathMutableStatus(subscript.to, current, lax, variables, mutable)
		}
		return jsonPathNonTemporal
	}
}

func jsonPathDatetimeTemplateHasZone(template string) bool {
	upper := strings.ToUpper(template)
	quoted := false
	for offset := 0; offset < len(upper); {
		switch upper[offset] {
		case '"':
			quoted = !quoted
			offset++
		case '\\':
			if quoted && offset+1 < len(upper) {
				offset += 2
			} else {
				offset++
			}
		default:
			if !quoted && (strings.HasPrefix(upper[offset:], "TZH") || strings.HasPrefix(upper[offset:], "TZM") ||
				strings.HasPrefix(upper[offset:], "TZ") || strings.HasPrefix(upper[offset:], "OF")) {
				return true
			}
			offset++
		}
	}
	return false
}

// CompileJSONPath parses PostgreSQL 18 JSON path syntax and owns the resulting
// plan. maximum bounds syntax, canonical text, and precompiled regular expressions.
func CompileJSONPath(ctx context.Context, input string, maximum uint64) (*JSONPathPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(input) { return nil, sqlError("22021", "invalid UTF8 sequence in jsonpath") }
	if maximum == 0 || uint64(len(input)) > maximum {
		return nil, jsonPathResourceError()
	}
	parser := jsonPathParser{ctx: ctx, lexer: jsonPathLexer{ctx: ctx, input: input}, maximum: maximum, owned: uint64(unsafe.Sizeof(JSONPathPlan{}))}
	plan, err := parser.parse()
	if err != nil {
		return nil, err
	}
	size, err := jsonPathCanonicalBytes(ctx, plan.expression, 8, 0)
	if err != nil { return nil, err }
	if !plan.lax { size += uint64(len("strict ")) }
	if parser.owned > maximum || size > maximum-parser.owned || size > uint64(math.MaxInt) {
		return nil, jsonPathResourceError()
	}
	var output strings.Builder
	output.Grow(int(size))
	if !plan.lax { output.WriteString("strict ") }
	if err := appendJSONPathCanonical(ctx, &output, plan.expression, 8); err != nil { return nil, err }
	if uint64(output.Cap()) > maximum-parser.owned { return nil, jsonPathResourceError() }
	plan.canonical = output.String()
	plan.ownedBytes = parser.owned + uint64(output.Cap())
	return plan, nil
}

// EncodeJSONPathBinary returns PostgreSQL's versioned binary protocol: one
// version byte followed by the canonical text representation.
func EncodeJSONPathBinary(ctx context.Context, plan *JSONPathPlan, maximum uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if plan == nil || plan.expression == nil {
		return nil, jsonPathSyntaxError("invalid empty plan")
	}
	size := uint64(len(plan.canonical)) + 1
	if size > maximum || size > uint64(math.MaxInt) {
		return nil, jsonPathResourceError()
	}
	result := make([]byte, int(size))
	result[0] = jsonPathWireVersion
	copy(result[1:], plan.canonical)
	return result, ctx.Err()
}

// DecodeJSONPathBinary validates the complete wire frame and compiles an owned plan.
func DecodeJSONPathBinary(ctx context.Context, data []byte, maximum uint64) (*JSONPathPlan, error) {
	if err := ctx.Err(); err != nil { return nil, err }
	if len(data) == 0 {
		return nil, jsonPathSyntaxError("missing jsonpath version number")
	}
	if data[0] != jsonPathWireVersion {
		return nil, sqlError("22023", "unsupported jsonpath version number: "+strconv.Itoa(int(data[0])))
	}
	if uint64(len(data)) > maximum {
		return nil, jsonPathResourceError()
	}
	if !utf8.Valid(data[1:]) {
		return nil, sqlError("22021", "invalid UTF8 sequence in jsonpath")
	}
	return CompileJSONPath(ctx, string(data[1:]), maximum-1)
}

func jsonPathSyntaxError(detail string) error {
	return sqlError("22P02", "invalid input syntax for type jsonpath: "+detail)
}

func jsonPathResourceError() error { return sqlError("54000", "jsonpath compile byte budget exceeded") }

type jsonPathTokenKind uint8

const (
	jpTokenEOF jsonPathTokenKind = iota
	jpTokenWord
	jpTokenString
	jpTokenNumber
	jpTokenVariable
	jpTokenSymbol
)

type jsonPathToken struct {
	kind jsonPathTokenKind
	text string
	pos  int
}

type jsonPathLexer struct {
	ctx      context.Context
	input    string
	position int
}

func (lexer *jsonPathLexer) next() (jsonPathToken, error) {
	for lexer.position < len(lexer.input) {
		if err := lexer.ctx.Err(); err != nil {
			return jsonPathToken{}, err
		}
		switch lexer.input[lexer.position] {
		case ' ', '\t', '\n', '\r', '\f':
			lexer.position++
		case '/':
			if lexer.position+1 >= len(lexer.input) || lexer.input[lexer.position+1] != '*' {
				return lexer.symbol(), nil
			}
			end := lexer.position + 2
			for end+1 < len(lexer.input) && lexer.input[end:end+2] != "*/" {
				if end&4095 == 0 { if err := lexer.ctx.Err(); err != nil { return jsonPathToken{}, err } }
				end++
			}
			if end+1 >= len(lexer.input) { return jsonPathToken{}, jsonPathSyntaxError("unexpected end of comment") }
			lexer.position = end + 2
		default:
			goto token
		}
	}
	return jsonPathToken{kind: jpTokenEOF, pos: lexer.position}, nil

token:
	start := lexer.position
	value := lexer.input[start]
	if value == '"' {
		text, err := lexer.quoted()
		return jsonPathToken{kind: jpTokenString, text: text, pos: start}, err
	}
	if value == '$' {
		lexer.position++
		if lexer.position < len(lexer.input) && lexer.input[lexer.position] == '"' {
			text, err := lexer.quoted()
			return jsonPathToken{kind: jpTokenVariable, text: text, pos: start}, err
		}
		name, err := lexer.unquoted()
		if err != nil {
			return jsonPathToken{}, err
		}
		if name == "" {
			return jsonPathToken{kind: jpTokenSymbol, text: "$", pos: start}, nil
		}
		return jsonPathToken{kind: jpTokenVariable, text: name, pos: start}, nil
	}
	if isJSONPathDigit(value) || value == '.' && lexer.position+1 < len(lexer.input) && isJSONPathDigit(lexer.input[lexer.position+1]) {
		text, err := lexer.number()
		return jsonPathToken{kind: jpTokenNumber, text: text, pos: start}, err
	}
	if isJSONPathSpecial(value) {
		return lexer.symbol(), nil
	}
	text, err := lexer.unquoted()
	return jsonPathToken{kind: jpTokenWord, text: text, pos: start}, err
}

func (lexer *jsonPathLexer) symbol() jsonPathToken {
	start := lexer.position
	lexer.position++
	text := lexer.input[start:lexer.position]
	if lexer.position < len(lexer.input) {
		pair := lexer.input[start : lexer.position+1]
		switch pair {
		case "&&", "||", "**", "<=", ">=", "==", "!=", "<>":
			lexer.position++
			text = pair
		}
	}
	return jsonPathToken{kind: jpTokenSymbol, text: text, pos: start}
}

func (lexer *jsonPathLexer) quoted() (string, error) {
	lexer.position++
	var output strings.Builder
	for lexer.position < len(lexer.input) {
		if lexer.position&4095 == 0 {
			if err := lexer.ctx.Err(); err != nil {
				return "", err
			}
		}
		c := lexer.input[lexer.position]
		lexer.position++
		if c == '"' {
			return output.String(), nil
		}
		if c != '\\' {
			if c == 0 || c < 0x20 {
				return "", jsonPathSyntaxError("invalid control character")
			}
			if c < utf8.RuneSelf {
				output.WriteByte(c)
				continue
			}
			r, width := utf8.DecodeRuneInString(lexer.input[lexer.position-1:])
			if r == utf8.RuneError && width == 1 {
				return "", sqlError("22021", "invalid UTF8 sequence in jsonpath")
			}
			output.WriteRune(r)
			lexer.position += width - 1
			continue
		}
		if lexer.position == len(lexer.input) {
			return "", jsonPathSyntaxError("unexpected end after backslash")
		}
		escape := lexer.input[lexer.position]
		lexer.position++
		switch escape {
		case 'b': output.WriteByte('\b')
		case 'f': output.WriteByte('\f')
		case 'n': output.WriteByte('\n')
		case 'r': output.WriteByte('\r')
		case 't': output.WriteByte('\t')
		case 'v': output.WriteByte('\v')
		case 'x':
			r, err := lexer.hexEscape(2, false)
			if err != nil { return "", err }
			output.WriteRune(r)
		case 'u':
			r, err := lexer.unicodeEscape()
			if err != nil { return "", err }
			output.WriteRune(r)
		default:
			if escape == 0 { return "", jsonPathSyntaxError("invalid null character") }
			output.WriteByte(escape)
		}
	}
	return "", jsonPathSyntaxError("unterminated quoted string")
}

func (lexer *jsonPathLexer) unicodeEscape() (rune, error) {
	braced := lexer.position < len(lexer.input) && lexer.input[lexer.position] == '{'
	if braced {
		lexer.position++
		start := lexer.position
		for lexer.position < len(lexer.input) && lexer.input[lexer.position] != '}' && lexer.position-start < 7 {
			lexer.position++
		}
		if lexer.position == len(lexer.input) || lexer.input[lexer.position] != '}' || lexer.position == start || lexer.position-start > 6 {
			return 0, jsonPathSyntaxError("invalid Unicode escape sequence")
		}
		width := lexer.position - start
		lexer.position++
		value, err := parseJSONPathHex(lexer.input[start:start+width])
		if err != nil || value == 0 || value > utf8.MaxRune || value >= 0xd800 && value <= 0xdfff {
			return 0, jsonPathSyntaxError("invalid Unicode escape sequence")
		}
		return rune(value), nil
	}
	first, err := lexer.hexEscape(4, true)
	if err != nil { return 0, err }
	if first == 0 { return 0, jsonPathSyntaxError("invalid Unicode escape value") }
	if first < 0xd800 || first > 0xdfff { return first, nil }
	if first > 0xdbff || lexer.position+2 > len(lexer.input) || lexer.input[lexer.position:lexer.position+2] != "\\u" {
		return 0, jsonPathSyntaxError("invalid Unicode surrogate pair")
	}
	lexer.position += 2
	second, err := lexer.hexEscape(4, true)
	if err != nil || second < 0xdc00 || second > 0xdfff {
		return 0, jsonPathSyntaxError("invalid Unicode surrogate pair")
	}
	return utf16.DecodeRune(first, second), nil
}

func (lexer *jsonPathLexer) hexEscape(width int, unicode bool) (rune, error) {
	if len(lexer.input)-lexer.position < width {
		return 0, jsonPathSyntaxError("invalid hexadecimal character sequence")
	}
	value, err := parseJSONPathHex(lexer.input[lexer.position : lexer.position+width])
	if err != nil {
		return 0, jsonPathSyntaxError("invalid hexadecimal character sequence")
	}
	lexer.position += width
	if !unicode && value == 0 {
		return 0, jsonPathSyntaxError("invalid hexadecimal character sequence")
	}
	return rune(value), nil
}

func parseJSONPathHex(text string) (int, error) {
	value := 0
	for i := range text {
		d := digit(text[i])
		if d < 0 || d > 15 { return 0, errors.New("invalid hex") }
		value = value*16 + d
	}
	return value, nil
}

func (lexer *jsonPathLexer) unquoted() (string, error) {
	var output strings.Builder
	for lexer.position < len(lexer.input) {
		c := lexer.input[lexer.position]
		if isJSONPathSpecial(c) || c == '"' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' {
			break
		}
		if c < 0x20 { return "", jsonPathSyntaxError("invalid control character") }
		lexer.position++
		if c != '\\' {
			if c == 0 { return "", jsonPathSyntaxError("invalid null character") }
			output.WriteByte(c)
			continue
		}
		if lexer.position == len(lexer.input) { return "", jsonPathSyntaxError("unexpected end after backslash") }
		escape := lexer.input[lexer.position]
		lexer.position++
		switch escape {
		case 'b': output.WriteByte('\b')
		case 'f': output.WriteByte('\f')
		case 'n': output.WriteByte('\n')
		case 'r': output.WriteByte('\r')
		case 't': output.WriteByte('\t')
		case 'v': output.WriteByte('\v')
		case 'x':
			r, err := lexer.hexEscape(2, false); if err != nil { return "", err }; output.WriteRune(r)
		case 'u':
			r, err := lexer.unicodeEscape(); if err != nil { return "", err }; output.WriteRune(r)
		default: output.WriteByte(escape)
		}
	}
	return output.String(), nil
}

func (lexer *jsonPathLexer) number() (string, error) {
	start := lexer.position
	if lexer.position+2 <= len(lexer.input) && lexer.input[lexer.position] == '0' {
		base := byte(0)
		switch lexer.input[lexer.position+1] {
		case 'x', 'X': base = 16
		case 'o', 'O': base = 8
		case 'b', 'B': base = 2
		}
		if base != 0 {
			lexer.position += 2
			digits := 0
			previousUnderscore := false
			for lexer.position < len(lexer.input) {
				c := lexer.input[lexer.position]
				if c == '_' && digits > 0 && !previousUnderscore { previousUnderscore = true; lexer.position++; continue }
				d := digit(c)
				if d < 0 || d >= int(base) { break }
				digits++; previousUnderscore = false; lexer.position++
			}
			if digits == 0 || previousUnderscore || lexer.position < len(lexer.input) && isJSONPathWordByte(lexer.input[lexer.position]) {
				return "", jsonPathSyntaxError("invalid numeric literal")
			}
			return lexer.input[start:lexer.position], nil
		}
	}
	seenDigit, seenPoint, seenExponent := false, false, false
	previousUnderscore := false
	for lexer.position < len(lexer.input) {
		c := lexer.input[lexer.position]
		if isJSONPathDigit(c) { seenDigit = true; previousUnderscore = false; lexer.position++; continue }
		if c == '_' && seenDigit && !previousUnderscore && lexer.position > start && isJSONPathDigit(lexer.input[lexer.position-1]) { previousUnderscore = true; lexer.position++; continue }
		if c == '.' && !seenPoint && !seenExponent && !previousUnderscore {
			if lexer.position+1 < len(lexer.input) {
				next := lexer.input[lexer.position+1]
				if !isJSONPathDigit(next) && next != 'e' && next != 'E' && next != '.' && next != '_' { break }
			}
			seenPoint = true; lexer.position++; continue
		}
		if (c == 'e' || c == 'E') && seenDigit && !seenExponent && !previousUnderscore {
			seenExponent = true; lexer.position++
			if lexer.position < len(lexer.input) && (lexer.input[lexer.position] == '+' || lexer.input[lexer.position] == '-') { lexer.position++ }
			seenDigit = false
			continue
		}
		break
	}
	if !seenDigit || previousUnderscore || lexer.position < len(lexer.input) && isJSONPathWordByte(lexer.input[lexer.position]) {
		return "", jsonPathSyntaxError("invalid numeric literal")
	}
	text := lexer.input[start:lexer.position]
	if len(text) > 1 && text[0] == '0' && isJSONPathDigit(text[1]) { return "", jsonPathSyntaxError("invalid numeric literal") }
	return text, nil
}

func isJSONPathDigit(c byte) bool { return c >= '0' && c <= '9' }
func isJSONPathWordByte(c byte) bool { return !isJSONPathSpecial(c) && c != '"' && c > ' ' }
func isJSONPathSpecial(c byte) bool { return strings.ContainsRune("?%$.[]{}()|&!=<>@#,*-+/:", rune(c)) }

type jsonPathParser struct {
	ctx     context.Context
	lexer   jsonPathLexer
	current jsonPathToken
	ready   bool
	maximum uint64
	owned   uint64
	nodes   int
}

func (parser *jsonPathParser) parse() (*JSONPathPlan, error) {
	if err := parser.advance(); err != nil { return nil, err }
	lax := true
	if parser.current.kind == jpTokenWord {
		switch strings.ToLower(parser.current.text) {
		case "strict": lax = false; if err := parser.advance(); err != nil { return nil, err }
		case "lax": if err := parser.advance(); err != nil { return nil, err }
		}
	}
	if parser.current.kind == jpTokenEOF { return nil, jsonPathSyntaxError("empty input") }
	expression, err := parser.expression(1, 0)
	if err != nil { return nil, err }
	if parser.current.kind != jpTokenEOF { return nil, parser.unexpected() }
	if err := validateJSONPathExpr(parser.ctx, expression, false, false, 0); err != nil { return nil, err }
	return &JSONPathPlan{expression: expression, lax: lax}, nil
}

func (parser *jsonPathParser) advance() error {
	token, err := parser.lexer.next()
	parser.current, parser.ready = token, true
	return err
}
func (parser *jsonPathParser) add(kind jsonPathKind) (*jsonPathExpr, error) {
	parser.nodes++
	const nodeBytes = uint64(unsafe.Sizeof(jsonPathExpr{}))
	if parser.nodes > 1<<20 || parser.maximum < nodeBytes || parser.owned > parser.maximum-nodeBytes {
		return nil, jsonPathResourceError()
	}
	parser.owned += nodeBytes
	return &jsonPathExpr{kind: kind, first: 0, last: -1}, nil
}

func (parser *jsonPathParser) expression(minimum, depth int) (*jsonPathExpr, error) {
	if depth >= jsonPathCompileDepth { return nil, jsonPathSyntaxError("expression nesting limit exceeded") }
	left, err := parser.prefix(depth + 1)
	if err != nil { return nil, err }
	left, err = parser.postfix(left, depth+1)
	if err != nil { return nil, err }
	for {
		kind, precedence, width, err := parser.infix()
		if err != nil { return nil, err }
		if precedence < minimum { break }
		if kind == jpStartsWith {
			if err := parser.advance(); err != nil { return nil, err }
			if !parser.word("with") { return nil, parser.unexpected() }
			if err := parser.advance(); err != nil { return nil, err }
		} else if kind == jpLikeRegex {
			if err := parser.advance(); err != nil { return nil, err }
			if parser.current.kind != jpTokenString { return nil, parser.unexpected() }
			node, err := parser.add(kind); if err != nil { return nil, err }
			node.left, node.text = left, parser.current.text
			if err := parser.advance(); err != nil { return nil, err }
			if parser.word("flag") {
				if err := parser.advance(); err != nil { return nil, err }
				if parser.current.kind != jpTokenString { return nil, parser.unexpected() }
				node.regexFlagText = parser.current.text
				if err := parser.advance(); err != nil { return nil, err }
			}
			node.regexFlagText, node.regexFlags, err = jsonPathRegexpFlags(node.regexFlagText)
			if err != nil { return nil, err }
			payload := uint64(len(node.text) + len(node.regexFlagText))
			if payload > parser.maximum-parser.owned { return nil, jsonPathResourceError() }
			parser.owned += payload
			node.regex, err = compileRegexp(parser.ctx, node.text, node.regexFlags, parser.maximum-parser.owned)
			if err != nil { return nil, err }
			parser.owned += node.regex.ownedBytes
			left = node
			continue
		} else {
			for range width { if err := parser.advance(); err != nil { return nil, err } }
		}
		right, err := parser.expression(precedence+1, depth+1)
		if err != nil { return nil, err }
		node, err := parser.add(kind); if err != nil { return nil, err }
		node.left, node.right = left, right
		left = node
	}
	return left, nil
}

func (parser *jsonPathParser) prefix(depth int) (*jsonPathExpr, error) {
	token := parser.current
	var kind jsonPathKind
	switch token.kind {
	case jpTokenString: kind = jpString
	case jpTokenNumber: kind = jpNumber
	case jpTokenVariable: kind = jpVariable
	case jpTokenWord:
		switch token.text {
		case "null": kind = jpNull
		case "true", "false": kind = jpBoolean
		default:
			switch strings.ToLower(token.text) {
			case "last": kind = jpLast
			case "exists":
				node, err := parser.add(jpExists); if err != nil { return nil, err }
				if err := parser.advance(); err != nil { return nil, err }
				if !parser.symbol("(") { return nil, parser.unexpected() }
				if err := parser.advance(); err != nil { return nil, err }
				node.left, err = parser.expression(1, depth+1)
				if err != nil { return nil, err }
				if !parser.symbol(")") { return nil, parser.unexpected() }
				return node, parser.advance()
			default: return nil, parser.unexpected()
			}
		}
	case jpTokenSymbol:
		switch token.text {
		case "$": kind = jpRoot
		case "@": kind = jpCurrent
		case "+": kind = jpPlus
		case "-": kind = jpMinus
		case "!": kind = jpNot
		case "(":
			if err := parser.advance(); err != nil { return nil, err }
			node, err := parser.expression(1, depth+1)
			if err != nil { return nil, err }
			if !parser.symbol(")") { return nil, parser.unexpected() }
			return node, parser.advance()
		default: return nil, parser.unexpected()
		}
	default: return nil, parser.unexpected()
	}
	node, err := parser.add(kind); if err != nil { return nil, err }
	switch kind {
	case jpString, jpVariable:
		node.text = token.text
		parser.owned += uint64(len(node.text))
	case jpNumber:
		if parser.owned > parser.maximum { return nil, jsonPathResourceError() }
		node.text, err = canonicalJSONPathNumber(parser.ctx, token.text, parser.maximum-parser.owned)
		if err != nil { return nil, jsonPathSyntaxError("invalid numeric literal") }
		parser.owned += uint64(len(node.text))
	case jpBoolean:
		node.boolean = token.text == "true"
	}
	if err := parser.advance(); err != nil { return nil, err }
	if kind == jpPlus || kind == jpMinus || kind == jpNot {
		if kind == jpNot && !parser.symbol("(") { return nil, jsonPathSyntaxError("NOT must precede a parenthesized predicate") }
		node.left, err = parser.expression(6, depth+1)
		if err != nil { return nil, err }
		if kind == jpPlus && node.left.kind == jpNumber { return node.left, nil }
		if kind == jpMinus && node.left.kind == jpNumber {
			if strings.HasPrefix(node.left.text, "-") { node.left.text = node.left.text[1:] } else { node.left.text = "-" + node.left.text; parser.owned++ }
			return node.left, nil
		}
	}
	return node, nil
}

func canonicalJSONPathNumber(ctx context.Context, input string, maximum uint64) (result string, err error) {
	input = strings.ReplaceAll(input, "_", "")
	if len(input) > 2 && input[0] == '0' {
		base := 0
		switch input[1] { case 'x', 'X': base = 16; case 'o', 'O': base = 8; case 'b', 'B': base = 2 }
		if base != 0 {
			integer, ok := new(big.Int).SetString(input[2:], base)
			if !ok { return "", jsonPathSyntaxError("invalid numeric literal") }
			input = integer.String()
		}
	}
	budget := CallBudget{OutputBytes: int64(min(maximum, uint64(math.MaxInt64))), MemoryBytes: maximum}
	work, err := budget.NewWorkspace(ctx)
	if err != nil { return "", err }
	defer func() { err = errors.Join(err, work.Close()) }()
	value, err := parseNumeric(ctx, work, input)
	if err != nil { return "", err }
	return numericText(ctx, value, budget.OutputLimit(work))
}

func (parser *jsonPathParser) postfix(left *jsonPathExpr, depth int) (*jsonPathExpr, error) {
	for {
		if parser.symbol(".") {
			if err := parser.advance(); err != nil { return nil, err }
			if parser.symbol("*") || parser.symbol("**") {
				kind := jpAnyMember
				if parser.current.text == "**" { kind = jpRecursive }
				node, err := parser.add(kind); if err != nil { return nil, err }
				node.left = left
				if err := parser.advance(); err != nil { return nil, err }
				if kind == jpRecursive && parser.symbol("{") {
					if err := parser.recursiveBounds(node); err != nil { return nil, err }
				}
				left = node
				continue
			}
			if parser.current.kind != jpTokenWord && parser.current.kind != jpTokenString { return nil, parser.unexpected() }
			name := parser.current.text
			if err := parser.advance(); err != nil { return nil, err }
			if parser.symbol("(") {
				node, err := parser.method(left, name, depth+1); if err != nil { return nil, err }
				left = node
				continue
			}
			node, err := parser.add(jpMember); if err != nil { return nil, err }
			node.left, node.text = left, name
			parser.owned += uint64(len(name))
			left = node
			continue
		}
		if parser.symbol("[") {
			node, err := parser.subscript(left, depth+1); if err != nil { return nil, err }
			left = node
			continue
		}
		if parser.symbol("?") {
			node, err := parser.add(jpFilter); if err != nil { return nil, err }
			node.left = left
			if err := parser.advance(); err != nil { return nil, err }
			if !parser.symbol("(") { return nil, parser.unexpected() }
			if err := parser.advance(); err != nil { return nil, err }
			node.right, err = parser.expression(1, depth+1)
			if err != nil { return nil, err }
			if !parser.symbol(")") { return nil, parser.unexpected() }
			if err := parser.advance(); err != nil { return nil, err }
			left = node
			continue
		}
		if parser.word("is") {
			if err := parser.advance(); err != nil { return nil, err }
			if !parser.word("unknown") { return nil, parser.unexpected() }
			node, err := parser.add(jpIsUnknown); if err != nil { return nil, err }
			node.left = left
			if err := parser.advance(); err != nil { return nil, err }
			left = node
			continue
		}
		return left, nil
	}
}

func (parser *jsonPathParser) recursiveBounds(node *jsonPathExpr) error {
	if err := parser.advance(); err != nil { return err }
	node.bounds = true
	first, err := parser.level(); if err != nil { return err }
	node.first, node.last = first, first
	if parser.word("to") {
		if err := parser.advance(); err != nil { return err }
		node.last, err = parser.level(); if err != nil { return err }
	}
	if node.first >= 0 && node.last >= 0 && node.last < node.first { return jsonPathSyntaxError("invalid range of recursive descent levels") }
	if !parser.symbol("}") { return parser.unexpected() }
	return parser.advance()
}

func (parser *jsonPathParser) level() (int, error) {
	if parser.word("last") { if err := parser.advance(); err != nil { return 0, err }; return -1, nil }
	if parser.current.kind != jpTokenNumber || strings.ContainsAny(parser.current.text, ".eE") { return 0, parser.unexpected() }
	text := strings.ReplaceAll(parser.current.text, "_", "")
	value, err := strconv.ParseInt(text, 0, 32)
	if err != nil || value < 0 { return 0, jsonPathSyntaxError("recursive descent level is out of range") }
	if err := parser.advance(); err != nil { return 0, err }
	return int(value), nil
}

func (parser *jsonPathParser) subscript(left *jsonPathExpr, depth int) (*jsonPathExpr, error) {
	if err := parser.advance(); err != nil { return nil, err }
	if parser.symbol("*") {
		node, err := parser.add(jpAnyArray); if err != nil { return nil, err }
		node.left = left
		if err := parser.advance(); err != nil { return nil, err }
		if !parser.symbol("]") { return nil, parser.unexpected() }
		return node, parser.advance()
	}
	node, err := parser.add(jpSubscript); if err != nil { return nil, err }
	node.left = left
	for {
		from, err := parser.expression(1, depth+1); if err != nil { return nil, err }
		item := jsonPathSubscript{from: from}
		if parser.word("to") {
			if err := parser.advance(); err != nil { return nil, err }
			item.to, err = parser.expression(1, depth+1); if err != nil { return nil, err }
		}
		before := cap(node.subscripts)
		node.subscripts = append(node.subscripts, item)
		parser.owned += uint64(cap(node.subscripts)-before) * uint64(unsafe.Sizeof(jsonPathSubscript{}))
		if !parser.symbol(",") { break }
		if err := parser.advance(); err != nil { return nil, err }
	}
	if !parser.symbol("]") { return nil, parser.unexpected() }
	return node, parser.advance()
}

func (parser *jsonPathParser) method(left *jsonPathExpr, name string, depth int) (*jsonPathExpr, error) {
	node, err := parser.add(jpMethod); if err != nil { return nil, err }
	node.left, node.text = left, strings.ToLower(name)
	parser.owned += uint64(len(node.text))
	if err := parser.advance(); err != nil { return nil, err }
	if !parser.symbol(")") {
		for {
			arg, err := parser.expression(1, depth+1); if err != nil { return nil, err }
			before := cap(node.args)
			node.args = append(node.args, arg)
			parser.owned += uint64(cap(node.args)-before) * uint64(unsafe.Sizeof(node.args[0]))
			if !parser.symbol(",") { break }
			if err := parser.advance(); err != nil { return nil, err }
		}
	}
	if !parser.symbol(")") { return nil, parser.unexpected() }
	if err := validateJSONPathMethod(node); err != nil { return nil, err }
	return node, parser.advance()
}

func validateJSONPathMethod(node *jsonPathExpr) error {
	count := len(node.args)
	switch node.text {
	case "abs", "size", "type", "floor", "double", "ceiling", "keyvalue", "bigint", "boolean", "date", "integer", "number", "string":
		if count != 0 { return jsonPathSyntaxError("jsonpath item method accepts no arguments") }
	case "datetime":
		if count > 1 || count == 1 && node.args[0].kind != jpString { return jsonPathSyntaxError(".datetime() accepts an optional string template") }
	case "decimal":
		if count > 2 { return jsonPathSyntaxError(".decimal() can only have an optional precision[,scale]") }
		for _, arg := range node.args { if arg.kind != jpNumber || strings.ContainsAny(arg.text, ".eE") { return jsonPathSyntaxError(".decimal() arguments must be integer literals") } }
	case "time", "time_tz", "timestamp", "timestamp_tz":
		if count > 1 || count == 1 && (node.args[0].kind != jpNumber || strings.ContainsAny(node.args[0].text, ".eE")) { return jsonPathSyntaxError("datetime precision must be an integer literal") }
	default:
		return jsonPathSyntaxError("unknown jsonpath item method ."+node.text+"()")
	}
	return nil
}

func (parser *jsonPathParser) infix() (jsonPathKind, int, int, error) {
	if parser.current.kind == jpTokenSymbol {
		switch parser.current.text {
		case "||": return jpOr, 1, 1, nil
		case "&&": return jpAnd, 2, 1, nil
		case "==": return jpEqual, 3, 1, nil
		case "!=", "<>": return jpNotEqual, 3, 1, nil
		case "<": return jpLess, 3, 1, nil
		case ">": return jpGreater, 3, 1, nil
		case "<=": return jpLessEqual, 3, 1, nil
		case ">=": return jpGreaterEqual, 3, 1, nil
		case "+": return jpAdd, 4, 1, nil
		case "-": return jpSubtract, 4, 1, nil
		case "*": return jpMultiply, 5, 1, nil
		case "/": return jpDivide, 5, 1, nil
		case "%": return jpModulo, 5, 1, nil
		}
	}
	if parser.word("starts") { return jpStartsWith, 3, 1, nil }
	if parser.word("like_regex") { return jpLikeRegex, 3, 1, nil }
	return 0, 0, 0, nil
}

func (parser *jsonPathParser) word(word string) bool { return parser.current.kind == jpTokenWord && strings.EqualFold(parser.current.text, word) }
func (parser *jsonPathParser) symbol(symbol string) bool { return parser.current.kind == jpTokenSymbol && parser.current.text == symbol }
func (parser *jsonPathParser) unexpected() error {
	if parser.current.kind == jpTokenEOF { return jsonPathSyntaxError("syntax error at end of jsonpath input") }
	return jsonPathSyntaxError("syntax error at or near "+strconv.Quote(parser.current.text))
}

// jsonPathRegexpFlags mirrors PostgreSQL's makeItemLikeRegex and
// jspConvertRegexFlags: LIKE_REGEX flags are i, s, m, x, and q, the canonical
// text lists them in that order, and the default is a Spencer ARE with
// REG_NLSTOP, so a dot does not match a newline. XQuery's "x" mode is
// unimplemented; when "q" is given, m, s, and x are ignored per the spec.
func jsonPathRegexpFlags(text string) (string, regexpFlags, error) {
	var ignoreCase, dotAll, multiLine, whiteSpace, quote bool
	for _, flag := range text {
		switch flag {
		case 'i': ignoreCase = true
		case 's': dotAll = true
		case 'm': multiLine = true
		case 'x': whiteSpace = true
		case 'q': quote = true
		default: return "", regexpFlags{}, jsonPathSyntaxError("Unrecognized flag character " + strconv.Quote(string(flag)) + " in LIKE_REGEX predicate")
		}
	}
	if whiteSpace && !quote {
		return "", regexpFlags{}, sqlError("0A000", "XQuery \"x\" flag (expanded regular expressions) is not implemented")
	}
	var canonical strings.Builder
	if ignoreCase { canonical.WriteByte('i') }
	if dotAll { canonical.WriteByte('s') }
	if multiLine { canonical.WriteByte('m') }
	if whiteSpace { canonical.WriteByte('x') }
	if quote { canonical.WriteByte('q') }
	flags := regexpFlags{syntax: regexpARE, ignoreCase: ignoreCase, newlineSensitive: true}
	if quote {
		flags.syntax = regexpQuote
	} else {
		if dotAll { flags.newlineSensitive = false }
		if multiLine { flags.newlineAnchors = true }
	}
	return canonical.String(), flags, nil
}

func validateJSONPathExpr(ctx context.Context, node *jsonPathExpr, filter, subscript bool, depth int) error {
	if err := ctx.Err(); err != nil { return err }
	if node == nil || depth >= jsonPathCompileDepth { return jsonPathSyntaxError("invalid expression tree") }
	if node.kind == jpCurrent && !filter { return jsonPathSyntaxError("@ is not allowed in root expressions") }
	if node.kind == jpLast && !subscript { return jsonPathSyntaxError("LAST is allowed only in array subscripts") }
	childFilter := filter || node.kind == jpFilter
	childSubscript := subscript || node.kind == jpSubscript
	if node.kind == jpFilter && !isJSONPathPredicate(node.right) { return jsonPathSyntaxError("filter expression must be a predicate") }
	if (node.kind == jpAnd || node.kind == jpOr || node.kind == jpNot || node.kind == jpIsUnknown) && !isJSONPathPredicate(node.left) { return jsonPathSyntaxError("boolean operand must be a predicate") }
	if (node.kind == jpAnd || node.kind == jpOr) && !isJSONPathPredicate(node.right) { return jsonPathSyntaxError("boolean operand must be a predicate") }
	if node.kind == jpStartsWith && node.right.kind != jpString && node.right.kind != jpVariable { return jsonPathSyntaxError("starts with operand must be a string or variable") }
	for _, child := range []*jsonPathExpr{node.left, node.right} {
		if child != nil {
			if err := validateJSONPathExpr(ctx, child, childFilter, childSubscript, depth+1); err != nil { return err }
		}
	}
	for _, child := range node.args {
		if err := validateJSONPathExpr(ctx, child, childFilter, childSubscript, depth+1); err != nil { return err }
	}
	for _, item := range node.subscripts {
		if err := validateJSONPathExpr(ctx, item.from, filter, true, depth+1); err != nil { return err }
		if item.to != nil { if err := validateJSONPathExpr(ctx, item.to, filter, true, depth+1); err != nil { return err } }
	}
	return nil
}

func jsonPathCanonicalBytes(ctx context.Context, node *jsonPathExpr, parent, depth int) (uint64, error) {
	if err := ctx.Err(); err != nil { return 0, err }
	if node == nil || depth >= jsonPathCompileDepth { return 0, jsonPathSyntaxError("invalid expression tree") }
	precedence := jsonPathPrecedence(node.kind)
	// PostgreSQL prints jpiNot and jpiIsUnknown with their own delimiters and
	// ignores the caller's bracketing request, so they never take a wrap pair.
	wrap := precedence != 0 && precedence < parent && node.kind != jpNot && node.kind != jpIsUnknown
	size := uint64(0)
	quoted := func(text string) (uint64, error) {
		bytes, err := jsonQuotedBytes(ctx, text)
		if err != nil { return 0, err }
		return uint64(bytes), nil
	}
	child := func(value *jsonPathExpr, priority int) (uint64, error) { return jsonPathCanonicalBytes(ctx, value, priority, depth+1) }
	add := func(value uint64) error {
		if value > math.MaxUint64-size { return jsonPathResourceError() }
		size += value
		return nil
	}
	switch node.kind {
	case jpNull: size = 4
	case jpString:
		value, err := quoted(node.text); if err != nil { return 0, err }; size = value
	case jpNumber: size = uint64(len(node.text))
	case jpBoolean: if node.boolean { size = 4 } else { size = 5 }
	case jpRoot, jpCurrent: size = 1
	case jpVariable:
		value, err := quoted(node.text); if err != nil { return 0, err }; size = value + 1
	case jpLast: size = 4
	case jpMember:
		value, err := child(node.left, 7); if err != nil { return 0, err }; name, err := quoted(node.text); if err != nil { return 0, err }; size = value + name + 1
	case jpAnyMember:
		value, err := child(node.left, 7); if err != nil { return 0, err }; size = value + 2
	case jpAnyArray:
		value, err := child(node.left, 7); if err != nil { return 0, err }; size = value + 3
	case jpRecursive:
		value, err := child(node.left, 7); if err != nil { return 0, err }; size = value + 3
		if node.bounds {
			first := len(strconv.Itoa(node.first)); if node.first < 0 { first = 4 }
			last := len(strconv.Itoa(node.last)); if node.last < 0 { last = 4 }
			size += uint64(first + 2)
			if node.last != node.first { size += uint64(last + len(" to ")) }
		}
	case jpSubscript:
		value, err := child(node.left, 7); if err != nil { return 0, err }; size = value + 2
		for index, item := range node.subscripts {
			if index != 0 { size += 2 }
			value, err = child(item.from, 0); if err != nil { return 0, err }; if err := add(value); err != nil { return 0, err }
			if item.to != nil { value, err = child(item.to, 0); if err != nil { return 0, err }; if err := add(uint64(len(" to "))+value); err != nil { return 0, err } }
		}
	case jpFilter:
		left, err := child(node.left, 7); if err != nil { return 0, err }; right, err := child(node.right, 1); if err != nil { return 0, err }; size = left + right + 3
	case jpMethod:
		value, err := child(node.left, 7); if err != nil { return 0, err }; size = value + uint64(len(node.text)) + 3
		for index, argument := range node.args { if index != 0 { size += 2 }; value, err = child(argument, 0); if err != nil { return 0, err }; if err := add(value); err != nil { return 0, err } }
	case jpExists:
		value, err := child(node.left, 0); if err != nil { return 0, err }; size = value + uint64(len("exists ()"))
	case jpNot, jpIsUnknown, jpPlus, jpMinus:
		priority := precedence
		if node.kind == jpNot || node.kind == jpIsUnknown { priority = 0 }
		value, err := child(node.left, priority); if err != nil { return 0, err }; size = value
		if node.kind == jpNot { size += 3 } else if node.kind == jpIsUnknown { size += uint64(len("(") + len(") is unknown")) } else { size++ }
	case jpLikeRegex:
		left, err := child(node.left, precedence); if err != nil { return 0, err }; pattern, err := quoted(node.text); if err != nil { return 0, err }; size = left + pattern + uint64(len(" like_regex "))
		if node.regexFlagText != "" { flags, err := quoted(node.regexFlagText); if err != nil { return 0, err }; size += flags + uint64(len(" flag ")) }
	default:
		left, err := child(node.left, precedence); if err != nil { return 0, err }; right, err := child(node.right, precedence+1); if err != nil { return 0, err }; size = left + right + uint64(len(jsonPathOperator(node.kind))) + 2
	}
	if wrap { if err := add(2); err != nil { return 0, err } }
	return size, nil
}

func appendJSONPathCanonical(ctx context.Context, output *strings.Builder, node *jsonPathExpr, parent int) error {
	if err := ctx.Err(); err != nil { return err }
	if node == nil { return jsonPathSyntaxError("invalid expression tree") }
	precedence := jsonPathPrecedence(node.kind)
	// PostgreSQL prints jpiNot and jpiIsUnknown with their own delimiters and
	// ignores the caller's bracketing request, so they never take a wrap pair.
	wrap := precedence != 0 && precedence < parent && node.kind != jpNot && node.kind != jpIsUnknown
	if wrap { output.WriteByte('(') }
	switch node.kind {
	case jpNull: output.WriteString("null")
	case jpString:
		if err := appendJSONQuoted(ctx, output, node.text); err != nil { return err }
	case jpNumber: output.WriteString(node.text)
	case jpBoolean: output.WriteString(strconv.FormatBool(node.boolean))
	case jpRoot: output.WriteByte('$')
	case jpCurrent: output.WriteByte('@')
	case jpVariable:
		output.WriteByte('$'); if err := appendJSONQuoted(ctx, output, node.text); err != nil { return err }
	case jpLast: output.WriteString("last")
	case jpMember:
		if err := appendJSONPathCanonical(ctx, output, node.left, 7); err != nil { return err }
		output.WriteByte('.'); if err := appendJSONQuoted(ctx, output, node.text); err != nil { return err }
	case jpAnyMember:
		if err := appendJSONPathCanonical(ctx, output, node.left, 7); err != nil { return err }; output.WriteString(".*")
	case jpAnyArray:
		if err := appendJSONPathCanonical(ctx, output, node.left, 7); err != nil { return err }; output.WriteString("[*]")
	case jpRecursive:
		if err := appendJSONPathCanonical(ctx, output, node.left, 7); err != nil { return err }; output.WriteString(".**")
		if node.bounds { output.WriteByte('{'); appendJSONPathLevel(output, node.first); if node.last != node.first { output.WriteString(" to "); appendJSONPathLevel(output, node.last) }; output.WriteByte('}') }
	case jpSubscript:
		if err := appendJSONPathCanonical(ctx, output, node.left, 7); err != nil { return err }; output.WriteByte('[')
		for i, item := range node.subscripts { if i != 0 { output.WriteString(", ") }; if err := appendJSONPathCanonical(ctx, output, item.from, 0); err != nil { return err }; if item.to != nil { output.WriteString(" to "); if err := appendJSONPathCanonical(ctx, output, item.to, 0); err != nil { return err } } }
		output.WriteByte(']')
	case jpFilter:
		if err := appendJSONPathCanonical(ctx, output, node.left, 7); err != nil { return err }; output.WriteString("?("); if err := appendJSONPathCanonical(ctx, output, node.right, 1); err != nil { return err }; output.WriteByte(')')
	case jpMethod:
		if err := appendJSONPathCanonical(ctx, output, node.left, 7); err != nil { return err }; output.WriteByte('.'); output.WriteString(node.text); output.WriteByte('(')
		for i, arg := range node.args { if i != 0 { output.WriteString(", ") }; if err := appendJSONPathCanonical(ctx, output, arg, 0); err != nil { return err } }; output.WriteByte(')')
	case jpExists:
		output.WriteString("exists ("); if err := appendJSONPathCanonical(ctx, output, node.left, 0); err != nil { return err }; output.WriteByte(')')
	case jpNot, jpIsUnknown, jpPlus, jpMinus:
		if node.kind == jpNot { output.WriteString("!(") } else if node.kind == jpIsUnknown { output.WriteByte('(') } else if node.kind == jpPlus { output.WriteByte('+') } else if node.kind == jpMinus { output.WriteByte('-') }
		childParent := precedence
		if node.kind == jpNot || node.kind == jpIsUnknown { childParent = 0 }
		if err := appendJSONPathCanonical(ctx, output, node.left, childParent); err != nil { return err }
		if node.kind == jpNot { output.WriteByte(')') } else if node.kind == jpIsUnknown { output.WriteString(") is unknown") }
	case jpLikeRegex:
		if err := appendJSONPathCanonical(ctx, output, node.left, precedence); err != nil { return err }; output.WriteString(" like_regex "); if err := appendJSONQuoted(ctx, output, node.text); err != nil { return err }; if node.regexFlagText != "" { output.WriteString(" flag "); if err := appendJSONQuoted(ctx, output, node.regexFlagText); err != nil { return err } }
	default:
		if err := appendJSONPathCanonical(ctx, output, node.left, precedence); err != nil { return err }
		output.WriteByte(' '); output.WriteString(jsonPathOperator(node.kind)); output.WriteByte(' ')
		if err := appendJSONPathCanonical(ctx, output, node.right, precedence+1); err != nil { return err }
	}
	if wrap { output.WriteByte(')') }
	return ctx.Err()
}
func isJSONPathPredicate(node *jsonPathExpr) bool {
	if node == nil { return false }
	switch node.kind {
	case jpExists, jpEqual, jpNotEqual, jpLess, jpGreater, jpLessEqual, jpGreaterEqual, jpStartsWith, jpLikeRegex:
		return true
	case jpNot, jpIsUnknown:
		return isJSONPathPredicate(node.left)
	case jpAnd, jpOr:
		return isJSONPathPredicate(node.left) && isJSONPathPredicate(node.right)
	}
	return false
}


func jsonPathPrecedence(kind jsonPathKind) int {
	switch kind { case jpOr: return 1; case jpAnd: return 2; case jpEqual, jpNotEqual, jpLess, jpGreater, jpLessEqual, jpGreaterEqual, jpStartsWith, jpLikeRegex, jpIsUnknown: return 3; case jpAdd, jpSubtract: return 4; case jpMultiply, jpDivide, jpModulo: return 5; case jpPlus, jpMinus, jpNot: return 6; default: return 0 }
}
func jsonPathOperator(kind jsonPathKind) string {
	switch kind { case jpAnd: return "&&"; case jpOr: return "||"; case jpEqual: return "=="; case jpNotEqual: return "!="; case jpLess: return "<"; case jpGreater: return ">"; case jpLessEqual: return "<="; case jpGreaterEqual: return ">="; case jpStartsWith: return "starts with"; case jpAdd: return "+"; case jpSubtract: return "-"; case jpMultiply: return "*"; case jpDivide: return "/"; case jpModulo: return "%" }; return "?"
}
func appendJSONPathLevel(output *strings.Builder, level int) { if level < 0 { output.WriteString("last") } else { output.WriteString(strconv.Itoa(level)) } }

