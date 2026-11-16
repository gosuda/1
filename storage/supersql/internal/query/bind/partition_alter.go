package bind

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func hierarchyAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || len(statement.Cmds) != 1 {
		return false
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	if command == nil {
		return false
	}
	if statement.Objtype == zpgparse.ObjectType_OBJECT_INDEX {
		return command.Subtype == zpgparse.AlterTableType_AT_AttachPartition
	}
	if statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE {
		return false
	}
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_AddInherit, zpgparse.AlterTableType_AT_DropInherit,
		zpgparse.AlterTableType_AT_AttachPartition, zpgparse.AlterTableType_AT_DetachPartition,
		zpgparse.AlterTableType_AT_DetachPartitionFinalize:
		return true
	}
	return false
}

func HierarchyAlter(statement *zpgparse.AlterTableStmt) bool {
	return hierarchyAlter(statement)
}

func bindHierarchyAlter(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if !hierarchyAlter(statement) || statement.Relation == nil {
		return nil, bindError("42601", "invalid inheritance ALTER TABLE", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTableStmt{AlterTableStmt: statement}})
	if err != nil {
		return nil, err
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	if statement.Objtype == zpgparse.ObjectType_OBJECT_INDEX {
		return b.bindAttachPartitionIndex(ctx, statement, command, snapshot, options)
	}
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_AddInherit, zpgparse.AlterTableType_AT_DropInherit:
		child, err := b.resolveTableRange(statement.Relation)
		if err != nil {
			return nil, err
		}
		parentRange := command.Def.GetRangeVar()
		if parentRange == nil {
			return nil, bindError("42601", "INHERIT requires a parent table", 0)
		}
		parent, err := b.resolveTableRange(parentRange)
		if err != nil {
			return nil, err
		}
		if command.Subtype == zpgparse.AlterTableType_AT_AddInherit {
			return b.bindAttachInheritance(ctx, child, parent, nil, snapshot, options, "ALTER TABLE")
		}
		return b.bindDetachInheritance(ctx, child, parent, false, false, false, options, "ALTER TABLE")
	case zpgparse.AlterTableType_AT_AttachPartition:
		parent, err := b.resolveTableRange(statement.Relation)
		if err != nil {
			return nil, err
		}
		partition := command.Def.GetPartitionCmd()
		if partition == nil || partition.Name == nil || partition.Bound == nil || partition.Concurrent {
			return nil, bindError("42601", "invalid ATTACH PARTITION command", 0)
		}
		child, err := b.resolveTableRange(partition.Name)
		if err != nil {
			return nil, err
		}
		return b.bindAttachInheritance(ctx, child, parent, partition.Bound, snapshot, options, "ALTER TABLE")
	case zpgparse.AlterTableType_AT_DetachPartition, zpgparse.AlterTableType_AT_DetachPartitionFinalize:
		parent, err := b.resolveTableRange(statement.Relation)
		if err != nil {
			return nil, err
		}
		partition := command.Def.GetPartitionCmd()
		if partition == nil || partition.Name == nil || partition.Bound != nil {
			return nil, bindError("42601", "invalid DETACH PARTITION command", 0)
		}
		child, err := b.resolveTableRange(partition.Name)
		if err != nil {
			return nil, err
		}
		finalize := command.Subtype == zpgparse.AlterTableType_AT_DetachPartitionFinalize
		return b.bindDetachInheritance(ctx, child, parent, true, partition.Concurrent, finalize, options, "ALTER TABLE")
	default:
		return nil, bindError("42601", "invalid inheritance ALTER TABLE", 0)
	}
}

func (b *binder) resolveTableRange(node *zpgparse.RangeVar) (*model.Descriptor, error) {
	if node == nil || node.Catalogname != "" || node.Relname == "" {
		return nil, bindError("42601", "invalid table reference", 0)
	}
	parts := []string{node.Relname}
	if node.Schemaname != "" {
		parts = []string{node.Schemaname, node.Relname}
	}
	descriptor, _, err := resolveNamespaceObject(b.ctx, b.catalog, b.options.SearchPath, parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(node.Location)+1)
	if err == nil {
		b.dependency(descriptor, 0)
	}
	return descriptor, err
}

