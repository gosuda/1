// Package convert holds the value conversions that the hqq-sqlite drivers
// share: the mapping of database/sql arguments to the five SQLite storage
// classes, the TEXT form of times, and the metadata derived from declared
// column types. Both drivers (gosuda.org/sqlite3 and the generated-client
// driver under it) use it, so they cannot diverge.
package convert

import "time"

// TimeLayout is the TEXT form of a time.Time argument. Times keep their own
// offset, so the text sorts correctly within one offset and round-trips to the
// same instant.
const TimeLayout = "2006-01-02 15:04:05.999999999-07:00"

// timeLayoutsWithZone are the TEXT forms carrying an explicit UTC offset.
var timeLayoutsWithZone = []string{
	TimeLayout,
	time.RFC3339Nano,
}

// timeLayoutsWithoutZone are the TEXT forms without a UTC offset.
var timeLayoutsWithoutZone = []string{
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
}

// ParseTime parses the TEXT of a DATE, DATETIME, or TIMESTAMP column. It reports
// false when no layout matches, and the caller keeps the text.
func ParseTime(s string) (time.Time, bool) {
	return ParseTimeInLocation(s, nil)
}

// ParseTimeInLocation parses the TEXT of a DATE, DATETIME, or TIMESTAMP column.
// When loc is non-nil, strings without an explicit UTC offset are interpreted
// in loc and parsed times are returned in loc.
func ParseTimeInLocation(s string, loc *time.Location) (time.Time, bool) {
	for _, layout := range timeLayoutsWithZone {
		if t, err := time.Parse(layout, s); err == nil {
			if loc != nil {
				t = t.In(loc)
			}
			return t, true
		}
	}
	zone := time.UTC
	if loc != nil {
		zone = loc
	}
	for _, layout := range timeLayoutsWithoutZone {
		if t, err := time.ParseInLocation(layout, s, zone); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

