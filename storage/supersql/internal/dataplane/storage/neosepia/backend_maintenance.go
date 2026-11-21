package neosepia

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type maintenancePlan struct {
	pin              *runPin
	capturedPosition uint64
	floor            model.Timestamp
	targets          []storage.ObjectRef
	retired          map[objectGeneration]struct{}
}

type maintenancePoint uint8

// Maintenance fault points name the durable publication boundaries of one
// collection: the captured plan, the base manifest, and the state record.
const (
	maintenanceAfterCapture maintenancePoint = 1
	maintenanceBeforeBase   maintenancePoint = 2
	maintenanceAfterBase    maintenancePoint = 3
	maintenanceAfterState   maintenancePoint = 4
)

type compactionTask struct {
	id   string
	done chan struct{}
	mu   sync.Mutex
	err  error
}

var _ storage.CompactionTaskHandle = (*compactionTask)(nil)

func (task *compactionTask) TaskID() string { return task.id }

func (task *compactionTask) Wait(ctx context.Context) error {
	select {
	case <-task.done:
		task.mu.Lock()
		defer task.mu.Unlock()
		return task.err
	default:
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-task.done:
		task.mu.Lock()
		defer task.mu.Unlock()
		return task.err
	}
}

func (task *compactionTask) finish(err error) {
	task.mu.Lock()
	task.err = err
	task.mu.Unlock()
	close(task.done)
}

func (backend *Backend) Collect(ctx context.Context, request storage.GCRequest) error {
	if request.Boundary < 0 || uint64(len(request.Retired)) > backend.limits.MaxFiles {
		return storage.ErrInvalidMutation
	}
	var previous storage.ObjectGeneration
	for position, retired := range request.Retired {
		if err := retired.Validate(request.Boundary); err != nil {
			return err
		}
		target := retired.Target()
		if position > 0 && (target.ObjectID < previous.ObjectID || target.ObjectID == previous.ObjectID && target.Generation <= previous.Generation) {
			return storage.ErrInvalidMutation
		}
		previous = target
	}
	if err := backend.acquireMaintenance(ctx); err != nil {
		return err
	}
	defer backend.releaseMaintenance()
	backend.mu.Lock()
	operation, cancel := context.WithCancel(backend.maintenanceCtx)
	backend.mu.Unlock()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	defer cancel()
	return backend.runMaintenance(operation, nil, request.Boundary, 0, request.Retired)
}

func (backend *Backend) ScheduleCompaction(ctx context.Context, request storage.CompactionRequest) (storage.CompactionTaskHandle, error) {
	if err := request.Target.Validate(); err != nil {
		return nil, err
	}
	if request.GCWatermark < 0 || request.TargetBlock < 0 || uint64(request.TargetBlock) > backend.limits.BlockBytes {
		return nil, storage.ErrInvalidMutation
	}
	if err := backend.acquireMaintenance(ctx); err != nil {
		return nil, err
	}
	id, err := newCompactionFileID()
	if err != nil {
		backend.releaseMaintenance()
		return nil, err
	}
	task := &compactionTask{id: id, done: make(chan struct{})}
	backend.mu.Lock()
	if backend.closed {
		backend.mu.Unlock()
		backend.releaseMaintenance()
		return nil, storage.ErrClosed
	}
	operation, cancel := context.WithCancel(backend.maintenanceCtx)
	backend.mu.Unlock()
	stop := context.AfterFunc(ctx, cancel)
	go func() {
		var taskErr error
		defer func() { task.finish(taskErr) }()
		defer backend.releaseMaintenance()
		defer cancel()
		defer stop()
		taskErr = backend.runMaintenance(operation, &request.Target, request.GCWatermark, request.TargetBlock, nil)
	}()
	return task, nil
}
func (backend *Backend) acquireMaintenance(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	backend.mu.Lock()
	if backend.closed {
		backend.mu.Unlock()
		return storage.ErrClosed
	}
	maintenanceCtx := backend.maintenanceCtx
	gate := backend.maintenanceGate
	backend.mu.Unlock()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-maintenanceCtx.Done():
		return storage.ErrClosed
	case gate <- struct{}{}:
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		<-gate
		return storage.ErrClosed
	}
	backend.maintenanceWG.Add(1)
	return nil
}

