package txn

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type sequencerTestClock struct {
	mu      sync.Mutex
	reading clock.Reading
	changed chan struct{}
	entered chan clock.Instant
}

func newSequencerTestClock() *sequencerTestClock {
	return &sequencerTestClock{reading: clock.Reading{Earliest: 100, Latest: 110, Status: clock.Synced, Epoch: clock.Epoch{Assurance: [16]byte{1}, LeapHistory: [32]byte{2}, Config: [32]byte{3}}}, changed: make(chan struct{}), entered: make(chan clock.Instant, 4)}
}

func (provider *sequencerTestClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.reading, nil
}

func (provider *sequencerTestClock) advance(earliest, latest clock.Instant) {
	provider.mu.Lock()
	defer provider.mu.Unlock()
	provider.reading.Earliest, provider.reading.Latest = earliest, latest
	close(provider.changed)
	provider.changed = make(chan struct{})
}

func (provider *sequencerTestClock) WaitUntilAfter(ctx context.Context, target clock.Instant, epoch clock.Epoch) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case provider.entered <- target:
	}
	for {
		provider.mu.Lock()
		reading, changed := provider.reading, provider.changed
		provider.mu.Unlock()
		if reading.Epoch != epoch {
			return clock.ErrClockEpochChanged
		}
		if reading.Earliest > target {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func TestSequencerWaitsStrictlyPastRecoveredReservation(t *testing.T) {
	for _, test := range []struct {
		name      string
		recovered model.Timestamp
		target    clock.Instant
	}{
		{name: "current_interval", target: 110},
		{name: "recovered_frontier", recovered: 150, target: 151},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				provider := newSequencerTestClock()
				sequencer, err := newCommitSequencer(provider, test.recovered, provider.reading.Epoch.LeapHistory)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				finished := make(chan struct{})
				published := make(chan model.Timestamp, 1)
				var runErr error
				go func() {
					defer close(finished)
					runErr = sequencer.fence(ctx, 0, func(_ context.Context, timestamp model.Timestamp, _ clock.Epoch) error {
						published <- timestamp
						return nil
					})
				}()
				defer func() { cancel(); <-finished }()
				select {
				case target := <-provider.entered:
					if target != test.target {
						t.Fatalf("reserved timestamp: got %d, want %d", target, test.target)
					}
				case <-finished:
					t.Fatalf("sequencer ended before clock wait: %v", runErr)
				}
				provider.advance(test.target, test.target+10)
				synctest.Wait()
				select {
				case timestamp := <-published:
					t.Fatalf("published at earliest equality: %d", timestamp)
				default:
				}
				provider.advance(test.target+1, test.target+10)
				<-finished
				if runErr != nil {
					t.Fatal(runErr)
				}
				if timestamp := <-published; timestamp != model.Timestamp(test.target) {
					t.Fatalf("published timestamp: got %d, want %d", timestamp, test.target)
				}
			})
		})
	}
}

func TestSequencerCancellationDoesNotReuseReservation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider := newSequencerTestClock()
		sequencer, err := newCommitSequencer(provider, 0, provider.reading.Epoch.LeapHistory)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		firstDone := make(chan struct{})
		published := make(chan model.Timestamp, 2)
		publish := func(_ context.Context, timestamp model.Timestamp, _ clock.Epoch) error {
			published <- timestamp
			return nil
		}
		var firstErr error
		go func() { defer close(firstDone); firstErr = sequencer.fence(ctx, 0, publish) }()
		defer func() { cancel(); <-firstDone }()
		<-provider.entered
		cancel()
		<-firstDone
		if !errors.Is(firstErr, context.Canceled) {
			t.Fatalf("canceled wait: %v", firstErr)
		}
		secondCtx, secondCancel := context.WithCancel(t.Context())
		secondDone := make(chan struct{})
		var secondErr error
		go func() { defer close(secondDone); secondErr = sequencer.fence(secondCtx, 0, publish) }()
		defer func() { secondCancel(); <-secondDone }()
		if target := <-provider.entered; target != 111 {
			t.Fatalf("abandoned reservation reused: got %d, want 111", target)
		}
		provider.advance(112, 112)
		<-secondDone
		if secondErr != nil {
			t.Fatal(secondErr)
		}
		if timestamp := <-published; timestamp != 111 {
			t.Fatalf("published canceled reservation: %d", timestamp)
		}
		select {
		case timestamp := <-published:
			t.Fatalf("canceled attempt also published: %d", timestamp)
		default:
		}
	})
}

type changingEpochClock struct{ reading clock.Reading }

func (provider *changingEpochClock) Read(context.Context) (clock.Reading, error) {
	return provider.reading, nil
}
func (provider *changingEpochClock) WaitUntilAfter(_ context.Context, target clock.Instant, _ clock.Epoch) error {
	provider.reading.Earliest, provider.reading.Latest = target+1, target+1
	provider.reading.Epoch.Config[0]++
	return nil
}

func TestSequencerRejectsEpochChangeAfterWait(t *testing.T) {
	provider := &changingEpochClock{reading: newSequencerTestClock().reading}
	sequencer, err := newCommitSequencer(provider, 0, provider.reading.Epoch.LeapHistory)
	if err != nil {
		t.Fatal(err)
	}
	published := false
	err = sequencer.fence(t.Context(), 0, func(context.Context, model.Timestamp, clock.Epoch) error { published = true; return nil })
	if !errors.Is(err, clock.ErrClockEpochChanged) || published {
		t.Fatalf("changed epoch publication: error=%v published=%v", err, published)
	}
}
