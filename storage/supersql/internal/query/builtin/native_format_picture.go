package builtin

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var nativeDatePictureTokens = [...]string{
	"MILLISECOND", "MICROSECOND", "Y,YYY", "IYYY", "IDDD", "YYYY", "HH24", "HH12", "SSSSS",
	"MONTH", "SSSS", "A.D.", "B.C.", "P.M.", "A.M.", "DAY", "DDD", "MON", "IYY", "YYY",
	"FF6", "FF5", "FF4", "FF3", "FF2", "FF1", "TZH", "TZM", "US", "MS", "IW", "IY", "YY",
	"HH", "MI", "SS", "RM", "DY", "CC", "MM", "DD", "AD", "BC", "PM", "AM", "ID", "WW", "OF", "TZ",
	"I", "Y", "Q", "W", "D", "J",
}

func nativePictureToken(format string, offset int) (string, string) {
	for _, token := range nativeDatePictureTokens {
		if offset+len(token) <= len(format) && strings.EqualFold(format[offset:offset+len(token)], token) {
			return token, format[offset : offset+len(token)]
		}
	}
	return "", ""
}
func nativePictureInputWidth(field string) int {
	switch field {
	case "Y", "I", "ID", "Q", "W", "D", "FF1":
		return 1
	case "YY", "IY", "IW", "CC", "MM", "DD", "HH24", "HH12", "HH", "MI", "SS", "FF2":
		return 2
	case "YYY", "IYY", "DDD", "IDDD", "MS", "MILLISECOND", "FF3":
		return 3
	case "YYYY", "IYYY", "FF4":
		return 4
	case "SSSS", "SSSSS", "FF5":
		return 5
	case "US", "MICROSECOND", "FF6":
		return 6
	case "J":
		return 7
	default:
		return 0
	}
}

func nativeTokenCase(token, value string) string {
	if token == strings.ToUpper(token) { return strings.ToUpper(value) }
	if token == strings.ToLower(token) { return strings.ToLower(value) }
	if value == "" { return value }
	_, size := utf8.DecodeRuneInString(value)
	return strings.ToUpper(value[:size]) + strings.ToLower(value[size:])
}

func nativeOrdinal(value int64, upper bool) string {
	absolute := value
	if absolute < 0 { absolute = -absolute }
	suffix := "th"
	if absolute%100 < 11 || absolute%100 > 13 {
		switch absolute % 10 { case 1: suffix = "st"; case 2: suffix = "nd"; case 3: suffix = "rd" }
	}
	if upper { return strings.ToUpper(suffix) }
	return suffix
}

func nativeFormatDatePicture(value int64, format string, zone namedTimeZone, zoned bool, locale model.FormatLocale) (string, error) {
	if !finiteTimestamp(value) {
		if value < 0 { return "-infinity", nil }
		return "infinity", nil
	}
	days, micros := splitTimestamp(value)
	var zoneType timezoneType
	if zoned { days, micros, zoneType = zone.localTimestampParts(value) }
	if days < -dateEpochJulian { return "", sqlError("22008", "timestamp out of range") }
	year, month, day := dateCalendar(days)
	displayYear, bc := year, false
	if displayYear <= 0 { displayYear, bc = 1-displayYear, true }
	julian := days + dateEpochJulian
	weekday := int64(dateWeekday(int32(julian)))
	isoDay := weekday; if isoDay == 0 { isoDay = 7 }
	isoYear := dateISOField(dateFieldISOYear, year, month, day)
	if isoYear <= 0 { isoYear = 1-isoYear }
	isoWeek := dateISOField(intervalInputWeek, year, month, day)
	dayOfYear := dateIntegralField(dateFieldDOY, days)
	hour := micros / intervalMicrosPerHour
	minute := micros / intervalMicrosPerMinute % 60
	second := micros / intervalMicrosPerSecond % 60
	secondOfDay := micros / intervalMicrosPerSecond
	fraction := fmt.Sprintf("%06d", micros%intervalMicrosPerSecond)
	months := [...]string{"", "JANUARY", "FEBRUARY", "MARCH", "APRIL", "MAY", "JUNE", "JULY", "AUGUST", "SEPTEMBER", "OCTOBER", "NOVEMBER", "DECEMBER"}
	weekdays := [...]string{"SUNDAY", "MONDAY", "TUESDAY", "WEDNESDAY", "THURSDAY", "FRIDAY", "SATURDAY"}
	roman := [...]string{"", "I", "II", "III", "IV", "V", "VI", "VII", "VIII", "IX", "X", "XI", "XII"}
	var out strings.Builder
	for offset := 0; offset < len(format); {
		if format[offset] == '"' {
			offset++
			for offset < len(format) && format[offset] != '"' { if format[offset] == '\\' && offset+1 < len(format) { offset++ }; out.WriteByte(format[offset]); offset++ }
			if offset < len(format) { offset++ }
			continue
		}
		fill, translation := false, false
		for offset+2 <= len(format) {
			prefix := strings.ToUpper(format[offset:offset+2])
			if prefix == "FM" { fill = true; offset += 2; continue }
			if prefix == "TM" { translation = true; offset += 2; continue }
			if prefix == "FX" { offset += 2; continue }
			break
		}
		field, spelling := nativePictureToken(format, offset)
		if field == "" { out.WriteByte(format[offset]); offset++; continue }
		offset += len(spelling)
		var text string
		var ordinal int64
		switch field {
		case "DAY":
			name := weekdays[weekday]
			if translation { name = locale.DayNames[weekday] }
			text = nativeTokenCase(spelling, name)
			if !fill && !translation { text = fmt.Sprintf("%-9s", text) }
		case "DY":
			name := weekdays[weekday][:3]
			if translation { name = locale.AbbreviatedDayNames[weekday] }
			text = nativeTokenCase(spelling, name)
		case "MONTH":
			name := months[month]
			if translation { name = locale.MonthNames[month-1] }
			text = nativeTokenCase(spelling, name)
			if !fill && !translation { text = fmt.Sprintf("%-9s", text) }
		case "MON":
			name := months[month][:3]
			if translation { name = locale.AbbreviatedMonthNames[month-1] }
			text = nativeTokenCase(spelling, name)
		case "RM": text = nativeTokenCase(spelling, roman[month]); if !fill { text = fmt.Sprintf("%-4s", text) }
		case "Y,YYY": digits := strconv.FormatInt(displayYear, 10); if len(digits) < 4 { digits = strings.Repeat("0", 4-len(digits))+digits }; text = digits[:len(digits)-3]+","+digits[len(digits)-3:]; ordinal = displayYear
		case "YYYY": text = fmt.Sprintf("%04d", displayYear); ordinal = displayYear
		case "YYY": text = fmt.Sprintf("%03d", displayYear%1000); ordinal = displayYear%1000
		case "YY": text = fmt.Sprintf("%02d", displayYear%100); ordinal = displayYear%100
		case "Y": text = strconv.FormatInt(displayYear%10, 10); ordinal = displayYear%10
		case "IYYY": text = fmt.Sprintf("%04d", isoYear); ordinal = isoYear
		case "IYY": text = fmt.Sprintf("%03d", isoYear%1000); ordinal = isoYear%1000
		case "IY": text = fmt.Sprintf("%02d", isoYear%100); ordinal = isoYear%100
		case "I": text = strconv.FormatInt(isoYear%10, 10); ordinal = isoYear%10
		case "CC": ordinal = (displayYear+99)/100; text = fmt.Sprintf("%02d", ordinal)
		case "Q": ordinal = (month-1)/3+1; text = strconv.FormatInt(ordinal, 10)
		case "MM": ordinal = month; text = fmt.Sprintf("%02d", month)
		case "WW": ordinal = (dayOfYear-1)/7+1; text = fmt.Sprintf("%02d", ordinal)
		case "W": ordinal = (day-1)/7+1; text = strconv.FormatInt(ordinal, 10)
		case "IW": ordinal = isoWeek; text = fmt.Sprintf("%02d", isoWeek)
		case "DDD": ordinal = dayOfYear; text = fmt.Sprintf("%03d", dayOfYear)
		case "IDDD": ordinal = (isoWeek-1)*7+isoDay; text = fmt.Sprintf("%03d", ordinal)
		case "DD": ordinal = day; text = fmt.Sprintf("%02d", day)
		case "D": ordinal = weekday+1; text = strconv.FormatInt(ordinal, 10)
		case "ID": ordinal = isoDay; text = strconv.FormatInt(ordinal, 10)
		case "J": ordinal = julian; text = strconv.FormatInt(julian, 10)
		case "HH", "HH12": ordinal = hour%12; if ordinal == 0 { ordinal = 12 }; text = fmt.Sprintf("%02d", ordinal)
		case "HH24": ordinal = hour; text = fmt.Sprintf("%02d", hour)
		case "MI": ordinal = minute; text = fmt.Sprintf("%02d", minute)
		case "SS": ordinal = second; text = fmt.Sprintf("%02d", second)
		case "SSSS", "SSSSS": ordinal = secondOfDay; text = strconv.FormatInt(secondOfDay, 10)
		case "MS": text = fraction[:3]
		case "US", "MICROSECOND": text = fraction
		case "MILLISECOND": text = fraction[:3]
		case "FF1", "FF2", "FF3", "FF4", "FF5", "FF6": text = fraction[:int(field[2]-'0')]
		case "AM", "PM", "A.M.", "P.M.": pm := hour >= 12; dotted := strings.Contains(field, "."); if pm { text = "PM" } else { text = "AM" }; if dotted { text = text[:1]+"."+text[1:]+"." }; text = nativeTokenCase(spelling, text)
		case "AD", "BC", "A.D.", "B.C.": text = "AD"; if bc { text = "BC" }; if strings.Contains(field, ".") { text = text[:1]+"."+text[1:]+"." }; text = nativeTokenCase(spelling, text)
		case "TZ": if zoned { text = nativeTokenCase(spelling, zoneType.name) }
		case "TZH": if zoned { offset := int64(zoneType.offset); sign := "+"; if offset < 0 { sign = "-"; offset = -offset }; text = fmt.Sprintf("%s%02d", sign, offset/3600) }
		case "TZM": if zoned { offset := int64(zoneType.offset); if offset < 0 { offset = -offset }; text = fmt.Sprintf("%02d", offset%3600/60) }
		case "OF": if zoned { offset := int64(zoneType.offset); sign := "+"; if offset < 0 { sign = "-"; offset = -offset }; text = fmt.Sprintf("%s%02d", sign, offset/3600); if offset%3600 != 0 { text += fmt.Sprintf(":%02d", offset%3600/60) } }
		}
		if fill { text = strings.TrimLeft(text, "0"); if text == "" { text = "0" } }
		if offset+2 <= len(format) {
			suffix := strings.ToUpper(format[offset:offset+2])
			if suffix == "TH" { text += nativeOrdinal(ordinal, format[offset:offset+2] == "TH"); offset += 2 }
			if suffix == "SP" { offset += 2 }
		}
		out.WriteString(text)
	}
	return out.String(), nil
}

