package neosepia

import (
	"context"
	"errors"
	"os"
	"sync"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

type Backend struct {
	commitMu           sync.Mutex
	mu                 sync.Mutex
	directory          *databaseDirectory
	runs               *runSet
	limits             Limits
	verifier           storage.FenceVerifier
	encoder            storage.KeyEncoder
	extras             []string
	state              databaseState
	durableClosed      storage.ReadFence
	stateDirty         bool
	frontier           recoveredPosition
	publishedEpoch     clock.Epoch
	transactions       ordered.Tree[recoveryTxnID, transactionRecord]
	pending            *pendingCommit
	metadataUncertain  bool
	collectionBoundary model.Timestamp
	closed             bool
	maintenanceCtx     context.Context
	maintenanceCancel  context.CancelFunc
	maintenanceGate    chan struct{}
	maintenanceWG      sync.WaitGroup
	closeDone          chan struct{}
	closeErr           error
	maintenanceFault   func(context.Context, maintenancePoint) error
	commitFault        commitFault
	stateFault         stateFault
}

type pendingCommit struct {
	decision     runDecision
	epoch        clock.Epoch
	transactions ordered.Tree[recoveryTxnID, transactionRecord]
	admission    *runAdmission
}

// State is an owned recovery/coordination view, not a new read barrier.
type State struct {
	LogPosition                      uint64
	CommitTS, ClosedTS, HistoryFloor model.Timestamp
	Epoch                            clock.Epoch
	Scale                            clock.ScaleDescriptor
}

var _ storage.Backend = (*Backend)(nil)
var _ storage.Maintenance = (*Backend)(nil)

func Open(ctx context.Context, config Config) (_ *Backend, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.Directory == "" || config.KeyEncoder == nil || config.FenceVerifier == nil {
		return nil, ErrInvalidConfig
	}
	if config.Limits == (Limits{}) {
		config.Limits = DefaultLimits()
	}
	if err := config.Limits.validate(); err != nil {
		return nil, err
	}
	extras, err := validateCheckpointExtras(config.ExtraCheckpointFiles)
	if err != nil {
		return nil, err
	}
	suppliedScale := config.Scale.Name != "" || config.Scale.LeapHistoryID != ([32]byte{}) || len(config.Scale.CanonicalLeapHistory) != 0
	if suppliedScale {
		if err := clock.ValidateScale(ctx, config.Scale, config.Limits.MetadataBytes/2); err != nil {
			return nil, err
		}
	}
	if err := inspectDatabaseDirectory(ctx, config.Directory); err != nil {
		return nil, err
	}
	directory, err := openDatabaseDirectory(ctx, config.Directory)
	if err != nil {
		return nil, err
	}
	maintenanceCtx, maintenanceCancel := context.WithCancel(context.Background())
	backend := &Backend{
		directory: directory, limits: config.Limits, verifier: config.FenceVerifier, encoder: config.KeyEncoder, extras: extras,
		maintenanceCtx: maintenanceCtx, maintenanceCancel: maintenanceCancel, maintenanceGate: make(chan struct{}, 1), closeDone: make(chan struct{}),
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, backend.Close())
		}
	}()
	if err := inspectDatabaseDirectory(ctx, config.Directory); err != nil {
		return nil, err
	}
	state, err := readDatabaseState(ctx, directory.root, config.Limits.MetadataBytes)
	if errors.Is(err, os.ErrNotExist) {
		if !suppliedScale {
			return nil, ErrInvalidConfig
		}
		state = databaseState{Generation: 1, Scales: []scaleVersion{{Scale: copyClockScale(config.Scale)}}}
		if _, err := writeDatabaseState(ctx, directory.root, state, config.Limits.MetadataBytes, nil); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if state.Closed.ClosedTS != 0 {
		if err := backend.verifier.VerifyReadFence(ctx, state.Closed); err != nil {
			return nil, err
		}
	}
	backend.state = state
	backend.durableClosed = state.Closed
	recovered, err := recoverPublishedRuns(ctx, directory, state, runRecoveryLimits{
		Log: config.Limits.recovery(), Preparation: config.Limits.preparation(), Resident: config.Limits.resident(),
		StagingBytes: config.Limits.ResidentBytes, IdentityBytes: config.Limits.IdentityBytes,
	}, func(record *storedCommit) error {
		if err := validateRecordState(record, state); err != nil {
			return err
		}
		backend.publishedEpoch = record.Batch.Epoch
		return nil
	})
	if err != nil {
		return nil, err
	}
	backend.runs, backend.frontier, backend.transactions, backend.publishedEpoch = recovered.Set, recovered.Position, recovered.Transactions, recovered.PublishedEpoch
	if err := backend.validateState(ctx, state); err != nil {
		return nil, err
	}
	if suppliedScale {
		current := state.Scales[len(state.Scales)-1].Scale
		if err := clock.ValidateScaleExtension(ctx, current, config.Scale, clock.Instant(backend.closedTimestamp()), config.Limits.MetadataBytes/2); err != nil {
			return nil, err
		}
		if current.LeapHistoryID != config.Scale.LeapHistoryID {
			if state.Generation == ^uint64(0) {
				return nil, storage.ErrCorruptStorage
			}
			state.Generation++
			versions := make([]scaleVersion, len(state.Scales)+1)
			copy(versions, state.Scales)
			versions[len(state.Scales)] = scaleVersion{Position: backend.frontier.LogPosition, Timestamp: backend.closedTimestamp(), Scale: copyClockScale(config.Scale)}
			state.Scales = versions
			if _, err := writeDatabaseState(ctx, directory.root, state, config.Limits.MetadataBytes, nil); err != nil {
				return nil, err
			}
			backend.state = state
			backend.durableClosed = state.Closed
		}
	}
	if err := directory.root.Remove(databaseStatePending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := syncCommitDirectory(directory.root); err != nil {
		return nil, err
	}
	if err := cleanupObsoleteRuns(ctx, directory.runs, backend.runs); err != nil {
		return nil, err
	}
	if err := backend.retireRecoveredWAL(ctx); err != nil {
		return nil, err
	}
	if err := cleanupCheckpointDirectories(ctx, directory.root); err != nil {
		return nil, err
	}
	return backend, nil
}

func copyClockScale(scale clock.ScaleDescriptor) clock.ScaleDescriptor {
	result := clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: scale.LeapHistoryID, CanonicalLeapHistory: make([]byte, len(scale.CanonicalLeapHistory))}
	copy(result.CanonicalLeapHistory, scale.CanonicalLeapHistory)
	return result
}