// resolveIndexRoot resolves a CREATE INDEX target. A materialized view is a
// valid index root, so it resolves here and the ordinary CREATE INDEX path
// applies the relation-kind gate; the partition planner declines a root that is
// not a partitioned table.
func (b *binder) resolveIndexRoot(node *zpgparse.RangeVar) (*model.Descriptor, error) {
	if node == nil || node.Catalogname != "" || node.Relname == "" {
		return nil, bindError("42601", "invalid table reference", 0)
	}
	parts := []string{node.Relname}
	if node.Schemaname != "" {
		parts = []string{node.Schemaname, node.Relname}
	}
	descriptor, _, err := resolveNamespaceObject(b.ctx, b.catalog, b.options.SearchPath, parts, []model.ObjectKind{model.ObjectTable, model.ObjectMaterializedView}, "42P01", int(node.Location)+1)
	if err == nil {
		b.dependency(descriptor, 0)
	}
	return descriptor, err
}

func (b *binder) bindAttachInheritance(ctx context.Context, child, parent *model.Descriptor, rawBound *zpgparse.PartitionBoundSpec, snapshot CatalogSnapshot, options CreateDeclarationOptions, tag string) (*BoundObjectCommand, error) {
	if child.ID() == parent.ID() {
		return nil, bindError("42P17", "table cannot inherit from itself", 0)
	}
	childSpec, err := child.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	parentSpec, err := parent.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if childSpec.Table == nil || parentSpec.Table == nil || slices.Contains(childSpec.Table.Parents, parent.ID()) ||
		childSpec.Table.PartitionBound != nil || rawBound != nil && len(childSpec.Table.Parents) != 0 {
		return nil, bindError("42P17", "table already belongs to this inheritance hierarchy", 0)
	}
	partition := rawBound != nil
	if partition != (parentSpec.Table.PartitionStrategy != 0) {
		return nil, bindError("42809", "partition attachment strategy differs from parent", 0)
	}
	if !partition && (childSpec.Table.PartitionStrategy != 0 || childSpec.Table.PartitionBound != nil ||
		parentSpec.Table.PartitionStrategy != 0 || parentSpec.Table.PartitionBound != nil) {
		return nil, bindError("42809", "declarative partition tables cannot participate in ordinary inheritance", 0)
	}
	var hierarchyMembers []*model.Descriptor
	hasDescendants := childSpec.Table.InheritanceChildren != 0
	if hasDescendants {
		if partition != (childSpec.Table.PartitionStrategy != 0) {
			return nil, bindError("0A000", "attached hierarchy mixes declarative partitioning and ordinary inheritance", 0)
		}
		hierarchyMembers, err = b.inheritanceDescriptors(child, 0)
		if err != nil {
			return nil, err
		}
		for _, member := range hierarchyMembers[1:] {
			table, ok := member.Table()
			declarative := ok && table.PartitionBound != nil && table.PartitionState == model.PartitionAttached
			if !ok || declarative != partition || partition && len(table.Parents) != 1 ||
				!partition && (table.PartitionStrategy != 0 || table.PartitionBound != nil) {
				return nil, bindError("0A000", "attached hierarchy mixes declarative partitioning and ordinary inheritance", 0)
			}
		}
	}
	if childSpec.Table.Persistence != parentSpec.Table.Persistence {
		return nil, bindError("42P16", "temporary and permanent tables cannot share an inheritance hierarchy", 0)
	}
	mapping, err := compatibleInheritedColumns(parentSpec.Table.Columns, childSpec.Table.Columns, partition)
	if err != nil {
		return nil, err
	}
	metadata := *cloneTableMetadata(childSpec.Table)
	for i := range metadata.Columns {
		column := &metadata.Columns[i]
		parentColumn := mapping[column.ID]
		if parentColumn != 0 && !column.SystemIdentity {
			column.InheritedFrom = append(slices.Clone(column.InheritedFrom), model.ColumnInheritance{Table: parent.ID(), Column: parentColumn})
		}
	}
	metadata.Parents = append(slices.Clone(childSpec.Table.Parents), parent.ID())
	if partition {
		metadata.PartitionBound, err = b.bindPartitionBound(ctx, rawBound, parent, parentSpec.Table)
		if err != nil {
			return nil, err
		}
		metadata.PartitionState = model.PartitionAttached
	}
	plan := &tableInheritancePlan{parents: []*model.Descriptor{parent}}
	if partition {
		plan.partitionParent = parent
	}
	for _, source := range parentSpec.Table.Columns {
		if source.State != model.StatePublic || source.SystemIdentity {
			continue
		}
		target := childColumnForParent(mapping, source.ID)
		if target == 0 {
			return nil, model.ErrInvalidDescriptor
		}
		plan.columns = append(plan.columns, inheritedColumnSource{parent: parent, column: source, child: target})
	}
	for _, constraint := range parentSpec.Table.Constraints {
		if constraint.NoInherit || constraint.Kind != model.ConstraintCheck && constraint.Kind != model.ConstraintNotNull &&
			(rawBound == nil || constraint.Kind != model.ConstraintPrimaryKey && constraint.Kind != model.ConstraintUnique && constraint.Kind != model.ConstraintForeignKey) {
			continue
		}
		columns := make([]model.ColumnID, len(constraint.Columns))
		for i, column := range constraint.Columns {
			columns[i] = childColumnForParent(mapping, column)
			if columns[i] == 0 {
				return nil, model.ErrInvalidDescriptor
			}
		}
		inherited := constraint
		inherited.Columns = slices.Clone(columns)
		inherited.OnDeleteColumns = slices.Clone(constraint.OnDeleteColumns)
		for i, column := range inherited.OnDeleteColumns {
			inherited.OnDeleteColumns[i] = childColumnForParent(mapping, column)
			if inherited.OnDeleteColumns[i] == 0 {
				return nil, model.ErrInvalidDescriptor
			}
		}
		inherited.OnUpdateColumns = slices.Clone(constraint.OnUpdateColumns)
		for i, column := range inherited.OnUpdateColumns {
			inherited.OnUpdateColumns[i] = childColumnForParent(mapping, column)
			if inherited.OnUpdateColumns[i] == 0 {
				return nil, model.ErrInvalidDescriptor
			}
		}
		plan.constraints = append(plan.constraints, inheritedConstraintSource{parent: parent, constraint: inherited, columns: columns, sources: 1})
	}
	originalConstraints := len(metadata.Constraints)
	state := declarationState{b: b, options: options, parent: child.Name().Parent, tableName: child.Name().Name, names: make(map[string]bool), reserved: make(map[string]bool), ids: map[model.ObjectID]bool{child.ID(): true, parent.ID(): true}, oids: map[model.OID]bool{child.OID(): true, parent.OID(): true}}
	for _, constraint := range metadata.Constraints {
		state.names[constraint.Name] = true
		state.ids[constraint.ID], state.oids[constraint.OID] = true, true
	}
	if err := state.installInheritedConstraints(&metadata, plan); err != nil {
		return nil, err
	}
	propagatedConstraints := make([]model.ObjectID, 0, len(plan.constraints))
	for _, source := range plan.constraints {
		if source.targetIndex >= 0 && source.targetIndex < len(metadata.Constraints) &&
			!slices.Contains(propagatedConstraints, metadata.Constraints[source.targetIndex].ID) {
			propagatedConstraints = append(propagatedConstraints, metadata.Constraints[source.targetIndex].ID)
		}
	}
	if err := state.installInheritedDefaultOIDs(&metadata, plan, nil); err != nil {
		return nil, err
	}
	childSpec.Dependencies = futureInheritanceDependencies(childSpec.Dependencies, []*model.Descriptor{parent})
	childSpec.Version++
	pendingMetadata := metadata
	pendingMetadata.Constraints = pendingMetadata.Constraints[:originalConstraints]
	pendingMetadata.Columns = slices.Clone(metadata.Columns)
	for i := range pendingMetadata.Columns {
		if pendingMetadata.Columns[i].Generated == nil {
			pendingMetadata.Columns[i].GeneratedStored = false
		}
	}
	childSpec.Table = &pendingMetadata
	pending, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if err := rebindInheritedExpressions(ctx, pending, &metadata, plan, snapshot, options); err != nil {
		return nil, err
	}
	partitionIndexes, partitionIndexUpdates, err := clonePartitionIndexes(ctx, b, pending, &metadata, plan, snapshot, options, state.ids, nil)
	if err != nil {
		return nil, err
	}
	childSpec.Table = &metadata
	nextChild, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if partition {
		if err := b.validatePartitionAttachment(ctx, parent, nextChild, options); err != nil {
			return nil, err
		}
	}
	if err := child.ValidateSuccessor(ctx, nextChild); err != nil {
		return nil, err
	}
	parentSpec.Table = cloneTableMetadata(parentSpec.Table)
	if parentSpec.Table.InheritanceChildren == ^uint32(0) {
		return nil, model.ErrInvalidDescriptor
	}
	parentSpec.Table.InheritanceChildren++
	if partition && metadata.PartitionBound.Default {
		if parentSpec.Table.PartitionDefault != 0 {
			return nil, bindError("42P17", "default partition already exists", 0)
		}
		parentSpec.Table.PartitionDefault = child.ID()
	}
	parentSpec.Version++
	nextParent, err := model.NewDescriptor(ctx, parentSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if err := parent.ValidateSuccessor(ctx, nextParent); err != nil {
		return nil, err
	}
	var subtreeUpdates []ObjectMutation
	if hasDescendants {
		subtreeIndexes, propagatedUpdates, err := b.propagateAttachedHierarchy(ctx, child, nextChild, hierarchyMembers, propagatedConstraints, partitionIndexes, partitionIndexUpdates, snapshot, options, &state, partition)
		if err != nil {
			return nil, err
		}
		partitionIndexes = append(partitionIndexes, subtreeIndexes...)
		subtreeUpdates = propagatedUpdates
	}
	updates := []ObjectMutation{{Base: parent, Next: nextParent}, {Base: child, Next: nextChild}}
	updates = append(updates, partitionIndexUpdates...)
	updates = append(updates, subtreeUpdates...)
	if partition {
		promotions, err := b.promoteAttachedTableIndexes(ctx, parent, nextChild, partitionIndexes, append(slices.Clone(partitionIndexUpdates), subtreeUpdates...), snapshot, options)
		if err != nil {
			return nil, err
		}
		updates = append(updates, promotions...)
	}
	if len(updates) > options.Descriptors.MaxItems || len(partitionIndexes) > options.Descriptors.MaxItems-len(updates) {
		return nil, model.ErrDescriptorLimit
	}
	command := &BoundObjectCommand{creates: partitionIndexes, updates: updates, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings), tag: tag}
	command.populate = true
	return command, nil
}

