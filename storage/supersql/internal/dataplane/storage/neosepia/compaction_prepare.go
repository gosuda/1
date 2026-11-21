package neosepia

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type objectGeneration struct {
	object     uint64
	generation uint64
}

func targetGeneration(target storage.ObjectRef) objectGeneration {
	return objectGeneration{object: uint64(target.ObjectID), generation: target.Generation}
}

const maxCompactionOpenWriters = 64

type compactionWriterKey struct {
	target storage.ObjectRef
	tier   runTier
	window uint64
}

type compactionWriter struct {
	target storage.ObjectRef
	name   string
	file   *os.File
	writer *sstWriter
	rows   uint64
	tier   runTier
	window uint64
	touch  uint64
}

type compactedRuns struct {
	root          *os.Root
	taskID        string
	limits        runPreparationLimits
	files         []preparedRun
	writers       map[compactionWriterKey]*compactionWriter
	maxWriters    int
	nextFile      uint64
	touchSeq      uint64
	scratch       []byte
	metadataBytes uint64
	detached      bool
	level         uint8
	target        storage.ObjectRef
	fragments     map[string]*sstFile
}

func newCompactedRuns(root *os.Root, taskID string, limits runPreparationLimits) (*compactedRuns, error) {
	if root == nil || taskID == "" || limits.MaxBuilders <= 0 || limits.MaxRuns == 0 {
		return nil, storage.ErrInvalidTarget
	}
	writerCost := uint64(unsafe.Sizeof(compactionWriter{}))
	fixed := uint64(64 << 10)
	minWritersCost := uint64(limits.MaxBuilders) * writerCost
	if fixed > limits.MetadataBytes || minWritersCost > limits.MetadataBytes-fixed {
		return nil, columnar.ErrBatchBudget
	}
	maxWriters := max(limits.MaxBuilders, maxCompactionOpenWriters)
	affordable := int((limits.MetadataBytes - fixed) / writerCost)
	if affordable < maxWriters {
		maxWriters = max(limits.MaxBuilders, affordable)
	}
	fixed += minWritersCost
	return &compactedRuns{
		root:          root,
		taskID:        taskID,
		limits:        limits,
		writers:       make(map[compactionWriterKey]*compactionWriter),
		maxWriters:    maxWriters,
		scratch:       make([]byte, 64<<10),
		metadataBytes: fixed,
	}, nil
}

func (runs *compactedRuns) appendTierBlock(ctx context.Context, target storage.ObjectRef, data []byte, rows uint64, tier runTier, window uint64, historyUntil model.Timestamp) error {
	return runs.appendTierBlockWithBlobs(ctx, target, data, nil, rows, tier, window, historyUntil)
}

func (runs *compactedRuns) appendTierBlockWithBlobs(ctx context.Context, target storage.ObjectRef, data []byte, blobs []toastBlob, rows uint64, tier runTier, window uint64, historyUntil model.Timestamp) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateScanTarget(runs.target, target); err != nil {
		return err
	}
	target = runs.target
	key := compactionWriterKey{target: target, tier: tier, window: window}
	active := runs.writers[key]
	if active == nil {
		if len(runs.writers) >= runs.maxWriters {
			if err := runs.evictLRUWriter(ctx); err != nil {
				return err
			}
		}
		writer, err := runs.openWriter(target, tier, window)
		if err != nil {
			return err
		}
		runs.writers[key] = writer
		active = writer
	}
	err := active.writer.AppendWithBlobs(ctx, data, blobs)
	if errors.Is(err, columnar.ErrBatchBudget) && active.rows != 0 {
		if finishErr := runs.finishWriter(ctx, key); finishErr != nil {
			return finishErr
		}
		writer, openErr := runs.openWriter(target, tier, window)
		if openErr != nil {
			return openErr
		}
		runs.writers[key] = writer
		active = writer
		err = active.writer.AppendWithBlobs(ctx, data, blobs)
	}
	if err != nil {
		return err
	}
	active.rows += rows
	active.writer.historyUntil = max(active.writer.historyUntil, historyUntil)
	runs.touchSeq++
	active.touch = runs.touchSeq
	return nil
}

