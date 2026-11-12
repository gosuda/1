package schema

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (r *IndexJobRunner) stepDefined(ctx context.Context, job Job, policy IndexWritePolicy, cancelJob bool) (bool, error) {
	if job.Revision == math.MaxUint64 {
		return false, ErrJobTransition
	}
	next := job
	next.Revision++
	if !r.claimed {
		next.Attempt = r.lease
	}
	if job.State < JobPublic && (cancelJob || r.failure != nil) {
		next.State = JobReverting
		if r.failure != nil {
			next.ErrorCode, next.FailureKey = r.failure.code, r.failure.key
		} else {
			next.ErrorCode = "57014"
		}
		return false, r.transitionDefined(ctx, job, policy, next, nil, nil)
	}
	switch job.State {
	case JobPending:
		next.State = JobDeleteOnly
	case JobDeleteOnly:
		next.State = JobWriteOnly
	case JobWriteOnly:
		pin, err := r.options.Manager.CurrentReadView(ctx)
		if err != nil {
			return false, err
		}
		next.State = JobBackfilling
		next.SnapshotTS = pin.Timestamp()
		next.ProtectedTimestamp = next.SnapshotTS
		return false, r.transitionDefined(ctx, job, policy, next, pin, nil)
	case JobBackfilling:
		return true, r.backfillDefined(ctx, job, policy)
	case JobValidating:
		proof, err := r.validateDefined(ctx, job, policy)
		if err != nil {
			return false, err
		}
		next.State = JobPublic
		return false, r.transitionDefined(ctx, job, policy, next, nil, &proof)
	case JobPublic:
		next.State = JobSucceeded
		next.ProtectedTimestamp = 0
	case JobReverting:
		next.State = JobFailed
		if job.ErrorCode == "57014" {
			next.State = JobCanceled
		}
		next.ProtectedTimestamp = 0
	default:
		return false, ErrJobTransition
	}
	return false, r.transitionDefined(ctx, job, policy, next, nil, nil)
}

func (r *IndexJobRunner) transitionDefined(ctx context.Context, job Job, policy IndexWritePolicy, next Job, pin txn.ReadView, proof *SchemaPublicationProof) error {
	transaction, err := r.begin(ctx, policy)
	if err != nil {
		if pin != nil {
			err = errors.Join(err, r.cleanup(pin.Close()))
		}
		return err
	}
	change := SchemaTransition{Expected: job, Next: next, Table: policy.Table, Protected: pin, Validation: proof}
	if err := r.options.Lifecycle.StageDefined(ctx, transaction, change); err != nil {
		return r.abort(ctx, transaction, pin, err)
	}
	return r.commit(ctx, transaction, next, pin)
}

func (r *IndexJobRunner) backfillDefined(ctx context.Context, job Job, policy IndexWritePolicy) error {
	if err := r.options.Protections.Require(job.ID, job.SnapshotTS); err != nil {
		return err
	}
	definition, err := r.readDefinition(ctx, job)
	if err != nil {
		return err
	}
	if definition.Operation == SchemaDropIndex {
		return r.commitDefinedChunk(ctx, job, policy, definition, nil, true)
	}
	if policy.Rows == nil || definition.Operation != SchemaAddColumn && policy.Generation == nil {
		return errIndexRunnerOptions
	}
	layout := policy.Rows.Layout()
	if r.scan != nil && r.scanTable != policy.Table {
		if err := r.closeScan(); err != nil {
			return err
		}
	}
	if r.scan == nil {
		reader, err := r.options.Manager.HistoricalSnapshot(ctx, job.SnapshotTS)
		if err != nil {
			return err
		}
		r.scanReader = reader
		r.scan, err = NewTableKeyScan(ctx, reader, policy.Keys, policy.Keys, job, policy.Table, layout.Columns, policy.Rows.KeyLayout(), r.options.Scan)
		if err != nil {
			return errors.Join(err, r.closeScan())
		}
		r.scanJob, r.scanTable = job, policy.Table
	}
	if !r.chunkReady {
		keys, complete, err := r.scan.Next(ctx)
		if err != nil {
			return errors.Join(err, r.closeScan())
		}
		r.keys, r.chunkComplete, r.chunkReady = keys, complete, true
	}
	if err := r.commitDefinedChunk(ctx, job, policy, definition, r.keys, r.chunkComplete); err != nil {
		return err
	}
	r.keys, r.chunkReady = nil, false
	if r.last.State != JobBackfilling {
		return r.closeScan()
	}
	return nil
}