func (b *binder) propagateAttachedHierarchy(ctx context.Context, rootBase, rootNext *model.Descriptor, members []*model.Descriptor, rootConstraints []model.ObjectID, rootIndexes []*model.Descriptor, rootIndexUpdates []ObjectMutation, snapshot CatalogSnapshot, options CreateDeclarationOptions, state *declarationState, declarative bool) ([]*model.Descriptor, []ObjectMutation, error) {
	if rootBase == nil || rootNext == nil || len(members) < 2 || state == nil {
		return nil, nil, model.ErrInvalidDescriptor
	}
	base := make(map[model.ObjectID]*model.Descriptor, len(members))
	for _, member := range members {
		if member == nil {
			return nil, nil, model.ErrInvalidDescriptor
		}
		base[member.ID()] = member
		state.ids[member.ID()], state.oids[member.OID()] = true, true
		if table, ok := member.Table(); ok {
			for _, constraint := range table.Constraints {
				state.ids[constraint.ID], state.oids[constraint.OID] = true, true
			}
		}
		if member.ID() != rootBase.ID() {
			b.dependency(member, 0)
		}
	}
	if base[rootBase.ID()] == nil {
		return nil, nil, model.ErrInvalidDescriptor
	}
	next := map[model.ObjectID]*model.Descriptor{rootBase.ID(): rootNext}
	propagated := map[model.ObjectID][]model.ObjectID{rootBase.ID(): slices.Clone(rootConstraints)}
	knownIndexes := make(map[model.ObjectID]*model.Descriptor, len(rootIndexes)+len(rootIndexUpdates))
	for _, index := range rootIndexes {
		if index != nil {
			knownIndexes[index.ID()] = index
		}
	}
	for _, update := range rootIndexUpdates {
		if update.Next != nil && update.Next.Kind() == model.ObjectIndex {
			knownIndexes[update.Next.ID()] = update.Next
		}
	}
	processed := map[model.ObjectID]bool{rootBase.ID(): true}
	var creates []*model.Descriptor
	var updates []ObjectMutation
	for len(processed) < len(base) {
		var ready []*model.Descriptor
		for id, descriptor := range base {
			if processed[id] {
				continue
			}
			table, ok := descriptor.Table()
			if !ok {
				return nil, nil, model.ErrInvalidDescriptor
			}
			internal, complete := 0, true
			for _, parentID := range table.Parents {
				if base[parentID] == nil {
					continue
				}
				internal++
				if !processed[parentID] {
					complete = false
				}
			}
			if internal != 0 && complete {
				ready = append(ready, descriptor)
			}
		}
		if len(ready) == 0 {
			return nil, nil, bindError("42P17", "circular inheritance hierarchy", 0)
		}
		slices.SortFunc(ready, func(left, right *model.Descriptor) int {
			return compareObjectID(left.ID(), right.ID())
		})
		for _, child := range ready {
			childTable, _ := child.Table()
			var parentBases, parentNext []*model.Descriptor
			for _, parentID := range childTable.Parents {
				if base[parentID] == nil {
					continue
				}
				if next[parentID] == nil {
					return nil, nil, model.ErrInvalidDescriptor
				}
				parentBases = append(parentBases, base[parentID])
				parentNext = append(parentNext, next[parentID])
			}
			if declarative && len(parentNext) != 1 {
				return nil, nil, model.ErrInvalidDescriptor
			}
			nextChild, childCreates, childUpdates, childConstraints, err := b.propagateInheritanceNode(ctx, child, parentBases, parentNext, propagated, knownIndexes, snapshot, options, state, declarative)
			if err != nil {
				return nil, nil, err
			}
			next[child.ID()] = nextChild
			propagated[child.ID()] = childConstraints
			creates = append(creates, childCreates...)
			updates = append(updates, ObjectMutation{Base: child, Next: nextChild})
			updates = append(updates, childUpdates...)
			for _, index := range childCreates {
				knownIndexes[index.ID()] = index
			}
			for _, update := range childUpdates {
				if update.Next != nil && update.Next.Kind() == model.ObjectIndex {
					knownIndexes[update.Next.ID()] = update.Next
				}
			}
			if len(updates) > options.Descriptors.MaxItems || len(creates) > options.Descriptors.MaxItems-len(updates) {
				return nil, nil, model.ErrDescriptorLimit
			}
			processed[child.ID()] = true
		}
	}
	return creates, updates, ctx.Err()
}

