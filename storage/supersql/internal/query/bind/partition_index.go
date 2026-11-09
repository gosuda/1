package bind

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func clonePartitionIndexes(ctx context.Context, b *binder, child *model.Descriptor, metadata *model.TableMetadata, plan *tableInheritancePlan, snapshot CatalogSnapshot, options CreateDeclarationOptions, ids map[model.ObjectID]bool, proposed map[model.ObjectID]*model.Descriptor) ([]*model.Descriptor, []ObjectMutation, error) {
	if b == nil || plan == nil || plan.partitionParent == nil {
		return nil, nil, nil
	}
	parentSpec, err := plan.partitionParent.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	if parentSpec.Table == nil {
		return nil, nil, model.ErrInvalidDescriptor
	}
	mapping := make(map[model.ColumnID]model.ColumnID)
	for _, column := range metadata.Columns {
		for _, inherited := range column.InheritedFrom {
			if inherited.Table == plan.partitionParent.ID() {
				mapping[inherited.Column] = column.ID
			}
		}
	}
	var result []*model.Descriptor
	var updates []ObjectMutation
	indexMap := make(map[model.ObjectID]model.ObjectID)
	known := make(map[model.ObjectID]*model.Descriptor, len(proposed))
	for id, descriptor := range proposed {
		known[id] = descriptor
	}
	for _, parentID := range parentSpec.Table.Indexes {
		parentIndex := known[parentID]
		if parentIndex == nil {
			published, err := snapshot.Descriptor(ctx, parentID)
			if err != nil {
				return nil, nil, err
			}
			parentIndex = published.Descriptor
		}
		parentIndexSpec, err := parentIndex.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, nil, err
		}
		if parentIndexSpec.Index == nil {
			return nil, nil, model.ErrInvalidDescriptor
		}
		if child.Version() > 1 {
			update, found, err := b.attachExistingPartitionIndex(ctx, plan.partitionParent, parentIndex, child, metadata, snapshot, options, known)
			if err != nil {
				return nil, nil, err
			}
			if found {
				indexMap[parentID] = update.Next.ID()
				known[update.Next.ID()] = update.Next
				if update.Base != update.Next {
					updates = append(updates, update)
				}
				continue
			}
		}
		indexID, indexOID, err := options.Identities.AllocateDeclarationIdentity(ctx)
		if err != nil {
			return nil, nil, err
		}
		if indexID == 0 || indexOID == 0 || ids[indexID] {
			return nil, nil, model.ErrInvalidDescriptor
		}
		ids[indexID] = true
		index := *parentIndexSpec.Index
		index.Table = child.ID()
		index.ParentIndex = parentID
		index.Invalid, index.NotReady, index.NotLive = false, false, false
		index.Keys = slices.Clone(index.Keys)
		for i := range index.Keys {
			key := &index.Keys[i]
			if key.Column != 0 {
				key.Column = mapping[key.Column]
				if key.Column == 0 {
					return nil, nil, model.ErrInvalidDescriptor
				}
			}
			if key.Expression != nil {
				key.Expression, err = transplantRowExpression(ctx, key.Expression, plan.partitionParent, child, 0, 0, 0, 0, model.TypeLayout{}, metadata.Columns, snapshot, options.Bind, rowIndexKey)
				if err != nil {
					return nil, nil, err
				}
			}
		}
		index.Include = slices.Clone(index.Include)
		for i, column := range index.Include {
			index.Include[i] = mapping[column]
			if index.Include[i] == 0 {
				return nil, nil, model.ErrInvalidDescriptor
			}
		}
		if index.Predicate != nil {
			index.Predicate, err = transplantRowExpression(ctx, index.Predicate, plan.partitionParent, child, 0, 0, 0, 0, model.TypeLayout{}, metadata.Columns, snapshot, options.Bind, rowIndexPredicate)
			if err != nil {
				return nil, nil, err
			}
		}
		name, err := b.partitionIndexName(child, indexID)
		if err != nil {
			return nil, nil, err
		}
		dependencies := []model.Dependency{{Object: child.ID(), Version: child.Version()}, {Object: parentID, Version: parentIndex.Version()}}
		for _, key := range index.Keys {
			if key.Column != 0 {
				dependencies = appendIndexDependency(dependencies, model.Dependency{Object: child.ID(), Version: child.Version(), Column: key.Column})
			}
			if key.Expression != nil {
				for _, dependency := range key.Expression.Dependencies {
					dependencies = appendIndexDependency(dependencies, dependency)
				}
			}
		}
		for _, column := range index.Include {
			dependencies = appendIndexDependency(dependencies, model.Dependency{Object: child.ID(), Version: child.Version(), Column: column})
		}
		if index.Predicate != nil {
			for _, dependency := range index.Predicate.Dependencies {
				dependencies = appendIndexDependency(dependencies, dependency)
			}
		}
		descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: indexID, OID: indexOID, Name: model.NamespaceName{Parent: child.Name().Parent, Kind: model.ObjectIndex, Name: name}, Version: 1, State: model.StatePublic, Physical: options.Physical, Dependencies: dependencies, Index: &index}, options.Descriptors)
		if err != nil {
			return nil, nil, err
		}
		metadata.Indexes = append(metadata.Indexes, indexID)
		indexMap[parentID] = indexID
		result = append(result, descriptor)
		known[indexID] = descriptor
	}
	for _, source := range plan.constraints {
		if source.targetIndex < 0 || source.targetIndex >= len(metadata.Constraints) {
			return nil, nil, model.ErrInvalidDescriptor
		}
		constraint := &metadata.Constraints[source.targetIndex]
		if source.constraint.Index == 0 {
			continue
		}
		constraint.Index = indexMap[source.constraint.Index]
		if constraint.Index == 0 {
			return nil, nil, model.ErrInvalidDescriptor
		}
	}
	return result, updates, nil
}

