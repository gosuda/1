package builtin

import "context"

type intervalInputFieldKind uint8

const (
	intervalInputNumberField intervalInputFieldKind = iota
	intervalInputDateField
	intervalInputTimeField
	intervalInputTextField
	intervalInputSignedField
	intervalInputSpecialField
)

type intervalInputField struct {
	text string
	kind intervalInputFieldKind
	sign byte
}

type intervalInputFields struct {
	fields [25]intervalInputField
	count  int
}

// The cursor borrows its text only during one invocation. PostgreSQL's cstring
// grammar stops at NUL; skipped whitespace still participates in cancellation.
type intervalInputCursor struct {
	text      string
	position  int
	nextCheck int
	err       error
}

func (cursor *intervalInputCursor) peek() byte {
	if cursor.err != nil || cursor.position >= len(cursor.text) {
		return 0
	}
	return cursor.text[cursor.position]
}

func (cursor *intervalInputCursor) advance(ctx context.Context) {
	cursor.position++
	if cursor.position >= cursor.nextCheck {
		cursor.nextCheck = cursor.position + cancellationChunk
		cursor.err = ctx.Err()
	}
}

func intervalInputDigit(c byte) bool { return c >= '0' && c <= '9' }
func intervalInputAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func intervalInputAlnum(c byte) bool { return intervalInputDigit(c) || intervalInputAlpha(c) }
func intervalInputPunct(c byte) bool { return c >= 33 && c <= 126 && !intervalInputAlnum(c) }

func intervalInputKeyword(text string) [10]byte {
	var lower [10]byte
	n := min(len(text), len(lower))
	for i := range n {
		c := text[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lower[i] = c
	}
	return lower
}

// Only the core date table controls whether letters followed by digits remain
// separate fields. Interval-unit recognition happens later in DecodeInterval.
func intervalInputCoreKeyword(text string) bool {
	lower := intervalInputKeyword(text)
	switch string(lower[:min(len(text), len(lower))]) {
	case "ad", "allballs", "am", "apr", "april", "at", "aug", "august", "bc", "d", "dec", "december", "dow", "doy", "dst", "epoch", "feb", "february", "fri", "friday", "h", "infinity", "isodow", "isoyear", "j", "jan", "january", "jd", "jul", "julian", "july", "jun", "june", "m", "mar", "march", "may", "mm", "mon", "monday", "nov", "november", "now", "oct", "october", "on", "pm", "s", "sat", "saturday", "sep", "sept", "september", "sun", "sunday", "t", "thu", "thur", "thurs", "thursday", "today", "tomorrow", "tue", "tues", "tuesday", "wed", "wednesday", "weds", "y", "yesterday":
		return true
	default:
		return false
	}
}

func (fields *intervalInputFields) scan(ctx context.Context, text string) (intervalInputStatus, error) {
	scanner := intervalInputScanner{intervalInputCursor: intervalInputCursor{text: text}, valid: true}
	for scanner.peek() != 0 {
		c := scanner.peek()
		if isTSSpace(c) {
			scanner.advance(ctx)
			continue
		}
		if fields.count == len(fields.fields) {
			return intervalInputBadFormat, scanner.err
		}
		if intervalInputPunct(c) && c != '.' && c != '+' && c != '-' {
			scanner.advance(ctx)
			continue
		}
		field := scanner.field(ctx)
		if !scanner.valid {
			return intervalInputBadFormat, scanner.err
		}
		fields.fields[fields.count] = field
		fields.count++
		scanner.used++
	}
	return intervalInputOK, scanner.err
}

type intervalInputScanner struct {
	intervalInputCursor
	used  int
	valid bool
}

func (scanner *intervalInputScanner) take(ctx context.Context) bool {
	if scanner.used+1 >= 256 {
		scanner.valid = false
		return false
	}
	scanner.used++
	scanner.advance(ctx)
	return true
}

func (scanner *intervalInputScanner) takeWhile(ctx context.Context, matches func(byte) bool) {
	for matches(scanner.peek()) {
		if !scanner.take(ctx) {
			return
		}
	}
}

func (scanner *intervalInputScanner) field(ctx context.Context) intervalInputField {
	start := scanner.position
	field := intervalInputField{}
	c := scanner.peek()
	switch {
	case intervalInputDigit(c):
		field.kind = scanner.number(ctx)
	case c == '.':
		if scanner.take(ctx) {
			scanner.takeWhile(ctx, intervalInputDigit)
		}
	case intervalInputAlpha(c):
		scanner.takeWhile(ctx, intervalInputAlpha)
		field.kind = intervalInputTextField
		if scanner.dateContinuation(scanner.text[start:scanner.position]) {
			field.kind = intervalInputDateField
			scanner.takeWhile(ctx, intervalInputDateByte)
		}
	case c == '+' || c == '-':
		field.sign = c
		if !scanner.take(ctx) {
			return field
		}
		for isTSSpace(scanner.peek()) {
			scanner.advance(ctx)
		}
		start = scanner.position
		field.kind = scanner.signed(ctx)
	default:
		scanner.valid = false
	}
	field.text = scanner.text[start:scanner.position]
	return field
}

func (scanner *intervalInputScanner) signed(ctx context.Context) intervalInputFieldKind {
	if intervalInputDigit(scanner.peek()) {
		scanner.takeWhile(ctx, func(c byte) bool { return intervalInputDigit(c) || c == ':' || c == '.' || c == '-' })
		return intervalInputSignedField
	}
	if intervalInputAlpha(scanner.peek()) {
		scanner.takeWhile(ctx, intervalInputAlpha)
		return intervalInputSpecialField
	}
	scanner.valid = false
	return intervalInputSignedField
}

func (scanner *intervalInputScanner) number(ctx context.Context) intervalInputFieldKind {
	scanner.takeWhile(ctx, intervalInputDigit)
	delimiter := scanner.peek()
	if delimiter == ':' {
		scanner.takeWhile(ctx, func(c byte) bool { return intervalInputDigit(c) || c == ':' || c == '.' })
		return intervalInputTimeField
	}
	if delimiter != '-' && delimiter != '/' && delimiter != '.' {
		return intervalInputNumberField
	}
	if !scanner.take(ctx) {
		return intervalInputDateField
	}
	if !intervalInputDigit(scanner.peek()) {
		scanner.takeWhile(ctx, func(c byte) bool { return intervalInputAlnum(c) || c == delimiter })
		return intervalInputDateField
	}
	kind := intervalInputDateField
	if delimiter == '.' {
		kind = intervalInputNumberField
	}
	scanner.takeWhile(ctx, intervalInputDigit)
	if scanner.peek() == delimiter {
		kind = intervalInputDateField
		scanner.takeWhile(ctx, func(c byte) bool { return intervalInputDigit(c) || c == delimiter })
	}
	return kind
}

func (scanner *intervalInputScanner) dateContinuation(word string) bool {
	c := scanner.peek()
	if c == '+' || intervalInputDigit(c) {
		return !intervalInputCoreKeyword(word)
	}
	return c == '-' || c == '/' || c == '.'
}

func intervalInputDateByte(c byte) bool {
	return intervalInputAlnum(c) || c == '+' || c == '-' || c == '/' || c == '_' || c == '.' || c == ':'
}
