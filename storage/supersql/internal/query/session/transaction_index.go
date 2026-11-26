package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var errSessionIndexPending = &model.Error{Code: "55000", Message: "schema intent ownership requires resolution"}

// SubmitIndex commits an autocommit intent, not the index build. The SQL caller
// subsequently waits through the database scheduler. Cancellation of that wait
// cannot cancel the durable job. A returned ID with an error is not commit proof;
// unresolved ownership remains in this session until Resolve and Close succeed.
func (transactions *Transactions) SubmitIndex(ctx context.Context, lifecycle *schema.IndexLifecycle, options schema.IndexSubmissionOptions) (schema.JobID, error) {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return 0, err
	}
	defer finish()
	if err := transactions.indexAutocommit(); err != nil {
		return 0, err
	}
	return transactions.submitIndex(operation, lifecycle, options)
}

func (transactions *Transactions) indexAutocommit() error {
	if transactions.active {
		if err := transactions.ordinary(); err != nil {
			return err
		}
		return transactions.fail(settingError("25001", "online schema changes cannot run inside a transaction block"))
	}
	if transactions.settings.InTransaction() {
		return transactions.abortPair(errSessionTransactionState)
	}
	staged := *transactions.settings
	if err := staged.Begin(BeginOptions{}); err != nil {
		return err
	}
	if staged.TransactionPolicy().ReadOnly {
		return settingError("25006", "cannot create an index in a read-only transaction")
	}
	return nil
}

func (transactions *Transactions) submitIndexIntent(ctx context.Context, noMemoryMessage string, submit func(txn.TransactionOptions) (*schema.IndexSubmission, error)) (schema.JobID, error) {
	resources := transactions.settings.Resources()
	if resources.EffectiveMaintenanceMemoryBytes <= 0 {
		return 0, settingError("53200", noMemoryMessage)
	}
	options := transactions.options
	options.MemoryBytes = min(options.MemoryBytes, uint64(resources.EffectiveMaintenanceMemoryBytes))
	if resources.EffectiveTempFileBytes <= 0 {
		options.SpillBytes = 0
		options.DisableSpill = true
	} else {
		options.SpillBytes = min(options.SpillBytes, uint64(resources.EffectiveTempFileBytes))
	}
	submission, err := submit(options)
	if submission == nil {
		return 0, sessionTransactionError(err)
	}
	transactions.indexSubmission = submission
	id := submission.ID()
	err = errors.Join(err, transactions.closeIndex(context.WithoutCancel(ctx)))
	return id, sessionTransactionError(err)
}

func (transactions *Transactions) submitIndex(ctx context.Context, lifecycle *schema.IndexLifecycle, options schema.IndexSubmissionOptions) (schema.JobID, error) {
	options.Manager = transactions.manager
	return transactions.submitIndexIntent(ctx, "concurrent index build has no maintenance memory", func(transactionOptions txn.TransactionOptions) (*schema.IndexSubmission, error) {
		options.Transaction = transactionOptions
		return lifecycle.SubmitIntent(ctx, options)
	})
}

func (transactions *Transactions) submitDefined(ctx context.Context, lifecycle *schema.IndexLifecycle, options schema.DefinedSubmissionOptions) (schema.JobID, error) {
	options.Manager = transactions.manager
	return transactions.submitIndexIntent(ctx, "online schema change has no maintenance memory", func(transactionOptions txn.TransactionOptions) (*schema.IndexSubmission, error) {
		options.Transaction = transactionOptions
		return lifecycle.SubmitDefinedIntent(ctx, options)
	})
}

func (transactions *Transactions) resolveIndex(ctx context.Context) (storage.CommitResolution, error) {
	resolution, err := transactions.indexSubmission.Resolve(ctx)
	if resolution.State == storage.CommitUnknown {
		return resolution, errors.Join(storage.ErrCommitIndeterminate, err)
	}
	err = errors.Join(err, transactions.closeIndex(context.WithoutCancel(ctx)))
	return resolution, err
}

func (transactions *Transactions) closeIndex(ctx context.Context) error {
	if transactions.indexSubmission == nil {
		return nil
	}
	if err := transactions.indexSubmission.Close(ctx); err != nil {
		return err
	}
	transactions.indexSubmission = nil
	return nil
}
