package session

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// Dependencies must cover binding-time namespace and descriptor reads, including
// absence and OWNED BY lookups. Publication adds exact write dependencies.
type SequenceCommandOptions struct {
	Binding      bind.CreateDeclarationOptions
	Dependencies []txn.ReadDependency
}
type SequenceCommandResult struct {
	Tag        string
	Descriptor *model.Descriptor
	Warnings   []model.Error
	Commit     EndResult
}
type sequenceIdentityAllocator struct {
	jobs        *schema.JobStore
	transaction *txn.Transaction
}

func (a sequenceIdentityAllocator) AllocateDeclarationIdentity(ctx context.Context) (model.ObjectID, model.OID, error) {
	allocated, err := a.jobs.AllocateIdentity(ctx, a.transaction)
	return allocated.ObjectID, allocated.OID, err
}

func ExecuteCreateSequence(ctx context.Context, t *Transactions, statement *zpgparse.CreateSeqStmt, options SequenceCommandOptions) (SequenceCommandResult, error) {
	if statement == nil {
		return SequenceCommandResult{}, settingError("42601", "missing CREATE SEQUENCE statement")
	}
	if statement.Sequence != nil && statement.Sequence.Relpersistence == "t" {
		node := proto.Clone(&zpgparse.Node{Node: &zpgparse.Node_CreateSeqStmt{CreateSeqStmt: statement}}).(*zpgparse.Node)
		if err := qualifyTemporaryObject(node); err != nil {
			return SequenceCommandResult{}, err
		}
		statement = node.GetCreateSeqStmt()
	}
	return executeSequenceDDL(ctx, t, statement, nil, options)
}
func ExecuteAlterSequence(ctx context.Context, t *Transactions, statement *zpgparse.AlterSeqStmt, options SequenceCommandOptions) (SequenceCommandResult, error) {
	if statement == nil {
		return SequenceCommandResult{}, settingError("42601", "missing ALTER SEQUENCE statement")
	}
	return executeSequenceDDL(ctx, t, nil, statement, options)
}

