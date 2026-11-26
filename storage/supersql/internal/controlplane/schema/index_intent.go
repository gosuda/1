package schema

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// NewAddIndexJob fixes the final public table version before the intent is
// persisted: delete-only, write-only and public each advance its write policy.
func NewAddIndexJob(id JobID, table storage.ObjectRef, index model.ObjectID) (Job, error) {
	if err := table.Validate(); err != nil {
		return Job{}, err
	}
	if id == 0 || index == 0 || index == table.ObjectID || index >= catalog.NamespaceObjectID || table.ObjectID >= catalog.NamespaceObjectID {
		return Job{}, ErrJobRecord
	}
	if table.DescriptorVersion > ^uint64(0)-3 {
		return Job{}, model.ErrInvalidDescriptor
	}
	return Job{ID: id, TableID: table.ObjectID, MutationID: index, Kind: AddIndexJob, State: JobPending,
		SourceVersion: table.DescriptorVersion, TargetVersion: table.DescriptorVersion + 3, Revision: 1}, nil
}

// CreateIntent stages the absent index definition and durable job together.
// The caller commits this transaction before scheduling a runner; cancellation
// of a later SQL waiter must not roll back an already committed intent.
func (l *IndexLifecycle) CreateIntent(ctx context.Context, transaction *txn.Transaction, table storage.ObjectRef, index *model.Descriptor, job Job) error {
	if l == nil || transaction == nil || index == nil {
		return errIndexLifecycle
	}
	if l.jobs == nil || l.catalog == nil {
		return errIndexLifecycle
	}
	if index.Kind() != model.ObjectIndex || index.State() != model.StateAbsent || index.Version() != 1 {
		return errIndexLifecycle
	}
	if index.ID() == l.jobs.Target().ObjectID || table.ObjectID == l.jobs.Target().ObjectID {
		return errIndexLifecycle
	}
	if job.Kind != AddIndexJob || job.State != JobPending || job.Revision != 1 || job.TableID != table.ObjectID || job.MutationID != index.ID() {
		return ErrJobRecord
	}
	definition, err := index.SpecWithin(ctx, l.limits)
	if err != nil {
		return err
	}
	if definition.Index == nil || definition.Index.Table != table.ObjectID {
		return errIndexLifecycle
	}
	if err := transaction.LockSchema(ctx, table); err != nil {
		return err
	}
	return transaction.Statement(ctx, func(operation context.Context) (err error) {
		dependencies, err := l.catalog.CreateDependencies(operation, index)
		if err != nil {
			return err
		}
		dependencies = append(dependencies, l.catalog.DescriptorDependency(table.ObjectID), l.jobs.Dependencies(job.ID, job.TableID))
		seen := map[model.ObjectID]bool{table.ObjectID: true}
		for _, dependency := range definition.Dependencies {
			if !seen[dependency.Object] {
				dependencies = append(dependencies, l.catalog.DescriptorDependency(dependency.Object))
				seen[dependency.Object] = true
			}
		}
		view, err := transaction.ReadView(operation, dependencies)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, view.Close()) }()
		snapshot, err := l.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, snapshot.Close()) }()
		base, err := snapshot.Descriptor(operation, table.ObjectID)
		if err != nil {
			return err
		}
		descriptor := base.Descriptor
		identity := descriptor.Physical()
		current := storage.ObjectRef{ObjectID: descriptor.ID(), Generation: identity.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: identity.EncodingVersion}
		if !tableShapedKind(descriptor.Kind()) || descriptor.State() != model.StatePublic || current != table || job.SourceVersion != descriptor.Version() {
			return catalog.ErrDescriptorChanged
		}
		change, err := l.catalog.Create(operation, snapshot, index)
		if err != nil {
			return err
		}
		if err := transaction.ApplyMutations(operation, change.Mutations); err != nil {
			return err
		}
		return l.jobs.Create(operation, transaction, job)
	})
}
