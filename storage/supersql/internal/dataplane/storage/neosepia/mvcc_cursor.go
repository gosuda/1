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
	"github.com/gosuda/supersql-alpha/internal/model"
)

type mergeHead struct {
	source  *runVersionCursor
	version versionRow
}

// MaxBytes covers merge state and active tombstones, excluding source blocks
// and selections retained by the consumer. Those have separate block budgets.
type mergeLimits struct{ MaxBytes, MaxKeyBytes uint64 }

type mvccCursor struct {
	heads        []mergeHead
	heap         []uint32
	ranges       activeRanges
	timestamp    model.Timestamp
	key          []byte
	hasKey       bool
	advance      bool
	closed       bool
	initialized  bool
	reverse      bool
	retainFuture bool
}

// Ownership of every nonnil source transfers even on error. Returned rows borrow
// their PAX block until Next/Close; callers may retain a selection's block lease.
// I/O begins at Next; exact own-write reads need not touch shadowed runs.
func newMVCCCursor(ctx context.Context, sources []*runVersionCursor, timestamp model.Timestamp, limits mergeLimits, direction storage.ScanDirection) (*mvccCursor, error) {
	return newVersionMerge(ctx, sources, timestamp, limits, direction, false)
}

// Retention emits every version above floor and one visible anchor per key at
// floor. Zero retains the complete history, including point and range tombstones.
// Sources must cover the complete target history: dropping old tombstones during
// a partial compaction can resurrect versions in omitted files. Ownership transfers
// on every return; emitted rows borrow their source block until Next or Close.
func newRetentionCursor(ctx context.Context, sources []*runVersionCursor, floor model.Timestamp, limits mergeLimits) (*mvccCursor, error) {
	return newVersionMerge(ctx, sources, floor, limits, storage.ScanForward, true)
}

