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
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const temporarySchemaName = "pg_temp"

func (named *sessionNamedObjects) ensureTemporaryMaps() {
	if named.temporary == nil {
		named.temporary = make(map[model.ObjectID]*model.Descriptor)
		named.temporaryNames = make(map[model.NamespaceName]model.ObjectID)
	}
	if named.temporaryIDs == nil {
		named.temporaryIDs = make(map[model.ObjectID]struct{})
	}
	if named.temporaryTargets == nil {
		named.temporaryTargets = make(map[model.ObjectID]execution.MutationTarget)
	}
	if named.temporaryRows == nil {
		named.temporaryRows = make(map[model.ObjectID]*txn.RowNormalizer)
	}
}

// isTemporary reports whether the object was created as a session-temporary
// object in this session. It stays true after the object is dropped: a
// temporary descriptor is never published, so a command that drops one must not
// declare, read, or require a catalog row for it. Identities are never reused,
// so a retained id cannot name a later object.
func (named *sessionNamedObjects) isTemporary(id model.ObjectID) bool {
	if named == nil {
		return false
	}
	_, exists := named.temporaryIDs[id]
	return exists
}

// overlayVersion names the exact session-temporary descriptor set
// addTemporaryOverlay attaches to a snapshot. It advances on every change to
// that set, so a cache keyed by it never reuses metadata built from a different
// overlay.
func (named *sessionNamedObjects) overlayVersion() uint64 {
	if named == nil {
		return 0
	}
	return named.temporaryVersion
}

func (named *sessionNamedObjects) temporaryDescriptors() []*model.Descriptor {
	if len(named.temporary) == 0 {
		return nil
	}
	result := make([]*model.Descriptor, 0, len(named.temporary))
	for _, descriptor := range named.temporary {
		result = append(result, descriptor)
	}
	slices.SortFunc(result, func(left, right *model.Descriptor) int {
		if left.ID() < right.ID() {
			return -1
		}
		if left.ID() > right.ID() {
			return 1
		}
		return 0
	})
	return result
}

func (named *sessionNamedObjects) setTemporary(descriptor *model.Descriptor) error {
	if descriptor == nil {
		return model.ErrInvalidDescriptor
	}
	named.ensureTemporaryMaps()
	before := named.temporary[descriptor.ID()]
	if conflict := named.temporaryNames[descriptor.Name()]; conflict != 0 && conflict != descriptor.ID() {
		return &model.Error{Code: "42P07", Message: "temporary relation already exists"}
	}
	named.temporaryChanges = append(named.temporaryChanges, temporaryCatalogChange{before: before, after: descriptor})
	if before != nil && before.Name() != descriptor.Name() {
		delete(named.temporaryNames, before.Name())
	}
	named.temporary[descriptor.ID()] = descriptor
	named.temporaryNames[descriptor.Name()] = descriptor.ID()
	named.temporaryIDs[descriptor.ID()] = struct{}{}
	named.temporaryVersion++
	return nil
}

func (named *sessionNamedObjects) dropTemporary(descriptor *model.Descriptor) error {
	if descriptor == nil || named.temporary[descriptor.ID()] == nil {
		return model.ErrInvalidDescriptor
	}
	id := descriptor.ID()
	before := named.temporary[id]
	beforeTarget, beforeHasTgt := named.temporaryTargets[id]
	beforeRow, beforeHasRow := named.temporaryRows[id]
	named.temporaryChanges = append(named.temporaryChanges, temporaryCatalogChange{
		before:       before,
		hasTarget:    true,
		targetID:     id,
		beforeTarget: beforeTarget,
		beforeHasTgt: beforeHasTgt,
		hasRow:       true,
		rowID:        id,
		beforeRow:    beforeRow,
		beforeHasRow: beforeHasRow,
	})
	delete(named.temporary, id)
	delete(named.temporaryNames, before.Name())
	delete(named.temporaryTargets, id)
	delete(named.temporaryRows, id)
	named.temporaryVersion++
	return nil
}

