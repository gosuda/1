package schema

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"slices"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	jobKindGuard              byte = 1
	jobKindActive             byte = 2
	jobKindDefinition         byte = 3
	jobKindJobRetirement      byte = 4
	jobKindOrdinaryRetirement byte = 5
	jobKindTerminal           byte = 8
)

var errPruneBatchFull = errors.New("terminal job prune batch full")

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

type jobStoreWatcher struct {
	id   uint64
	wake func()
}

// JobStore borrows Encoder and has no independent commit. Create/Update must be
// composed with descriptor/policy changes inside the caller's atomic statement.
// The composition root allocates Target; no new global ObjectID is reserved here.
type JobStore struct {
	options     JobStoreOptions
	normalizer  *txn.RowNormalizer
	wakeMu      sync.Mutex
	nextWatcher uint64
	watchers    []jobStoreWatcher
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
	for _, kind := range [...]byte{jobKindActive, jobKindTerminal} {
		if _, err := options.Encoder.EncodePrefix(ctx, jobKeyLayout(), []model.Value{model.BytesVal(jobLogicalKey(kind, math.MaxUint64))}, options.KeyBytes); err != nil {
			return nil, err
		}
	}
	columns := jobColumns()
	normalizer, err := txn.NewRowNormalizer(ctx, options.Target, []txn.ColumnRule{{Layout: columns[0], KeyPosition: 1}, {Layout: columns[1]}}, options.MetadataBytes)
	if err != nil {
		return nil, err
	}
	return &JobStore{options: options, normalizer: normalizer}, nil
}

func (s *JobStore) registerWake(wake func()) func() {
	if s == nil || wake == nil {
		return func() {}
	}
	s.wakeMu.Lock()
	s.nextWatcher++
	id := s.nextWatcher
	s.watchers = append(s.watchers, jobStoreWatcher{id: id, wake: wake})
	s.wakeMu.Unlock()
	return func() {
		s.wakeMu.Lock()
		defer s.wakeMu.Unlock()
		for i, watcher := range s.watchers {
			if watcher.id == id {
				s.watchers = slices.Delete(s.watchers, i, i+1)
				return
			}
		}
	}
}

