package builtin

import "github.com/gosuda/supersql-alpha/internal/query/builtin/internal/tzrule"

// TimeZoneContext captures session timezone semantics. An empty Name selects UTC.
// Fixed offsets are seconds east of UTC; the abbreviation inventory is Default.
type TimeZoneContext struct {
	Name          string
	Fixed         bool
	OffsetSeconds int32
}

// Offsets address the generated immutable TZif byte string, not allocated slices.
type timezoneZoneRecord struct {
	name                                   string
	times, indices, types, chars, charsEnd uint32
	count                                  uint32
	typeCount                              uint16
	defaultType                            uint8
	timeSize                               uint8
	future                                 tzrule.Rule
	hasFuture                              bool
}

type timezoneAbbreviationRecord struct {
	name   string
	offset int32
	zone   int32 // -1 for a fixed offset; otherwise an index into timezoneZones.
	dst    bool
}