func (b *binder) attachExistingPartitionIndex(ctx context.Context, parent, parentIndex, child *model.Descriptor, childTable *model.TableMetadata, snapshot CatalogSnapshot, options CreateDeclarationOptions, proposed map[model.ObjectID]*model.Descriptor) (ObjectMutation, bool, error) {
	if parent == nil || parentIndex == nil || child == nil || childTable == nil {
		return ObjectMutation{}, false, model.ErrInvalidDescriptor
	}
	parentMetadata, ok := parentIndex.Index()
	if !ok {
		return ObjectMutation{}, false, model.ErrInvalidDescriptor
	}
	expected, expectedDependencies, err := transplantPartitionIndex(ctx, parent, parentIndex, parentMetadata, child, childTable, snapshot, options)
	if err != nil {
		return ObjectMutation{}, false, err
	}
	expected.Table = child.ID()
	for _, id := range childTable.Indexes {
		current := proposed[id]
		if current == nil {
			published, err := snapshot.Descriptor(ctx, id)
			if err != nil {
				return ObjectMutation{}, false, err
			}
			current = published.Descriptor
		}
		if current == nil {
			return ObjectMutation{}, false, model.ErrInvalidDescriptor
		}
		metadata, ok := current.Index()
		if !ok || current.State() != model.StatePublic || !metadata.Valid() || metadata.Table != child.ID() ||
			metadata.ParentIndex != 0 && metadata.ParentIndex != parentIndex.ID() {
			continue
		}
		candidate := expected
		candidate.ParentIndex = metadata.ParentIndex
		if !equivalentPartitionIndex(candidate, metadata) {
			continue
		}
		if metadata.ParentIndex == parentIndex.ID() {
			spec, err := current.SpecWithin(ctx, options.Descriptors)
			if err != nil || spec.Index == nil || spec.Version == ^uint64(0) {
				return ObjectMutation{}, false, errors.Join(err, model.ErrInvalidDescriptor)
			}
			b.dependency(current, 0)
			if slices.Equal(spec.Dependencies, expectedDependencies) {
				return ObjectMutation{Base: current, Next: current}, true, nil
			}
			spec.Dependencies = expectedDependencies
			spec.Version++
			next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
			if err != nil {
				return ObjectMutation{}, false, err
			}
			if err := current.ValidateSuccessor(ctx, next); err != nil {
				return ObjectMutation{}, false, err
			}
			return ObjectMutation{Base: current, Next: next}, true, nil
		}
		spec, err := current.SpecWithin(ctx, options.Descriptors)
		if err != nil || spec.Index == nil || spec.Version == ^uint64(0) {
			return ObjectMutation{}, false, errors.Join(err, model.ErrInvalidDescriptor)
		}
		nextMetadata := metadata
		nextMetadata.ParentIndex = parentIndex.ID()
		spec.Index = &nextMetadata
		spec.Dependencies = expectedDependencies
		spec.Version++
		next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
		if err != nil {
			return ObjectMutation{}, false, err
		}
		if err := current.ValidateSuccessor(ctx, next); err != nil {
			return ObjectMutation{}, false, err
		}
		b.dependency(current, 0)
		return ObjectMutation{Base: current, Next: next}, true, nil
	}
	return ObjectMutation{}, false, ctx.Err()
}

func detachPartitionIndexUpdates(ctx context.Context, child *model.TableMetadata, parent *model.TableMetadata, snapshot CatalogSnapshot, limits model.DescriptorLimits) ([]ObjectMutation, error) {
	parentIndexes := make(map[model.ObjectID]bool, len(parent.Indexes))
	for _, id := range parent.Indexes {
		parentIndexes[id] = true
	}
	var updates []ObjectMutation
	for _, id := range child.Indexes {
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil {
			return nil, err
		}
		spec, err := published.Descriptor.SpecWithin(ctx, limits)
		if err != nil {
			return nil, err
		}
		if spec.Index == nil || !parentIndexes[spec.Index.ParentIndex] {
			continue
		}
		index := *spec.Index
		spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
			return dependency.Object == index.ParentIndex
		})
		index.ParentIndex = 0
		spec.Index = &index
		spec.Version++
		next, err := model.NewDescriptor(ctx, spec, limits)
		if err != nil {
			return nil, err
		}
		if err := published.Descriptor.ValidateSuccessor(ctx, next); err != nil {
			return nil, err
		}
		updates = append(updates, ObjectMutation{Base: published.Descriptor, Next: next})
	}
	return updates, nil
}

