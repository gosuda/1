package builtin

import (
	"context"
	"strings"
)

func (p *temporalDecoder) acceptToken(ctx context.Context, tokens []temporalToken, index int, prefix *byte, options ResolveOptions, timeOnly bool) error {
	token := tokens[index]
	text := token.text
	if *prefix == 'u' {
		if token.kind == 'n' {
			whole, _, _ := strings.Cut(text, ".")
			if _, err := temporalInteger(whole); err != nil {
				return err
			}
		}
		return temporalBadFormat()
	}
	if *prefix == 'j' {
		*prefix = 0
		return p.julianToken(token)
	}
	switch token.kind {
	case 't':
		*prefix = 0
		return p.time(text)
	case 'z':
		return p.numericZone(text)
	case 'n':
		datedNumber := index == 0 && strings.Contains(text, ".") && len(tokens) >= 2 && tokens[len(tokens)-1].kind == 'd'
		force := *prefix == 't' || (timeOnly && !datedNumber)
		*prefix = 0
		if !force && p.mask == 0 && strings.Contains(text, ".") {
			return p.date(text)
		}
		if len(text) >= 6 || force || p.mask == 7 {
			return p.compact(text, force)
		}
		return p.number(text)
	default:
		return p.wordToken(ctx, tokens, index, prefix, options, timeOnly)
	}
}

func (p *temporalDecoder) numericZone(text string) error {
	if p.fields.HasZone {
		return temporalBadFormat()
	}
	_, status := decodeNumericTimezone(text)
	if status == intervalInputOverflow {
		return sqlError("22009", "time zone displacement out of range")
	}
	if status != intervalInputOK {
		return temporalBadFormat()
	}
	p.fields.Zone, p.fields.HasZone = text, true
	return nil
}

func (p *temporalDecoder) julianToken(token temporalToken) error {
	if p.mask != 0 {
		return temporalBadFormat()
	}
	text := token.text
	if token.kind == 'd' {
		whole, zone, ok := strings.Cut(text, "-")
		if !ok {
			return temporalBadFormat()
		}
		text = whole
		if err := p.numericZone("-" + zone); err != nil {
			return err
		}
		p.fields.HasTime = true
	}
	whole, fraction, fractional := strings.Cut(text, ".")
	value, err := temporalInteger(whole)
	if err != nil {
		return err
	}
	year, month, day := dateCalendar(value - dateEpochJulian)
	p.fields.Year, p.fields.Month, p.fields.Day = year, int(month), int(day)
	p.mask, p.julian = 7, true
	if fractional {
		p.fields.TimeMicros, err = temporalFraction("."+fraction, intervalMicrosPerDay)
		if err != nil {
			return err
		}
		p.fields.HasTime = true
	}
	return nil
}

func (p *temporalDecoder) wordToken(ctx context.Context, tokens []temporalToken, index int, prefix *byte, options ResolveOptions, timeOnly bool) error {
	token := tokens[index]
	lower := strings.ToLower(token.text)
	signed := len(lower) > 1 && (lower[0] == '+' || lower[0] == '-')
	if token.kind == 's' && signed {
		lower = lower[:1] + strings.TrimSpace(lower[1:])
	}
	switch lower {
	case "at", "on":
		return nil
	case "t":
		dateMissing := !timeOnly && p.mask != 7
		if dateMissing || *prefix != 0 {
			return temporalBadFormat()
		}
		*prefix = 't'
	case "j", "jd", "julian":
		if *prefix != 0 {
			return temporalBadFormat()
		}
		*prefix = 'j'
	case "y", "m", "mm", "d", "h", "s", "dow", "doy", "isodow", "isoyear":
		if *prefix != 0 {
			return temporalBadFormat()
		}
		*prefix = 'u'
	case "am", "pm":
		if p.meridian {
			return temporalBadFormat()
		}
		p.meridian, p.pm = true, lower == "pm"
	case "ad", "bc":
		if p.era {
			return temporalBadFormat()
		}
		p.era, p.bc = true, lower == "bc"
	case "sun", "sunday", "mon", "monday", "tue", "tues", "tuesday", "wed", "weds", "wednesday", "thu", "thur", "thurs", "thursday", "fri", "friday", "sat", "saturday":
		if p.weekday {
			return temporalBadFormat()
		}
		p.weekday = true
	case "dst":
		if p.dst {
			return temporalBadFormat()
		}
		p.dst = true
	case "now", "today", "yesterday", "tomorrow", "epoch", "infinity", "+infinity", "-infinity":
		return p.specialToken(lower, timeOnly)
	case "allballs":
		if p.fields.HasTime || p.fields.HasZone {
			return temporalBadFormat()
		}
		p.fields.HasTime, p.fields.HasZone, p.fields.Zone = true, true, "+00"
		if p.mask == 0 {
			p.fields.Kind = temporalInputAllBalls
		}
	default:
		return p.dateOrZoneToken(ctx, tokens, index, options, timeOnly)
	}
	return nil
}