func (r *IndexJobRunner) commitDefinedChunk(ctx context.Context, job Job, policy IndexWritePolicy, definition JobDefinition, keys [][]byte, complete bool) error {
	transaction, err := r.begin(ctx, policy)
	if err != nil {
		return err
	}
	var next Job
	err = transaction.Statement(ctx, func(ctx context.Context) error {
		var err error
		next, err = definedBackfillChunk(ctx, transaction, r.options.Jobs, job, r.lease, policy, definition, keys, complete, r.options.Backfill)
		return err
	})
	if err != nil {
		return r.abort(ctx, transaction, nil, err)
	}
	if err := r.commit(ctx, transaction, next, nil); err != nil {
		return err
	}
	r.scanJob = next
	return nil
}

func definedBackfillChunk(ctx context.Context, transaction txn.MutationApplier, jobs *JobStore, expected Job, attempt uint64, policy IndexWritePolicy, definition JobDefinition, keys [][]byte, complete bool, limits BackfillLimits) (Job, error) {
	if transaction == nil || jobs == nil || policy.Rows == nil || limits.MaxRows == 0 || limits.MaxKeyBytes == 0 || limits.WorkBytes == 0 || uint64(len(keys)) > limits.MaxRows {
		return Job{}, errIndexBackfillChunk
	}
	current, err := jobs.Get(ctx, transaction, expected.ID)
	if err != nil {
		return Job{}, err
	}
	if !sameBackfillJob(current, expected) || current.State != JobBackfilling || current.Kind == AddIndexJob || current.Revision == math.MaxUint64 {
		return Job{}, ErrJobChanged
	}
	if err := validateJobDefinition(ctx, current, definition, jobs.options.RecordBytes); err != nil {
		return Job{}, err
	}
	memory, err := backfillJobWorkspace(jobs, limits.WorkBytes)
	if err != nil {
		return Job{}, err
	}
	if err := memory.charge(1, uint64(len(current.ResumeKey))); err != nil {
		return Job{}, err
	}
	if policy.Indexes != nil {
		if err := memory.charge(1, policy.Indexes.WorkspaceBytes()); err != nil {
			return Job{}, err
		}
	}
	if len(keys) == 0 && !complete && definition.Operation != SchemaDropIndex {
		return Job{}, errIndexBackfillChunk
	}
	remainingKeys := limits.MaxKeyBytes
	previous := current.ResumeKey
	for _, key := range keys {
		if uint64(len(key)) > remainingKeys || len(key) == 0 || bytes.Compare(key, previous) <= 0 {
			return Job{}, errIndexBackfillChunk
		}
		remainingKeys -= uint64(len(key))
		previous = key
	}
	next := current
	next.Revision++
	if complete {
		next.State = JobValidating
	}
	next.Attempt = attempt
	if len(keys) != 0 {
		next.ResumeKey = bytes.Clone(keys[len(keys)-1])
	}
	if err := validJob(next, jobs.options.RecordBytes); err != nil {
		return Job{}, err
	}
	if definition.Operation != SchemaDropIndex {
		layout := policy.Rows.Layout()
		if err := memory.charge(uint64(len(layout.Columns)), uint64(unsafe.Sizeof(model.ColumnID(0)))+uint64(unsafe.Sizeof(storage.ColumnLayout{}))+uint64(unsafe.Sizeof(model.Value{}))); err != nil {
			return Job{}, err
		}
		columns := make([]model.ColumnID, len(layout.Columns))
		for i := range layout.Columns {
			columns[i] = layout.Columns[i].ID
		}
		request := storage.ReadRequest{Target: policy.Table, ColumnIDs: columns, Layout: layout.Columns}
		targetSpec, err := definition.TargetTable.Spec(ctx)
		if err != nil || targetSpec.Table == nil {
			return Job{}, errors.Join(errJobDefinition, err)
		}
		generated := make(map[model.ColumnID]struct{}, len(targetSpec.Table.Columns))
		for _, column := range targetSpec.Table.Columns {
			if column.Generated != nil {
				generated[column.ID] = struct{}{}
			}
		}
		for _, key := range keys {
			request.Key = key
			row, err := transaction.Get(ctx, request)
			if err != nil {
				if indexRowAbsent(err) {
					continue
				}
				return Job{}, indexJobFailure(key, err)
			}
			if row.Deleted || !bytes.Equal(row.Key, key) {
				continue
			}
			if definition.Operation == SchemaAddColumn {
				if row.CommitTS > current.SnapshotTS {
					continue
				}
				ids := make([]model.ColumnID, 0, len(columns)-1)
				values := make([]model.Value, 0, len(columns)-1)
				for i, id := range columns {
					if model.ObjectID(id) == current.MutationID {
						continue
					}
					if _, skip := generated[id]; skip {
						continue
					}
					ids, values = append(ids, id), append(values, row.Values[i])
				}
				mutation := storage.Mutation{Op: storage.MutationReplace, Target: policy.Table, Key: row.Key, ColumnIDs: ids, Values: values}
				if err := transaction.ApplyMutations(ctx, []storage.Mutation{mutation}); err != nil {
					return Job{}, indexJobFailure(key, err)
				}
			} else if err := policy.Generation.Expand(ctx, transaction, nil, &row); err != nil {
				return Job{}, indexJobFailure(key, err)
			}
		}
	}
	if err := jobs.Update(ctx, transaction, current.Revision, next); err != nil {
		return Job{}, err
	}
	return next, nil
}

