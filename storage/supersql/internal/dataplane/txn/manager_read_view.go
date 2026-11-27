package txn

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// ErrNoClosedFrontier reports a manager that has not published any closed
// timestamp yet, so no durable state exists to read without certifying one.
var ErrNoClosedFrontier = errors.New("no closed frontier has been published")

// CatalogReadIdentity identifies the exact catalog rows one read view exposes.
// Derived catalog metadata, such as the declared-type runtime, can be reused
// across statements while two views report equal identities, because equal
// identities imply identical catalog content.
type CatalogReadIdentity struct {
	// Live reports a snapshot taken at or above the published frontier. Every
	// published commit is visible to it, so Version identifies its content.
	Live bool
	// Fixed is the exact timestamp of a snapshot taken at or below the
	// frontier. Its visible rows are a pure function of that timestamp, which
	// never changes for an already published instant.
	Fixed model.Timestamp
	// Version counts published commits that rewrote catalog rows, as of the
	// snapshot's creation. It is meaningful only for a live snapshot.
	Version uint64
	// Overlay is the content version of the in-transaction catalog overlay the
	// view reads; zero when the view has no overlay. Overlay versions restart at
	// zero in every transaction, so OverlayOwner names the transaction that staged
	// them: equal identities then imply the same staged rows, never two
	// transactions that happen to stage the same number of changes.
	Overlay      uint64
	OverlayOwner model.TxnID
	// SnapshotOverlay is the snapshot's own overlay generation: it advances on
	// every AddSessionOverlay call. A statement that adds its newly declared
	// descriptors to a borrowed snapshot changes the rows that snapshot exposes
	// without publishing a commit, so the generation must key any cache derived
	// from those rows.
	SnapshotOverlay uint64
}

// openedCatalogSnapshot is one opened reader together with the identity of the
// catalog content it exposes.
type openedCatalogSnapshot struct {
	reader    storage.SnapshotReader
	timestamp model.Timestamp
	identity  CatalogReadIdentity
}

type managerReadView struct {
	storage.SnapshotReader
	timestamp model.Timestamp
	identity  CatalogReadIdentity
}

func (view *managerReadView) Timestamp() model.Timestamp { return view.timestamp }

// CatalogIdentity names the catalog content this view exposes. It is captured
// with the snapshot, never re-derived from a later frontier.
func (view *managerReadView) CatalogIdentity() CatalogReadIdentity { return view.identity }

// CurrentReadView owns a certified current snapshot with its exact selected
// timestamp. The caller closes it; Manager and its Clock remain borrowed.
func (manager *Manager) CurrentReadView(ctx context.Context) (ReadView, error) {
	opened, err := manager.openStrongSnapshot(ctx, nil, 0)
	if err != nil {
		return nil, err
	}
	return &managerReadView{SnapshotReader: opened.reader, timestamp: opened.timestamp, identity: opened.identity}, nil
}

// ClosedReadView owns a snapshot at the already published closed frontier. It
// never certifies a new instant, so it never advances the durable read fence;
// pollers that rescan durable state use it instead of CurrentReadView.
//
// state.ClosedTS and catalogVersion are captured together under manager.mu,
// where publishResolved advances both atomically on commit. Every published
// commit at or before state.ClosedTS is therefore reflected in catalogVersion,
// and any later or in-flight commit receives a strictly larger timestamp from
// the sequencer. Reporting Live: true with Version: catalogVersion preserves
// catalog-derived caches (such as prepared statement plans) across non-catalog
// DML commits while invalidating immediately on any DDL commit.
func (manager *Manager) ClosedReadView(ctx context.Context) (ReadView, error) {
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		return nil, storage.ErrClosed
	}
	closed := manager.frontier.ClosedTS
	manager.mu.Unlock()
	if closed == 0 {
		return nil, ErrNoClosedFrontier
	}
	operation, finish, err := manager.operation(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	manager.mu.Lock()
	state := manager.frontier
	catalogVersion := manager.catalogVersion
	manager.mu.Unlock()
	if state.ClosedTS == 0 {
		return nil, ErrNoClosedFrontier
	}
	if state.ClosedTS < state.HistoryFloor {
		return nil, storage.ErrSnapshotTooOld
	}
	fence, err := manager.authority.Issue(operation, state.ClosedTS, state.LogPosition, state.Epoch)
	if err != nil {
		return nil, err
	}
	reader, err := manager.backend.OpenSnapshot(operation, storage.SnapshotSpec{Timestamp: state.ClosedTS, Fence: fence})
	if err != nil {
		return nil, err
	}
	return &managerReadView{
		SnapshotReader: reader,
		timestamp:      state.ClosedTS,
		identity:       CatalogReadIdentity{Live: true, Version: catalogVersion},
	}, nil
}
