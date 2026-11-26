package txn

import (
	"context"
	"math"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type sequencingClock interface {
	Read(context.Context) (clock.Reading, error)
	WaitUntilAfter(context.Context, clock.Instant, clock.Epoch) error
}

// The gate stays held through durable submission and publication. Reservations
// survive failed waits, so a later attempt cannot reuse an abandoned timestamp.
type commitSequencer struct {
	provider sequencingClock
	history  [32]byte
	reserved model.Timestamp
	anchored bool
	gate     chan struct{}
}

func newCommitSequencer(provider sequencingClock, recovered model.Timestamp, history [32]byte) (*commitSequencer, error) {
	if provider == nil {
		return nil, clock.ErrClockUnavailable
	}
	if recovered < 0 {
		return nil, model.ErrTimestampRange
	}
	if history == ([32]byte{}) {
		return nil, clock.ErrClockEpochChanged
	}
	result := &commitSequencer{provider: provider, history: history, reserved: recovered, gate: make(chan struct{}, 1)}
	result.gate <- struct{}{}
	return result, nil
}

// closeAt closes an exact historical instant without reserving a newer snapshot.
// Zero is reserved for a new strong barrier, never an exact historical request.
func (sequencer *commitSequencer) closeAt(ctx context.Context, timestamp model.Timestamp, publish func(context.Context, model.Timestamp, clock.Epoch) error) error {
	if timestamp <= 0 {
		return model.ErrTimestampRange
	}
	return sequencer.fence(ctx, timestamp, publish)
}

func (sequencer *commitSequencer) fence(ctx context.Context, exact model.Timestamp, publish func(context.Context, model.Timestamp, clock.Epoch) error) error {
	if err := sequencer.lock(ctx); err != nil {
		return err
	}
	defer sequencer.unlock()
	return sequencer.fenceLocked(ctx, exact, publish)
}

func (sequencer *commitSequencer) lock(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-sequencer.gate:
		return nil
	}
}

func (sequencer *commitSequencer) unlock() { sequencer.gate <- struct{}{} }

func (sequencer *commitSequencer) fenceLocked(ctx context.Context, exact model.Timestamp, publish func(context.Context, model.Timestamp, clock.Epoch) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if exact == 0 && sequencer.reserved == model.Timestamp(math.MaxInt64) {
		return model.ErrTimestampRange
	}
	reading, err := sequencer.provider.Read(ctx)
	if err != nil {
		return err
	}
	if err := validateCertifiedReading(reading, sequencer.history); err != nil {
		return err
	}
	timestamp := exact
	if exact == 0 {
		timestamp = max(model.Timestamp(reading.Latest), sequencer.reserved+1)
	} else if exact > model.Timestamp(reading.Latest) {
		return model.ErrTimestampRange
	}
	if timestamp == model.Timestamp(math.MaxInt64) {
		return clock.ErrClockRange
	}
	sequencer.reserved = max(sequencer.reserved, timestamp)
	if !sequencer.anchored {
		if err := sequencer.provider.WaitUntilAfter(ctx, clock.Instant(timestamp), reading.Epoch); err != nil {
			return err
		}
		if err := sequencer.checkFence(ctx, timestamp, reading.Epoch); err != nil {
			return err
		}
		sequencer.anchored = true
	}
	return publish(ctx, timestamp, reading.Epoch)
}

func (sequencer *commitSequencer) checkFence(ctx context.Context, timestamp model.Timestamp, epoch clock.Epoch) error {
	fresh, err := sequencer.provider.Read(ctx)
	if err != nil {
		return err
	}
	if err := validateCertifiedReading(fresh, sequencer.history); err != nil {
		return err
	}
	if fresh.Epoch != epoch {
		return clock.ErrClockEpochChanged
	}
	if !sequencer.anchored && fresh.Earliest <= clock.Instant(timestamp) {
		return clock.ErrClockUnavailable
	}
	return ctx.Err()
}

func validateCertifiedReading(reading clock.Reading, history [32]byte) error {
	if reading.Earliest < 0 || reading.Latest < reading.Earliest {
		return clock.ErrClockRange
	}
	if reading.Status != clock.Synced && reading.Status != clock.Holdover {
		return clock.ErrClockUnavailable
	}
	if reading.Epoch.Assurance == ([16]byte{}) || reading.Epoch.Config == ([32]byte{}) {
		return clock.ErrClockUnavailable
	}
	if reading.Epoch.LeapHistory != history {
		return clock.ErrClockEpochChanged
	}
	return nil
}
