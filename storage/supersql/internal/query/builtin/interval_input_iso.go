package builtin

import (
	"context"
	"math"
	"math/bits"
	"strconv"
	"strings"
)

func (parts *intervalInputParts) decodeISO(ctx context.Context, text string) (intervalInputStatus, error) {
	input := intervalISOInput{parts: parts, cursor: intervalInputCursor{text: text}, datePart: true}
	if input.cursor.peek() != 'P' {
		return intervalInputBadFormat, nil
	}
	input.cursor.advance(ctx)
	if input.cursor.peek() == 0 {
		return intervalInputBadFormat, input.cursor.err
	}
	for input.cursor.peek() != 0 {
		if input.cursor.peek() == 'T' {
			input.datePart, input.haveField = false, false
			input.cursor.advance(ctx)
			continue
		}
		value, fraction, width, status := input.cursor.isoNumber(ctx)
		if status != intervalInputOK {
			return status, input.cursor.err
		}
		unit := input.cursor.peek()
		if unit != 0 {
			input.cursor.advance(ctx)
		}
		if input.datePart {
			status = input.date(ctx, value, fraction, width, unit)
		} else {
			status = input.time(ctx, value, fraction, width, unit)
		}
		if status != intervalInputOK {
			return status, input.cursor.err
		}
	}
	return intervalInputOK, input.cursor.err
}

type intervalISOInput struct {
	parts               *intervalInputParts
	cursor              intervalInputCursor
	datePart, haveField bool
}

func (input *intervalISOInput) date(ctx context.Context, value int64, fraction float64, width int, unit byte) intervalInputStatus {
	fieldUnit := intervalInputYear
	switch unit {
	case 'Y':
	case 'M':
		fieldUnit = intervalInputMonth
	case 'W':
		fieldUnit = intervalInputWeek
	case 'D':
		fieldUnit = intervalInputDay
	case 'T', 0, '-':
		return input.dateAlternative(ctx, value, fraction, width, unit)
	default:
		return intervalInputBadFormat
	}
	if !input.parts.addUnit(fieldUnit, value, fraction) {
		return intervalInputOverflow
	}
	input.haveField = true
	return intervalInputOK
}

func (input *intervalISOInput) dateAlternative(ctx context.Context, value int64, fraction float64, width int, unit byte) intervalInputStatus {
	if unit != '-' && width == 8 && !input.haveField {
		if !input.parts.addUnit(intervalInputYear, value/10000, 0) || !input.parts.addUnit(intervalInputMonth, value/100%100, 0) || !input.parts.addUnit(intervalInputDay, value%100, fraction) {
			return intervalInputOverflow
		}
		input.datePart, input.haveField = false, false
		return intervalInputOK
	}
	if input.haveField {
		return intervalInputBadFormat
	}
	if !input.parts.addUnit(intervalInputYear, value, fraction) {
		return intervalInputOverflow
	}
	if unit == 0 || unit == 'T' {
		input.datePart = false
		return intervalInputOK
	}
	value, fraction, _, status := input.cursor.isoNumber(ctx)
	if status != intervalInputOK {
		return status
	}
	if !input.parts.addUnit(intervalInputMonth, value, fraction) {
		return intervalInputOverflow
	}
	if input.cursor.peek() == 0 || input.cursor.peek() == 'T' {
		input.datePart = false
		return intervalInputOK
	}
	if input.cursor.peek() != '-' {
		return intervalInputBadFormat
	}
	input.cursor.advance(ctx)
	value, fraction, _, status = input.cursor.isoNumber(ctx)
	if status != intervalInputOK {
		return status
	}
	if !input.parts.addUnit(intervalInputDay, value, fraction) {
		return intervalInputOverflow
	}
	if input.cursor.peek() != 0 && input.cursor.peek() != 'T' {
		return intervalInputBadFormat
	}
	input.datePart = false
	return intervalInputOK
}

