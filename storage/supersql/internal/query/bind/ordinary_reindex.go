package bind

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func BindReindexTransactional(ctx context.Context, statement *zpgparse.ReindexStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSchemaDDL, error) {
	return bindReindex(ctx, statement, snapshot, options, false)
}

func BindReindexConcurrent(ctx context.Context, statement *zpgparse.ReindexStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSchemaDDL, error) {
	return bindReindex(ctx, statement, snapshot, options, true)
}

func ReindexConcurrent(statement *zpgparse.ReindexStmt) bool {
	concurrent, err := reindexOptions(statement)
	return err == nil && concurrent
}

func bindReindex(ctx context.Context, statement *zpgparse.ReindexStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions, durable bool) (*BoundSchemaDDL, error) {
	if statement == nil || statement.Relation == nil || snapshot == nil || statement.Relation.Relname == "" || statement.Relation.Catalogname != "" || statement.Relation.Alias != nil {
		return nil, bindError("42601", "invalid REINDEX command", 0)
	}
	concurrent, err := reindexOptions(statement)
	if err != nil {
		return nil, err
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_ReindexStmt{ReindexStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	var table *model.Descriptor
	var selected model.ObjectID
	switch statement.Kind {
	case zpgparse.ReindexObjectType_REINDEX_OBJECT_TABLE:
		table, err = resolveOrdinaryObject(ctx, b, parts, []model.ObjectKind{model.ObjectTable, model.ObjectMaterializedView}, "42P01")
	case zpgparse.ReindexObjectType_REINDEX_OBJECT_INDEX:
		var index *model.Descriptor
		index, err = resolveOrdinaryObject(ctx, b, parts, []model.ObjectKind{model.ObjectIndex}, "42704")
		if err == nil {
			metadata, ok := index.Index()
			if !ok || metadata.Table == 0 {
				return nil, model.ErrInvalidDescriptor
			}
			published, resolveErr := snapshot.Descriptor(ctx, metadata.Table)
			if resolveErr != nil {
				return nil, resolveErr
			}
			table, selected = published.Descriptor, index.ID()
			b.dependency(index, 0)
		}
	default:
		return nil, bindError("42601", "REINDEX requires INDEX or TABLE", int(statement.Relation.Location)+1)
	}
	if err != nil {
		return nil, err
	}
	if table == nil || table.State() != model.StatePublic || table.Kind() != model.ObjectTable && table.Kind() != model.ObjectMaterializedView {
		return nil, model.ErrInvalidDescriptor
	}
	b.dependency(table, 0)
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Table == nil || spec.Version == ^uint64(0) || spec.Physical.Generation == 0 || spec.Physical.Generation == ^uint64(0) {
		return nil, missing("unavailable REINDEX target", int(statement.Relation.Location)+1)
	}
	temporary := spec.Table.Persistence == "t"
	if durable && (!concurrent || temporary) {
		return nil, missing("concurrent REINDEX target", int(statement.Relation.Location)+1)
	}
	if !durable && concurrent && !temporary {
		return nil, missing("REINDEX CONCURRENTLY", int(statement.Relation.Location)+1)
	}
	if durable {
		if table.Kind() != model.ObjectTable {
			return nil, missing("concurrent materialized-view REINDEX", int(statement.Relation.Location)+1)
		}
	}
	versionReserve, operation := uint64(1), schema.SchemaCreateIndexTransactional
	if durable {
		versionReserve, operation = 3, schema.SchemaReindex
	}
	definitions, err := reindexPartitionHierarchy(ctx, b, snapshot, options, table, selected, versionReserve, operation)
	if err != nil {
		return nil, err
	}
	if durable {
		definitions, err = prepareConcurrentReindexDefinitions(ctx, snapshot, options, definitions, selected)
		if err != nil {
			return nil, err
		}
	}
	if definitions != nil {
		if len(definitions) == 0 {
			return &BoundSchemaDDL{source: table, noOp: true, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
		}
		if spec.Table.PartitionStrategy == 0 && len(definitions) == 1 {
			mutation := selected
			if mutation == 0 {
				mutation = table.ID()
			}
			return &BoundSchemaDDL{
				source: table, mutation: mutation, definition: definitions[0],
				dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings),
			}, ctx.Err()
		}
		return &BoundSchemaDDL{
			source: table, mutation: selected, definitions: definitions,
			dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings),
		}, ctx.Err()
	}
	sourceIndexes, err := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
	if err != nil {
		return nil, err
	}
	if selected != 0 && !slices.ContainsFunc(sourceIndexes, func(index *model.Descriptor) bool { return index.ID() == selected }) {
		return nil, model.ErrInvalidDescriptor
	}
	if len(sourceIndexes) == 0 {
		return &BoundSchemaDDL{source: table, noOp: true, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
	}
	if spec.Version > ^uint64(0)-versionReserve {
		return nil, model.ErrDescriptorLimit
	}
	spec.Version += versionReserve
	spec.Physical.Generation++
	target, err := rebindTargetTableExpressions(ctx, table, &spec, snapshot, options)
	if err != nil {
		return nil, err
	}
	targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, sourceIndexes, target, 0)
	if err != nil {
		return nil, err
	}
	for i, index := range targetIndexes {
		targetSpec, specErr := index.SpecWithin(ctx, options.Descriptors)
		if specErr != nil || targetSpec.Index == nil {
			return nil, errors.Join(model.ErrInvalidDescriptor, specErr)
		}
		if selected != 0 && index.ID() != selected {
			targetSpec.Physical = sourceIndexes[i].Physical()
		}
		if selected == 0 || index.ID() == selected {
			targetSpec.Index.Invalid = false
			targetSpec.Index.NotReady = false
			targetSpec.Index.NotLive = false
		}
		targetIndexes[i], specErr = model.NewDescriptor(ctx, targetSpec, options.Descriptors)
		if specErr != nil {
			return nil, specErr
		}
	}
	definition := schema.JobDefinition{
		Operation: operation, SourceTable: table, TargetTable: target,
		SourceIndexes: sourceIndexes, TargetIndexes: targetIndexes,
	}
	if durable {
		prepared, prepareErr := prepareConcurrentReindexDefinitions(ctx, snapshot, options, []schema.JobDefinition{definition}, selected)
		if prepareErr != nil {
			return nil, prepareErr
		}
		definition = prepared[0]
		rebuilt := false
		for i, target := range definition.TargetIndexes {
			rebuilt = rebuilt || target.Physical().Generation > definition.SourceIndexes[i].Physical().Generation
		}
		if !rebuilt {
			return &BoundSchemaDDL{source: table, noOp: true, dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings)}, ctx.Err()
		}
	}
	mutation := selected
	if mutation == 0 {
		mutation = table.ID()
	}
	return &BoundSchemaDDL{
		source: table, mutation: mutation, definition: definition,
		dependencies: slices.Clone(b.dependencies), warnings: slices.Clone(b.warnings),
	}, ctx.Err()
}

