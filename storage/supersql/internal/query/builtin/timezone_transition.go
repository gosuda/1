package builtin

import "context"

type timezoneType struct {
	offset int32
	dst    bool
	name   string
}

func timezoneUint32(at uint32) uint32 {
	return uint32(timezoneZoneBytes[at])<<24 | uint32(timezoneZoneBytes[at+1])<<16 | uint32(timezoneZoneBytes[at+2])<<8 | uint32(timezoneZoneBytes[at+3])
}

func (zone *timezoneZoneRecord) transition(index uint32) int64 {
	at := zone.times + index*uint32(zone.timeSize)
	if zone.timeSize == 4 {
		return int64(int32(timezoneUint32(at)))
	}
	return int64(uint64(timezoneUint32(at))<<32 | uint64(timezoneUint32(at+4)))
}

func (zone *timezoneZoneRecord) typeAt(index uint8) timezoneType {
	at := zone.types + uint32(index)*6
	start := zone.chars + uint32(timezoneZoneBytes[at+5])
	end := start
	for end < zone.charsEnd && timezoneZoneBytes[end] != 0 {
		end++
	}
	return timezoneType{offset: int32(timezoneUint32(at)), dst: timezoneZoneBytes[at+4] != 0, name: timezoneZoneBytes[start:end]}
}

func (zone *timezoneZoneRecord) cutoff(seconds int64) uint32 {
	lo, hi := uint32(0), zone.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		if seconds < zone.transition(mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return lo
}

func (zone namedTimeZone) at(seconds int64) timezoneType {
	if zone.record == nil {
		if zone.posix {
			return timezoneRuleAt(zone.rule, seconds)
		}
		return timezoneType{offset: zone.offset, name: zone.fixedName}
	}
	r := zone.record
	if r.hasFuture && (r.count == 0 || seconds > r.transition(r.count-1)) {
		return timezoneRuleAt(r.future, seconds)
	}
	cutoff := r.cutoff(seconds)
	if cutoff == 0 {
		return r.typeAt(r.defaultType)
	}
	return r.typeAt(timezoneZoneBytes[r.indices+cutoff-1])
}

func (zone namedTimeZone) next(seconds int64) (int64, timezoneType, bool) {
	if zone.record == nil {
		if zone.posix {
			return timezoneRuleNext(zone.rule, seconds)
		}
		return 0, timezoneType{}, false
	}
	r := zone.record
	cutoff := r.cutoff(seconds)
	if cutoff < r.count {
		return r.transition(cutoff), r.typeAt(timezoneZoneBytes[r.indices+cutoff]), true
	}
	if r.hasFuture {
		return timezoneRuleNext(r.future, seconds)
	}
	return 0, timezoneType{}, false
}

func (zone namedTimeZone) localOffset(seconds int64) int32 {
	before := zone.at(seconds - 86400)
	boundary, after, found := zone.next(seconds - 86400)
	if !found {
		return before.offset
	}
	beforeTime, afterTime := seconds-int64(before.offset), seconds-int64(after.offset)
	if beforeTime < boundary && afterTime < boundary {
		return before.offset
	}
	if beforeTime > boundary && afterTime >= boundary {
		return after.offset
	}
	// Gaps use the pre-transition offset; folds use the post-transition offset,
	// independently of DST labels (including negative DST and dateline jumps).
	if beforeTime > afterTime {
		return before.offset
	}
	return after.offset
}

func (zone namedTimeZone) abbreviationOffset(ctx context.Context, name string, seconds int64) (int32, bool, error) {
	if zone.record == nil {
		if zone.posix {
			current := zone.at(seconds)
			if timezoneEqual(name, current.name) {
				return current.offset, true, nil
			}
			if timezoneEqual(name, zone.rule.StandardName) {
				return zone.rule.StandardOffset, true, nil
			}
			if zone.rule.Daylight && timezoneEqual(name, zone.rule.DaylightName) {
				return zone.rule.DaylightOffset, true, nil
			}
		}
		return zone.offset, timezoneEqual(name, zone.fixedName), nil
	}
	r := zone.record
	if r.hasFuture && (r.count == 0 || seconds > r.transition(r.count-1)) {
		current := timezoneRuleAt(r.future, seconds)
		if timezoneEqual(name, current.name) {
			return current.offset, true, nil
		}
		if timezoneEqual(name, r.future.StandardName) {
			return r.future.StandardOffset, true, nil
		}
		if r.future.Daylight && timezoneEqual(name, r.future.DaylightName) {
			return r.future.DaylightOffset, true, nil
		}
	}
	cutoff := r.cutoff(seconds)
	for i := cutoff; i > 0; i-- {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		typ := r.typeAt(timezoneZoneBytes[r.indices+i-1])
		if timezoneEqual(name, typ.name) {
			return typ.offset, true, nil
		}
	}
	typ := r.typeAt(r.defaultType)
	if timezoneEqual(name, typ.name) {
		return typ.offset, true, nil
	}
	for i := cutoff; i < r.count; i++ {
		if i&63 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
		}
		typ := r.typeAt(timezoneZoneBytes[r.indices+i])
		if timezoneEqual(name, typ.name) {
			return typ.offset, true, nil
		}
	}
	if r.hasFuture {
		if timezoneEqual(name, r.future.StandardName) {
			return r.future.StandardOffset, true, nil
		}
		if r.future.Daylight && timezoneEqual(name, r.future.DaylightName) {
			return r.future.DaylightOffset, true, nil
		}
	}
	return 0, false, nil
}
