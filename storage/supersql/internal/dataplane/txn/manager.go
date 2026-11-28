package txn

import (
	"context"
	"errors"
	"sync"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var ErrManagerConfig = errors.New("invalid transaction manager configuration")
var ErrManagerCapacity = &model.Error{Code: "53300", Message: "transaction manager operation limit exceeded"}

// RecoveredState comes from validated backend recovery, not a live-clock estimate.
// LeapHistory is the installed mapping; Epoch can refer to an earlier compatible
// mapping when the last durable barrier predates a future-history installation.
type RecoveredState struct {
	LogPosition                      uint64
	CommitTS, ClosedTS, HistoryFloor model.Timestamp
	Epoch                            clock.Epoch
	LeapHistory                      [32]byte
}

type ManagerConfig struct {
	Backend            storage.Backend
	Clock              clock.Clock
	Authority          *FenceAuthority
	Recovered          RecoveredState
	HistoricalReadOnly bool
	MaxOperations      int
	MaxTransactions    int
	MaxLocks           int
	LockBytes          uint64
	WaitGraph          *locks.WaitGraph
}

// Manager borrows Backend and Clock. Close cancels and joins manager operations;
// the composition owner closes the backend afterward. Returned readers own pins.
type Manager struct {
	mu        sync.Mutex
	backend   storage.Backend
	authority *FenceAuthority
	sequencer *commitSequencer
	frontier        RecoveredState
	pending         *managerCommit
	pendingInFlight bool
	// catalogVersion counts published commits that rewrote catalog rows. It is
	// monotonic for the life of the manager and is read under manager.mu
	// together with the snapshot frontier it identifies.
	catalogVersion        uint64
	historicalOnly        bool
	closed                bool
	active, maximum       int
	life                  context.Context
	cancel                context.CancelFunc
	operations            sync.WaitGroup
	locks                 *lockRegistry
	transactions          map[model.TxnID]*Transaction
	maxTransactions       int
	transactionCleanups   sync.WaitGroup
	closeDone             chan struct{}
	closeErr              error
	transactionCloseError error
}

func NewManager(ctx context.Context, config ManagerConfig) (*Manager, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.Backend == nil || config.Authority == nil || config.MaxOperations <= 0 {
		return nil, ErrManagerConfig
	}
	state := config.Recovered
	if state.CommitTS < 0 || state.ClosedTS < state.CommitTS || state.HistoryFloor < 0 || state.HistoryFloor > state.ClosedTS {
		return nil, ErrManagerConfig
	}
	if (state.LogPosition == 0) != (state.CommitTS == 0) || state.LeapHistory == ([32]byte{}) {
		return nil, ErrManagerConfig
	}
	if state.ClosedTS > 0 {
		if state.Epoch.Assurance == ([16]byte{}) || state.Epoch.Config == ([32]byte{}) || state.Epoch.LeapHistory == ([32]byte{}) {
			return nil, ErrManagerConfig
		}
	}
	if config.MaxTransactions == 0 {
		config.MaxTransactions = config.MaxOperations
	}
	if config.MaxLocks == 0 {
		config.MaxLocks = 1 << 20
	}
	if config.LockBytes == 0 {
		config.LockBytes = 64 << 20
	}
	lockRegistry, err := newLockRegistry(config.MaxTransactions, config.MaxLocks, config.LockBytes, config.WaitGraph)
	if err != nil {
		return nil, errors.Join(ErrManagerConfig, err)
	}
	manager := &Manager{backend: config.Backend, authority: config.Authority, frontier: state, historicalOnly: config.HistoricalReadOnly, maximum: config.MaxOperations, locks: lockRegistry, maxTransactions: config.MaxTransactions, transactions: make(map[model.TxnID]*Transaction), closeDone: make(chan struct{})}
	manager.life, manager.cancel = context.WithCancel(context.Background())
	if config.HistoricalReadOnly {
		return manager, nil
	}
	if config.Clock == nil {
		return nil, errors.Join(clock.ErrClockUnavailable, manager.Close())
	}
	scale := config.Clock.Scale()
	if err := clock.ValidateScale(ctx, scale, 16<<20); err != nil {
		return nil, errors.Join(err, manager.Close())
	}
	if scale.LeapHistoryID != state.LeapHistory {
		return nil, errors.Join(clock.ErrClockEpochChanged, manager.Close())
	}
	sequencer, err := newCommitSequencer(config.Clock, state.ClosedTS, state.LeapHistory)
	if err != nil {
		return nil, errors.Join(err, manager.Close())
	}
	manager.sequencer = sequencer
	reader, err := manager.StrongSnapshot(ctx)
	if err != nil {
		return nil, errors.Join(err, manager.Close())
	}
	if err := reader.Close(); err != nil {
		return nil, errors.Join(err, manager.Close())
	}
	return manager, nil
}

func (manager *Manager) operation(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.closed {
		return nil, nil, storage.ErrClosed
	}
	if manager.active == manager.maximum {
		return nil, nil, ErrManagerCapacity
	}
	manager.active++
	manager.operations.Add(1)
	operation, cancel := context.WithCancel(ctx)
	var cancellation sync.WaitGroup
	cancellation.Add(1)
	stop := context.AfterFunc(manager.life, func() { defer cancellation.Done(); cancel() })
	return operation, func() {
		if stop() {
			cancellation.Done()
		}
		cancellation.Wait()
		cancel()
		manager.mu.Lock()
		manager.active--
		manager.mu.Unlock()
		manager.operations.Done()
	}, nil
}

func (manager *Manager) StrongSnapshot(ctx context.Context) (storage.SnapshotReader, error) {
	opened, err := manager.openStrongSnapshot(ctx, nil, 0)
	return opened.reader, err
}

// CheckpointSpec issues a certified fence for an external Checkpointer (backup
// orchestration). Timestamp and Fence.ClosedTS are equal: the certified capture
// instant closed above the durable frontier. Historical-only managers cannot
// certify a fresh fence.
func (manager *Manager) CheckpointSpec(ctx context.Context) (spec storage.SnapshotSpec, err error) {
	operation, finish, err := manager.operation(ctx)
	if err != nil {
		return spec, err
	}
	defer finish()
	if manager.historicalOnly {
		// No fresh certification is possible; re-issue the fence over the
		// persisted closed frontier exactly as stored.
		manager.mu.Lock()
		state := manager.frontier
		manager.mu.Unlock()
		if state.ClosedTS == 0 {
			return spec, clock.ErrClockUnavailable
		}
		fence, err := manager.authority.Issue(operation, state.ClosedTS, state.LogPosition, state.Epoch)
		if err != nil {
			return spec, err
		}
		return storage.SnapshotSpec{Timestamp: state.ClosedTS, Fence: fence}, nil
	}
	if err := manager.sequencer.lock(operation); err != nil {
		return spec, err
	}
	defer manager.sequencer.unlock()
	// The gate is held before the unresolved-commit check: a commit that becomes
	// indeterminate while this call waits for the gate must not be followed by a
	// fresh fence over a frontier that was never published.
	if manager.commitUnresolved() {
		return storage.SnapshotSpec{}, storage.ErrCommitIndeterminate
	}
	err = manager.sequencer.fenceLocked(operation, 0, func(ctx context.Context, timestamp model.Timestamp, epoch clock.Epoch) error {
		manager.mu.Lock()
		state := manager.frontier
		manager.mu.Unlock()
		if timestamp <= state.ClosedTS {
			epoch = state.Epoch
		}
		closed := max(timestamp, state.ClosedTS)
		fence, err := manager.authority.Issue(ctx, closed, state.LogPosition, epoch)
		if err != nil {
			return err
		}
		spec = storage.SnapshotSpec{Timestamp: closed, Fence: fence}
		return nil
	})
	if err != nil {
		return storage.SnapshotSpec{}, err
	}
	return spec, nil
}

func (manager *Manager) openStrongSnapshot(ctx context.Context, overlay storage.OverlayView, overlayVersion uint64) (openedCatalogSnapshot, error) {
	var opened openedCatalogSnapshot
	operation, finish, err := manager.operation(ctx)
	if err != nil {
		return opened, err
	}
	defer finish()
	if manager.historicalOnly {
		return opened, clock.ErrClockUnavailable
	}
	if err := manager.sequencer.lock(operation); err != nil {
		return opened, err
	}
	defer manager.sequencer.unlock()
	if manager.commitUnresolved() {
		return opened, storage.ErrCommitIndeterminate
	}
	err = manager.sequencer.fenceLocked(operation, 0, func(ctx context.Context, timestamp model.Timestamp, epoch clock.Epoch) error {
		reader, catalogVersion, err := manager.openClosedSnapshot(ctx, timestamp, epoch, overlay)
		if err != nil {
			return err
		}
		opened = openedCatalogSnapshot{
			reader:    reader,
			timestamp: timestamp,
			identity:  CatalogReadIdentity{Live: true, Version: catalogVersion, Overlay: overlayVersion},
		}
		return nil
	})
	if err != nil {
		if opened.reader != nil {
			err = errors.Join(err, opened.reader.Close())
		}
		return openedCatalogSnapshot{}, err
	}
	return opened, nil
}

func (manager *Manager) openTransactionSnapshot(ctx context.Context, overlay storage.OverlayView, overlayVersion uint64) (openedCatalogSnapshot, error) {
	operation, finish, err := manager.operation(ctx)
	if err != nil {
		return openedCatalogSnapshot{}, err
	}
	defer finish()
	if manager.historicalOnly {
		return openedCatalogSnapshot{}, clock.ErrClockUnavailable
	}
	reading, err := manager.sequencer.provider.Read(operation)
	if err != nil {
		return openedCatalogSnapshot{}, err
	}
	if err := validateCertifiedReading(reading, manager.sequencer.history); err != nil {
		return openedCatalogSnapshot{}, err
	}
	manager.mu.Lock()
	state := manager.frontier
	catalogVersion := manager.catalogVersion
	unresolved := manager.pending != nil && !manager.pendingInFlight
	manager.mu.Unlock()
	if unresolved {
		return openedCatalogSnapshot{}, storage.ErrCommitIndeterminate
	}
	if state.ClosedTS == 0 {
		return manager.openStrongSnapshot(ctx, overlay, overlayVersion)
	}
	fence, err := manager.authority.Issue(operation, state.ClosedTS, state.LogPosition, state.Epoch)
	if err != nil {
		return openedCatalogSnapshot{}, err
	}
	reader, err := manager.backend.OpenSnapshot(operation, storage.SnapshotSpec{Timestamp: state.ClosedTS, Fence: fence, Overlay: overlay})
	if err != nil {
		return openedCatalogSnapshot{}, err
	}
	return openedCatalogSnapshot{
		reader:    reader,
		timestamp: state.ClosedTS,
		identity:  CatalogReadIdentity{Live: true, Version: catalogVersion, Overlay: overlayVersion},
	}, nil
}

func (manager *Manager) HistoricalSnapshot(ctx context.Context, timestamp model.Timestamp) (storage.SnapshotReader, error) {
	opened, err := manager.historicalSnapshot(ctx, timestamp)
	return opened.reader, err
}

func (manager *Manager) historicalSnapshot(ctx context.Context, timestamp model.Timestamp) (openedCatalogSnapshot, error) {
	operation, finish, err := manager.operation(ctx)
	if err != nil {
		return openedCatalogSnapshot{}, err
	}
	defer finish()
	manager.mu.Lock()
	state := manager.frontier
	manager.mu.Unlock()
	if timestamp <= 0 {
		return openedCatalogSnapshot{}, model.ErrTimestampRange
	}
	if timestamp < state.HistoryFloor {
		return openedCatalogSnapshot{}, storage.ErrSnapshotTooOld
	}
	if timestamp > state.ClosedTS {
		if manager.historicalOnly {
			return openedCatalogSnapshot{}, clock.ErrClockUnavailable
		}
		var opened openedCatalogSnapshot
		err := manager.sequencer.closeAt(operation, timestamp, func(ctx context.Context, closed model.Timestamp, epoch clock.Epoch) error {
			reader, catalogVersion, err := manager.openClosedSnapshot(ctx, closed, epoch, nil)
			if err != nil {
				return err
			}
			opened = openedCatalogSnapshot{
				reader:    reader,
				timestamp: closed,
				identity:  CatalogReadIdentity{Live: true, Version: catalogVersion},
			}
			return nil
		})
		if err != nil {
			if opened.reader != nil {
				err = errors.Join(err, opened.reader.Close())
			}
			return openedCatalogSnapshot{}, err
		}
		return opened, nil
	}
	fence, err := manager.authority.Issue(operation, state.ClosedTS, state.LogPosition, state.Epoch)
	if err != nil {
		return openedCatalogSnapshot{}, err
	}
	reader, err := manager.backend.OpenSnapshot(operation, storage.SnapshotSpec{Timestamp: timestamp, Fence: fence})
	if err != nil {
		return openedCatalogSnapshot{}, err
	}
	// A read at or below the published frontier sees exactly the commits whose
	// timestamps are at most timestamp, so the timestamp alone identifies its
	// catalog content; the published catalog version would keep advancing.
	return openedCatalogSnapshot{
		reader:    reader,
		timestamp: timestamp,
		identity:  CatalogReadIdentity{Fixed: timestamp},
	}, nil
}

// Called under the sequencer gate, including the durable backend fence update.
// The returned version is the catalog content version the snapshot exposes:
// every published commit is visible to a snapshot taken above the frontier.
func (manager *Manager) openClosedSnapshot(ctx context.Context, timestamp model.Timestamp, epoch clock.Epoch, overlay storage.OverlayView) (storage.SnapshotReader, uint64, error) {
	manager.mu.Lock()
	state := manager.frontier
	catalogVersion := manager.catalogVersion
	unresolved := manager.pending != nil
	manager.mu.Unlock()
	if unresolved && timestamp > state.ClosedTS {
		return nil, 0, storage.ErrCommitIndeterminate
	}
	closed := max(timestamp, state.ClosedTS)
	if timestamp <= state.ClosedTS {
		epoch = state.Epoch
	}
	fence, err := manager.authority.Issue(ctx, closed, state.LogPosition, epoch)
	if err != nil {
		return nil, 0, err
	}
	reader, err := manager.backend.OpenSnapshot(ctx, storage.SnapshotSpec{Timestamp: timestamp, Fence: fence, Overlay: overlay})
	if err != nil {
		return nil, 0, err
	}
	manager.mu.Lock()
	manager.frontier.ClosedTS = closed
	manager.frontier.Epoch = epoch
	manager.mu.Unlock()
	return reader, catalogVersion, nil
}

func (manager *Manager) Close() error {
	manager.cancel()
	manager.mu.Lock()
	if manager.closed {
		done := manager.closeDone
		manager.mu.Unlock()
		<-done
		manager.mu.Lock()
		err := manager.closeErr
		manager.mu.Unlock()
		return err
	}
	manager.closed = true
	manager.mu.Unlock()
	err := manager.closeTransactions()
	manager.operations.Wait()
	manager.mu.Lock()
	manager.backend = nil
	manager.authority = nil
	manager.sequencer = nil
	manager.pending = nil
	manager.transactions = nil
	manager.closeErr = err
	close(manager.closeDone)
	manager.mu.Unlock()
	return err
}
