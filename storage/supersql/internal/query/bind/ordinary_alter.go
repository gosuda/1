package bind

import (
	"context"
	"slices"
	"strings"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// OrdinaryObjectAlter identifies metadata-only relation commands staged by the
// caller transaction rather than the durable schema-job service.
func OrdinaryObjectAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || statement.Relation == nil || len(statement.Cmds) == 0 {
		return false
	}
	for _, node := range statement.Cmds {
		command := node.GetAlterTableCmd()
		if command == nil {
			return false
		}
		switch statement.Objtype {
		case zpgparse.ObjectType_OBJECT_TABLE:
			switch command.Subtype {
			case zpgparse.AlterTableType_AT_ColumnDefault,
				zpgparse.AlterTableType_AT_SetIdentity,
				zpgparse.AlterTableType_AT_DropIdentity,
				zpgparse.AlterTableType_AT_SetExpression,
				zpgparse.AlterTableType_AT_DropExpression,
				zpgparse.AlterTableType_AT_AddOf,
				zpgparse.AlterTableType_AT_DropOf,
				zpgparse.AlterTableType_AT_SetNotNull,
				zpgparse.AlterTableType_AT_DropNotNull,
				zpgparse.AlterTableType_AT_SetStatistics:
			default:
				return false
			}
		case zpgparse.ObjectType_OBJECT_INDEX:
			if command.Subtype != zpgparse.AlterTableType_AT_SetStatistics {
				return false
			}
		case zpgparse.ObjectType_OBJECT_VIEW:
			if command.Subtype != zpgparse.AlterTableType_AT_SetRelOptions && command.Subtype != zpgparse.AlterTableType_AT_ResetRelOptions && command.Subtype != zpgparse.AlterTableType_AT_ReplaceRelOptions {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func SchemaJobAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE || statement.Relation == nil || len(statement.Cmds) == 0 {
		return false
	}
	for _, node := range statement.Cmds {
		command := node.GetAlterTableCmd()
		if command == nil {
			return false
		}
		switch command.Subtype {
		case zpgparse.AlterTableType_AT_AddColumn,
			zpgparse.AlterTableType_AT_DropColumn,
			zpgparse.AlterTableType_AT_AlterColumnType,
			zpgparse.AlterTableType_AT_AddConstraint,
			zpgparse.AlterTableType_AT_DropConstraint,
			zpgparse.AlterTableType_AT_AlterConstraint,
			zpgparse.AlterTableType_AT_ValidateConstraint:
		default:
			return false
		}
	}
	return true
}
func TransactionalGenerationAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE || statement.Relation == nil || len(statement.Cmds) != 1 {
		return false
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	if command == nil {
		return false
	}
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_AddColumn, zpgparse.AlterTableType_AT_DropColumn, zpgparse.AlterTableType_AT_AlterColumnType:
		return true
	default:
		return false
	}
}


func IdentityDeclarationAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE || statement.Relation == nil || len(statement.Cmds) != 1 {
		return false
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	return command != nil && command.Subtype == zpgparse.AlterTableType_AT_AddIdentity
}
func ExpressionAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE || statement.Relation == nil || len(statement.Cmds) != 1 {
		return false
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	return command != nil && (command.Subtype == zpgparse.AlterTableType_AT_SetExpression || command.Subtype == zpgparse.AlterTableType_AT_DropExpression)
}


func IdentitySequenceAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE || statement.Relation == nil || len(statement.Cmds) != 1 {
		return false
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	return command != nil && command.Subtype == zpgparse.AlterTableType_AT_SetIdentity
}

func BindIdentitySequenceAlter(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*zpgparse.AlterSeqStmt, error) {
	if !IdentitySequenceAlter(statement) {
		return nil, bindError("42601", "invalid SET IDENTITY command", 0)
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTableStmt{AlterTableStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	table, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(statement.Relation.Location)+1)
	if err != nil {
		if statement.MissingOk && isMissingObject(err) {
			return nil, nil
		}
		return nil, err
	}
	metadata, ok := table.Table()
	if !ok {
		return nil, model.ErrInvalidDescriptor
	}
	index := slices.IndexFunc(metadata.Columns, func(column model.ColumnDescriptor) bool {
		return column.Name == command.Name && column.State == model.StatePublic && !column.SystemIdentity
	})
	if index < 0 {
		return nil, bindError("42703", "column does not exist", 0)
	}
	column := metadata.Columns[index]
	if column.IdentitySequence == 0 {
		return nil, bindError("55000", "column is not an identity column", 0)
	}
	published, err := snapshot.Descriptor(ctx, column.IdentitySequence)
	if err != nil {
		return nil, err
	}
	sequence := published.Descriptor
	configuration, ok := sequence.Sequence()
	if !ok || configuration.OwnerTable != table.ID() || configuration.OwnerColumn != column.ID {
		return nil, model.ErrInvalidDescriptor
	}
	var sequenceOptions []*zpgparse.Node
	list := command.Def.GetList()
	if list == nil {
		return nil, bindError("42601", "SET IDENTITY requires options", 0)
	}
	for _, node := range list.Items {
		option := node.GetDefElem()
		if option == nil {
			return nil, bindError("42601", "invalid identity sequence option", 0)
		}
		if option.Defname != "generated" {
			sequenceOptions = append(sequenceOptions, node)
		}
	}
	if len(sequenceOptions) == 0 {
		return nil, nil
	}
	parent, err := snapshot.Descriptor(ctx, sequence.Name().Parent)
	if err != nil {
		return nil, err
	}
	if parent.Descriptor.Kind() != model.ObjectSchema {
		return nil, model.ErrInvalidDescriptor
	}
	relation := &zpgparse.RangeVar{Schemaname: parent.Descriptor.Name().Name, Relname: sequence.Name().Name, Inh: true, Relpersistence: "p", Location: -1}
	return &zpgparse.AlterSeqStmt{Sequence: relation, Options: sequenceOptions, ForIdentity: false, MissingOk: false}, ctx.Err()
}

func BindAlterTableIdentity(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundDeclaration, error) {
	if statement == nil || statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE || statement.Relation == nil || len(statement.Cmds) != 1 {
		return nil, bindError("42601", "invalid ADD IDENTITY command", 0)
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	if command == nil || command.Subtype != zpgparse.AlterTableType_AT_AddIdentity || command.Name == "" || command.Recurse || command.MissingOk || command.Behavior != zpgparse.DropBehavior_DROP_RESTRICT {
		return nil, bindError("42601", "invalid ADD IDENTITY command", 0)
	}
	identity := command.Def.GetConstraint()
	if identity == nil || identity.Contype != zpgparse.ConstrType_CONSTR_IDENTITY {
		return nil, bindError("42601", "invalid identity declaration", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTableStmt{AlterTableStmt: statement}})
	if err != nil {
		return nil, err
	}
	if statement.Relation.Catalogname != "" || statement.Relation.Relname == "" || statement.Relation.Alias != nil {
		return nil, bindError("42601", "invalid ALTER TABLE relation", int(statement.Relation.Location)+1)
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	base, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(statement.Relation.Location)+1)
	if err != nil {
		if statement.MissingOk && isMissingObject(err) {
			return &BoundDeclaration{warnings: []model.Error{{Code: "42P01", Message: "relation does not exist, skipping"}}}, nil
		}
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool {
		return column.Name == command.Name && column.State == model.StatePublic && !column.SystemIdentity
	})
	if index < 0 {
		return nil, bindError("42703", "column does not exist", 0)
	}
	column := &spec.Table.Columns[index]
	if column.IdentitySequence != 0 {
		return nil, bindError("42710", "column is already an identity column", 0)
	}
	if column.Default != nil || column.Generated != nil {
		return nil, bindError("428C9", "identity column cannot have a default or generation expression", 0)
	}
	wasNullable := column.Nullable
	b.dependency(base, 0)
	state := declarationState{
		b:         b,
		options:   options,
		parent:    base.Name().Parent,
		tableName: base.Name().Name,
		names:     make(map[string]bool),
		reserved:  make(map[string]bool),
		ids:       map[model.ObjectID]bool{base.ID(): true},
		oids:      map[model.OID]bool{base.OID(): true},
	}
	for _, constraint := range spec.Table.Constraints {
		if constraint.Name != "" {
			state.reserved[constraint.Name] = true
		}
	}
	sequences, _, err := state.ownedSequences(spec, nil, []tableConstraint{{column: column.ID, raw: identity}}, make(map[model.ColumnID]*zpgparse.Node))
	if err != nil {

		return nil, err
	}
	if len(sequences) != 1 {
		return nil, model.ErrInvalidDescriptor
	}
	var validation model.ObjectID
	if wasNullable {
		if len(spec.Table.Constraints) >= options.Descriptors.MaxItems {
			return nil, model.ErrDescriptorLimit
		}
		name, err := state.constraintName("", column.Name, "not_null", false)
		if err != nil {
			return nil, err
		}
		id, oid, err := state.allocate()
		if err != nil {
			return nil, err
		}
		spec.Table.Constraints = append(spec.Table.Constraints, model.ConstraintDescriptor{
			ID: id, OID: oid, Name: name, Kind: model.ConstraintNotNull,
			Columns: []model.ColumnID{column.ID}, Validated: true, Enforced: true, Local: true,
		})
		validation = id
	}
	pending := &pendingIdentityDeclaration{base: base, spec: spec, options: options, dependencies: b.dependencies, warnings: b.warnings, validation: validation}
	return &BoundDeclaration{identity: pending, sequences: sequences, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
}
func BindAlterTableExpression(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, *BoundSchemaDDL, error) {
	if !ExpressionAlter(statement) {
		return nil, nil, bindError("42601", "invalid ALTER COLUMN EXPRESSION command", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTableStmt{AlterTableStmt: statement}})
	if err != nil {
		return nil, nil, err
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	table, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(statement.Relation.Location)+1)
	if err != nil {
		if statement.MissingOk && isMissingObject(err) {
			command, bindErr := bindOrdinaryObjectAlter(ctx, statement, snapshot, options)
			return command, nil, bindErr
		}
		return nil, nil, err
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	command := statement.Cmds[0].GetAlterTableCmd()
	position := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool {
		return column.Name == command.Name && column.State == model.StatePublic && !column.SystemIdentity
	})
	if position < 0 {
		return nil, nil, bindError("42703", "column does not exist", 0)
	}
	column := &spec.Table.Columns[position]
	if column.Generated == nil {
		command, bindErr := bindOrdinaryObjectAlter(ctx, statement, snapshot, options)
		return command, nil, bindErr
	}
	rewrite := command.Subtype == zpgparse.AlterTableType_AT_SetExpression ||
		command.Subtype == zpgparse.AlterTableType_AT_DropExpression && !column.GeneratedStored
	if !rewrite {
		command, bindErr := bindOrdinaryObjectAlter(ctx, statement, snapshot, options)
		return command, nil, bindErr
	}
	b.dependency(table, 0)
	previous := column.Generated
	var transforms []schema.GenerationTransform
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_SetExpression:
		if command.Def == nil {
			return nil, nil, bindError("42601", "SET EXPRESSION requires an expression", 0)
		}
		definition, _, err := defineRowExpression(ctx, command.Def, table, spec.Table.Columns, snapshot, options.Bind, rowStoredGenerated, column.ID, &column.Layout, 0)
		if err != nil {
			return nil, nil, err
		}
		column.Generated = definition
		for _, dependency := range definition.Dependencies {
			b.dependencies = appendIndexDependency(b.dependencies, dependency)
		}
	case zpgparse.AlterTableType_AT_DropExpression:
		transform, err := redefineStoredGenerationTransform(ctx, previous, table, spec.Table.Columns, column.ID, column.Layout, snapshot, options.Bind)
		if err != nil {
			return nil, nil, err
		}
		transforms = []schema.GenerationTransform{{Column: column.ID, Expression: transform}}
		for _, dependency := range previous.Dependencies {
			b.dependencies = appendIndexDependency(b.dependencies, dependency)
		}
		column.Generated = nil
		column.GeneratedStored = false
		column.DefaultOID = 0
	default:
		return nil, nil, bindError("42601", "invalid ALTER COLUMN EXPRESSION command", 0)
	}
	if err := removeUnusedDefaultDependencies(ctx, snapshot, &spec, previous); err != nil {
		return nil, nil, err
	}
	if spec.Version == ^uint64(0) || spec.Physical.Generation == ^uint64(0) {
		return nil, nil, model.ErrDescriptorLimit
	}
	indexes, err := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
	if err != nil {
		return nil, nil, err
	}
	spec.Version++
	spec.Physical.Generation++
	target, err := rebindTargetTableExpressions(ctx, table, &spec, snapshot, options)
	if err != nil {
		return nil, nil, err
	}
	targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, indexes, target, column.ID)
	if err != nil {
		return nil, nil, err
	}
	bound := &BoundSchemaDDL{
		source: table,
		definition: schema.JobDefinition{
			Operation:     schema.SchemaAlterColumnExpression,
			SourceTable:   table,
			TargetTable:   target,
			SourceIndexes: indexes,
			TargetIndexes: targetIndexes,
			Transforms:    transforms,
		},
		mutation:     model.ObjectID(column.ID),
		dependencies: b.dependencies,
		warnings:     b.warnings,
	}
	return nil, bound, ctx.Err()
}

func redefineStoredGenerationTransform(ctx context.Context, definition *model.ExpressionDefinition, table *model.Descriptor, columns []model.ColumnDescriptor, targetColumn model.ColumnID, target model.TypeLayout, snapshot CatalogSnapshot, options Options) (*model.ExpressionDefinition, error) {
	format, version := rowStoredGenerated.encoding()
	if definition == nil || definition.SemanticVersion != version || definition.SQL != "" || table == nil {
		return nil, missing("stored generation expression semantic encoding", 0)
	}
	encoding, ast, err := decodeRowExpression(ctx, definition.Encoding, options.Limits, format, version)
	if err != nil {
		return nil, err
	}
	if encoding.Table != table.ID() || encoding.TargetColumn != targetColumn || encoding.Target == nil {
		return nil, bindError("42804", "generation expression identity changed", 0)
	}
	if err := rewriteIndexAST(ctx, ast.ProtoReflect(), func(node *zpgparse.Node) error {
		parameter := node.GetParamRef()
		if parameter == nil {
			return nil
		}
		position := int(parameter.Number) - 1
		if position < 0 || position >= len(encoding.Columns) {
			return bindError("42P02", "generation expression parameter metadata changed", 0)
		}
		column := tableColumn(columns, encoding.Columns[position].Column)
		if column.ID == 0 {
			return bindError("42703", "generation expression column does not exist", 0)
		}
		node.Node = columnReference(column.Name).Node
		return nil
	}); err != nil {
		return nil, err
	}
	options.SearchPath = slices.Clone(encoding.SearchPath)
	options.Collation = encoding.Collation
	options.DefaultCollation = encoding.DefaultCollation
	options.Builtin = encoding.Builtin
	transform, _, err := defineRowExpression(ctx, ast, table, columns, snapshot, options, rowGenerationTransform, targetColumn, &target, 0)
	return transform, err
}
func (d *BoundSchemaDDL) TransactionalExpressionCommand() (*BoundObjectCommand, error) {
	if d == nil || d.definition.Operation != schema.SchemaAlterColumnExpression || d.definition.SourceTable == nil || d.definition.TargetTable == nil || len(d.definition.SourceIndexes) != len(d.definition.TargetIndexes) {
		return nil, model.ErrInvalidDescriptor
	}
	updates := make([]ObjectMutation, 0, len(d.definition.SourceIndexes)+1)
	updates = append(updates, ObjectMutation{Base: d.definition.SourceTable, Next: d.definition.TargetTable})
	for i := range d.definition.SourceIndexes {
		updates = append(updates, ObjectMutation{Base: d.definition.SourceIndexes[i], Next: d.definition.TargetIndexes[i]})
	}
	return &BoundObjectCommand{updates: updates, dependencies: slices.Clone(d.dependencies), warnings: slices.Clone(d.warnings), tag: "ALTER TABLE"}, nil
}


func bindOrdinaryObjectAlter(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if !OrdinaryObjectAlter(statement) {
		return nil, bindError("42601", "invalid metadata ALTER command", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTableStmt{AlterTableStmt: statement}})
	if err != nil {
		return nil, err
	}
	if statement.Relation.Catalogname != "" || statement.Relation.Relname == "" || statement.Relation.Alias != nil {
		return nil, bindError("42601", "invalid ALTER relation", int(statement.Relation.Location)+1)
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	kinds := []model.ObjectKind{model.ObjectTable}
	switch statement.Objtype {
	case zpgparse.ObjectType_OBJECT_INDEX:
		kinds = []model.ObjectKind{model.ObjectIndex}
	case zpgparse.ObjectType_OBJECT_VIEW:
		kinds = []model.ObjectKind{model.ObjectView}
	}
	base, _, err := b.resolve(parts, kinds, "42P01", int(statement.Relation.Location)+1)
	if err != nil {
		if statement.MissingOk && isMissingObject(err) {
			return &BoundObjectCommand{tag: "ALTER " + strings.TrimPrefix(statement.Objtype.String(), "OBJECT_"), warnings: []model.Error{{Code: "42P01", Message: "relation does not exist, skipping"}}}, nil
		}
		return nil, err
	}
	b.dependency(base, 0)
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	var drops []*model.Descriptor
	var validations []model.ObjectID
	typedAlter := false
	for _, node := range statement.Cmds {
		command := node.GetAlterTableCmd()
		if spec.Table != nil {
			switch command.Subtype {
			case zpgparse.AlterTableType_AT_ColumnDefault:
				if err := bindAlterColumnDefault(ctx, b, snapshot, options, &spec, command); err != nil {
					return nil, err
				}
			case zpgparse.AlterTableType_AT_SetIdentity, zpgparse.AlterTableType_AT_DropIdentity:
				dropped, err := bindAlterIdentityMetadata(ctx, b, snapshot, options, &spec, command)
				if err != nil {
					return nil, err
				}
				if dropped != nil {
					drops = append(drops, dropped)
				}
			case zpgparse.AlterTableType_AT_SetExpression, zpgparse.AlterTableType_AT_DropExpression:
				if err := bindAlterGeneratedExpression(ctx, b, snapshot, options, base, &spec, command); err != nil {
					return nil, err
				}
			case zpgparse.AlterTableType_AT_SetNotNull, zpgparse.AlterTableType_AT_DropNotNull:
				validation, err := bindAlterNotNull(ctx, b, options, &spec, command)
				if err != nil {
					return nil, err
				}
				if validation != 0 {
					validations = append(validations, validation)
				}
			case zpgparse.AlterTableType_AT_AddOf, zpgparse.AlterTableType_AT_DropOf:
				if err := bindAlterTypedTable(ctx, b, snapshot, options, &spec, command); err != nil {
					return nil, err
				}
				typedAlter = true
			case zpgparse.AlterTableType_AT_SetStatistics:
				if err := bindSetStatistics(&spec, command, false); err != nil {
					return nil, err
				}
			}
		} else if spec.Index != nil {
			if err := bindSetStatistics(&spec, command, true); err != nil {
				return nil, err
			}
		} else if spec.View != nil {
			reset := command.Subtype == zpgparse.AlterTableType_AT_ResetRelOptions
			if command.Subtype == zpgparse.AlterTableType_AT_ReplaceRelOptions {
				spec.View.CheckOption = ""
				spec.View.SecurityBarrier = false
				spec.View.SecurityInvoker = false
			}
			if err := applyViewOptions(spec.View, command.Def, reset); err != nil {
				return nil, err
			}
			if spec.View.CheckOption != "" && (spec.View.Automatic == nil || len(spec.View.Automatic.Columns) == 0) {
				return nil, bindError("0A000", "WITH CHECK OPTION is supported only on automatically updatable views", viewOptionPosition(command.Def, "check_option", statement.Relation.Location))
			}
		} else {
			return nil, model.ErrInvalidDescriptor
		}
	}
	validations = slices.DeleteFunc(validations, func(validation model.ObjectID) bool {
		return spec.Table == nil || !slices.ContainsFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
			return constraint.ID == validation
		})
	})
	slices.Sort(validations)
	validations = slices.Compact(validations)
	if spec.Version == ^uint64(0) {
		return nil, model.ErrDescriptorLimit
	}
	spec.Version++
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	tag := "ALTER TABLE"
	switch statement.Objtype {
	case zpgparse.ObjectType_OBJECT_INDEX:
		tag = "ALTER INDEX"
	case zpgparse.ObjectType_OBJECT_VIEW:
		tag = "ALTER VIEW"
	}
	updates := []ObjectMutation{{Base: base, Next: next}}
	if typedAlter {
		typeUpdates, err := updateImplicitRelationRowTypes(ctx, snapshot, next, options.Descriptors)
		if err != nil {
			return nil, err
		}
		updates = append(updates, typeUpdates...)
	}
	return &BoundObjectCommand{tag: tag, updates: updates, drops: drops, validations: validations, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()

}
func bindSetStatistics(spec *model.DescriptorSpec, command *zpgparse.AlterTableCmd, index bool) error {
	if spec == nil || command == nil || command.Subtype != zpgparse.AlterTableType_AT_SetStatistics || command.Def == nil || command.Recurse || command.MissingOk || command.Behavior != zpgparse.DropBehavior_DROP_RESTRICT {
		return bindError("42601", "invalid SET STATISTICS command", 0)
	}
	value := command.Def.GetInteger()
	if value == nil || value.Ival < -1 || value.Ival > 10000 {
		return bindError("22023", "statistics target must be between 0 and 10000, or -1 to reset", 0)
	}
	target, set := int32(value.Ival), value.Ival >= 0
	if !set {
		target = 0
	}
	if index {
		position := int(command.Num) - 1
		if command.Name != "" || position < 0 || spec.Index == nil || position >= len(spec.Index.Keys) {
			return bindError("42703", "index column does not exist", 0)
		}
		key := &spec.Index.Keys[position]
		if key.Expression == nil {
			return bindError("0A000", "statistics target can only be set for an index expression", 0)
		}
		key.StatisticsTarget, key.StatisticsTargetSet = target, set
		return nil
	}
	if command.Name == "" || command.Num != 0 || spec.Table == nil {
		return bindError("42601", "invalid column statistics target", 0)
	}
	position := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool {
		return column.Name == command.Name && column.State == model.StatePublic && !column.SystemIdentity
	})
	if position < 0 {
		return bindError("42703", "column does not exist", 0)
	}
	column := &spec.Table.Columns[position]
	column.StatisticsTarget, column.StatisticsTargetSet = target, set
	return nil
}

func bindAlterTypedTable(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, spec *model.DescriptorSpec, command *zpgparse.AlterTableCmd) error {
	if command.Name != "" || command.Recurse || command.MissingOk || command.Behavior != zpgparse.DropBehavior_DROP_RESTRICT {
		return bindError("42601", "invalid typed table alteration", 0)
	}
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_AddOf:
		if spec.Table.TypedOf != 0 {
			return bindError("55000", "table is already a typed table", 0)
		}
		if len(spec.Table.Parents) != 0 || spec.Table.PartitionBound != nil {
			return bindError("42P16", "typed tables cannot participate in inheritance or partitioning", 0)
		}
		typeName := command.Def.GetTypeName()
		if typeName == nil {
			return bindError("42601", "OF requires a composite type", 0)
		}
		parts, err := names(typeName.Names)
		if err != nil {
			return err
		}
		typ, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectType}, "42704", int(typeName.Location)+1)
		if err != nil {
			return err
		}
		typeSpec, err := typ.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return err
		}
		if typeSpec.Type == nil || typeSpec.Type.DeclaredKind() != model.TypeKindComposite || typeSpec.Type.Relation == 0 {
			return bindError("42809", "typed table requires a standalone composite type", int(typeName.Location)+1)
		}
		published, err := snapshot.Descriptor(ctx, typeSpec.Type.Relation)
		if err != nil {
			return err
		}
		relation := published.Descriptor
		if relation.Kind() != model.ObjectCompositeRelation {
			return model.ErrInvalidDescriptor
		}
		columns := make([]*model.ColumnDescriptor, 0, len(spec.Table.Columns))
		for i := range spec.Table.Columns {
			column := &spec.Table.Columns[i]
			if column.SystemIdentity {
				continue
			}
			if column.State != model.StatePublic {
				return bindError("55000", "typed table columns must be public", 0)
			}
			columns = append(columns, column)
		}
		if len(columns) != len(typeSpec.Type.Fields) {
			return bindError("42804", "table row does not match composite type", int(typeName.Location)+1)
		}
		mappings := make([]model.TypedColumnMapping, len(columns))
		for i, field := range typeSpec.Type.Fields {
			column := columns[i]
			if field.ID == 0 || field.Name != column.Name || field.Nullable != column.Nullable || !sameRoutineResult(field.Layout, column.Layout) {
				return bindError("42804", "table row does not match composite type", int(typeName.Location)+1)
			}
			mappings[i] = model.TypedColumnMapping{Field: field.ID, Column: column.ID}
		}
		b.dependency(typ, 0)
		b.dependency(relation, 0)
		spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: typ.ID(), Version: typ.Version()})
		spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: relation.ID(), Version: relation.Version()})
		spec.Table.TypedOf = typ.ID()
		spec.Table.TypedColumns = mappings
		return nil
	case zpgparse.AlterTableType_AT_DropOf:
		if command.Def != nil {
			return bindError("42601", "NOT OF does not accept a type", 0)
		}
		if spec.Table.TypedOf == 0 {
			return bindError("55000", "table is not a typed table", 0)
		}
		published, err := snapshot.Descriptor(ctx, spec.Table.TypedOf)
		if err != nil {
			return err
		}
		typ := published.Descriptor
		if typ.Kind() != model.ObjectType {
			return model.ErrInvalidDescriptor
		}
		b.dependency(typ, 0)
		typeSpec, err := typ.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return err
		}
		var relation *model.Descriptor
		if typeSpec.Type != nil && typeSpec.Type.Relation != 0 {
			published, err := snapshot.Descriptor(ctx, typeSpec.Type.Relation)
			if err != nil {
				return err
			}
			relation = published.Descriptor
			b.dependency(relation, 0)
		}
		spec.Table.TypedOf = 0
		spec.Table.TypedColumns = nil
		removeTypedTableDependencies(spec, typ, relation)
		return nil
	default:
		return bindError("42601", "invalid typed table alteration", 0)
	}
}