func (r *IndexJobRunner) readDefinition(ctx context.Context, job Job) (definition JobDefinition, err error) {
	view, err := r.options.Manager.CurrentReadView(ctx)
	if err != nil {
		return JobDefinition{}, err
	}
	defer func() { err = errors.Join(err, r.cleanup(view.Close())) }()
	return r.options.Jobs.Definition(ctx, view, job)
}

func (r *IndexJobRunner) validateDefined(ctx context.Context, job Job, policy IndexWritePolicy) (proof SchemaPublicationProof, err error) {
	view, err := r.options.Manager.CurrentReadView(ctx)
	if err != nil {
		return SchemaPublicationProof{}, err
	}
	defer func() { err = errors.Join(err, r.cleanup(view.Close())) }()
	current, err := r.options.Jobs.Get(ctx, view, job.ID)
	if err != nil || !sameBackfillJob(current, job) {
		return SchemaPublicationProof{}, errors.Join(ErrJobChanged, err)
	}
	definition, err := r.options.Jobs.Definition(ctx, view, current)
	if err != nil {
		return SchemaPublicationProof{}, err
	}
	switch definition.Operation {
	case SchemaAddColumn:
		err = r.validateDefinedTable(ctx, view, current, policy, nil)
	case SchemaDropIndex:
		_, found := policy.Indexes.index(current.MutationID)
		if !found {
			err = ErrJobChanged
		}
	case SchemaDropColumn, SchemaAlterColumnType, SchemaRewritePrimaryKey, SchemaReindex:
		err = r.validateDefinedTable(ctx, view, current, policy, policy.Generation)
	default:
		err = errJobDefinition
	}
	if err != nil {
		return SchemaPublicationProof{}, err
	}
	target, err := descriptorTarget(definition.TargetTable)
	if err != nil {
		return SchemaPublicationProof{}, err
	}
	return SchemaPublicationProof{jobs: r.options.Jobs, job: current, table: policy.Table, target: target, definition: definition}, nil
}

