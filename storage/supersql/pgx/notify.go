package pgx

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha"
)

// Notification represents a PostgreSQL LISTEN/NOTIFY message delivered to a Conn.
type Notification struct {
	PID     uint32
	Channel string
	Payload string
}

type notificationWaiter interface {
	WaitForNotification(ctx context.Context) (supersql.Notification, error)
}

// WaitForNotification blocks until a notification arrives on any channel this
// connection is listening on (via LISTEN), ctx is canceled, or the connection
// closes.
func (c *Conn) WaitForNotification(ctx context.Context) (*Notification, error) {
	if c == nil || c.sqlConn == nil {
		return nil, errors.New("pgx: connection is closed")
	}
	var got supersql.Notification
	err := c.sqlConn.Raw(func(driverConn any) error {
		if hc, ok := driverConn.(*hookConn); ok {
			driverConn = hc.inner
		}
		waiter, ok := driverConn.(notificationWaiter)
		if !ok {
			return errors.New("pgx: underlying connection does not support notifications")
		}
		var waitErr error
		got, waitErr = waiter.WaitForNotification(ctx)
		return waitErr
	})
	if err != nil {
		return nil, wrapPgError(err)
	}
	return &Notification{
		PID:     uint32(got.PID),
		Channel: got.Channel,
		Payload: got.Payload,
	}, nil
}

// WaitForNotification blocks until a notification arrives on this acquired
// pool connection, ctx is canceled, or the connection is released/closed.
func (pc *PoolConn) WaitForNotification(ctx context.Context) (*Notification, error) {
	if pc == nil || pc.conn == nil {
		return nil, errors.New("pgx: pool connection is closed")
	}
	return pc.conn.WaitForNotification(ctx)
}