type nativeParsedPicture struct { year, isoYear int64; month, day, hour, minute, second, micros, isoOrdinal int; pm, meridian, bc bool; zone string; julian int64; hasJulian bool }

func nativeParseDatePicture(input, format string, locale model.FormatLocale) (nativeParsedPicture, error) {
	result := nativeParsedPicture{month: 1, day: 1}
	inputOffset := 0
	exact := strings.HasPrefix(strings.ToUpper(format), "FX")
	if exact {
		format = format[2:]
	}
	advanceInput := func() bool {
		if inputOffset >= len(input) {
			return false
		}
		_, size := utf8.DecodeRuneInString(input[inputOffset:])
		inputOffset += size
		return true
	}
	skipSpaces := func() {
		for inputOffset < len(input) {
			current, size := utf8.DecodeRuneInString(input[inputOffset:])
			if !unicode.IsSpace(current) {
				return
			}
			inputOffset += size
		}
	}
	readDigits := func(minimum, maximum int) (int64, int, bool) {
		start := inputOffset
		for inputOffset < len(input) && inputOffset-start < maximum && input[inputOffset] >= '0' && input[inputOffset] <= '9' {
			inputOffset++
		}
		if inputOffset-start < minimum {
			inputOffset = start
			return 0, 0, false
		}
		value, parseErr := strconv.ParseInt(input[start:inputOffset], 10, 64)
		return value, inputOffset - start, parseErr == nil
	}
	readName := func(names []string) (int, bool) {
		selected, length := -1, 0
		for index, name := range names {
			if len(name) <= length || inputOffset+len(name) > len(input) {
				continue
			}
			if strings.EqualFold(input[inputOffset:inputOffset+len(name)], name) {
				selected, length = index, len(name)
			}
		}
		if selected < 0 {
			return 0, false
		}
		inputOffset += length
		return selected, true
	}
	nearestYear := func(value int64) int64 {
		switch {
		case value < 70:
			return value + 2000
		case value < 100:
			return value + 1900
		case value < 520:
			return value + 2000
		case value < 1000:
			return value + 1000
		default:
			return value
		}
	}
	englishMonths := [...]string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}
	englishMonthAbbreviations := [...]string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	englishDays := [...]string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}
	englishDayAbbreviations := [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	var century, yearValue, ordinalDay, weekOfYear int64
	yearDigits, isoYearDigits := 0, 0
	yearNegative, isoNegative := false, false
	gregorian, iso := false, false
	clock12 := false
	hourSet, minuteSet, secondSet, secondsOfDaySet := false, false, false, false
	millisecondsSet, microsecondsSet := false, false
	monthSet, daySet, weekOfMonthSet, weekOfYearSet := false, false, false, false
	isoWeekSet, isoDaySet, isoOrdinalSet := false, false, false
	weekOfMonth, isoWeek, isoDay := 0, 0, 0
	secondsOfDay, milliseconds, microseconds := 0, 0, 0
	yearSet, isoYearSet, eraSet := false, false, false
	setParsedYear := func(target *int64, seen *bool, sign *bool, value int64, negative bool) bool {
		if *seen && (*target != value || *sign != negative) { return false }
		*target, *seen, *sign = value, true, negative
		return true
	}
	setParsed := func(target *int, seen *bool, value int) bool {
		if *seen && *target != value { return false }
		*target, *seen = value, true
		return true
	}
	for offset := 0; offset < len(format); {
		if format[offset] == '"' {
			offset++
			for offset < len(format) && format[offset] != '"' {
				if format[offset] == '\\' && offset+1 < len(format) {
					offset++
				}
				if !advanceInput() {
					return result, temporalBadFormat()
				}
				_, size := utf8.DecodeRuneInString(format[offset:])
				offset += size
			}
			if offset < len(format) {
				offset++
			}
			continue
		}
		translation := false
		for offset+2 <= len(format) {
			prefix := strings.ToUpper(format[offset : offset+2])
			if prefix == "FM" {
				offset += 2
				continue
			}
			if prefix == "TM" {
				translation = true
				offset += 2
				continue
			}
			break
		}
		field, spelling := nativePictureToken(format, offset)
		if field == "" {
			formatRune, size := utf8.DecodeRuneInString(format[offset:])
			if inputOffset >= len(input) {
				if exact || unicode.IsLetter(formatRune) || unicode.IsDigit(formatRune) {
					return result, temporalBadFormat()
				}
				offset += size
				continue
			}
			if exact {
				advanceInput()
			} else if unicode.IsSpace(formatRune) {
				skipSpaces()
			} else if !unicode.IsLetter(formatRune) && !unicode.IsDigit(formatRune) {
				skipSpaces()
				if inputOffset < len(input) {
					inputRune, _ := utf8.DecodeRuneInString(input[inputOffset:])
					if !unicode.IsLetter(inputRune) && !unicode.IsDigit(inputRune) && !unicode.IsSpace(inputRune) {
						advanceInput()
					}
				}
				skipSpaces()
			} else {
				advanceInput()
			}
			offset += size
			continue
		}
		offset += len(spelling)
		if !exact {
			skipSpaces()
		}
		width := nativePictureInputWidth(field)
		switch field {
		case "MONTH", "MON":
			names := englishMonths[:]
			if field == "MON" {
				names = englishMonthAbbreviations[:]
			}
			if translation {
				names = locale.MonthNames[:]
				if field == "MON" {
					names = locale.AbbreviatedMonthNames[:]
				}
			}
			month, found := readName(names)
			if !found {
				return result, temporalBadFormat()
			}
			if !setParsed(&result.month, &monthSet, month+1) { return result, temporalBadFormat() }
			gregorian = true
		case "RM":
			start := inputOffset
			for inputOffset < len(input) {
				current, size := utf8.DecodeRuneInString(input[inputOffset:])
				if !unicode.IsLetter(current) {
					break
				}
				inputOffset += size
			}
			month, ok := nativeParseRoman(input[start:inputOffset])
			if !ok || month > 12 {
				return result, temporalBadFormat()
			}
			if !setParsed(&result.month, &monthSet, int(month)) { return result, temporalBadFormat() }
			gregorian = true
		case "DAY", "DY":
			names := englishDays[:]
			if field == "DY" {
				names = englishDayAbbreviations[:]
			}
			if translation {
				names = locale.DayNames[:]
				if field == "DY" {
					names = locale.AbbreviatedDayNames[:]
				}
			}
			if _, found := readName(names); !found {
				return result, temporalBadFormat()
			}
		case "AM", "PM", "A.M.", "P.M.":
			length := 2
			if strings.Contains(field, ".") {
				length = 4
			}
			if inputOffset+length > len(input) {
				return result, temporalBadFormat()
			}
			word := strings.ToUpper(input[inputOffset : inputOffset+length])
			if word != "AM" && word != "PM" && word != "A.M." && word != "P.M." {
				return result, temporalBadFormat()
			}
			inputOffset += length
			pm := strings.HasPrefix(word, "P")
			if result.meridian && result.pm != pm { return result, temporalBadFormat() }
			result.meridian, result.pm, clock12 = true, pm, true
		case "AD", "BC", "A.D.", "B.C.":
			length := 2
			if strings.Contains(field, ".") {
				length = 4
			}
			if inputOffset+length > len(input) {
				return result, temporalBadFormat()
			}
			word := strings.ToUpper(input[inputOffset : inputOffset+length])
			if word != "AD" && word != "BC" && word != "A.D." && word != "B.C." {
				return result, temporalBadFormat()
			}
			inputOffset += length
			bc := strings.HasPrefix(word, "B")
			if eraSet && result.bc != bc { return result, temporalBadFormat() }
			result.bc, eraSet = bc, true
		case "TZ":
			start := inputOffset
			for inputOffset < len(input) {
				current, size := utf8.DecodeRuneInString(input[inputOffset:])
				if !unicode.IsLetter(current) {
					break
				}
				inputOffset += size
			}
			if inputOffset == start {
				return result, temporalBadFormat()
			}
			result.zone = input[start:inputOffset]
		case "TZH", "OF":
			start := inputOffset
			if inputOffset < len(input) && (input[inputOffset] == '+' || input[inputOffset] == '-') {
				inputOffset++
			}
			if _, _, ok := readDigits(1, 2); !ok {
				return result, temporalBadFormat()
			}
			if field == "OF" && inputOffset < len(input) && input[inputOffset] == ':' {
				inputOffset++
				if _, _, ok := readDigits(2, 2); !ok {
					return result, temporalBadFormat()
				}
			}
			result.zone = input[start:inputOffset]
		case "TZM":
			start := inputOffset
			if _, _, ok := readDigits(2, 2); !ok {
				return result, temporalBadFormat()
			}
			if result.zone == "" {
				result.zone = "+00:" + input[start:inputOffset]
			} else {
				result.zone += ":" + input[start:inputOffset]
			}
		case "Y,YYY":
			negative := false
			if inputOffset < len(input) && input[inputOffset] == '-' {
				negative = true
				inputOffset++
			}
			high, _, ok := readDigits(1, 10)
			if !ok || inputOffset >= len(input) || input[inputOffset] != ',' {
				return result, temporalBadFormat()
			}
			inputOffset++
			low, _, ok := readDigits(3, 3)
			if !ok || high > (math.MaxInt64-low)/1000 {
				return result, temporalOverflow()
			}
			value := high*1000+low
			if !setParsedYear(&result.year, &yearSet, &yearNegative, value, negative) { return result, temporalBadFormat() }
			yearValue, yearDigits, gregorian = value, 4, true
		default:
			if width == 0 {
				continue
			}
			signedYear := field == "YYYY" || field == "YYY" || field == "YY" || field == "Y" || field == "IYYY" || field == "IYY" || field == "IY" || field == "I"
			negative := false
			if signedYear && inputOffset < len(input) && input[inputOffset] == '-' {
				negative = true
				inputOffset++
			}
			maximum := width
			if (field == "YYYY" || field == "IYYY") && offset < len(format) {
				next, _ := nativePictureToken(format, offset)
				if nativePictureInputWidth(next) == 0 {
					maximum = 10
				}
			}
			minimum := width
			if !exact {
				minimum = 1
			}
			value, digits, ok := readDigits(minimum, maximum)
			if !ok {
				return result, temporalBadFormat()
			}
			switch field {
			case "YYYY":
				if !setParsedYear(&result.year, &yearSet, &yearNegative, value, negative) { return result, temporalBadFormat() }
				yearValue, yearDigits, gregorian = value, 4, true
			case "YYY", "YY", "Y":
				if !setParsedYear(&result.year, &yearSet, &yearNegative, nearestYear(value), negative) { return result, temporalBadFormat() }
				yearValue, yearDigits, gregorian = value, width, true
			case "IYYY":
				if !setParsedYear(&result.isoYear, &isoYearSet, &isoNegative, value, negative) { return result, temporalBadFormat() }
				isoYearDigits, iso = 4, true
			case "IYY", "IY", "I":
				if !setParsedYear(&result.isoYear, &isoYearSet, &isoNegative, nearestYear(value), negative) { return result, temporalBadFormat() }
				isoYearDigits, iso = width, true
			case "IW":
				if value < 1 || value > 53 {
					return result, temporalOverflow()
				}
				if !setParsed(&isoWeek, &isoWeekSet, int(value)) { return result, temporalBadFormat() }
				iso = true
			case "ID":
				if value < 1 || value > 7 {
					return result, temporalOverflow()
				}
				if !setParsed(&isoDay, &isoDaySet, int(value)) { return result, temporalBadFormat() }
				iso = true
			case "IDDD":
				if value < 1 || value > 371 {
					return result, temporalOverflow()
				}
				if isoOrdinalSet && result.isoOrdinal != int(value) { return result, temporalBadFormat() }
				result.isoOrdinal, isoOrdinalSet, iso = int(value), true, true
			case "CC":
				if value < 1 {
					return result, temporalOverflow()
				}
				century = value
			case "Q":
				if value < 1 || value > 4 {
					return result, temporalOverflow()
				}
			case "W":
				if value < 1 || value > 5 {
					return result, temporalOverflow()
				}
				if !setParsed(&weekOfMonth, &weekOfMonthSet, int(value)) { return result, temporalBadFormat() }
				gregorian = true
			case "WW":
				if value < 1 || value > 53 {
					return result, temporalOverflow()
				}
				if weekOfYearSet && weekOfYear != value { return result, temporalBadFormat() }
				weekOfYear, weekOfYearSet, gregorian = value, true, true
			case "D":
				if value < 1 || value > 7 {
					return result, temporalOverflow()
				}
				gregorian = true
			case "MM":
				if !setParsed(&result.month, &monthSet, int(value)) { return result, temporalBadFormat() }
				gregorian = true
			case "DD":
				if !setParsed(&result.day, &daySet, int(value)) { return result, temporalBadFormat() }
				gregorian = true
			case "DDD":
				ordinalDay, gregorian = value, true
			case "HH24":
				if !setParsed(&result.hour, &hourSet, int(value)) { return result, temporalBadFormat() }
			case "HH12", "HH":
				if !setParsed(&result.hour, &hourSet, int(value)) { return result, temporalBadFormat() }
				clock12 = true
			case "MI":
				if !setParsed(&result.minute, &minuteSet, int(value)) { return result, temporalBadFormat() }
			case "SS":
				if !setParsed(&result.second, &secondSet, int(value)) { return result, temporalBadFormat() }
			case "SSSS", "SSSSS":
				if value > 86399 {
					return result, temporalOverflow()
				}
				if !setParsed(&secondsOfDay, &secondsOfDaySet, int(value)) { return result, temporalBadFormat() }
			case "MS", "MILLISECOND":
				for digits < 6 {
					value *= 10
					digits++
				}
				if !setParsed(&milliseconds, &millisecondsSet, int(value)) { return result, temporalBadFormat() }
			case "US", "MICROSECOND", "FF1", "FF2", "FF3", "FF4", "FF5", "FF6":
				for digits < 6 {
					value *= 10
					digits++
				}
				if !setParsed(&microseconds, &microsecondsSet, int(value)) { return result, temporalBadFormat() }
			case "J":
				if value < 1 {
					return result, temporalOverflow()
				}
				result.julian, result.hasJulian = value, true
			}
		}
		if offset+2 <= len(format) {
			switch strings.ToUpper(format[offset:offset+2]) {
			case "TH":
				if !advanceInput() || !advanceInput() {
					return result, temporalBadFormat()
				}
				offset += 2
			case "SP":
				offset += 2
			}
		}
	}
	if !exact {
		skipSpaces()
	}
	if inputOffset != len(input) {
		return result, temporalBadFormat()
	}
	if gregorian && iso {
		return result, sqlError("22007", "invalid combination of date conventions")
	}
	if century != 0 {
		if yearDigits == 0 {
			result.year = (century-1)*100 + 1
		} else if yearDigits <= 2 {
			result.year = (century-1)*100 + yearValue
			if yearValue == 0 {
				result.year = century * 100
			}
		}
	}
	if secondsOfDaySet {
		if !hourSet { result.hour = secondsOfDay / 3600 }
		if !minuteSet { result.minute = secondsOfDay / 60 % 60 }
		if !secondSet { result.second = secondsOfDay % 60 }
	}
	result.micros = milliseconds + microseconds
	if weekOfMonthSet { result.day = (weekOfMonth-1)*7+1 }
	if weekOfYearSet { ordinalDay = (weekOfYear-1)*7+1 }
	if yearNegative {
		result.year = 1 - result.year
	}
	if isoNegative {
		result.isoYear = 1 - result.isoYear
	}
	if result.bc {
		if iso && isoYearDigits != 0 {
			result.isoYear = 1 - result.isoYear
		} else if yearDigits != 0 || century != 0 {
			result.year = 1 - result.year
		}
	}
	if ordinalDay != 0 && (result.month <= 1 || result.day <= 1) {
		if yearDigits == 0 && century == 0 && !result.bc {
			return result, sqlError("22007", "cannot calculate day of year without year information")
		}
		year := result.year
		days := dateFieldJulianDay(year, 1, 1) - dateEpochJulian + ordinalDay - 1
		y, month, day := dateCalendar(days)
		if y != year {
			return result, temporalOverflow()
		}
		if result.month <= 1 { result.month = int(month) }
		if result.day <= 1 { result.day = int(day) }
	}
	if isoWeekSet {
		day := 1
		if isoDaySet { day = isoDay }
		result.isoOrdinal = (isoWeek-1)*7 + day
	} else if isoDaySet && !isoOrdinalSet {
		result.isoOrdinal = isoDay
	}
	if iso {
		isoYear := result.isoYear
		if result.isoOrdinal == 0 {
			result.isoOrdinal = 1
		}
		y, month, day := dateCalendar(dateISOYearStart(isoYear) - dateEpochJulian + int64(result.isoOrdinal) - 1)
		if dateISOField(dateFieldISOYear, y, month, day) != isoYear {
			return result, temporalOverflow()
		}
		result.year, result.month, result.day = y, int(month), int(day)
	}
	if result.hasJulian {
		result.year, result.month, result.day = func() (int64, int, int) {
			year, month, day := dateCalendar(result.julian - dateEpochJulian)
			return year, int(month), int(day)
		}()
	}
	if clock12 {
		if result.hour < 1 || result.hour > 12 {
			return result, temporalOverflow()
		}
		if result.hour == 12 {
			result.hour = 0
		}
		if result.pm {
			result.hour += 12
		}
	}
	if result.month < 1 || result.month > 12 || result.day < 1 || int64(result.day) > timestampMonthDays(result.year, int64(result.month)) || result.hour < 0 || result.hour > 23 || result.minute < 0 || result.minute > 59 || result.second < 0 || result.second > 59 || result.micros < 0 || result.micros >= int(intervalMicrosPerSecond) {
		return result, temporalOverflow()
	}
	return result, nil
}