// setTemporaryTargets registers the mutation capabilities of the
// session-temporary relations one declaration produced. A temporary descriptor
// is never published, so a later statement can reach neither its write policy
// nor its row normalizer through the catalog.
func (named *sessionNamedObjects) setTemporaryTargets(admitted *DeclarationResult) {
	if admitted == nil {
		return
	}
	named.ensureTemporaryMaps()
	for _, target := range admitted.Targets() {
		if target.Descriptor == nil {
			continue
		}
		id := target.Descriptor.ID()
		beforeTarget, beforeHasTgt := named.temporaryTargets[id]
		named.temporaryChanges = append(named.temporaryChanges, temporaryCatalogChange{
			hasTarget:    true,
			targetID:     id,
			beforeTarget: beforeTarget,
			beforeHasTgt: beforeHasTgt,
		})
		named.temporaryTargets[id] = target
	}
	for id, normalizer := range admitted.rows {
		beforeRow, beforeHasRow := named.temporaryRows[id]
		named.temporaryChanges = append(named.temporaryChanges, temporaryCatalogChange{
			hasRow:       true,
			rowID:        id,
			beforeRow:    beforeRow,
			beforeHasRow: beforeHasRow,
		})
		named.temporaryRows[id] = normalizer
	}
}

// temporaryTarget returns the registered mutation target of one live
// session-temporary relation.
func (named *sessionNamedObjects) temporaryTarget(id model.ObjectID) (execution.MutationTarget, bool) {
	if named == nil {
		return execution.MutationTarget{}, false
	}
	target, ok := named.temporaryTargets[id]
	return target, ok
}

// temporaryMutationResolver answers a write to a session-temporary relation
// before consulting the catalog. Temporary descriptors are never published, so
// the durable policy chain cannot resolve them; the session registered their
// normalizers, for the table and for each index over it, when the declaration
// was admitted.
type temporaryMutationResolver struct {
	named *sessionNamedObjects
	base  txn.MutationResolver
}

func (resolver temporaryMutationResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if normalizer := resolver.named.temporaryRows[target.ObjectID]; normalizer != nil {
		if normalizer.Layout().Target != target {
			return nil, storage.ErrInvalidTarget
		}
		return normalizer, nil
	}
	if resolver.base == nil {
		return nil, storage.ErrInvalidLayout
	}
	return resolver.base.ResolveMutation(ctx, target)
}

func (named *sessionNamedObjects) rollbackTemporary(mark int) {
	if mark < 0 || mark > len(named.temporaryChanges) {
		return
	}
	if len(named.temporaryChanges) == mark {
		return
	}
	named.ensureTemporaryMaps()
	named.temporaryVersion++
	for i := len(named.temporaryChanges) - 1; i >= mark; i-- {
		change := named.temporaryChanges[i]
		if change.after != nil {
			delete(named.temporary, change.after.ID())
			delete(named.temporaryNames, change.after.Name())
		}
		if change.before != nil {
			named.temporary[change.before.ID()] = change.before
			named.temporaryNames[change.before.Name()] = change.before.ID()
		}
		if change.hasTarget {
			if change.beforeHasTgt {
				named.temporaryTargets[change.targetID] = change.beforeTarget
			} else {
				delete(named.temporaryTargets, change.targetID)
			}
		}
		if change.hasRow {
			if change.beforeHasRow {
				named.temporaryRows[change.rowID] = change.beforeRow
			} else {
				delete(named.temporaryRows, change.rowID)
			}
		}
	}
	clear(named.temporaryChanges[mark:])
	named.temporaryChanges = named.temporaryChanges[:mark]
}

func (named *sessionNamedObjects) finishTemporary(committed bool) {
	if !committed {
		named.rollbackTemporary(0)
	}
	clear(named.temporaryChanges)
	named.temporaryChanges = named.temporaryChanges[:0]
	clear(named.temporaryMarks)
	named.temporaryMarks = named.temporaryMarks[:0]
}

