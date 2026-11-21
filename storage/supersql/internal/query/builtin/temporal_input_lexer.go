package builtin

import (
	"context"
	"strings"
)

type temporalInputKind uint8

const (
	temporalInputCivil temporalInputKind = iota
	temporalInputNow
	temporalInputToday
	temporalInputYesterday
	temporalInputTomorrow
	temporalInputEpoch
	temporalInputPositiveInfinity
	temporalInputNegativeInfinity
	temporalInputAllBalls
)

type temporalInputFields struct {
	Year             int64
	Month, Day       int
	TimeMicros       int64
	HasDate, HasTime bool
	Zone             string
	HasZone          bool
	Kind             temporalInputKind
}
type temporalToken struct {
	text string
	kind byte
}

func temporalBadFormat() error  { return sqlError("22007", "invalid input syntax for date/time") }
func temporalOverflow() error   { return sqlError("22008", "date/time field value out of range") }
func temporalAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func temporalCore(s string) bool {
	switch strings.ToLower(s) {
	case "j", "jd", "julian", "t", "at", "on", "am", "pm", "ad", "bc", "now", "today", "tomorrow", "yesterday", "epoch", "infinity", "allballs":
		return true
	case "y", "m", "mm", "d", "h", "s", "dow", "doy", "isodow", "isoyear", "dst":
		return true
	case "sun", "sunday", "mon", "monday", "tue", "tues", "tuesday", "wed", "weds", "wednesday", "thu", "thur", "thurs", "thursday", "fri", "friday", "sat", "saturday":
		return true
	}
	return temporalMonth(s) != 0
}
func temporalLex(ctx context.Context, text string, tokens *[25]temporalToken) (int, error) {
	count, used := 0, 0
	for position := 0; position < len(text) && text[position] != 0; {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if isTSSpace(text[position]) {
			position++
			continue
		}
		if count == len(tokens) {
			return 0, temporalBadFormat()
		}
		token, next, err := scanTemporalToken(ctx, text, position)
		if err != nil {
			return 0, err
		}
		position = next
		if token.kind == 0 {
			continue
		}
		used += len(token.text) + 1
		if used > 153 {
			return 0, temporalBadFormat()
		}
		tokens[count] = token
		count++
	}
	return count, nil
}

func temporalTokenAvailable(text string, position, limit int) bool {
	return position < limit && text[position] != 0
}

func temporalDigitsEnd(text string, position, limit int) int {
	for temporalTokenAvailable(text, position, limit) && intervalInputDigit(text[position]) {
		position++
	}
	return position
}

func temporalDateSeparator(value byte) bool {
	return value == '-' || value == '/' || value == '.'
}

func temporalTimeCharacter(value byte) bool {
	return intervalInputDigit(value) || value == ':' || value == '.'
}

func temporalZoneCharacter(value byte) bool {
	return temporalAlpha(value) || intervalInputDigit(value) || strings.ContainsRune("+-/_.:", rune(value))
}

func scanTemporalToken(ctx context.Context, text string, start int) (temporalToken, int, error) {
	limit := min(len(text), start+154)
	value := text[start]
	switch {
	case intervalInputDigit(value):
		position, kind := scanTemporalNumber(text, start, limit)
		return temporalToken{text[start:position], kind}, position, nil
	case temporalAlpha(value):
		position, kind := scanTemporalWord(text, start, limit)
		return temporalToken{text[start:position], kind}, position, nil
	case value == '+' || value == '-':
		return scanTemporalSign(ctx, text, start)
	case value == '.':
		position := temporalDigitsEnd(text, start+1, limit)
		return temporalToken{text[start:position], 'n'}, position, nil
	case value > 32 && value < 127:
		return temporalToken{}, start + 1, nil
	default:
		return temporalToken{}, start, temporalBadFormat()
	}
}

func scanTemporalNumber(text string, start, limit int) (int, byte) {
	position := temporalDigitsEnd(text, start, limit)
	if !temporalTokenAvailable(text, position, limit) {
		return position, 'n'
	}
	if text[position] == ':' {
		position++
		for temporalTokenAvailable(text, position, limit) && temporalTimeCharacter(text[position]) {
			position++
		}
		return position, 't'
	}
	if !temporalDateSeparator(text[position]) {
		return position, 'n'
	}
	delimiter := text[position]
	position++
	if !temporalTokenAvailable(text, position, limit) || !intervalInputDigit(text[position]) {
		for temporalTokenAvailable(text, position, limit) {
			value := text[position]
			if !intervalInputDigit(value) && !temporalAlpha(value) && value != delimiter {
				break
			}
			position++
		}
		return position, 'd'
	}
	kind := byte('d')
	if delimiter == '.' {
		kind = 'n'
	}
	position = temporalDigitsEnd(text, position, limit)
	if temporalTokenAvailable(text, position, limit) && text[position] == delimiter {
		kind = 'd'
		position++
		for temporalTokenAvailable(text, position, limit) && (intervalInputDigit(text[position]) || text[position] == delimiter) {
			position++
		}
	}
	return position, kind
}

func scanTemporalWord(text string, start, limit int) (int, byte) {
	position := start
	for temporalTokenAvailable(text, position, limit) && temporalAlpha(text[position]) {
		position++
	}
	if !temporalTokenAvailable(text, position, limit) {
		return position, 's'
	}
	next := text[position]
	date := temporalDateSeparator(next)
	zoneSuffix := next == '+' || intervalInputDigit(next)
	if !date && (!zoneSuffix || temporalCore(text[start:position])) {
		return position, 's'
	}
	position++
	for temporalTokenAvailable(text, position, limit) && temporalZoneCharacter(text[position]) {
		position++
	}
	return position, 'd'
}

func scanTemporalSign(ctx context.Context, text string, start int) (temporalToken, int, error) {
	position := start + 1
	for position < len(text) && isTSSpace(text[position]) {
		if position%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return temporalToken{}, position, err
			}
		}
		position++
	}
	body := position
	limit := min(len(text), body+153)
	if !temporalTokenAvailable(text, position, limit) {
		return temporalToken{}, position, temporalBadFormat()
	}
	kind := byte('z')
	if temporalAlpha(text[position]) {
		kind = 's'
		for temporalTokenAvailable(text, position, limit) && temporalAlpha(text[position]) {
			position++
		}
	} else {
		if !intervalInputDigit(text[position]) {
			return temporalToken{}, position, temporalBadFormat()
		}
		for temporalTokenAvailable(text, position, limit) {
			value := text[position]
			if !intervalInputDigit(value) && !strings.ContainsRune(":.-", rune(value)) {
				break
			}
			position++
		}
	}
	token := text[start:position]
	if body != start+1 {
		token = text[start:start+1] + text[body:position]
	}
	return temporalToken{token, kind}, position, nil
}
func temporalMonth(s string) int {
	names := [12]string{"january", "february", "march", "april", "may", "june", "july", "august", "september", "october", "november", "december"}
	for i, name := range names {
		septemberAlias := i == 8 && timezoneEqual(s, "sept")
		if timezoneEqual(s, name) || timezoneEqual(s, name[:3]) || septemberAlias {
			return i + 1
		}
	}
	return 0
}
