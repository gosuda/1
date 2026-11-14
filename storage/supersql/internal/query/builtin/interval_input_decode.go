package builtin

import (
	"math"
	"strconv"
	"strings"
)

type intervalInputUnit uint8

const (
	intervalInputIgnore intervalInputUnit = iota
	intervalInputMonth
	intervalInputYear
	intervalInputDay
	intervalInputHour
	intervalInputMinute
	intervalInputSecond
	intervalInputMillisecond
	intervalInputMicrosecond
	intervalInputWeek
	intervalInputDecade
	intervalInputCentury
	intervalInputMillennium
	intervalInputUnsupported
	intervalInputAgo
	intervalInputInfinity
	intervalInputUnknown
)

const intervalInputTimeMask = uint32(1<<intervalInputHour | 1<<intervalInputMinute | 1<<intervalInputSecond | 1<<intervalInputMillisecond | 1<<intervalInputMicrosecond)
const intervalInputDateMask = uint32(1<<intervalInputMonth | 1<<intervalInputYear | 1<<intervalInputDay)

func intervalInputLookup(field intervalInputField) intervalInputUnit {
	lower := intervalInputKeyword(field.text)
	word := lower[:min(len(field.text), len(lower))]
	if field.sign != 0 {
		if string(word) == "infinity" {
			return intervalInputInfinity
		}
		return intervalInputUnknown
	}
	return intervalInputUnitKeyword(string(word))
}

func intervalInputUnitKeyword(word string) intervalInputUnit {
	switch word {
	case "at", "on":
		return intervalInputIgnore
	case "ago":
		return intervalInputAgo
	case "c", "cent", "centuries", "century":
		return intervalInputCentury
	case "d", "day", "days":
		return intervalInputDay
	case "dec", "decade", "decades", "decs":
		return intervalInputDecade
	case "h", "hour", "hours", "hr", "hrs":
		return intervalInputHour
	case "m", "min", "mins", "minute", "minutes", "mm":
		return intervalInputMinute
	case "microsecon", "us", "usec", "usecond", "useconds", "usecs":
		return intervalInputMicrosecond
	case "mil", "millennia", "millennium", "mils":
		return intervalInputMillennium
	case "millisecon", "ms", "msec", "msecond", "mseconds", "msecs":
		return intervalInputMillisecond
	case "mon", "mons", "month", "months":
		return intervalInputMonth
	case "s", "sec", "second", "seconds", "secs":
		return intervalInputSecond
	case "w", "week", "weeks":
		return intervalInputWeek
	case "y", "year", "years", "yr", "yrs":
		return intervalInputYear
	case "infinity":
		return intervalInputInfinity
	case "qtr", "quarter", "timezone", "timezone_h", "timezone_m", "dow", "doy", "isodow", "isoyear", "j", "jd", "julian":
		return intervalInputUnsupported
	default:
		return intervalInputUnknown
	}
}

func intervalInputRangeUnit(fieldRange int32) intervalInputUnit {
	switch fieldRange {
	case intervalYearMask:
		return intervalInputYear
	case intervalMonthMask, intervalYearMask | intervalMonthMask:
		return intervalInputMonth
	case intervalDayMask:
		return intervalInputDay
	case intervalHourMask, intervalDayMask | intervalHourMask:
		return intervalInputHour
	case intervalMinuteMask, intervalHourMask | intervalMinuteMask, intervalDayMask | intervalHourMask | intervalMinuteMask:
		return intervalInputMinute
	default:
		return intervalInputSecond
	}
}

func (parts *intervalInputParts) decode(fields *intervalInputFields, fieldRange int32, style IntervalStyle) intervalInputStatus {
	decoder := intervalInputDecoder{parts: parts, fieldRange: fieldRange}
	decoder.forceNegative = style == IntervalSQLStandard && fields.count > 0 && fields.fields[0].sign == '-'
	if decoder.forceNegative {
		for _, field := range fields.fields[1:fields.count] {
			if field.sign != 0 {
				decoder.forceNegative = false
				break
			}
		}
	}
	var seen uint32
	for i := fields.count - 1; i >= 0; i-- {
		mask, status := decoder.field(fields.fields[i], i == fields.count-1)
		if status != intervalInputOK {
			return status
		}
		if seen&mask != 0 {
			return intervalInputBadFormat
		}
		seen |= mask
	}
	if seen == 0 || decoder.pendingUnit {
		return intervalInputBadFormat
	}
	if !decoder.ago {
		return intervalInputOK
	}
	if parts.micros == math.MinInt64 || parts.days == math.MinInt32 || parts.months == math.MinInt32 || parts.years == math.MinInt32 {
		return intervalInputOverflow
	}
	parts.micros = -parts.micros
	parts.days = -parts.days
	parts.months = -parts.months
	parts.years = -parts.years
	return intervalInputOK
}

