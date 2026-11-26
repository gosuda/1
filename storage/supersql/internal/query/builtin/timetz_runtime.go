package builtin

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func compileTimetzRuntime(signature Signature, options ResolveOptions, session namedTimeZone) scalarKernel {
	zonedAge := compileTimestamptzAge(session)
	midnightZone := compileDateTimezone(session)
	return func(ctx context.Context, args []model.Value, budget CallBudget) (model.Value, error) {
		if err := ctx.Err(); err != nil {
			return model.Value{}, err
		}
		switch signature.ID {
		case 1350:
			return timetzInput(ctx, args[0].StrVal, args[2].IntVal, options, session, budget)
		case 1388:
			value := args[0].IntVal
			if !finiteTimestamp(value) {
				return model.NullVal(model.TypeBytes), nil
			}
			if !validTimestamp(value) {
				return model.Value{}, sqlError("22008", "timestamp out of range")
			}
			_, micros, typ := session.localTimestampParts(value)
			return timetzResult(ctx, micros, -typ.offset, budget)
		case 2037, 6336:
			index := 0
			zone, dynamic, name := session, false, ""
			if signature.ID == 2037 {
				index = 1
				name = timezoneArgumentName(args[0].StrVal)
				var ok bool
				zone, dynamic, ok = timezoneLookup(name, session)
				if !ok {
					return model.Value{}, sqlError("22023", "time zone not recognized")
				}
			}
			micros, west, err := DecodeTimetz(args[index].BytesVal)
			if err != nil {
				return model.Value{}, err
			}
			offset := zone.offset
			if zone.record != nil || zone.posix || dynamic {
				now, err := temporalTransactionTimestamp(ctx, budget)
				if err != nil {
					return model.Value{}, err
				}
				seconds := timezoneFloor(now, intervalMicrosPerSecond) + timezoneUnixEpoch
				offset = zone.at(seconds).offset
				if dynamic {
					abbreviation, found, err := zone.abbreviationOffset(ctx, name, now/intervalMicrosPerSecond+timezoneUnixEpoch)
					if err != nil {
						return model.Value{}, err
					}
					if found {
						offset = abbreviation
					} else {
						offset = zone.localOffset(seconds + int64(offset))
					}
				}
			}
			return timetzAtOffset(ctx, micros, west, -offset, budget)
		case 2047:
			micros := args[0].IntVal
			if micros < 0 || micros > intervalMicrosPerDay {
				return model.Value{}, sqlError("22008", "time out of range")
			}
			now, err := temporalTransactionTimestamp(ctx, budget)
			if err != nil {
				return model.Value{}, err
			}
			days, _, _ := session.localTimestampParts(now)
			offset := session.localOffset(days*86400 + micros/intervalMicrosPerSecond + timezoneUnixEpoch)
			return timetzResult(ctx, micros, -offset, budget)
		case 1386, 2059:
			now, err := temporalTransactionTimestamp(ctx, budget)
			if err != nil {
				return model.Value{}, err
			}
			days, _, _ := session.localTimestampParts(now)
			if signature.ID == 1386 {
				dateArgs := [1]model.Value{dateValue(days)}
				midnight, err := midnightZone(ctx, dateArgs[:], budget)
				if err != nil {
					return model.Value{}, err
				}
				ageArgs := [2]model.Value{midnight, args[0]}
				return zonedAge(ctx, ageArgs[:], budget)
			}
			midnight, err := timestampFromDate(days)
			if err != nil {
				return model.Value{}, err
			}
			ageArgs := [2]model.Value{timestampValue(midnight), args[0]}
			return timestampAge(ctx, ageArgs[:], budget)
		}
		return model.Value{}, &MissingImplementationError{Signature: signature}
	}
}
