package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Inputs need not cover the whole table. Point and range tombstones stay active
// so an omitted lower level cannot resurrect a deleted key.
func appendTieredBlocks(ctx context.Context, source *mvccCursor, floor model.Timestamp, limits blockLimits, maxKeyBytes uint64, output *compactedRuns) (err error) {
	var lease *blockLease
	defer func() {
		if lease != nil { err = errors.Join(err, lease.release()) }
		err = errors.Join(err, source.Close())
	}()
	if maxKeyBytes > limits.MaxDecodedBytes || uint64(limits.MaxRows) > (limits.MaxDecodedBytes-maxKeyBytes)/4 {
		return columnar.ErrBatchBudget
	}
	limits.MaxDecodedBytes -= maxKeyBytes + uint64(limits.MaxRows)*4
	key := make([]byte, 0, int(maxKeyBytes))
	order := make([]uint32, 0, limits.MaxRows)
	var haveKey bool
	var newer, historyUntil model.Timestamp
	var tier runTier
	var window uint64
	var emit func([]uint32) error
	emit = func(selected []uint32) error {
		encoded, err := rewriteBlock(ctx, lease.block, selected, limits)
		if errors.Is(err, columnar.ErrBatchBudget) && len(selected) > 1 {
			middle := len(selected)/2
			if err := emit(selected[:middle]); err != nil { return err }
			return emit(selected[middle:])
		}
		if err != nil { return err }
		return output.appendTierBlock(ctx, lease.block.Target(), encoded, uint64(len(selected)), tier, window, historyUntil)
	}
	flush := func() error {
		if lease == nil { return nil }
		if err := emit(order); err != nil { return err }
		err := lease.release()
		lease = nil
		order = order[:0]
		historyUntil = 0
		return err
	}
	for {
		row, nextErr := source.Next(ctx)
		if nextErr == io.EOF { return flush() }
		if nextErr != nil { return nextErr }
		rowTier, rowWindow, until := activeTier, uint64(0), model.Timestamp(0)
		if row.kind == storage.MutationDeleteRange {
			if source.ranges.suppresses(row.timestamp, row.ordinal) {
				covering := source.ranges.entries[source.ranges.heaps[1].slots[0]]
				if bytes.Compare(covering.end, row.end) >= 0 {
					until = covering.timestamp
				}
			}
			if until == 0 {
				if err := source.ranges.add(row.end, row.timestamp, row.ordinal); err != nil { return err }
			}
		} else {
			if haveKey && bytes.Equal(key, row.key) {
				until = newer
				newer = row.timestamp
			} else {
				if uint64(len(row.key)) > maxKeyBytes { return columnar.ErrBatchBudget }
				key = append(key[:0], row.key...)
				haveKey, newer = true, row.timestamp
			}
			if source.ranges.suppresses(row.timestamp, row.ordinal) {
				until = max(until, source.ranges.entries[source.ranges.heaps[1].slots[0]].timestamp)
			}
		}
		if until != 0 {
			if until <= floor || until == row.timestamp { continue }
			rowTier = inactiveTier
			rowWindow = uint64(row.timestamp)/inactiveWindowWidth
		}
		if lease != row.lease || len(order) == cap(order) || tier != rowTier || window != rowWindow {
			if err := flush(); err != nil { return err }
		}
		if lease == nil {
			lease = row.lease.retain()
			tier, window = rowTier, rowWindow
		}
		historyUntil = max(historyUntil, until)
		order = append(order, uint32(row.row))
	}
}