func removeTypedTableDependencies(spec *model.DescriptorSpec, typ, relation *model.Descriptor) {
	expressions := nonTriggerExpressionDependencies(*spec)
	objects := nonTriggerObjectDependencies(*spec)
	kept := spec.Dependencies[:0]
	for _, dependency := range spec.Dependencies {
		candidate := typ != nil && dependency.Object == typ.ID() || relation != nil && dependency.Object == relation.ID()
		if !candidate {
			kept = append(kept, dependency)
			continue
		}
		if _, retain := expressions[dependency]; retain {
			kept = append(kept, dependency)
			continue
		}
		if _, retain := objects[dependency.Object]; retain {
			kept = append(kept, dependency)
			continue
		}
		if typ != nil && dependency.Object == typ.ID() && tableUsesType(spec.Table, model.TypeID(typ.OID())) {
			kept = append(kept, dependency)
		}
	}
	spec.Dependencies = kept
}
func bindAlterGeneratedExpression(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, spec *model.DescriptorSpec, command *zpgparse.AlterTableCmd) error {
	if command.Name == "" || command.Recurse || command.Behavior != zpgparse.DropBehavior_DROP_RESTRICT {
		return bindError("42601", "invalid ALTER COLUMN EXPRESSION command", 0)
	}
	index := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool {
		return column.Name == command.Name && column.State == model.StatePublic && !column.SystemIdentity
	})
	if index < 0 {
		return bindError("42703", "column does not exist", 0)
	}
	column := &spec.Table.Columns[index]
	if column.Generated == nil {
		if command.Subtype == zpgparse.AlterTableType_AT_DropExpression && command.MissingOk {
			b.warnings = append(b.warnings, model.Error{Code: "42704", Message: "generation expression does not exist, skipping"})
			return nil
		}
		return bindError("55000", "column is not a generated column", 0)
	}
	previous := column.Generated
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_SetExpression:
		if command.Def == nil {
			return bindError("42601", "SET EXPRESSION requires an expression", 0)
		}
		if column.GeneratedStored {
			return missing("stored generated expression rewrite", 0)
		}
		definition, _, err := defineRowExpression(ctx, command.Def, table, spec.Table.Columns, snapshot, options.Bind, rowStoredGenerated, column.ID, &column.Layout, 0)
		if err != nil {
			return err
		}
		column.Generated = definition
		for _, dependency := range definition.Dependencies {
			b.dependencies = appendIndexDependency(b.dependencies, dependency)
		}
	case zpgparse.AlterTableType_AT_DropExpression:
		if !column.GeneratedStored {
			return missing("virtual generated column storage materialization", 0)
		}
		column.Generated = nil
		column.GeneratedStored = false
		column.DefaultOID = 0
	default:
		return bindError("42601", "invalid ALTER COLUMN EXPRESSION command", 0)
	}
	return removeUnusedDefaultDependencies(ctx, snapshot, spec, previous)
}