func (named *sessionNamedObjects) applyTemporaryCommand(ctx context.Context, bound *bind.BoundObjectCommand, limits model.DescriptorLimits) ([]*model.Descriptor, error) {
	mark := len(named.temporaryChanges)
	drops := bound.Drops()
	if len(drops) != 0 {
		expanded, err := named.planTemporaryDrops(ctx, drops, bound.Cascade(), limits)
		if err != nil {
			named.rollbackTemporary(mark)
			return nil, err
		}
		drops = expanded
	}
	for _, descriptor := range drops {
		if err := named.dropTemporary(descriptor); err != nil {
			named.rollbackTemporary(mark)
			return nil, err
		}
	}
	for _, update := range bound.Updates() {
		if err := named.setTemporary(update.Next); err != nil {
			named.rollbackTemporary(mark)
			return nil, err
		}
	}
	for _, descriptor := range bound.Creates() {
		if err := named.setTemporary(descriptor); err != nil {
			named.rollbackTemporary(mark)
			return nil, err
		}
	}
	return drops, nil
}

func (named *sessionNamedObjects) planTemporaryDrops(ctx context.Context, requested []*model.Descriptor, cascade bool, limits model.DescriptorLimits) ([]*model.Descriptor, error) {
	targets := make(map[model.ObjectID]*model.Descriptor, len(requested))
	for _, d := range requested {
		if d != nil {
			targets[d.ID()] = d
		}
	}
	dependents := make(map[model.ObjectID][]*model.Descriptor)
	for _, d := range named.temporaryDescriptors() {
		id := d.ID()
		inTargets := targets[id] != nil
		spec, err := d.SpecWithin(ctx, limits)
		if err != nil {
			return nil, err
		}
		checkDep := func(depID model.ObjectID) error {
			if depID == 0 || depID == id {
				return nil
			}
			if !inTargets && !cascade && targets[depID] != nil {
				return &model.Error{Code: "2BP01", Message: "cannot drop object because other objects depend on it"}
			}
			dependents[depID] = append(dependents[depID], d)
			return nil
		}
		if d.Name().Parent != 0 {
			if err := checkDep(d.Name().Parent); err != nil {
				return nil, err
			}
		}
		if dt, ok := d.DeclaredType(); ok && dt.Relation != 0 {
			if err := checkDep(dt.Relation); err != nil {
				return nil, err
			}
		}
		for _, dep := range spec.Dependencies {
			if err := checkDep(dep.Object); err != nil {
				return nil, err
			}
		}
	}
	if cascade {
		queue := make([]model.ObjectID, 0, len(targets))
		for id := range targets {
			queue = append(queue, id)
		}
		slices.Sort(queue)
		for head := 0; head < len(queue); head++ {
			for _, child := range dependents[queue[head]] {
				if targets[child.ID()] == nil {
					targets[child.ID()] = child
					queue = append(queue, child.ID())
				}
			}
		}
	}
	result := make([]*model.Descriptor, 0, len(targets))
	for _, d := range targets {
		result = append(result, d)
	}
	slices.SortFunc(result, func(a, b *model.Descriptor) int {
		if a.ID() < b.ID() {
			return -1
		}
		if a.ID() > b.ID() {
			return 1
		}
		return 0
	})
	return result, nil
}

func (named *sessionNamedObjects) applyTemporaryDeclaration(bound *bind.BoundDeclaration) error {
	if bound == nil || bound.Existing() != nil {
		return model.ErrInvalidDescriptor
	}
	mark := len(named.temporaryChanges)
	for _, update := range bound.Updates() {
		current := named.temporary[update.Base.ID()]
		if current == nil || current.Version() != update.Base.Version() || update.Next.Version() != update.Base.Version()+1 {
			named.rollbackTemporary(mark)
			return model.ErrInvalidDescriptor
		}
		if err := named.setTemporary(update.Next); err != nil {
			named.rollbackTemporary(mark)
			return err
		}
	}
	for _, descriptor := range bound.Descriptors() {
		if err := named.setTemporary(descriptor); err != nil {
			named.rollbackTemporary(mark)
			return err
		}
	}
	return nil
}

func addTemporaryOverlay(snapshot *catalog.CatalogSnapshot, named *sessionNamedObjects) error {
	if snapshot == nil || named == nil {
		return nil
	}
	return snapshot.AddSessionOverlay(named.temporaryDescriptors())
}