func (p *temporalDecoder) specialToken(text string, timeOnly bool) error {
	if timeOnly && text != "now" {
		return temporalBadFormat()
	}
	if p.fields.Kind != temporalInputCivil || p.mask != 0 {
		return temporalBadFormat()
	}
	switch text {
	case "now":
		p.fields.Kind = temporalInputNow
	case "today":
		p.fields.Kind = temporalInputToday
	case "yesterday":
		p.fields.Kind = temporalInputYesterday
	case "tomorrow":
		p.fields.Kind = temporalInputTomorrow
	case "epoch":
		p.fields.Kind = temporalInputEpoch
	case "-infinity":
		p.fields.Kind = temporalInputNegativeInfinity
	default:
		p.fields.Kind = temporalInputPositiveInfinity
	}
	p.mask = 7
	if p.fields.Kind == temporalInputNow || p.fields.Kind >= temporalInputEpoch {
		zoneConflict := !timeOnly && p.fields.HasZone
		if p.fields.HasTime || zoneConflict {
			return temporalBadFormat()
		}
		p.fields.HasTime = true
		if !timeOnly {
			p.fields.HasZone = true
		}
	}
	return nil
}

func (p *temporalDecoder) dateOrZoneToken(ctx context.Context, tokens []temporalToken, index int, options ResolveOptions, timeOnly bool) error {
	token := tokens[index]
	text := token.text
	if month := temporalMonth(text); month != 0 {
		return p.month(month)
	}
	datePosition := !timeOnly
	if index == 0 && len(tokens) >= 2 {
		datePosition = datePosition || tokens[len(tokens)-1].kind == 'd' || tokens[1].kind == 't'
	}
	if token.kind == 'd' && intervalInputDigit(text[0]) {
		if p.mask != 7 && datePosition {
			return p.date(text)
		}
		return p.timeZoneToken(text)
	}
	if token.kind == 'd' && temporalDateStartsMonth(text) {
		return p.date(text)
	}
	if p.fields.HasZone {
		return temporalBadFormat()
	}
	session, _, err := resolveTimeZoneContext(options.TimeZone)
	if err != nil {
		return err
	}
	if _, _, ok := timezoneLookup(text, session); !ok {
		if token.kind == 'd' {
			return sqlError("22023", "time zone not recognized")
		}
		return temporalBadFormat()
	}
	p.fields.Zone, p.fields.HasZone = text, true
	return nil
}

func (p *temporalDecoder) timeZoneToken(text string) error {
	base, zone, ok := strings.Cut(text, "-")
	if !ok {
		return temporalBadFormat()
	}
	if err := p.time(base); err != nil {
		return err
	}
	if p.fields.HasZone {
		return temporalBadFormat()
	}
	p.fields.Zone, p.fields.HasZone = "-"+zone, true
	if _, status := decodeNumericTimezone(p.fields.Zone); status != intervalInputOK {
		return sqlError("22009", "time zone displacement out of range")
	}
	return nil
}