func bindAlterIdentityMetadata(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, spec *model.DescriptorSpec, command *zpgparse.AlterTableCmd) (*model.Descriptor, error) {
	if command.Name == "" || command.Recurse || command.Behavior != zpgparse.DropBehavior_DROP_RESTRICT {
		return nil, bindError("42601", "invalid ALTER COLUMN IDENTITY command", 0)
	}
	index := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool {
		return column.Name == command.Name && column.State == model.StatePublic && !column.SystemIdentity
	})
	if index < 0 {
		return nil, bindError("42703", "column does not exist", 0)
	}
	column := &spec.Table.Columns[index]
	if column.IdentitySequence == 0 {
		if command.Subtype == zpgparse.AlterTableType_AT_DropIdentity && command.MissingOk {
			b.warnings = append(b.warnings, model.Error{Code: "42704", Message: "identity does not exist, skipping"})
			return nil, nil
		}
		return nil, bindError("55000", "column is not an identity column", 0)
	}
	published, err := snapshot.Descriptor(ctx, column.IdentitySequence)
	if err != nil {
		return nil, err
	}
	sequence := published.Descriptor
	if sequence.Kind() != model.ObjectSequence {
		return nil, model.ErrInvalidDescriptor
	}
	b.dependency(sequence, 0)
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_DropIdentity:
		column.IdentitySequence = 0
		column.IdentityAlways = false
		spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
			return dependency.Object == sequence.ID()
		})
		return sequence, nil
	case zpgparse.AlterTableType_AT_SetIdentity:
		list := command.Def.GetList()
		if list == nil || len(list.Items) == 0 {
			return nil, bindError("42601", "SET IDENTITY requires options", 0)
		}
		seenGenerated := false
		for _, node := range list.Items {
			option := node.GetDefElem()
			if option == nil {
				return nil, bindError("42601", "invalid identity sequence option", 0)
			}
			if option.Defname != "generated" {
				if !options.IdentitySequenceAltered {
					return nil, missing("identity sequence alteration options", int(option.Location)+1)
				}
				continue
			}
			if seenGenerated || option.Arg.GetInteger() == nil {
				return nil, bindError("42601", "conflicting or invalid identity generation mode", int(option.Location)+1)
			}
			switch byte(option.Arg.GetInteger().Ival) {
			case 'a':
				column.IdentityAlways = true
			case 'd':
				column.IdentityAlways = false
			default:
				return nil, bindError("42601", "invalid identity generation mode", int(option.Location)+1)
			}
			seenGenerated = true
		}
		return nil, nil
	default:
		return nil, bindError("42601", "invalid ALTER COLUMN IDENTITY command", 0)
	}
}