func nativePictureTimestamp(ctx context.Context, input, format string, zone namedTimeZone, dateOnly bool, locale model.FormatLocale) (model.Value,error) {
	parsed,err:=nativeParseDatePicture(input,format,locale);if err!=nil{return model.Value{},err}
	fields:=temporalInputFields{Year:parsed.year,Month:parsed.month,Day:parsed.day,TimeMicros:int64(parsed.hour)*intervalMicrosPerHour+int64(parsed.minute)*intervalMicrosPerMinute+int64(parsed.second)*intervalMicrosPerSecond+int64(parsed.micros),HasDate:true,HasTime:!dateOnly,Zone:parsed.zone,HasZone:parsed.zone!="",Kind:temporalInputCivil}
	if dateOnly { days:=dateFieldJulianDay(fields.Year,int64(fields.Month),int64(fields.Day))-dateEpochJulian;if !validDate(days){return model.Value{},sqlError("22008","date out of range")};return dateValue(days),nil }
	return temporalTimestampFields(ctx,fields,temporalInputCivil,true,zone)
}

func nativeFormatNumberPicture(input, picture string, locale model.FormatLocale) (string, error) {
	fill := false
	var cleaned strings.Builder
	quoted := false
	for index := 0; index < len(picture); {
		if picture[index] == '"' {
			quoted = !quoted
			cleaned.WriteByte(picture[index])
			index++
			continue
		}
		if !quoted && index+2 <= len(picture) && strings.EqualFold(picture[index:index+2], "FM") {
			fill = true
			index += 2
			continue
		}
		cleaned.WriteByte(picture[index])
		index++
	}
	picture = cleaned.String()
	upper := strings.ToUpper(picture)
	romanFormat, lowerRoman, bracketNegative := false, false, false
	placedSigns, localeSigns := 0, 0
	exponent := -1
	localeDecimal := false
	quoted = false
	for index := 0; index < len(picture); {
		if picture[index] == '"' { quoted = !quoted; index++; continue }
		if quoted {
			if picture[index] == '\\' && index+1 < len(picture) { index++ }
			_, size := utf8.DecodeRuneInString(picture[index:])
			index += size
			continue
		}
		if index+4 <= len(picture) && strings.EqualFold(picture[index:index+4], "EEEE") {
			if exponent >= 0 { return "", sqlError("22023", "invalid numeric format") }
			exponent = index
			index += 4
			continue
		}
		if index+2 <= len(picture) {
			token := upper[index:index+2]
			switch token {
			case "RN":
				romanFormat = true
				lowerRoman = picture[index:index+2] == "rn"
				index += 2
				continue
			case "PR":
				bracketNegative = true
				index += 2
				continue
			case "MI", "PL", "SG":
				placedSigns++
				index += 2
				continue
			case "SP":
				index += 2
				continue
		}
		}
		if upper[index] == 'D' { localeDecimal = true }
		if upper[index] == 'S' { localeSigns++ }
		index++
	}
	if quoted { return "", sqlError("22023", "invalid numeric format") }
	if localeSigns > 1 || localeSigns != 0 && (placedSigns != 0 || bracketNegative) || bracketNegative && placedSigns != 0 {
		return "", sqlError("22023", "invalid numeric format")
	}
	if romanFormat && !strings.EqualFold(picture, "RN") { return "", sqlError("22023", "invalid numeric format") }
	if romanFormat {
		value, err := strconv.ParseFloat(input, 64)
		if err != nil || value < 1 || value > 3999 { return strings.Repeat("#", 15), nil }
		roman := nativeRoman(int64(math.Round(value)))
		if lowerRoman { roman = strings.ToLower(roman) }
		if !fill { roman = fmt.Sprintf("%15s", roman) }
		return roman, nil
	}
	if exponent >= 0 {
		if exponent+4 != len(upper) || fill {
			return "", sqlError("22023", "invalid numeric format")
		}
		digits, digitCount, decimalCount := 0, 0, 0
		afterDecimal := false
		for _, token := range upper[:exponent] {
			switch token {
			case '9', '0':
				digitCount++
				if afterDecimal { digits++ }
			case '.', 'D':
				decimalCount++
				afterDecimal = true
			default:
				return "", sqlError("22023", "invalid numeric format")
			}
		}
		if digitCount == 0 || decimalCount > 1 {
			return "", sqlError("22023", "invalid numeric format")
		}
		text, err := nativeScientificDecimal(input, digits)
		if err != nil { return "", err }
		if localeDecimal {
			text = strings.Replace(text, ".", locale.DecimalPoint, 1)
		}
		if !strings.HasPrefix(text, "-") { text = " " + text }
		return text, nil
	}

	decimal := -1
	virtual := -1
	var integerSlots, fractionSlots []int
	quoted = false
	for index := 0; index < len(picture); index++ {
		if picture[index] == '"' { quoted = !quoted; continue }
		if quoted { continue }
		switch upper[index] {
		case '.', 'D':
			if decimal >= 0 || virtual >= 0 { return "", sqlError("22023", "invalid numeric format") }
			decimal = index
		case 'V':
			if virtual >= 0 || decimal >= 0 { return "", sqlError("22023", "invalid numeric format") }
			virtual = index
		case '9', '0':
			if decimal >= 0 { fractionSlots = append(fractionSlots, index) } else { integerSlots = append(integerSlots, index) }
		}
	}
	if virtual >= 0 {
		shift := 0
		for _, position := range integerSlots { if position > virtual { shift++ } }
		input = nativeShiftDecimal(input, shift)
	}
	whole, fraction, negative, err := nativeRoundDecimal(input, len(fractionSlots))
	if err != nil { return "", err }
	if len(whole) > len(integerSlots) { return strings.Repeat("#", max(1, len(picture))), nil }

	renderedDigits := make([]string, len(picture))
	wholeDigits := whole
	mandatoryInteger := -1
	for index, position := range integerSlots { if upper[position] == '0' { mandatoryInteger = index; break } }
	if whole == "0" && mandatoryInteger < 0 { wholeDigits = "" }
	source := len(wholeDigits)-1
	for index := len(integerSlots)-1; index >= 0; index-- {
		position := integerSlots[index]
		if source >= 0 {
			renderedDigits[position] = wholeDigits[source:source+1]
			source--
		} else if upper[position] == '0' || mandatoryInteger >= 0 && index >= mandatoryInteger {
			renderedDigits[position] = "0"
		} else if !fill {
			renderedDigits[position] = " "
		}
	}
	lastFraction := len(fractionSlots)-1
	if fill {
		for lastFraction >= 0 && upper[fractionSlots[lastFraction]] == '9' && fraction[lastFraction] == '0' { lastFraction-- }
	}
	for index, position := range fractionSlots {
		if index <= lastFraction || upper[position] == '0' { renderedDigits[position] = fraction[index:index+1] }
	}

	var out strings.Builder
	firstDigitOutput := -1
	lastDigitPosition := -1
	if len(integerSlots) != 0 { lastDigitPosition = integerSlots[len(integerSlots)-1] }
	if len(fractionSlots) != 0 { lastDigitPosition = fractionSlots[len(fractionSlots)-1] }
	anchoredSign := false
	suffixSign := false
	ordinal := false
	uppercaseOrdinal := false
	explicitSign := bracketNegative
	quoted = false
	for index := 0; index < len(picture); {
		if picture[index] == '"' {
			quoted = !quoted
			index++
			continue
		}
		if quoted {
			if picture[index] == '\\' && index+1 < len(picture) { index++ }
			out.WriteByte(picture[index])
			index++
			continue
		}
		if index+2 <= len(picture) {
			token := upper[index:index+2]
			switch token {
			case "PR", "SP":
				index += 2
				continue
			case "MI", "PL", "SG":
				explicitSign = true
				switch token {
				case "MI": if negative { out.WriteByte('-') } else if !fill { out.WriteByte(' ') }
				case "PL": if !negative { out.WriteByte('+') } else if !fill { out.WriteByte(' ') }
				case "SG": if negative { out.WriteByte('-') } else { out.WriteByte('+') }
				}
				index += 2
				continue
			case "TH":
				ordinal = true
				uppercaseOrdinal = picture[index:index+2] == "TH"
				index += 2
				continue
			}
		}
		if upper[index] == 'S' {
			explicitSign = true
			if decimal >= 0 && index > decimal || decimal < 0 && index > lastDigitPosition { suffixSign = true } else { anchoredSign = true }
			index++
			continue
		}
		if renderedDigits[index] != "" {
			if firstDigitOutput < 0 { firstDigitOutput = out.Len() }
			out.WriteString(renderedDigits[index])
			index++
			continue
		}
		switch upper[index] {
		case '9', '0', 'V', 'B':
		case '.':
			if firstDigitOutput < 0 { firstDigitOutput = out.Len() }
			out.WriteByte('.')
		case 'D':
			if firstDigitOutput < 0 { firstDigitOutput = out.Len() }
			out.WriteString(locale.DecimalPoint)
		case ',', 'G':
			left := false
			for _, position := range integerSlots { if position < index && strings.TrimSpace(renderedDigits[position]) != "" { left = true; break } }
			separator := ","
			if upper[index] == 'G' { separator = locale.ThousandsSeparator }
			if left {
				out.WriteString(separator)
			} else if !fill {
				out.WriteString(strings.Repeat(" ", utf8.RuneCountInString(separator)))
			}
		case 'L':
			out.WriteString(locale.CurrencySymbol)
		case 'C':
			out.WriteString(locale.ISOCurrencySymbol)
		default:
			out.WriteByte(picture[index])
		}
		index++
	}
	result := out.String()
	if bracketNegative {
		if negative { result = "<" + strings.TrimSpace(result) + ">" } else if !fill { result = " " + result + " " }
	} else {
		sign := " "
		if anchoredSign || suffixSign {
			sign = locale.PositiveSign
			if negative { sign = locale.NegativeSign }
		} else if negative {
			sign = "-"
		}
		if suffixSign {
			result += sign
		} else if firstDigitOutput >= 0 && (anchoredSign || !explicitSign) {
			position := firstDigitOutput
			for position < len(result) && result[position] == ' ' { position++ }
			result = result[:position] + sign + result[position:]
		}
	}
	if ordinal && !negative && len(fractionSlots) == 0 {
		value, _ := strconv.ParseInt(whole, 10, 64)
		result += nativeOrdinal(value, uppercaseOrdinal)
	}
	if fill { result = strings.TrimSpace(result) }
	return result, nil
}