// TemporaryCatalogSnapshot returns whether a transaction is active along with a
// race-free snapshot of the session's temporary overlay descriptors, monotonic
// overlay version, and effective search_path (with pg_temp prepended when any
// temporary object exists). Safe both inside and outside an active session
// operation.
func (transactions *Transactions) TemporaryCatalogSnapshot(ctx context.Context) (active bool, descriptors []*model.Descriptor, version uint64, searchPath []string) {
	if transactions == nil {
		return false, nil, 0, nil
	}
	if ctx != nil {
		if token, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation); ok && token.owner == transactions && token.active.Load() {
			return transactions.active, transactions.named.temporaryDescriptors(), transactions.named.overlayVersion(), transactions.searchPath()
		}
	}
	transactions.gate <- struct{}{}
	defer func() { <-transactions.gate }()
	return transactions.active, transactions.named.temporaryDescriptors(), transactions.named.overlayVersion(), transactions.searchPath()
}

// SearchPathIn returns the session's effective search_path (with pg_temp
// prepended when temporary objects exist). Safe both inside and outside an
// active session operation.
func (transactions *Transactions) SearchPathIn(ctx context.Context) []string {
	if transactions == nil {
		return nil
	}
	if ctx != nil {
		if token, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation); ok && token.owner == transactions && token.active.Load() {
			return transactions.searchPath()
		}
	}
	transactions.gate <- struct{}{}
	defer func() { <-transactions.gate }()
	return transactions.searchPath()
}

func (transactions *Transactions) ensureTemporarySchema(ctx context.Context, options *bind.CreateDeclarationOptions) error {
	transactions.named.ensureTemporaryMaps()
	name := model.NamespaceName{Kind: model.ObjectSchema, Name: temporarySchemaName}
	if id := transactions.named.temporaryNames[name]; id != 0 && transactions.named.temporary[id] != nil {
		return nil
	}
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return err
	}
	descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: id, OID: oid, Name: name, Version: 1, State: model.StatePublic, Schema: &model.SchemaMetadata{Owner: options.Owner}}, options.Descriptors)
	if err != nil {
		return err
	}
	return transactions.named.setTemporary(descriptor)
}

func temporaryObjectNode(node *zpgparse.Node) bool {
	if node == nil {
		return false
	}
	if create := node.GetCreateStmt(); create != nil && create.Relation != nil {
		return create.Relation.Relpersistence == "t" || create.Relation.Schemaname == temporarySchemaName
	}
	if create := node.GetCreateTableAsStmt(); create != nil && create.Into != nil && create.Into.Rel != nil {
		return create.Into.Rel.Relpersistence == "t" || create.Into.Rel.Schemaname == temporarySchemaName
	}
	if create := node.GetViewStmt(); create != nil && create.View != nil {
		return create.View.Relpersistence == "t" || create.View.Schemaname == temporarySchemaName
	}
	if create := node.GetCreateSeqStmt(); create != nil && create.Sequence != nil {
		return create.Sequence.Relpersistence == "t" || create.Sequence.Schemaname == temporarySchemaName
	}
	return false
}

func qualifyTemporaryObject(node *zpgparse.Node) error {
	if create := node.GetCreateStmt(); create != nil && create.Relation != nil {
		if create.Relation.Schemaname != "" && create.Relation.Schemaname != temporarySchemaName {
			return &model.Error{Code: "42P16", Message: "temporary relation cannot be created in a permanent schema"}
		}
		create.Relation.Relpersistence = "t"
		create.Relation.Schemaname = temporarySchemaName
		return nil
	}
	if create := node.GetCreateTableAsStmt(); create != nil && create.Into != nil && create.Into.Rel != nil {
		if create.Into.Rel.Schemaname != "" && create.Into.Rel.Schemaname != temporarySchemaName {
			return &model.Error{Code: "42P16", Message: "temporary relation cannot be created in a permanent schema"}
		}
		create.Into.Rel.Relpersistence = "t"
		create.Into.Rel.Schemaname = temporarySchemaName
		return nil
	}
	if create := node.GetViewStmt(); create != nil && create.View != nil {
		if create.View.Schemaname != "" && create.View.Schemaname != temporarySchemaName {
			return &model.Error{Code: "42P16", Message: "temporary relation cannot be created in a permanent schema"}
		}
		create.View.Relpersistence = "t"
		create.View.Schemaname = temporarySchemaName
		return nil
	}
	if create := node.GetCreateSeqStmt(); create != nil && create.Sequence != nil {
		if create.Sequence.Schemaname != "" && create.Sequence.Schemaname != temporarySchemaName {
			return &model.Error{Code: "42P16", Message: "temporary sequence cannot be created in a permanent schema"}
		}
		create.Sequence.Relpersistence = "t"
		create.Sequence.Schemaname = temporarySchemaName
		return nil
	}
	return errors.New("temporary object statement is invalid")
}

