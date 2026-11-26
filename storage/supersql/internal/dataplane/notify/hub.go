package notify

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultQueueCapacity = 4096

// Notification represents one delivered PostgreSQL LISTEN/NOTIFY event.
type Notification struct {
	Channel   string
	Payload   string
	SenderPID uint64
	Timestamp time.Time
}

// PendingNotification is staged inside a transaction and delivered only on COMMIT.
type PendingNotification struct {
	Channel string
	Payload string
}

// Subscription receives committed notifications for one session or Go listener.
type Subscription struct {
	hub       *Hub
	sessionID uint64
	mu        sync.Mutex
	channels  map[string]struct{}
	queue     []Notification
	maxQueue  int
	signal    chan struct{}
	closed    bool
}

// Hub manages in-process LISTEN/NOTIFY subscriptions and transactional fanout.
type Hub struct {
	mu          sync.RWMutex
	subs        map[uint64]*Subscription
	byChannel   map[string]map[uint64]*Subscription
	nextGoSubID uint64
	maxQueue    int
	closed      bool
}

// NewHub creates a notification hub with bounded per-subscriber queues.
func NewHub(maxQueue int) *Hub {
	if maxQueue <= 0 {
		maxQueue = defaultQueueCapacity
	}
	return &Hub{
		subs:        make(map[uint64]*Subscription),
		byChannel:   make(map[string]map[uint64]*Subscription),
		nextGoSubID: 1 << 62, // separate from SQL session IDs (which start at 1)
		maxQueue:    maxQueue,
	}
}

func normalizeChannel(ch string) string {
	return strings.ToLower(strings.TrimSpace(ch))
}

func newClosedSubscription(h *Hub, sessionID uint64) *Subscription {
	sig := make(chan struct{})
	close(sig)
	return &Subscription{
		hub:       h,
		sessionID: sessionID,
		channels:  make(map[string]struct{}),
		signal:    sig,
		closed:    true,
	}
}

// EnsureSession returns or creates the subscription handle for a SQL session.
func (h *Hub) EnsureSession(sessionID uint64) *Subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return newClosedSubscription(h, sessionID)
	}
	if sub, ok := h.subs[sessionID]; ok && !sub.closed {
		return sub
	}
	sub := &Subscription{
		hub:       h,
		sessionID: sessionID,
		channels:  make(map[string]struct{}),
		maxQueue:  h.maxQueue,
		signal:    make(chan struct{}, 1),
	}
	h.subs[sessionID] = sub
	return sub
}

// SubscribeGo registers a standalone Go listener on one or more channels.
func (h *Hub) SubscribeGo(channels ...string) *Subscription {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return newClosedSubscription(h, 0)
	}
	h.nextGoSubID++
	id := h.nextGoSubID
	sub := &Subscription{
		hub:       h,
		sessionID: id,
		channels:  make(map[string]struct{}),
		maxQueue:  h.maxQueue,
		signal:    make(chan struct{}, 1),
	}
	h.subs[id] = sub
	for _, raw := range channels {
		ch := normalizeChannel(raw)
		if ch == "" {
			continue
		}
		sub.channels[ch] = struct{}{}
		if h.byChannel[ch] == nil {
			h.byChannel[ch] = make(map[uint64]*Subscription)
		}
		h.byChannel[ch][id] = sub
	}
	h.mu.Unlock()
	return sub
}

// Listen subscribes sessionID to channel.
func (h *Hub) Listen(sessionID uint64, channel string) *Subscription {
	ch := normalizeChannel(channel)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return newClosedSubscription(h, sessionID)
	}
	sub, ok := h.subs[sessionID]
	if !ok || sub.closed {
		sub = &Subscription{
			hub:       h,
			sessionID: sessionID,
			channels:  make(map[string]struct{}),
			maxQueue:  h.maxQueue,
			signal:    make(chan struct{}, 1),
		}
		h.subs[sessionID] = sub
	}
	if ch != "" {
		sub.mu.Lock()
		sub.channels[ch] = struct{}{}
		sub.mu.Unlock()
		if h.byChannel[ch] == nil {
			h.byChannel[ch] = make(map[uint64]*Subscription)
		}
		h.byChannel[ch][sessionID] = sub
	}
	return sub
}