type intervalInputDecoder struct {
	parts                           *intervalInputParts
	fieldRange                      int32
	unit                            intervalInputUnit
	forceNegative, pendingUnit, ago bool
}

func (decoder *intervalInputDecoder) field(field intervalInputField, last bool) (uint32, intervalInputStatus) {
	switch field.kind {
	case intervalInputTimeField:
		micros, status := intervalInputTime(field.text, decoder.fieldRange)
		if status != intervalInputOK {
			return 0, status
		}
		return decoder.time(micros), intervalInputOK
	case intervalInputSignedField, intervalInputDateField, intervalInputNumberField:
		return decoder.number(field)
	case intervalInputTextField, intervalInputSpecialField:
		return decoder.keyword(field, last)
	default:
		return 0, intervalInputBadFormat
	}
}

func (decoder *intervalInputDecoder) time(micros int64) uint32 {
	if decoder.forceNegative && micros > 0 {
		micros = -micros
	}
	decoder.parts.micros = micros
	decoder.unit, decoder.pendingUnit = intervalInputDay, false
	return intervalInputTimeMask
}

func (decoder *intervalInputDecoder) number(field intervalInputField) (uint32, intervalInputStatus) {
	if field.kind == intervalInputSignedField && strings.ContainsRune(field.text, ':') {
		micros, status := intervalInputTime(field.text, decoder.fieldRange)
		if status == intervalInputOK {
			if field.sign == '-' {
				micros = -micros
			}
			return decoder.time(micros), intervalInputOK
		}
	}
	if decoder.unit == intervalInputIgnore {
		decoder.unit = intervalInputRangeUnit(decoder.fieldRange)
	}
	value, fraction, unit, status := intervalInputNumber(field, decoder.unit)
	if status != intervalInputOK {
		return 0, status
	}
	if decoder.forceNegative && value > 0 {
		value = -value
	}
	if decoder.forceNegative && fraction > 0 {
		fraction = -fraction
	}
	if unit < intervalInputMonth || unit > intervalInputMillennium {
		return 0, intervalInputBadFormat
	}
	if !decoder.parts.addUnit(unit, value, fraction) {
		return 0, intervalInputOverflow
	}
	mask := uint32(1) << unit
	if unit == intervalInputSecond && fraction != 0 {
		mask |= 1<<intervalInputMillisecond | 1<<intervalInputMicrosecond
	}
	decoder.unit = unit
	if unit == intervalInputHour {
		decoder.unit = intervalInputDay
	}
	decoder.pendingUnit = false
	return mask, intervalInputOK
}

func intervalInputNumber(field intervalInputField, unit intervalInputUnit) (int64, float64, intervalInputUnit, intervalInputStatus) {
	value, end, status := intervalInputInteger(field.text, field.sign == '-', 64)
	if status != intervalInputOK {
		return 0, 0, unit, status
	}
	if end == len(field.text) {
		return value, 0, unit, intervalInputOK
	}
	switch field.text[end] {
	case '-':
		month, n, status := intervalInputInteger(field.text[end+1:], false, 32)
		if status != intervalInputOK || month < 0 || month >= 12 {
			return 0, 0, unit, intervalInputOverflow
		}
		if end+1+n != len(field.text) {
			return 0, 0, unit, intervalInputBadFormat
		}
		if field.sign == '-' {
			month = -month
		}
		if value > math.MaxInt64/12 || value < math.MinInt64/12 {
			return 0, 0, unit, intervalInputOverflow
		}
		value *= 12
		if !intervalInputAdd(&value, month) {
			return 0, 0, unit, intervalInputOverflow
		}
		return value, 0, intervalInputMonth, intervalInputOK
	case '.':
		fraction, status := intervalInputFraction(field.text[end:])
		if field.sign == '-' {
			fraction = -fraction
		}
		return value, fraction, unit, status
	default:
		return 0, 0, unit, intervalInputBadFormat
	}
}

