package schema

import (
	"bytes"
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

type JobReader interface {
	Get(context.Context, storage.ReadRequest) (model.Row, error)
}
type JobStoreOptions struct {
	Target                               storage.ObjectRef
	Encoder                              storage.KeyEncoder
	MetadataBytes, RecordBytes, KeyBytes uint64
	MaxJobs                              uint64
	BatchRows                            int
}

// JobStore borrows Encoder and has no independent commit. Create/Update must be
// composed with descriptor/policy changes inside the caller's atomic statement.
// The composition root allocates Target; no new global ObjectID is reserved here.
type JobStore struct {
	options    JobStoreOptions
	normalizer *txn.RowNormalizer
}

func NewJobStore(ctx context.Context, options JobStoreOptions) (*JobStore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := options.Target.Validate(); err != nil {
		return nil, err
	}
	invalidStorage := options.Encoder == nil || options.MetadataBytes == 0 || options.RecordBytes < jobHeaderBytes
	invalidBounds := options.KeyBytes == 0 || options.MaxJobs == 0 || options.BatchRows <= 0
	if invalidStorage || invalidBounds {
		return nil, ErrJobLimit
	}
	if options.RecordBytes > uint64(math.MaxInt)/8 || options.MetadataBytes > uint64(math.MaxInt) || options.KeyBytes > options.RecordBytes {
		return nil, ErrJobLimit
	}
	if _, err := options.Encoder.EncodePrefix(ctx, jobKeyLayout(), []model.Value{model.BytesVal(jobLogicalKey(2, math.MaxUint64))}, options.KeyBytes); err != nil {
		return nil, err
	}
	columns := jobColumns()
	normalizer, err := txn.NewRowNormalizer(ctx, options.Target, []txn.ColumnRule{{Layout: columns[0], KeyPosition: 1}, {Layout: columns[1]}}, options.MetadataBytes)
	if err != nil {
		return nil, err
	}
	return &JobStore{options: options, normalizer: normalizer}, nil
}
func jobColumns() []storage.ColumnLayout {
	typ := model.Type{ID: 17, Storage: model.TypeBytes}
	return []storage.ColumnLayout{{ID: 1, Type: typ}, {ID: 2, Type: typ}}
}
func jobKeyLayout() []storage.KeyColumn {
	return []storage.KeyColumn{{ColumnID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
}
func (s *JobStore) Layouts() []storage.ObjectLayout {
	return []storage.ObjectLayout{s.normalizer.Layout()}
}
func (s *JobStore) KeyLayout() []storage.KeyColumn { return s.normalizer.KeyLayout() }
func (s *JobStore) Target() storage.ObjectRef      { return s.options.Target }
func (s *JobStore) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if target != s.options.Target {
		return nil, storage.ErrInvalidLayout
	}
	return s.normalizer, nil
}

type jobMutationResolver struct {
	store *JobStore
	base  txn.MutationResolver
}

func (s *JobStore) WithBaseResolver(base txn.MutationResolver) txn.MutationResolver {
	return jobMutationResolver{s, base}
}
func (r jobMutationResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if target.ObjectID == r.store.options.Target.ObjectID {
		return r.store.ResolveMutation(ctx, target)
	}
	if r.base == nil {
		return nil, storage.ErrInvalidLayout
	}
	return r.base.ResolveMutation(ctx, target)
}

func jobLogicalKey(kind byte, id uint64) []byte {
	result := make([]byte, 9)
	result[0] = kind
	binary.BigEndian.PutUint64(result[1:], id)
	return result
}
func (s *JobStore) encodedKey(ctx context.Context, key []byte) ([]byte, error) {
	return s.options.Encoder.EncodePrefix(ctx, jobKeyLayout(), []model.Value{model.BytesVal(key)}, s.options.KeyBytes)
}
func (s *JobStore) read(ctx context.Context, reader JobReader, key []byte) ([]byte, error) {
	return s.readLimit(ctx, reader, key, s.options.RecordBytes)
}

// readLimit reads one record and rejects payloads over maximum. Callers that own
// a record whose contract is not a job, such as the native catalog manifest,
// supply their own bound instead of the per-job RecordBytes.
func (s *JobStore) readLimit(ctx context.Context, reader JobReader, key []byte, maximum uint64) ([]byte, error) {
	encoded, err := s.encodedKey(ctx, key)
	if err != nil {
		return nil, err
	}
	row, err := reader.Get(ctx, storage.ReadRequest{Target: s.options.Target, Key: encoded, ColumnIDs: []model.ColumnID{2}, Layout: jobColumns()})
	if err != nil {
		return nil, err
	}
	if row.Deleted {
		return nil, storage.ErrRowNotFound
	}
	if len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].Type != model.TypeBytes {
		return nil, ErrJobRecord
	}
	if uint64(len(row.Values[0].BytesVal)) > maximum {
		return nil, ErrJobLimit
	}
	return row.Values[0].BytesVal, nil
}
func (s *JobStore) Get(ctx context.Context, reader JobReader, id JobID) (Job, error) {
	if reader == nil || id == 0 {
		return Job{}, ErrJobRecord
	}
	data, err := s.read(ctx, reader, jobLogicalKey(2, uint64(id)))
	if err != nil {
		return Job{}, err
	}
	job, err := DecodeJob(ctx, data, s.options.RecordBytes)
	if err != nil {
		return Job{}, err
	}
	if job.ID != id {
		return Job{}, ErrJobRecord
	}
	return job, nil
}
func (s *JobStore) Active(ctx context.Context, reader JobReader, table model.ObjectID) (Job, error) {
	if reader == nil || table == 0 {
		return Job{}, ErrJobRecord
	}
	data, err := s.read(ctx, reader, jobLogicalKey(1, uint64(table)))
	if err != nil {
		return Job{}, err
	}
	if len(data) != 8 {
		return Job{}, ErrJobRecord
	}
	job, err := s.Get(ctx, reader, JobID(binary.BigEndian.Uint64(data)))
	if err != nil {
		return Job{}, errors.Join(ErrJobRecord, err)
	}
	if job.TableID != table || job.Terminal() {
		return Job{}, ErrJobRecord
	}
	return job, nil
}
func (s *JobStore) mutation(ctx context.Context, operation storage.MutationOp, key, payload []byte) (storage.Mutation, error) {
	encoded, err := s.encodedKey(ctx, key)
	if err != nil {
		return storage.Mutation{}, err
	}
	mutation := storage.Mutation{Op: operation, Target: s.options.Target, Key: encoded}
	if operation != storage.MutationDelete {
		mutation.ColumnIDs = []model.ColumnID{1, 2}
		mutation.Values = []model.Value{model.BytesVal(key), model.BytesVal(payload)}
	}
	return mutation, nil
}

func (s *JobStore) Create(ctx context.Context, transaction txn.MutationApplier, job Job) error {
	if job.Kind != AddIndexJob {
		return errJobDefinition
	}
	return s.create(ctx, transaction, job, nil)
}

func (s *JobStore) Update(ctx context.Context, transaction txn.MutationApplier, expected uint64, next Job) error {
	if transaction == nil {
		return ErrJobRecord
	}
	if uint64(len(next.ResumeKey)) > s.options.KeyBytes || uint64(len(next.FailureKey)) > s.options.KeyBytes {
		return ErrJobLimit
	}
	payload, err := EncodeJob(ctx, next, s.options.RecordBytes)
	if err != nil {
		return err
	}
	previous, err := s.Get(ctx, transaction, next.ID)
	if err != nil {
		return err
	}
	if previous.Revision != expected {
		return ErrJobChanged
	}
	if err := validateJobSuccessor(previous, next); err != nil {
		return err
	}
	active, err := s.Active(ctx, transaction, next.TableID)
	if err != nil {
		return err
	}
	if active.ID != next.ID {
		return ErrJobConflict
	}
	var mutations [2]storage.Mutation
	mutations[0], err = s.mutation(ctx, storage.MutationUpdate, jobLogicalKey(2, uint64(next.ID)), payload)
	if err != nil {
		return err
	}
	count := 1
	if next.Terminal() {
		mutations[1], err = s.mutation(ctx, storage.MutationDelete, jobLogicalKey(1, uint64(next.TableID)), nil)
		if err != nil {
			return err
		}
		count++
	}
	return transaction.ApplyMutations(ctx, mutations[:count])
}

// Dependencies covers the job record, immutable definition, retirement record,
// and active-table guard. Acquire it before composing job and descriptor mutations.
func (s *JobStore) Dependencies(id JobID, table model.ObjectID) txn.ReadDependency {
	keys := [][]byte{jobLogicalKey(1, uint64(table)), jobLogicalKey(2, uint64(id)), jobLogicalKey(3, uint64(id)), jobLogicalKey(4, uint64(id))}
	spans := make([]storage.KeySpan, len(keys))
	for i, key := range keys {
		bound := storage.KeyBound{Values: []model.Value{model.BytesVal(key)}, Inclusive: true}
		spans[i] = storage.KeySpan{Lower: bound, Upper: bound}
	}
	return txn.ReadDependency{Target: s.options.Target, KeyLayout: jobKeyLayout(), Spans: spans}
}

// Scan streams owned job records; callback ownership ends with this call unless
// the callback retains a returned Job. Cursor and issued plan are always retired.
func (s *JobStore) Scan(ctx context.Context, reader storage.SnapshotReader, visit func(Job) error) (err error) {
	if reader == nil || visit == nil {
		return ErrJobRecord
	}
	request := storage.PushdownRequest{Target: s.options.Target, Layout: jobColumns(), KeyLayout: jobKeyLayout(), ColumnIDs: []model.ColumnID{1, 2}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{2})}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{3})}}}}}
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
	var previous JobID
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
		if batch.Rows() > s.options.BatchRows || len(batch.Columns) != 2 {
			return columnar.ErrInvalidBatch
		}
		for row := range batch.Rows() {
			if seen == s.options.MaxJobs {
				return ErrJobLimit
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
			nullValue := key.IsNull || value.IsNull
			wrongType := key.Type != model.TypeBytes || value.Type != model.TypeBytes
			if nullValue || wrongType {
				return ErrJobRecord
			}
			if len(key.BytesVal) != 9 || key.BytesVal[0] != 2 {
				return ErrJobRecord
			}
			job, err := DecodeJob(ctx, value.BytesVal, s.options.RecordBytes)
			if err != nil {
				return err
			}
			if !bytes.Equal(key.BytesVal, jobLogicalKey(2, uint64(job.ID))) {
				return ErrJobRecord
			}
			if job.ID <= previous {
				return ErrJobRecord
			}
			previous = job.ID
			if err := visit(job); err != nil {
				return err
			}
			seen++
		}
		batch.Reset()
	}
}