func (transactions *Transactions) stageTemporarySequenceDrops(ctx context.Context, descriptors []*model.Descriptor) error {
	for _, descriptor := range descriptors {
		spec, err := descriptor.Spec(ctx)
		if err != nil {
			return err
		}
		if spec.Sequence == nil || spec.Sequence.Persistence != "t" || spec.Sequence.Incarnation == 0 {
			continue
		}
		index, err := transactions.sequences.reserve(schema.SequenceIncarnation(spec.Sequence.Incarnation))
		if err != nil {
			return err
		}
		transactions.sequences.changes[index].published = true
	}
	return nil
}

func (named *sessionNamedObjects) temporaryClosure(ctx context.Context, root model.ObjectID) ([]*model.Descriptor, error) {
	selected := map[model.ObjectID]bool{root: true}
	changed := true
	for changed {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		changed = false
		for id, descriptor := range named.temporary {
			if selected[id] {
				continue
			}
			spec, err := descriptor.Spec(ctx)
			if err != nil {
				return nil, err
			}
			for _, dependency := range spec.Dependencies {
				if selected[dependency.Object] {
					selected[id] = true
					changed = true
					break
				}
			}
		}
	}
	result := make([]*model.Descriptor, 0, len(selected))
	for id := range selected {
		if descriptor := named.temporary[id]; descriptor != nil {
			result = append(result, descriptor)
		}
	}
	slices.SortFunc(result, func(left, right *model.Descriptor) int {
		if left.ID() > right.ID() {
			return -1
		}
		if left.ID() < right.ID() {
			return 1
		}
		return 0
	})
	return result, nil
}

