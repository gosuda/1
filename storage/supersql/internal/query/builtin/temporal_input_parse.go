package builtin

import (
	"context"
	"math"
	"strconv"
	"strings"
)

type temporalDecoder struct {
	fields                                                   temporalInputFields
	mask                                                     uint8
	shortYear, textMonth, bc, era, meridian, weekday, julian bool
	dst                                                      bool
	pm                                                       bool
	doy                                                      int64
	order                                                    DateOrder
}

func temporalInteger(s string) (int64, error) {
	if len(s) == 0 {
		return 0, temporalBadFormat()
	}
	for i := range len(s) {
		if !intervalInputDigit(s[i]) {
			return 0, temporalBadFormat()
		}
	}
	v, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return 0, temporalOverflow()
	}
	return v, nil
}
func temporalFraction(s string, scale int64) (int64, error) {
	if len(s) == 0 || s[0] != '.' {
		return 0, temporalBadFormat()
	}
	for i := 1; i < len(s); i++ {
		if !intervalInputDigit(s[i]) {
			return 0, temporalBadFormat()
		}
	}
	if len(s) == 1 {
		return 0, nil
	}
	v, err := strconv.ParseFloat("0"+s, 64)
	if err != nil {
		return 0, temporalBadFormat()
	}
	// Julian decoding passes a double to dt2time's integral Timestamp argument.
	if scale == intervalMicrosPerDay {
		return int64(v * float64(scale)), nil
	}
	return int64(math.RoundToEven(v * float64(scale))), nil
}
func (p *temporalDecoder) number(s string) error {
	v, err := temporalInteger(s)
	if err != nil {
		return err
	}
	if len(s) == 3 && p.mask == 1 && v >= 1 && v <= 366 {
		p.doy = v
		p.mask = 7
		return nil
	}
	bit := uint8(0)
	switch p.mask {
	case 0:
		if len(s) >= 3 || p.order == DateYMD {
			bit = 1
		} else if p.order == DateDMY {
			bit = 4
		} else {
			bit = 2
		}
	case 1:
		bit = 2
	case 2:
		if p.textMonth && (len(s) >= 3 || p.order == DateYMD) {
			bit = 1
		} else {
			bit = 4
		}
	case 3:
		bit = 4
		if p.textMonth && len(s) >= 3 && p.shortYear {
			p.fields.Day = int(p.fields.Year)
			p.fields.Year = v
			p.shortYear = false
			p.mask |= bit
			return nil
		}
	case 4:
		bit = 2
	case 6:
		bit = 1
	default:
		return temporalBadFormat()
	}
	switch bit {
	case 1:
		p.fields.Year = v
		p.shortYear = len(s) <= 2
	case 2:
		p.fields.Month = int(v)
	case 4:
		p.fields.Day = int(v)
	}
	p.mask |= bit
	return nil
}
func (p *temporalDecoder) month(month int) error {
	if p.mask&2 != 0 {
		if p.textMonth || p.mask&4 != 0 || p.fields.Month < 1 || p.fields.Month > 31 {
			return temporalBadFormat()
		}
		p.fields.Day = p.fields.Month
		p.mask |= 4
	}
	p.fields.Month = month
	p.mask |= 2
	p.textMonth = true
	return nil
}
func (p *temporalDecoder) date(s string) error {
	var parts [25]string
	n := 0
	for start := 0; start < len(s); {
		for start < len(s) && !temporalAlpha(s[start]) && !intervalInputDigit(s[start]) {
			start++
		}
		if start == len(s) {
			return temporalBadFormat()
		}
		end := start
		if intervalInputDigit(s[start]) {
			for end < len(s) && intervalInputDigit(s[end]) {
				end++
			}
		} else {
			for end < len(s) && temporalAlpha(s[end]) {
				end++
			}
		}
		if end == start || n == len(parts) {
			return temporalBadFormat()
		}
		parts[n] = s[start:end]
		n++
		if end == len(s) {
			break
		}
		start = end + 1
	}
	// DecodeDate recognizes the textual month before assigning numeric fields.
	for i := range n {
		if temporalAlpha(parts[i][0]) {
			m := temporalMonth(parts[i])
			if m == 0 {
				return temporalBadFormat()
			}
			if err := p.month(m); err != nil {
				return err
			}
			parts[i] = ""
		}
	}
	for i := range n {
		if parts[i] != "" {
			if err := p.number(parts[i]); err != nil {
				return err
			}
		}
	}
	if p.mask != 7 {
		return temporalBadFormat()
	}
	return nil
}
func (p *temporalDecoder) time(s string) error {
	if p.fields.HasTime {
		return temporalBadFormat()
	}
	base, frac, hasFrac := strings.Cut(s, ".")
	var micros int64
	if hasFrac {
		var err error
		micros, err = temporalFraction("."+frac, intervalMicrosPerSecond)
		if err != nil {
			return err
		}
	}
	var hour, minute, second int64
	var err error
	if strings.Contains(base, ":") {
		a, b, ok := strings.Cut(base, ":")
		if !ok {
			return temporalBadFormat()
		}
		hour, err = temporalInteger(a)
		if err != nil {
			return err
		}
		b, c, seconds := strings.Cut(b, ":")
		minute, err = temporalInteger(b)
		if err != nil {
			return err
		}
		if seconds {
			second, err = temporalInteger(c)
			if err != nil {
				return err
			}
		} else if hasFrac {
			second = minute
			minute = hour
			hour = 0
		}
	} else {
		if len(base) != 4 && len(base) != 6 {
			return temporalBadFormat()
		}
		hour, err = temporalInteger(base[:2])
		if err != nil {
			return err
		}
		minute, err = temporalInteger(base[2:4])
		if err != nil {
			return err
		}
		if len(base) == 6 {
			second, err = temporalInteger(base[4:])
			if err != nil {
				return err
			}
		}
	}
	if hour > 24 || minute > 59 || second > 60 {
		return temporalOverflow()
	}
	total := hour*intervalMicrosPerHour + minute*intervalMicrosPerMinute + second*intervalMicrosPerSecond + micros
	if total > intervalMicrosPerDay {
		return temporalOverflow()
	}
	p.fields.TimeMicros = total
	p.fields.HasTime = true
	return nil
}
func (p *temporalDecoder) compact(s string, forceTime bool) error {
	if forceTime || p.mask == 7 || strings.Contains(s, ".") {
		return p.time(s)
	}
	if p.mask != 0 || len(s) < 6 {
		return p.number(s)
	}
	year, err := temporalInteger(s[:len(s)-4])
	if err != nil {
		return err
	}
	p.fields.Year = year
	p.shortYear = len(s) == 6
	p.mask = 1
	if err := p.number(s[len(s)-4 : len(s)-2]); err != nil {
		return err
	}
	return p.number(s[len(s)-2:])
}
func parseTemporalInputMode(ctx context.Context, text string, options ResolveOptions, timeOnly bool) (temporalInputFields, error) {
	var tokens [25]temporalToken
	count, err := temporalLex(ctx, text, &tokens)
	if err != nil {
		return temporalInputFields{}, err
	}
	decoder := temporalDecoder{order: options.DateOrder}
	prefix := byte(0)
	for index := range count {
		if err := ctx.Err(); err != nil {
			return temporalInputFields{}, err
		}
		if err := decoder.acceptToken(ctx, tokens[:count], index, &prefix, options, timeOnly); err != nil {
			return temporalInputFields{}, err
		}
	}
	if prefix != 0 || count == 0 {
		return temporalInputFields{}, temporalBadFormat()
	}
	if decoder.dst {
		if err := decoder.adjustDST(options); err != nil {
			return temporalInputFields{}, err
		}
	}
	civil := decoder.fields.Kind == temporalInputCivil || decoder.fields.Kind == temporalInputAllBalls
	if civil {
		if decoder.mask != 0 && decoder.mask != 7 {
			return temporalInputFields{}, temporalBadFormat()
		}
		if decoder.mask == 7 {
			if err := decoder.completeDate(); err != nil {
				return temporalInputFields{}, err
			}
		}
	}
	if decoder.meridian {
		if err := decoder.adjustMeridian(); err != nil {
			return temporalInputFields{}, err
		}
	}
	if !decoder.fields.HasDate && !decoder.fields.HasTime && decoder.fields.Kind == temporalInputCivil {
		return temporalInputFields{}, temporalBadFormat()
	}
	return decoder.fields, nil
}

