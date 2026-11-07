package schema

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type BackfillScanLimits struct {
	BatchRows              int
	MaxKeyBytes, WorkBytes uint64
}

// IndexKeyScan borrows the fixed protected reader and immutable IndexSet. It
// owns one issued plan/cursor and returns bounded owned key chunks. The caller
// closes it before releasing the protected snapshot. Next and Close serialize.
type IndexKeyScan struct {
	reader   storage.SnapshotReader
	cursor   storage.BatchCursor
	plan     storage.PushdownPlan
	encoder  storage.KeyEncoder
	layout   []storage.KeyColumn
	values   []model.Value
	batch    columnar.Batch
	lane     int
	last     []byte
	maximum  uint64
	rows     int
	closed   bool
	closeErr error
}

func (s *IndexSet) BackfillKeys(ctx context.Context, reader storage.SnapshotReader, decoder storage.KeyDecoder, job Job, limits BackfillScanLimits) (*IndexKeyScan, error) {
	if s == nil || reader == nil || decoder == nil {
		return nil, ErrJobRecord
	}
	wrongJob := job.Kind != AddIndexJob || job.State != JobBackfilling || job.TableID != s.target.ObjectID
	invalidProtection := job.SnapshotTS <= 0 || job.ProtectedTimestamp != job.SnapshotTS
	if wrongJob || invalidProtection {
		return nil, ErrJobRecord
	}
	index := slices.IndexFunc(s.indexes, func(binding indexBinding) bool { return binding.target.ObjectID == job.MutationID })
	if index < 0 {
		return nil, ErrJobRecord
	}
	plan := s.indexes[index].plan
	if plan.State() != model.StateWriteOnly && plan.State() != model.StatePublic {
		return nil, ErrJobTransition
	}
	columns := make([]model.ColumnID, len(plan.baseKeyLayout))
	for i, key := range plan.baseKeyLayout {
		columns[i] = key.ColumnID
	}
	return newKeyScan(ctx, reader, decoder, plan.encoder, job, s.target, plan.baseColumns, plan.baseKeyLayout, columns, limits)
}

// newKeyScan builds the shared bounded key-scan state used by both the index
// backfill and table rewrite entry points. The caller resolves its target,
// column layout, key layout and key column projection during its prologue.
func newKeyScan(ctx context.Context, reader storage.SnapshotReader, decoder storage.KeyDecoder, encoder storage.KeyEncoder, job Job, target storage.ObjectRef, layout []storage.ColumnLayout, keyLayout []storage.KeyColumn, columns []model.ColumnID, limits BackfillScanLimits) (_ *IndexKeyScan, err error) {
	if limits.BatchRows <= 0 || limits.MaxKeyBytes == 0 || limits.WorkBytes == 0 || limits.MaxKeyBytes > uint64(math.MaxInt)/16 || len(keyLayout) == 0 {
		return nil, ErrJobLimit
	}
	if uint64(len(job.ResumeKey)) > limits.MaxKeyBytes {
		return nil, ErrJobLimit
	}
	width := uint64(len(keyLayout))
	fixed := uint64(unsafe.Sizeof(IndexKeyScan{})) + uint64(unsafe.Sizeof(storage.PushdownPlan{})) + 4096
	unit := uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(unsafe.Sizeof(model.ColumnID(0)))
	if width > (math.MaxUint64-fixed)/unit {
		return nil, ErrJobLimit
	}
	fixed += width * unit
	for _, key := range keyLayout {
		if key.Type.ID == 1700 {
			if fixed > math.MaxUint64-(1<<20) {
				return nil, ErrJobLimit
			}
			fixed += 1 << 20
		}
	}
	if fixed > limits.WorkBytes || limits.MaxKeyBytes > (limits.WorkBytes-fixed)/4 {
		return nil, ErrJobLimit
	}
	fixed += 4 * limits.MaxKeyBytes
	perKey := limits.MaxKeyBytes + uint64(unsafe.Sizeof([]byte{}))
	rows := min(uint64(limits.BatchRows), (limits.WorkBytes-fixed)/perKey)
	if rows == 0 || rows > uint64(math.MaxInt) {
		return nil, ErrJobLimit
	}
	bound := storage.KeyBound{Unbounded: true}
	if len(job.ResumeKey) != 0 {
		values, err := decoder.DecodeKey(ctx, keyLayout, job.ResumeKey, limits.WorkBytes-fixed+4*limits.MaxKeyBytes)
		if err != nil {
			return nil, err
		}
		bound = storage.KeyBound{Values: values, Inclusive: false}
	}
	request := storage.PushdownRequest{Target: target, Layout: layout, KeyLayout: keyLayout, ColumnIDs: columns, Spans: []storage.KeySpan{{Lower: bound, Upper: storage.KeyBound{Unbounded: true}}}}
	scan := &IndexKeyScan{reader: reader, encoder: encoder, layout: keyLayout, values: make([]model.Value, len(columns)), last: bytes.Clone(job.ResumeKey), maximum: limits.MaxKeyBytes, rows: int(rows), batch: columnar.Batch{Columns: make([]columnar.Vector, 0, len(columns))}}
	defer func() {
		if err != nil {
			err = errors.Join(err, scan.Close())
		}
	}()
	scan.plan, err = reader.PlanScan(ctx, request)
	if err != nil {
		return nil, err
	}
	plan := scan.plan
	wrongSnapshot := plan.SnapshotTS != job.SnapshotTS || plan.Request.Target != request.Target
	filtered := !plan.Accepted.Empty() || !plan.Residual.Empty() || plan.AppliedLimit.Enabled
	wrongOutput := !slices.Equal(plan.Projection, columns) || len(plan.Ordering) != len(keyLayout)
	if wrongSnapshot || filtered || wrongOutput {
		return nil, storage.ErrInvalidScanPlan
	}
	for i, key := range keyLayout {
		if plan.Ordering[i] != (storage.ScanOrdering{KeyOrdinal: uint32(i), Type: key.Type, Descending: key.Descending, NullsFirst: key.NullsFirst}) {
			return nil, storage.ErrInvalidScanPlan
		}
	}
	expected, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: request, SnapshotTS: job.SnapshotTS}, limits.WorkBytes)
	if err != nil {
		return nil, err
	}
	actual, err := storage.HashPushdownPlan(ctx, storage.PushdownPlan{Request: plan.Request, SnapshotTS: plan.SnapshotTS}, limits.WorkBytes)
	if err != nil {
		return nil, err
	}
	if expected != actual || plan.Binding == ([32]byte{}) {
		return nil, storage.ErrInvalidScanPlan
	}
	scan.cursor, err = reader.VectorizedScan(ctx, plan)
	if err != nil {
		return nil, err
	}
	if scan.cursor == nil {
		return nil, storage.ErrInvalidScanPlan
	}
	return scan, nil
}

