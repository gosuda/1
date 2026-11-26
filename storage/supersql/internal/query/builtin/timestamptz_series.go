package builtin

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type TimestamptzSeriesOptions struct {
	TimeZone     TimeZoneContext
	ExplicitZone *string
	// MemoryBytes bounds the complete retained state, including its header.
	MemoryBytes uint64
	// Allocator is borrowed through Close; nil enforces only MemoryBytes.
	Allocator columnar.Allocator
}

// TimestamptzSeries owns fixed-size calendar iteration state. It is single-owner
// and must not be copied after construction. Its zero value is exhausted.
type TimestamptzSeries struct {
	zone                namedTimeZone
	current, stop, next int64
	step                model.Interval
	claim               columnar.Allocation
	direction           int
	ready, closed       bool
}

var errTimestamptzSeriesAdvance = errors.New("timestamptz series Advance requires a successful Peek")

// NewTimestamptzSeries borrows input names only during construction.
func NewTimestamptzSeries(ctx context.Context, start, stop int64, step model.Interval, options TimestamptzSeriesOptions) (_ *TimestamptzSeries, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bytes := uint64(unsafe.Sizeof(TimestamptzSeries{}))
	if options.MemoryBytes < bytes {
		return nil, sqlError("54000", "timestamptz series work limit exceeded")
	}
	var claim columnar.Allocation
	if options.Allocator != nil {
		claim, err = options.Allocator.Reserve(bytes)
		if err != nil {
			return nil, err
		}
	}
	s := &TimestamptzSeries{current: start, stop: stop, step: step, claim: claim}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	s.zone, _, err = resolveTimeZoneContext(options.TimeZone)
	if err != nil {
		return nil, err
	}
	if options.ExplicitZone != nil {
		var found bool
		// Dynamic abbreviations select their full zone, not a sampled offset.
		s.zone, _, found = timezoneLookup(timezoneArgumentName(*options.ExplicitZone), s.zone)
		if !found {
			return nil, sqlError("22023", "time zone not recognized")
		}
	}
	// Arithmetic needs offsets and rules, never borrowed abbreviation names.
	s.zone.rule.StandardName = ""
	s.zone.rule.DaylightName = ""
	s.zone.fixedName = ""
	s.direction = compareInterval(step, model.Interval{})
	if s.direction == 0 {
		return nil, sqlError("22023", "step size cannot equal zero")
	}
	if intervalInfinity(step) != 0 {
		return nil, sqlError("22023", "step size cannot be infinite")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return s, nil
}

// Peek prepares the next calendar step before exposing the current value, even
// on the final row. Repeated calls replay that value without advancing.
// Exhaustion, cancellation and arithmetic errors close the state.
func (s *TimestamptzSeries) Peek(ctx context.Context) (value model.Value, ok bool, err error) {
	if s == nil || s.closed {
		return model.Value{}, false, nil
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, s.Close())
		}
	}()
	if err = ctx.Err(); err != nil {
		return model.Value{}, false, err
	}
	if s.ready {
		return timestampValue(s.current), true, nil
	}
	pastUpper := s.direction > 0 && s.current > s.stop
	pastLower := s.direction < 0 && s.current < s.stop
	if s.direction == 0 || pastUpper || pastLower {
		return model.Value{}, false, s.Close()
	}
	next, err := s.zone.addTimestampInterval(s.current, s.step)
	if err != nil {
		return model.Value{}, false, err
	}
	if err = ctx.Err(); err != nil {
		return model.Value{}, false, err
	}
	s.next = next.IntVal
	s.ready = true
	return timestampValue(s.current), true, nil
}

// Advance commits a successful Peek without allocation or repeated arithmetic.
func (s *TimestamptzSeries) Advance() error {
	if s == nil || s.closed || !s.ready {
		return errTimestamptzSeriesAdvance
	}
	s.current = s.next
	s.next = 0
	s.ready = false
	return nil
}

func (s *TimestamptzSeries) Close() error {
	if s == nil || s.closed {
		return nil
	}
	claim := s.claim
	*s = TimestamptzSeries{closed: true}
	if claim != nil {
		claim.Release()
	}
	return nil
}
