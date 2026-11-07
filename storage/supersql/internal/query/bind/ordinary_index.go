package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func BindCreateIndexTransactional(ctx context.Context, statement *zpgparse.IndexStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions, expressions IndexExpressionCompiler) (*BoundSchemaDDL, error) {
	if statement == nil || statement.Relation == nil || statement.Concurrent {
		return nil, bindError("42601", "invalid transactional CREATE INDEX", 0)
	}
	hierarchy, handled, err := BindCreatePartitionIndex(ctx, statement, snapshot, options, expressions)
	if err != nil {
		return nil, err
	}
	if handled {
		result := &BoundSchemaDDL{hierarchyIndex: hierarchy}
		if hierarchy != nil {
			result.dependencies = hierarchy.Dependencies()
			result.warnings = hierarchy.Warnings()
			updates := hierarchy.Updates()
			if len(updates) != 0 {
				result.source = updates[0].Base
			}
		}
		return result, ctx.Err()
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_IndexStmt{IndexStmt: statement}})
	if err != nil {
		return nil, err
	}
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if id == 0 || oid == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	created, err := BindCreateIndex(ctx, statement, snapshot, CreateIndexOptions{
		Bind: options.Bind, Descriptors: options.Descriptors, ObjectID: id, OID: oid,
		Physical: options.Physical, Expressions: expressions,
	})
	if err != nil {
		return nil, err
	}
	if created.Existing != nil {
		b.dependency(created.Existing, 0)
		return &BoundSchemaDDL{source: created.Table, metadataOnly: created.Existing, dependencies: b.dependencies, warnings: []model.Error{{Code: "42P07", Message: "relation already exists, skipping"}}}, ctx.Err()
	}
	if created.Table == nil || created.Index == nil {
		return nil, model.ErrInvalidDescriptor
	}
	table := created.Table
	tableSpec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if tableSpec.Table == nil || len(tableSpec.Table.Indexes) >= options.Descriptors.MaxItems || tableSpec.Version == ^uint64(0) || tableSpec.Physical.Generation == ^uint64(0) {
		return nil, model.ErrDescriptorLimit
	}
	b.dependency(table, 0)
	indexSpec, err := created.Index.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	for _, dependency := range indexSpec.Dependencies {
		published, resolveErr := snapshot.Descriptor(ctx, dependency.Object)
		if resolveErr != nil {
			return nil, resolveErr
		}
		b.dependency(published.Descriptor, dependency.Column)
	}
	sourceIndexes, err := onlineSourceIndexes(ctx, b, snapshot, options, tableSpec.Table.Indexes)
	if err != nil {
		return nil, err
	}
	tableSpec.Table.Indexes = append(tableSpec.Table.Indexes, created.Index.ID())
	tableSpec.Version++
	tableSpec.Physical.Generation++
	targetTable, err := rebindTargetTableExpressions(ctx, table, &tableSpec, snapshot, options)
	if err != nil {
		return nil, err
	}
	targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, sourceIndexes, targetTable, 0)
	if err != nil {
		return nil, err
	}
	indexSpec.State = model.StatePublic
	for i := range indexSpec.Dependencies {
		if indexSpec.Dependencies[i].Object == table.ID() {
			indexSpec.Dependencies[i].Version = targetTable.Version()
		}
	}
	index, err := model.NewDescriptor(ctx, indexSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	targetIndexes = append(targetIndexes, index)
	definition := schema.JobDefinition{
		Operation: schema.SchemaCreateIndexTransactional, SourceTable: table, TargetTable: targetTable,
		SourceIndexes: sourceIndexes, TargetIndexes: targetIndexes,
	}
	return &BoundSchemaDDL{source: table, definition: definition, mutation: index.ID(), indexAddition: index, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
}

func (d *BoundSchemaDDL) TransactionalIndexCommand() (*BoundObjectCommand, error) {
	if d == nil || d.definition.Operation != schema.SchemaCreateIndexTransactional || d.indexAddition == nil || d.definition.SourceTable == nil || d.definition.TargetTable == nil || len(d.definition.TargetIndexes) != len(d.definition.SourceIndexes)+1 {
		return nil, model.ErrInvalidDescriptor
	}
	updates := make([]ObjectMutation, 0, len(d.definition.SourceIndexes)+1)
	updates = append(updates, ObjectMutation{Base: d.definition.SourceTable, Next: d.definition.TargetTable})
	for i := range d.definition.SourceIndexes {
		updates = append(updates, ObjectMutation{Base: d.definition.SourceIndexes[i], Next: d.definition.TargetIndexes[i]})
	}
	return &BoundObjectCommand{
		creates: []*model.Descriptor{d.indexAddition}, updates: updates,
		dependencies: slices.Clone(d.dependencies), warnings: slices.Clone(d.warnings), tag: "CREATE INDEX",
	}, nil
}
func bindDropIndexes(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.RemoveType != zpgparse.ObjectType_OBJECT_INDEX || statement.Concurrent {
		return nil, bindError("42601", "invalid transactional DROP INDEX", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	type tableDrop struct {
		base *model.Descriptor
		spec model.DescriptorSpec
	}
	tables := make(map[model.ObjectID]*tableDrop)
	var order []model.ObjectID
	var drops []*model.Descriptor
	seen := make(map[model.ObjectID]bool)
	for _, node := range statement.Objects {
		list := node.GetList()
		if list == nil {
			return nil, bindError("42601", "invalid DROP INDEX name", 0)
		}
		parts, err := names(list.Items)
		if err != nil {
			return nil, err
		}
		index, err := resolveOrdinaryObject(ctx, b, parts, []model.ObjectKind{model.ObjectIndex}, "42704")
		if err != nil {
			if statement.MissingOk && isMissingObject(err) {
				b.warnings = append(b.warnings, model.Error{Code: "42704", Message: "index does not exist, skipping"})
				continue
			}
			return nil, err
		}
		if seen[index.ID()] {
			continue
		}
		indexes := []*model.Descriptor{index}
		descendants, err := partitionIndexDropClosure(ctx, index, snapshot, uint64(options.Bind.Limits.MaxOperands), options.Descriptors.MaxItems)
		if err != nil {
			return nil, err
		}
		indexes = append(indexes, descendants...)
		for _, index := range indexes {
			if seen[index.ID()] {
				continue
			}
			seen[index.ID()] = true
			metadata, ok := index.Index()
			if !ok || metadata.Table == 0 {
				return nil, model.ErrInvalidDescriptor
			}
			entry := tables[metadata.Table]
			if entry == nil {
				published, err := snapshot.Descriptor(ctx, metadata.Table)
				if err != nil {
					return nil, err
				}
				table := published.Descriptor
				spec, err := table.SpecWithin(ctx, options.Descriptors)
				if err != nil {
					return nil, err
				}
				if spec.Table == nil || spec.Version == ^uint64(0) {
					return nil, model.ErrInvalidDescriptor
				}
				entry = &tableDrop{base: table, spec: spec}
				tables[metadata.Table] = entry
				order = append(order, metadata.Table)
				b.dependency(table, 0)
			}
			for _, constraint := range entry.spec.Table.Constraints {
				if constraint.Index == index.ID() {
					return nil, bindError("2BP01", "cannot drop an index that backs a table constraint", 0)
				}
			}
			position := slices.Index(entry.spec.Table.Indexes, index.ID())
			if position < 0 {
				return nil, model.ErrInvalidDescriptor
			}
			entry.spec.Table.Indexes = slices.Delete(entry.spec.Table.Indexes, position, position+1)
			entry.spec.Dependencies = slices.DeleteFunc(entry.spec.Dependencies, func(dependency model.Dependency) bool {
				return dependency.Object == index.ID()
			})
			b.dependency(index, 0)
			if len(drops) >= options.Descriptors.MaxItems {
				return nil, model.ErrDescriptorLimit
			}
			drops = append(drops, index)
		}
	}
	updates := make([]ObjectMutation, 0, len(order))
	for _, id := range order {
		entry := tables[id]
		entry.spec.Version++
		next, err := model.NewDescriptor(ctx, entry.spec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		updates = append(updates, ObjectMutation{Base: entry.base, Next: next})
	}
	return &BoundObjectCommand{
		updates: updates, drops: drops, dependencies: slices.Clone(b.dependencies),
		warnings: slices.Clone(b.warnings), cascade: statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE, tag: "DROP INDEX",
	}, ctx.Err()
}
