package session

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type TypeCommandOptions struct {
	Binding        bind.CreateDeclarationOptions
	Dependencies   []txn.ReadDependency
	MaxDescriptors uint32
	Cascade        TypeCascadeOptions
	Validation     TypeDomainValidationOptions
}

type TypeCascadeOptions struct {
	Policies *schema.IndexPolicyProvider
	Scan     schema.BackfillScanLimits
}

type typeCascadeRewrite struct {
	Plan  *schema.GenerationPlan
	Codec schema.KeyCodec
}

type TypeCommandResult struct {
	Tag      string
	Warnings []model.Error
	Commit   EndResult
}

func (transactions *Transactions) runTypeCommand(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution) (ExecutionResult, bool, error) {
	if statement == nil || statement.Stmt == nil || !isTypeCommand(statement.Stmt) {
		return ExecutionResult{}, false, nil
	}
	if options.Execution.TypeDDL == nil {
		return ExecutionResult{}, true, errPreparedInvocation
	}
	if len(options.Parameters) != 0 || len(options.Execution.Environment.Parameters) != 0 {
		return ExecutionResult{}, true, errors.Join(settingError("42P02", "type command does not accept parameters"), rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	if err := rejectPreparedEnvironment(options.Execution.Environment, nil); err != nil {
		return ExecutionResult{}, true, err
	}
	result, err := ExecuteTypeCommand(ctx, transactions, statement.Stmt, *options.Execution.TypeDDL)
	return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
}

func isTypeCommand(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_DefineStmt:
		return statement.DefineStmt != nil && statement.DefineStmt.Kind == zpgparse.ObjectType_OBJECT_TYPE
	case *zpgparse.Node_CreateDomainStmt, *zpgparse.Node_CreateEnumStmt, *zpgparse.Node_CompositeTypeStmt, *zpgparse.Node_CreateRangeStmt, *zpgparse.Node_AlterEnumStmt, *zpgparse.Node_AlterDomainStmt:
		return true
	case *zpgparse.Node_AlterTableStmt:
		return statement.AlterTableStmt != nil && statement.AlterTableStmt.Objtype == zpgparse.ObjectType_OBJECT_TYPE
	case *zpgparse.Node_DropStmt:
		return statement.DropStmt != nil && (statement.DropStmt.RemoveType == zpgparse.ObjectType_OBJECT_TYPE || statement.DropStmt.RemoveType == zpgparse.ObjectType_OBJECT_DOMAIN)
	case *zpgparse.Node_RenameStmt:
		return statement.RenameStmt != nil && (statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_TYPE || statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_DOMAIN || statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_DOMCONSTRAINT || statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_ATTRIBUTE)
	case *zpgparse.Node_AlterObjectSchemaStmt:
		return statement.AlterObjectSchemaStmt != nil && (statement.AlterObjectSchemaStmt.ObjectType == zpgparse.ObjectType_OBJECT_TYPE || statement.AlterObjectSchemaStmt.ObjectType == zpgparse.ObjectType_OBJECT_DOMAIN)
	default:
		return false
	}
}

func ExecuteTypeCommand(ctx context.Context, transactions *Transactions, node *zpgparse.Node, options TypeCommandOptions) (result TypeCommandResult, err error) {
	if transactions == nil || node == nil || options.MaxDescriptors == 0 {
		return result, errSessionTransactionConfig
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if transactions.sequences == nil {
		return result, transactions.fail(settingError("0A000", "catalog DDL services are not configured"))
	}
	automatic := !transactions.active
	if automatic {
		if err := transactions.begin(operation, TransactionStart{}); err != nil {
			return result, err
		}
	}
	if err := transactions.ordinary(); err != nil {
		if automatic && transactions.active {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	complete := false
	defer func() {
		if !complete {
			transactions.markFailed(errSessionStatementPanic)
			if automatic {
				transactions.closeError = errors.Join(transactions.closeError, transactions.endRollback(context.WithoutCancel(operation)))
			}
		}
	}()
	options.Validation = normalizeDomainValidationOptions(options.Validation, options.Binding.Descriptors)
	var bound *bind.BoundTypeCommand
	err = transactions.routineAtomicStatement(operation, func(ctx context.Context) error {
		if transactions.settings.TransactionPolicy().ReadOnly {
			return settingError("25006", "cannot execute type DDL in a read-only transaction")
		}
		options.Binding.Bind.SearchPath = transactions.settings.SearchPath()
		options.Binding.Owner = transactions.settings.Principal().OID()
		options.Binding.Bind.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		options.Binding.Bind.Temporal = transactions.temporal
		options.Binding.Identities = sequenceIdentityAllocator{jobs: transactions.sequences.config.Identities, transaction: transactions.transaction}
		var bindErr error
		bound, bindErr = transactions.bindTypeCommand(ctx, node, options)
		if bindErr == nil {
			result.Tag, result.Warnings = bound.CommandTag(), bound.Warnings()
		}
		return bindErr
	})
	var lockIDs []model.ObjectID
	if err == nil {
		err = transactions.lockTypeNamespaceScans(operation, bound, options)
		var schemaLocks []storage.ObjectRef
		var rangeLocks []txn.ReadDependency
		if err == nil {
			lockIDs, schemaLocks, rangeLocks, err = transactions.typeCommandLocks(operation, bound, options)
		}
		if err == nil && len(rangeLocks) != 0 {
			var lockView txn.ReadView
			lockView, err = transactions.transaction.ReadView(operation, rangeLocks)
			if err == nil {
				err = lockView.Close()
			}
		}
		for _, target := range schemaLocks {
			if err == nil {
				err = transactions.transaction.LockSchema(operation, target)
			}
		}
		for _, id := range lockIDs {
			if err == nil {
				err = transactions.sequences.config.Catalog.LockDescriptor(operation, transactions.transaction, id)
			}
		}
	}
	if err == nil {
		err = transactions.routineAtomicStatement(operation, func(ctx context.Context) error {
			return transactions.stageTypeCommand(ctx, bound, lockIDs, options)
		})
	}
	err = errors.Join(err, transactions.markDataAccess())
	complete = true
	if err != nil {
		transactions.markFailed(err)
		if automatic {
			err = errors.Join(err, transactions.endRollback(context.WithoutCancel(operation)))
		}
		return result, err
	}
	if automatic {
		result.Commit, err = transactions.commit(operation, false)
	}
	return result, err
}

func (transactions *Transactions) bindTypeCommand(ctx context.Context, node *zpgparse.Node, options TypeCommandOptions) (*bind.BoundTypeCommand, error) {
	view, err := transactions.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return nil, err
	}
	snapshot, err := transactions.sequences.config.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return nil, errors.Join(err, view.Close())
	}
	if err := transactions.activateDeclaredTypes(ctx, snapshot, options.Binding.Bind.Builtin); err != nil {
		return nil, errors.Join(err, snapshot.Close(), view.Close())
	}
	bound, handled, bindErr := bind.BindTypeCommand(ctx, node, snapshot, options.Binding)
	if !handled && bindErr == nil {
		bindErr = errPreparedInvocation
	}
	return bound, errors.Join(bindErr, snapshot.Close(), view.Close())
}

func (transactions *Transactions) lockTypeNamespaceScans(ctx context.Context, bound *bind.BoundTypeCommand, options TypeCommandOptions) (err error) {
	if bound == nil || (!bound.Cascade() && bound.DomainValidationID() == 0 && len(bound.Drops()) == 0 && len(bound.Updates()) == 0) {
		return nil
	}
	store := transactions.sequences.config.Catalog
	root, err := store.NamespaceScanDependency(ctx, 0, model.ObjectSchema)
	if err != nil {
		return err
	}
	view, err := transactions.transaction.ReadView(ctx, []txn.ReadDependency{root})
	if err != nil {
		return err
	}
	if err := view.Close(); err != nil {
		return err
	}
	view, err = transactions.transaction.ReadView(ctx, options.Dependencies)
	if err != nil {
		return err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	dependencies, err := store.TypeCascadeScanDependencies(ctx, snapshot)
	err = errors.Join(err, snapshot.Close(), view.Close())
	if err != nil {
		return err
	}
	dependencies = typeNamespaceScanDependencies(dependencies)
	view, err = transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	return view.Close()
}

func (transactions *Transactions) typeCommandLocks(ctx context.Context, bound *bind.BoundTypeCommand, options TypeCommandOptions) (_ []model.ObjectID, _ []storage.ObjectRef, _ []txn.ReadDependency, err error) {
	if bound == nil {
		return nil, nil, nil, errPreparedInvocation
	}
	locked := make(map[model.ObjectID]struct{})
	schemaLocked := make(map[model.ObjectID]storage.ObjectRef)
	var rangeLocks []txn.ReadDependency
	for _, update := range bound.Updates() {
		locked[update.Base.ID()] = struct{}{}
	}
	for _, descriptor := range bound.Drops() {
		locked[descriptor.ID()] = struct{}{}
	}
	if bound.Cascade() || len(bound.Drops()) != 0 || len(bound.Updates()) != 0 {
		store := transactions.sequences.config.Catalog
		view, openErr := transactions.transaction.ReadView(ctx, options.Dependencies)
		if openErr != nil {
			return nil, nil, nil, openErr
		}
		snapshot, openErr := store.TransactionSnapshot(view, catalog.BorrowReader)
		if openErr != nil {
			return nil, nil, nil, errors.Join(openErr, view.Close())
		}
		defer func() { err = errors.Join(err, snapshot.Close(), view.Close()) }()
		rangeLocks, openErr = store.TypeCascadeScanDependencies(ctx, snapshot)
		if openErr != nil {
			return nil, nil, nil, openErr
		}
		rangeLocks = typeNamespaceScanDependencies(rangeLocks)
		updates := bound.Updates()
		catalogUpdates := make([]catalog.TypeUpdate, len(updates))
		for i := range updates {
			catalogUpdates[i] = catalog.TypeUpdate{Base: updates[i].Base, Next: updates[i].Next}
		}
		dependents, dependentErr := store.CompositeTypeDependents(ctx, snapshot, catalogUpdates, options.MaxDescriptors)
		if dependentErr != nil {
			return nil, nil, nil, dependentErr
		}
		if len(dependents) != 0 && !bound.Cascade() {
			return nil, nil, nil, &model.Error{Code: "2BP01", Message: "cannot alter type because it is the type of a typed table"}
		}
		for _, dependent := range dependents {
			locked[dependent.Table.ID()] = struct{}{}
			physical := dependent.Table.Physical()
			schemaLocked[dependent.Table.ID()] = storage.ObjectRef{ObjectID: dependent.Table.ID(), Generation: physical.Generation, DescriptorVersion: dependent.Table.Version(), EncodingVersion: physical.EncodingVersion}
			spec, specErr := dependent.Table.SpecWithin(ctx, options.Binding.Descriptors)
			if specErr != nil || spec.Table == nil {
				return nil, nil, nil, errors.Join(model.ErrInvalidDescriptor, specErr)
			}
			for _, indexID := range spec.Table.Indexes {
				locked[indexID] = struct{}{}
				published, descriptorErr := snapshot.Descriptor(ctx, indexID)
				if descriptorErr != nil {
					return nil, nil, nil, descriptorErr
				}
				index := published.Descriptor
				indexPhysical := index.Physical()
				schemaLocked[indexID] = storage.ObjectRef{ObjectID: indexID, Generation: indexPhysical.Generation, DescriptorVersion: index.Version(), EncodingVersion: indexPhysical.EncodingVersion}
			}
			if spec.Table.RowType != 0 {
				locked[spec.Table.RowType] = struct{}{}
				published, descriptorErr := snapshot.Descriptor(ctx, spec.Table.RowType)
				if descriptorErr != nil {
					return nil, nil, nil, descriptorErr
				}
				rowSpec, specErr := published.Descriptor.SpecWithin(ctx, options.Binding.Descriptors)
				if specErr != nil {
					return nil, nil, nil, specErr
				}
				if rowSpec.Type == nil || rowSpec.Type.Array == 0 {
					return nil, nil, nil, model.ErrInvalidDescriptor
				}
				locked[rowSpec.Type.Array] = struct{}{}
			}
		}
		typed, cascadeIndexUpdates, indexDrops, planErr := bindTypeCascadeMetadata(ctx, dependents, snapshot, options.Binding)
		if planErr != nil {
			return nil, nil, nil, planErr
		}
		dependencyCascade, planErr := store.PlanTypeDependencyCascade(ctx, snapshot, typed, cascadeIndexUpdates, indexDrops, options.MaxDescriptors)
		if planErr != nil {
			return nil, nil, nil, planErr
		}
		for _, update := range dependencyCascade.DependencyTableUpdates {
			locked[update.Base.ID()] = struct{}{}
		}
		for _, descriptor := range dependencyCascade.IndexDrops {
			locked[descriptor.ID()] = struct{}{}
			physical := descriptor.Physical()
			if physical.Generation != 0 {
				schemaLocked[descriptor.ID()] = storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
			}
		}
		for _, descriptor := range dependencyCascade.ObjectDrops {
			locked[descriptor.ID()] = struct{}{}
			physical := descriptor.Physical()
			if physical.Generation != 0 {
				schemaLocked[descriptor.ID()] = storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
			}
		}
		drops, dropErr := store.TypeDropOrder(ctx, snapshot, bound.Drops(), bound.Cascade(), options.MaxDescriptors)
		if dropErr != nil {
			return nil, nil, nil, dropErr
		}
		for _, descriptor := range drops {
			locked[descriptor.ID()] = struct{}{}
			physical := descriptor.Physical()
			if physical.Generation != 0 {
				schemaLocked[descriptor.ID()] = storage.ObjectRef{ObjectID: descriptor.ID(), Generation: physical.Generation, DescriptorVersion: descriptor.Version(), EncodingVersion: physical.EncodingVersion}
			}
		}
	}
	if validation := bound.DomainValidationID(); validation != 0 {
		store := transactions.sequences.config.Catalog
		view, openErr := transactions.transaction.ReadView(ctx, options.Dependencies)
		if openErr != nil {
			return nil, nil, nil, openErr
		}
		snapshot, openErr := store.TransactionSnapshot(view, catalog.BorrowReader)
		if openErr != nil {
			return nil, nil, nil, errors.Join(openErr, view.Close())
		}
		var domain model.TypeID
		for _, update := range bound.Updates() {
			spec, specErr := update.Next.SpecWithin(ctx, options.Binding.Descriptors)
			if specErr != nil {
				openErr = specErr
				break
			}
			if spec.Type != nil && spec.Type.DeclaredKind() == model.TypeKindDomain && slices.ContainsFunc(spec.Type.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.ID == validation }) {
				domain = model.TypeID(update.Next.OID())
			}
		}
		if openErr == nil && domain == 0 {
			openErr = model.ErrInvalidDescriptor
		}
		if openErr == nil {
			references, referenceErr := store.DomainColumnReferences(ctx, snapshot, domain, options.MaxDescriptors)
			openErr = referenceErr
			for _, reference := range references {
				locked[reference.Table.ID()] = struct{}{}
				physical := reference.Table.Physical()
				schemaLocked[reference.Table.ID()] = storage.ObjectRef{ObjectID: reference.Table.ID(), Generation: physical.Generation, DescriptorVersion: reference.Table.Version(), EncodingVersion: physical.EncodingVersion}
			}
		}
		openErr = errors.Join(openErr, snapshot.Close(), view.Close())
		if openErr != nil {
			return nil, nil, nil, openErr
		}
	}
	descriptorLocks := make([]model.ObjectID, 0, len(locked))
	for id := range locked {
		descriptorLocks = append(descriptorLocks, id)
	}
	slices.Sort(descriptorLocks)
	if uint64(len(descriptorLocks)) > uint64(options.MaxDescriptors) {
		return nil, nil, nil, settingError("54000", "type command lock descriptor limit exceeded")
	}
	schemaLocks := make([]storage.ObjectRef, 0, len(schemaLocked))
	for _, target := range schemaLocked {
		schemaLocks = append(schemaLocks, target)
	}
	slices.SortFunc(schemaLocks, func(left, right storage.ObjectRef) int {
		switch {
		case left.ObjectID < right.ObjectID:
			return -1
		case left.ObjectID > right.ObjectID:
			return 1
		default:
			return 0
		}
	})
	return descriptorLocks, schemaLocks, rangeLocks, nil
}

func (transactions *Transactions) stageTypeCommand(ctx context.Context, bound *bind.BoundTypeCommand, locked []model.ObjectID, options TypeCommandOptions) error {
	if bound == nil {
		return errPreparedInvocation
	}
	store := transactions.sequences.config.Catalog
	dependencies := slicesCloneReadDependencies(options.Dependencies)
	for _, dependency := range bound.Dependencies() {
		dependencies = append(dependencies, store.DescriptorDependency(dependency.Object))
	}
	for _, descriptor := range bound.Creates() {
		reads, err := store.CreateDependencies(ctx, descriptor)
		if err != nil {
			return err
		}
		dependencies = append(dependencies, reads...)
	}
	updates := bound.Updates()
	catalogUpdates := make([]catalog.TypeUpdate, len(updates))
	for i := range updates {
		catalogUpdates[i] = catalog.TypeUpdate{Base: updates[i].Base, Next: updates[i].Next}
		dependencies = append(dependencies, store.DescriptorDependency(updates[i].Base.ID()))
	}
	for _, descriptor := range bound.Drops() {
		dependencies = append(dependencies, store.DescriptorDependency(descriptor.ID()))
	}
	var typedUpdates, dependencyTableUpdates, indexUpdates, additionUpdates []catalog.ObjectUpdate
	var indexDrops, dependencyDrops, typeDrops []*model.Descriptor
	var rewrites []typeCascadeRewrite
	var cascadeDependencies []model.Dependency
	var retired []storage.ObjectGeneration
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	err = func() error {
		if err := checkTypeCommandDependencies(ctx, snapshot, bound); err != nil {
			return err
		}
		if bound.Cascade() || bound.DomainValidationID() != 0 || len(bound.Drops()) != 0 || len(bound.Updates()) != 0 {
			scanDependencies, err := store.TypeCascadeScanDependencies(ctx, snapshot)
			if err != nil {
				return err
			}
			dependencies = append(dependencies, scanDependencies...)
		}
		typeDrops, err = store.TypeDropOrder(ctx, snapshot, bound.Drops(), bound.Cascade(), options.MaxDescriptors)
		if err != nil {
			return err
		}
		for _, descriptor := range typeDrops {
			if !typeObjectLocked(locked, descriptor.ID()) {
				return catalog.ErrDescriptorChanged
			}
		}
		for _, descriptor := range typeDrops {
			dependencies = append(dependencies, store.DescriptorDependency(descriptor.ID()))
			cascadeDependencies = append(cascadeDependencies, model.Dependency{Object: descriptor.ID(), Version: descriptor.Version()})
			physical := descriptor.Physical()
			if physical.Generation != 0 {
				retired = append(retired, storage.ObjectGeneration{ObjectID: descriptor.ID(), Generation: physical.Generation})
			}
		}
		if bound.Cascade() {
			dependents, err := store.CompositeTypeDependents(ctx, snapshot, catalogUpdates, options.MaxDescriptors)
			if err != nil {
				return err
			}
			typedUpdates = make([]catalog.ObjectUpdate, 0, len(dependents))
			targets := make(map[model.ObjectID]*model.Descriptor, len(dependents))
			for _, dependent := range dependents {
				var cascade *bind.BoundSchemaDDL
				if len(dependent.Parents) == 0 {
					cascade, err = bind.BindCompositeTypedTableCascade(ctx, dependent.Table, dependent.Before, dependent.After, snapshot, options.Binding)
				} else {
					parentsBefore := make([]*model.Descriptor, len(dependent.Parents))
					parentsAfter := make([]*model.Descriptor, len(dependent.Parents))
					for i, parentID := range dependent.Parents {
						published, resolveErr := snapshot.Descriptor(ctx, parentID)
						if resolveErr != nil {
							return resolveErr
						}
						parentsBefore[i] = published.Descriptor
						parentsAfter[i] = targets[parentID]
						if parentsAfter[i] == nil {
							return model.ErrInvalidDescriptor
						}
					}
					cascade, err = bind.BindInheritedTableCascade(ctx, dependent.Table, parentsBefore, parentsAfter, snapshot, options.Binding)
				}
				if err != nil {
					return err
				}
				for _, dependency := range cascade.Dependencies() {
					dependencies = append(dependencies, store.DescriptorDependency(dependency.Object))
					cascadeDependencies = append(cascadeDependencies, dependency)
				}
				for _, index := range cascade.IndexDrops() {
					indexDrops = append(indexDrops, index)
					dependencies = append(dependencies, store.DescriptorDependency(index.ID()))
					physical := index.Physical()
					retired = append(retired, storage.ObjectGeneration{ObjectID: index.ID(), Generation: physical.Generation})
				}
				target := cascade.MetadataOnly()
				definition := cascade.Definition()
				if target == nil {
					target = definition.TargetTable
				}
				target, rowTypes, err := typedTableRowTypeUpdates(ctx, snapshot, target, options.Binding.Descriptors)
				if err != nil {
					return err
				}
				addsColumns, err := typedTableAddsColumns(ctx, cascade.Source(), target, options.Binding.Descriptors)
				if err != nil {
					return err
				}
				if addsColumns {
					additionUpdates = append(additionUpdates, catalog.ObjectUpdate{Base: cascade.Source(), Next: target})
					physical := cascade.Source().Physical()
					dependencies = append(dependencies, txn.ReadDependency{
						Target: storage.ObjectRef{ObjectID: cascade.Source().ID(), Generation: physical.Generation, DescriptorVersion: cascade.Source().Version(), EncodingVersion: physical.EncodingVersion},
						Spans:  []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
					})
				}
				if cascade.MetadataOnly() == nil {
					if options.Cascade.Policies == nil {
						return settingError("22023", "type cascade rewrite services are not configured")
					}
					definition.TargetTable = target
					plan, codec, err := options.Cascade.Policies.GenerationPlan(ctx, snapshot, definition)
					if err != nil {
						return err
					}
					rewrites = append(rewrites, typeCascadeRewrite{Plan: plan, Codec: codec})
					dependencies = append(dependencies, txn.ReadDependency{
						Target: plan.Source(), KeyLayout: plan.SourceKeyLayout(),
						Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
					})
					for i := range definition.SourceIndexes {
						indexUpdates = append(indexUpdates, catalog.ObjectUpdate{Base: definition.SourceIndexes[i], Next: definition.TargetIndexes[i]})
						dependencies = append(dependencies, store.DescriptorDependency(definition.SourceIndexes[i].ID()))
						physical := definition.SourceIndexes[i].Physical()
						retired = append(retired, storage.ObjectGeneration{ObjectID: definition.SourceIndexes[i].ID(), Generation: physical.Generation})
					}
					physical := cascade.Source().Physical()
					retired = append(retired, storage.ObjectGeneration{ObjectID: cascade.Source().ID(), Generation: physical.Generation})
				}
				targets[dependent.Table.ID()] = target
				typedUpdates = append(typedUpdates, catalog.ObjectUpdate{Base: cascade.Source(), Next: target})
				dependencies = append(dependencies, store.DescriptorDependency(cascade.Source().ID()))
				for _, update := range rowTypes {
					dependencies = append(dependencies, store.DescriptorDependency(update.Base.ID()))
				}
				catalogUpdates = append(catalogUpdates, rowTypes...)
			}
		}
		if len(typedUpdates) != 0 {
			dependencyCascade, err := store.PlanTypeDependencyCascade(ctx, snapshot, typedUpdates, indexUpdates, indexDrops, options.MaxDescriptors)
			if err != nil {
				return err
			}
			if !typeCascadePlanLocked(locked, dependencyCascade, indexUpdates) {
				return catalog.ErrDescriptorChanged
			}
			typedUpdates = dependencyCascade.TypedTableUpdates
			dependencyTableUpdates = dependencyCascade.DependencyTableUpdates
			dependencyDrops = dependencyCascade.ObjectDrops
			knownIndexes := make(map[model.ObjectID]bool, len(indexDrops))
			for _, index := range indexDrops {
				knownIndexes[index.ID()] = true
			}
			indexDrops = dependencyCascade.IndexDrops
			droppedIndexIDs := make(map[model.ObjectID]bool, len(indexDrops))
			for _, index := range indexDrops {
				droppedIndexIDs[index.ID()] = true
			}
			indexUpdates = slices.DeleteFunc(indexUpdates, func(update catalog.ObjectUpdate) bool {
				return droppedIndexIDs[update.Base.ID()]
			})
			for _, update := range dependencyTableUpdates {
				dependencies = append(dependencies, store.DescriptorDependency(update.Base.ID()))
				cascadeDependencies = append(cascadeDependencies, model.Dependency{Object: update.Base.ID(), Version: update.Base.Version()})
			}
			for _, index := range indexDrops {
				dependencies = append(dependencies, store.DescriptorDependency(index.ID()))
				cascadeDependencies = append(cascadeDependencies, model.Dependency{Object: index.ID(), Version: index.Version()})
				if !knownIndexes[index.ID()] {
					physical := index.Physical()
					retired = appendTypeRetirement(retired, storage.ObjectGeneration{ObjectID: index.ID(), Generation: physical.Generation})
				}
			}
			for _, descriptor := range dependencyDrops {
				dependencies = append(dependencies, store.DescriptorDependency(descriptor.ID()))
				cascadeDependencies = append(cascadeDependencies, model.Dependency{Object: descriptor.ID(), Version: descriptor.Version()})
				physical := descriptor.Physical()
				if physical.Generation != 0 {
					retired = appendTypeRetirement(retired, storage.ObjectGeneration{ObjectID: descriptor.ID(), Generation: physical.Generation})
				}
			}
		}
		if validation := bound.DomainValidationID(); validation != 0 {
			domain, err := domainValidationType(ctx, validation, catalogUpdates, options.Binding.Descriptors)
			if err != nil {
				return err
			}
			references, err := store.DomainColumnReferences(ctx, snapshot, domain, options.MaxDescriptors)
			if err != nil {
				return err
			}
			for _, reference := range references {
				dependencies = append(dependencies, store.DescriptorDependency(reference.Table.ID()))
				physical := reference.Table.Physical()
				dependencies = append(dependencies, txn.ReadDependency{
					Target: storage.ObjectRef{ObjectID: reference.Table.ID(), Generation: physical.Generation, DescriptorVersion: reference.Table.Version(), EncodingVersion: physical.EncodingVersion},
					Spans:  []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
				})
			}
		}
		return nil
	}()
	err = errors.Join(err, snapshot.Close(), view.Close())
	if err != nil {
		return err
	}
	retirementID, err := transactions.prepareObjectRetirement(ctx, retired)
	if err != nil {
		return err
	}
	if retirementID != 0 {
		dependency, err := transactions.sequences.config.Identities.RetirementDependency(retirementID)
		if err != nil {
			return err
		}
		dependencies = append(dependencies, dependency)
	}
	view, err = transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err = store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	err = func() error {
		if err := checkTypeCommandDependencies(ctx, snapshot, bound); err != nil {
			return err
		}
		for _, dependency := range cascadeDependencies {
			published, err := snapshot.Descriptor(ctx, dependency.Object)
			if err != nil {
				return err
			}
			if published.Descriptor.Version() != dependency.Version || published.Descriptor.State() != model.StatePublic {
				return catalog.ErrDescriptorChanged
			}
		}
		if err := validateDomainTypeChange(ctx, store, snapshot, view, bound, catalogUpdates, options.Validation, options.MaxDescriptors); err != nil {
			return err
		}
		var validate schema.GenerationValueValidator
		var types builtin.TypeResolver
		if len(rewrites) != 0 || len(additionUpdates) != 0 {
			if options.Validation.Call.MemoryBytes == 0 || len(additionUpdates) != 0 && (options.Validation.BatchRows <= 0 || options.Validation.ValueBytes == 0) {
				return settingError("22023", "type cascade value validation is not configured")
			}
			types, err = NewDeclaredTypeRuntimeWithUpdates(ctx, snapshot, options.Validation.Runtime, catalogUpdates)
			if err != nil {
				return err
			}
			validate = func(ctx context.Context, layout model.TypeLayout, value model.Value) error {
				return builtin.ValidateResolvedValue(ctx, builtin.ResolveOptions{Types: types, InformationSchemaTypes: options.Binding.Bind.CatalogRelations}, layout, value, options.Validation.Call, options.Validation.Runtime.Descriptors.MaxDepth)
			}
		}
		if err := validateTypedTableAdditions(ctx, snapshot, view, additionUpdates, types, options.Binding.Bind.CatalogRelations, options.Validation); err != nil {
			return err
		}
		changes, _, err := store.ApplyTypeChanges(ctx, snapshot, catalog.TypeChangeSet{
			Creates: bound.Creates(), Updates: catalogUpdates, Drops: bound.Drops(), CascadeDrops: typeDrops,
			TypedTableUpdates: typedUpdates, DependencyTableUpdates: dependencyTableUpdates,
			TypedIndexUpdates: indexUpdates, TypedIndexDrops: indexDrops, DependencyDrops: dependencyDrops,
			Cascade: bound.Cascade(), MaxDescriptors: options.MaxDescriptors,
		})
		if err != nil {
			return err
		}
		for _, rewrite := range rewrites {
			if err := schema.RewriteGenerationInTransaction(ctx, transactions.transaction, view, view.Timestamp(), rewrite.Plan, rewrite.Codec, options.Cascade.Scan, validate); err != nil {
				return err
			}
		}
		var mutations []storage.Mutation
		for _, change := range changes {
			mutations = append(mutations, change.Mutations...)
		}
		if len(mutations) != 0 {
			if err := transactions.transaction.ApplyMutations(ctx, mutations); err != nil {
				return err
			}
		}
		if retirementID != 0 {
			return transactions.sequences.config.Identities.StageRetirements(ctx, transactions.transaction, retirementID, retired)
		}
		return nil
	}()
	return errors.Join(err, snapshot.Close(), view.Close())
}

func typeNamespaceScanDependencies(dependencies []txn.ReadDependency) []txn.ReadDependency {
	return slices.DeleteFunc(dependencies, func(dependency txn.ReadDependency) bool {
		return dependency.Target.ObjectID != catalog.NamespaceObjectID
	})
}

func typeObjectLocked(locked []model.ObjectID, id model.ObjectID) bool {
	_, found := slices.BinarySearch(locked, id)
	return found
}

func typeCascadePlanLocked(locked []model.ObjectID, plan catalog.TypeDependencyCascade, indexUpdates []catalog.ObjectUpdate) bool {
	for _, updates := range [][]catalog.ObjectUpdate{plan.TypedTableUpdates, plan.DependencyTableUpdates, indexUpdates} {
		for _, update := range updates {
			if update.Base == nil || !typeObjectLocked(locked, update.Base.ID()) {
				return false
			}
		}
	}
	for _, descriptors := range [][]*model.Descriptor{plan.IndexDrops, plan.ObjectDrops} {
		for _, descriptor := range descriptors {
			if descriptor == nil || !typeObjectLocked(locked, descriptor.ID()) {
				return false
			}
		}
	}
	return true
}

func checkTypeCommandDependencies(ctx context.Context, snapshot *catalog.CatalogSnapshot, bound *bind.BoundTypeCommand) error {
	for _, dependency := range bound.Dependencies() {
		published, err := snapshot.Descriptor(ctx, dependency.Object)
		if err != nil {
			return err
		}
		if published.Descriptor.Version() != dependency.Version || published.Descriptor.State() != model.StatePublic {
			return catalog.ErrDescriptorChanged

		}
	}
	return nil
}
func bindTypeCascadeMetadata(ctx context.Context, dependents []catalog.CompositeTypeDependent, snapshot *catalog.CatalogSnapshot, options bind.CreateDeclarationOptions) ([]catalog.ObjectUpdate, []catalog.ObjectUpdate, []*model.Descriptor, error) {
	updates := make([]catalog.ObjectUpdate, 0, len(dependents))
	var indexUpdates []catalog.ObjectUpdate
	var indexDrops []*model.Descriptor
	targets := make(map[model.ObjectID]*model.Descriptor, len(dependents))
	for _, dependent := range dependents {
		var cascade *bind.BoundSchemaDDL
		var err error
		if len(dependent.Parents) == 0 {
			cascade, err = bind.BindCompositeTypedTableCascade(ctx, dependent.Table, dependent.Before, dependent.After, snapshot, options)
		} else {
			parentsBefore := make([]*model.Descriptor, len(dependent.Parents))
			parentsAfter := make([]*model.Descriptor, len(dependent.Parents))
			for i, parentID := range dependent.Parents {
				published, resolveErr := snapshot.Descriptor(ctx, parentID)
				if resolveErr != nil {
					return nil, nil, nil, resolveErr
				}
				parentsBefore[i] = published.Descriptor
				parentsAfter[i] = targets[parentID]
				if parentsAfter[i] == nil {
					return nil, nil, nil, model.ErrInvalidDescriptor
				}
			}
			cascade, err = bind.BindInheritedTableCascade(ctx, dependent.Table, parentsBefore, parentsAfter, snapshot, options)
		}
		if err != nil {
			return nil, nil, nil, err
		}
		target := cascade.MetadataOnly()
		if target == nil {
			definition := cascade.Definition()
			target = definition.TargetTable
			for i := range definition.SourceIndexes {
				indexUpdates = append(indexUpdates, catalog.ObjectUpdate{Base: definition.SourceIndexes[i], Next: definition.TargetIndexes[i]})
			}
		}
		if target == nil {
			return nil, nil, nil, model.ErrInvalidDescriptor
		}
		targets[dependent.Table.ID()] = target
		updates = append(updates, catalog.ObjectUpdate{Base: cascade.Source(), Next: target})
		indexDrops = append(indexDrops, cascade.IndexDrops()...)
	}
	return updates, indexUpdates, indexDrops, nil
}

func typedTableRowTypeUpdates(ctx context.Context, snapshot *catalog.CatalogSnapshot, table *model.Descriptor, limits model.DescriptorLimits) (*model.Descriptor, []catalog.TypeUpdate, error) {
	if table == nil {
		return nil, nil, model.ErrInvalidDescriptor
	}
	spec, err := table.SpecWithin(ctx, limits)
	if err != nil {
		return nil, nil, err
	}
	if spec.Table == nil || spec.Table.RowType == 0 {
		return nil, nil, model.ErrInvalidDescriptor
	}
	published, err := snapshot.Descriptor(ctx, spec.Table.RowType)
	if err != nil {
		return nil, nil, err
	}
	rowType := published.Descriptor
	rowSpec, err := rowType.SpecWithin(ctx, limits)
	if err != nil {
		return nil, nil, err
	}
	if rowSpec.Type == nil || rowSpec.Type.Array == 0 || rowType.Version() == ^uint64(0) {
		return nil, nil, model.ErrInvalidDescriptor
	}
	published, err = snapshot.Descriptor(ctx, rowSpec.Type.Array)
	if err != nil {
		return nil, nil, err
	}
	array := published.Descriptor
	nextRowType, nextArray, err := model.NextImplicitRelationRowTypes(ctx, table, rowType, array, array.Name().Name, limits)
	if err != nil {
		return nil, nil, err
	}
	return table, []catalog.TypeUpdate{{Base: rowType, Next: nextRowType}, {Base: array, Next: nextArray}}, nil
}

func appendTypeRetirement(retirements []storage.ObjectGeneration, retirement storage.ObjectGeneration) []storage.ObjectGeneration {
	if retirement.Generation == 0 || slices.Contains(retirements, retirement) {
		return retirements
	}
	return append(retirements, retirement)
}

func typedTableAddsColumns(ctx context.Context, base, next *model.Descriptor, limits model.DescriptorLimits) (bool, error) {
	if base == nil || next == nil || base.ID() != next.ID() {
		return false, model.ErrInvalidDescriptor
	}
	before, err := base.SpecWithin(ctx, limits)
	if err != nil || before.Table == nil {
		return false, errors.Join(model.ErrInvalidDescriptor, err)
	}
	after, err := next.SpecWithin(ctx, limits)
	if err != nil || after.Table == nil {
		return false, errors.Join(model.ErrInvalidDescriptor, err)
	}
	existing := make(map[model.ColumnID]bool, len(before.Table.Columns))
	for _, column := range before.Table.Columns {
		existing[column.ID] = true
	}
	return slices.ContainsFunc(after.Table.Columns, func(column model.ColumnDescriptor) bool {
		return !existing[column.ID]
	}), nil
}

func slicesCloneReadDependencies(dependencies []txn.ReadDependency) []txn.ReadDependency {
	return append([]txn.ReadDependency(nil), dependencies...)
}
