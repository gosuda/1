package neosepia

import (
	"bytes"
	"context"
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/columnar"
)

// Retained fragments keep their source's level. Their bounds become occupied
// intervals so a later interval in this pass cannot overlap a pinned fragment.
func (runs *compactedRuns) retainFragments(ctx context.Context, work levelCompaction) ([]emittedInterval, error) {
	var occupied []emittedInterval
	for _, file := range work.files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if bytes.Compare(file.lowerBound(), work.lower) < 0 {
			if err := runs.linkFragment(file, file.descriptor.LowerKey, work.lower); err != nil {
				return nil, err
			}
			if file.level != 0 {
				occupied = append(occupied, emittedInterval{level: file.level, lower: file.descriptor.LowerKey, upper: bytes.Clone(work.lower)})
			}
		}
		if !file.endsBefore(work.upper) {
			if err := runs.linkFragment(file, work.upper, file.descriptor.UpperKey); err != nil {
				return nil, err
			}
			if file.level != 0 {
				occupied = append(occupied, emittedInterval{level: file.level, lower: bytes.Clone(work.upper), upper: file.descriptor.UpperKey})
			}
		}
	}
	return occupied, nil
}

func (runs *compactedRuns) linkFragment(file *sstFile, lower, upper []byte) error {
	if uint64(len(runs.files)) >= runs.limits.MaxRuns {
		return columnar.ErrBatchBudget
	}
	name := fmt.Sprintf("f-%s-%08x.active.sst", runs.taskID[:min(16, len(runs.taskID))], len(runs.files))
	if err := runs.reserveFile(name); err != nil {
		return err
	}
	// The map node and both owned bounds remain charged until publication.
	cost := uint64(len(lower)+len(upper)) + 128
	if cost > runs.limits.MetadataBytes-runs.metadataBytes {
		return columnar.ErrBatchBudget
	}
	if err := runs.root.Link(file.descriptor.Name, name); err != nil {
		return err
	}
	descriptor := file.descriptor
	descriptor.Name = name
	descriptor.LowerKey, descriptor.UpperKey = bytes.Clone(lower), bytes.Clone(upper)
	runs.files = append(runs.files, descriptor)
	if runs.fragments == nil {
		runs.fragments = make(map[string]*sstFile)
	}
	runs.fragments[name] = file
	runs.metadataBytes += cost
	return nil
}