// BindCreatePartitionIndex plans one metadata-only partitioned index and, unless
// ONLY was specified, a populated index partition for every attached descendant.
// The boolean is false when the target is not a partitioned table.
func BindCreatePartitionIndex(ctx context.Context, statement *zpgparse.IndexStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions, expressions IndexExpressionCompiler) (*BoundObjectCommand, bool, error) {
	if statement == nil || statement.Relation == nil || statement.Concurrent {
		return nil, false, nil
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_IndexStmt{IndexStmt: statement}})
	if err != nil {
		return nil, true, err
	}
	root, err := b.resolveIndexRoot(statement.Relation)
	if err != nil {
		return nil, true, err
	}
	// Only a partitioned table has index partitions to plan. A materialized
	// view resolves here so that the ordinary CREATE INDEX path decides
	// whether it can be indexed; it has no partition strategy.
	if root.Kind() != model.ObjectTable {
		return nil, false, nil
	}
	rootTable, ok := root.Table()
	if !ok {
		return nil, true, model.ErrInvalidDescriptor
	}
	if rootTable.PartitionStrategy == 0 {
		return nil, false, nil
	}
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, true, err
	}
	created, err := BindCreateIndex(ctx, statement, snapshot, CreateIndexOptions{
		Bind: options.Bind, Descriptors: options.Descriptors, ObjectID: id, OID: oid,
		Physical: options.Physical, Expressions: expressions, PartitionHierarchy: true,
	})
	if err != nil {
		return nil, true, err
	}
	if created.Existing != nil {
		b.dependency(created.Existing, 0)
		return &BoundObjectCommand{
			dependencies: slices.Clone(b.dependencies),
			warnings: []model.Error{{Code: "42P07", Message: "relation already exists, skipping"}},
			tag: "CREATE INDEX",
		}, true, nil
	}
	if created.Index == nil {
		return nil, true, model.ErrInvalidDescriptor
	}
	rootIndexSpec, err := created.Index.SpecWithin(ctx, options.Descriptors)
	if err != nil || rootIndexSpec.Index == nil {
		return nil, true, errors.Join(err, model.ErrInvalidDescriptor)
	}
	if rootIndexSpec.Index.Unique {
		if err := partitionIndexIncludesKeys(rootTable, rootIndexSpec.Index); err != nil {
			return nil, true, err
		}
	}
	rootSpec, err := root.SpecWithin(ctx, options.Descriptors)
	if err != nil || rootSpec.Table == nil || rootSpec.Version == ^uint64(0) || len(rootSpec.Table.Indexes) >= options.Descriptors.MaxItems {
		return nil, true, errors.Join(err, model.ErrDescriptorLimit)
	}
	rootMetadata := cloneTableMetadata(rootSpec.Table)
	rootMetadata.Indexes = append(rootMetadata.Indexes, created.Index.ID())
	rootSpec.Table, rootSpec.Version = rootMetadata, rootSpec.Version+1
	nextRoot, err := model.NewDescriptor(ctx, rootSpec, options.Descriptors)
	if err != nil {
		return nil, true, err
	}
	if err := root.ValidateSuccessor(ctx, nextRoot); err != nil {
		return nil, true, err
	}
	rootIndexSpec.Index.Invalid = !statement.Relation.Inh
	rootIndexSpec.State = model.StatePublic
	for i := range rootIndexSpec.Dependencies {
		if rootIndexSpec.Dependencies[i].Object == root.ID() {
			rootIndexSpec.Dependencies[i].Version = nextRoot.Version()
		}
	}
	rootIndex, err := model.NewDescriptor(ctx, rootIndexSpec, options.Descriptors)
	if err != nil {
		return nil, true, err
	}
	command := &BoundObjectCommand{
		creates: []*model.Descriptor{rootIndex},
		updates: []ObjectMutation{{Base: root, Next: nextRoot}},
		dependencies: slices.Clone(b.dependencies),
		tag: "CREATE INDEX",
	}
	b.dependency(root, 0)
	if !statement.Relation.Inh {
		command.dependencies = slices.Clone(b.dependencies)
		return command, true, ctx.Err()
	}
	members, err := b.inheritanceDescriptors(root, int(statement.Relation.Location)+1)
	if err != nil {
		return nil, true, err
	}
	byObject := make(map[model.ObjectID]*model.Descriptor, len(members))
	children := make(map[model.ObjectID][]*model.Descriptor)
	for _, descriptor := range members {
		byObject[descriptor.ID()] = descriptor
	}
	for _, descriptor := range members[1:] {
		table, ok := descriptor.Table()
		if !ok {
			return nil, true, model.ErrInvalidDescriptor
		}
		if table.PartitionBound == nil {
			continue
		}
		if table.PartitionState != model.PartitionAttached || len(table.Parents) != 1 || byObject[table.Parents[0]] == nil {
			return nil, true, model.ErrInvalidDescriptor
		}
		children[table.Parents[0]] = append(children[table.Parents[0]], descriptor)
	}
	for parent := range children {
		slices.SortFunc(children[parent], func(left, right *model.Descriptor) int {
			return compareObjectID(left.ID(), right.ID())
		})
	}
	var add func(*model.Descriptor, *model.Descriptor) error
	add = func(parent, parentIndex *model.Descriptor) error {
		for _, child := range children[parent.ID()] {
			if len(command.creates) >= options.Descriptors.MaxItems || len(command.updates) >= options.Descriptors.MaxItems {
				return model.ErrDescriptorLimit
			}
			childTable, ok := child.Table()
			if !ok {
				return model.ErrInvalidDescriptor
			}
			update, found, err := b.attachExistingPartitionIndex(ctx, parent, parentIndex, child, &childTable, snapshot, options, nil)
			if err != nil {
				return err
			}
			if found {
				if update.Base != update.Next {
					command.updates = append(command.updates, update)
				}
				b.dependency(child, 0)
				if childTable.PartitionStrategy != 0 {
					if err := add(child, update.Next); err != nil {
						return err
					}
				}
				continue
			}
			indexID, indexOID, err := options.Identities.AllocateDeclarationIdentity(ctx)
			if err != nil {
				return err
			}
			index, nextChild, err := b.cloneCreatedPartitionIndex(ctx, parent, parentIndex, child, indexID, indexOID, snapshot, options)
			if err != nil {
				return err
			}
			command.creates = append(command.creates, index)
			command.updates = append(command.updates, ObjectMutation{Base: child, Next: nextChild})
			b.dependency(child, 0)
			if childTable.PartitionStrategy != 0 {
				if err := add(child, index); err != nil {
					return err
				}
			} else {
				command.populate = true
			}
		}
		return nil
	}
	if err := add(root, rootIndex); err != nil {
		return nil, true, err
	}
	command.dependencies = slices.Clone(b.dependencies)
	return command, true, ctx.Err()
}

