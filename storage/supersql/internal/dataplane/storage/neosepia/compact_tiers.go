package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const maxTierBlockCoalescers = 16

type tierWindowKey struct {
	tier   runTier
	window uint64
}

// Inputs need not cover the whole table. Point and range tombstones stay active
// so an omitted lower level cannot resurrect a deleted key.
func appendTieredBlocks(ctx context.Context, source *mvccCursor, floor model.Timestamp, limits blockLimits, maxKeyBytes uint64, output *compactedRuns) (err error) {
	coalescers := make(map[tierWindowKey]*paxBlockCoalescer)
	defer func() {
		for key, c := range coalescers {
			err = errors.Join(err, c.Close())
			delete(coalescers, key)
		}
		err = errors.Join(err, source.Close())
	}()
	if maxKeyBytes > limits.MaxDecodedBytes || uint64(limits.MaxRows) > (limits.MaxDecodedBytes-maxKeyBytes)/4 {
		return columnar.ErrBatchBudget
	}
	limits.MaxDecodedBytes -= maxKeyBytes
	key := make([]byte, 0, int(maxKeyBytes))
	var haveKey bool
	var newer model.Timestamp
	var touchSeq uint64
	totalBudget := limits.MaxDecodedBytes / 2
	if totalBudget == 0 {
		totalBudget = limits.MaxDecodedBytes
	}
	perCoalescerBudget := limits.MaxDecodedBytes

	emitFor := func(tw tierWindowKey) coalescedBlockEmit {
		return func(target storage.ObjectRef, data []byte, blobs []toastBlob, rows uint64, historyUntil model.Timestamp) error {
			return output.appendTierBlockWithBlobs(ctx, target, data, blobs, rows, tw.tier, tw.window, historyUntil)
		}
	}

	evictOne := func(keep tierWindowKey, closeAfter bool) error {
		var victimKey tierWindowKey
		var victim *paxBlockCoalescer
		for k, c := range coalescers {
			if k == keep {
				continue
			}
			if closeAfter && c.rows() == 0 {
				victimKey, victim = k, c
				break
			}
			if !closeAfter && c.rows() == 0 {
				continue
			}
			if victim == nil || c.touch < victim.touch {
				victimKey, victim = k, c
			}
		}
		if victim == nil {
			return nil
		}
		if err := victim.flush(ctx, emitFor(victimKey)); err != nil {
			return err
		}
		if closeAfter {
			if err := victim.Close(); err != nil {
				return err
			}
			delete(coalescers, victimKey)
		}
		return nil
	}

	flushAll := func() error {
		if len(coalescers) == 0 {
			return nil
		}
		keys := make([]tierWindowKey, 0, len(coalescers))
		for k := range coalescers {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b tierWindowKey) int {
			if a.tier != b.tier {
				if a.tier < b.tier {
					return -1
				}
				return 1
			}
			if a.window < b.window {
				return -1
			}
			if a.window > b.window {
				return 1
			}
			return 0
		})
		for _, k := range keys {
			if err := coalescers[k].flush(ctx, emitFor(k)); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		row, nextErr := source.Next(ctx)
		if nextErr == io.EOF {
			return flushAll()
		}
		if nextErr != nil {
			return nextErr
		}
		rowTier, rowWindow, until := activeTier, uint64(0), model.Timestamp(0)
		if row.kind == storage.MutationDeleteRange {
			if source.ranges.suppresses(row.timestamp, row.ordinal) {
				covering := source.ranges.entries[source.ranges.heaps[1].slots[0]]
				if bytes.Compare(covering.end, row.end) >= 0 {
					until = covering.timestamp
				}
			}
			if until == 0 {
				if err := source.ranges.add(row.end, row.timestamp, row.ordinal); err != nil {
					return err
				}
			}
		} else {
			if haveKey && bytes.Equal(key, row.key) {
				until = newer
				newer = row.timestamp
			} else {
				if uint64(len(row.key)) > maxKeyBytes {
					return columnar.ErrBatchBudget
				}
				key = append(key[:0], row.key...)
				haveKey, newer = true, row.timestamp
			}
			if source.ranges.suppresses(row.timestamp, row.ordinal) {
				until = max(until, source.ranges.entries[source.ranges.heaps[1].slots[0]].timestamp)
			}
		}
		if until != 0 {
			if until <= floor || until == row.timestamp {
				continue
			}
			rowTier = inactiveTier
			rowWindow = uint64(row.timestamp) / inactiveWindowWidth
		}
		tw := tierWindowKey{tier: rowTier, window: rowWindow}
		coalescer := coalescers[tw]
		if coalescer == nil {
			if len(coalescers) >= maxTierBlockCoalescers {
				if err := evictOne(tw, true); err != nil {
					return err
				}
			}
			coalescer = newPAXBlockCoalescer(limits, perCoalescerBudget)
			coalescers[tw] = coalescer
		}
		if err := coalescer.appendRow(ctx, row.lease.block, row.row, row.key, row.end, row.timestamp, row.ordinal, row.kind, until, emitFor(tw)); err != nil {
			return err
		}
		touchSeq++
		coalescer.touch = touchSeq
		if len(coalescers) > 1 {
			var buffered uint64
			for _, c := range coalescers {
				buffered += c.arenaBytes()
			}
			for buffered > totalBudget && len(coalescers) > 1 {
				prev := buffered
				if err := evictOne(tw, true); err != nil {
					return err
				}
				buffered = 0
				for _, c := range coalescers {
					buffered += c.arenaBytes()
				}
				if buffered >= prev {
					break
				}
			}
		}
	}
}
