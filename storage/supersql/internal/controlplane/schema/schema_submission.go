package schema

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type DefinedSubmissionOptions struct {
	Manager    SchemaTransactions
	Policies   *IndexPolicyProvider
	Definition JobDefinition
	// ExpectedDependencies are descriptor-version preconditions checked in the
	// same transaction that commits the job intent.
	ExpectedDependencies []model.Dependency
	Job                  Job
	Transaction          txn.TransactionOptions
}

// SubmitDefinedIntent validates the complete source policy before committing a
// pending job and immutable target definition. The scheduler owns the durable
// job after commit, independently of the submitting session.
func (l *IndexLifecycle) SubmitDefinedIntent(ctx context.Context, options DefinedSubmissionOptions) (*IndexSubmission, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l == nil || mutationPolicyNil(options.Manager) || options.Policies == nil || l.jobs == nil {
		return nil, errIndexLifecycle
	}
	provider := options.Policies
	if provider.catalog != l.catalog || provider.jobs != l.jobs {
		return nil, errIndexLifecycle
	}
	job := options.Job
	if err := validJob(job, l.jobs.options.RecordBytes); err != nil {
		return nil, err
	}
	wrongState := job.Kind == AddIndexJob || job.State != JobPending || job.Revision != 1
	started := job.Attempt != 0 || job.SnapshotTS != 0 || job.ProtectedTimestamp != 0 || len(job.ResumeKey) != 0 || len(job.FailureKey) != 0
	if wrongState || started {
		return nil, ErrJobRecord
	}
	if err := validateJobDefinition(ctx, job, options.Definition, l.jobs.options.RecordBytes); err != nil {
		return nil, err
	}
	table, err := descriptorTarget(options.Definition.SourceTable)
	if err != nil {
		return nil, err
	}
	policy, err := provider.policy(ctx, job.TableID, table, &job, nil)
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
	if err := l.createDefinedIntent(ctx, transaction, job, options.Definition, options.ExpectedDependencies); err != nil {
		return submission, errors.Join(err, submission.closeTransaction(context.WithoutCancel(ctx)))
	}
	result, err := transaction.Commit(ctx)
	switch transaction.State() {
	case txn.Indeterminate:
		return submission, errors.Join(storage.ErrCommitIndeterminate, err)
	case txn.Committed:
		submission.resolution = storage.CommitResolution{State: storage.CommitCommitted, Result: result}
		submission.cleanupErr = err
	}
	return submission, errors.Join(err, submission.closeTransaction(context.WithoutCancel(ctx)))
}