func partitionIndexIncludesKeys(table model.TableMetadata, index *model.IndexMetadata) error {
	for _, partitionKey := range table.PartitionKeys {
		if partitionKey.Column == 0 || !slices.ContainsFunc(index.Keys, func(key model.KeyElement) bool {
			return key.Column == partitionKey.Column
		}) {
			return bindError("0A000", "unique index on partitioned table must include every partition key column", 0)
		}
	}
	return nil
}

func (b *binder) cloneCreatedPartitionIndex(ctx context.Context, parent, parentIndex, child *model.Descriptor, id model.ObjectID, oid model.OID, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*model.Descriptor, *model.Descriptor, error) {
	if id == 0 || oid == 0 || parent == nil || parentIndex == nil || child == nil {
		return nil, nil, model.ErrInvalidDescriptor
	}
	parentMetadata, parentOK := parentIndex.Index()
	childSpec, err := child.SpecWithin(ctx, options.Descriptors)
	if err != nil || !parentOK || childSpec.Table == nil || childSpec.Version == ^uint64(0) || len(childSpec.Table.Indexes) >= options.Descriptors.MaxItems {
		return nil, nil, errors.Join(err, model.ErrInvalidDescriptor)
	}
	index, dependencies, err := transplantPartitionIndex(ctx, parent, parentIndex, parentMetadata, child, childSpec.Table, snapshot, options)
	if err != nil {
		return nil, nil, err
	}
	index.Table, index.ParentIndex = child.ID(), parentIndex.ID()
	if index.Unique {
		if err := partitionIndexIncludesKeys(*childSpec.Table, &index); err != nil {
			return nil, nil, err
		}
	}
	childMetadata := cloneTableMetadata(childSpec.Table)
	childMetadata.Indexes = append(childMetadata.Indexes, id)
	childSpec.Table, childSpec.Version = childMetadata, childSpec.Version+1
	nextChild, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	if err := child.ValidateSuccessor(ctx, nextChild); err != nil {
		return nil, nil, err
	}
	for i := range dependencies {
		if dependencies[i].Object == child.ID() {
			dependencies[i].Version = nextChild.Version()
		}
	}
	name, err := b.partitionIndexName(child, id)
	if err != nil {
		return nil, nil, err
	}
	descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: id, OID: oid, Name: model.NamespaceName{Parent: child.Name().Parent, Kind: model.ObjectIndex, Name: name},
		Version: 1, State: model.StatePublic, Physical: options.Physical, Dependencies: dependencies, Index: &index,
	}, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	return descriptor, nextChild, nil
}