func (runs *compactedRuns) evictLRUWriter(ctx context.Context) error {
	var victimKey compactionWriterKey
	var victim *compactionWriter
	// Prefer evicting the least-recently-used inactive window writer so active
	// output stays contiguous in a single run unless it hits compactionFileBytes.
	for key, writer := range runs.writers {
		if writer.tier != inactiveTier {
			continue
		}
		if victim == nil || writer.touch < victim.touch {
			victimKey, victim = key, writer
		}
	}
	if victim == nil {
		for key, writer := range runs.writers {
			if victim == nil || writer.touch < victim.touch {
				victimKey, victim = key, writer
			}
		}
	}
	if victim == nil {
		return columnar.ErrBatchBudget
	}
	return runs.finishWriter(ctx, victimKey)
}

func (runs *compactedRuns) openWriter(target storage.ObjectRef, tier runTier, window uint64) (*compactionWriter, error) {
	if uint64(len(runs.files)+len(runs.writers)) >= runs.limits.MaxRuns {
		return nil, columnar.ErrBatchBudget
	}
	suffix := "active"
	if tier == inactiveTier {
		suffix = "inactive"
	}
	seq := runs.nextFile
	runs.nextFile++
	name := fmt.Sprintf("c-%s-%08x.%s.sst", runs.taskID[:min(16, len(runs.taskID))], seq, suffix)
	if !validBaseRunName(name) {
		return nil, storage.ErrInvalidTarget
	}
	file, err := runs.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	writer, err := newSSTWriter(file, target, runs.limits.File)
	if err != nil {
		return nil, errors.Join(err, file.Close(), runs.root.Remove(name))
	}
	writer.tier = tier
	writer.timeWindow = window
	if tier == activeTier {
		writer.level = runs.level
	}
	return &compactionWriter{target: target, name: name, file: file, writer: writer, tier: tier, window: window}, nil
}

func (runs *compactedRuns) finishWriter(ctx context.Context, key compactionWriterKey) (err error) {
	active := runs.writers[key]
	if active == nil || active.writer == nil {
		delete(runs.writers, key)
		return nil
	}
	delete(runs.writers, key)
	file, name, target, rows := active.file, active.name, active.target, active.rows
	defer func() {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
		if err != nil {
			removeErr := runs.root.Remove(name)
			if !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
	}()
	if rows == 0 {
		return storage.ErrBatchEmpty
	}
	if err := runs.reserveFile(name); err != nil {
		return err
	}
	if err := active.writer.Finish(ctx); err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	checksum, err := checksumRunFile(ctx, file, info.Size(), runs.scratch)
	if err != nil {
		return err
	}
	closeErr := file.Close()
	file = nil
	if closeErr != nil {
		return closeErr
	}
	runs.files = append(runs.files, preparedRun{Name: name, Target: target, Rows: rows, Bytes: uint64(info.Size()), Checksum: checksum})
	return nil
}

func (runs *compactedRuns) reserveFile(name string) error {
	extra := uint64(len(name))
	if len(runs.files) == cap(runs.files) {
		capacity := min(max(4, len(runs.files)+1, min(math.MaxInt/2, cap(runs.files))*2), int(runs.limits.MaxRuns))
		extra += uint64(capacity-cap(runs.files)) * uint64(unsafe.Sizeof(preparedRun{}))
		if extra > runs.limits.MetadataBytes-runs.metadataBytes {
			return columnar.ErrBatchBudget
		}
		files := make([]preparedRun, len(runs.files), capacity)
		copy(files, runs.files)
		runs.files = files
	} else if extra > runs.limits.MetadataBytes-runs.metadataBytes {
		return columnar.ErrBatchBudget
	}
	runs.metadataBytes += extra
	return nil
}

func (runs *compactedRuns) finishWriters(ctx context.Context) error {
	if len(runs.writers) == 0 {
		return nil
	}
	keys := make([]compactionWriterKey, 0, len(runs.writers))
	for key := range runs.writers {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b compactionWriterKey) int {
		if a.tier != b.tier {
			if a.tier < b.tier {
				return -1
			}
			return 1
		}
		if a.window != b.window {
			if a.window < b.window {
				return -1
			}
			return 1
		}
		if a.target.ObjectID != b.target.ObjectID {
			if a.target.ObjectID < b.target.ObjectID {
				return -1
			}
			return 1
		}
		if a.target.Generation < b.target.Generation {
			return -1
		}
		if a.target.Generation > b.target.Generation {
			return 1
		}
		return 0
	})
	for _, key := range keys {
		if err := runs.finishWriter(ctx, key); err != nil {
			return err
		}
	}
	return nil
}

