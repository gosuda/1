package builtin

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type tsParsedToken struct {
	typ    uint16
	text   string
	start  int
	length int
}

type tsLexizedToken struct {
	lexeme  string
	position int
}

const tsTokenTypeCount = 23

func tsTokenInfo(token uint16) (string, string) {
	switch token {
	case 1: return "asciiword", "Word, all ASCII"
	case 2: return "word", "Word, all letters"
	case 3: return "numword", "Word, letters and digits"
	case 4: return "email", "Email address"
	case 5: return "url", "URL"
	case 6: return "host", "Host"
	case 7: return "sfloat", "Scientific notation"
	case 8: return "version", "Version number"
	case 9: return "hword_numpart", "Hyphenated word part, letters and digits"
	case 10: return "hword_part", "Hyphenated word part, all letters"
	case 11: return "hword_asciipart", "Hyphenated word part, all ASCII"
	case 12: return "blank", "Space symbols"
	case 13: return "tag", "XML tag"
	case 14: return "protocol", "Protocol head"
	case 15: return "numhword", "Hyphenated word, letters and digits"
	case 16: return "asciihword", "Hyphenated word, all ASCII"
	case 17: return "hword", "Hyphenated word, all letters"
	case 18: return "url_path", "URL path"
	case 19: return "file", "File or path name"
	case 20: return "float", "Decimal notation"
	case 21: return "int", "Signed integer"
	case 22: return "uint", "Unsigned integer"
	case 23: return "entity", "XML entity"
	default: return "", ""
	}
}

func parseDefaultText(ctx context.Context, text string, maxTokens int) ([]tsParsedToken, error) {
	if !utf8.ValidString(text) { return nil, sqlError("22021", "invalid UTF-8 in text search input") }
	tokens := make([]tsParsedToken, 0, min(len(text)/4+1, maxTokens))
	appendToken := func(typ uint16, value string, start, length int) error {
		if len(tokens) >= maxTokens { return sqlError("54000", "text search token limit exceeded") }
		tokens = append(tokens, tsParsedToken{typ: typ, text: value, start: start, length: length})
		return nil
	}
	for offset := 0; offset < len(text); {
		if err := ctx.Err(); err != nil { return nil, err }
		start := offset
		r, width := utf8.DecodeRuneInString(text[offset:])
		if unicode.IsSpace(r) {
			offset += width
			for offset < len(text) { next, n := utf8.DecodeRuneInString(text[offset:]); if !unicode.IsSpace(next) { break }; offset += n }
			if err := appendToken(12, text[start:offset], start, offset-start); err != nil { return nil, err }
			continue
		}
		if text[offset] == '<' {
			if end := strings.IndexByte(text[offset:], '>'); end >= 0 {
				offset += end+1
				if err := appendToken(13, text[start:offset], start, offset-start); err != nil { return nil, err }
				continue
			}
		}
		if text[offset] == '&' {
			if end := strings.IndexByte(text[offset:], ';'); end > 1 {
				offset += end+1
				if err := appendToken(23, text[start:offset], start, offset-start); err != nil { return nil, err }
				continue
			}
		}
		if !tsTokenRune(r) {
			offset += width
			continue
		}
		offset += width
		for offset < len(text) {
			next, n := utf8.DecodeRuneInString(text[offset:])
			if !tsTokenRune(next) { break }
			offset += n
		}
		candidate := strings.Trim(text[start:offset], ".:@/~_")
		trimmedStart := strings.Index(text[start:offset], candidate)
		if candidate == "" { continue }
		start += trimmedStart
		kind := classifyTSToken(candidate)
		if err := appendToken(kind, candidate, start, len(candidate)); err != nil { return nil, err }
		if strings.IndexRune(candidate, '-') > 0 {
			partStart := start
			for _, part := range strings.Split(candidate, "-") {
				if part != "" {
					partKind := classifyTSHyphenPart(part)
					if err := appendToken(partKind, part, partStart, len(part)); err != nil { return nil, err }
				}
				partStart += len(part)+1
			}
		}
	}
	return tokens, nil
}