func transplantPartitionIndex(ctx context.Context, parent, parentIndex *model.Descriptor, source model.IndexMetadata, child *model.Descriptor, childTable *model.TableMetadata, snapshot CatalogSnapshot, options CreateDeclarationOptions) (model.IndexMetadata, []model.Dependency, error) {
	if _, ok := parent.Table(); !ok {
		return model.IndexMetadata{}, nil, model.ErrInvalidDescriptor
	}
	mapping := make(map[model.ColumnID]model.ColumnID)
	for _, column := range childTable.Columns {
		for _, inherited := range column.InheritedFrom {
			if inherited.Table == parent.ID() {
				mapping[inherited.Column] = column.ID
			}
		}
	}
	result := source
	result.Invalid, result.NotReady, result.NotLive = false, false, false
	result.Keys = slices.Clone(source.Keys)
	dependencies := []model.Dependency{{Object: child.ID(), Version: child.Version()}, {Object: parentIndex.ID(), Version: parentIndex.Version()}}
	for i := range result.Keys {
		key := &result.Keys[i]
		if key.Column != 0 {
			key.Column = mapping[key.Column]
			if key.Column == 0 {
				return model.IndexMetadata{}, nil, model.ErrInvalidDescriptor
			}
			dependencies = appendIndexDependency(dependencies, model.Dependency{Object: child.ID(), Version: child.Version(), Column: key.Column})
		}
		if key.Expression != nil {
			var err error
			key.Expression, err = transplantRowExpression(ctx, key.Expression, parent, child, 0, 0, 0, 0, model.TypeLayout{}, childTable.Columns, snapshot, options.Bind, rowIndexKey)
			if err != nil {
				return model.IndexMetadata{}, nil, err
			}
			for _, dependency := range key.Expression.Dependencies {
				dependencies = appendIndexDependency(dependencies, dependency)
			}
		}
	}
	result.Include = slices.Clone(source.Include)
	for i, column := range result.Include {
		result.Include[i] = mapping[column]
		if result.Include[i] == 0 {
			return model.IndexMetadata{}, nil, model.ErrInvalidDescriptor
		}
		dependencies = appendIndexDependency(dependencies, model.Dependency{Object: child.ID(), Version: child.Version(), Column: result.Include[i]})
	}
	if source.Predicate != nil {
		var err error
		result.Predicate, err = transplantRowExpression(ctx, source.Predicate, parent, child, 0, 0, 0, 0, model.TypeLayout{}, childTable.Columns, snapshot, options.Bind, rowIndexPredicate)
		if err != nil {
			return model.IndexMetadata{}, nil, err
		}
		for _, dependency := range result.Predicate.Dependencies {
			dependencies = appendIndexDependency(dependencies, dependency)
		}
	}

	return result, dependencies, nil
}

func (b *binder) partitionIndexName(table *model.Descriptor, id model.ObjectID) (string, error) {
	base := table.Name().Name
	for attempt := uint32(0); attempt < b.options.Limits.MaxNodes; attempt++ {
		suffix := "_partition_index_" + strconv.FormatUint(uint64(id), 10)
		if attempt != 0 {
			suffix += "_" + strconv.FormatUint(uint64(attempt), 10)
		}
		name := clipIndexIdentifier(base, 63-len(suffix)) + suffix
		conflict, err := b.indexNameConflict(table.Name().Parent, name)
		if err != nil {
			return "", err
		}
		if conflict == nil {
			return name, nil
		}
	}
	return "", bindError("54000", "partition index name search limit exceeded", 0)
}

func (b *binder) promotePartitionIndexAncestors(ctx context.Context, parent, attached *model.Descriptor, snapshot CatalogSnapshot, options CreateDeclarationOptions) ([]ObjectMutation, error) {
	if parent == nil || attached == nil {
		return nil, model.ErrInvalidDescriptor
	}
	overrides := map[model.ObjectID]*model.Descriptor{attached.ID(): attached}
	return b.promotePartitionIndexChain(ctx, parent, snapshot, options, overrides, nil)
}

func (b *binder) promoteAttachedTableIndexes(ctx context.Context, parent, child *model.Descriptor, created []*model.Descriptor, mutations []ObjectMutation, snapshot CatalogSnapshot, options CreateDeclarationOptions) ([]ObjectMutation, error) {
	if parent == nil || child == nil {
		return nil, model.ErrInvalidDescriptor
	}
	parentTable, ok := parent.Table()
	if !ok || parentTable.PartitionStrategy == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	overrides := make(map[model.ObjectID]*model.Descriptor, len(created)+len(mutations))
	for _, descriptor := range created {
		if descriptor != nil && descriptor.Kind() == model.ObjectIndex {
			overrides[descriptor.ID()] = descriptor
		}
	}
	for _, mutation := range mutations {
		if mutation.Next != nil && mutation.Next.Kind() == model.ObjectIndex {
			overrides[mutation.Next.ID()] = mutation.Next
		}
	}
	var result []ObjectMutation
	for _, id := range parentTable.Indexes {
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil {
			return nil, err
		}
		index := published.Descriptor
		if index == nil {
			return nil, model.ErrInvalidDescriptor
		}
		updates, err := b.promotePartitionIndexChain(ctx, index, snapshot, options, overrides, []*model.Descriptor{child})
		if err != nil {
			return nil, err
		}
		result = append(result, updates...)
	}
	return result, ctx.Err()
}