func (runs *compactedRuns) finish(ctx context.Context) error {
	if err := runs.finishWriters(ctx); err != nil {
		return err
	}
	return syncCommitDirectory(runs.root)
}

func (runs *compactedRuns) detach() []preparedRun {
	files := runs.files
	runs.files = nil
	runs.detached = true
	return files
}

func (runs *compactedRuns) Close() error {
	if runs.detached {
		return nil
	}
	runs.detached = true
	var err error
	for key, writer := range runs.writers {
		if writer.file != nil {
			err = errors.Join(err, writer.file.Close())
			writer.file = nil
		}
		if writer.name != "" {
			removeErr := runs.root.Remove(writer.name)
			if !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
		delete(runs.writers, key)
	}
	for _, file := range runs.files {
		removeErr := runs.root.Remove(file.Name)
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	runs.files = nil
	runs.writers = nil
	runs.scratch = nil
	return err
}

func (pin *runPin) compactionCursor(ctx context.Context, work levelCompaction, floor model.Timestamp, limits Limits) (_ *mvccCursor, err error) {
	ledgerBytes := uint64(unsafe.Sizeof(sourceMemory{}))
	perSource := uint64(unsafe.Sizeof(runVersionCursor{})) + uint64(unsafe.Sizeof((*runVersionCursor)(nil)))
	var count, visited uint64
	if err := pin.visit(ctx, work.target, func(file *sstFile) error {
		visited++
		if _, included := work.files[file.descriptor.Name]; !included {
			return nil
		}
		if count == uint64(math.MaxInt) || ledgerBytes > limits.ReadSourceBytes || count >= (limits.ReadSourceBytes-ledgerBytes)/perSource {
			return columnar.ErrBatchBudget
		}
		count++
		return nil
	}); err != nil {
		return nil, err
	}
	// Retention may drop versions at or below the floor only when the sources
	// cover the complete target history. A partial compaction that omitted a file
	// would otherwise resurrect a version it still holds.
	if count != visited {
		floor = 0
	}
	var memory *sourceMemory
	if count != 0 {
		memory = &sourceMemory{limit: limits.ReadSourceBytes - ledgerBytes - count*perSource}
	}
	sources := make([]*runVersionCursor, 0, int(count))
	defer func() {
		if err != nil {
			for _, source := range sources {
				err = errors.Join(err, source.Close())
			}
		}
	}()
	if err := pin.visit(ctx, work.target, func(file *sstFile) error {
		if _, included := work.files[file.descriptor.Name]; !included {
			return nil
		}
		first, end := file.rangeBlocks(0, file.count, work.lower, work.upper)
		source, err := newRunVersionCursor(file, first, end, limits.ReadBlockBytes, memory)
		if err != nil {
			return err
		}
		source.lowerKey, source.upperKey = work.lower, work.upper
		sources = append(sources, source)
		return nil
	}); err != nil {
		return nil, err
	}
	return newRetentionCursor(ctx, sources, floor, mergeLimits{MaxBytes: limits.ReadMergeBytes, MaxKeyBytes: limits.KeyBytes})
}