func (r *IndexJobRunner) validateDefinedTable(ctx context.Context, reader txn.ReadView, job Job, policy IndexWritePolicy, generation *GenerationPlan) (err error) {
	if policy.Rows == nil {
		return errIndexRunnerOptions
	}
	layout := policy.Rows.Layout()
	scanJob := job
	scanJob.State = JobBackfilling
	scanJob.SnapshotTS, scanJob.ProtectedTimestamp = reader.Timestamp(), reader.Timestamp()
	scanJob.ResumeKey = nil
	scan, err := NewTableKeyScan(ctx, reader, policy.Keys, policy.Keys, scanJob, policy.Table, layout.Columns, policy.Rows.KeyLayout(), r.options.Scan)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, scan.Close()) }()
	columns := make([]model.ColumnID, len(layout.Columns))
	for i := range layout.Columns {
		columns[i] = layout.Columns[i].ID
	}
	request := storage.ReadRequest{Target: policy.Table, ColumnIDs: columns, Layout: layout.Columns}
	var targetRequest storage.ReadRequest
	if generation != nil {
		targetRequest = storage.ReadRequest{Target: generation.target, ColumnIDs: make([]model.ColumnID, len(generation.targetLayout)), Layout: generation.targetLayout}
		for i := range generation.targetLayout {
			targetRequest.ColumnIDs[i] = generation.targetLayout[i].ID
		}
	}
	var rows uint64
	for {
		keys, complete, err := scan.Next(ctx)
		if err != nil {
			return err
		}
		for _, key := range keys {
			rows++
			if rows > r.options.Validation.MaxRows {
				return ErrJobLimit
			}
			request.Key = key
			row, err := reader.Get(ctx, request)
			if err != nil {
				return indexJobFailure(key, err)
			}
			for i, value := range row.Values {
				declared := model.TypeLayout{Type: layout.Columns[i].Type, Children: layout.Columns[i].Children}
				if err := storage.ValidateStoredValue(ctx, value, declared); err != nil {
					return indexJobFailure(key, err)
				}
			}
			if generation == nil {
				continue
			}
			expected, err := generation.transform(ctx, &row)
			if err != nil {
				return indexJobFailure(key, err)
			}
			targetRequest.Key = expected.Key
			actual, err := reader.Get(ctx, targetRequest)
			if err != nil || actual.Deleted || !reflect.DeepEqual(actual.Values, expected.Values) {
				return indexJobFailure(key, errors.Join(errIndexValidationCorrupt, err))
			}
		}
		if complete {
			break
		}
	}
	if generation != nil {
		for _, descriptor := range generation.targetIndexes.indexes {
			if _, err := generation.targetIndexes.ValidateIndex(ctx, reader, descriptor.target.ObjectID, r.options.Validation); err != nil {
				return err
			}
		}
		// A second bounded key scan detects target rows that could only arise from
		// key collisions or stale writes; dual-write remains active throughout.
		targetJob := scanJob
		targetJob.TableID = generation.target.ObjectID
		targetScan, err := NewTableKeyScan(ctx, reader, policy.Keys, policy.Keys, targetJob, generation.target, generation.targetLayout, generation.targetKeys, r.options.Scan)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, targetScan.Close()) }()
		var targetRows uint64
		for {
			keys, complete, err := targetScan.Next(ctx)
			if err != nil {
				return err
			}
			targetRows += uint64(len(keys))
			if targetRows > r.options.Validation.MaxRows {
				return ErrJobLimit
			}
			if complete {
				break
			}
		}
		if targetRows != rows {
			return errIndexValidationCorrupt
		}
	}
	return nil
}