func bindAlterColumnDefault(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, spec *model.DescriptorSpec, command *zpgparse.AlterTableCmd) error {
	if command.Name == "" || command.Recurse || command.MissingOk || command.Behavior != zpgparse.DropBehavior_DROP_RESTRICT || command.Subtype != zpgparse.AlterTableType_AT_ColumnDefault {
		return bindError("42601", "invalid ALTER COLUMN DEFAULT command", 0)
	}
	index := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool {
		return column.Name == command.Name && column.State == model.StatePublic && !column.SystemIdentity
	})
	if index < 0 {
		return bindError("42703", "column does not exist", 0)
	}
	column := &spec.Table.Columns[index]
	if column.Generated != nil || column.IdentitySequence != 0 {
		return bindError("428C9", "generated and identity columns do not accept defaults", 0)
	}
	previous := column.Default
	column.Default = nil
	column.DefaultOID = 0
	if command.Def != nil {
		bound, err := defineDefault(ctx, command.Def, column.Layout, snapshot, options.Bind, true)
		if err != nil {
			return err
		}
		_, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
		if err != nil {
			return err
		}
		if oid == 0 {
			return model.ErrInvalidDescriptor
		}
		column.Default = &bound.Definition
		column.DefaultOID = oid
		b.warnings = append(b.warnings, bound.Warnings...)
		for _, dependency := range bound.Definition.Dependencies {
			b.dependencies = appendIndexDependency(b.dependencies, dependency)
			spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
		}
	}
	return removeUnusedDefaultDependencies(ctx, snapshot, spec, previous)
}