func tsTokenRune(r rune) bool {
	if unicode.IsLetter(r) || unicode.IsDigit(r) { return true }
	return strings.ContainsRune("._%+@:/~-", r)
}

func classifyTSToken(token string) uint16 {
	lower := strings.ToLower(token)
	if strings.Contains(lower, "://") {
		if parsed, err := url.Parse(lower); err == nil && parsed.Scheme != "" && parsed.Host != "" { return 5 }
	}
	if at := strings.LastIndexByte(token, '@'); at > 0 && at+1 < len(token) && strings.ContainsRune(token[at+1:], '.') { return 4 }
	if strings.ContainsRune(token, '/') {
		if strings.HasPrefix(token, "/") || strings.HasPrefix(token, "./") || strings.HasPrefix(token, "../") || strings.ContainsRune(token, '.') { return 19 }
		return 18
	}
	if strings.IndexRune(token, '-') > 0 {
		letters, digits, ascii := tsCharacterClasses(token)
		if letters && digits { return 15 }; if ascii { return 16 }; return 17
	}
	if strings.ContainsRune(lower, 'e') {
		if _, err := strconv.ParseFloat(token, 64); err == nil { return 7 }
	}
	letters, digits, ascii := tsCharacterClasses(token)
	if letters && strings.ContainsRune(token, '.') { return 6 }
	if letters && !digits { if ascii { return 1 }; return 2 }
	if letters && digits { return 3 }
	if strings.Count(token, ".") > 1 { return 8 }
	if strings.ContainsRune(token, '.') { return 20 }
	if strings.HasPrefix(token, "+") || strings.HasPrefix(token, "-") { return 21 }
	if digits { return 22 }
	return 1
}

func classifyTSHyphenPart(token string) uint16 {
	letters, digits, ascii := tsCharacterClasses(token)
	if letters && digits { return 9 }; if ascii { return 11 }; return 10
}

func tsCharacterClasses(token string) (letters, digits, ascii bool) {
	ascii = true
	for _, r := range token {
		if r >= utf8.RuneSelf { ascii = false }
		letters = letters || unicode.IsLetter(r)
		digits = digits || unicode.IsDigit(r)
	}
	return letters, digits, ascii
}

func textSearchMapping(configuration model.TextSearchConfiguration, token uint16) []model.TextSearchDictionary {
	for _, mapping := range configuration.Mappings { if mapping.TokenType == token { return mapping.Dictionaries } }
	return nil
}

func lexizeText(ctx context.Context, configuration model.TextSearchConfiguration, text string, maxTokens int) ([]tsLexizedToken, error) {
	if configuration.Parser != tsDefaultParserOID { return nil, sqlError("0A000", "unsupported native text search parser") }
	tokens, err := parseDefaultText(ctx, text, maxTokens)
	if err != nil { return nil, err }
	out := make([]tsLexizedToken, 0, len(tokens))
	position := 0
	for _, token := range tokens {
		dictionaries := textSearchMapping(configuration, token.typ)
		if len(dictionaries) == 0 { continue }
		position++
		for _, dictionary := range dictionaries {
			lexemes, err := lexizeTextSearch(ctx, dictionary, token.text)
			if err != nil { return nil, err }
			if lexemes == nil { continue }
			for _, lexeme := range lexemes {
				if len(lexeme) > tsMaxLexemeBytes { return nil, sqlError("54000", "text search lexeme is too long") }
				out = append(out, tsLexizedToken{lexeme: lexeme, position: min(position, tsMaxPosition)})
				if len(out) > maxTokens { return nil, sqlError("54000", "text search token limit exceeded") }
			}
			break
		}
	}
	return out, nil
}

func toTSVector(ctx context.Context, configuration model.TextSearchConfiguration, text string, budget int64) ([]byte, error) {
	maxTokens := max(1, min(len(text)+1, tsMaxDataBytes))
	words, err := lexizeText(ctx, configuration, text, maxTokens)
	if err != nil { return nil, err }
	lexemes := make([]tsLexeme, 0, len(words))
	for _, word := range words { lexemes = append(lexemes, tsLexeme{text: word.lexeme, positions: []tsPosition{makeTSPosition(word.position, 0)}}) }
	return encodeTSVector(ctx, normalizeTSVector(lexemes), budget)
}