func (b *binder) propagateInheritanceNode(ctx context.Context, child *model.Descriptor, parentBases, parents []*model.Descriptor, propagated map[model.ObjectID][]model.ObjectID, knownIndexes map[model.ObjectID]*model.Descriptor, snapshot CatalogSnapshot, options CreateDeclarationOptions, state *declarationState, declarative bool) (*model.Descriptor, []*model.Descriptor, []ObjectMutation, []model.ObjectID, error) {
	childSpec, err := child.SpecWithin(ctx, options.Descriptors)
	if err != nil || childSpec.Table == nil || childSpec.Version == ^uint64(0) || len(parentBases) != len(parents) {
		return nil, nil, nil, nil, errors.Join(err, model.ErrInvalidDescriptor)
	}
	metadata := cloneTableMetadata(childSpec.Table)
	plan := &tableInheritancePlan{parents: slices.Clone(parents)}
	if declarative {
		plan.partitionParent = parents[0]
	}
	var alreadyInherited []inheritedConstraintSource
	for parentIndex, parent := range parents {
		parentTable, ok := parent.Table()
		if !ok {
			return nil, nil, nil, nil, model.ErrInvalidDescriptor
		}
		parentBaseTable, ok := parentBases[parentIndex].Table()
		if !ok {
			return nil, nil, nil, nil, model.ErrInvalidDescriptor
		}
		mapping, err := compatibleInheritedColumns(parentTable.Columns, metadata.Columns, declarative)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		for _, constraintID := range propagated[parent.ID()] {
			index := slices.IndexFunc(parentTable.Constraints, func(candidate model.ConstraintDescriptor) bool {
				return candidate.ID == constraintID
			})
			if index < 0 {
				return nil, nil, nil, nil, model.ErrInvalidDescriptor
			}
			constraint := parentTable.Constraints[index]
			inherited := constraint
			inherited.Columns = make([]model.ColumnID, len(constraint.Columns))
			for i, column := range constraint.Columns {
				inherited.Columns[i] = childColumnForParent(mapping, column)
				if inherited.Columns[i] == 0 {
					return nil, nil, nil, nil, model.ErrInvalidDescriptor
				}
			}
			inherited.OnDeleteColumns = slices.Clone(constraint.OnDeleteColumns)
			for i, column := range inherited.OnDeleteColumns {
				inherited.OnDeleteColumns[i] = childColumnForParent(mapping, column)
				if inherited.OnDeleteColumns[i] == 0 {
					return nil, nil, nil, nil, model.ErrInvalidDescriptor
				}
			}
			inherited.OnUpdateColumns = slices.Clone(constraint.OnUpdateColumns)
			for i, column := range inherited.OnUpdateColumns {
				inherited.OnUpdateColumns[i] = childColumnForParent(mapping, column)
				if inherited.OnUpdateColumns[i] == 0 {
					return nil, nil, nil, nil, model.ErrInvalidDescriptor
				}
			}
			source := inheritedConstraintSource{parent: parent, constraint: inherited, columns: inherited.Columns, sources: 1}
			existingIndex := slices.IndexFunc(metadata.Constraints, func(candidate model.ConstraintDescriptor) bool {
				return candidate.Name == inherited.Name
			})
			if existingIndex >= 0 {
				existing := metadata.Constraints[existingIndex]
				sourceExisted := slices.ContainsFunc(parentBaseTable.Constraints, func(candidate model.ConstraintDescriptor) bool {
					return candidate.ID == constraint.ID
				})
				edgeExists := sourceExisted && existing.InheritanceCount != 0
				if declarative {
					edgeExists = edgeExists && existing.ParentTable == parent.ID() && existing.ParentConstraint == constraint.ID
				}
				if edgeExists {
					if !sameInheritedConstraint(existing, inherited) {
						return nil, nil, nil, nil, bindError("42710", "inherited constraint name conflicts with another constraint", 0)
					}
					source.targetIndex, source.reused = existingIndex, true
					alreadyInherited = append(alreadyInherited, source)
					continue
				}
			}
			plan.constraints = append(plan.constraints, source)
		}
	}
	originalConstraints := len(metadata.Constraints)
	state.names = make(map[string]bool, len(metadata.Constraints))
	for _, constraint := range metadata.Constraints {
		state.names[constraint.Name] = true
		state.ids[constraint.ID], state.oids[constraint.OID] = true, true
	}
	if err := state.installInheritedConstraints(metadata, plan); err != nil {
		return nil, nil, nil, nil, err
	}
	expressionPlan := *plan
	expressionPlan.constraints = append(slices.Clone(plan.constraints), alreadyInherited...)
	childConstraints := make([]model.ObjectID, 0, len(expressionPlan.constraints))
	for _, source := range expressionPlan.constraints {
		if source.targetIndex >= 0 && source.targetIndex < len(metadata.Constraints) &&
			!slices.Contains(childConstraints, metadata.Constraints[source.targetIndex].ID) {
			childConstraints = append(childConstraints, metadata.Constraints[source.targetIndex].ID)
		}
	}
	childSpec.Dependencies = futureInheritanceDependencies(childSpec.Dependencies, parentBases)
	childSpec.Version++
	pendingMetadata := *metadata
	pendingMetadata.Constraints = slices.Clone(metadata.Constraints[:originalConstraints])
	pendingMetadata.Columns = slices.Clone(metadata.Columns)
	for i := range pendingMetadata.Columns {
		if pendingMetadata.Columns[i].Generated == nil {
			pendingMetadata.Columns[i].GeneratedStored = false
		}
	}
	childSpec.Table = &pendingMetadata
	pending, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := rebindInheritedExpressions(ctx, pending, metadata, &expressionPlan, snapshot, options); err != nil {
		return nil, nil, nil, nil, err
	}
	indexes, indexUpdates, err := clonePartitionIndexes(ctx, b, pending, metadata, &expressionPlan, snapshot, options, state.ids, knownIndexes)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	childSpec.Table = metadata
	nextChild, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if err := child.ValidateSuccessor(ctx, nextChild); err != nil {
		return nil, nil, nil, nil, err
	}
	return nextChild, indexes, indexUpdates, childConstraints, nil
}