func (backend *Backend) releaseMaintenance() {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if !backend.metadataUncertain {
		backend.collectionBoundary = 0
	}
	<-backend.maintenanceGate
	backend.maintenanceWG.Done()
}

func (backend *Backend) runMaintenance(ctx context.Context, requested *storage.ObjectRef, watermark model.Timestamp, targetBlock int, retired []storage.ObjectRetirement) (err error) {
	plan, err := backend.captureMaintenance(ctx, requested, watermark, retired)
	if err != nil {
		return err
	}
	defer func() {
		if plan.pin != nil {
			err = errors.Join(err, plan.pin.Close())
		}
	}()
	if err := backend.maintenanceBoundary(ctx, maintenanceAfterCapture); err != nil {
		return err
	}
	if plan.pin == nil {
		return backend.publishEmptyCollection(ctx, watermark)
	}
	fileID, err := newCompactionFileID()
	if err != nil {
		return err
	}
	preparation := backend.limits.preparation()
	preparation.File.MaxBytes = min(preparation.File.MaxBytes, compactionFileBytes)
	prepared, err := newCompactedRuns(backend.directory.runs, fileID, preparation)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, prepared.Close()) }()
	blockLimits := backend.limits.preparation().File.Block
	if targetBlock != 0 {
		blockLimits.MaxBytes = uint64(targetBlock)
	}
	compacted := make(map[string]struct{})
	emitted := make(map[objectGeneration][]emittedInterval)
	for _, target := range plan.targets {
		if _, remove := plan.retired[targetGeneration(target)]; remove {
			if err := plan.pin.visit(ctx, target, func(file *sstFile) error {
				compacted[file.descriptor.Name] = struct{}{}
				return nil
			}); err != nil {
				return err
			}
			continue
		}
		// A maintenance pass rewrites one key interval per iteration. Repeat
		// until every file of the target is either rewritten or dropped;
		// otherwise superseded files survive publication and their physical
		// storage is never reclaimed.
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := plan.pin.visit(ctx, target, func(file *sstFile) error {
				if file.tier == inactiveTier && file.historyUntil <= plan.floor {
					compacted[file.descriptor.Name] = struct{}{}
				}
				return nil
			}); err != nil {
				return err
			}
			var work levelCompaction
			if requested == nil {
				work, err = plan.pin.selectTargetCompaction(ctx, target, compacted, backend.limits)
			} else {
				work, err = plan.pin.selectLevelCompaction(ctx, target, true, compacted)
			}
			if err != nil {
				return err
			}
			if len(work.files) == 0 {
				break
			}
			for name := range work.files {
				compacted[name] = struct{}{}
			}
			// The destination level accepts at most one file per key range.
			// An earlier interval of this pass may already occupy the range at
			// that level; demote a conflicting output to L0, where overlap is
			// legal, and let a later pass merge it upward.
			level := work.level
			for _, interval := range emitted[targetGeneration(target)] {
				if interval.level == level && interval.overlaps(work.lower, work.upper) {
					level = 0
					break
				}
			}
			prepared.level = level
			prepared.target = work.target
			cursor, err := plan.pin.compactionCursor(ctx, work, plan.floor, backend.limits)
			if err != nil {
				return err
			}
			if err := appendTieredBlocks(ctx, cursor, plan.floor, blockLimits, backend.limits.KeyBytes, prepared); err != nil {
				return err
			}
			if err := prepared.finishWriters(ctx); err != nil {
				return err
			}
			prepared.writers = prepared.writers[:0]
			occupied, err := prepared.retainFragments(ctx, work)
			if err != nil {
				return err
			}
			key := targetGeneration(target)
			emitted[key] = append(emitted[key], occupied...)
			if level != 0 {
				emitted[key] = append(emitted[key], emittedInterval{level: level, lower: work.lower, upper: work.upper})
			}
		}
	}
	if err := prepared.finish(ctx); err != nil {
		return err
	}
	if err := plan.pin.Close(); err != nil {
		plan.pin = nil
		return err
	}
	plan.pin = nil
	files := prepared.files
	transferred, err := backend.publishMaintenance(ctx, plan, compacted, files, prepared.fragments, plan.floor)
	if transferred {
		prepared.detach()
	}
	return err
}

