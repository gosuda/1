package schema

import (
	"bytes"
	"context"
	"errors"
	"math"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (r *IndexJobRunner) drive(ctx context.Context, cancelJob bool) (Job, error) {
	if r.cleanupErr != nil {
		return r.last, r.cleanupErr
	}
	if r.pending != nil {
		if r.pending.transaction.State() == txn.Indeterminate {
			return r.last, storage.ErrCommitIndeterminate
		}
		return r.last, errIndexRunnerProtectionPending
	}
	retries := 0
	var retryRevision uint64
	for {
		if err := ctx.Err(); err != nil {
			return r.last, err
		}
		job, err := r.readJob(ctx)
		if job.ID != 0 {
			r.last = job
		}
		if err != nil {
			return r.last, err
		}
		if job.Terminal() {
			r.failure = nil
			return job, indexRunnerTerminalError(job)
		}
		if job.ProtectedTimestamp != 0 {
			if err := r.options.Protections.Require(job.ID, job.ProtectedTimestamp); err != nil {
				return job, err
			}
		}
		if r.scan != nil && !sameBackfillJob(job, r.scanJob) {
			if err := r.closeScan(); err != nil {
				return job, err
			}
		}
		if retries != 0 {
			if job.Revision != retryRevision {
				retries = 0
			} else if err := r.retryWait(ctx, retries); err != nil {
				return job, err
			}
		}
		policy, err := r.options.Policies.Policy(ctx, job)
		expectedAttempt := r.attempt
		if r.claimed {
			expectedAttempt = r.lease
		}
		if err == nil && job.Attempt != expectedAttempt {
			err = ErrJobChanged
		}
		chunk := false
		if err == nil {
			chunk, err = r.step(ctx, job, policy, cancelJob)
		}
		if err == nil {
			retries = 0
			if chunk && r.options.Pace != nil {
				if err := r.options.Pace(ctx); err != nil {
					return r.last, err
				}
			}
			continue
		}
		if r.pending != nil || r.cleanupErr != nil || errors.Is(err, storage.ErrCommitIndeterminate) {
			return r.last, errors.Join(err, r.cleanupErr)
		}
		if job.State < JobPublic && r.failure == nil {
			if failure := indexRunnerPermanentFailure(err); failure != nil {
				r.failure = failure
				if ctx.Err() != nil {
					return r.last, errors.Join(err, ctx.Err())
				}
				retries = 0
				continue
			}
		}
		if ctx.Err() != nil {
			return r.last, errors.Join(err, ctx.Err())
		}
		if indexRunnerRetryable(err) {
			if retries == r.options.MaxRetries {
				return r.last, err
			}
			retries++
			retryRevision = job.Revision
			continue
		}
		return r.last, err
	}
}

func (r *IndexJobRunner) step(ctx context.Context, job Job, policy IndexWritePolicy, cancelJob bool) (bool, error) {
	if job.Kind != AddIndexJob {
		return r.stepDefined(ctx, job, policy, cancelJob)
	}
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
			next.ErrorCode = r.failure.code
			next.FailureKey = r.failure.key
		} else {
			next.ErrorCode = "57014"
		}
		return false, r.transition(ctx, job, policy, next, nil, nil)
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
		return false, r.transition(ctx, job, policy, next, pin, nil)
	case JobBackfilling:
		return true, r.backfill(ctx, job, policy)
	case JobValidating:
		proof, err := r.validate(ctx, job, policy.Indexes)
		if err != nil {
			return false, err
		}
		next.State = JobPublic
		return false, r.transition(ctx, job, policy, next, nil, &proof)
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
	return false, r.transition(ctx, job, policy, next, nil, nil)
}

func (r *IndexJobRunner) validate(ctx context.Context, job Job, indexes *IndexSet) (IndexPublicationProof, error) {
	view, err := r.options.Manager.CurrentReadView(ctx)
	if err != nil {
		return IndexPublicationProof{}, err
	}
	proof, err := indexes.ValidateJobIndex(ctx, view, r.options.Jobs, job, r.options.Validation)
	return proof, errors.Join(err, r.cleanup(view.Close()))
}

