package noderuntime

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"gosuda.org/ivnp/foundation"
)

func TestRequestDestinationPublicationDebounces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &Controller{
			ctx:                    t.Context(),
			destinationPublishWake: make(chan *destinationRuntime, 4),
			publishDebounce:        50 * time.Millisecond,
		}
		runtime := &destinationRuntime{}
		d.requestDestinationPublication(runtime)
		synctest.Wait()
		select {
		case queued := <-d.destinationPublishWake:
			queued.publishQueued.Store(destinationMaintenanceIdle)
			if queued != runtime {
				t.Fatalf("leading publish wake = %v, want destination runtime", queued)
			}
		default:
			t.Fatal("leading publish wake was not enqueued immediately")
		}

		d.requestDestinationPublication(runtime)
		d.requestDestinationPublication(runtime)
		<-time.After(d.publishDebounce - time.Millisecond)
		synctest.Wait()
		select {
		case <-d.destinationPublishWake:
			t.Fatal("trailing publish wake fired before debounce window elapsed")
		default:
		}

		<-time.After(time.Millisecond)
		synctest.Wait()
		select {
		case queued := <-d.destinationPublishWake:
			queued.publishQueued.Store(destinationMaintenanceIdle)
			if queued != runtime {
				t.Fatalf("trailing publish wake = %v, want destination runtime", queued)
			}
		default:
			t.Fatal("trailing publish wake did not fire at debounce expiry")
		}

		<-time.After(4 * d.publishDebounce)
		synctest.Wait()
		select {
		case <-d.destinationPublishWake:
			t.Fatal("publication wake storm: uncoalesced event inside debounce window")
		default:
		}
	})
}

type stubLeaseSetPublisher struct {
	confirmed atomic.Bool
}

func (s *stubLeaseSetPublisher) Maintain(context.Context) (int, error) { return 0, nil }
func (s *stubLeaseSetPublisher) HandleDeliveryStatus(foundation.I2NPDeliveryStatusMessage) bool {
	return false
}
func (s *stubLeaseSetPublisher) Confirmed() bool                { return s.confirmed.Load() }
func (s *stubLeaseSetPublisher) NeedsInitialConfirmation() bool { return !s.confirmed.Load() }
func (s *stubLeaseSetPublisher) Close()                         {}

func TestRequestDestinationPublicationSkipsDebounceBeforeConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &Controller{
			ctx:                    t.Context(),
			destinationPublishWake: make(chan *destinationRuntime, 4),
			publishDebounce:        50 * time.Millisecond,
		}
		stub := &stubLeaseSetPublisher{}
		runtime := &destinationRuntime{publisher: &destinationPublisher{publisher: stub}}

		// Before initial confirmation, consecutive wakes must bypass debounce.
		d.requestDestinationPublication(runtime)
		queued := <-d.destinationPublishWake
		queued.publishQueued.Store(destinationMaintenanceIdle)

		d.requestDestinationPublication(runtime)
		select {
		case queued = <-d.destinationPublishWake:
			queued.publishQueued.Store(destinationMaintenanceIdle)
		default:
			t.Fatal("unconfirmed publication wake was debounced during cold start")
		}

		// Once confirmed, normal debounce coalesces rapid wakes.
		stub.confirmed.Store(true)
		d.requestDestinationPublication(runtime)
		queued = <-d.destinationPublishWake
		queued.publishQueued.Store(destinationMaintenanceIdle)

		d.requestDestinationPublication(runtime)
		<-time.After(d.publishDebounce - time.Millisecond)
		synctest.Wait()
		select {
		case <-d.destinationPublishWake:
			t.Fatal("confirmed publication wake fired before debounce window elapsed")
		default:
		}
		<-time.After(time.Millisecond)
		synctest.Wait()
		select {
		case queued = <-d.destinationPublishWake:
			queued.publishQueued.Store(destinationMaintenanceIdle)
		default:
			t.Fatal("confirmed trailing publication wake did not fire at debounce expiry")
		}
	})
}
