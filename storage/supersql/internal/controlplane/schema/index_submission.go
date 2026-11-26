package schema

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type SchemaTransactions interface {
	Begin(context.Context, txn.TransactionOptions) (*txn.Transaction, error)
}

type IndexSubmissionOptions struct {
	Manager     SchemaTransactions
	Policies    *IndexPolicyProvider
	Table       storage.ObjectRef
	Index       *model.Descriptor
	Job         Job
	Transaction txn.TransactionOptions
}

// IndexSubmission owns an intent transaction until its outcome and cleanup are
// settled. A non-nil submission must be retained even when SubmitIntent returns
// an error. Resolve and Close never resubmit the intent or cancel a durable job.
// The borrowed manager must outlive an unresolved submission.
type IndexSubmission struct {
	id          JobID
	gate        chan struct{}
	transaction *txn.Transaction
	resolution  storage.CommitResolution
	cleanupErr  error
}

// SubmitIntent validates executable index metadata before committing an absent
// descriptor and pending job atomically. It is an autocommit operation: the SQL
// session must reject an explicit transaction block before calling it. Once
// committed, a scheduler discovers the job independently of the submitting call.
func (l *IndexLifecycle) SubmitIntent(ctx context.Context, options IndexSubmissionOptions) (*IndexSubmission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil || mutationPolicyNil(options.Manager) || options.Policies == nil || options.Index == nil {
		return nil, errIndexLifecycle
	}
	provider := options.Policies
	if provider.catalog != l.catalog || provider.jobs != l.jobs || l.jobs == nil {
		return nil, errIndexLifecycle
	}
	job := options.Job
	if err := validJob(job, l.jobs.options.RecordBytes); err != nil {
		return nil, err
	}
	wrongState := job.Kind != AddIndexJob || job.State != JobPending || job.Revision != 1
	started := job.Attempt != 0 || job.SnapshotTS != 0 || len(job.ResumeKey) != 0 || len(job.FailureKey) != 0
	wrongTarget := job.TableID != options.Table.ObjectID || job.MutationID != options.Index.ID() || job.SourceVersion != options.Table.DescriptorVersion
	if wrongState || started || wrongTarget {
		return nil, ErrJobRecord
	}
	if options.Index.Kind() != model.ObjectIndex || options.Index.State() != model.StateAbsent || options.Index.Version() != 1 {
		return nil, errIndexLifecycle
	}
	for _, target := range provider.internal {
		if target.ObjectID == options.Index.ID() {
			return nil, errIndexLifecycle
		}
	}
	if err := options.Table.Validate(); err != nil {
		return nil, err
	}
	policy, err := provider.policy(ctx, options.Table.ObjectID, options.Table, nil, options.Index)
	if err != nil {
		return nil, err
	}
	transactionOptions := options.Transaction
	transactionOptions.Mode = txn.ReadWrite
	transactionOptions.Timestamp = 0
	transactionOptions.Resolver = policy.Resolver
	transactionOptions.KeyEncoder = policy.Keys
	transaction, err := options.Manager.Begin(ctx, transactionOptions)
	if transaction == nil {
		if err != nil {
			return nil, err
		}
		return nil, errIndexLifecycle
	}
	submission := &IndexSubmission{id: job.ID, gate: make(chan struct{}, 1), transaction: transaction}
	if err != nil {
		return submission, errors.Join(err, submission.closeTransaction(context.WithoutCancel(ctx)))
	}
	if err := l.CreateIntent(ctx, transaction, options.Table, options.Index, job); err != nil {
		return submission, errors.Join(err, submission.closeTransaction(context.WithoutCancel(ctx)))
	}
	result, err := transaction.Commit(ctx)
	switch transaction.State() {
	case txn.Indeterminate:
		return submission, errors.Join(storage.ErrCommitIndeterminate, err)
	case txn.Committed:
		submission.resolution = storage.CommitResolution{State: storage.CommitCommitted, Result: result}
		// A committed Commit error is cleanup failure, not permission to retry.
		submission.cleanupErr = err
	}
	return submission, errors.Join(err, submission.closeTransaction(context.WithoutCancel(ctx)))
}

func (s *IndexSubmission) ID() JobID { return s.id }

func (s *IndexSubmission) enter(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-s.gate
			return nil, err
		}
		return func() { <-s.gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Resolve only queries the original transaction outcome. CommitNotCommitted
// authorizes a caller to plan a new submission; CommitUnknown never does.
func (s *IndexSubmission) Resolve(ctx context.Context) (storage.CommitResolution, error) {
	finish, err := s.enter(ctx)
	if err != nil {
		return storage.CommitResolution{}, err
	}
	defer finish()
	if s.transaction == nil {
		return s.resolution, s.cleanupErr
	}
	if s.transaction.State() == txn.Indeterminate {
		resolution, err := s.transaction.Resolve(ctx)
		if s.transaction.State() == txn.Indeterminate {
			return resolution, errors.Join(storage.ErrCommitIndeterminate, err)
		}
		s.resolution = resolution
		if err != nil && s.cleanupErr == nil {
			s.cleanupErr = err
		}
	}
	err = s.closeTransaction(context.WithoutCancel(ctx))
	return s.resolution, err
}

// Close releases only a settled transaction. An unknown outcome keeps ownership
// intact and requires Resolve, including when the original caller was canceled.
func (s *IndexSubmission) Close(ctx context.Context) error {
	finish, err := s.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return s.closeTransaction(ctx)
}

func (s *IndexSubmission) closeTransaction(ctx context.Context) error {
	if s.transaction == nil {
		return s.cleanupErr
	}
	if s.transaction.State() == txn.Indeterminate {
		return errors.Join(storage.ErrCommitIndeterminate, s.cleanupErr)
	}
	err := s.transaction.Close(ctx)
	if s.transaction.State() == txn.Aborted {
		s.resolution = storage.CommitResolution{State: storage.CommitNotCommitted}
	}
	if err != nil {
		if s.cleanupErr == nil {
			s.cleanupErr = err
		}
		return errors.Join(s.cleanupErr, err)
	}
	s.transaction = nil
	return s.cleanupErr
}
