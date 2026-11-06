package schema

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type indexRunnerCommit struct {
	transaction *txn.Transaction
	next        Job
	pin         txn.ReadView
}

func (r *IndexJobRunner) begin(ctx context.Context, policy IndexWritePolicy) (*txn.Transaction, error) {
	if policy.Resolver == nil || policy.Indexes == nil || policy.Keys == nil || policy.Table.ObjectID != r.last.TableID {
		return nil, errIndexRunnerOptions
	}
	options := r.options.Transaction
	options.Mode = txn.ReadWrite
	options.Timestamp = 0
	options.Resolver = policy.Resolver
	options.KeyEncoder = policy.Keys
	return r.options.Manager.Begin(ctx, options)
}

func (r *IndexJobRunner) abort(ctx context.Context, transaction *txn.Transaction, pin txn.ReadView, cause error) error {
	closeErr := r.cleanup(transaction.Close(context.WithoutCancel(ctx)))
	if pin != nil {
		closeErr = errors.Join(closeErr, r.cleanup(pin.Close()))
	}
	return errors.Join(cause, closeErr)
}

func (r *IndexJobRunner) commit(ctx context.Context, transaction *txn.Transaction, next Job, pin txn.ReadView) error {
	r.pending = &indexRunnerCommit{transaction: transaction, next: next, pin: pin}
	_, err := transaction.Commit(ctx)
	if transaction.State() == txn.Indeterminate {
		return errors.Join(storage.ErrCommitIndeterminate, err)
	}
	cleanupContext := context.WithoutCancel(ctx)
	if transaction.State() == txn.Committed {
		r.last = next
		r.cleanup(err)
	} else {
		err = errors.Join(err, r.cleanup(transaction.Close(cleanupContext)))
	}
	return errors.Join(err, r.finishPending(cleanupContext))
}

func (r *IndexJobRunner) resolvePending(ctx context.Context) error {
	if r.pending == nil {
		return nil
	}
	transaction := r.pending.transaction
	var err error
	if transaction.State() == txn.Indeterminate {
		_, err = transaction.Resolve(ctx)
		if transaction.State() == txn.Indeterminate {
			return errors.Join(storage.ErrCommitIndeterminate, err)
		}
		r.cleanup(err)
	}
	return errors.Join(err, r.finishPending(context.WithoutCancel(ctx)))
}

func (r *IndexJobRunner) finishPending(ctx context.Context) error {
	pending := r.pending
	if pending == nil {
		return nil
	}
	switch pending.transaction.State() {
	case txn.Indeterminate:
		return storage.ErrCommitIndeterminate
	case txn.Committed:
		r.last = pending.next
		if pending.next.Attempt == r.lease {
			r.claimed = true
		}
		if pending.pin != nil {
			if err := r.options.Protections.Retain(ctx, pending.next, pending.pin); err != nil {
				return errors.Join(errIndexRunnerProtectionPending, err)
			}
			pending.pin = nil
		}
	case txn.Aborted:
		if pending.pin != nil {
			err := r.cleanup(pending.pin.Close())
			pending.pin = nil
			r.pending = nil
			return err
		}
	default:
		return errIndexRunnerProtectionPending
	}
	r.pending = nil
	return nil
}

func (r *IndexJobRunner) transition(ctx context.Context, job Job, policy IndexWritePolicy, next Job, pin txn.ReadView, proof *IndexPublicationProof) error {
	transaction, err := r.begin(ctx, policy)
	if err != nil {
		if pin != nil {
			err = errors.Join(err, r.cleanup(pin.Close()))
		}
		return err
	}
	dependencies := make([]model.ObjectID, 0)
	if binding, found := policy.Indexes.index(job.MutationID); found {
		for _, dependency := range binding.plan.pendingDependencies {
			if dependency.Object != 0 && !slices.Contains(dependencies, dependency.Object) {
				dependencies = append(dependencies, dependency.Object)
			}
		}
	}
	change := IndexTransition{Expected: job, Next: next, Table: policy.Table, Protected: pin, Dependencies: dependencies, Validation: proof}
	if err := r.options.Lifecycle.Stage(ctx, transaction, change); err != nil {
		return r.abort(ctx, transaction, pin, err)
	}
	return r.commit(ctx, transaction, next, pin)
}
