package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Each key's versions still run newest first. Only the predecessor position and
// one owned key survive between groups; history does not accumulate in memory.
type reverseRunState struct {
	first, nextBlock, nextRow int
	lastBlock, lastRow        int
	rangeIndex                int
	key                       []byte
	group                     *runVersionCursor
	groupCursor               runVersionCursor
	point                     versionRow
	hasPoint                  bool
	hasKey                    bool
}

func reverseRunMemory(limits mergeLimits) (uint64, error) {
	fixed := uint64(unsafe.Sizeof(reverseRunState{})) + uint64(unsafe.Sizeof(runVersionCursor{}))
	if fixed > limits.MaxBytes || limits.MaxKeyBytes > limits.MaxBytes-fixed || limits.MaxKeyBytes > uint64(math.MaxInt) {
		return 0, columnar.ErrBatchBudget
	}
	return fixed + limits.MaxKeyBytes, nil
}

func newReverseRunVersionCursor(run *sstFile, first, end int, maxBlockBytes uint64, limits mergeLimits, memory *sourceMemory) (*runVersionCursor, error) {
	if _, err := reverseRunMemory(limits); err != nil {
		return nil, err
	}
	cursor, err := newRunVersionCursor(run, first, end, maxBlockBytes, memory)
	if err != nil {
		return nil, err
	}
	cursor.reverse = &reverseRunState{first: cursor.nextBlock, nextBlock: cursor.endBlock - 1, nextRow: -1, key: make([]byte, 0, int(limits.MaxKeyBytes))}
	return cursor, nil
}

func (cursor *runVersionCursor) nextReverse(ctx context.Context) (versionRow, error) {
	state := cursor.reverse
	for state.rangeIndex < cursor.run.rangeCount && !cursor.run.containsKey(cursor.run.key(cursor.run.rangeEntry(state.rangeIndex).start)) {
		if err := ctx.Err(); err != nil {
			return versionRow{}, err
		}
		state.rangeIndex++
	}
	if !state.hasPoint {
		point, err := cursor.nextReversePoint(ctx)
		if err != nil && err != io.EOF {
			return versionRow{}, err
		}
		state.point, state.hasPoint = point, err == nil
	}
	if state.rangeIndex < cursor.run.rangeCount {
		entry := cursor.run.rangeEntry(state.rangeIndex)
		end := cursor.run.key(entry.end)
		// A range ending at a point's key becomes active after that point.
		if !state.hasPoint || bytes.Compare(end, state.point.key) > 0 {
			state.rangeIndex++
			return versionRow{key: cursor.run.key(entry.start), end: end, timestamp: entry.timestamp, ordinal: entry.ordinal, kind: storage.MutationDeleteRange}, nil
		}
	}
	if !state.hasPoint {
		return versionRow{}, io.EOF
	}
	point := state.point
	state.point = versionRow{}
	state.hasPoint = false
	return point, nil
}

func (cursor *runVersionCursor) nextReversePoint(ctx context.Context) (versionRow, error) {
	state := cursor.reverse
	if cursor.run.rows == uint64(cursor.run.rangeCount) {
		return versionRow{}, io.EOF
	}
	for {
		if err := ctx.Err(); err != nil {
			return versionRow{}, err
		}
		if state.group == nil {
			for state.nextBlock >= state.first && cursor.snapshotTS > 0 && cursor.run.entry(state.nextBlock).minCommitTS > cursor.snapshotTS {
				if err := ctx.Err(); err != nil {
					return versionRow{}, err
				}
				cursor.memory.observe(storage.ScanObservation{BlocksPruned: storage.ObservationCount{Value: 1, Known: true}})
				state.nextBlock--
				state.nextRow = -1
			}
			if state.nextBlock < state.first {
				return versionRow{}, io.EOF
			}
			if err := cursor.openReverseGroup(ctx); err != nil {
				return versionRow{}, err
			}
		}
		if !cursor.run.containsKey(state.key) {
			if err := state.group.Close(); err != nil {
				return versionRow{}, err
			}
			state.group = nil
			continue
		}
		if state.group.nextBlock-1 == state.lastBlock && state.group.row == state.lastRow {
			if state.lastBlock == state.nextBlock {
				cursor.lease = state.group.lease.retain()
			}
			if err := state.group.Close(); err != nil {
				return versionRow{}, err
			}
			state.group = nil
			continue
		}
		point, err := state.group.Next(ctx)
		if err != nil {
			if err == io.EOF {
				return versionRow{}, storage.ErrCorruptStorage
			}
			return versionRow{}, err
		}
		if !bytes.Equal(point.key, state.key) {
			return versionRow{}, storage.ErrCorruptStorage
		}
		if point.kind != storage.MutationDeleteRange {
			return point, nil
		}
	}
}