// Unlisten removes channel from sessionID's subscriptions. If channel is "" or "*",
// every channel for sessionID is removed.
func (h *Hub) Unlisten(sessionID uint64, channel string) {
	ch := normalizeChannel(channel)
	h.mu.Lock()
	defer h.mu.Unlock()
	sub, ok := h.subs[sessionID]
	if !ok {
		return
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if ch == "" || ch == "*" {
		for existing := range sub.channels {
			if listeners := h.byChannel[existing]; listeners != nil {
				delete(listeners, sessionID)
				if len(listeners) == 0 {
					delete(h.byChannel, existing)
				}
			}
		}
		clear(sub.channels)
		return
	}
	delete(sub.channels, ch)
	if listeners := h.byChannel[ch]; listeners != nil {
		delete(listeners, sessionID)
		if len(listeners) == 0 {
			delete(h.byChannel, ch)
		}
	}
}

// CloseSession unsubscribes and closes sessionID's subscription handle.
func (h *Hub) CloseSession(sessionID uint64) {
	h.mu.Lock()
	sub, ok := h.subs[sessionID]
	if !ok {
		h.mu.Unlock()
		return
	}
	delete(h.subs, sessionID)
	sub.mu.Lock()
	for ch := range sub.channels {
		if listeners := h.byChannel[ch]; listeners != nil {
			delete(listeners, sessionID)
			if len(listeners) == 0 {
				delete(h.byChannel, ch)
			}
		}
	}
	clear(sub.channels)
	alreadyClosed := sub.closed
	sub.closed = true
	sub.queue = nil
	sub.mu.Unlock()
	h.mu.Unlock()
	if !alreadyClosed {
		close(sub.signal)
	}
}

// Close closes the hub and every active subscription, unblocking all waiters.
// Subsequent SubscribeGo, Listen, and EnsureSession calls return closed handles.
func (h *Hub) Close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	toClose := make([]*Subscription, 0, len(h.subs))
	for _, sub := range h.subs {
		toClose = append(toClose, sub)
	}
	clear(h.subs)
	clear(h.byChannel)
	h.mu.Unlock()

	for _, sub := range toClose {
		sub.mu.Lock()
		clear(sub.channels)
		sub.queue = nil
		alreadyClosed := sub.closed
		sub.closed = true
		sub.mu.Unlock()
		if !alreadyClosed {
			close(sub.signal)
		}
	}
}

// ListeningChannels returns the sorted channel names sessionID currently listens on.
func (h *Hub) ListeningChannels(sessionID uint64) []string {
	h.mu.RLock()
	sub, ok := h.subs[sessionID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	sub.mu.Lock()
	defer sub.mu.Unlock()
	if len(sub.channels) == 0 {
		return nil
	}
	out := make([]string, 0, len(sub.channels))
	for ch := range sub.channels {
		out = append(out, ch)
	}
	slices.Sort(out)
	return out
}

// Publish atomically deduplicates pending notifications from one committed
// transaction (preserving order of first occurrence per PostgreSQL rules) and
// delivers them to every matching subscriber without blocking.
func (h *Hub) Publish(senderID uint64, pending []PendingNotification) {
	if h == nil || len(pending) == 0 {
		return
	}
	type dedupKey struct{ ch, payload string }
	seen := make(map[dedupKey]struct{}, len(pending))
	now := time.Now().UTC()
	deduped := make([]Notification, 0, len(pending))
	for _, p := range pending {
		ch := normalizeChannel(p.Channel)
		if ch == "" {
			continue
		}
		k := dedupKey{ch: ch, payload: p.Payload}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		deduped = append(deduped, Notification{
			Channel:   ch,
			Payload:   p.Payload,
			SenderPID: senderID,
			Timestamp: now,
		})
	}
	if len(deduped) == 0 {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.closed {
		return
	}
	for _, n := range deduped {
		listeners := h.byChannel[n.Channel]
		for _, sub := range listeners {
			sub.enqueue(n)
		}
	}
}

func (s *Subscription) enqueue(n Notification) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if len(s.queue) >= s.maxQueue {
		// Drop oldest to stay strictly bounded and never block committers.
		copy(s.queue, s.queue[1:])
		s.queue[len(s.queue)-1] = n
	} else {
		s.queue = append(s.queue, n)
	}
	s.mu.Unlock()
	select {
	case s.signal <- struct{}{}:
	default:
	}
}

// Signal returns a channel that receives a wakeup when notifications are queued
// (or closes when the subscription closes).
func (s *Subscription) Signal() <-chan struct{} {
	return s.signal
}

// Drain removes and returns all queued notifications in FIFO order.
func (s *Subscription) Drain() []Notification {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return nil
	}
	out := make([]Notification, len(s.queue))
	copy(out, s.queue)
	s.queue = s.queue[:0]
	return out
}

// Pop removes and returns the next queued notification, if any.
func (s *Subscription) Pop() (Notification, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return Notification{}, false
	}
	n := s.queue[0]
	copy(s.queue, s.queue[1:])
	s.queue = s.queue[:len(s.queue)-1]
	return n, true
}

// Wait blocks until the next notification arrives, ctx is canceled, or the
// subscription is closed.
func (s *Subscription) Wait(ctx context.Context) (Notification, error) {
	for {
		if n, ok := s.Pop(); ok {
			return n, nil
		}
		select {
		case <-ctx.Done():
			return Notification{}, ctx.Err()
		case _, ok := <-s.signal:
			if !ok {
				if n, has := s.Pop(); has {
					return n, nil
				}
				return Notification{}, context.Canceled
			}
		}
	}
}

// Close unsubscribes and closes this subscription.
func (s *Subscription) Close() {
	if s == nil || s.hub == nil {
		return
	}
	s.hub.CloseSession(s.sessionID)
}