func nativeShiftDecimal(input string, places int) string {
	sign := ""; if strings.HasPrefix(input, "-") || strings.HasPrefix(input, "+") { sign, input = input[:1], input[1:] }
	whole, fraction, _ := strings.Cut(input, ".")
	digits := whole+fraction
	position := len(whole)+places
	if position >= len(digits) { return sign+digits+strings.Repeat("0", position-len(digits)) }
	if position <= 0 { return sign+"0."+strings.Repeat("0", -position)+digits }
	return sign+digits[:position]+"."+digits[position:]
}

func nativeScientificDecimal(input string, scale int) (string, error) {
	negative := strings.HasPrefix(input, "-")
	input = strings.TrimPrefix(strings.TrimPrefix(input, "-"), "+")
	mantissa, exponentText := input, ""
	if position := strings.IndexAny(input, "eE"); position >= 0 {
		mantissa, exponentText = input[:position], input[position+1:]
		if exponentText == "" { return "", sqlError("22003", "numeric value is out of range") }
	}
	exponent := int64(0)
	if exponentText != "" {
		value, err := strconv.ParseInt(exponentText, 10, 64)
		if err != nil { return "", sqlError("22003", "numeric value is out of range") }
		exponent = value
	}
	var coefficient strings.Builder
	decimal := -1
	for _, current := range mantissa {
		switch {
		case current == '.' && decimal < 0:
			decimal = coefficient.Len()
		case current >= '0' && current <= '9':
			coefficient.WriteRune(current)
		default:
			return "", sqlError("22003", "numeric value is out of range")
		}
	}
	if decimal < 0 { decimal = coefficient.Len() }
	raw := coefficient.String()
	if raw == "" { return "", sqlError("22003", "numeric value is out of range") }
	first := strings.IndexFunc(raw, func(current rune) bool { return current != '0' })
	if first < 0 {
		number := "0"
		if scale != 0 { number += "." + strings.Repeat("0", scale) }
		return number + "e+00", nil
	}
	delta := int64(decimal-first-1)
	if delta > 0 && exponent > math.MaxInt64-delta || delta < 0 && exponent < math.MinInt64-delta {
		return "", sqlError("22003", "numeric value is out of range")
	}
	exponent += delta
	significand := raw[first:]
	target := scale + 1
	if len(significand) <= target {
		significand += strings.Repeat("0", target+1-len(significand))
	}
	rounded := []byte(significand[:target])
	if significand[target] >= '5' {
		carry := true
		for index := len(rounded)-1; index >= 0 && carry; index-- {
			if rounded[index] == '9' {
				rounded[index] = '0'
			} else {
				rounded[index]++
				carry = false
			}
		}
		if carry {
			if exponent == math.MaxInt64 { return "", sqlError("22003", "numeric value is out of range") }
			rounded[0] = '1'
			exponent++
		}
	}
	exponentSign := "+"
	exponentDigits := strconv.FormatInt(exponent, 10)
	if exponent < 0 {
		exponentSign = "-"
		exponentDigits = strings.TrimPrefix(exponentDigits, "-")
	}
	if len(exponentDigits) < 2 { exponentDigits = "0" + exponentDigits }
	sign := ""
	if negative { sign = "-" }
	number := string(rounded[:1])
	if scale != 0 { number += "." + string(rounded[1:]) }
	return sign + number + "e" + exponentSign + exponentDigits, nil
}

