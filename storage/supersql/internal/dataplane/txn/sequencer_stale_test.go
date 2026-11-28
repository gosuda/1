package txn

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Models a provider acknowledging a stale watermark rather than fresh bounds.
type prematureWaitClock struct{ reading clock.Reading }

func (provider *prematureWaitClock) Read(context.Context) (clock.Reading, error) {
	return provider.reading, nil
}
func (provider *prematureWaitClock) WaitUntilAfter(_ context.Context, target clock.Instant, _ clock.Epoch) error {
	provider.reading.Earliest = target
	provider.reading.Latest = target + 10
	return nil
}

func TestSequencerRejectsStaleWaitAcknowledgment(t *testing.T) {
	provider := &prematureWaitClock{reading: newSequencerTestClock().reading}
	sequencer, err := newCommitSequencer(provider, 0, provider.reading.Epoch.LeapHistory)
	if err != nil {
		t.Fatal(err)
	}
	published := false
	err = sequencer.fence(t.Context(), 0, func(context.Context, model.Timestamp, clock.Epoch) error { published = true; return nil })
	if !errors.Is(err, clock.ErrClockUnavailable) || published {
		t.Fatalf("stale clock proof: error=%v published=%v", err, published)
	}
}
