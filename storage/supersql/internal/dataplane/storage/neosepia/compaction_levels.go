package neosepia

import (
	"bytes"
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type levelCompaction struct {
	target       storage.ObjectRef
	level        uint8
	files        map[string]*sstFile
	lower, upper []byte
}

// emittedInterval records a half-open key range already occupied at a level by
// this maintenance pass. Compaction outputs and retained fragments share one
// publication, so later intervals must not overlap earlier occupants at levels
// that require disjoint coverage.
type emittedInterval struct {
	level        uint8
	lower, upper []byte
}

// Empty bounds are unbounded; stored work intervals always carry finite keys.
func (interval emittedInterval) overlaps(lower, upper []byte) bool {
	return (len(interval.lower) == 0 || bytes.Compare(interval.lower, upper) < 0) && (len(interval.upper) == 0 || bytes.Compare(lower, interval.upper) < 0)
}

func activeLevelBudget(level uint8) uint64 {
	budget := uint64(10 << 20)
	for range level {
		budget *= 10
	}
	return budget
}

// selectTargetCompaction coalesces all remaining files of a target into a
// single level-1 interval when the target has uncompacted L0 runs, multiple
// active runs, or multiple inactive runs in the same time window and its total
// size fits within the single-pass compaction and merge budgets.
func (pin *runPin) selectTargetCompaction(ctx context.Context, target storage.ObjectRef, exclude map[string]struct{}, limits Limits) (levelCompaction, error) {
	var sizes [MaxLevels]uint64
	var l0Count, activeCount uint64
	var totalSize uint64
	var minLower, maxUpper []byte
	var windowCollision bool
	windows := make(map[uint64]struct{})
	files := make(map[string]*sstFile)
	if err := pin.visit(ctx, target, func(file *sstFile) error {
		if _, done := exclude[file.descriptor.Name]; done {
			return nil
		}
		first, end := file.fragmentBlocks(0, file.count)
		var size uint64
		for i := first; i < end; i++ {
			size += file.entry(i).length
		}
		if size > math.MaxUint64-totalSize {
			return columnar.ErrBatchBudget
		}
		totalSize += size
		if file.tier == activeTier {
			if size > math.MaxUint64-sizes[file.level] {
				return columnar.ErrBatchBudget
			}
			sizes[file.level] += size
			activeCount++
			if file.level == 0 {
				l0Count++
			}
		} else {
			if _, exists := windows[file.timeWindow]; exists {
				windowCollision = true
			} else {
				windows[file.timeWindow] = struct{}{}
			}
		}
		lower := file.lowerBound()
		if minLower == nil || bytes.Compare(lower, minLower) < 0 {
			minLower = bytes.Clone(lower)
		}
		upper := file.upperBound()
		if maxUpper == nil || bytes.Compare(upper, maxUpper) > 0 {
			maxUpper = upper
		}
		files[file.descriptor.Name] = file
		return nil
	}); err != nil {
		return levelCompaction{}, err
	}
	needsCompaction := l0Count > 0 || activeCount > 1 || windowCollision
	if !needsCompaction {
		for candidate := 1; candidate < MaxLevels-1; candidate++ {
			if sizes[candidate] > activeLevelBudget(uint8(candidate)) {
				needsCompaction = true
				break
			}
		}
	}
	if !needsCompaction {
		return levelCompaction{}, nil
	}
	count := uint64(len(files))
	ledgerBytes := uint64(unsafe.Sizeof(sourceMemory{}))
	perSource := uint64(unsafe.Sizeof(runVersionCursor{})) + uint64(unsafe.Sizeof((*runVersionCursor)(nil)))
	perBlock := uint64(unsafe.Sizeof(paxBlock{})) + uint64(unsafe.Sizeof(blockLease{}))
	mergeFixed := uint64(unsafe.Sizeof(mvccCursor{})) + count*(uint64(unsafe.Sizeof(mergeHead{}))+4)
	if totalSize <= compactionChunkBytes &&
		count > 0 &&
		ledgerBytes < limits.ReadSourceBytes &&
		count < (limits.ReadSourceBytes-ledgerBytes)/(perSource+perBlock*2) &&
		totalSize <= limits.ReadSourceBytes-ledgerBytes-count*(perSource+perBlock*2) &&
		mergeFixed < limits.ReadMergeBytes &&
		limits.KeyBytes <= limits.ReadMergeBytes-mergeFixed {
		return levelCompaction{
			target: target,
			level:  1,
			files:  files,
			lower:  minLower,
			upper:  maxUpper,
		}, nil
	}
	return pin.selectLevelCompaction(ctx, target, true, exclude)
}

// selectLevelCompaction chooses the next key interval to rewrite for a target.
// Names in exclude were already rewritten or dropped by this maintenance pass,
// so they neither seed nor join a later interval.
func (pin *runPin) selectLevelCompaction(ctx context.Context, target storage.ObjectRef, force bool, exclude map[string]struct{}) (levelCompaction, error) {
	var sizes [MaxLevels]uint64
	var counts [MaxLevels]uint64
	var oldest [MaxLevels]*sstFile
	if err := pin.visit(ctx, target, func(file *sstFile) error {
		if file.tier != activeTier {
			return nil
		}
		if _, done := exclude[file.descriptor.Name]; done {
			return nil
		}
		level := file.level
		first, end := file.fragmentBlocks(0, file.count)
		var size uint64
		for i := first; i < end; i++ {
			size += file.entry(i).length
		}
		if size > math.MaxUint64-sizes[level] {
			return columnar.ErrBatchBudget
		}
		sizes[level] += size
		counts[level]++
		if oldest[level] == nil || file.descriptor.Name < oldest[level].descriptor.Name {
			oldest[level] = file
		}
		return nil
	}); err != nil {
		return levelCompaction{}, err
	}
	level := -1
	if counts[0] >= MaxL0Files || (force && counts[0] != 0) {
		level = 0
	}
	if level < 0 {
		for candidate := 1; candidate < MaxLevels-1; candidate++ {
			if sizes[candidate] > activeLevelBudget(uint8(candidate)) {
				level = candidate
				break
			}
		}
	}
	if level < 0 {
		return levelCompaction{}, nil
	}
	seed := oldest[level]
	work := levelCompaction{target: target, level: uint8(level + 1), files: make(map[string]*sstFile), lower: bytes.Clone(seed.lowerBound()), upper: seed.upperBound()}
	var destination *sstFile
	if err := pin.visit(ctx, target, func(file *sstFile) error {
		if _, done := exclude[file.descriptor.Name]; done {
			return nil
		}
		if file.tier == activeTier && file.level == work.level && file.overlaps(work.lower, work.upper) && (destination == nil || bytes.Compare(file.lowerBound(), destination.lowerBound()) < 0) {
			destination = file
		}
		return nil
	}); err != nil {
		return levelCompaction{}, err
	}
	if destination != nil {
		upper := destination.upperBound()
		if bytes.Compare(upper, work.upper) < 0 {
			work.upper = upper
		}
	}
	// A sparse flush can span the entire database. Limit its contribution to
	// one indexed key interval; hardlinked residual fragments stay in L0.
	first, end := seed.fragmentBlocks(0, seed.count)
	if first < end {
		maximum := seed.key(seed.entry(first).maximum)
		upper := make([]byte, len(maximum)+1)
		copy(upper, maximum)
		if bytes.Compare(upper, work.upper) < 0 {
			work.upper = upper
		}
	}
	var total uint64
	add := func(file *sstFile) error {
		first, end := file.rangeBlocks(0, file.count, work.lower, work.upper)
		var size uint64
		for i := first; i < end; i++ {
			size += file.entry(i).length
		}
		if size > compactionChunkBytes-total {
			return columnar.ErrBatchBudget
		}
		total += size
		work.files[file.descriptor.Name] = file
		return nil
	}
	if err := add(seed); err != nil {
		return levelCompaction{}, err
	}
	if destination != nil && destination.overlaps(work.lower, work.upper) {
		if err := add(destination); err != nil {
			return levelCompaction{}, err
		}
	}
	if level == 0 {
		if err := pin.visit(ctx, target, func(file *sstFile) error {
			if len(work.files) >= MaxL0Files+1 || file.tier != activeTier || file.level != 0 || !file.overlaps(work.lower, work.upper) {
				return nil
			}
			if _, exists := work.files[file.descriptor.Name]; exists {
				return nil
			}
			if _, done := exclude[file.descriptor.Name]; done {
				return nil
			}
			first, end := file.rangeBlocks(0, file.count, work.lower, work.upper)
			var size uint64
			for i := first; i < end; i++ {
				size += file.entry(i).length
			}
			if size > compactionChunkBytes-total {
				return nil
			}
			return add(file)
		}); err != nil {
			return levelCompaction{}, err
		}
	}
	return work, nil
}