func (transactions *Transactions) applyTemporaryCommitActions(ctx context.Context) error {
	options := transactions.named.temporaryOptions
	if options == nil || len(transactions.named.temporary) == 0 {
		return nil
	}
	if transactions.implicitReadOnly || transactions.settings.TransactionPolicy().ReadOnly || transactions.historical > 0 {
		return nil
	}
	return transactions.routineAtomicStatement(ctx, func(statement context.Context) (statementErr error) {
		mark := len(transactions.named.temporaryChanges)
		defer func() {
			if statementErr != nil {
				transactions.named.rollbackTemporary(mark)
			}
		}()
		current := transactions.named.temporaryDescriptors()
		var drops []*model.Descriptor
		var admissions []*model.Descriptor
		var retired []*model.Descriptor
		dropped := make(map[model.ObjectID]bool)
		for _, descriptor := range current {
			spec, err := descriptor.SpecWithin(statement, options.Binding.Descriptors)
			if err != nil {
				return err
			}
			if spec.Table == nil || spec.Table.Persistence != "t" {
				continue
			}
			switch spec.Table.OnCommit {
			case "", "preserve":
				continue
			case "drop":
				closure, err := transactions.named.temporaryClosure(statement, descriptor.ID())
				if err != nil {
					return err
				}
				for _, child := range closure {
					if !dropped[child.ID()] {
						dropped[child.ID()] = true
						drops = append(drops, child)
					}
				}
			case "delete":
				nextSpec := spec
				if nextSpec.Version == ^uint64(0) || nextSpec.Physical.Generation == ^uint64(0) {
					return model.ErrInvalidDescriptor
				}
				nextSpec.Version++
				nextSpec.Physical.Generation++
				next, err := model.NewDescriptor(statement, nextSpec, options.Binding.Descriptors)
				if err != nil {
					return err
				}
				if err := transactions.named.setTemporary(next); err != nil {
					return err
				}
				admissions = append(admissions, next)
				retired = append(retired, descriptor)
				for _, indexID := range spec.Table.Indexes {
					baseIndex := transactions.named.temporary[indexID]
					if baseIndex == nil {
						return model.ErrInvalidDescriptor
					}
					indexSpec, err := baseIndex.SpecWithin(statement, options.Binding.Descriptors)
					if err != nil {
						return err
					}
					if indexSpec.Version == ^uint64(0) || indexSpec.Physical.Generation == ^uint64(0) {
						return model.ErrInvalidDescriptor
					}
					indexSpec.Version++
					indexSpec.Physical.Generation++
					for i := range indexSpec.Dependencies {
						if indexSpec.Dependencies[i].Object == descriptor.ID() {
							indexSpec.Dependencies[i].Version = next.Version()
						}
					}
					nextIndex, err := model.NewDescriptor(statement, indexSpec, options.Binding.Descriptors)
					if err != nil {
						return err
					}
					if err := transactions.named.setTemporary(nextIndex); err != nil {
						return err
					}
					admissions = append(admissions, nextIndex)
					retired = append(retired, baseIndex)
				}
				rowTypeID := spec.Table.RowType
				if rowTypeID != 0 {
					baseRow := transactions.named.temporary[rowTypeID]
					if baseRow == nil {
						return model.ErrInvalidDescriptor
					}
					rowSpec, err := baseRow.SpecWithin(statement, options.Binding.Descriptors)
					if err != nil || rowSpec.Type == nil {
						return model.ErrInvalidDescriptor
					}
					baseArray := transactions.named.temporary[rowSpec.Type.Array]
					if baseArray == nil {
						return model.ErrInvalidDescriptor
					}
					nextRow, nextArray, err := model.NextImplicitRelationRowTypes(statement, next, baseRow, baseArray, baseArray.Name().Name, options.Binding.Descriptors)
					if err != nil {
						return err
					}
					if err := transactions.named.setTemporary(nextRow); err != nil {
						return err
					}
					if err := transactions.named.setTemporary(nextArray); err != nil {
						return err
					}
				}
			default:
				return model.ErrInvalidDescriptor
			}
		}
		for _, descriptor := range drops {
			if err := transactions.named.dropTemporary(descriptor); err != nil {
				return err
			}
		}
		if err := transactions.stageTemporarySequenceDrops(statement, drops); err != nil {
			return err
		}
		retirements := append(objectDescriptorRetirements(retired), objectDescriptorRetirements(drops)...)
		if err := transactions.acquireObjectRetirement(statement, retirements); err != nil {
			return err
		}
		if len(admissions) != 0 {
			if err := transactions.admitTemporaryDescriptors(statement, admissions, *options); err != nil {
				return err
			}
		}
		return nil
	})
}

