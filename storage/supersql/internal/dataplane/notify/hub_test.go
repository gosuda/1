package notify

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestHubListenPublishDedupAndWait(t *testing.T) {
	hub := NewHub(16)
	sub1 := hub.Listen(1, "Orders")
	sub2 := hub.SubscribeGo("orders", "alerts")
	defer sub1.Close()
	defer sub2.Close()

	// Duplicate (channel, payload) within one commit must be deduplicated.
	hub.Publish(42, []PendingNotification{
		{Channel: "ORDERS", Payload: "id=1"},
		{Channel: "orders", Payload: "id=1"},
		{Channel: "orders", Payload: "id=2"},
		{Channel: "alerts", Payload: "warn"},
	})

	got1 := sub1.Drain()
	if len(got1) != 2 || got1[0].Payload != "id=1" || got1[1].Payload != "id=2" || got1[0].SenderPID != 42 {
		t.Fatalf("sub1 notifications = %+v, want 2 deduped orders", got1)
	}
	got2 := sub2.Drain()
	if len(got2) != 3 || got2[2].Channel != "alerts" || got2[2].Payload != "warn" {
		t.Fatalf("sub2 notifications = %+v, want 3 notifications", got2)
	}

	// Test blocking Wait wakeup.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	go func() {
		time.Sleep(15 * time.Millisecond)
		hub.Publish(99, []PendingNotification{{Channel: "orders", Payload: "async"}})
	}()
	n, err := sub1.Wait(ctx)
	if err != nil || n.Payload != "async" || n.SenderPID != 99 {
		t.Fatalf("Wait = %+v, %v; want async from 99", n, err)
	}

	// Unlisten and verify no more deliveries.
	hub.Unlisten(1, "orders")
	if chs := hub.ListeningChannels(1); len(chs) != 0 {
		t.Fatalf("ListeningChannels after unlisten = %v, want empty", chs)
	}
	hub.Publish(99, []PendingNotification{{Channel: "orders", Payload: "after-unlisten"}})
	if len(sub1.Drain()) != 0 {
		t.Fatal("received notification after UNLISTEN")
	}
}

func TestHubConcurrentPublishAndBoundedQueue(t *testing.T) {
	hub := NewHub(8)
	sub := hub.SubscribeGo("events")
	defer sub.Close()

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := range 20 {
				hub.Publish(uint64(id), []PendingNotification{{Channel: "events", Payload: "msg"}})
				_ = j
			}
		}(i)
	}
	wg.Wait()

	drained := sub.Drain()
	if len(drained) != 8 {
		t.Fatalf("bounded queue len = %d, want 8", len(drained))
	}
}

func TestHubCloseUnblocksWaiters(t *testing.T) {
	hub := NewHub(16)
	sub1 := hub.SubscribeGo("events")
	sub2 := hub.Listen(42, "events")

	errCh := make(chan error, 2)
	go func() {
		_, err := sub1.Wait(context.Background())
		errCh <- err
	}()
	go func() {
		_, err := sub2.Wait(context.Background())
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	hub.Close()

	for range 2 {
		select {
		case err := <-errCh:
			if err == nil {
				t.Fatal("expected error from Wait after Hub.Close()")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Hub.Close() failed to unblock Wait")
		}
	}

	// Post-close SubscribeGo returns an already-closed subscription immediately.
	postSub := hub.SubscribeGo("events")
	if _, err := postSub.Wait(context.Background()); err == nil {
		t.Fatal("expected error from post-close SubscribeGo.Wait")
	}
}