func plainTSQuery(ctx context.Context, configuration model.TextSearchConfiguration, text string, phrase bool, budget int64) ([]byte, error) {
	words, err := lexizeText(ctx, configuration, text, max(1, min(len(text)+1, tsMaxDataBytes)))
	if err != nil { return nil, err }
	var root *tsQueryNode
	lastPosition := 0
	for _, word := range words {
		node := &tsQueryNode{kind: tsQueryValue, word: word.lexeme}
		if root == nil { root = node } else if phrase { root = joinTSQuery(tsQueryPhrase, uint16(word.position-lastPosition), root, node) } else { root = joinTSQuery(tsQueryAnd, 0, root, node) }
		lastPosition = word.position
	}
	return encodeTSQuery(ctx, root, budget)
}

func morphTSQuery(ctx context.Context, configuration model.TextSearchConfiguration, data []byte, budget int64) ([]byte, error) {
	root, _, err := decodeTSQuery(ctx, data)
	if err != nil { return nil, err }
	var morph func(*tsQueryNode) (*tsQueryNode, error)
	morph = func(node *tsQueryNode) (*tsQueryNode, error) {
		if node == nil { return nil, nil }
		if node.kind == tsQueryValue {
			words, err := lexizeText(ctx, configuration, node.word, max(1, len(node.word)+1))
			if err != nil { return nil, err }
			var result *tsQueryNode
			for _, word := range words {
				value := &tsQueryNode{kind: tsQueryValue, word: word.lexeme, weight: node.weight, prefix: node.prefix}
				result = joinTSQuery(tsQueryAnd, 0, result, value)
			}
			return result, nil
		}
		right, err := morph(node.right); if err != nil { return nil, err }
		if node.op == tsQueryNot { if right == nil { return nil, nil }; return &tsQueryNode{kind: tsQueryOp, op: tsQueryNot, right: right}, nil }
		left, err := morph(node.left); if err != nil { return nil, err }
		if left == nil { return right, nil }; if right == nil { return left, nil }
		return &tsQueryNode{kind: tsQueryOp, op: node.op, distance: node.distance, left: left, right: right}, nil
	}
	root, err = morph(root)
	if err != nil { return nil, err }
	return encodeTSQuery(ctx, root, budget)
}

func websearchTSQuery(ctx context.Context, configuration model.TextSearchConfiguration, text string, budget int64) ([]byte, error) {
	var root *tsQueryNode
	negate, pendingOr := false, false
	for offset := 0; offset < len(text); {
		for offset < len(text) && isTSSpace(text[offset]) { offset++ }
		if offset == len(text) { break }
		if text[offset] == '-' { negate = true; offset++; continue }
		if offset+2 <= len(text) && strings.EqualFold(text[offset:offset+2], "or") && (offset+2 == len(text) || isTSSpace(text[offset+2])) { pendingOr = true; offset += 2; continue }
		start, phrase := offset, false
		if text[offset] == '"' { phrase = true; start = offset+1; offset = start; for offset < len(text) && text[offset] != '"' { offset++ } } else { for offset < len(text) && !isTSSpace(text[offset]) { offset++ } }
		part := text[start:offset]; if phrase && offset < len(text) { offset++ }
		query, err := plainTSQuery(ctx, configuration, part, phrase, int64(tsMaxDataBytes)); if err != nil { return nil, err }
		node, _, err := decodeTSQuery(ctx, query); if err != nil { return nil, err }
		if node == nil { negate = false; continue }
		if negate { node = &tsQueryNode{kind: tsQueryOp, op: tsQueryNot, right: node}; negate = false }
		if root == nil { root = node } else if pendingOr { root = joinTSQuery(tsQueryOr, 0, root, node); pendingOr = false } else { root = joinTSQuery(tsQueryAnd, 0, root, node) }
	}
	return encodeTSQuery(ctx, root, budget)
}