func nativeRoundDecimal(input string, scale int) (string, string, bool, error) {
	if strings.ContainsAny(input, "eE") { value,err:=strconv.ParseFloat(input,64);if err!=nil{return "","",false,sqlError("22003","numeric value is out of range")};input=strconv.FormatFloat(value,'f',scale,64) }
	negative:=strings.HasPrefix(input,"-");input=strings.TrimPrefix(strings.TrimPrefix(input,"-"),"+")
	whole,fraction,_:=strings.Cut(input,".");if whole==""{whole="0"}
	for _,r:=range whole+fraction{if r<'0'||r>'9'{return "","",false,sqlError("22003","numeric value is out of range")}}
	if len(fraction)>scale {
		round:=fraction[scale]>='5';fraction=fraction[:scale]
		if round { digits:=[]byte(whole+fraction);carry:=byte(1);for index:=len(digits)-1;index>=0&&carry!=0;index--{if digits[index]=='9'{digits[index]='0'}else{digits[index]++;carry=0}};if carry!=0{digits=append([]byte{'1'},digits...)};split:=len(digits)-scale;whole,fraction=string(digits[:split]),string(digits[split:]) }
	}
	if len(fraction)<scale{fraction+=strings.Repeat("0",scale-len(fraction))}
	whole=strings.TrimLeft(whole,"0");if whole==""{whole="0"};if whole=="0"&&strings.Trim(fraction,"0")==""{negative=false}
	return whole,fraction,negative,nil
}

