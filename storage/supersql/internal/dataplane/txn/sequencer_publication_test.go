package txn

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSequencerHoldsGateThroughPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		provider := newSequencerTestClock()
		sequencer, err := newCommitSequencer(provider, 0, provider.reading.Epoch.LeapHistory)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		publishing := make(chan struct{})
		release := make(chan struct{})
		releasePublication := sync.OnceFunc(func() { close(release) })
		firstDone := make(chan struct{})
		var secondDone chan struct{}
		var firstErr, secondErr error
		published := make(chan model.Timestamp, 2)
		go func() {
			defer close(firstDone)
			firstErr = sequencer.fence(ctx, 0, func(_ context.Context, timestamp model.Timestamp, _ clock.Epoch) error {
				close(publishing)
				<-release
				published <- timestamp
				return nil
			})
		}()
		defer func() {
			cancel()
			releasePublication()
			<-firstDone
			if secondDone != nil {
				<-secondDone
			}
		}()
		<-provider.entered
		provider.advance(111, 111)
		select {
		case <-publishing:
		case <-firstDone:
			t.Fatalf("first publication did not begin: %v", firstErr)
		}
		secondDone = make(chan struct{})
		go func() {
			defer close(secondDone)
			secondErr = sequencer.fence(ctx, 0, func(_ context.Context, timestamp model.Timestamp, _ clock.Epoch) error {
				published <- timestamp
				return nil
			})
		}()
		synctest.Wait()
		select {
		case target := <-provider.entered:
			t.Fatalf("reserved %d while earlier publication was unfinished", target)
		default:
		}
		releasePublication()
		<-firstDone
		if firstErr != nil {
			t.Fatal(firstErr)
		}
		<-secondDone
		if secondErr != nil {
			t.Fatal(secondErr)
		}
		if first, second := <-published, <-published; first != 110 || second != 111 {
			t.Fatalf("publication order: got %d,%d, want 110,111", first, second)
		}
	})
}