func removeUnusedDefaultDependencies(ctx context.Context, snapshot CatalogSnapshot, spec *model.DescriptorSpec, previous *model.ExpressionDefinition) error {
	if previous == nil {
		return nil
	}
	expressions := nonTriggerExpressionDependencies(*spec)
	objects := nonTriggerObjectDependencies(*spec)
	old := make(map[model.Dependency]bool, len(previous.Dependencies))
	for _, dependency := range previous.Dependencies {
		old[dependency] = true
	}
	kept := spec.Dependencies[:0]
	for _, dependency := range spec.Dependencies {
		if !old[dependency] {
			kept = append(kept, dependency)
			continue
		}
		if _, retain := expressions[dependency]; retain {
			kept = append(kept, dependency)
			continue
		}
		if _, retain := objects[dependency.Object]; retain {
			kept = append(kept, dependency)
			continue
		}
		published, err := snapshot.Descriptor(ctx, dependency.Object)
		if err != nil {
			return err
		}
		if published.Descriptor.Kind() == model.ObjectType && tableUsesType(spec.Table, model.TypeID(published.Descriptor.OID())) {
			kept = append(kept, dependency)
		}
	}
	spec.Dependencies = kept
	return nil
}

func tableUsesType(table *model.TableMetadata, id model.TypeID) bool {
	if table == nil {
		return false
	}
	for _, column := range table.Columns {
		if tableLikeLayoutContainsType(column.Layout, id) {
			return true
		}
	}
	return false
}

