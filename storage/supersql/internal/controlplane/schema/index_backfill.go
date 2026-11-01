package schema

import (
	"bytes"
	"context"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// MaxRows and MaxKeyBytes bound the entire borrowed key chunk. WorkBytes bounds
// row, entry, owner, projection and checkpoint workspace; zero admits nothing.
// Native reads, staged mutations and expression internals retain their own quotas.
type BackfillLimits struct {
	MaxRows     uint64
	MaxKeyBytes uint64
	WorkBytes   uint64
}

var errIndexBackfillChunk = &model.Error{Code: "22023", Message: "invalid index backfill chunk", Cause: storage.ErrInvalidMutation}

// BackfillChunk stages latest-row index entries and their durable checkpoint in
// the caller's statement. Errors require rollback of that statement; the returned
// job is not committed. Resolve an indeterminate commit before retrying a chunk.
// Keys select work at the job's protected snapshot, never supply row values.
func (s *IndexSet) BackfillChunk(ctx context.Context, transaction txn.MutationApplier, jobs *JobStore, expected Job, keys [][]byte, complete bool, limits BackfillLimits) (Job, error) {
	return s.backfillChunk(ctx, transaction, jobs, expected, expected.Attempt, keys, complete, limits)
}

func (s *IndexSet) backfillChunk(ctx context.Context, transaction txn.MutationApplier, jobs *JobStore, expected Job, attempt uint64, keys [][]byte, complete bool, limits BackfillLimits) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if s == nil || s.table == nil || transaction == nil || jobs == nil || jobs.normalizer == nil {
		return Job{}, errIndexBackfillChunk
	}
	if limits.MaxRows == 0 || limits.MaxKeyBytes == 0 || limits.WorkBytes == 0 || limits.WorkBytes > math.MaxInt || uint64(len(keys)) > limits.MaxRows {
		return Job{}, errIndexSetLimit
	}
	memory, err := backfillJobWorkspace(jobs, limits.WorkBytes)
	if err != nil {
		return Job{}, err
	}
	current, err := jobs.Get(ctx, transaction, expected.ID)
	if err != nil {
		return Job{}, err
	}
	if !sameBackfillJob(current, expected) {
		return Job{}, ErrJobChanged
	}
	if current.Kind != AddIndexJob || current.State != JobBackfilling || current.Revision == math.MaxUint64 || current.SnapshotTS <= 0 || current.ProtectedTimestamp != current.SnapshotTS {
		return Job{}, ErrJobTransition
	}
	binding, found := s.index(current.MutationID)
	if !found || binding.plan == nil || binding.target != binding.plan.target {
		return Job{}, errIndexSetDescriptor
	}
	physical := s.table.Physical()
	target := storage.ObjectRef{ObjectID: s.table.ID(), Generation: physical.Generation, DescriptorVersion: s.table.Version(), EncodingVersion: physical.EncodingVersion}
	invalidTable := !tableShapedKind(s.table.Kind()) || s.table.State() != model.StatePublic
	wrongTarget := s.target != target || s.target.ObjectID != current.TableID
	wrongPlan := binding.plan.tableID != current.TableID || binding.plan.tableVersion != s.target.DescriptorVersion
	if invalidTable || wrongTarget || wrongPlan {
		return Job{}, errIndexSetDescriptor
	}
	if binding.plan.State() != model.StateWriteOnly && binding.plan.State() != model.StatePublic {
		return Job{}, ErrJobTransition
	}
	if len(keys) == 0 && !complete {
		return Job{}, errIndexBackfillChunk
	}
	remainingKeys := limits.MaxKeyBytes
	previous := current.ResumeKey
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return Job{}, err
		}
		if uint64(len(key)) > remainingKeys {
			return Job{}, errIndexSetLimit
		}
		remainingKeys -= uint64(len(key))
		if len(key) == 0 || bytes.Compare(key, previous) <= 0 {
			return Job{}, errIndexBackfillChunk
		}
		previous = key
	}
	next := current
	next.Revision++
	next.Attempt = attempt
	if complete {
		next.State = JobValidating
	}
	if len(keys) != 0 {
		next.ResumeKey = keys[len(keys)-1]
	}
	if uint64(len(next.ResumeKey)) > jobs.options.KeyBytes || uint64(len(next.FailureKey)) > jobs.options.KeyBytes {
		return Job{}, ErrJobLimit
	}
	if err := validJob(next, jobs.options.RecordBytes); err != nil {
		return Job{}, err
	}
	if err := memory.charge(1, uint64(len(next.ResumeKey))); err != nil {
		return Job{}, err
	}
	if len(keys) != 0 {
		if err := memory.charge(1, s.workspaceBytes); err != nil {
			return Job{}, err
		}
		if err := memory.charge(1, binding.plan.entryBytes()); err != nil {
			return Job{}, err
		}
		if err := memory.charge(uint64(len(binding.plan.baseColumns)), uint64(unsafe.Sizeof(model.ColumnID(0)))); err != nil {
			return Job{}, err
		}
		// The checkpoint cannot alias the scan batch that the caller may reuse.
		next.ResumeKey = bytes.Clone(next.ResumeKey)
		columns := make([]model.ColumnID, len(binding.plan.baseColumns))
		for i, column := range binding.plan.baseColumns {
			if err := ctx.Err(); err != nil {
				return Job{}, err
			}
			columns[i] = column.ID
		}
		request := storage.ReadRequest{Target: s.target, ColumnIDs: columns, Layout: binding.plan.baseColumns}
		for _, key := range keys {
			if err := ctx.Err(); err != nil {
				return Job{}, err
			}
			request.Key = key
			row, err := transaction.Get(ctx, request)
			if err != nil {
				if indexRowAbsent(err) {
					continue
				}
				return Job{}, indexJobFailure(key, err)
			}
			if err := ctx.Err(); err != nil {
				return Job{}, err
			}
			if row.Deleted {
				continue
			}
			if !bytes.Equal(row.Key, key) {
				return Job{}, indexJobFailure(key, &model.Error{Code: "XX001", Message: "index backfill read returned a different base key", Cause: storage.ErrCorruptStorage})
			}
			if err := binding.expand(ctx, transaction, nil, &row, s.limits.EntryBytes); err != nil {
				return Job{}, indexJobFailure(key, err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	if err := jobs.Update(ctx, transaction, current.Revision, next); err != nil {
		return Job{}, err
	}
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	return next, nil
}

func sameBackfillJob(current, expected Job) bool {
	return current.Revision == expected.Revision &&
		current.ID == expected.ID && current.TableID == expected.TableID && current.MutationID == expected.MutationID &&
		current.Kind == expected.Kind && current.State == expected.State &&
		current.SourceVersion == expected.SourceVersion && current.TargetVersion == expected.TargetVersion &&
		current.SnapshotTS == expected.SnapshotTS && current.ProtectedTimestamp == expected.ProtectedTimestamp &&
		current.Attempt == expected.Attempt && current.ErrorCode == expected.ErrorCode &&
		bytes.Equal(current.ResumeKey, expected.ResumeKey) && bytes.Equal(current.FailureKey, expected.FailureKey)
}

func backfillJobWorkspace(jobs *JobStore, maximum uint64) (indexSetBudget, error) {
	memory := indexSetBudget{remaining: maximum}
	// Update overlaps its encoded successor, locked predecessor and active-job
	// check with this call's owned job. Codec results have separate key limits.
	if err := memory.charge(8, jobs.options.RecordBytes); err != nil {
		return indexSetBudget{}, err
	}
	if err := memory.charge(4, jobs.options.KeyBytes); err != nil {
		return indexSetBudget{}, err
	}
	fixed := 4*uint64(unsafe.Sizeof(Job{})) + 4*uint64(unsafe.Sizeof(model.Row{})) +
		4*uint64(unsafe.Sizeof(storage.ReadRequest{})) + 8*uint64(unsafe.Sizeof(model.Value{})) +
		8*uint64(unsafe.Sizeof(storage.ColumnLayout{})) + 2*uint64(unsafe.Sizeof(storage.Mutation{})) +
		8*uint64(unsafe.Sizeof(model.ColumnID(0))) + 4*9
	if err := memory.charge(1, fixed); err != nil {
		return indexSetBudget{}, err
	}
	return memory, nil
}