func (r *IndexJobRunner) backfill(ctx context.Context, job Job, policy IndexWritePolicy) error {
	if err := r.options.Protections.Require(job.ID, job.SnapshotTS); err != nil {
		return err
	}
	binding, found := policy.Indexes.index(job.MutationID)
	if !found {
		return ErrJobRecord
	}
	if r.scan != nil && (r.scanTable != policy.Table || r.scanIndex != binding.target) {
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
		r.scan, err = policy.Indexes.BackfillKeys(ctx, reader, policy.Keys, job, r.options.Scan)
		if err != nil {
			return errors.Join(err, r.closeScan())
		}
		r.scanJob, r.scanTable, r.scanIndex = job, policy.Table, binding.target
	}
	if !r.chunkReady {
		keys, complete, err := r.scan.Next(ctx)
		if err != nil {
			return errors.Join(err, r.closeScan())
		}
		r.keys, r.chunkComplete, r.chunkReady = keys, complete, true
	}
	transaction, err := r.begin(ctx, policy)
	if err != nil {
		return err
	}
	var next Job
	err = transaction.Statement(ctx, func(ctx context.Context) error {
		var err error
		next, err = policy.Indexes.backfillChunk(ctx, transaction, r.options.Jobs, job, r.lease, r.keys, r.chunkComplete, r.options.Backfill)
		return err
	})
	if err != nil {
		return r.abort(ctx, transaction, nil, err)
	}
	if err := r.commit(ctx, transaction, next, nil); err != nil {
		return err
	}
	r.scanJob = next
	r.keys, r.chunkReady = nil, false
	if next.State != JobBackfilling {
		return r.closeScan()
	}
	return nil
}

func (r *IndexJobRunner) closeScan() error {
	var err error
	if r.scan != nil {
		err = r.scan.Close()
		r.scan = nil
	}
	if r.scanReader != nil {
		err = errors.Join(err, r.scanReader.Close())
		r.scanReader = nil
	}
	r.keys = nil
	r.scanJob = Job{}
	r.scanTable, r.scanIndex = storage.ObjectRef{}, storage.ObjectRef{}
	r.chunkReady, r.chunkComplete = false, false
	return r.cleanup(err)
}

func (r *IndexJobRunner) retryWait(ctx context.Context, retry int) error {
	delay := r.options.RetryDelay
	for attempt := 1; attempt < retry && delay < r.options.MaxRetryDelay; attempt++ {
		if delay > r.options.MaxRetryDelay/2 {
			delay = r.options.MaxRetryDelay
		} else {
			delay *= 2
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func indexRunnerRetryable(err error) bool {
	return allCauses(err, func(err error) bool {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, storage.ErrCommitIndeterminate) {
			return false
		}
		sqlError, ok := err.(*model.Error)
		if !ok || sqlError.Code != "40001" {
			return false
		}
		var nested *model.Error
		return !errors.As(sqlError.Cause, &nested)
	})
}

func indexRunnerPermanentFailure(err error) *indexRunnerFailure {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	code := indexRunnerPermanentCode(err)
	if code == "" {
		return nil
	}
	failure := &indexRunnerFailure{code: code}
	var keyed *IndexJobFailure
	if errors.As(err, &keyed) {
		failure.key = bytes.Clone(keyed.Key)
	}
	return failure
}

func indexRunnerPermanentCode(err error) string {
	if err == nil {
		return ""
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var code string
		for _, cause := range joined.Unwrap() {
			next := indexRunnerPermanentCode(cause)
			if next == "" || (code != "" && code != next) {
				return ""
			}
			code = next
		}
		return code
	}
	if sqlError, ok := err.(*model.Error); ok {
		if sqlError == errIndexValidationCorrupt || sqlError == errIndexValidationPlan {
			return sqlError.Code
		}
		code := sqlError.Code
		if len(code) == 5 && (code[:2] == "22" || code[:2] == "23") {
			var nested *model.Error
			if errors.As(sqlError.Cause, &nested) {
				return indexRunnerPermanentCode(sqlError.Cause)
			}
			return code
		}
	}
	return indexRunnerPermanentCode(errors.Unwrap(err))
}

type indexJobTerminalError struct {
	id      JobID
	state   JobState
	failure model.Error
}

func (e *indexJobTerminalError) Error() string    { return e.failure.Error() }
func (e *indexJobTerminalError) Unwrap() error    { return &e.failure }
func (e *indexJobTerminalError) SQLState() string { return e.failure.Code }

func indexRunnerTerminalError(job Job) error {
	if job.State == JobCanceled {
		return &indexJobTerminalError{id: job.ID, state: job.State, failure: model.Error{Code: job.ErrorCode, Message: "index job canceled", Cause: context.Canceled}}
	}
	if job.State == JobFailed {
		return &indexJobTerminalError{id: job.ID, state: job.State, failure: model.Error{Code: job.ErrorCode, Message: "index job failed"}}
	}
	return nil
}
