package neosepia

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (pin *runPin) bindTimestamp(ctx context.Context, timestamp model.Timestamp) error {
	set := pin.owner
	set.mu.Lock()
	defer set.mu.Unlock()
	if pin.closed {
		return storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if timestamp <= 0 || (pin.timestamp != 0 && pin.timestamp != timestamp) {
		return storage.ErrSnapshotNotClosed
	}
	pin.timestamp = timestamp
	return nil
}

func (set *runSet) checkCollectionBoundary(ctx context.Context, boundary model.Timestamp) error {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return storage.ErrClosed
	}
	if boundary < 0 {
		return storage.ErrInvalidMutation
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for pin := set.pinHead; pin != nil; pin = pin.next {
		if err := ctx.Err(); err != nil {
			return err
		}
		if pin.timestamp < boundary {
			return storage.ErrSnapshotPinned
		}
	}
	return nil
}