func (decoder *intervalInputDecoder) keyword(field intervalInputField, last bool) (uint32, intervalInputStatus) {
	if decoder.pendingUnit {
		return 0, intervalInputBadFormat
	}
	decoder.unit = intervalInputLookup(field)
	switch decoder.unit {
	case intervalInputIgnore:
	case intervalInputAgo:
		if !last {
			return 0, intervalInputBadFormat
		}
		decoder.ago = true
	case intervalInputInfinity:
		if !last {
			return 0, intervalInputBadFormat
		}
		decoder.parts.infinity = 1
		if field.sign == '-' {
			decoder.parts.infinity = -1
		}
		return intervalInputDateMask | intervalInputTimeMask, intervalInputOK
	case intervalInputUnknown:
		return 0, intervalInputBadFormat
	default:
		decoder.pendingUnit = true
	}
	return 0, intervalInputOK
}

// strtoi64/strtoint return zero and leave the end pointer at the start when no
// digits are present. DecodeInterval intentionally accepts that in fields like '.'.
func intervalInputInteger(text string, negative bool, bits int) (int64, int, intervalInputStatus) {
	position := 0
	if len(text) > 0 && (text[0] == '+' || text[0] == '-') {
		if text[0] == '-' {
			negative = !negative
		}
		position++
	}
	start := position
	limit := uint64(math.MaxInt64)
	if bits == 32 {
		limit = math.MaxInt32
	}
	if negative {
		limit++
	}
	var value uint64
	for position < len(text) && intervalInputDigit(text[position]) {
		digit := uint64(text[position] - '0')
		if value > (limit-digit)/10 {
			return 0, position, intervalInputOverflow
		}
		value = value*10 + digit
		position++
	}
	if position == start {
		return 0, 0, intervalInputOK
	}
	if negative {
		return -int64(value), position, intervalInputOK
	}
	return int64(value), position, intervalInputOK
}

func intervalInputFraction(text string) (float64, intervalInputStatus) {
	if len(text) == 1 {
		return 0, intervalInputOK
	}
	for i := 1; i < len(text); i++ {
		if !intervalInputDigit(text[i]) {
			return 0, intervalInputBadFormat
		}
	}
	fraction, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, intervalInputBadFormat
	}
	return fraction, intervalInputOK
}

func intervalInputTime(text string, fieldRange int32) (int64, intervalInputStatus) {
	hours, end, status := intervalInputInteger(text, false, 64)
	if status != intervalInputOK {
		return 0, status
	}
	if end == len(text) || text[end] != ':' {
		return 0, intervalInputBadFormat
	}
	minutes, n, status := intervalInputInteger(text[end+1:], false, 32)
	if status != intervalInputOK {
		return 0, status
	}
	end += n + 1
	var seconds, micros int64
	minuteSecond := end == len(text) && fieldRange == intervalMinuteMask|intervalSecondMask
	if end < len(text) {
		switch text[end] {
		case '.':
			micros, status = intervalInputTimeFraction(text[end:])
			minuteSecond = true
		case ':':
			seconds, n, status = intervalInputInteger(text[end+1:], false, 32)
			if status != intervalInputOK {
				return 0, status
			}
			micros, status = intervalInputTimeFraction(text[end+1+n:])
		default:
			return 0, intervalInputBadFormat
		}
		if status != intervalInputOK {
			return 0, status
		}
	}
	if minuteSecond {
		if hours < math.MinInt32 || hours > math.MaxInt32 {
			return 0, intervalInputOverflow
		}
		hours, minutes, seconds = 0, hours, minutes
	}
	invalidMinute := minutes < 0 || minutes > 59
	invalidSecond := seconds < 0 || seconds > 60
	invalidFraction := micros < 0 || micros > 1_000_000
	if hours < 0 || invalidMinute || invalidSecond || invalidFraction {
		return 0, intervalInputOverflow
	}
	if !intervalInputMultiplyAdd(&micros, hours, intervalMicrosPerHour) || !intervalInputMultiplyAdd(&micros, minutes, intervalMicrosPerMinute) || !intervalInputMultiplyAdd(&micros, seconds, intervalMicrosPerSecond) {
		return 0, intervalInputOverflow
	}
	return micros, intervalInputOK
}

func intervalInputTimeFraction(text string) (int64, intervalInputStatus) {
	if text == "" {
		return 0, intervalInputOK
	}
	if text[0] != '.' {
		return 0, intervalInputBadFormat
	}
	fraction, status := intervalInputFraction(text)
	if status != intervalInputOK {
		return 0, status
	}
	return int64(math.RoundToEven(fraction * 1_000_000)), intervalInputOK
}