func applyViewOptions(view *model.ViewMetadata, raw *zpgparse.Node, reset bool) error {
	if view == nil || raw == nil || raw.GetList() == nil {
		return bindError("42601", "invalid view option list", 0)
	}
	seen := make(map[string]bool)
	for _, node := range raw.GetList().Items {
		option := node.GetDefElem()
		if option == nil || option.Defnamespace != "" || option.Defname == "" || seen[option.Defname] {
			return bindError("42601", "invalid or repeated view option", 0)
		}
		seen[option.Defname] = true
		switch option.Defname {
		case "check_option":
			if reset {
				if option.Arg != nil {
					return bindError("42601", "RESET check_option does not accept a value", int(option.Location)+1)
				}
				view.CheckOption = ""
				continue
			}
			valueNode := option.Arg.GetString_()
			if valueNode == nil || valueNode.Sval != "local" && valueNode.Sval != "cascaded" {
				return bindError("22023", "check_option must be local or cascaded", int(option.Location)+1)
			}
			view.CheckOption = valueNode.Sval
		case "security_barrier", "security_invoker":
			value := false
			if reset {
				if option.Arg != nil {
					return bindError("42601", "RESET view boolean option does not accept a value", int(option.Location)+1)
				}
			} else {
				var err error
				value, err = viewBooleanOption(option)
				if err != nil {
					return err
				}
			}
			if option.Defname == "security_barrier" {
				view.SecurityBarrier = value
			} else {
				view.SecurityInvoker = value
			}
		default:
			return missing("view option "+option.Defname, int(option.Location)+1)
		}
	}
	return nil
}

