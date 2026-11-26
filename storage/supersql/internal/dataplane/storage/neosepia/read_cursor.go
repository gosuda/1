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

type readRow struct {
	versionRow
	own *storage.Mutation
}

// Own writes are a separate precedence layer, never a fabricated commit timestamp.
// Both heads are borrowed until Next; the committed head keeps its native PAX lease.
type readCursor struct {
	base                                   *mvccCursor
	overlay                                storage.MutationCursor
	target                                 storage.ObjectRef
	committed                              versionRow
	mutation                               storage.Mutation
	baseReady, baseDone, ownReady, ownDone bool
	ranges                                 activeRanges
	orderKey, pointKey                     []byte
	orderOrdinal                           uint64
	hasOrder, hasPoint, closed             bool
	orderRange                             bool
}

// Base ownership transfers on every return. Scan retains the immutable overlay
// nodes independently of the caller's view. Limits cover only this merge layer.
func newReadCursor(ctx context.Context, base *mvccCursor, view storage.OverlayView, target storage.ObjectRef, limits mergeLimits) (_ *readCursor, err error) {
	if base == nil {
		return nil, storage.ErrInvalidScanPlan
	}
	cursor := &readCursor{base: base, target: target, ownDone: view == nil}
	defer func() {
		if err != nil {
			err = errors.Join(err, cursor.Close())
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if view == nil {
		return cursor, nil
	}
	fixed := uint64(unsafe.Sizeof(readCursor{}))
	if limits.MaxKeyBytes > uint64(math.MaxInt) || fixed > limits.MaxBytes || limits.MaxKeyBytes > (limits.MaxBytes-fixed)/2 {
		return nil, columnar.ErrBatchBudget
	}
	cursor.orderKey = make([]byte, 0, int(limits.MaxKeyBytes))
	cursor.pointKey = make([]byte, 0, int(limits.MaxKeyBytes))
	cursor.ranges = newActiveRanges(limits.MaxBytes - fixed - 2*limits.MaxKeyBytes)
	direction := storage.ScanForward
	if base.reverse {
		direction = storage.ScanReverse
		cursor.ranges.reverse = true
	}
	cursor.overlay, err = view.Scan(ctx, storage.OverlayScanRequest{Target: target, Direction: direction})
	if err != nil {
		return nil, err
	}
	if cursor.overlay == nil {
		return nil, storage.ErrInvalidScanPlan
	}
	return cursor, nil
}

func (cursor *readCursor) Next(ctx context.Context) (_ readRow, err error) {
	if cursor.closed {
		return readRow{}, io.EOF
	}
	defer func() {
		if err != nil {
			err = iterationError(err, cursor.Close())
		}
	}()
	if cursor.overlay == nil {
		row, err := cursor.base.Next(ctx)
		return readRow{versionRow: row}, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return readRow{}, err
		}
		if !cursor.baseReady && !cursor.baseDone {
			cursor.committed, err = cursor.base.Next(ctx)
			if err != nil && err != io.EOF {
				return readRow{}, err
			}
			cursor.baseReady, cursor.baseDone = err == nil, err == io.EOF
		}
		if !cursor.ownReady && !cursor.ownDone {
			if err := cursor.advanceOwn(ctx); err != nil {
				return readRow{}, err
			}
		}
		if !cursor.baseReady && !cursor.ownReady {
			return readRow{}, io.EOF
		}
		if cursor.ownReady && cursor.ownFirst() {
			mutation := &cursor.mutation
			cursor.ranges.advance(cursor.ownPosition(mutation))
			cursor.ownReady = false
			if mutation.Op == storage.MutationDeleteRange {
				boundary := mutation.EndKey
				if cursor.base.reverse {
					boundary = mutation.Key
				}
				if err := cursor.ranges.add(boundary, 0, mutation.Ordinal); err != nil {
					return readRow{}, err
				}
				continue
			}
			if cursor.hasPoint && bytes.Equal(cursor.pointKey, mutation.Key) {
				continue
			}
			cursor.pointKey = append(cursor.pointKey[:0], mutation.Key...)
			cursor.hasPoint = true
			if mutation.Op == storage.MutationDelete || cursor.ranges.suppresses(0, mutation.Ordinal) {
				continue
			}
			return readRow{versionRow: versionRow{key: mutation.Key, ordinal: mutation.Ordinal, kind: mutation.Op}, own: mutation}, nil
		}
		row := cursor.committed
		cursor.ranges.advance(row.key)
		cursor.baseReady = false
		if (cursor.hasPoint && bytes.Equal(cursor.pointKey, row.key)) || cursor.ranges.suppresses(0, 0) {
			continue
		}
		return readRow{versionRow: row}, nil
	}
}

func (cursor *readCursor) advanceOwn(ctx context.Context) error {
	mutation, err := cursor.overlay.Next(ctx)
	if err == io.EOF {
		cursor.ownDone = true
		cursor.mutation = storage.Mutation{}
		return nil
	}
	if err != nil {
		return err
	}
	if mutation.Target != cursor.target {
		return storage.ErrInvalidTarget
	}
	if mutation.Ordinal == 0 {
		return storage.ErrInvalidScanPlan
	}
	position := cursor.ownPosition(&mutation)
	if len(position) > cap(cursor.orderKey) || len(mutation.Key) > cap(cursor.pointKey) {
		return columnar.ErrBatchBudget
	}
	if cursor.hasOrder {
		order := bytes.Compare(cursor.orderKey, position)
		reversedTie := cursor.orderOrdinal <= mutation.Ordinal
		if cursor.base.reverse {
			order = -order
			if cursor.orderRange != (mutation.Op == storage.MutationDeleteRange) {
				reversedTie = cursor.orderRange
			}
		}
		if order > 0 || (order == 0 && reversedTie) {
			return storage.ErrInvalidScanPlan
		}
	}
	switch mutation.Op {
	case storage.MutationUpsert:
		if len(mutation.ColumnIDs) != len(mutation.Values) || len(mutation.EndKey) != 0 {
			return storage.ErrInvalidScanPlan
		}
	case storage.MutationDelete:
		if len(mutation.ColumnIDs) != 0 || len(mutation.Values) != 0 || len(mutation.EndKey) != 0 {
			return storage.ErrInvalidScanPlan
		}
	case storage.MutationDeleteRange:
		if len(mutation.Key) == 0 || len(mutation.EndKey) == 0 || bytes.Compare(mutation.Key, mutation.EndKey) >= 0 || len(mutation.ColumnIDs) != 0 || len(mutation.Values) != 0 {
			return storage.ErrInvalidScanPlan
		}
	default:
		return storage.ErrUnnormalizedBatch
	}
	cursor.orderKey = append(cursor.orderKey[:0], position...)
	cursor.orderRange = mutation.Op == storage.MutationDeleteRange
	cursor.orderOrdinal = mutation.Ordinal
	cursor.hasOrder = true
	cursor.mutation = mutation
	cursor.ownReady = true
	return nil
}

func (cursor *readCursor) ownPosition(mutation *storage.Mutation) []byte {
	if cursor.base.reverse && mutation.Op == storage.MutationDeleteRange {
		return mutation.EndKey
	}
	return mutation.Key
}

func (cursor *readCursor) ownFirst() bool {
	if !cursor.baseReady {
		return true
	}
	order := bytes.Compare(cursor.ownPosition(&cursor.mutation), cursor.committed.key)
	if cursor.base.reverse {
		if order == 0 {
			return cursor.mutation.Op != storage.MutationDeleteRange
		}
		return order > 0
	}
	return order <= 0
}

func (cursor *readCursor) Close() error {
	if cursor.closed {
		return nil
	}
	cursor.closed = true
	err := cursor.base.Close()
	if cursor.overlay != nil {
		err = errors.Join(err, cursor.overlay.Close())
	}
	cursor.base = nil
	cursor.overlay = nil
	cursor.committed = versionRow{}
	cursor.mutation = storage.Mutation{}
	cursor.ranges.Close()
	cursor.orderKey = nil
	cursor.pointKey = nil
	return err
}
