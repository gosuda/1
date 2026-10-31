package gstimeclock

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"gosuda.org/gstime"

	"github.com/gosuda/supersql-alpha/clock"
)

func ownHistory(history *gstime.LeapHistory) (*gstime.LeapHistory, error) {
	if history == nil {
		return nil, fmt.Errorf("%w: nil leap history", clock.ErrClockUnavailable)
	}
	owned, err := gstime.ParseLeapHistory(history.CanonicalBytes())
	if err != nil {
		return nil, errors.Join(clock.ErrClockUnavailable, err)
	}
	if owned.ID != history.ID || owned.InitialTaiMinusUtc != history.InitialTaiMinusUtc || !slices.Equal(owned.Entries, history.Entries) {
		return nil, errors.Join(clock.ErrClockEpochChanged, gstime.ErrLeapHistoryMismatch)
	}
	// The provider's civil conversion assumes midnight transitions and performs
	// unchecked second arithmetic. Bound the immutable table before using it.
	var cumulative int64
	for _, entry := range owned.Entries {
		second := entry.TransitionUnixSecond
		if second < -62135596800 || second >= 253402300800 || second%86400 != 0 {
			return nil, fmt.Errorf("%w: leap transition is outside civil range or not midnight", clock.ErrClockRange)
		}
		cumulative += int64(entry.Delta)
		if cumulative < -math.MaxInt32 || cumulative > math.MaxInt32 {
			return nil, clock.ErrClockRange
		}
	}
	return owned, nil
}

func (c *certifiedClock) FromUTC(value time.Time) (clock.Instant, error) {
	utc := value.UTC()
	if utc.Year() < 1 || utc.Year() > 9999 {
		return 0, clock.ErrClockRange
	}
	label := gstime.UtcLabel{
		Date:          gstime.GregorianDate{Year: int32(utc.Year()), Month: uint8(utc.Month()), Day: uint8(utc.Day())},
		SecondOfDay:   uint32(utc.Hour()*3600 + utc.Minute()*60 + utc.Second()),
		Nanos:         uint32(utc.Nanosecond()),
		LeapHistoryID: c.history.ID,
	}
	second := utc.Unix()
	var cumulative int64
	for _, entry := range c.history.Entries {
		if second >= entry.TransitionUnixSecond {
			cumulative += int64(entry.Delta)
		}
		if entry.Delta == -1 && second == entry.TransitionUnixSecond-1 {
			return 0, fmt.Errorf("%w: UTC label removed by a negative leap", clock.ErrClockRange)
		}
	}
	siSecond := second + cumulative
	if siSecond < 0 || siSecond > math.MaxInt64/1_000_000_000 || siSecond == math.MaxInt64/1_000_000_000 && int64(label.Nanos) > math.MaxInt64%1_000_000_000 {
		return 0, clock.ErrClockRange
	}
	// UnixNanosToGstInstant rejects the ordinary second before a positive leap;
	// an explicit civil label distinguishes it from the inserted second.
	instant, err := c.history.UtcToGstInstant(label)
	if err != nil {
		return 0, errors.Join(clock.ErrClockRange, err)
	}
	if instant < 0 || c.history.GstInstantToUtc(instant) != label {
		return 0, clock.ErrClockRange
	}
	return clock.Instant(instant), nil
}

func (c *certifiedClock) ToUTC(instant clock.Instant) (clock.UTCProjection, error) {
	if instant < 0 {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	label := c.history.GstInstantToUtc(gstime.GstInstant(instant))
	if label.LeapHistoryID != c.history.ID || label.Date.Year < 1 || label.Date.Year > 9999 || label.Date.Month < 1 || label.Date.Month > 12 || label.Date.Day < 1 || label.Date.Day > 31 || label.SecondOfDay > 86400 || label.Nanos >= 1_000_000_000 {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	leap := label.SecondOfDay == 86400
	second := min(label.SecondOfDay, 86399)
	utc := time.Date(int(label.Date.Year), time.Month(label.Date.Month), int(label.Date.Day), int(second/3600), int(second/60%60), int(second%60), int(label.Nanos), time.UTC)
	if utc.Year() != int(label.Date.Year) || utc.Month() != time.Month(label.Date.Month) || utc.Day() != int(label.Date.Day) {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	back, err := c.history.UtcToGstInstant(label)
	if err != nil {
		return clock.UTCProjection{}, errors.Join(clock.ErrClockRange, err)
	}
	if back != gstime.GstInstant(instant) {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	return clock.UTCProjection{Time: utc, LeapSecond: leap}, nil
}
