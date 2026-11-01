package session

import (
	"context"
	"errors"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type TableCommandOptions struct {
	Binding         bind.CreateDeclarationOptions
	Dependencies    []txn.ReadDependency
	Columns         ColumnExpressionOptions
	Indexes         IndexExpressionOptions
	Limits          schema.IndexLimits
	PolicyBytes     uint64
	Constraints     *schema.IndexPolicyProvider
	LookupTemporary bool
	populate        func(context.Context, *bind.BoundDeclaration) (uint64, error)
}

type TableCommandResult struct {
	RowsAffected uint64
	Tag          string
	Descriptor   *model.Descriptor
	Mutations    *DeclarationResult
	Warnings     []model.Error
	Commit       EndResult
}

type declarationCommandBinder func(context.Context, bind.CatalogSnapshot, bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error)

func ExecuteCreateTable(ctx context.Context, t *Transactions, statement *zpgparse.CreateStmt, options TableCommandOptions) (TableCommandResult, error) {
	if statement == nil {
		return TableCommandResult{}, settingError("42601", "missing CREATE TABLE statement")
	}
	temporary := statement.Relation != nil && statement.Relation.Relpersistence == "t"
	if temporary {
		node := proto.Clone(&zpgparse.Node{Node: &zpgparse.Node_CreateStmt{CreateStmt: statement}}).(*zpgparse.Node)
		if err := qualifyTemporaryObject(node); err != nil {
			return TableCommandResult{}, err
		}
		statement = node.GetCreateStmt()
	}
	return executeTableDeclaration(ctx, t, options, "CREATE TABLE", temporary, func(ctx context.Context, snapshot bind.CatalogSnapshot, options bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		return bind.BindCreateTable(ctx, statement, snapshot, options)
	})
}
func ExecuteAlterTableIdentity(ctx context.Context, t *Transactions, statement *zpgparse.AlterTableStmt, options TableCommandOptions) (TableCommandResult, error) {
	if statement == nil {
		return TableCommandResult{}, settingError("42601", "missing ALTER TABLE statement")
	}
	options.LookupTemporary = true
	return executeTableDeclaration(ctx, t, options, "ALTER TABLE", false, func(ctx context.Context, snapshot bind.CatalogSnapshot, options bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		return bind.BindAlterTableIdentity(ctx, statement, snapshot, options)
	})
}

func ExecuteCreateSchema(ctx context.Context, t *Transactions, statement *zpgparse.CreateSchemaStmt, options TableCommandOptions) (TableCommandResult, error) {
	if statement == nil {
		return TableCommandResult{}, settingError("42601", "missing CREATE SCHEMA statement")
	}
	return executeTableDeclaration(ctx, t, options, "CREATE SCHEMA", false, func(ctx context.Context, snapshot bind.CatalogSnapshot, options bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		return bind.BindCreateSchema(ctx, statement, snapshot, options)
	})
}
func executeTableDeclaration(ctx context.Context, t *Transactions, options TableCommandOptions, tag string, temporary bool, bindCommand declarationCommandBinder) (result TableCommandResult, err error) {
	if t == nil {
		return result, errSessionTransactionConfig
	}
	operation, finish, err := t.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if t.sequences == nil {
		return result, t.fail(settingError("0A000", "catalog DDL services are not configured"))
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
	err = t.routineAtomicStatement(operation, func(ctx context.Context) (statementErr error) {
		if t.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot execute catalog DDL in a read-only transaction")
		}
		temporaryMark := len(t.named.temporaryChanges)
		defer func() {
			if statementErr != nil {
				t.named.rollbackTemporary(temporaryMark)
			}
		}()
		options.Binding.Bind.SearchPath = declarationSearchPath(t.settings.SearchPath(), options.Binding.SchemaContext)
		options.Binding.Owner = t.settings.Principal().OID()
		options.Binding.Bind.Builtin = t.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		options.Binding.Bind.Temporal = t.temporal
		options.Binding.Identities = sequenceIdentityAllocator{jobs: t.sequences.config.Identities, transaction: t.transaction}
		options.Binding.Encoder = t.options.KeyEncoder
		if temporary {
			if err := t.ensureTemporarySchema(ctx, &options.Binding); err != nil {
				return err
			}
		}
		if (temporary || options.LookupTemporary) && !slices.Contains(options.Binding.Bind.SearchPath, temporarySchemaName) {
			options.Binding.Bind.SearchPath = append([]string{temporarySchemaName}, options.Binding.Bind.SearchPath...)
		}
		// Binding a declaration resolves catalog objects the statement names, such
		// as the parent of a foreign key, from the namespace collection. Widen the
		// declared reads with the discovery set so those lookups are covered; the
		// caller's dependencies describe only the objects the statement creates.
		bindDependencies, err := catalogDiscoveryDependencies(ctx, t.transaction, t.sequences.config.Catalog, catalogDiscovery{}, options.Dependencies, options.Binding.Bind.Limits.MaxBytes)
		if err != nil {
			return err
		}
		view, err := t.transaction.ReadView(ctx, bindDependencies)
		if err != nil {
			return err
		}
		snapshot, err := t.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		if err := addTemporaryOverlay(snapshot, &t.named); err != nil {
			return errors.Join(err, snapshot.Close(), view.Close())
		}
		if err := t.activateDeclaredTypes(ctx, snapshot, options.Binding.Bind.Builtin); err != nil {
			return errors.Join(err, snapshot.Close(), view.Close())
		}
		bound, err := bindCommand(ctx, snapshot, options.Binding)
		if err == nil {
			bound, err = t.finalizeTableSequences(ctx, snapshot, bound)
		}
		if err != nil {
			return errors.Join(err, snapshot.Close(), view.Close())
		}
		if bound.RequiresPartitionValidation() && options.populate == nil {
			return errors.Join(settingError("22023", "partition validation services are not configured"), snapshot.Close(), view.Close())
		}
		if bound.Existing() == nil && len(bound.Descriptors()) == 0 && len(bound.Updates()) == 0 {
			if len(bound.Warnings()) == 0 {
				return errors.Join(model.ErrInvalidDescriptor, snapshot.Close(), view.Close())
			}
			result.Tag, result.Warnings = tag, bound.Warnings()
			return errors.Join(snapshot.Close(), view.Close())
		}
		if validations := bound.Validations(); len(validations) != 0 {
			if options.Constraints == nil {
				return errors.Join(settingError("22023", "constraint validation services are not configured"), snapshot.Close(), view.Close())
			}
			if err := snapshot.AddSessionOverlay(bound.Descriptors()); err != nil {
				return errors.Join(err, snapshot.Close(), view.Close())
			}
			updates := bound.Updates()
			tableUpdate := -1
			for i := range updates {
				if updates[i].Base != nil && updates[i].Next != nil && updates[i].Next.Kind() == model.ObjectTable {
					tableUpdate = i
					break
				}
			}
			if tableUpdate < 0 {
				return errors.Join(model.ErrInvalidDescriptor, snapshot.Close(), view.Close())
			}
			for _, validation := range validations {
				update := updates[tableUpdate]
				if err := options.Constraints.ValidateConstraint(ctx, t.transaction, snapshot, update.Base.Version(), update.Base, update.Next, validation); err != nil {
					return errors.Join(err, snapshot.Close(), view.Close())
				}
			}
		}
		stageTemporary := temporary
		if !stageTemporary {
			for _, update := range bound.Updates() {
				if update.Next != nil {
					spec, specErr := update.Next.SpecWithin(ctx, options.Binding.Descriptors)
					if specErr != nil {
						return errors.Join(specErr, snapshot.Close(), view.Close())
					}
					if spec.Table != nil && spec.Table.Persistence == "t" {
						stageTemporary = true
						break
					}
				}
			}
		}
		columns := options.Columns
		columns.Builtin = t.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		columns.Temporal = t.temporal
		declarationOptions := DeclarationOptions{Catalog: t.sequences.config.Catalog, Encoder: t.options.KeyEncoder, Columns: columns, Indexes: options.Indexes, Limits: options.Limits, PolicyBytes: options.PolicyBytes}
		if stageTemporary {
			if err := t.named.applyTemporaryDeclaration(bound); err != nil {
				return errors.Join(err, snapshot.Close(), view.Close())
			}
			if err := addTemporaryOverlay(snapshot, &t.named); err != nil {
				return errors.Join(err, snapshot.Close(), view.Close())
			}
			var temporary *DeclarationResult
			temporary, err = admitDeclaration(ctx, t.transaction, snapshot, bound.Descriptors(), declarationOptions)
			result.Mutations = temporary
			if err == nil {
				t.named.setTemporaryTargets(temporary)
			}
			maximum := options.Binding.Descriptors.MaxItems
			if maximum < 1 {
				maximum = 1
			}
			objectOptions := ObjectCommandOptions{Binding: options.Binding, Dependencies: options.Dependencies, Columns: options.Columns, Indexes: options.Indexes, Limits: options.Limits, PolicyBytes: options.PolicyBytes, MaxDescriptors: uint32(maximum)}
			t.named.temporaryOptions = &objectOptions
		} else {
			result.Mutations, err = StageDeclaration(ctx, t.transaction, bound, declarationOptions)
		}
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			return err
		}
		if options.populate != nil {
			result.RowsAffected, err = options.populate(ctx, bound)
			if err != nil {
				return err
			}
		}
		for i := mark; i < len(t.sequences.changes); i++ {
			t.sequences.changes[i].published = true
		}
		result.Tag, result.Warnings = tag, bound.Warnings()
		result.Descriptor = bound.Existing()
		if descriptors := bound.Descriptors(); len(descriptors) != 0 {
			result.Descriptor = descriptors[0]
		}
		return nil
	})
	err = errors.Join(err, t.markDataAccess())
	complete = true
	if err != nil {
		t.sequences.abandon(mark)
		t.markFailed(err)
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
func (t *Transactions) finalizeTableSequences(ctx context.Context, snapshot *catalog.CatalogSnapshot, bound *bind.BoundDeclaration) (*bind.BoundDeclaration, error) {
	definitions := bound.PendingSequences()
	if len(definitions) == 0 {
		return bound, nil
	}
	if len(definitions) > cap(t.sequences.changes)-len(t.sequences.changes) {
		return nil, &model.Error{Code: "54000", Message: "owned sequence declaration limit exceeded"}
	}
	descriptors := make([]*model.Descriptor, len(definitions))
	for i, definition := range definitions {
		index, err := t.sequences.reserve(0)
		if err != nil {
			return nil, err
		}
		config := definition.Configuration()
		state, err := t.sequences.config.Service.Initialize(ctx, schema.SequenceConfig{Start: config.Start, Increment: config.Increment, Min: config.Min, Max: config.Max, Cache: config.Cache, Cycle: config.Cycle})
		change := &t.sequences.changes[index]
		change.created = state.Incarnation
		if err != nil {
			var pending *schema.PendingSequenceError
			if errors.As(err, &pending) {
				change.initialization = pending.Transaction
			}
			return nil, err
		}
		descriptors[i], err = definition.Finalize(ctx, uint64(state.Incarnation))
		if err != nil {
			return nil, err
		}
	}
	return bound.FinalizeSequences(ctx, snapshot, descriptors)
}