func (b *binder) promotePartitionIndexChain(ctx context.Context, parent *model.Descriptor, snapshot CatalogSnapshot, options CreateDeclarationOptions, overrides map[model.ObjectID]*model.Descriptor, additional []*model.Descriptor) ([]ObjectMutation, error) {
	var updates []ObjectMutation
	current := parent
	for depth := 0; depth < options.Descriptors.MaxDepth; depth++ {
		if proposed := overrides[current.ID()]; proposed != nil {
			current = proposed
		}
		metadata, ok := current.Index()
		if !ok {
			return nil, model.ErrInvalidDescriptor
		}
		if metadata.Valid() || !metadata.Ready() || !metadata.Live() {
			return updates, nil
		}
		complete, err := b.partitionIndexComplete(ctx, current, snapshot, options, overrides, additional)
		if err != nil {
			return nil, err
		}
		if !complete {
			return updates, nil
		}
		spec, err := current.SpecWithin(ctx, options.Descriptors)
		if err != nil || spec.Index == nil || spec.Version == ^uint64(0) {
			return nil, errors.Join(err, model.ErrInvalidDescriptor)
		}
		nextMetadata := *spec.Index
		nextMetadata.Invalid = false
		spec.Index = &nextMetadata
		spec.Version++
		next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if err := current.ValidateSuccessor(ctx, next); err != nil {
			return nil, err
		}
		updates = append(updates, ObjectMutation{Base: current, Next: next})
		overrides[current.ID()] = next
		b.dependency(current, 0)
		if metadata.ParentIndex == 0 {
			return updates, nil
		}
		published, err := snapshot.Descriptor(ctx, metadata.ParentIndex)
		if err != nil {
			return nil, err
		}
		current = published.Descriptor
		if current == nil {
			return nil, model.ErrInvalidDescriptor
		}
	}
	return nil, model.ErrDescriptorLimit
}

func (b *binder) partitionIndexComplete(ctx context.Context, index *model.Descriptor, snapshot CatalogSnapshot, options CreateDeclarationOptions, overrides map[model.ObjectID]*model.Descriptor, additional []*model.Descriptor) (bool, error) {
	metadata, ok := index.Index()
	if !ok {
		return false, model.ErrInvalidDescriptor
	}
	published, err := snapshot.Descriptor(ctx, metadata.Table)
	if err != nil {
		return false, err
	}
	table := published.Descriptor
	if table == nil {
		return false, model.ErrInvalidDescriptor
	}
	tableMetadata, ok := table.Table()
	if !ok || tableMetadata.PartitionStrategy == 0 {
		return false, model.ErrInvalidDescriptor
	}
	members, err := b.inheritanceDescriptors(table, 0)
	if err != nil {
		return false, err
	}
	for _, child := range additional {
		if child == nil {
			return false, model.ErrInvalidDescriptor
		}
		if !slices.ContainsFunc(members, func(member *model.Descriptor) bool { return member.ID() == child.ID() }) {
			members = append(members, child)
		}
	}
	for _, child := range members[1:] {
		childTable, ok := child.Table()
		if !ok || childTable.PartitionBound == nil || childTable.PartitionState != model.PartitionAttached {
			return false, model.ErrInvalidDescriptor
		}
		if len(childTable.Parents) != 1 || childTable.Parents[0] != table.ID() {
			continue
		}
		matches := 0
		for _, id := range childTable.Indexes {
			candidate := overrides[id]
			if candidate == nil {
				published, err := snapshot.Descriptor(ctx, id)
				if err != nil {
					return false, err
				}
				candidate = published.Descriptor
			}
			if candidate == nil || candidate.State() != model.StatePublic {
				continue
			}
			candidateMetadata, ok := candidate.Index()
			if !ok {
				return false, model.ErrInvalidDescriptor
			}
			if candidateMetadata.ParentIndex != index.ID() {
				continue
			}
			matches++
			if !candidateMetadata.Valid() {
				return false, nil
			}
		}
		if matches != 1 {
			if matches > 1 {
				return false, model.ErrInvalidDescriptor
			}
			return false, nil
		}
	}
	return true, ctx.Err()
}