func viewOptionPosition(raw *zpgparse.Node, name string, fallback int32) int {
	if raw != nil {
		if list := raw.GetList(); list != nil {
			for _, node := range list.Items {
				if option := node.GetDefElem(); option != nil && option.Defname == name {
					return int(option.Location) + 1
				}
			}
		}
	}
	return int(fallback) + 1
}

func viewBooleanOption(option *zpgparse.DefElem) (bool, error) {
	if option.Arg == nil {
		return true, nil
	}
	value, ok := copyRawString(option.Arg)
	if !ok {
		return false, bindError("22023", "view option requires a boolean", int(option.Location)+1)
	}
	switch strings.ToLower(value) {
	case "true", "on", "yes", "1":
		return true, nil
	case "false", "off", "no", "0":
		return false, nil
	default:
		return false, bindError("22023", "view option requires a boolean", int(option.Location)+1)
	}
}

func applyCreateViewOptions(view *model.ViewMetadata, nodes []*zpgparse.Node) error {
	if len(nodes) == 0 {
		return nil
	}
	list := &zpgparse.Node{Node: &zpgparse.Node_List{List: &zpgparse.List{Items: nodes}}}
	trailing := view.CheckOption
	view.CheckOption = ""
	if err := applyViewOptions(view, list, false); err != nil {
		return err
	}
	if trailing != "" {
		if view.CheckOption != "" {
			return bindError("42601", "view check option specified more than once", 0)
		}
		view.CheckOption = trailing
	}
	return nil
}
