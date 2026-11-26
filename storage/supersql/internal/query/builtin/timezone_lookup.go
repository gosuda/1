package builtin

import (
	"strings"

	"github.com/gosuda/supersql-alpha/internal/query/builtin/internal/tzrule"
)

type namedTimeZone struct {
	record    *timezoneZoneRecord
	rule      tzrule.Rule
	posix     bool
	offset    int32
	fixedName string
}

func timezoneLower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

func timezoneCompare(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := timezoneLower(a[i]), timezoneLower(b[i])
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return 0
}

func timezoneEqual(a, b string) bool { return timezoneCompare(a, b) == 0 }

func timezoneFullName(name string) (namedTimeZone, bool) {
	if len(name) == 0 || len(name) > 255 || strings.IndexByte(name, 0) >= 0 {
		return namedTimeZone{}, false
	}
	if timezoneEqual(name, "GMT") {
		return namedTimeZone{fixedName: "GMT"}, true
	}
	lookup := name
	if lookup[0] == ':' {
		lookup = lookup[1:]
	}
	lo, hi := 0, len(timezoneZones)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if timezoneCompare(timezoneZones[mid].name, lookup) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(timezoneZones) && timezoneEqual(timezoneZones[lo].name, lookup) {
		return namedTimeZone{record: &timezoneZones[lo]}, true
	}
	if name[0] == ':' {
		return namedTimeZone{}, false
	}
	rule, err := tzrule.Parse(name)
	if err != nil {
		return namedTimeZone{}, false
	}
	return namedTimeZone{rule: timezoneNormalizeRule(rule), posix: true}, true
}

func (zone namedTimeZone) knownAbbreviation(name string) (int32, bool, bool) {
	var first timezoneType
	known, fixed := false, true
	accept := func(typ timezoneType) {
		if !timezoneEqual(name, typ.name) {
			return
		}
		if !known {
			first = typ
			known = true
		} else if first.offset != typ.offset || first.dst != typ.dst {
			fixed = false
		}
	}
	if zone.record != nil {
		for i := uint16(0); i < zone.record.typeCount; i++ {
			accept(zone.record.typeAt(uint8(i)))
		}
		if zone.record.hasFuture {
			rule := timezoneNormalizeRule(zone.record.future)
			accept(timezoneType{offset: rule.StandardOffset, name: rule.StandardName})
			if rule.Daylight {
				accept(timezoneType{offset: rule.DaylightOffset, dst: true, name: rule.DaylightName})
			}
		}
	} else if zone.posix {
		accept(timezoneType{offset: zone.rule.StandardOffset, name: zone.rule.StandardName})
		if zone.rule.Daylight {
			accept(timezoneType{offset: zone.rule.DaylightOffset, dst: true, name: zone.rule.DaylightName})
		}
	} else if zone.fixedName != "" {
		accept(timezoneType{offset: zone.offset, name: zone.fixedName})
	}
	return first.offset, known, fixed
}

func timezoneLookup(name string, session namedTimeZone) (namedTimeZone, bool, bool) {
	// DecodeTimezoneName lowercases and truncates identifiers before abbreviation lookup.
	token := name
	if len(token) > 63 {
		token = token[:63]
	}
	if offset, known, fixed := session.knownAbbreviation(token); known {
		if fixed {
			return namedTimeZone{offset: offset}, false, true
		}
		return session, true, true
	}
	if len(token) > 10 {
		token = token[:10]
	}
	lo, hi := 0, len(timezoneDefaultAbbreviations)
	for lo < hi {
		mid := lo + (hi-lo)/2
		if timezoneCompare(timezoneDefaultAbbreviations[mid].name, token) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < len(timezoneDefaultAbbreviations) && timezoneEqual(timezoneDefaultAbbreviations[lo].name, token) {
		entry := timezoneDefaultAbbreviations[lo]
		if entry.zone < 0 {
			return namedTimeZone{offset: entry.offset}, false, true
		}
		return namedTimeZone{record: &timezoneZones[entry.zone]}, true, true
	}
	zone, ok := timezoneFullName(name)
	return zone, false, ok
}