func prepareConcurrentReindexDefinitions(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, definitions []schema.JobDefinition, selected model.ObjectID) ([]schema.JobDefinition, error) {
	for definitionIndex := range definitions {
		definition := &definitions[definitionIndex]
		if definition.Operation != schema.SchemaReindex || definition.SourceTable == nil || len(definition.SourceIndexes) != len(definition.TargetIndexes) {
			return nil, model.ErrInvalidDescriptor
		}
		table, err := definition.SourceTable.SpecWithin(ctx, options.Descriptors)
		if err != nil || table.Table == nil {
			return nil, errors.Join(model.ErrInvalidDescriptor, err)
		}
		for indexPosition, source := range definition.SourceIndexes {
			metadata, ok := source.Index()
			if !ok {
				return nil, model.ErrInvalidDescriptor
			}
			chosen := selected == 0
			if selected != 0 {
				chosen, err = partitionIndexDescendsFrom(ctx, snapshot, source, selected, options.Descriptors.MaxDepth, nil)
				if err != nil {
					return nil, err
				}
			}
			exclusion := slices.ContainsFunc(table.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
				return constraint.Kind == model.ConstraintExclude && constraint.Index == source.ID()
			})
			if exclusion && selected != 0 && chosen {
				return nil, bindError("0A000", "REINDEX INDEX CONCURRENTLY cannot rebuild an exclusion constraint index", 0)
			}
			rebuild := chosen && !exclusion && (selected != 0 || metadata.Valid())
			if rebuild && (!metadata.Ready() || !metadata.Live()) {
				return nil, bindError("55000", "concurrent reindex requires a live, write-maintained target index", 0)
			}
			targetSpec, err := definition.TargetIndexes[indexPosition].SpecWithin(ctx, options.Descriptors)
			if err != nil || targetSpec.Index == nil {
				return nil, errors.Join(model.ErrInvalidDescriptor, err)
			}
			if rebuild {
				targetSpec.Index.Invalid = false
				targetSpec.Index.NotReady = false
				targetSpec.Index.NotLive = false
			} else {
				targetSpec.Physical = source.Physical()
				targetSpec.Index.Invalid = metadata.Invalid
				targetSpec.Index.NotReady = metadata.NotReady
				targetSpec.Index.NotLive = metadata.NotLive
			}
			definition.TargetIndexes[indexPosition], err = model.NewDescriptor(ctx, targetSpec, options.Descriptors)
			if err != nil {
				return nil, err
			}
		}
	}
	return definitions, ctx.Err()
}