func newCompactionFileID() (string, error) {
	var identity [8]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(identity[:]), nil
}

func (backend *Backend) captureMaintenance(ctx context.Context, requested *storage.ObjectRef, watermark model.Timestamp, retired []storage.ObjectRetirement) (_ maintenancePlan, err error) {
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		return maintenancePlan{}, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return maintenancePlan{}, err
	}
	if backend.pending != nil || backend.metadataUncertain {
		return maintenancePlan{}, storage.ErrCommitIndeterminate
	}
	floor := backend.state.HistoryFloor
	if requested == nil {
		if watermark < floor || watermark > backend.closedTimestamp() {
			return maintenancePlan{}, storage.ErrInvalidMutation
		}
		if err := backend.runs.checkCollectionBoundary(ctx, watermark); err != nil {
			return maintenancePlan{}, err
		}
		backend.collectionBoundary = watermark
		floor = watermark
	} else if watermark > floor {
		return maintenancePlan{}, storage.ErrInvalidMutation
	}
	pin, err := backend.runs.pin(ctx)
	if err != nil {
		return maintenancePlan{}, err
	}
	if pin.generation == nil {
		if err := pin.Close(); err != nil {
			return maintenancePlan{}, err
		}
		return maintenancePlan{floor: floor}, nil
	}
	if floor > 0 {
		if err := pin.bindTimestamp(ctx, floor); err != nil {
			return maintenancePlan{}, errors.Join(err, pin.Close())
		}
	}
	targets, err := pin.maintenanceTargets(ctx)
	if err != nil {
		return maintenancePlan{}, errors.Join(err, pin.Close())
	}
	if requested != nil {
		found := false
		for _, target := range targets {
			if targetGeneration(target) == targetGeneration(*requested) {
				targets = []storage.ObjectRef{target}
				found = true
				break
			}
		}
		if !found {
			return maintenancePlan{}, errors.Join(storage.ErrInvalidTarget, pin.Close())
		}
	}
	retiredSet := make(map[objectGeneration]struct{}, len(retired))
	for _, retirement := range retired {
		target := retirement.Target()
		retiredSet[objectGeneration{object: uint64(target.ObjectID), generation: target.Generation}] = struct{}{}
	}
	return maintenancePlan{pin: pin, capturedPosition: pin.generation.position, floor: floor, targets: targets, retired: retiredSet}, nil
}

func (backend *Backend) publishEmptyCollection(ctx context.Context, boundary model.Timestamp) error {
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if boundary <= backend.state.HistoryFloor {
		return ctx.Err()
	}
	if boundary > backend.closedTimestamp() || backend.state.Generation == math.MaxUint64 {
		return storage.ErrInvalidMutation
	}
	next := backend.state
	next.Generation++
	next.HistoryFloor = boundary
	uncertain, err := writeDatabaseState(ctx, backend.directory.root, next, backend.limits.MetadataBytes, backend.stateFault)
	if err != nil {
		backend.metadataUncertain = uncertain
		if uncertain {
			return errors.Join(storage.ErrCommitIndeterminate, err)
		}
		return err
	}
	backend.state = next
	backend.durableClosed = next.Closed
	backend.stateDirty = false
	return nil
}