func (input *intervalISOInput) time(ctx context.Context, value int64, fraction float64, width int, unit byte) intervalInputStatus {
	fieldUnit := intervalInputHour
	switch unit {
	case 'H':
	case 'M':
		fieldUnit = intervalInputMinute
	case 'S':
		fieldUnit = intervalInputSecond
	case 0, ':':
		return input.timeAlternative(ctx, value, fraction, width, unit)
	default:
		return intervalInputBadFormat
	}
	if !input.parts.addUnit(fieldUnit, value, fraction) {
		return intervalInputOverflow
	}
	input.haveField = true
	return intervalInputOK
}

func (input *intervalISOInput) timeAlternative(ctx context.Context, value int64, fraction float64, width int, unit byte) intervalInputStatus {
	if unit == 0 && width == 6 && !input.haveField {
		// PG's basic-form fractional tail is measured in microseconds, not seconds.
		if !input.parts.addMicros(value/10000, 0, intervalMicrosPerHour) || !input.parts.addMicros(value/100%100, 0, intervalMicrosPerMinute) || !input.parts.addMicros(value%100, 0, intervalMicrosPerSecond) || !input.parts.fractionalMicros(fraction, 1) {
			return intervalInputOverflow
		}
		return intervalInputOK
	}
	if input.haveField {
		return intervalInputBadFormat
	}
	if !input.parts.addMicros(value, fraction, intervalMicrosPerHour) {
		return intervalInputOverflow
	}
	if unit == 0 {
		return intervalInputOK
	}
	value, fraction, _, status := input.cursor.isoNumber(ctx)
	if status != intervalInputOK {
		return status
	}
	if !input.parts.addMicros(value, fraction, intervalMicrosPerMinute) {
		return intervalInputOverflow
	}
	if input.cursor.peek() == 0 {
		return intervalInputOK
	}
	if input.cursor.peek() != ':' {
		return intervalInputBadFormat
	}
	input.cursor.advance(ctx)
	value, fraction, _, status = input.cursor.isoNumber(ctx)
	if status != intervalInputOK {
		return status
	}
	if !input.parts.addMicros(value, fraction, intervalMicrosPerSecond) {
		return intervalInputOverflow
	}
	if input.cursor.peek() != 0 {
		return intervalInputBadFormat
	}
	return intervalInputOK
}