func (p *temporalDecoder) adjustDST(options ResolveOptions) error {
	if !p.fields.HasZone {
		return temporalBadFormat()
	}
	offset, status := decodeNumericTimezone(p.fields.Zone)
	if status != intervalInputOK {
		session, _, err := resolveTimeZoneContext(options.TimeZone)
		if err != nil {
			return err
		}
		zone, dynamic, ok := timezoneLookup(p.fields.Zone, session)
		if !ok || dynamic || zone.record != nil || zone.posix {
			return temporalBadFormat()
		}
		offset = int64(zone.offset)
	}
	offset += 3600
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	p.fields.Zone = sign + strconv.FormatInt(offset/3600, 10) + ":" + strconv.FormatInt(offset%3600/60, 10) + ":" + strconv.FormatInt(offset%60, 10)
	return nil
}

func (p *temporalDecoder) completeDate() error {
	if !p.julian {
		if err := p.completeYear(); err != nil {
			return err
		}
	}
	if p.doy != 0 {
		year, month, day := dateCalendar(dateFieldJulianDay(p.fields.Year, 1, 1) - dateEpochJulian + p.doy - 1)
		p.fields.Year, p.fields.Month, p.fields.Day = year, int(month), int(day)
	}
	if p.fields.Month < 1 || p.fields.Month > 12 || p.fields.Day < 1 || int64(p.fields.Day) > timestampMonthDays(p.fields.Year, int64(p.fields.Month)) {
		return temporalOverflow()
	}
	p.fields.HasDate = true
	return nil
}

func (p *temporalDecoder) completeYear() error {
	if p.bc {
		if p.fields.Year <= 0 {
			return temporalOverflow()
		}
		p.fields.Year = 1 - p.fields.Year
		return nil
	}
	if p.shortYear {
		if p.fields.Year < 70 {
			p.fields.Year += 2000
		} else {
			p.fields.Year += 1900
		}
		return nil
	}
	if p.fields.Year <= 0 {
		return temporalOverflow()
	}
	return nil
}

func (p *temporalDecoder) adjustMeridian() error {
	hour := p.fields.TimeMicros / intervalMicrosPerHour
	if !p.fields.HasTime || hour > 12 {
		return temporalOverflow()
	}
	if !p.pm && hour == 12 {
		p.fields.TimeMicros -= 12 * intervalMicrosPerHour
	} else if p.pm && hour != 12 {
		p.fields.TimeMicros += 12 * intervalMicrosPerHour
	}
	return nil
}
func temporalDateStartsMonth(s string) bool {
	end := 0
	for end < len(s) && temporalAlpha(s[end]) {
		end++
	}
	return end > 0 && end < len(s) && (s[end] == '-' || s[end] == '/' || s[end] == '.') && temporalMonth(s[:end]) != 0
}