func nativeParseNumberPicture(input, picture string, locale model.FormatLocale) (string,error) {
	upper:=strings.ToUpper(picture)
	quoted:=false
	decimalFields,virtualFields:=0,0
	roman,romanOther:=false,false
	for offset:=0;offset<len(picture); {
		if picture[offset]=='"' { quoted=!quoted;romanOther=true;offset++;continue }
		if quoted { romanOther=true;_,size:=utf8.DecodeRuneInString(picture[offset:]);offset+=size;continue }
		if offset+2<=len(picture) {
			token:=upper[offset:offset+2]
			switch token {
			case "RN":
				roman=true
				offset+=2
				continue
			case "FM":
				offset+=2
				continue
			case "MI","PL","SG","PR","SP","TH":
				romanOther=true
				offset+=2
				continue
			}
		}
		switch upper[offset] {
		case '.','D': decimalFields++
		case 'V': virtualFields++
		}
		romanOther=true
		offset++
	}
	if quoted||decimalFields>1||virtualFields>1||decimalFields!=0&&virtualFields!=0||roman&&romanOther{return "",sqlError("22023","invalid numeric format")}
	if roman {
		value,ok:=nativeParseRoman(strings.TrimSpace(input));if !ok{return "",sqlError("22P02","invalid input syntax for type numeric")}
		return strconv.FormatInt(value,10),nil
	}

	input=strings.TrimSpace(input)
	negative:=false
	if strings.HasPrefix(input,"<")&&strings.HasSuffix(input,">") {
		negative=true
		input=strings.TrimSpace(input[1:len(input)-1])
	}
	stripSign:=func(sign string, isNegative bool) bool {
		if sign=="" { return false }
		if strings.HasPrefix(input,sign) { input=strings.TrimSpace(input[len(sign):]);negative=isNegative;return true }
		if strings.HasSuffix(input,sign) { input=strings.TrimSpace(input[:len(input)-len(sign)]);negative=isNegative;return true }
		return false
	}
	if !stripSign(locale.NegativeSign,true)&&!stripSign("-",true) {
		if !stripSign(locale.PositiveSign,false) { stripSign("+",false) }
	}

	var digits strings.Builder
	inputOffset:=0
	virtual:=false
	virtualScale:=0
	sawDigit:=false
	skipSpaces:=func(){for inputOffset<len(input){r,size:=utf8.DecodeRuneInString(input[inputOffset:]);if !unicode.IsSpace(r){break};inputOffset+=size}}
	consumeExact:=func(value string) bool {
		if value!=""&&strings.HasPrefix(input[inputOffset:],value){inputOffset+=len(value);return true}
		return false
	}
	skipInputRune:=func() {
		if inputOffset<len(input){_,size:=utf8.DecodeRuneInString(input[inputOffset:]);inputOffset+=size}
	}
	inputData:=func() bool {
		if inputOffset>=len(input){return false}
		r,_:=utf8.DecodeRuneInString(input[inputOffset:])
		if r>='0'&&r<='9'||r=='+'||r=='-'||r=='.'||r==','{return true}
		remaining:=input[inputOffset:]
		return locale.DecimalPoint!=""&&strings.HasPrefix(remaining,locale.DecimalPoint)||
			locale.ThousandsSeparator!=""&&strings.HasPrefix(remaining,locale.ThousandsSeparator)||
			locale.PositiveSign!=""&&strings.HasPrefix(remaining,locale.PositiveSign)||
			locale.NegativeSign!=""&&strings.HasPrefix(remaining,locale.NegativeSign)
	}
	skipNonData:=func(count int) {
		for count>0&&!inputData()&&inputOffset<len(input){skipInputRune();count--}
	}
	consumeSign:=func(positive,negativeSign bool) bool {
		if negativeSign&&(consumeExact(locale.NegativeSign)||consumeExact("-")){negative=true;return true}
		if positive&&(consumeExact(locale.PositiveSign)||consumeExact("+")){negative=false;return true}
		return false
	}
	quoted=false
	for offset:=0;offset<len(picture); {
		if picture[offset]=='"' { quoted=!quoted;offset++;continue }
		if quoted {
			if picture[offset]=='\\'&&offset+1<len(picture){offset++}
			skipInputRune()
			_,size:=utf8.DecodeRuneInString(picture[offset:])
			offset+=size
			continue
		}
		if offset+2<=len(picture) {
			token:=upper[offset:offset+2]
			switch token {
			case "FM":
				offset+=2
				continue
			case "MI":
				skipSpaces()
				if !consumeSign(false,true){skipNonData(1)}
				offset+=2
				continue
			case "PL":
				skipSpaces()
				if !consumeSign(true,false){skipNonData(1)}
				offset+=2
				continue
			case "SG":
				skipSpaces()
				if !consumeSign(true,true){skipNonData(1)}
				offset+=2
				continue
			case "PR","SP":
				offset+=2
				continue
			case "TH":
				skipNonData(2)
				offset+=2
				continue
			}
		}
		switch upper[offset] {
		case '9','0':
			skipSpaces()
			if inputOffset<len(input) {
				r,size:=utf8.DecodeRuneInString(input[inputOffset:])
				if r>='0'&&r<='9' {
					digits.WriteRune(r)
					inputOffset+=size
					sawDigit=true
				}
			}
			if virtual { virtualScale++ }
		case '.':
			if consumeExact(".") { digits.WriteByte('.') }
		case 'D':
			if consumeExact(locale.DecimalPoint) { digits.WriteByte('.') }
		case ',':
			consumeExact(",")
		case 'G':
			consumeExact(locale.ThousandsSeparator)
		case 'L':
			if !consumeExact(locale.CurrencySymbol){skipNonData(max(1,utf8.RuneCountInString(locale.CurrencySymbol)))}
		case 'C':
			if !consumeExact(locale.ISOCurrencySymbol){skipNonData(max(1,utf8.RuneCountInString(locale.ISOCurrencySymbol)))}
		case 'B':
		case 'S':
			skipSpaces()
			consumeSign(true,true)
		case 'V':
			virtual=true
		default:
			skipInputRune()
		}
		offset++
	}
	skipSpaces()
	if inputOffset!=len(input)||!sawDigit{return "",sqlError("22P02","invalid input syntax for type numeric")}
	text:=digits.String()
	if text==""||text=="."{return "",sqlError("22P02","invalid input syntax for type numeric")}
	if strings.HasPrefix(text,"."){text="0"+text}
	if strings.HasSuffix(text,"."){text+="0"}
	if virtualScale!=0{text=nativeShiftDecimal(text,-virtualScale)}
	if negative{text="-"+text}
	return text,nil
}


