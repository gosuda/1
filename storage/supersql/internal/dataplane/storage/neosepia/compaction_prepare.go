package neosepia

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
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

type compactionWriter struct {
	target storage.ObjectRef
	name   string
	file   *os.File
	writer *sstWriter
	rows   uint64
	tier   runTier
	window uint64
}

type compactedRuns struct {
	root          *os.Root
	taskID        string
	limits        runPreparationLimits
	files         []preparedRun
	writers       []compactionWriter
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
	fixed := uint64(64 << 10)
	fixed += uint64(limits.MaxBuilders) * uint64(unsafe.Sizeof(compactionWriter{}))
	if fixed > limits.MetadataBytes {
		return nil, columnar.ErrBatchBudget
	}
	return &compactedRuns{root: root, taskID: taskID, limits: limits, writers: make([]compactionWriter, 0, limits.MaxBuilders), scratch: make([]byte, 64<<10), metadataBytes: fixed}, nil
}

func (runs *compactedRuns) appendTierBlock(ctx context.Context, target storage.ObjectRef, data []byte, rows uint64, tier runTier, window uint64, historyUntil model.Timestamp) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateScanTarget(runs.target, target); err != nil {
		return err
	}
	target = runs.target
	index := -1
	for i := range runs.writers {
		if runs.writers[i].writer == nil {
			continue
		}
		if runs.writers[i].target == target && runs.writers[i].tier == tier {
			index = i
			break
		}
	}
	if index < 0 {
		for i := range runs.writers {
			if runs.writers[i].writer == nil {
				index = i
				break
			}
		}
		if index < 0 && len(runs.writers) == cap(runs.writers) {
			return columnar.ErrBatchBudget
		}
		writer, err := runs.openWriter(target, tier, window)
		if err != nil {
			return err
		}
		if index < 0 {
			runs.writers = append(runs.writers, writer)
			index = len(runs.writers) - 1
		} else {
			runs.writers[index] = writer
		}
	}
	active := &runs.writers[index]
	if active.window != window {
		if err := runs.finishWriter(ctx, index); err != nil {
			return err
		}
		writer, err := runs.openWriter(target, tier, window)
		if err != nil {
			return err
		}
		runs.writers[index] = writer
		active = &runs.writers[index]
	}
	err := active.writer.Append(ctx, data)
	if errors.Is(err, columnar.ErrBatchBudget) && active.rows != 0 {
		if err := runs.finishWriter(ctx, index); err != nil {
			return err
		}
		writer, err := runs.openWriter(target, tier, window)
		if err != nil {
			return err
		}
		runs.writers[index] = writer
		active = &runs.writers[index]
		err = active.writer.Append(ctx, data)
	}
	if err != nil {
		return err
	}
	active.rows += rows
	active.writer.historyUntil = max(active.writer.historyUntil, historyUntil)
	return nil
}

func (runs *compactedRuns) openWriter(target storage.ObjectRef, tier runTier, window uint64) (compactionWriter, error) {
	activeWriters := 0
	for i := range runs.writers {
		if runs.writers[i].writer != nil {
			activeWriters++
		}
	}
	if uint64(len(runs.files)+activeWriters) >= runs.limits.MaxRuns {
		return compactionWriter{}, columnar.ErrBatchBudget
	}
	suffix := "active"
	if tier == inactiveTier {
		suffix = "inactive"
	}
	name := fmt.Sprintf("c-%s-%08x.%s.sst", runs.taskID[:min(16, len(runs.taskID))], len(runs.files)+activeWriters, suffix)
	if !validBaseRunName(name) {
		return compactionWriter{}, storage.ErrInvalidTarget
	}
	file, err := runs.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return compactionWriter{}, err
	}
	writer, err := newSSTWriter(file, target, runs.limits.File)
	if err != nil {
		return compactionWriter{}, errors.Join(err, file.Close(), runs.root.Remove(name))
	}
	writer.tier = tier
	writer.timeWindow = window
	if tier == activeTier {
		writer.level = runs.level
	}
	return compactionWriter{target: target, name: name, file: file, writer: writer, tier: tier, window: window}, nil
}

func (runs *compactedRuns) finishWriter(ctx context.Context, index int) (err error) {
	active := &runs.writers[index]
	if active.writer == nil {
		return nil
	}
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
		if err != nil {
			*active = compactionWriter{}
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
	*active = compactionWriter{}
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
	for i := range runs.writers {
		if runs.writers[i].writer != nil {
			if err := runs.finishWriter(ctx, i); err != nil {
				return err
			}
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
	for i := range runs.writers {
		if runs.writers[i].file != nil {
			err = errors.Join(err, runs.writers[i].file.Close())
			runs.writers[i].file = nil
		}
		if runs.writers[i].name != "" {
			removeErr := runs.root.Remove(runs.writers[i].name)
			if !errors.Is(removeErr, os.ErrNotExist) {
				err = errors.Join(err, removeErr)
			}
		}
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
