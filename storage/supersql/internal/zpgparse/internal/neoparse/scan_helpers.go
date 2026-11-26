package neoparse

import (
	"strconv"
	"strings"
)

func nestedCommentEnd(data string, start int) (int, bool) {
	depth := 1
	position := start + 2
	for position < len(data) && depth > 0 {
		switch {
		case position+1 < len(data) && data[position] == '/' && data[position+1] == '*':
			depth++
			position += 2
		case position+1 < len(data) && data[position] == '*' && data[position+1] == '/':
			depth--
			position += 2
		default:
			position++
		}
	}
	return position, depth == 0
}

func operatorPrefix(operator string) string {
	slashstar, dashdash := strings.Index(operator, "/*"), strings.Index(operator, "--")
	cut := slashstar
	if slashstar < 0 {
		cut = dashdash
	} else if dashdash >= 0 {
		cut = min(slashstar, dashdash)
	}
	if cut > 0 {
		operator = operator[:cut]
	}
	length := len(operator)
	if length <= 1 {
		return operator
	}
	if operator[length-1] != '+' && operator[length-1] != '-' {
		return operator
	}
	if strings.ContainsAny(operator[:length-1], "~!@#^&|`?%") {
		return operator
	}
	for length > 1 && (operator[length-1] == '+' || operator[length-1] == '-') {
		length--
	}
	return operator[:length]
}

func operatorToken(operator string, value *yySymType) int {
	if len(operator) == 1 && strings.ContainsRune(",()[].;:+-*/%^<>=", rune(operator[0])) {
		return int(operator[0])
	}
	switch operator {
	case "=>":
		return EQUALS_GREATER
	case ">=":
		return GREATER_EQUALS
	case "<=":
		return LESS_EQUALS
	case "<>", "!=":
		return NOT_EQUALS
	}
	if value != nil {
		value.setstr(operator)
	}
	return Op
}

func unicodeEscapeAt(data string, position int) bool {
	if position+1 >= len(data) || data[position] != '\\' {
		return false
	}
	return data[position+1] == 'u' || data[position+1] == 'U'
}

func unicodeQuotedIdentifier(value string) bool {
	if len(value) < 4 {
		return false
	}
	prefix := value[0] == 'u' || value[0] == 'U'
	return prefix && value[1] == '&' && value[2] == '"' && value[len(value)-1] == '"'
}

func integerToken(raw string, base, prefix int, value *yySymType) int {
	parsed, err := strconv.ParseInt(strings.ReplaceAll(raw[prefix:], "_", ""), base, 32)
	if err != nil {
		if value != nil {
			value.setstr(raw)
		}
		return FCONST
	}
	if value != nil {
		value.setint(parsed)
	}
	return ICONST
}