func validateRecordState(record *storedCommit, state databaseState) error {
	if record.Batch.Epoch.Assurance == ([16]byte{}) || record.Batch.Epoch.Config == ([32]byte{}) {
		return storage.ErrCorruptStorage
	}
	active := state.Scales[0]
	for _, version := range state.Scales[1:] {
		if version.Position >= record.Result.LogPosition {
			break
		}
		active = version
	}
	if record.Batch.Epoch.LeapHistory != active.Scale.LeapHistoryID || record.Result.CommitTS <= active.Timestamp {
		return storage.ErrCorruptStorage
	}
	if state.Closed.ClosedTS != 0 {
		if record.Result.LogPosition <= state.Closed.LogPosition {
			if record.Result.CommitTS > state.Closed.ClosedTS {
				return storage.ErrCorruptStorage
			}
		} else if record.Result.CommitTS <= state.Closed.ClosedTS {
			return storage.ErrCorruptStorage
		}
	}
	return nil
}

func (backend *Backend) validateState(ctx context.Context, state databaseState) error {
	closed := max(state.Closed.ClosedTS, backend.frontier.CommitTS)
	if state.Closed.LogPosition > backend.frontier.LogPosition || state.HistoryFloor > closed {
		return storage.ErrCorruptStorage
	}
	for _, version := range state.Scales {
		if version.Position > backend.frontier.LogPosition || version.Timestamp > closed {
			return storage.ErrCorruptStorage
		}
	}
	if state.Closed.ClosedTS != 0 {
		return backend.runs.validateFence(ctx, state.Closed, backend.transactions)
	}
	return ctx.Err()
}

func (backend *Backend) closedTimestamp() model.Timestamp {
	return max(backend.state.Closed.ClosedTS, backend.frontier.CommitTS)
}

func (backend *Backend) State(ctx context.Context) (State, error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		return State{}, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	epoch := backend.publishedEpoch
	if backend.state.Closed.ClosedTS >= backend.frontier.CommitTS {
		epoch = backend.state.Closed.Epoch
	}
	return State{LogPosition: backend.frontier.LogPosition, CommitTS: backend.frontier.CommitTS, ClosedTS: backend.closedTimestamp(), HistoryFloor: backend.state.HistoryFloor, Epoch: epoch, Scale: copyClockScale(backend.state.Scales[len(backend.state.Scales)-1].Scale)}, nil
}

func (backend *Backend) Close() error {
	backend.mu.Lock()
	if backend.closed {
		done := backend.closeDone
		backend.mu.Unlock()
		if done != nil {
			<-done
		}
		backend.mu.Lock()
		err := backend.closeErr
		backend.mu.Unlock()
		return err
	}
	backend.closed = true
	backend.maintenanceCancel()
	done := backend.closeDone
	backend.mu.Unlock()

	backend.maintenanceWG.Wait()

	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	var stateErr error
	if backend.stateDirty && !backend.metadataUncertain && backend.pending == nil && backend.runs != nil && backend.directory != nil && backend.state.Generation < ^uint64(0) {
		next := backend.state
		next.Generation++
		_, stateErr = writeDatabaseState(context.Background(), backend.directory.root, next, backend.limits.MetadataBytes, nil)
		if stateErr == nil {
			backend.state = next
			backend.durableClosed = next.Closed
			backend.stateDirty = false
		}
	}
	var runErr error
	if backend.runs != nil {
		runErr = backend.runs.Close()
	}
	backend.pending = nil
	backend.transactions = ordered.Tree[recoveryTxnID, transactionRecord]{}
	backend.state = databaseState{}
	backend.durableClosed = storage.ReadFence{}
	backend.stateDirty = false
	backend.encoder = nil
	backend.verifier = nil
	backend.commitFault = nil
	backend.stateFault = nil
	backend.maintenanceFault = nil
	backend.closeErr = errors.Join(stateErr, runErr, backend.directory.Close())
	err := backend.closeErr
	backend.mu.Unlock()
	close(done)
	return err
}
