package storage

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// RetentionState is one lock-consistent view, not a reservation or a read fence.
// Collect must recheck pins and the closed frontier before accepting a boundary.
type RetentionState struct {
	HistoryFloor  model.Timestamp
	ClosedTS      model.Timestamp
	LeapHistoryID [32]byte
	// OldestPin includes snapshots, derived cursors, checkpoints and maintenance.
	// A present zero pin has no bound timestamp and prevents any advancement.
	OldestPin model.Timestamp
	HasPin    bool
}

// RetentionBackend is borrowed from the database's backend owner. Collection
// preserves catalog-reachable reads at the boundary and above; explicitly retired
// keyspaces require ObjectRetirement evidence. Errors may require commit
// resolution even when publication happened. RetentionState rejects indeterminate
// metadata.
type RetentionBackend interface {
	RetentionState(context.Context) (RetentionState, error)
	Collect(context.Context, GCRequest) error
}
