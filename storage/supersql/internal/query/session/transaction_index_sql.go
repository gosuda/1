package session

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type ConcurrentIndexOptions struct {
	Binding   bind.CreateIndexOptions
	JobID     schema.JobID
	Lifecycle *schema.IndexLifecycle
	Policies  *schema.IndexPolicyProvider
	Scheduler *schema.IndexJobScheduler
}

type IndexCommandResult struct {
	Tag     string
	Warning *model.Error
	// On a submission error, only ID may be populated; State zero is not a
	// durable job record. Transactions.Resolve settles the retained intent.
	Job schema.Job
}

var errConcurrentIndexOptions = &model.Error{Code: "22023", Message: "invalid concurrent index command options"}

// ExecuteCreateIndexConcurrently borrows one binding snapshot and database-owned
// schema services. All services, snapshot and Transactions must belong to the same
// database. Assigned IDs must already be reserved durably by the database owner.
// The session remains serialized through the terminal wait; cancellation after
// intent commit ends only this call, never the database scheduler or its job.
func ExecuteCreateIndexConcurrently(ctx context.Context, transactions *Transactions, statement *zpgparse.IndexStmt, snapshot bind.CatalogSnapshot, options ConcurrentIndexOptions) (result IndexCommandResult, err error) {
	if statement == nil {
		return result, settingError("42601", "missing index statement")
	}
	missingSession := transactions == nil || snapshot == nil
	invalidCommand := !statement.Concurrent || options.JobID == 0
	missingServices := options.Lifecycle == nil || options.Policies == nil || options.Scheduler == nil
	if missingSession || invalidCommand || missingServices {
		return result, errConcurrentIndexOptions
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if err := transactions.indexAutocommit(); err != nil {
		return result, err
	}
	resources := transactions.settings.Resources()
	if resources.EffectiveMaintenanceMemoryBytes <= 0 {
		return result, settingError("53200", "concurrent index build has no maintenance memory")
	}
	options.Binding.Bind.Limits.MaxBytes = min(options.Binding.Bind.Limits.MaxBytes, uint64(resources.EffectiveMaintenanceMemoryBytes))
	options.Binding.Descriptors.MaxBytes = min(options.Binding.Descriptors.MaxBytes, uint64(resources.EffectiveMaintenanceMemoryBytes))
	options.Binding.Bind.Temporal = transactions.temporal
	bound, err := bind.BindCreateIndex(operation, statement, snapshot, options.Binding)
	if err != nil {
		return result, sessionTransactionError(err)
	}
	if bound.Existing != nil {
		result.Tag = "CREATE INDEX"
		result.Warning = &model.Error{Code: "42P07", Message: "relation already exists, skipping"}
		return result, nil
	}
	if bound.Table == nil || bound.Index == nil {
		return result, errConcurrentIndexOptions
	}
	physical := bound.Table.Physical()
	table := storage.ObjectRef{ObjectID: bound.Table.ID(), Generation: physical.Generation, DescriptorVersion: bound.Table.Version(), EncodingVersion: physical.EncodingVersion}
	job, err := schema.NewAddIndexJob(options.JobID, table, bound.Index.ID())
	if err != nil {
		return result, err
	}
	id, err := transactions.submitIndex(operation, options.Lifecycle, schema.IndexSubmissionOptions{
		Policies: options.Policies, Table: table, Index: bound.Index, Job: job,
	})
	result.Job.ID = id
	if err != nil {
		return result, err
	}
	completed, err := options.Scheduler.Wait(operation, id)
	if completed.ID != 0 {
		result.Job = completed
	}
	if err != nil {
		return result, sessionTransactionError(err)
	}
	result.Tag = "CREATE INDEX"
	return result, nil
}
