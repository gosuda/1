// Package gstimeclock adapts gstime's certified SI intervals; it does not qualify raw backends.
package gstimeclock

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gosuda.org/gstime"

	"github.com/gosuda/supersql-alpha/clock"
)

type certifiedClock struct {
	service        *gstime.ClockService
	history        *gstime.LeapHistory
	mu             sync.Mutex
	epoch          clock.Epoch // guarded by mu
	lifetime       context.Context
	maxWidth       int64
	maxAge         int64
	maxHoldoverAge int64
}

// New borrows a service, which may still be unanchored. Its caller owns workers,
// raw-clock qualification and invalidation, and must keep the service history immutable.
func New(service *gstime.ClockService, history *gstime.LeapHistory) (clock.Clock, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: nil service", clock.ErrClockUnavailable)
	}
	owned, err := ownHistory(history)
	if err != nil {
		return nil, err
	}
	c := &certifiedClock{service: service, history: owned}
	// Construction happens before the clock is published, so the initial epoch is
	// captured without the lock.
	sample := service.Now()
	c.epoch = sampleEpoch(sample)
	if err := c.validateIdentity(sample); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *certifiedClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := c.contextError(ctx); err != nil {
		return clock.Reading{}, err
	}
	sample := c.service.Now()
	if err := c.contextError(ctx); err != nil {
		return clock.Reading{}, err
	}
	if err := c.adoptEpoch(sample); err != nil {
		return clock.Reading{}, err
	}
	return c.validate(sample)
}

func (c *certifiedClock) contextError(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if c.lifetime != nil {
		if err := context.Cause(c.lifetime); err != nil {
			return errors.Join(clock.ErrClockUnavailable, err)
		}
	}
	return nil
}

func sampleEpoch(sample gstime.AssuredNow) clock.Epoch {
	return clock.Epoch{Assurance: sample.AssuranceEpochID, LeapHistory: sample.LeapHistoryID, Config: sample.ConfigID}
}

// adoptEpoch re-pins the certified epoch to the service's current assurance
// epoch. gstime mints a new assurance epoch whenever it leaves the synced state
// (Invalidate, expired anchor, identity change), so a re-acquired clock is read
// under its new epoch; fences issued under the old epoch fail in WaitUntilAfter.
func (c *certifiedClock) adoptEpoch(sample gstime.AssuredNow) error {
	if sample.LeapHistoryID != c.history.ID {
		return errors.Join(clock.ErrClockEpochChanged, gstime.ErrLeapHistoryMismatch)
	}
	if sample.AssuranceEpochID == ([16]byte{}) || sample.ConfigID == ([32]byte{}) {
		return errors.Join(clock.ErrClockUnavailable, gstime.ErrPublicationUnavailable)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if sample.ConfigID != c.epoch.Config {
		return errors.Join(clock.ErrClockEpochChanged, gstime.ErrConfigurationMismatch)
	}
	c.epoch.Assurance = sample.AssuranceEpochID
	return nil
}

func (c *certifiedClock) currentEpoch() clock.Epoch {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

func (c *certifiedClock) validateIdentity(sample gstime.AssuredNow) error {
	if sample.LeapHistoryID != c.history.ID {
		return errors.Join(clock.ErrClockEpochChanged, gstime.ErrLeapHistoryMismatch)
	}
	if sampleEpoch(sample) != c.currentEpoch() {
		return errors.Join(clock.ErrClockEpochChanged, gstime.ErrConfigurationMismatch)
	}
	if sample.AssuranceEpochID == ([16]byte{}) || sample.ConfigID == ([32]byte{}) {
		return errors.Join(clock.ErrClockUnavailable, gstime.ErrPublicationUnavailable)
	}
	return nil
}

func (c *certifiedClock) validate(sample gstime.AssuredNow) (clock.Reading, error) {
	if err := c.validateIdentity(sample); err != nil {
		return clock.Reading{}, err
	}
	if sample.Status != gstime.StatusSynced && sample.Status != gstime.StatusHoldover || !sample.HasInterval {
		return clock.Reading{}, sampleError(sample)
	}
	interval := sample.Interval
	if interval.Earliest < 0 || interval.Latest < interval.Earliest || sample.Age < 0 {
		return clock.Reading{}, errors.Join(clock.ErrClockRange, gstime.ErrArithmeticFault)
	}
	if c.maxWidth > 0 && int64(interval.Latest-interval.Earliest) >= c.maxWidth {
		return clock.Reading{}, errors.Join(clock.ErrClockUnavailable, gstime.ErrBoundExpired)
	}
	status := clock.Synced
	maxAge := c.maxAge
	if sample.Status == gstime.StatusHoldover {
		status = clock.Holdover
		maxAge = c.maxHoldoverAge
	}
	if maxAge > 0 && int64(sample.Age) > maxAge {
		return clock.Reading{}, errors.Join(clock.ErrClockUnavailable, gstime.ErrBoundExpired)
	}
	return clock.Reading{Earliest: clock.Instant(interval.Earliest), Latest: clock.Instant(interval.Latest), Epoch: sampleEpoch(sample), Status: status}, nil
}

// Now exposes reasons rather than its discarded propagation error. Preserve the
// provider API category without pretending to recover an error it did not return.
func sampleError(sample gstime.AssuredNow) error {
	category := clock.ErrClockUnavailable
	cause := gstime.ErrDesynchronized
	switch sample.Reason {
	case gstime.ReasonLeapHistoryMismatch:
		category, cause = clock.ErrClockEpochChanged, gstime.ErrLeapHistoryMismatch
	case gstime.ReasonConfigurationChanged:
		category, cause = clock.ErrClockEpochChanged, gstime.ErrConfigurationMismatch
	case gstime.ReasonArithmeticOverflow:
		category, cause = clock.ErrClockRange, gstime.ErrArithmeticFault
	case gstime.ReasonRawDiscontinuity, gstime.ReasonRawScaleInvalid:
		cause = gstime.ErrRawFault
	case gstime.ReasonRawBoundExpired, gstime.ReasonBoundTooOld, gstime.ReasonBoundTooWide:
		cause = gstime.ErrBoundExpired
	default:
		if sample.Status == gstime.StatusUnanchored {
			cause = gstime.ErrUnanchored
		} else if sample.Status == gstime.StatusSynced || sample.Status == gstime.StatusHoldover {
			cause = gstime.ErrPublicationUnavailable
		}
	}
	return fmt.Errorf("%w: %w (status %s, reason %s)", category, cause, sample.Status, sample.Reason)
}

func (c *certifiedClock) WaitUntilAfter(ctx context.Context, target clock.Instant, epoch clock.Epoch) error {
	if target < 0 {
		return clock.ErrClockRange
	}
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		reading, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if reading.Epoch != epoch {
			return clock.ErrClockEpochChanged
		}
		if reading.Earliest > target {
			return nil
		}
		delay := time.Duration(target - reading.Earliest)
		delay = max(100*time.Microsecond, min(delay, 5*time.Millisecond))
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		var stopped <-chan struct{}
		if c.lifetime != nil {
			stopped = c.lifetime.Done()
		}
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-stopped:
			return errors.Join(clock.ErrClockUnavailable, context.Cause(c.lifetime))
		case <-timer.C:
		}
	}
}

func (c *certifiedClock) Scale() clock.ScaleDescriptor {
	return clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: c.history.ID, CanonicalLeapHistory: c.history.CanonicalBytes()}
}