func nativeFormatIntervalPicture(value model.Interval, format string) string {
	years, months := int64(value.Months/12), int64(value.Months%12)
	days, micros := int64(value.Days), value.Microseconds
	hours := micros / intervalMicrosPerHour
	minutes := micros / intervalMicrosPerMinute % 60
	seconds := micros / intervalMicrosPerSecond % 60
	fraction := micros % intervalMicrosPerSecond
	if fraction < 0 { fraction = -fraction }
	fractionText := fmt.Sprintf("%06d", fraction)
	var out strings.Builder
	for offset := 0; offset < len(format); {
		if format[offset] == '"' {
			offset++
			for offset < len(format) && format[offset] != '"' { if format[offset] == '\\' && offset+1 < len(format) { offset++ }; out.WriteByte(format[offset]); offset++ }
			if offset < len(format) { offset++ }
			continue
		}
		fill := false
		if offset+2 <= len(format) && strings.EqualFold(format[offset:offset+2], "FM") { fill = true; offset += 2 }
		field, spelling := nativePictureToken(format, offset)
		if field == "" { out.WriteByte(format[offset]); offset++; continue }
		offset += len(spelling)
		var text string
		var ordinal int64
		switch field {
		case "YYYY": ordinal = years; text = fmt.Sprintf("%04d", years)
		case "YYY": ordinal = years%1000; text = fmt.Sprintf("%03d", ordinal)
		case "YY": ordinal = years%100; text = fmt.Sprintf("%02d", ordinal)
		case "Y": ordinal = years%10; text = strconv.FormatInt(ordinal, 10)
		case "MM": ordinal = months; text = fmt.Sprintf("%02d", months)
		case "DD": ordinal = days; text = fmt.Sprintf("%02d", days)
		case "HH", "HH12": ordinal = hours%12; if ordinal == 0 { ordinal = 12 }; text = fmt.Sprintf("%02d", ordinal)
		case "HH24": ordinal = hours; text = fmt.Sprintf("%02d", hours)
		case "MI": ordinal = minutes; text = fmt.Sprintf("%02d", minutes)
		case "SS": ordinal = seconds; text = fmt.Sprintf("%02d", seconds)
		case "SSSS", "SSSSS": ordinal = hours*3600+minutes*60+seconds; text = strconv.FormatInt(ordinal, 10)
		case "MS", "MILLISECOND": text = fractionText[:3]
		case "US", "MICROSECOND": text = fractionText
		case "FF1", "FF2", "FF3", "FF4", "FF5", "FF6": text = fractionText[:int(field[2]-'0')]
		default: text = spelling
		}
		if fill { text = strings.TrimLeft(text, "0"); if text == "" { text = "0" } }
		if offset+2 <= len(format) {
			suffix := strings.ToUpper(format[offset:offset+2])
			if suffix == "TH" { text += nativeOrdinal(ordinal, format[offset:offset+2] == "TH"); offset += 2 }
			if suffix == "SP" { offset += 2 }
		}
		out.WriteString(text)
	}
	return out.String()
}
func nativeParseRoman(input string) (int64,bool) {
	input=strings.ToUpper(input);if input==""{return 0,false}
	values:=map[byte]int64{'I':1,'V':5,'X':10,'L':50,'C':100,'D':500,'M':1000}
	var total int64
	for index:=0;index<len(input);index++{current,ok:=values[input[index]];if !ok{return 0,false};if index+1<len(input)&&values[input[index+1]]>current{total-=current}else{total+=current}}
	return total,total>=1&&total<=3999&&nativeRoman(total)==input
}

func nativeRoman(value int64) string { values:=[]struct{n int64;s string}{{1000,"M"},{900,"CM"},{500,"D"},{400,"CD"},{100,"C"},{90,"XC"},{50,"L"},{40,"XL"},{10,"X"},{9,"IX"},{5,"V"},{4,"IV"},{1,"I"}};var out strings.Builder;for _,part:=range values{for value>=part.n{out.WriteString(part.s);value-=part.n}};return out.String() }
