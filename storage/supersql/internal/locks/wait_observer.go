package locks

import (
	"context"
	"time"
)

// LockWaitObserver receives the elapsed time of each acquisition that blocked.
type LockWaitObserver interface {
	ObserveLockWait(time.Duration)
}

type lockWaitObserverKey struct{}

func WithLockWaitObserver(ctx context.Context, observer LockWaitObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, lockWaitObserverKey{}, observer)
}

// ReportLockWait reports after a lock manager has released its internal mutex.
func ReportLockWait(ctx context.Context, elapsed time.Duration) {
	if ctx == nil || elapsed < 0 {
		return
	}
	if observer, ok := ctx.Value(lockWaitObserverKey{}).(LockWaitObserver); ok {
		observer.ObserveLockWait(elapsed)
	}
}