// strtod's mantissa can be arbitrarily long. 768 significant decimal digits
// exceed the longest binary64 rounding boundary; retain a sticky tail rather
// than allocating an input-sized token. Hexadecimal needs fewer digits.
func (cursor *intervalInputCursor) isoNumber(ctx context.Context) (int64, float64, int, intervalInputStatus) {
	if c := cursor.peek(); !intervalInputDigit(c) && c != '-' && c != '.' {
		return 0, 0, 0, intervalInputBadFormat
	}
	negative := cursor.peek() == '-'
	if negative {
		cursor.advance(ctx)
	}
	if negative && len(cursor.text)-cursor.position >= 3 {
		prefix := cursor.text[cursor.position : cursor.position+3]
		if strings.EqualFold(prefix, "inf") || strings.EqualFold(prefix, "nan") {
			return 0, 0, 0, intervalInputOverflow
		}
	}
	start := cursor.position
	hex := false
	hexPrefix := len(cursor.text)-cursor.position >= 2 && (cursor.text[cursor.position+1] == 'x' || cursor.text[cursor.position+1] == 'X')
	if cursor.peek() == '0' && hexPrefix {
		after := cursor.position + 2
		if after < len(cursor.text) && cursor.text[after] == '.' {
			after++
		}
		if after < len(cursor.text) && digit(cursor.text[after]) >= 0 && digit(cursor.text[after]) < 16 {
			hex = true
			cursor.advance(ctx)
			cursor.advance(ctx)
		}
	}
	var significant [768]byte
	kept, digits, beforePoint, leading, width := 0, 0, 0, 0, 0
	point, nonzero, sticky := false, false, false
	for {
		c := cursor.peek()
		if c == '.' && !point {
			point = true
			cursor.advance(ctx)
			continue
		}
		d := digit(c)
		decimalEnd := d >= 10 && !hex
		if d < 0 || decimalEnd || d >= 16 {
			break
		}
		digits++
		if !point {
			beforePoint++
		}
		if !nonzero && d == 0 {
			leading++
		} else {
			nonzero = true
			if kept < len(significant) {
				significant[kept] = c
				kept++
			} else if d != 0 {
				sticky = true
			}
		}
		cursor.advance(ctx)
	}
	if digits == 0 {
		return 0, 0, 0, intervalInputBadFormat
	}
	// Integer width counts decimal digits in the original spelling, even for
	// hexadecimal and scientific values used by the alternative ISO syntax.
	for i := start; i < cursor.position && intervalInputDigit(cursor.text[i]); i++ {
		width++
		if width > 8 {
			break
		}
	}
	exponent := int64(0)
	c := cursor.peek()
	decimalExponent := !hex && (c == 'e' || c == 'E')
	hexExponent := hex && (c == 'p' || c == 'P')
	if decimalExponent || hexExponent {
		marker := cursor.position
		cursor.advance(ctx)
		exponentNegative := cursor.peek() == '-'
		if cursor.peek() == '+' || exponentNegative {
			cursor.advance(ctx)
		}
		if !intervalInputDigit(cursor.peek()) {
			cursor.position = marker
		} else {
			for intervalInputDigit(cursor.peek()) {
				if exponent < math.MaxInt64/16 {
					exponent = exponent*10 + int64(cursor.peek()-'0')
				}
				cursor.advance(ctx)
			}
			if exponentNegative {
				exponent = -exponent
			}
		}
	}
	if cursor.err != nil {
		return 0, 0, 0, intervalInputBadFormat
	}
	if !nonzero {
		return 0, 0, width, intervalInputOK
	}
	shift := int64(beforePoint - leading - kept)
	if hex {
		shift *= 4
	}
	if !intervalInputAdd(&exponent, shift) {
		return 0, 0, width, intervalInputBadFormat
	}
	var number [810]byte
	out := number[:0]
	if negative {
		out = append(out, '-')
	}
	if hex {
		out = append(out, '0', 'x')
	}
	out = append(out, significant[:kept]...)
	if sticky {
		out = append(out, '1')
		if hex {
			exponent -= 4
		} else {
			exponent--
		}
	}
	if hex {
		out = append(out, 'p')
	} else {
		out = append(out, 'e')
	}
	out = strconv.AppendInt(out, exponent, 10)
	value, err := strconv.ParseFloat(string(out), 64)
	if err != nil {
		return 0, 0, width, intervalInputBadFormat
	}
	// The pinned libc rejects decimal subnormals; exact hexadecimal subnormals survive.
	subnormalError := math.Abs(value) < 0x1p-1022 && (!hex || !exactISOHexSubnormal(value, significant[:kept], exponent, sticky))
	if value == 0 || subnormalError {
		return 0, 0, width, intervalInputBadFormat
	}
	if value < -1e15 || value > 1e15 {
		return 0, 0, width, intervalInputOverflow
	}
	integer := int64(value)
	return integer, value - float64(integer), width, intervalInputOK
}

func exactISOHexSubnormal(value float64, significant []byte, exponent int64, sticky bool) bool {
	if sticky {
		return false
	}
	for len(significant) > 0 && significant[len(significant)-1] == '0' {
		significant = significant[:len(significant)-1]
		exponent += 4
	}
	if len(significant) > 14 {
		return false
	}
	var input uint64
	for _, c := range significant {
		input = input*16 + uint64(digit(c))
	}
	if input == 0 {
		return false
	}
	mantissa := math.Float64bits(value) & ((1 << 52) - 1)
	inputZeros, wantedZeros := bits.TrailingZeros64(input), bits.TrailingZeros64(mantissa)
	return input>>inputZeros == mantissa>>wantedZeros && exponent+int64(inputZeros) == -1074+int64(wantedZeros)
}
