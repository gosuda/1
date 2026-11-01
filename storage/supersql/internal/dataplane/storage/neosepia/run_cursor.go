package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Leases are confined to one scan invocation. A selection can retain its block
// while the source advances, without copying column payloads or visibility keys.
type blockLease struct {
	block      *paxBlock
	references int
}

func (lease *blockLease) retain() *blockLease { lease.references++; return lease }
func (lease *blockLease) release() error {
	lease.references--
	if lease.references != 0 {
		return nil
	}
	block := lease.block
	lease.block = nil
	memory := block.memory
	err := block.Close()
	memory.release(uint64(unsafe.Sizeof(blockLease{})))
	return err
}

type versionRow struct {
	lease     *blockLease
	row       int
	key, end  []byte
	timestamp model.Timestamp
	ordinal   uint64
	kind      storage.MutationOp
}

// The run is borrowed and must remain pinned by the snapshot. Next borrows its
// row until Next/Close; retaining the lease extends only that block's lifetime.
// maxBytes is per block; boundary validation can hold two blocks. Shared source
// memory remains charged until the last lease releases each block.
type runVersionCursor struct {
	run                 *sstFile
	nextBlock, endBlock int
	maxBytes            uint64
	memory              *sourceMemory
	lease               *blockLease
	row                 int
	closed              bool
	reverse             *reverseRunState
	snapshotTS          model.Timestamp
	lowerKey, upperKey  []byte
	pointKey            []byte
	sequence            *runSequenceState
}

func newRunVersionCursor(run *sstFile, first, end int, maxBytes uint64, memory *sourceMemory) (*runVersionCursor, error) {
	if run == nil {
		return nil, storage.ErrInvalidScanPlan
	}
	run.mu.RLock()
	defer run.mu.RUnlock()
	if run.file == nil {
		return nil, storage.ErrClosed
	}
	if first < 0 || first > end || end > run.count {
		return nil, storage.ErrInvalidScanPlan
	}
	if maxBytes <= uint64(unsafe.Sizeof(blockLease{})) {
		return nil, columnar.ErrBatchBudget
	}
	first, end = run.fragmentBlocks(first, end)
	return &runVersionCursor{run: run, nextBlock: first, endBlock: end, maxBytes: maxBytes, memory: memory}, nil
}

func (cursor *runVersionCursor) Next(ctx context.Context) (_ versionRow, err error) {
	if cursor.closed {
		return versionRow{}, io.EOF
	}
	defer func() {
		if err != nil {
			err = iterationError(err, cursor.Close())
		}
	}()
	if err := ctx.Err(); err != nil {
		return versionRow{}, err
	}
	if cursor.sequence != nil {
		return cursor.nextSequence(ctx)
	}
	if cursor.reverse != nil {
		return cursor.nextReverse(ctx)
	}
	for {
		if err := ctx.Err(); err != nil {
			return versionRow{}, err
		}
		if cursor.lease == nil || cursor.row == cursor.lease.block.Rows() {
			if cursor.nextBlock == cursor.endBlock {
				return versionRow{}, io.EOF
			}
			if err := cursor.advanceBlock(ctx); err != nil {
				return versionRow{}, err
			}
		}
		key, end, timestamp, ordinal, kind, err := cursor.lease.block.Version(cursor.row)
		if err != nil {
			return versionRow{}, err
		}
		result := versionRow{lease: cursor.lease, row: cursor.row, key: key, end: end, timestamp: timestamp, ordinal: ordinal, kind: kind}
		cursor.row++
		if !cursor.run.containsKey(key) || (len(cursor.lowerKey) != 0 && bytes.Compare(key, cursor.lowerKey) < 0) || (len(cursor.upperKey) != 0 && bytes.Compare(key, cursor.upperKey) >= 0) {
			continue
		}
		return result, nil
	}
}

func (cursor *runVersionCursor) readLease(ctx context.Context, index int) (*blockLease, error) {
	header := uint64(unsafe.Sizeof(blockLease{}))
	if err := cursor.memory.reserve(header); err != nil {
		return nil, err
	}
	block, err := cursor.run.ReadBlock(ctx, index, cursor.maxBytes-header, cursor.memory)
	if err != nil {
		cursor.memory.release(header)
		return nil, err
	}
	return &blockLease{block: block, references: 1}, nil
}

func (cursor *runVersionCursor) advanceBlock(ctx context.Context) error {
	for cursor.nextBlock < cursor.endBlock && ((cursor.snapshotTS > 0 && cursor.run.entry(cursor.nextBlock).minCommitTS > cursor.snapshotTS) || (cursor.pointKey != nil && !cursor.run.entryMayContainPoint(cursor.nextBlock, cursor.pointKey, cursor.snapshotTS))) {
		if err := ctx.Err(); err != nil {
			return err
		}
		cursor.memory.observe(storage.ScanObservation{BlocksPruned: storage.ObservationCount{Value: 1, Known: true}})
		cursor.nextBlock++
	}
	if cursor.nextBlock == cursor.endBlock {
		return io.EOF
	}
	next, err := cursor.readLease(ctx, cursor.nextBlock)
	if err != nil {
		return err
	}
	if cursor.lease != nil {
		if err := checkRunBoundary(cursor.lease.block, next.block); err != nil {
			return errors.Join(err, next.release())
		}
		if err := cursor.lease.release(); err != nil {
			cursor.lease = nil
			return errors.Join(err, next.release())
		}
	}
	cursor.lease = next
	cursor.row = 0
	cursor.nextBlock++
	return nil
}

func checkRunBoundary(previous, next *paxBlock) error {
	lastKey, _, lastTimestamp, lastOrdinal, _, err := previous.Version(previous.Rows() - 1)
	if err != nil {
		return err
	}
	firstKey, _, firstTimestamp, firstOrdinal, _, err := next.Version(0)
	if err != nil {
		return err
	}
	order := bytes.Compare(lastKey, firstKey)
	versionNotDescending := firstTimestamp > lastTimestamp || (firstTimestamp == lastTimestamp && firstOrdinal >= lastOrdinal)
	if order > 0 || (order == 0 && versionNotDescending) {
		return storage.ErrCorruptStorage
	}
	return nil
}

func (cursor *runVersionCursor) Close() error {
	if cursor.closed {
		return nil
	}
	cursor.closed = true
	cursor.run = nil
	cursor.memory = nil
	var err error
	if state := cursor.sequence; state != nil {
		err = state.current.Close()
		cursor.sequence = nil
	}
	if state := cursor.reverse; state != nil {
		cursor.reverse = nil
		if state.group != nil {
			err = state.group.Close()
		}
	}
	if cursor.lease == nil {
		return err
	}
	lease := cursor.lease
	cursor.lease = nil
	return errors.Join(err, lease.release())
}

func iterationError(cause, cleanup error) error {
	if cleanup == nil {
		return cause
	}
	if cause == io.EOF {
		return cleanup
	}
	return errors.Join(cause, cleanup)
}
