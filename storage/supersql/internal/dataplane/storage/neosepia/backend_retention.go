package neosepia

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

var _ storage.RetentionBackend = (*Backend)(nil)

func (backend *Backend) RetentionState(ctx context.Context) (storage.RetentionState, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		return storage.RetentionState{}, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return storage.RetentionState{}, err
	}
	if backend.pending != nil || backend.metadataUncertain {
		return storage.RetentionState{}, storage.ErrCommitIndeterminate
	}
	set := backend.runs
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return storage.RetentionState{}, storage.ErrClosed
	}
	state := storage.RetentionState{
		HistoryFloor:  backend.state.HistoryFloor,
		ClosedTS:      backend.closedTimestamp(),
		LeapHistoryID: backend.state.Scales[len(backend.state.Scales)-1].Scale.LeapHistoryID,
	}
	for pin := set.pinHead; pin != nil; pin = pin.next {
		if err := ctx.Err(); err != nil {
			return storage.RetentionState{}, err
		}
		if !state.HasPin || pin.timestamp < state.OldestPin {
			state.OldestPin = pin.timestamp
			state.HasPin = true
		}
	}
	return state, ctx.Err()
}