func newVersionMerge(ctx context.Context, sources []*runVersionCursor, timestamp model.Timestamp, limits mergeLimits, direction storage.ScanDirection, retainFuture bool) (_ *mvccCursor, err error) {
	var cursor *mvccCursor
	defer func() {
		if err != nil && cursor != nil {
			err = errors.Join(err, cursor.Close())
		}
		if err != nil && cursor == nil {
			for _, source := range sources {
				if source != nil {
					err = errors.Join(err, source.Close())
				}
			}
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if timestamp < 0 || (timestamp == 0 && !retainFuture) {
		return nil, storage.ErrSnapshotNotClosed
	}
	if direction != storage.ScanForward && direction != storage.ScanReverse {
		return nil, storage.ErrInvalidScanPlan
	}
	if uint64(len(sources)) > math.MaxUint32 || limits.MaxKeyBytes > uint64(math.MaxInt) {
		return nil, columnar.ErrBatchBudget
	}
	fixed := uint64(unsafe.Sizeof(mvccCursor{})) + uint64(len(sources))*(uint64(unsafe.Sizeof(mergeHead{}))+4)
	if fixed > limits.MaxBytes || limits.MaxKeyBytes > limits.MaxBytes-fixed {
		return nil, columnar.ErrBatchBudget
	}
	for _, source := range sources {
		if source == nil {
			return nil, storage.ErrInvalidScanPlan
		}
		reverse := source.reverse != nil || (source.sequence != nil && source.sequence.direction == storage.ScanReverse)
		if reverse != (direction == storage.ScanReverse) {
			return nil, storage.ErrInvalidScanPlan
		}
	}
	cursor = &mvccCursor{heads: make([]mergeHead, len(sources)), heap: make([]uint32, 0, len(sources)), timestamp: timestamp, key: make([]byte, 0, int(limits.MaxKeyBytes)), ranges: newActiveRanges(limits.MaxBytes - fixed - limits.MaxKeyBytes)}
	cursor.reverse = direction == storage.ScanReverse
	cursor.retainFuture = retainFuture
	cursor.ranges.reverse = cursor.reverse
	for i, source := range sources {
		if !retainFuture {
			source.snapshotTS = timestamp
		}
		cursor.heads[i].source = source
	}
	return cursor, nil
}

func (cursor *mvccCursor) initialize(ctx context.Context) error {
	for i := range cursor.heads {
		version, err := cursor.heads[i].source.Next(ctx)
		if err == io.EOF {
			continue
		}
		if err != nil {
			return err
		}
		cursor.heads[i].version = version
		cursor.heap = append(cursor.heap, uint32(i))
	}
	for index := len(cursor.heap) / 2; index > 0; index-- {
		cursor.down(index - 1)
	}
	cursor.initialized = true
	return nil
}

func (cursor *mvccCursor) Next(ctx context.Context) (_ versionRow, err error) {
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
	if !cursor.initialized {
		if err := cursor.initialize(ctx); err != nil {
			return versionRow{}, err
		}
	}
	if cursor.advance {
		if err := cursor.advanceHead(ctx); err != nil {
			return versionRow{}, err
		}
		cursor.advance = false
	}
	for len(cursor.heap) != 0 {
		if err := ctx.Err(); err != nil {
			return versionRow{}, err
		}
		row := cursor.heads[cursor.heap[0]].version
		cursor.ranges.advance(versionPosition(&row, cursor.reverse))
		if row.timestamp > cursor.timestamp {
			if cursor.retainFuture {
				cursor.advance = true
				return row, nil
			}
			if err := cursor.advanceHead(ctx); err != nil {
				return versionRow{}, err
			}
			continue
		}
		if row.kind == storage.MutationDeleteRange {
			boundary := row.end
			if cursor.reverse {
				boundary = row.key
			}
			if err := cursor.ranges.add(boundary, row.timestamp, row.ordinal); err != nil {
				return versionRow{}, err
			}
			if err := cursor.advanceHead(ctx); err != nil {
				return versionRow{}, err
			}
			continue
		}
		if cursor.hasKey && bytes.Equal(cursor.key, row.key) {
			if err := cursor.advanceHead(ctx); err != nil {
				return versionRow{}, err
			}
			continue
		}
		if len(row.key) > cap(cursor.key) {
			return versionRow{}, columnar.ErrBatchBudget
		}
		cursor.key = append(cursor.key[:0], row.key...)
		cursor.hasKey = true
		if row.kind == storage.MutationDelete || cursor.ranges.suppresses(row.timestamp, row.ordinal) {
			if err := cursor.advanceHead(ctx); err != nil {
				return versionRow{}, err
			}
			continue
		}
		cursor.advance = true
		return row, nil
	}
	return versionRow{}, io.EOF
}

func (cursor *mvccCursor) advanceHead(ctx context.Context) error {
	head := &cursor.heads[cursor.heap[0]]
	version, err := head.source.Next(ctx)
	if err != nil {
		head.version = versionRow{}
		if err != io.EOF {
			return err
		}
		last := len(cursor.heap) - 1
		cursor.heap[0] = cursor.heap[last]
		cursor.heap = cursor.heap[:last]
	} else {
		head.version = version
	}
	if len(cursor.heap) != 0 {
		cursor.down(0)
	}
	return nil
}

func (cursor *mvccCursor) less(left, right int) bool {
	a, b := &cursor.heads[cursor.heap[left]].version, &cursor.heads[cursor.heap[right]].version
	if order := bytes.Compare(versionPosition(a, cursor.reverse), versionPosition(b, cursor.reverse)); order != 0 {
		if cursor.reverse {
			return order > 0
		}
		return order < 0
	}
	if cursor.reverse && (a.kind == storage.MutationDeleteRange) != (b.kind == storage.MutationDeleteRange) {
		return a.kind != storage.MutationDeleteRange
	}
	if a.timestamp != b.timestamp {
		return a.timestamp > b.timestamp
	}
	return a.ordinal > b.ordinal
}

func versionPosition(row *versionRow, reverse bool) []byte {
	if reverse && row.kind == storage.MutationDeleteRange {
		return row.end
	}
	return row.key
}

func (cursor *mvccCursor) down(index int) {
	for index < len(cursor.heap)/2 {
		child := index*2 + 1
		if child+1 < len(cursor.heap) && cursor.less(child+1, child) {
			child++
		}
		if !cursor.less(child, index) {
			return
		}
		cursor.heap[index], cursor.heap[child] = cursor.heap[child], cursor.heap[index]
		index = child
	}
}

func (cursor *mvccCursor) Close() error {
	if cursor.closed {
		return nil
	}
	cursor.closed = true
	var err error
	for i := range cursor.heads {
		if source := cursor.heads[i].source; source != nil {
			err = errors.Join(err, source.Close())
		}
	}
	cursor.heads = nil
	cursor.heap = nil
	cursor.key = nil
	cursor.ranges.Close()
	return err
}