func (s *JobStore) notifyWatchers() {
	if s == nil {
		return
	}
	s.wakeMu.Lock()
	for _, watcher := range s.watchers {
		watcher.wake()
	}
	s.wakeMu.Unlock()
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
func (s *JobStore) getWithKind(ctx context.Context, reader JobReader, id JobID) (Job, byte, error) {
	if reader == nil || id == 0 {
		return Job{}, 0, ErrJobRecord
	}
	kind := jobKindActive
	data, err := s.read(ctx, reader, jobLogicalKey(jobKindActive, uint64(id)))
	if errors.Is(err, storage.ErrRowNotFound) {
		kind = jobKindTerminal
		data, err = s.read(ctx, reader, jobLogicalKey(jobKindTerminal, uint64(id)))
	}
	if err != nil {
		return Job{}, 0, err
	}
	job, err := DecodeJob(ctx, data, s.options.RecordBytes)
	if err != nil {
		return Job{}, 0, err
	}
	if job.ID != id || (kind == jobKindTerminal && !job.Terminal()) {
		return Job{}, 0, ErrJobRecord
	}
	return job, kind, nil
}
func (s *JobStore) Get(ctx context.Context, reader JobReader, id JobID) (Job, error) {
	job, _, err := s.getWithKind(ctx, reader, id)
	return job, err
}
func (s *JobStore) Active(ctx context.Context, reader JobReader, table model.ObjectID) (Job, error) {
	if reader == nil || table == 0 {
		return Job{}, ErrJobRecord
	}
	data, err := s.read(ctx, reader, jobLogicalKey(jobKindGuard, uint64(table)))
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
	if !next.Terminal() {
		mutation, err := s.mutation(ctx, storage.MutationUpdate, jobLogicalKey(jobKindActive, uint64(next.ID)), payload)
		if err != nil {
			return err
		}
		return transaction.ApplyMutations(ctx, []storage.Mutation{mutation})
	}
	// Move completed/failed/canceled jobs out of the active job prefix (2) into
	// the prunable terminal archive prefix (8) so active scans stay bounded by
	// concurrent jobs rather than lifetime job count.
	var mutations [4]storage.Mutation
	mutations[0], err = s.mutation(ctx, storage.MutationDelete, jobLogicalKey(jobKindActive, uint64(next.ID)), nil)
	if err != nil {
		return err
	}
	mutations[1], err = s.mutation(ctx, storage.MutationDelete, jobLogicalKey(jobKindGuard, uint64(next.TableID)), nil)
	if err != nil {
		return err
	}
	mutations[2], err = s.mutation(ctx, storage.MutationInsert, jobLogicalKey(jobKindTerminal, uint64(next.ID)), payload)
	if err != nil {
		return err
	}
	count := 3
	if next.Kind != AddIndexJob {
		mutations[3], err = s.mutation(ctx, storage.MutationDelete, jobLogicalKey(jobKindDefinition, uint64(next.ID)), nil)
		if err != nil {
			return err
		}
		count++
	}
	return transaction.ApplyMutations(ctx, mutations[:count])
}

// DeleteTerminalJob removes one terminal job record (and any leftover immutable
// definition) once its protection has cleared and no retirement record remains.
func (s *JobStore) DeleteTerminalJob(ctx context.Context, transaction txn.MutationApplier, id JobID) error {
	if s == nil || s.normalizer == nil || transaction == nil || id == 0 {
		return ErrJobRecord
	}
	job, kind, err := s.getWithKind(ctx, transaction, id)
	if err != nil {
		return err
	}
	if !job.Terminal() || job.ProtectedTimestamp != 0 {
		return ErrJobTransition
	}
	switch _, _, retErr := s.retirements(ctx, transaction, id); {
	case retErr == nil:
		return ErrJobConflict
	case !errors.Is(retErr, storage.ErrRowNotFound):
		return retErr
	}
	return s.deletePrunableJobs(ctx, transaction, []prunableJob{{id: id, kind: kind, hasDefinition: job.Kind != AddIndexJob}})
}

type prunableJob struct {
	id            JobID
	kind          byte
	hasDefinition bool
}

// PruneTerminalJobs deletes the oldest terminal jobs whose retirement work is
// complete while retaining up to keepRecent most recent terminal job records.
func (s *JobStore) PruneTerminalJobs(ctx context.Context, reader storage.SnapshotReader, transaction txn.MutationApplier, keepRecent uint64) (int, error) {
	if s == nil || s.normalizer == nil || reader == nil || transaction == nil {
		return 0, ErrJobRecord
	}
	batchLimit := max(s.options.MaxJobs, 64)
	prunable, err := s.collectPrunableTerminalJobs(ctx, reader, keepRecent, batchLimit, nil)
	if err != nil {
		return 0, err
	}
	if len(prunable) == 0 {
		return 0, nil
	}
	if err := s.deletePrunableJobs(ctx, transaction, prunable); err != nil {
		return 0, err
	}
	return len(prunable), nil
}

func (s *JobStore) collectPrunableTerminalJobs(ctx context.Context, reader storage.SnapshotReader, keepRecent, batchLimit uint64, ignoreRetirements map[JobID]bool) ([]prunableJob, error) {
	if batchLimit == 0 {
		return nil, nil
	}
	windowCap := min(keepRecent, 64)
	window := make([]prunableJob, 0, windowCap)
	prunable := make([]prunableJob, 0, min(batchLimit, 64))
	consider := func(candidate prunableJob) error {
		if !ignoreRetirements[candidate.id] {
			switch _, _, err := s.retirements(ctx, reader, candidate.id); {
			case err == nil:
				return nil
			case !errors.Is(err, storage.ErrRowNotFound):
				return err
			}
		}
		prunable = append(prunable, candidate)
		if uint64(len(prunable)) >= batchLimit {
			return errPruneBatchFull
		}
		return nil
	}
	visit := func(kind byte, job Job) error {
		if !job.Terminal() || job.ProtectedTimestamp != 0 {
			return nil
		}
		entry := prunableJob{id: job.ID, kind: kind, hasDefinition: job.Kind != AddIndexJob}
		if keepRecent == 0 {
			return consider(entry)
		}
		window = append(window, entry)
		if uint64(len(window)) <= keepRecent {
			return nil
		}
		oldest := window[0]
		copy(window, window[1:])
		window = window[:len(window)-1]
		return consider(oldest)
	}
	err := s.scanKind(ctx, reader, jobKindTerminal, false, func(job Job) error {
		return visit(jobKindTerminal, job)
	})
	if errors.Is(err, errPruneBatchFull) {
		return prunable, nil
	}
	if err != nil {
		return nil, err
	}
	// Also prune any legacy terminal records left in prefix 2 by earlier releases.
	err = s.scanKind(ctx, reader, jobKindActive, false, func(job Job) error {
		if !job.Terminal() {
			return nil
		}
		return visit(jobKindActive, job)
	})
	if errors.Is(err, errPruneBatchFull) {
		return prunable, nil
	}
	if err != nil {
		return nil, err
	}
	return prunable, nil
}

func (s *JobStore) deletePrunableJobs(ctx context.Context, transaction txn.MutationApplier, jobs []prunableJob) error {
	if len(jobs) == 0 {
		return nil
	}
	mutations := make([]storage.Mutation, 0, len(jobs)*2)
	for _, entry := range jobs {
		delJob, err := s.mutation(ctx, storage.MutationDelete, jobLogicalKey(entry.kind, uint64(entry.id)), nil)
		if err != nil {
			return err
		}
		mutations = append(mutations, delJob)
		if entry.hasDefinition {
			delDef, err := s.mutation(ctx, storage.MutationDelete, jobLogicalKey(jobKindDefinition, uint64(entry.id)), nil)
			if err != nil {
				return err
			}
			mutations = append(mutations, delDef)
		}
	}
	return transaction.ApplyMutations(ctx, mutations)
}

// Dependencies covers the job record, immutable definition, retirement record,
// terminal archive record, and active-table guard. Acquire it before composing
// job and descriptor mutations.
func (s *JobStore) Dependencies(id JobID, table model.ObjectID) txn.ReadDependency {
	keys := [][]byte{
		jobLogicalKey(jobKindGuard, uint64(table)),
		jobLogicalKey(jobKindActive, uint64(id)),
		jobLogicalKey(jobKindDefinition, uint64(id)),
		jobLogicalKey(jobKindJobRetirement, uint64(id)),
		jobLogicalKey(jobKindTerminal, uint64(id)),
	}
	spans := make([]storage.KeySpan, len(keys))
	for i, key := range keys {
		bound := storage.KeyBound{Values: []model.Value{model.BytesVal(key)}, Inclusive: true}
		spans[i] = storage.KeySpan{Lower: bound, Upper: bound}
	}
	return txn.ReadDependency{Target: s.options.Target, KeyLayout: jobKeyLayout(), Spans: spans}
}

// Scan streams owned active job records; callback ownership ends with this call
// unless the callback retains a returned Job. Cursor and issued plan are always
// retired. MaxJobs bounds active (non-terminal) jobs, not lifetime history.
func (s *JobStore) Scan(ctx context.Context, reader storage.SnapshotReader, visit func(Job) error) error {
	return s.scanKind(ctx, reader, jobKindActive, true, visit)
}

// ScanTerminal streams archived terminal job records in ascending JobID order.
func (s *JobStore) ScanTerminal(ctx context.Context, reader storage.SnapshotReader, visit func(Job) error) error {
	return s.scanKind(ctx, reader, jobKindTerminal, false, visit)
}

func (s *JobStore) scanKind(ctx context.Context, reader storage.SnapshotReader, kind byte, enforceActiveLimit bool, visit func(Job) error) (err error) {
	if reader == nil || visit == nil {
		return ErrJobRecord
	}
	request := storage.PushdownRequest{
		Target:    s.options.Target,
		Layout:    jobColumns(),
		KeyLayout: jobKeyLayout(),
		ColumnIDs: []model.ColumnID{1, 2},
		Spans: []storage.KeySpan{{
			Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{kind})}, Inclusive: true},
			Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{kind + 1})}},
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
	var activeSeen uint64
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
			if len(key.BytesVal) != 9 || key.BytesVal[0] != kind {
				return ErrJobRecord
			}
			job, err := DecodeJob(ctx, value.BytesVal, s.options.RecordBytes)
			if err != nil {
				return err
			}
			if !bytes.Equal(key.BytesVal, jobLogicalKey(kind, uint64(job.ID))) {
				return ErrJobRecord
			}
			if kind == jobKindTerminal && !job.Terminal() {
				return ErrJobRecord
			}
			if job.ID <= previous {
				return ErrJobRecord
			}
			if enforceActiveLimit && !job.Terminal() {
				if activeSeen == s.options.MaxJobs {
					return ErrJobLimit
				}
				activeSeen++
			}
			previous = job.ID
			if err := visit(job); err != nil {
				return err
			}
		}
		batch.Reset()
	}
}

