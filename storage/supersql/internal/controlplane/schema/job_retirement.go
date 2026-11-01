package schema

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var retirementMagic = [8]byte{'S', 'J', 'R', 'T', '0', '0', '0', '1'}

const retirementHeaderBytes = 12

// RetirementID identifies one atomic ordinary-DDL reachability change.
type RetirementID uint64

func normalizeRetirements(retired []storage.ObjectGeneration) ([]storage.ObjectGeneration, error) {
	result := slices.Clone(retired)
	for _, object := range result {
		if err := object.Validate(); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(result, func(left, right storage.ObjectGeneration) int {
		if left.ObjectID != right.ObjectID {
			return cmp.Compare(uint64(left.ObjectID), uint64(right.ObjectID))
		}
		return cmp.Compare(left.Generation, right.Generation)
	})
	result = slices.Compact(result)
	return result, nil
}

func encodeRetirements(ctx context.Context, retired []storage.ObjectGeneration, limit uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit < retirementHeaderBytes || len(retired) == 0 || uint64(len(retired)) > math.MaxUint32 || uint64(len(retired)) > (limit-retirementHeaderBytes)/16 {
		return nil, ErrJobLimit
	}
	data := make([]byte, retirementHeaderBytes+16*len(retired))
	copy(data, retirementMagic[:])
	binary.BigEndian.PutUint32(data[8:], uint32(len(retired)))
	for position, object := range retired {
		offset := retirementHeaderBytes + 16*position
		binary.BigEndian.PutUint64(data[offset:], uint64(object.ObjectID))
		binary.BigEndian.PutUint64(data[offset+8:], object.Generation)
	}
	return data, nil
}

func decodeRetirements(ctx context.Context, data []byte, limit uint64) ([]storage.ObjectGeneration, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uint64(len(data)) > limit || len(data) < retirementHeaderBytes || !slices.Equal(data[:8], retirementMagic[:]) {
		return nil, ErrJobRecord
	}
	count := uint64(binary.BigEndian.Uint32(data[8:]))
	if count == 0 || count > uint64(math.MaxInt)/16 || retirementHeaderBytes+16*count != uint64(len(data)) {
		return nil, ErrJobRecord
	}
	result := make([]storage.ObjectGeneration, int(count))
	for position := range result {
		offset := retirementHeaderBytes + 16*position
		result[position] = storage.ObjectGeneration{ObjectID: model.ObjectID(binary.BigEndian.Uint64(data[offset:])), Generation: binary.BigEndian.Uint64(data[offset+8:])}
		if err := result[position].Validate(); err != nil {
			return nil, errors.Join(ErrJobRecord, err)
		}
		if position > 0 {
			previous := result[position-1]
			current := result[position]
			if previous.ObjectID > current.ObjectID || previous.ObjectID == current.ObjectID && previous.Generation >= current.Generation {
				return nil, ErrJobRecord
			}
		}
	}
	return result, nil
}

func (s *JobStore) stageRetirements(ctx context.Context, transaction txn.MutationApplier, key []byte, retired []storage.ObjectGeneration) error {
	if transaction == nil || len(key) != 9 {
		return ErrJobRecord
	}
	normalized, err := normalizeRetirements(retired)
	if err != nil {
		return err
	}
	if len(normalized) == 0 {
		return nil
	}
	payload, err := encodeRetirements(ctx, normalized, s.options.RecordBytes)
	if err != nil {
		return err
	}
	mutation, err := s.mutation(ctx, storage.MutationInsert, key, payload)
	if err != nil {
		return err
	}
	return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
}

// RetirementDependency protects one ordinary-DDL retirement journal key.
func (s *JobStore) RetirementDependency(id RetirementID) (txn.ReadDependency, error) {
	if s == nil || s.normalizer == nil || id == 0 {
		return txn.ReadDependency{}, ErrJobRecord
	}
	bound := storage.KeyBound{Values: []model.Value{model.BytesVal(jobLogicalKey(5, uint64(id)))}, Inclusive: true}
	return txn.ReadDependency{Target: s.options.Target, KeyLayout: jobKeyLayout(), Spans: []storage.KeySpan{{Lower: bound, Upper: bound}}}, nil
}

// StageRetirements journals physical generations in the transaction that makes
// them catalog-unreachable. The caller supplies a database-unique identity.
func (s *JobStore) StageRetirements(ctx context.Context, transaction txn.MutationApplier, id RetirementID, retired []storage.ObjectGeneration) error {
	if id == 0 {
		return ErrJobRecord
	}
	return s.stageRetirements(ctx, transaction, jobLogicalKey(5, uint64(id)), retired)
}

func (s *JobStore) recordRetirements(ctx context.Context, transaction txn.MutationApplier, id JobID, retired []storage.ObjectGeneration) error {
	if id == 0 {
		return ErrJobRecord
	}
	return s.stageRetirements(ctx, transaction, jobLogicalKey(4, uint64(id)), retired)
}

func (s *JobStore) retirementRecord(ctx context.Context, reader JobReader, key []byte) (model.Timestamp, []storage.ObjectGeneration, error) {
	if s == nil || reader == nil || len(key) != 9 {
		return 0, nil, ErrJobRecord
	}
	encoded, err := s.encodedKey(ctx, key)
	if err != nil {
		return 0, nil, err
	}
	row, err := reader.Get(ctx, storage.ReadRequest{Target: s.options.Target, Key: encoded, ColumnIDs: []model.ColumnID{2}, Layout: jobColumns()})
	if err != nil {
		return 0, nil, err
	}
	if row.Deleted {
		return 0, nil, storage.ErrRowNotFound
	}
	if row.CommitTS <= 0 || len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].Type != model.TypeBytes {
		return 0, nil, ErrJobRecord
	}
	retired, err := decodeRetirements(ctx, row.Values[0].BytesVal, s.options.RecordBytes)
	if err != nil {
		return 0, nil, err
	}
	return row.CommitTS, retired, nil
}