func (b *binder) bindAttachPartitionIndex(ctx context.Context, statement *zpgparse.AlterTableStmt, command *zpgparse.AlterTableCmd, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Relation == nil || command == nil {
		return nil, bindError("42601", "invalid ALTER INDEX ATTACH PARTITION", 0)
	}
	partition := command.Def.GetPartitionCmd()
	if partition == nil || partition.Name == nil || partition.Bound != nil || partition.Concurrent {
		return nil, bindError("42601", "invalid ALTER INDEX ATTACH PARTITION", 0)
	}
	parentIndex, err := b.resolveIndexRange(statement.Relation)
	if err != nil {
		return nil, err
	}
	childIndex, err := b.resolveIndexRange(partition.Name)
	if err != nil {
		return nil, err
	}
	parentMetadata, parentOK := parentIndex.Index()
	childMetadata, childOK := childIndex.Index()
	if !parentOK || !childOK || parentIndex.ID() == childIndex.ID() || childMetadata.ParentIndex != 0 ||
		parentMetadata.Valid() || !parentMetadata.Ready() || !parentMetadata.Live() || !childMetadata.Valid() {
		return nil, bindError("42809", "index is not attachable to this partitioned index", 0)
	}
	parentPublished, err := snapshot.Descriptor(ctx, parentMetadata.Table)
	if err != nil {
		return nil, err
	}
	childPublished, err := snapshot.Descriptor(ctx, childMetadata.Table)
	if err != nil {
		return nil, err
	}
	parentTableDescriptor, childTableDescriptor := parentPublished.Descriptor, childPublished.Descriptor
	if parentTableDescriptor == nil || childTableDescriptor == nil {
		return nil, model.ErrInvalidDescriptor
	}
	parentTable, parentTableOK := parentTableDescriptor.Table()
	childTable, childTableOK := childTableDescriptor.Table()
	directChild := childTableOK && childTable.PartitionBound != nil && childTable.PartitionState == model.PartitionAttached && len(childTable.Parents) == 1 && childTable.Parents[0] == parentTableDescriptor.ID()
	if !parentTableOK || parentTable.PartitionStrategy == 0 || !directChild ||
		!slices.Contains(parentTable.Indexes, parentIndex.ID()) || !slices.Contains(childTable.Indexes, childIndex.ID()) {
		return nil, bindError("42809", "index tables do not form a direct partition relationship", 0)
	}
	expected, _, err := transplantPartitionIndex(ctx, parentTableDescriptor, parentIndex, parentMetadata, childTableDescriptor, &childTable, snapshot, options)
	if err != nil {
		return nil, err
	}
	expected.Table, expected.ParentIndex = childTableDescriptor.ID(), parentIndex.ID()
	if !equivalentPartitionIndex(expected, childMetadata) {
		return nil, bindError("42809", "partition index definition differs from parent index", 0)
	}
	for _, id := range childTable.Indexes {
		if id == childIndex.ID() {
			continue
		}
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil {
			return nil, err
		}
		if published.Descriptor == nil {
			return nil, model.ErrInvalidDescriptor
		}
		metadata, ok := published.Descriptor.Index()
		if ok && metadata.ParentIndex == parentIndex.ID() {
			return nil, bindError("42710", "partition already has an index attached to this parent index", 0)
		}
	}
	spec, err := childIndex.SpecWithin(ctx, options.Descriptors)
	if err != nil || spec.Index == nil || spec.Version == ^uint64(0) {
		return nil, errors.Join(err, model.ErrInvalidDescriptor)
	}
	nextMetadata := *spec.Index
	nextMetadata.ParentIndex = parentIndex.ID()
	spec.Index = &nextMetadata
	spec.Version++
	spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: parentIndex.ID(), Version: parentIndex.Version()})
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if err := childIndex.ValidateSuccessor(ctx, next); err != nil {
		return nil, err
	}
	updates := []ObjectMutation{{Base: childIndex, Next: next}}
	promotions, err := b.promotePartitionIndexAncestors(ctx, parentIndex, next, snapshot, options)
	if err != nil {
		return nil, err
	}
	updates = append(updates, promotions...)
	for _, descriptor := range []*model.Descriptor{parentIndex, childIndex, parentTableDescriptor, childTableDescriptor} {
		b.dependency(descriptor, 0)
	}
	return &BoundObjectCommand{
		updates: updates,
		dependencies: slices.Clone(b.dependencies),
		tag: "ALTER INDEX",
	}, ctx.Err()
}

func (b *binder) resolveIndexRange(node *zpgparse.RangeVar) (*model.Descriptor, error) {
	if node == nil || node.Catalogname != "" || node.Relname == "" {
		return nil, bindError("42601", "invalid index reference", 0)
	}
	parts := []string{node.Relname}
	if node.Schemaname != "" {
		parts = []string{node.Schemaname, node.Relname}
	}
	descriptor, _, err := resolveNamespaceObject(b.ctx, b.catalog, b.options.SearchPath, parts, []model.ObjectKind{model.ObjectIndex}, "42704", int(node.Location)+1)
	if err == nil {
		b.dependency(descriptor, 0)
	}
	return descriptor, err
}

func sameStructuralExpression(left, right *model.ExpressionDefinition) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.SemanticVersion != right.SemanticVersion || left.SQL != right.SQL || !slices.Equal(left.Encoding, right.Encoding) ||
		len(left.Dependencies) != len(right.Dependencies) {
		return false
	}
	for i := range left.Dependencies {
		if left.Dependencies[i].Object != right.Dependencies[i].Object || left.Dependencies[i].Column != right.Dependencies[i].Column {
			return false
		}
	}
	return true
}

