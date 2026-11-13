package neosepia

import (
	"bytes"
	"context"
	"slices"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type runTier uint8

const (
	activeTier runTier = iota
	inactiveTier
	MaxLevels = 8
	MaxL0Files = 4
	compactionChunkBytes uint64 = 64 << 20
	compactionFileBytes uint64 = 16 << 20
	inactiveWindowWidth = uint64(time.Hour)
)

// All indexes borrow an ordering slice owned by the generation. generation.files
// keeps manifest order: callers such as checkpoint capture and digest rely on it.
func (generation *runGeneration) indexTiers() {
	ordered := slices.Clone(generation.files)
	slices.SortFunc(ordered, func(a, b *sstFile) int {
		if a.tier != b.tier {
			return int(a.tier) - int(b.tier)
		}
		if a.level != b.level {
			return int(a.level) - int(b.level)
		}
		if a.target.ObjectID != b.target.ObjectID {
			if a.target.ObjectID < b.target.ObjectID {
				return -1
			}
			return 1
		}
		if a.target.Generation != b.target.Generation {
			if a.target.Generation < b.target.Generation {
				return -1
			}
			return 1
		}
		return bytes.Compare(a.lowerBound(), b.lowerBound())
	})
	end := 0
	for end < len(ordered) && ordered[end].tier == activeTier {
		end++
	}
	generation.activeFiles = ordered[:end:end]
	generation.inactiveFiles = ordered[end:len(ordered):len(ordered)]
	first := 0
	for level := range MaxLevels {
		end := first
		for end < len(generation.activeFiles) && generation.activeFiles[end].level == uint8(level) {
			end++
		}
		generation.levels[level] = generation.activeFiles[first:end:end]
		first = end
	}
}

func (pin *runPin) visitSnapshot(ctx context.Context, target storage.ObjectRef, timestamp model.Timestamp, visit func(*sstFile) error) error {
	return pin.visit(ctx, target, func(file *sstFile) error {
		// A fixed strong snapshot opened after a later compaction still needs
		// history. SQL syntax cannot decide which physical tier is visible.
		if file.tier == inactiveTier && timestamp >= file.historyUntil {
			return nil
		}
		return visit(file)
	})
}

func (generation *runGeneration) validateLevels(ctx context.Context) error {
	for level := 1; level < MaxLevels; level++ {
		var previous *sstFile
		for _, file := range generation.levels[level] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if previous != nil && targetGeneration(previous.target) == targetGeneration(file.target) && !previous.endsBefore(file.lowerBound()) {
				return storage.ErrCorruptStorage
			}
			previous = file
		}
	}
	return nil
}