func (s *JobStore) retirements(ctx context.Context, reader JobReader, id JobID) (model.Timestamp, []storage.ObjectGeneration, error) {
	if id == 0 {
		return 0, nil, ErrJobRecord
	}
	return s.retirementRecord(ctx, reader, jobLogicalKey(4, uint64(id)))
}

func (s *JobStore) ordinaryRetirements(ctx context.Context, reader JobReader, id RetirementID) (model.Timestamp, []storage.ObjectGeneration, error) {
	if id == 0 {
		return 0, nil, ErrJobRecord
	}
	return s.retirementRecord(ctx, reader, jobLogicalKey(5, uint64(id)))
}

func (s *JobStore) deleteRetirementRecord(ctx context.Context, transaction txn.MutationApplier, key []byte) error {
	if transaction == nil || len(key) != 9 {
		return ErrJobRecord
	}
	mutation, err := s.mutation(ctx, storage.MutationDelete, key, nil)
	if err != nil {
		return err
	}
	return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
}

func (s *JobStore) deleteRetirements(ctx context.Context, transaction txn.MutationApplier, id JobID) error {
	if id == 0 {
		return ErrJobRecord
	}
	return s.deleteRetirementRecord(ctx, transaction, jobLogicalKey(4, uint64(id)))
}

func (s *JobStore) deleteOrdinaryRetirements(ctx context.Context, transaction txn.MutationApplier, id RetirementID) error {
	if id == 0 {
		return ErrJobRecord
	}
	return s.deleteRetirementRecord(ctx, transaction, jobLogicalKey(5, uint64(id)))
}

func (s *JobStore) scanOrdinaryRetirements(ctx context.Context, reader storage.SnapshotReader, visit func(RetirementID) error) (err error) {
	if s == nil || reader == nil || visit == nil {
		return ErrJobRecord
	}
	request := storage.PushdownRequest{
		Target: s.options.Target, Layout: jobColumns(), KeyLayout: jobKeyLayout(),
		ColumnIDs: []model.ColumnID{1},
		Spans: []storage.KeySpan{{
			Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{5})}, Inclusive: true},
			Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{6})}},
		}},
	}
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
	var seen uint64
	var previous RetirementID
	batch := columnar.Batch{MaxRows: s.options.BatchRows}
	defer batch.Reset()
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
		if err := batch.Validate(max(batch.Length, s.options.BatchRows)); err != nil {
			return err
		}
		if batch.Rows() > s.options.BatchRows || len(batch.Columns) != 1 {
			return columnar.ErrInvalidBatch
		}
		for row := range batch.Rows() {
			if seen == s.options.MaxJobs {
				return ErrJobLimit
			}
			key, _, err := columnar.CopyValue(ctx, &batch.Columns[0], batch.Lane(row), s.options.KeyBytes)
			if err != nil {
				return err
			}
			if key.IsNull || key.Type != model.TypeBytes || len(key.BytesVal) != 9 || key.BytesVal[0] != 5 {
				return ErrJobRecord
			}
			id := RetirementID(binary.BigEndian.Uint64(key.BytesVal[1:]))
			if id == 0 || id <= previous || !bytes.Equal(key.BytesVal, jobLogicalKey(5, uint64(id))) {
				return ErrJobRecord
			}
			previous = id
			if err := visit(id); err != nil {
				return err
			}
			seen++
		}
		batch.Reset()
	}
}