func compatibleInheritedColumns(parent, child []model.ColumnDescriptor, exact bool) (map[model.ColumnID]model.ColumnID, error) {
	mapping := make(map[model.ColumnID]model.ColumnID)
	matched := 0
	if exact {
		parentIdentity := slices.IndexFunc(parent, func(column model.ColumnDescriptor) bool { return column.State == model.StatePublic && column.SystemIdentity })
		childIdentity := slices.IndexFunc(child, func(column model.ColumnDescriptor) bool { return column.State == model.StatePublic && column.SystemIdentity })
		if (parentIdentity < 0) != (childIdentity < 0) {
			return nil, bindError("42804", "partition row identity differs from parent", 0)
		}
		if parentIdentity >= 0 {
			if parentIdentity != childIdentity || !sameType(parent[parentIdentity].Layout, child[childIdentity].Layout) {
				return nil, bindError("42804", "partition row identity differs from parent", 0)
			}
			mapping[child[childIdentity].ID] = parent[parentIdentity].ID
		}
	}
	for _, parentColumn := range parent {
		if parentColumn.State != model.StatePublic || parentColumn.SystemIdentity {
			continue
		}
		index := slices.IndexFunc(child, func(candidate model.ColumnDescriptor) bool {
			return candidate.State == model.StatePublic && !candidate.SystemIdentity && candidate.Name == parentColumn.Name
		})
		if index < 0 || !sameType(parentColumn.Layout, child[index].Layout) ||
			parentColumn.GeneratedStored != child[index].GeneratedStored ||
			(parentColumn.Generated != nil) != (child[index].Generated != nil) {
			return nil, bindError("42804", "child table columns differ from parent", 0)
		}
		mapping[child[index].ID] = parentColumn.ID
		matched++
	}
	if exact {
		visible := 0
		parentPosition := 0
		for _, childColumn := range child {
			if childColumn.State != model.StatePublic || childColumn.SystemIdentity {
				continue
			}
			for parentPosition < len(parent) && (parent[parentPosition].State != model.StatePublic || parent[parentPosition].SystemIdentity) {
				parentPosition++
			}
			if parentPosition >= len(parent) || mapping[childColumn.ID] != parent[parentPosition].ID {
				return nil, bindError("42804", "partition columns differ from parent", 0)
			}
			parentPosition++
			visible++
		}
		if visible != matched {
			return nil, bindError("42804", "partition columns differ from parent", 0)
		}
	}
	return mapping, nil
}

