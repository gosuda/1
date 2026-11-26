package supersql

import (
	"context"
	"sync"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/notify"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

// Notification represents a committed PostgreSQL LISTEN/NOTIFY event.
type Notification struct {
	Channel   string
	Payload   string
	PID       uint64
	Timestamp time.Time
}

func fromInternalNotification(n notify.Notification) Notification {
	return Notification{
		Channel:   n.Channel,
		Payload:   n.Payload,
		PID:       n.SenderPID,
		Timestamp: n.Timestamp,
	}
}

// Subscription delivers committed notifications for one or more channels
// directly to in-process Go callers. Call Close when done to release resources.
type Subscription struct {
	sub       *notify.Subscription
	ch        chan Notification
	cancel    context.CancelFunc
	done      chan struct{}
	closeOnce sync.Once
}

// C returns a receive-only channel of committed notifications. The channel
// closes after Close is called.
func (s *Subscription) C() <-chan Notification {
	if s == nil {
		return nil
	}
	return s.ch
}

// Wait blocks until the next notification arrives, ctx is canceled, or the
// subscription is closed.
func (s *Subscription) Wait(ctx context.Context) (Notification, error) {
	if s == nil || s.ch == nil {
		return Notification{}, context.Canceled
	}
	select {
	case n, ok := <-s.ch:
		if !ok {
			return Notification{}, context.Canceled
		}
		return n, nil
	case <-ctx.Done():
		return Notification{}, ctx.Err()
	}
}

// Close unsubscribes from all channels and stops background delivery.
func (s *Subscription) Close() {
	if s == nil {
		return
	}
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.sub != nil {
			s.sub.Close()
		}
		if s.done != nil {
			<-s.done
		}
	})
}

// Subscribe registers an in-process Go listener on one or more PostgreSQL
// LISTEN/NOTIFY channels. Channel names are normalized case-insensitively like
// unquoted SQL identifiers.
func (database *Catalog) Subscribe(channels ...string) *Subscription {
	if database == nil || database.hub == nil || database.checkOpen() != nil {
		closedCh := make(chan Notification)
		close(closedCh)
		return &Subscription{ch: closedCh}
	}
	parent := database.workerCtx
	if parent == nil {
		parent = context.Background()
	}
	if parent.Err() != nil {
		closedCh := make(chan Notification)
		close(closedCh)
		return &Subscription{ch: closedCh}
	}
	sub := database.hub.SubscribeGo(channels...)
	ctx, cancel := context.WithCancel(parent)
	out := &Subscription{
		sub:    sub,
		ch:     make(chan Notification, 64),
		cancel: cancel,
		done:   make(chan struct{}),
	}
	go func() {
		defer close(out.done)
		defer close(out.ch)
		defer sub.Close()
		defer cancel()
		for {
			if ctx.Err() != nil {
				return
			}
			n, err := sub.Wait(ctx)
			if err != nil {
				return
			}
			select {
			case out.ch <- fromInternalNotification(n):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out
}

// Notify publishes a notification directly to all listeners on channel.
// It validates PostgreSQL channel (<= 63 bytes, non-empty) and payload
// (<= 8000 bytes) limits.
func (database *Catalog) Notify(channel, payload string) error {
	if err := database.checkOpen(); err != nil {
		return err
	}
	ch, err := builtin.ValidateNotifyChannel(channel)
	if err != nil {
		return wrapError(err)
	}
	if err := builtin.ValidateNotifyPayload(payload); err != nil {
		return wrapError(err)
	}
	if database.hub == nil {
		return ErrDatabaseClosed
	}
	database.hub.Publish(1, []notify.PendingNotification{{Channel: ch, Payload: payload}})
	return nil
}

// SessionID returns this connection's internal session identifier.
func (c *connection) SessionID() uint64 {
	if c == nil || c.scope == nil {
		return 0
	}
	return c.scope.id
}

// SetSessionUser binds the protected session_user identity on this connection
// so RESET ROLE, RESET ALL, and PL/pgSQL DO blocks cannot escalate back to the
// built-in superuser.
func (c *connection) SetSessionUser(ctx context.Context, role string) error {
	if err := c.check(); err != nil {
		return err
	}
	if c.scope == nil || c.scope.settings == nil {
		return ErrDatabaseClosed
	}
	return wrapError(c.scope.settings.SetSessionUser(ctx, role, false))
}

// WaitForNotification blocks until a committed notification arrives on any
// channel this connection is listening on, ctx is canceled, or the connection
// closes.
func (c *connection) WaitForNotification(ctx context.Context) (Notification, error) {
	if err := c.check(); err != nil {
		return Notification{}, err
	}
	if c.database == nil || c.database.hub == nil || c.scope == nil {
		return Notification{}, ErrDatabaseClosed
	}
	sub := c.database.hub.EnsureSession(c.scope.id)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	goDone := make(chan struct{})
	go func() {
		select {
		case <-c.done:
			cancel()
		case <-goDone:
		}
	}()
	defer close(goDone)
	n, err := sub.Wait(waitCtx)
	if err != nil {
		if ctx.Err() != nil {
			return Notification{}, ctx.Err()
		}
		if c.closed.Load() {
			return Notification{}, ErrDatabaseClosed
		}
		return Notification{}, err
	}
	return fromInternalNotification(n), nil
}

// DrainNotifications removes and returns any notifications currently queued
// for this connection without blocking.
func (c *connection) DrainNotifications() []Notification {
	if c == nil || c.closed.Load() || c.database == nil || c.database.hub == nil || c.scope == nil {
		return nil
	}
	raw := c.database.hub.EnsureSession(c.scope.id).Drain()
	if len(raw) == 0 {
		return nil
	}
	out := make([]Notification, len(raw))
	for i, n := range raw {
		out[i] = fromInternalNotification(n)
	}
	return out
}
