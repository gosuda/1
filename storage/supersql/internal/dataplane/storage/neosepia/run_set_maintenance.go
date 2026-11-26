package neosepia

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (pin *runPin) maintenanceTargets(ctx context.Context) ([]storage.ObjectRef, error) {
	if pin == nil || pin.closed {
		return nil, storage.ErrClosed
	}
	targets := make(map[objectGeneration]storage.ObjectRef)
	for generation := pin.generation; generation != nil; generation = generation.parent {
		for _, file := range generation.files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			key := targetGeneration(file.target)
			if existing, exists := targets[key]; !exists || file.target.DescriptorVersion > existing.DescriptorVersion {
				targets[key] = file.target
			}
		}
	}
	result := make([]storage.ObjectRef, 0, len(targets))
	for _, target := range targets {
		result = append(result, target)
	}
	slices.SortFunc(result, func(a, b storage.ObjectRef) int {
		if a.ObjectID != b.ObjectID {
			if a.ObjectID < b.ObjectID {
				return -1
			}
			return 1
		}
		if a.Generation < b.Generation {
			return -1
		}
		if a.Generation > b.Generation {
			return 1
		}
		return 0
	})
	return result, nil
}

// Only named inputs are replaced; commits after capture remain additive.
func (set *runSet) replacementDescriptors(ctx context.Context, capturedPosition uint64, compacted map[string]struct{}, replacements []preparedRun) ([]preparedRun, error) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return nil, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	found := false
	for generation := set.head; generation != nil; generation = generation.parent {
		if generation.position == capturedPosition {
			found = true
			break
		}
	}
	if !found {
		return nil, storage.ErrInvalidTarget
	}
	if set.files > uint64(math.MaxInt-len(replacements)) {
		return nil, columnar.ErrBatchBudget
	}
	result := make([]preparedRun, 0, len(replacements)+int(set.files))
	names := make(map[string]struct{}, cap(result))
	appendDescriptor := func(descriptor preparedRun) error {
		if !validBaseRunName(descriptor.Name) || descriptor.Rows == 0 || descriptor.Bytes == 0 {
			return storage.ErrCorruptStorage
		}
		if _, exists := names[descriptor.Name]; exists {
			return storage.ErrCorruptStorage
		}
		names[descriptor.Name] = struct{}{}
		result = append(result, descriptor)
		return nil
	}
	for _, descriptor := range replacements {
		if err := appendDescriptor(descriptor); err != nil {
			return nil, err
		}
	}
	for generation := set.head; generation != nil; generation = generation.parent {
		for _, file := range generation.files {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if generation.position <= capturedPosition {
				if _, replaced := compacted[file.descriptor.Name]; replaced {
					continue
				}
			}
			if err := appendDescriptor(file.descriptor); err != nil {
				return nil, err
			}
		}
	}
	slices.SortFunc(result, func(a, b preparedRun) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}