func (s *IndexKeyScan) Next(ctx context.Context) (keys [][]byte, complete bool, err error) {
	if s.closed {
		return nil, true, s.closeErr
	}
	previous := s.last
	defer func() {
		if err != nil {
			keys = nil
			complete = false
			err = errors.Join(err, s.Close())
		} else if !s.closed {
			s.last = bytes.Clone(previous)
		}
	}()
	keys = make([][]byte, 0, s.rows)
	for len(keys) < s.rows {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if s.lane == s.batch.Rows() {
			s.batch.Reset()
			s.batch.MaxRows = s.rows - len(keys)
			s.lane = 0
			nextErr := s.cursor.Next(ctx, &s.batch)
			if nextErr == io.EOF {
				if err := s.Close(); err != nil {
					return nil, false, err
				}
				return keys, true, nil
			}
			if nextErr != nil {
				return nil, false, nextErr
			}
			if err := s.batch.Validate(max(s.batch.Length, s.batch.MaxRows)); err != nil {
				return nil, false, err
			}
			if s.batch.Rows() > s.batch.MaxRows || len(s.batch.Columns) != len(s.layout) {
				return nil, false, columnar.ErrInvalidBatch
			}
			if s.batch.Rows() == 0 {
				continue
			}
		}
		remaining := s.maximum
		for i, column := range s.layout {
			vector := &s.batch.Columns[i]
			if vector.Type != column.Type {
				return nil, false, columnar.ErrValueLayout
			}
			value, used, err := columnar.CopyValue(ctx, vector, s.batch.Lane(s.lane), remaining)
			if err != nil {
				return nil, false, err
			}
			s.values[i] = value
			remaining -= used
		}
		key, err := s.encoder.EncodePrefix(ctx, s.layout, s.values, s.maximum)
		clear(s.values)
		if err != nil {
			return nil, false, err
		}
		if len(key) == 0 || len(previous) != 0 && bytes.Compare(key, previous) <= 0 {
			return nil, false, ErrJobRecord
		}
		previous = key
		keys = append(keys, key)
		s.lane++
	}
	return keys, false, nil
}

func (s *IndexKeyScan) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.cursor != nil {
		s.closeErr = s.cursor.Close()
		s.cursor = nil
	}
	if s.plan.Binding != ([32]byte{}) {
		s.closeErr = errors.Join(s.closeErr, s.reader.ReleasePlan(s.plan))
	}
	s.plan = storage.PushdownPlan{}
	s.batch = columnar.Batch{}
	s.values, s.layout, s.last = nil, nil, nil
	s.reader, s.encoder = nil, nil
	return s.closeErr
}