func (cursor *runVersionCursor) openReverseGroup(ctx context.Context) (err error) {
	state := cursor.reverse
	last := state.nextBlock
	lease := cursor.lease
	cursor.lease = nil
	if lease == nil {
		lease, err = cursor.readLease(ctx, last)
		if err != nil {
			return err
		}
	}
	defer func() {
		if lease != nil {
			err = errors.Join(err, lease.release())
		}
	}()
	block := lease.block
	row := state.nextRow
	if row < 0 {
		row = block.Rows() - 1
	}
	key, _, _, _, _, err := block.Version(row)
	if err != nil {
		return err
	}
	if len(key) > cap(state.key) {
		return columnar.ErrBatchBudget
	}
	if state.hasKey && bytes.Compare(key, state.key) >= 0 {
		return storage.ErrCorruptStorage
	}
	state.key = append(state.key[:0], key...)
	state.hasKey = true
	first := cursor.reverseFirstBlock(state.key, last)
	for first < last && cursor.snapshotTS > 0 && cursor.run.entry(first).minCommitTS > cursor.snapshotTS {
		if err := ctx.Err(); err != nil {
			return err
		}
		cursor.memory.observe(storage.ScanObservation{BlocksPruned: storage.ObservationCount{Value: 1, Known: true}})
		first++
	}
	if first > last {
		return storage.ErrCorruptStorage
	}
	if first != last {
		earlier, readErr := cursor.readLease(ctx, first)
		if readErr != nil {
			return readErr
		}
		closeErr := lease.release()
		lease = earlier
		block = earlier.block
		if closeErr != nil {
			return closeErr
		}
	}
	firstRow, err := reverseFirstRow(block, state.key)
	if err != nil {
		return err
	}
	state.nextBlock, state.nextRow = first, firstRow-1
	if firstRow == 0 {
		state.nextBlock--
	}
	state.lastBlock, state.lastRow = last, row+1
	state.groupCursor = runVersionCursor{run: cursor.run, nextBlock: first + 1, endBlock: last + 1, maxBytes: cursor.maxBytes, memory: cursor.memory, lease: lease, row: firstRow, snapshotTS: cursor.snapshotTS}
	state.group = &state.groupCursor
	lease = nil
	return nil
}

func (cursor *runVersionCursor) reverseFirstBlock(key []byte, last int) int {
	low, high := cursor.reverse.first, last+1
	for low < high {
		mid := low + (high-low)/2
		if bytes.Compare(cursor.run.key(cursor.run.entry(mid).maximum), key) < 0 {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low
}

func reverseFirstRow(block *paxBlock, key []byte) (int, error) {
	low, high := 0, block.Rows()
	for low < high {
		mid := low + (high-low)/2
		candidate, _, _, _, _, err := block.Version(mid)
		if err != nil {
			return 0, err
		}
		if bytes.Compare(candidate, key) < 0 {
			low = mid + 1
		} else {
			high = mid
		}
	}
	if low == block.Rows() {
		return 0, storage.ErrCorruptStorage
	}
	candidate, _, _, _, _, err := block.Version(low)
	if err != nil {
		return 0, err
	}
	if !bytes.Equal(candidate, key) {
		return 0, storage.ErrCorruptStorage
	}
	return low, nil
}
