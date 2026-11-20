package neosepia

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Unchanged immutable files already passed their whole-file digest check at
// admission. Retain their handles instead of rereading every table's payload.
func (set *runSet) openReplacementFiles(ctx context.Context, root *os.Root, descriptors []preparedRun, fragments map[string]*sstFile, limits sstLimits, maxBytes uint64) (_ []*sstFile, err error) {
	set.mu.Lock()
	defer set.mu.Unlock()
	if set.closed { return nil, storage.ErrClosed }
	const fixedScratchBytes = uint64(64 << 10)
	entryBytes := uint64(unsafe.Sizeof(preparedRun{})) + uint64(unsafe.Sizeof((*sstFile)(nil))) + 64
	entries := uint64(len(descriptors)) + set.files
	if entries > maxBytes/entryBytes {
		return nil, columnar.ErrBatchBudget
	}
	used := entries * entryBytes
	// The scratch buffer is a transient decode window, not resident data. Cap it
	// by half the remaining budget so a tight ResidentBytes limit cannot reject
	// every replacement before the caller learns its durable outcome.
	scratchBytes := min(fixedScratchBytes, (maxBytes-used)/2)
	if scratchBytes == 0 {
		return nil, columnar.ErrBatchBudget
	}
	used += scratchBytes
	current := make(map[string]*sstFile)
	for generation := set.head; generation != nil; generation = generation.parent {
		for _, file := range generation.files {
			if err := ctx.Err(); err != nil { return nil, err }
			current[file.descriptor.Name] = file
		}
	}
	opened := make([]*sstFile, 0, len(descriptors))
	defer func() {
		if err != nil { err = errors.Join(err, closeRunFiles(opened)) }
	}()
	scratch := make([]byte, scratchBytes)
	for _, descriptor := range descriptors {
		if err := ctx.Err(); err != nil { return nil, err }
		if file, exists := current[descriptor.Name]; exists {
			file.mu.Lock()
			if file.file == nil || !samePreparedRun(file.descriptor, descriptor) || file.references == math.MaxUint64 {
				file.mu.Unlock()
				return nil, storage.ErrCorruptStorage
			}
			file.references++
			file.mu.Unlock()
			opened = append(opened, file)
			continue
		}
		var file *sstFile
		if source, linked := fragments[descriptor.Name]; linked {
			if current[source.descriptor.Name] != source {
				return nil, storage.ErrCorruptStorage
			}
			file, err = openLinkedFragment(ctx, root, descriptor, source, limits, maxBytes-used)
		} else {
			file, err = openPreparedRun(ctx, root, descriptor, limits, maxBytes-used, scratch)
		}
		if err != nil { return nil, err }
		opened = append(opened, file)
		cost := uint64(unsafe.Sizeof(sstFile{})) + uint64(cap(file.index)) + uint64(cap(file.descriptor.LowerKey)+cap(file.descriptor.UpperKey))
		if cost > maxBytes-used { return nil, columnar.ErrBatchBudget }
		used += cost
	}
	return opened, nil
}

func openLinkedFragment(ctx context.Context, root *os.Root, descriptor preparedRun, source *sstFile, limits sstLimits, maxBytes uint64) (_ *sstFile, err error) {
	file, err := root.Open(descriptor.Name)
	if err != nil { return nil, err }
	owned := true
	defer func() { if owned { err = errors.Join(err, file.Close()) } }()
	info, err := file.Stat()
	if err != nil { return nil, err }
	original, err := source.file.Stat()
	if err != nil { return nil, err }
	if !os.SameFile(info, original) || descriptor.Checksum != source.descriptor.Checksum || descriptor.Target != source.target || descriptor.Bytes != source.descriptor.Bytes || descriptor.Rows != source.rows {
		return nil, storage.ErrCorruptStorage
	}
	limits.MaxIndexBytes = min(limits.MaxIndexBytes, maxBytes)
	run, err := openSST(ctx, file, limits)
	if err != nil { return nil, err }
	owned = false
	run.descriptor = descriptor
	run.physicalPath, err = filepath.Abs(filepath.Join(root.Name(), descriptor.Name))
	if err != nil { return nil, errors.Join(err, run.Close()) }
	return run, nil
}
