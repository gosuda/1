package txn

import (
	"context"
	"errors"
	"math"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestExactHistoricalFenceWaitsPastRequestedInstant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider := newSequencerTestClock()
		sequencer, err := newCommitSequencer(provider, 90, provider.reading.Epoch.LeapHistory)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		published := make(chan model.Timestamp, 1)
		go func() {
			done <- sequencer.closeAt(ctx, 105, func(_ context.Context, timestamp model.Timestamp, _ clock.Epoch) error {
				published <- timestamp
				return nil
			})
		}()
		defer cancel()
		if target := <-provider.entered; target != 105 {
			t.Errorf("wait target = %d, want exact 105", target)
		}
		provider.advance(105, 110)
		synctest.Wait()
		select {
		case timestamp := <-published:
			t.Errorf("equality authorized historical fence %d", timestamp)
		default:
		}
		provider.advance(106, 110)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if timestamp := <-published; timestamp != 105 {
			t.Fatalf("closed %d, want exact 105", timestamp)
		}
		// Subsequent writes cannot receive the historical barrier's timestamp.
		if err := sequencer.fence(ctx, 0, func(_ context.Context, timestamp model.Timestamp, _ clock.Epoch) error {
			published <- timestamp
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if timestamp := <-published; timestamp != 110 {
			t.Fatalf("next reservation = %d, want 110", timestamp)
		}
	})
}

func TestExactHistoricalFenceRejectsUncertifiableMaximum(t *testing.T) {
	provider := newSequencerTestClock()
	provider.reading.Earliest, provider.reading.Latest = math.MaxInt64, math.MaxInt64
	sequencer, err := newCommitSequencer(provider, 90, provider.reading.Epoch.LeapHistory)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = sequencer.closeAt(t.Context(), model.Timestamp(math.MaxInt64), func(context.Context, model.Timestamp, clock.Epoch) error { called = true; return nil })
	if !errors.Is(err, clock.ErrClockRange) || called {
		t.Fatalf("uncertifiable maximum: published=%v err=%v", called, err)
	}
}