func executeSequenceDDL(ctx context.Context, t *Transactions, create *zpgparse.CreateSeqStmt, alter *zpgparse.AlterSeqStmt, options SequenceCommandOptions) (result SequenceCommandResult, err error) {
	if t == nil {
		return result, errSessionTransactionConfig
	}
	operation, finish, err := t.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if t.sequences == nil {
		return result, t.fail(settingError("0A000", "sequence DDL services are not configured"))
	}
	automatic := !t.active
	if automatic {
		if err := t.begin(operation, TransactionStart{}); err != nil {
			return result, err
		}
	}
	if err := t.ordinary(); err != nil {
		if automatic && t.active {
			err = errors.Join(err, t.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	mark := len(t.sequences.changes)
	temporaryMark := len(t.named.temporaryChanges)
	complete := false
	defer func() {
		if !complete {
			t.sequences.abandon(mark)
			t.markFailed(errSessionStatementPanic)
			if automatic {
				t.closeError = errors.Join(t.closeError, t.endRollback(context.WithoutCancel(operation)))
			}
		}
	}()
	run := func() error {
		if t.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot execute sequence DDL in a read-only transaction")
		}
		options.Binding.Bind.SearchPath = declarationSearchPath(t.settings.SearchPath(), options.Binding.SchemaContext)
		if options.Binding.SchemaContext == "" && len(t.named.temporary) != 0 && !slices.Contains(options.Binding.Bind.SearchPath, temporarySchemaName) {
			options.Binding.Bind.SearchPath = append([]string{temporarySchemaName}, options.Binding.Bind.SearchPath...)
		}
		options.Binding.Owner = t.settings.Principal().OID()
		options.Binding.Bind.Builtin = t.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		options.Binding.Bind.Temporal = t.temporal
		options.Binding.Identities = sequenceIdentityAllocator{jobs: t.sequences.config.Identities, transaction: t.transaction}
		var phase sequenceDDLBinding
		if alter != nil {
			var err error
			phase, err = t.prepareAlterSequence(operation, alter, options)
			if err != nil {
				return err
			}
			if base := phase.definition.Base(); base != nil {
				physical := base.Physical()
				target := storage.ObjectRef{ObjectID: base.ID(), Generation: physical.Generation, DescriptorVersion: base.Version(), EncodingVersion: physical.EncodingVersion}
				if err := t.transaction.LockSchema(operation, target); err != nil {
					return err
				}
				if phase.definition.Configuration().Persistence != "t" {
					if err := t.sequences.config.Catalog.LockDescriptor(operation, t.transaction, base.ID()); err != nil {
						return err
					}
				}
			}
		}
		return t.routineAtomicStatement(operation, func(ctx context.Context) error {
			if create != nil && create.Sequence != nil && create.Sequence.Relpersistence == "t" {
				if err := t.ensureTemporarySchema(ctx, &options.Binding); err != nil {
					return err
				}
			}
			if create != nil {
				bound, err := t.bindSequenceDDL(ctx, create, nil, options)
				if err != nil {
					return err
				}
				phase.definition = bound
			}
			bound := phase.definition
			result.Warnings = bound.Warnings()
			if create != nil {
				result.Tag = "CREATE SEQUENCE"
			} else {
				result.Tag = "ALTER SEQUENCE"
			}
			if bound.Skipped() {
				return nil
			}
			descriptor, err := t.stageSequenceDDL(ctx, bound, phase.ownershipError)
			result.Descriptor = descriptor
			return err
		})
	}
	err = run()
	err = errors.Join(err, t.markDataAccess())
	complete = true
	if err != nil {
		t.sequences.abandon(mark)
		t.markFailed(err)
		t.named.rollbackTemporary(temporaryMark)
		if automatic {
			err = errors.Join(err, t.endRollback(context.WithoutCancel(operation)))
		} else {
			err = errors.Join(err, t.sequences.clean(context.WithoutCancel(operation)))
		}
		return result, err
	}
	if automatic {
		result.Commit, err = t.commit(operation, false)
	}
	return result, err
}

func (t *Transactions) bindSequenceDDL(ctx context.Context, create *zpgparse.CreateSeqStmt, alter *zpgparse.AlterSeqStmt, options SequenceCommandOptions) (*bind.BoundSequenceDefinition, error) {
	view, err := t.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := t.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	if err := addTemporaryOverlay(snapshot, &t.named); err != nil {
		return nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	var bound *bind.BoundSequenceDefinition
	if create != nil {
		bound, err = bind.BindCreateSequence(ctx, create, snapshot, options.Binding)
	} else {
		bound, err = bind.BindAlterSequence(ctx, alter, snapshot, options.Binding)
	}
	return bound, errors.Join(err, snapshot.Close(), view.Close())
}
func (t *Transactions) stageSequenceDDL(ctx context.Context, bound *bind.BoundSequenceDefinition, ownershipError error) (*model.Descriptor, error) {
	configuration := bound.Configuration()
	incarnation := schema.SequenceIncarnation(configuration.Incarnation)
	index := -1
	if bound.RewritesState() {
		var err error
		index, err = t.sequences.reserve(incarnation)
		if err != nil {
			return nil, err
		}
		config := schema.SequenceConfig{Start: configuration.Start, Increment: configuration.Increment, Min: configuration.Min, Max: configuration.Max, Cache: configuration.Cache, Cycle: configuration.Cycle}
		var state schema.SequenceState
		if bound.Base() == nil {
			state, err = t.sequences.config.Service.Initialize(ctx, config)
		} else {
			var restart *int64
			if value, requested := bound.Restart(); requested {
				restart = &value
			}
			state, err = t.sequences.config.Service.Reconfigure(ctx, incarnation, config, restart)
		}
		change := &t.sequences.changes[index]
		change.created = state.Incarnation
		if err != nil {
			var pending *schema.PendingSequenceError
			if errors.As(err, &pending) {
				change.initialization = pending.Transaction
			}
			return nil, err
		}
		incarnation = state.Incarnation
	}
	descriptor, err := bound.Finalize(ctx, uint64(incarnation))
	if err != nil {
		return nil, err
	}
	if base := bound.Base(); base != nil {
		reference := SequenceReference{Object: base.ID(), Identity: schema.SequenceIncarnation(configuration.Identity), Incarnation: schema.SequenceIncarnation(configuration.Incarnation)}
		if err := t.sequences.config.Session.InvalidateLease(ctx, reference); err != nil {
			return nil, err
		}
	}
	if ownershipError != nil {
		return nil, ownershipError
	}
	if configuration.Persistence == "t" {
		if err := t.named.setTemporary(descriptor); err != nil {
			return nil, err
		}
	} else if err := t.publishSequenceDescriptor(ctx, bound, descriptor); err != nil {
		return nil, err
	}
	if index >= 0 {
		t.sequences.changes[index].published = true
	}
	return descriptor, nil
}
func (t *Transactions) publishSequenceDescriptor(ctx context.Context, bound *bind.BoundSequenceDefinition, descriptor *model.Descriptor) error {
	store := t.sequences.config.Catalog
	dependencies, err := store.CreateDependencies(ctx, descriptor)
	if err != nil {
		return err
	}
	external := bound.Dependencies()
	for _, dep := range external {
		dependencies = append(dependencies, store.DescriptorDependency(dep.Object))
	}
	view, err := t.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	err = func() error {
		for _, dep := range external {
			current, err := snapshot.Descriptor(ctx, dep.Object)
			if err != nil {
				return err
			}
			if current.Descriptor.Version() != dep.Version || current.Descriptor.State() != model.StatePublic {
				return catalog.ErrDescriptorChanged
			}
		}
		var change catalog.Change
		var err error
		if base := bound.Base(); base != nil {
			change, err = store.Update(ctx, snapshot, base.Version(), descriptor)
		} else {
			change, err = store.Create(ctx, snapshot, descriptor)
		}
		if err != nil {
			return err
		}
		return t.transaction.ApplyMutations(ctx, change.Mutations)
	}()
	return errors.Join(err, snapshot.Close(), view.Close())
}