func (set *runSet) checkReplacement(ctx context.Context, position uint64, timestamp model.Timestamp, files []*sstFile) error {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed {
		return storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if set.head == nil || position != set.head.position || timestamp != set.head.timestamp {
		return storage.ErrInvalidMutation
	}
	var reclaimBytes, reclaimFiles uint64
	for generation := set.head; generation != nil && generation.references == 1; generation = generation.parent {
		reclaimBytes += generation.bytes
		reclaimFiles += uint64(len(generation.files))
	}
	residentBytes := set.bytes - reclaimBytes
	residentFiles := set.files - reclaimFiles
	if uint64(len(files)) > set.limits.MaxFiles-residentFiles {
		return columnar.ErrBatchBudget
	}
	remaining := set.limits.MaxBytes - residentBytes
	bytes := uint64(unsafe.Sizeof(runGeneration{})) + uint64(cap(files))*uint64(unsafe.Sizeof((*sstFile)(nil)))
	if bytes > remaining {
		return columnar.ErrBatchBudget
	}
	for _, file := range files {
		if file == nil {
			return storage.ErrCorruptStorage
		}
		file.mu.RLock()
		closed := file.file == nil
		cost := uint64(unsafe.Sizeof(sstFile{})) + uint64(cap(file.index)) + uint64(cap(file.descriptor.LowerKey)+cap(file.descriptor.UpperKey))
		file.mu.RUnlock()
		if closed || cost > remaining-bytes {
			return columnar.ErrBatchBudget
		}
		bytes += cost
	}
	generation := runGeneration{files: files}
	generation.indexTiers()
	return generation.validateLevels(ctx)
}

func (set *runSet) hasBase(generation uint64) bool {
	set.mu.Lock()
	defer set.mu.Unlock()
	return set.head != nil && set.head.baseGeneration == generation
}

func (set *runSet) installRecoveredBase(ctx context.Context, base runBase, rootFiles []*sstFile) error {
	set.mu.Lock()
	head := set.head
	set.mu.Unlock()
	if head == nil {
		return set.publishBase(ctx, base.Position, base.CommitTS, base.Generation, rootFiles)
	}
	return set.replaceLevelRange(ctx, base.Position, base.CommitTS, base.Generation, rootFiles)
}

func closeRunFiles(files []*sstFile) error {
	var err error
	for _, file := range files {
		if file != nil {
			err = errors.Join(err, file.Close())
		}
	}
	return err
}

func (set *runSet) replaceLevelRange(ctx context.Context, position uint64, timestamp model.Timestamp, baseGeneration uint64, files []*sstFile) (err error) {
	clear(files[len(files):cap(files)])
	set.mu.Lock()
	defer set.mu.Unlock()
	published := false
	defer func() {
		if err != nil && !published {
			for _, file := range files {
				if file != nil {
					err = errors.Join(err, file.Close())
				}
			}
		}
	}()
	if set.closed {
		return storage.ErrClosed
	}
	if set.reservation != nil {
		return storage.ErrCommitIndeterminate
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if position == 0 || timestamp <= 0 || baseGeneration == 0 {
		return storage.ErrInvalidMutation
	}
	if set.head == nil || position != set.head.position || timestamp != set.head.timestamp {
		return storage.ErrInvalidMutation
	}
	var reclaimBytes, reclaimFiles uint64
	for generation := set.head; generation != nil && generation.references == 1; generation = generation.parent {
		reclaimBytes += generation.bytes
		reclaimFiles += uint64(len(generation.files))
	}
	residentBytes := set.bytes - reclaimBytes
	residentFiles := set.files - reclaimFiles
	if uint64(len(files)) > set.limits.MaxFiles-residentFiles {
		return columnar.ErrBatchBudget
	}
	remaining := set.limits.MaxBytes - residentBytes
	bytes := uint64(unsafe.Sizeof(runGeneration{}))
	pointers := uint64(unsafe.Sizeof((*sstFile)(nil)))
	if bytes > remaining || uint64(cap(files)) > (remaining-bytes)/pointers {
		return columnar.ErrBatchBudget
	}
	bytes += uint64(cap(files)) * pointers
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if file == nil {
			return storage.ErrCorruptStorage
		}
		file.mu.RLock()
		closed := file.file == nil
		cost := uint64(unsafe.Sizeof(sstFile{})) + uint64(cap(file.index)) + uint64(cap(file.descriptor.LowerKey)+cap(file.descriptor.UpperKey))
		file.mu.RUnlock()
		if closed || cost > remaining-bytes {
			return columnar.ErrBatchBudget
		}
		bytes += cost
	}
	next := &runGeneration{files: files, position: position, timestamp: timestamp, baseGeneration: baseGeneration, references: 1, bytes: bytes}
	next.indexTiers()
	if err := next.validateLevels(ctx); err != nil {
		return err
	}
	active := make(map[string]struct{}, len(files))
	for _, file := range files {
		active[file.descriptor.Name] = struct{}{}
	}
	old := set.head
	for generation := old; generation != nil; generation = generation.parent {
		for _, file := range generation.files {
			if _, retained := active[file.descriptor.Name]; !retained {
				file.markReclaim()
			}
		}
	}
	set.head = next
	set.bytes += bytes
	published = true
	set.files += uint64(len(files))
	return set.releaseLocked(old)
}