func equivalentPartitionIndex(expected, actual model.IndexMetadata) bool {
	if expected.Table != actual.Table || expected.Unique != actual.Unique || expected.NullsNotDistinct != actual.NullsNotDistinct ||
		expected.DeferredUnique != actual.DeferredUnique || expected.ParentIndex != actual.ParentIndex ||
		len(expected.Keys) != len(actual.Keys) || !slices.Equal(expected.Include, actual.Include) ||
		!sameStructuralExpression(expected.Predicate, actual.Predicate) {
		return false
	}
	if expected.PendingConstraint != nil || actual.PendingConstraint != nil || len(expected.PendingDependencies) != 0 || len(actual.PendingDependencies) != 0 {
		return false
	}
	for i, left := range expected.Keys {
		right := actual.Keys[i]
		if left.Column != right.Column || left.Type != right.Type || left.OperatorClass != right.OperatorClass || left.Collation != right.Collation ||
			left.Descending != right.Descending || left.NullsFirst != right.NullsFirst ||
			!slices.EqualFunc(left.Children, right.Children, sameType) ||
			!sameStructuralExpression(left.Expression, right.Expression) {
			return false
		}
	}
	return true
}

func partitionIndexDropClosure(ctx context.Context, root *model.Descriptor, snapshot CatalogSnapshot, maxScan uint64, maxIndexes int) ([]*model.Descriptor, error) {
	metadata, ok := root.Index()
	if !ok || snapshot == nil || maxScan == 0 || maxIndexes <= 0 {
		return nil, model.ErrInvalidDescriptor
	}
	if metadata.ParentIndex != 0 {
		return nil, bindError("2BP01", "cannot drop an attached index partition directly", 0)
	}
	published, err := snapshot.Descriptor(ctx, metadata.Table)
	if err != nil {
		return nil, err
	}
	if published.Descriptor == nil {
		return nil, model.ErrInvalidDescriptor
	}
	table, ok := published.Descriptor.Table()
	if !ok {
		return nil, model.ErrInvalidDescriptor
	}
	if table.PartitionStrategy == 0 {
		return nil, nil
	}
	var schemas []*model.Descriptor
	var scanned uint64
	visit := func() error {
		scanned++
		if scanned > maxScan {
			return model.ErrDescriptorLimit
		}
		return ctx.Err()
	}
	if err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(published catalog.PublishedDescriptor) error {
		if err := visit(); err != nil {
			return err
		}
		if published.Descriptor != nil && published.Descriptor.State() == model.StatePublic {
			schemas = append(schemas, published.Descriptor)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	children := make(map[model.ObjectID][]*model.Descriptor)
	for _, schema := range schemas {
		if err := snapshot.ScanNamespace(ctx, schema.ID(), model.ObjectIndex, func(published catalog.PublishedDescriptor) error {
			if err := visit(); err != nil {
				return err
			}
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic || descriptor.ID() == root.ID() {
				return nil
			}
			index, ok := descriptor.Index()
			if !ok {
				return model.ErrInvalidDescriptor
			}
			if index.ParentIndex != 0 {
				children[index.ParentIndex] = append(children[index.ParentIndex], descriptor)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for parent := range children {
		slices.SortFunc(children[parent], func(left, right *model.Descriptor) int {
			return compareObjectID(left.ID(), right.ID())
		})
	}
	var result []*model.Descriptor
	visiting := make(map[model.ObjectID]bool)
	var descend func(*model.Descriptor) error
	descend = func(parentIndex *model.Descriptor) error {
		if visiting[parentIndex.ID()] {
			return model.ErrInvalidDescriptor
		}
		visiting[parentIndex.ID()] = true
		parentMetadata, ok := parentIndex.Index()
		if !ok {
			return model.ErrInvalidDescriptor
		}
		parentTablePublished, err := snapshot.Descriptor(ctx, parentMetadata.Table)
		if err != nil {
			return err
		}
		if parentTablePublished.Descriptor == nil {
			return model.ErrInvalidDescriptor
		}
		for _, childIndex := range children[parentIndex.ID()] {
			if len(result) >= maxIndexes {
				return model.ErrDescriptorLimit
			}
			childMetadata, ok := childIndex.Index()
			if !ok {
				return model.ErrInvalidDescriptor
			}
			childTablePublished, err := snapshot.Descriptor(ctx, childMetadata.Table)
			if err != nil {
				return err
			}
			if childTablePublished.Descriptor == nil {
				return model.ErrInvalidDescriptor
			}
			childTable, ok := childTablePublished.Descriptor.Table()
			direct := ok && childTable.PartitionBound != nil && childTable.PartitionState == model.PartitionAttached &&
				len(childTable.Parents) == 1 && childTable.Parents[0] == parentTablePublished.Descriptor.ID()
			if !direct {
				return model.ErrInvalidDescriptor
			}
			result = append(result, childIndex)
			if err := descend(childIndex); err != nil {
				return err
			}
		}
		delete(visiting, parentIndex.ID())
		return nil
	}
	if err := descend(root); err != nil {
		return nil, err
	}
	return result, ctx.Err()
}
