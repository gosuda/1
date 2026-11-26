package builtin

import (
	"cmp"
	"context"
	"strings"
	"unicode/utf8"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimezone(signature Signature, options ResolveOptions) (scalarKernel, uint64, error) {
	session, retained, err := retainTimeZoneContext(options.TimeZone)
	if err != nil {
		return nil, 0, err
	}
	switch signature.ID {
	case 3462, 3463:
		return compileMakeTimestamptz(signature, session), retained, nil
	case 1199:
		return compileTimestamptzAge(session), retained, nil
	case 1217, 1284:
		return compileTimestamptzTrunc(signature, session), retained, nil
	case 1171, 6203:
		return compileTimestamptzFields(signature, session), retained, nil
	case 1189, 1190, 2549, 6221, 6222, 6223, 6273:
		return compileTimezoneArithmetic(signature, session), retained, nil
	case 1151:
		return compileTimestamptzOutput(session, options), retained, nil
	case 1174:
		return compileDateTimezone(session), retained, nil
	case 1176:
		return compileDateTimeTimezone(session), retained, nil
	case 1178, 2019:
		return compileTimezoneProjection(signature, session), retained, nil
	case 2027, 2028, 6334, 6335:
		return compileSessionTimezone(signature, session), retained, nil
	}
	kernel := func(ctx context.Context, args []model.Value, _ CallBudget) (model.Value, error) {
		return convertNamedTimezone(ctx, args[1].IntVal, timezoneArgumentName(args[0].StrVal), signature.ID == 1159, session)
	}
	return kernel, retained, nil
}

func retainTimeZoneContext(capture TimeZoneContext) (namedTimeZone, uint64, error) {
	session, borrowedNames, err := resolveTimeZoneContext(capture)
	if err != nil {
		return namedTimeZone{}, 0, err
	}
	retained := uint64(unsafe.Sizeof(session))
	if borrowedNames {
		// Parse borrows names; clone only the retained semantic tokens.
		session.rule.StandardName = cloneTimezoneToken(session.rule.StandardName)
		session.rule.DaylightName = cloneTimezoneToken(session.rule.DaylightName)
		retained += uint64(len(session.rule.StandardName) + len(session.rule.DaylightName))
	}
	return session, retained, nil
}

func convertNamedTimezone(ctx context.Context, value int64, name string, toLocal bool, session namedTimeZone) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	if !finiteTimestamp(value) {
		return timestampValue(value), nil
	}
	zone, dynamic, ok := timezoneLookup(name, session)
	if !ok {
		return model.Value{}, sqlError("22023", "time zone not recognized")
	}
	seconds := timezoneFloor(value, intervalMicrosPerSecond) + timezoneUnixEpoch
	offset := zone.at(seconds).offset
	if !toLocal {
		offset = zone.localOffset(seconds)
	}
	if dynamic {
		probe := seconds
		if toLocal {
			// timestamptz_to_time_t truncates towards zero, unlike timestamp2tm.
			probe = value/intervalMicrosPerSecond + timezoneUnixEpoch
		} else {
			probe -= int64(offset)
		}
		abbreviation, found, err := zone.abbreviationOffset(ctx, name, probe)
		if err != nil {
			return model.Value{}, err
		}
		if found {
			offset = abbreviation
		} else if toLocal {
			// PostgreSQL re-resolves the local civil time on an unknown dynamic
			// abbreviation, which matters inside a backward transition.
			offset = zone.localOffset(seconds + int64(offset))
		}
	}
	adjustment := int64(offset) * intervalMicrosPerSecond
	if toLocal {
		adjustment = -adjustment
	}
	result, ok := subtractTimestamp(value, adjustment)
	if !ok || !validTimestamp(result) {
		return model.Value{}, sqlError("22008", "timestamp out of range")
	}
	return timestampValue(result), ctx.Err()
}

// resolveTimeZoneContext borrows semantic names from capture. The bool reports
// whether those names need cloning when retained by a callable.
func resolveTimeZoneContext(capture TimeZoneContext) (namedTimeZone, bool, error) {
	session, ok := timezoneFullName(cmp.Or(capture.Name, "UTC"))
	if !ok {
		return namedTimeZone{}, false, sqlError("22023", "invalid session time zone")
	}
	if !capture.Fixed && session.at(timezoneUnixEpoch).offset%60 != 0 {
		return namedTimeZone{}, false, sqlError("22023", "session time zone appears to use leap seconds")
	}
	borrowedNames := session.posix
	if capture.Fixed {
		if capture.OffsetSeconds < -604800 || capture.OffsetSeconds > 604800 {
			return namedTimeZone{}, false, sqlError("22023", "invalid session time zone offset")
		}
		// Preserve the canonical abbreviation, not the spelling of an alias.
		identity := session.rule.StandardName
		if !session.posix {
			identity = session.at(timezoneUnixEpoch).name
		}
		session = namedTimeZone{posix: true}
		session.rule.StandardName = identity
		session.rule.StandardOffset = capture.OffsetSeconds
	}
	return session, borrowedNames, nil
}

func cloneTimezoneToken(name string) string {
	var result strings.Builder
	result.Grow(len(name))
	for index := range len(name) {
		c := name[index]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		result.WriteByte(c)
	}
	return result.String()
}

func timezoneArgumentName(name string) string {
	// Match text_to_cstring_buffer's bounded, encoding-safe truncation.
	if len(name) > 255 {
		end := 255
		for end > 0 && !utf8.RuneStart(name[end]) {
			end--
		}
		name = name[:end]
	}
	if end := strings.IndexByte(name, 0); end >= 0 {
		name = name[:end]
	}
	return name
}