func reindexOptions(statement *zpgparse.ReindexStmt) (bool, error) {
	if statement == nil {
		return false, bindError("42601", "invalid REINDEX command", 0)
	}
	seen := make(map[string]bool, len(statement.Params))
	concurrent := false
	for _, node := range statement.Params {
		option := node.GetDefElem()
		if option == nil || option.Defnamespace != "" || option.Defaction != zpgparse.DefElemAction_DEFELEM_UNSPEC {
			return false, bindError("42601", "invalid REINDEX option", 0)
		}
		if seen[option.Defname] {
			return false, bindError("42601", "duplicate REINDEX option", int(option.Location)+1)
		}
		seen[option.Defname] = true
		switch option.Defname {
		case "verbose":
			if _, err := reindexBooleanOption(option); err != nil {
				return false, err
			}
		case "concurrently":
			value, err := reindexBooleanOption(option)
			if err != nil {
				return false, err
			}
			concurrent = value
		default:
			return false, missing("REINDEX option "+option.Defname, int(option.Location)+1)
		}
	}
	return concurrent, nil
}

func reindexBooleanOption(option *zpgparse.DefElem) (bool, error) {
	if option.Arg == nil {
		return true, nil
	}
	value, ok := copyRawString(option.Arg)
	if !ok {
		return false, bindError("22023", "REINDEX option requires a boolean", int(option.Location)+1)
	}
	switch strings.ToLower(value) {
	case "true", "on", "yes", "1":
		return true, nil
	case "false", "off", "no", "0":
		return false, nil
	default:
		return false, bindError("22023", "REINDEX option requires a boolean", int(option.Location)+1)
	}
}

func (d *BoundSchemaDDL) TransactionalReindexCommand() (*BoundObjectCommand, error) {
	if d == nil {
		return nil, model.ErrInvalidDescriptor
	}
	definitions := d.Definitions()
	if len(definitions) == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	updateCount := 0
	for _, definition := range definitions {
		if definition.Operation != schema.SchemaCreateIndexTransactional || definition.SourceTable == nil || definition.TargetTable == nil || len(definition.SourceIndexes) != len(definition.TargetIndexes) {
			return nil, model.ErrInvalidDescriptor
		}
		updateCount += len(definition.SourceIndexes) + 1
	}
	updates := make([]ObjectMutation, 0, updateCount)
	for _, definition := range definitions {
		updates = append(updates, ObjectMutation{Base: definition.SourceTable, Next: definition.TargetTable})
		for i := range definition.SourceIndexes {
			updates = append(updates, ObjectMutation{Base: definition.SourceIndexes[i], Next: definition.TargetIndexes[i]})
		}
	}
	return &BoundObjectCommand{
		updates: updates, dependencies: slices.Clone(d.dependencies), warnings: slices.Clone(d.warnings), tag: "REINDEX",
	}, nil
}

func (d *BoundSchemaDDL) SelectReindexDefinition(table model.ObjectID) (*BoundSchemaDDL, error) {
	if d == nil || table == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	definitions := d.Definitions()
	position := slices.IndexFunc(definitions, func(definition schema.JobDefinition) bool {
		return definition.SourceTable != nil && definition.SourceTable.ID() == table
	})
	if position < 0 {
		return nil, model.ErrInvalidDescriptor
	}
	selected := *d
	selected.definition = definitions[position]
	selected.definitions = nil
	selected.source = definitions[position].SourceTable
	selected.mutation = selected.source.ID()
	return &selected, nil
}
