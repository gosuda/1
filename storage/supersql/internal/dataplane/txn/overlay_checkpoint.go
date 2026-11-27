package txn

import (
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

var errOverlayCheckpoint = errors.New("overlay checkpoint belongs to another transaction")

type overlayCheckpoint struct {
	owner   *Overlay
	state   overlayState
	ordinal uint64
}

func (overlay *Overlay) checkpoint() (overlayCheckpoint, error) {
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	if overlay.closed {
		return overlayCheckpoint{}, storage.ErrClosed
	}
	return overlayCheckpoint{owner: overlay, state: overlay.state, ordinal: overlay.lastOrdinal}, nil
}

// The transaction validates savepoint branch membership through its mutation log
// before restoring this root. Frozen scan views retain their original roots.
func (overlay *Overlay) restore(checkpoint overlayCheckpoint) error {
	overlay.mu.Lock()
	defer overlay.mu.Unlock()
	if overlay.closed {
		return storage.ErrClosed
	}
	if checkpoint.owner != overlay {
		return errOverlayCheckpoint
	}
	// Ordinals only advance while staging, so an unchanged ordinal means the
	// checkpoint still describes the staged state and the restore cannot change
	// the catalog rows a frozen view exposes.
	if overlay.lastOrdinal != checkpoint.ordinal {
		overlay.catalogVersion++
	}
	overlay.state, overlay.lastOrdinal = checkpoint.state, checkpoint.ordinal
	// Discarded nodes can still belong to frozen views; retain the conservative
	// allocation charge until the overlay closes rather than undercount live data.
	return nil
}
