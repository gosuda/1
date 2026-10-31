package schema

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Scan streams live and retired incarnations for reconciliation after interrupted
// catalog publication. A scan does not prove unreachability: callers must fence
// publishers and pinned catalog readers before Retire/Reclaim. It copies one row;
// the reader bounds borrowed physical blocks and the allocator caps record count.
func (s *SequenceStore) Scan(ctx context.Context, reader storage.SnapshotReader, visit func(SequenceState) error) (err error) {
	if err := s.check(ctx, reader); err != nil {
		return err
	}
	if visit == nil {
		return &model.Error{Code: "22023", Message: "sequence scan requires a visitor"}
	}
	request := storage.PushdownRequest{Target: s.options.Target, Layout: jobColumns(), KeyLayout: jobKeyLayout(), ColumnIDs: []model.ColumnID{1, 2}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{1})}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{2})}}}}}
	plan, err := reader.PlanScan(ctx, request)
	if plan.Binding != ([32]byte{}) {
		defer func() { err = errors.Join(err, reader.ReleasePlan(plan)) }()
	}
	if err != nil {
		return err
	}
	if plan.Request.Target != request.Target || !plan.Accepted.Empty() || !plan.Residual.Empty() || plan.AppliedLimit.Enabled || !slices.Equal(plan.Projection, request.ColumnIDs) {
		return storage.ErrInvalidScanPlan
	}
	cursor, err := reader.VectorizedScan(ctx, plan)
	if cursor != nil {
		defer func() { err = errors.Join(err, cursor.Close()) }()
	}
	if err != nil {
		return err
	}
	if cursor == nil {
		return storage.ErrInvalidScanPlan
	}
	batch := columnar.Batch{MaxRows: 1}
	defer batch.Reset()
	var count, previous uint64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := cursor.Next(ctx, &batch)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := batch.Validate(max(batch.Length, 1)); err != nil {
			return err
		}
		if batch.Rows() > 1 || len(batch.Columns) != 2 {
			return columnar.ErrInvalidBatch
		}
		for row := range batch.Rows() {
			if count == s.options.MaxIncarnations {
				return sequenceLimitError()
			}
			lane := batch.Lane(row)
			key, _, err := columnar.CopyValue(ctx, &batch.Columns[0], lane, s.options.KeyBytes)
			if err != nil {
				return err
			}
			value, _, err := columnar.CopyValue(ctx, &batch.Columns[1], lane, s.options.RecordBytes)
			if err != nil {
				return err
			}
			invalidValue := key.IsNull || value.IsNull || key.Type != model.TypeBytes || value.Type != model.TypeBytes
			invalidKey := len(key.BytesVal) != 9 || key.BytesVal[0] != 1
			if invalidValue || invalidKey {
				return sequenceCorruptError(nil)
			}
			id := binary.BigEndian.Uint64(key.BytesVal[1:])
			if id <= previous {
				return sequenceCorruptError(nil)
			}
			state, err := decodeSequence(value.BytesVal, SequenceIncarnation(id), s.options.MaxCache)
			if err != nil {
				return err
			}
			if err := visit(state); err != nil {
				return err
			}
			previous = id
			count++
		}
		batch.Reset()
	}
}