func childColumnForParent(mapping map[model.ColumnID]model.ColumnID, parent model.ColumnID) model.ColumnID {
	for child, source := range mapping {
		if source == parent {
			return child
		}
	}
	return 0
}

func (b *binder) bindDetachInheritance(ctx context.Context, child, parent *model.Descriptor, requirePartition, concurrent, finalize bool, options CreateDeclarationOptions, tag string) (*BoundObjectCommand, error) {
	childSpec, err := child.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	parentSpec, err := parent.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if childSpec.Table == nil || parentSpec.Table == nil || !slices.Contains(childSpec.Table.Parents, parent.ID()) {
		return nil, bindError("42P17", "table is not a child of the specified parent", 0)
	}
	partition := childSpec.Table.PartitionBound != nil
	if partition != requirePartition {
		return nil, bindError("42809", "inheritance detach form does not match relationship kind", 0)
	}
	if finalize && (!partition || childSpec.Table.PartitionState != model.PartitionDetachPending) {
		return nil, bindError("55000", "partition detach is not pending", 0)
	}
	if concurrent && (!partition || finalize) {
		return nil, bindError("0A000", "CONCURRENTLY requires DETACH PARTITION", 0)
	}
	metadata := cloneTableMetadata(childSpec.Table)
	if concurrent {
		if metadata.PartitionState != model.PartitionAttached {
			return nil, bindError("55000", "partition is not attached", 0)
		}
		metadata.PartitionState = model.PartitionDetachPending
	} else {
		metadata.Parents = slices.DeleteFunc(metadata.Parents, func(id model.ObjectID) bool { return id == parent.ID() })
		metadata.PartitionBound = nil
		metadata.PartitionState = 0
		for i := range metadata.Columns {
			column := &metadata.Columns[i]
			column.InheritedFrom = slices.DeleteFunc(column.InheritedFrom, func(source model.ColumnInheritance) bool { return source.Table == parent.ID() })
			if len(column.InheritedFrom) == 0 {
				column.Local = true
			}
		}
		for i := range metadata.Constraints {
			constraint := &metadata.Constraints[i]
			parentConstraint := slices.IndexFunc(parentSpec.Table.Constraints, func(candidate model.ConstraintDescriptor) bool {
				return candidate.Name == constraint.Name && candidate.Kind == constraint.Kind
			})
			if constraint.ParentTable != parent.ID() && parentConstraint < 0 {
				continue
			}
			if constraint.InheritanceCount > 0 {
				constraint.InheritanceCount--
			}
			if constraint.InheritanceCount == 0 {
				constraint.ParentTable, constraint.ParentConstraint = 0, 0
				constraint.Local = true
				continue
			}
			if constraint.ParentTable == parent.ID() {
				replacementTable, replacementConstraint, err := inheritedConstraintParent(ctx, b.catalog, metadata.Parents, constraint.Name, constraint.Kind)
				if err != nil {
					return nil, err
				}
				constraint.ParentTable, constraint.ParentConstraint = replacementTable, replacementConstraint
			}
		}
	}
	childSpec.Table = metadata
	if concurrent {
		childSpec.Dependencies = futureInheritanceDependencies(childSpec.Dependencies, []*model.Descriptor{parent})
	} else {
		childSpec.Dependencies = removeInheritanceDependency(childSpec.Dependencies, parent.ID())
	}
	childSpec.Version++
	nextChild, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if err := child.ValidateSuccessor(ctx, nextChild); err != nil {
		return nil, err
	}
	updates := []ObjectMutation{{Base: child, Next: nextChild}}
	if partition && !concurrent {
		indexUpdates, err := detachPartitionIndexUpdates(ctx, childSpec.Table, parentSpec.Table, b.catalog, options.Descriptors)
		if err != nil {
			return nil, err
		}
		updates = append(updates, indexUpdates...)
	}
	if !finalize {
		parentSpec.Table = cloneTableMetadata(parentSpec.Table)
		if parentSpec.Table.InheritanceChildren == 0 {
			return nil, model.ErrInvalidDescriptor
		}
		parentSpec.Table.InheritanceChildren--
		if parentSpec.Table.PartitionDefault == child.ID() {
			parentSpec.Table.PartitionDefault = 0
		}
		parentSpec.Version++
		nextParent, err := model.NewDescriptor(ctx, parentSpec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if err := parent.ValidateSuccessor(ctx, nextParent); err != nil {
			return nil, err
		}
		updates = append([]ObjectMutation{{Base: parent, Next: nextParent}}, updates...)
	}
	return &BoundObjectCommand{updates: updates, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings), tag: tag}, nil
}

func cloneTableMetadata(metadata *model.TableMetadata) *model.TableMetadata {
	clone := *metadata
	clone.Columns = slices.Clone(metadata.Columns)
	for i := range clone.Columns {
		clone.Columns[i].InheritedFrom = slices.Clone(metadata.Columns[i].InheritedFrom)
	}
	clone.Indexes = slices.Clone(metadata.Indexes)
	clone.Parents = slices.Clone(metadata.Parents)
	clone.Constraints = slices.Clone(metadata.Constraints)
	return &clone
}

func inheritedConstraintParent(ctx context.Context, snapshot CatalogSnapshot, parents []model.ObjectID, name string, kind model.ConstraintKind) (model.ObjectID, model.ObjectID, error) {
	for _, parent := range parents {
		published, err := snapshot.Descriptor(ctx, parent)
		if err != nil {
			return 0, 0, err
		}
		table, ok := published.Descriptor.Table()
		if !ok {
			return 0, 0, model.ErrInvalidDescriptor
		}
		for _, constraint := range table.Constraints {
			if constraint.Name == name && constraint.Kind == kind {
				return parent, constraint.ID, nil
			}
		}
	}
	return 0, 0, model.ErrInvalidDescriptor
}