// maintenanceBoundary reports the configured fault hook at one publication
// boundary. A backend without a hook publishes unconditionally.
func (backend *Backend) maintenanceBoundary(ctx context.Context, point maintenancePoint) error {
	if backend.maintenanceFault == nil {
		return nil
	}
	return backend.maintenanceFault(ctx, point)
}

func (backend *Backend) publishMaintenance(ctx context.Context, plan maintenancePlan, compacted map[string]struct{}, replacements []preparedRun, fragments map[string]*sstFile, boundary model.Timestamp) (transferred bool, err error) {
	backend.commitMu.Lock()
	defer backend.commitMu.Unlock()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		return false, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if backend.pending != nil || backend.metadataUncertain {
		return false, storage.ErrCommitIndeterminate
	}
	if boundary < backend.state.HistoryFloor || boundary > backend.closedTimestamp() || backend.state.Generation == math.MaxUint64 {
		return false, storage.ErrInvalidMutation
	}
	descriptors, err := backend.runs.replacementDescriptors(ctx, plan.capturedPosition, compacted, replacements)
	if err != nil {
		return false, err
	}
	generation := backend.state.Generation + 1
	if err := backend.directory.runs.Remove(baseManifestName(generation)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	base := runBase{
		Generation: generation, Position: backend.frontier.LogPosition, CommitTS: backend.frontier.CommitTS,
		Floor: boundary, Epoch: backend.publishedEpoch, Files: descriptors, Transactions: backend.transactions,
	}
	if err := backend.maintenanceBoundary(ctx, maintenanceBeforeBase); err != nil {
		return false, err
	}
	if err := writeRunBase(ctx, backend.directory.runs, base, backend.limits.MetadataBytes, backend.limits.IdentityBytes, backend.limits.MaxFiles); err != nil {
		return false, err
	}
	opened, err := backend.runs.openReplacementFiles(ctx, backend.directory.runs, descriptors, fragments, backend.limits.preparation().File, backend.limits.ResidentBytes)
	if err != nil {
		return false, errors.Join(err, backend.removeUnpublishedBase(generation))
	}
	openedOwned := true
	defer func() {
		if openedOwned {
			err = errors.Join(err, closeRunFiles(opened))
		}
	}()
	next := backend.state
	previousBase := next.BaseGeneration
	next.Generation = generation
	next.BaseGeneration = generation
	if err := backend.runs.checkReplacement(ctx, base.Position, base.CommitTS, opened); err != nil {
		return false, errors.Join(err, backend.removeUnpublishedBase(generation))
	}
	if err := backend.maintenanceBoundary(ctx, maintenanceAfterBase); err != nil {
		return false, errors.Join(err, backend.removeUnpublishedBase(generation))
	}
	next.HistoryFloor = boundary
	uncertain, stateErr := writeDatabaseState(ctx, backend.directory.root, next, backend.limits.MetadataBytes, backend.stateFault)
	if stateErr != nil {
		backend.metadataUncertain = uncertain
		if uncertain {
			return true, errors.Join(storage.ErrCommitIndeterminate, stateErr)
		}
		return false, errors.Join(stateErr, backend.removeUnpublishedBase(generation))
	}
	backend.durableClosed = next.Closed
	backend.stateDirty = false
	if err := backend.maintenanceBoundary(ctx, maintenanceAfterState); err != nil {
		// The durable state already names the new base while the run set still
		// holds the previous generation, so the collection is indeterminate
		// until a later reconcile installs the published base.
		backend.metadataUncertain = true
		return true, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	publicationCtx := context.WithoutCancel(ctx)
	openedOwned = false
	replaceErr := backend.runs.replaceLevelRange(publicationCtx, base.Position, base.CommitTS, base.Generation, opened)
	if replaceErr != nil {
		if !backend.runs.hasBase(base.Generation) {
			backend.metadataUncertain = true
			return true, errors.Join(storage.ErrCommitIndeterminate, replaceErr)
		}
	}
	backend.state = next
	cleanupErr := backend.cleanupSupersededRunMetadata(publicationCtx, base, previousBase)
	return true, errors.Join(replaceErr, cleanupErr)
}

func (backend *Backend) removeUnpublishedBase(generation uint64) error {
	err := backend.directory.runs.Remove(baseManifestName(generation))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return errors.Join(err, syncCommitDirectory(backend.directory.runs))
}

func (backend *Backend) cleanupSupersededRunMetadata(ctx context.Context, base runBase, previousBase uint64) (err error) {
	directory, err := backend.directory.runs.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close(), syncCommitDirectory(backend.directory.runs)) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(256)
		for _, entry := range entries {
			name := entry.Name()
			if len(name) != 25 || !strings.HasSuffix(name, ".runs") || !decimalPrefix(name[:20]) {
				continue
			}
			position, parseErr := strconv.ParseUint(name[:20], 10, 64)
			if parseErr != nil || position == 0 {
				return storage.ErrCorruptStorage
			}
			if position <= base.Position {
				if err := backend.directory.runs.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if previousBase != 0 && previousBase != base.Generation {
		if err := backend.directory.runs.Remove(baseManifestName(previousBase)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return retireCoveredWAL(ctx, backend.directory.commits, base.Position)
}

func (backend *Backend) retireRecoveredWAL(ctx context.Context) error {
	if backend.state.BaseGeneration == 0 {
		return nil
	}
	base, err := loadRunBase(ctx, backend.directory.runs, backend.state.BaseGeneration, backend.limits.MetadataBytes, backend.limits.IdentityBytes, backend.limits.MaxFiles)
	if err != nil {
		return err
	}
	return retireCoveredWAL(ctx, backend.directory.commits, base.Position)
}

// MaintenanceBacklog reports reclaimable work from one lock-consistent view:
// published commit segments in commits/ and live run files. It never waits on
// the maintenance gate.
func (backend *Backend) MaintenanceBacklog(ctx context.Context) (walSegments, runFiles uint64, err error) {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if backend.closed {
		return 0, 0, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	set := backend.runs
	set.mu.Lock()
	if set.closed {
		set.mu.Unlock()
		return 0, 0, storage.ErrClosed
	}
	runFiles = set.files
	set.mu.Unlock()
	directory, err := backend.directory.commits.Open(".")
	if err != nil {
		return 0, 0, err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		entries, readErr := directory.ReadDir(256)
		for _, entry := range entries {
			_, pending, err := commitFilePosition(entry.Name())
			if err != nil {
				return 0, 0, err
			}
			if pending {
				continue
			}
			walSegments++
		}
		if errors.Is(readErr, io.EOF) {
			return walSegments, runFiles, nil
		}
		if readErr != nil {
			return 0, 0, readErr
		}
	}
}

// UncompactedRunFiles reports the number of live L0 run files and post-base
// commit run files that have not yet been folded into the durable base.
func (backend *Backend) UncompactedRunFiles() uint64 {
	set := backend.runs
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return 0
	}
	var count uint64
	for generation := set.head; generation != nil; generation = generation.parent {
		if generation.baseGeneration == 0 {
			count += uint64(len(generation.files))
			continue
		}
		count += uint64(len(generation.levels[0]))
		break
	}
	return count
}

// A durable base owns the covered decisions and transaction identities. Captured
// checkpoints own hardlinks; snapshots read SST handles, never these WAL names.
func retireCoveredWAL(ctx context.Context, root *os.Root, covered uint64) (err error) {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close(), syncCommitDirectory(root)) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(256)
		for _, entry := range entries {
			position, pending, err := commitFilePosition(entry.Name())
			if err != nil {
				return err
			}
			if pending || position > covered {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return storage.ErrCorruptStorage
			}
			if err := root.Remove(entry.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}
