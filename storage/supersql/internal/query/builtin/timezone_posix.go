package builtin

import "github.com/gosuda/supersql-alpha/internal/query/builtin/internal/tzrule"

const timezoneUnixEpoch = int64(946684800)

func timezoneFloor(value, divisor int64) int64 {
	q := value / divisor
	if value%divisor < 0 {
		q--
	}
	return q
}

func timezoneRuleTransition(year int64, transition tzrule.Transition, before int32) int64 {
	first := dateFieldJulianDay(year, 1, 1)
	day := int64(transition.Day)
	switch transition.Kind {
	case tzrule.JulianNoLeap:
		day--
		leap := year%4 == 0 && (year%100 != 0 || year%400 == 0)
		if day >= 59 && leap {
			day++
		}
	case tzrule.MonthWeekDay:
		month := int64(transition.Month)
		start := dateFieldJulianDay(year, month, 1)
		weekday := (start + 1) % 7
		if weekday < 0 {
			weekday += 7
		}
		day = (int64(transition.Weekday)-weekday+7)%7 + 7*(int64(transition.Week)-1)
		next := dateFieldJulianDay(year, month+1, 1)
		if month == 12 {
			next = dateFieldJulianDay(year+1, 1, 1)
		}
		if start+day >= next {
			day -= 7
		}
		day += start - first
	}
	return (first-dateEpochJulian+day)*86400 + timezoneUnixEpoch + int64(transition.Seconds) - int64(before)
}

func timezoneRuleYear(seconds int64) int64 {
	year, _, _ := dateCalendar(timezoneFloor(seconds-timezoneUnixEpoch, 86400))
	return year
}

func timezoneRuleEvents(rule tzrule.Rule, year int64) (int64, int64, bool) {
	start := timezoneRuleTransition(year, rule.Start, rule.StandardOffset)
	end := timezoneRuleTransition(year, rule.End, rule.DaylightOffset)
	yearSeconds := (dateFieldJulianDay(year+1, 1, 1) - dateFieldJulianDay(year, 1, 1)) * 86400
	return start, end, end < start || start < end && end-start < yearSeconds+int64(rule.DaylightOffset)-int64(rule.StandardOffset)
}

func timezoneNormalizeRule(rule tzrule.Rule) tzrule.Rule {
	if !rule.Daylight {
		return rule
	}
	// tzparse discards the standard type when no year emits transitions.
	for year := int64(2000); year < 2400; year++ {
		if _, _, emitted := timezoneRuleEvents(rule, year); emitted {
			return rule
		}
	}
	return tzrule.Rule{StandardName: rule.DaylightName, StandardOffset: rule.DaylightOffset}
}

// Adjacent years cover signed transition times that cross a year boundary.
func timezoneRuleAt(rule tzrule.Rule, seconds int64) timezoneType {
	result := timezoneType{offset: rule.StandardOffset, name: rule.StandardName}
	if !rule.Daylight {
		return result
	}
	result = timezoneType{offset: rule.DaylightOffset, dst: true, name: rule.DaylightName}
	year := timezoneRuleYear(seconds)
	latest := int64(-1 << 63)
	for y := year + 1; y >= year-400; y-- {
		start, end, emitted := timezoneRuleEvents(rule, y)
		if emitted {
			if start <= seconds && start > latest {
				latest = start
				result = timezoneType{offset: rule.DaylightOffset, dst: true, name: rule.DaylightName}
			}
			if end <= seconds && end > latest {
				latest = end
				result = timezoneType{offset: rule.StandardOffset, name: rule.StandardName}
			}
		}
		if y < year && latest != int64(-1<<63) {
			break
		}
	}
	return result
}

func timezoneRuleNext(rule tzrule.Rule, seconds int64) (int64, timezoneType, bool) {
	if !rule.Daylight {
		return 0, timezoneType{}, false
	}
	year := timezoneRuleYear(seconds)
	next := int64(1<<63 - 1)
	var after timezoneType
	for y := year - 1; y <= year+400; y++ {
		start, end, emitted := timezoneRuleEvents(rule, y)
		if emitted {
			if start > seconds && start < next {
				next = start
				after = timezoneType{offset: rule.DaylightOffset, dst: true, name: rule.DaylightName}
			}
			if end > seconds && end < next {
				next = end
				after = timezoneType{offset: rule.StandardOffset, name: rule.StandardName}
			}
		}
		if y > year && next != int64(1<<63-1) {
			break
		}
	}
	return next, after, next != int64(1<<63-1)
}