func (transactions *Transactions) admitTemporaryDescriptors(ctx context.Context, descriptors []*model.Descriptor, options ObjectCommandOptions) error {
	store := transactions.sequences.config.Catalog
	dependencies := options.Dependencies
	if dependencies == nil {
		dependencies = catalogLayoutDependencies(store)
	}
	view, err := transactions.transaction.ReadView(ctx, dependencies)
	if err != nil {
		return err
	}
	snapshot, err := store.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return errors.Join(err, view.Close())
	}
	err = func() error {
		if err := addTemporaryOverlay(snapshot, &transactions.named); err != nil {
			return err
		}
		columns := options.Columns
		columns.Builtin = transactions.settings.BuiltinOptions(options.Binding.Bind.Collation, options.Binding.Bind.DefaultCollation)
		if columns.Builtin.InformationSchemaTypes == nil {
			columns.Builtin.InformationSchemaTypes = options.Columns.Builtin.InformationSchemaTypes
		}
		if columns.Builtin.InformationSchemaFunctions == nil {
			columns.Builtin.InformationSchemaFunctions = options.Columns.Builtin.InformationSchemaFunctions
		}
		if err := transactions.activateDeclaredTypes(ctx, snapshot, columns.Builtin); err != nil {
			return err
		}
		columns.Builtin.Types = transactions.statementTypes
		columns.Temporal = transactions.temporal
		declarationOptions := DeclarationOptions{Catalog: store, Encoder: transactions.options.KeyEncoder, Columns: columns, Indexes: options.Indexes, Limits: options.Limits, PolicyBytes: options.PolicyBytes}
		admitted, err := declarationTargets(ctx, transactions.transaction, snapshot, descriptors, declarationOptions, false)
		if err != nil {
			return err
		}
		columnCompiler, err := NewColumnExpressionCompiler(snapshot, declarationOptions.Columns)
		if err != nil {
			return err
		}
		if len(admitted.targets) != 0 {
			admitted.rows = make(map[model.ObjectID]*txn.RowNormalizer)
		}
		for _, target := range admitted.targets {
			table := target.Descriptor
			if table == nil {
				continue
			}
			retainedBytes, err := admitDeclarationPolicyBytes(ctx, table, declarationOptions)
			if err != nil {
				return err
			}
			policy, err := schema.NewTablePolicy(ctx, schema.TablePolicyOptions{
				Descriptor:    table,
				Encoder:       declarationOptions.Encoder,
				Expressions:   columnCompiler,
				MetadataBytes: declarationOptions.Limits.MetadataBytes,
				RowBytes:      declarationOptions.Limits.EntryBytes,
				MaxColumns:    declarationOptions.Limits.MaxColumns,
			})
			if err != nil {
				return err
			}
			if policy.RetainedBytes() > retainedBytes {
				return model.ErrValueBudget
			}
			admitted.rows[table.ID()] = policy
			if indexSet, ok := target.Expansion.(*schema.IndexSet); ok && indexSet != nil {
				for _, layout := range indexSet.Layouts() {
					indexPolicy, err := indexSet.ResolveMutation(ctx, layout.Target)
					if err != nil {
						return err
					}
					if indexPolicy.RetainedBytes() > declarationOptions.PolicyBytes {
						return model.ErrValueBudget
					}
					admitted.rows[layout.Target.ObjectID] = indexPolicy
				}
			}
		}
		transactions.named.setTemporaryTargets(admitted)
		return nil
	}()
	return errors.Join(err, snapshot.Close(), view.Close())
}

func (transactions *Transactions) discardSessionTemporary(ctx context.Context) error {
	descriptors := transactions.named.temporaryDescriptors()
	if len(descriptors) == 0 {
		return nil
	}
	if err := transactions.stageTemporarySequenceDrops(ctx, descriptors); err != nil {
		return err
	}
	if err := transactions.acquireObjectRetirement(ctx, objectDescriptorRetirements(descriptors)); err != nil {
		return err
	}
	for _, descriptor := range descriptors {
		if err := transactions.named.dropTemporary(descriptor); err != nil {
			return err
		}
	}
	transactions.named.temporaryOptions = nil
	return nil
}

// temporaryMutationTargets resolves the write policy of a session-temporary
// relation before falling back to the catalog. A temporary descriptor is never
// published, so the catalog cannot rebuild its policy and index layouts; the
// session retains them when the declaration is admitted.
type temporaryMutationTargets struct {
	named    *sessionNamedObjects
	fallback execution.MutationTargets
}

func (targets temporaryMutationTargets) ResolveMutationTarget(ctx context.Context, ref storage.ObjectRef) (execution.MutationTarget, error) {
	if err := ctx.Err(); err != nil {
		return execution.MutationTarget{}, err
	}
	if target, ok := targets.named.temporaryTarget(ref.ObjectID); ok {
		return target.ResolveMutationTarget(ctx, ref)
	}
	if targets.fallback == nil {
		return execution.MutationTarget{}, storage.ErrInvalidTarget
	}
	return targets.fallback.ResolveMutationTarget(ctx, ref)
}
