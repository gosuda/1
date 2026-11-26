package bind

import (
	"context"
	"errors"
	"math"
	"slices"
	"sort"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type BoundSchemaDDL struct {
	definition                schema.JobDefinition
	definitions               []schema.JobDefinition
	source                    *model.Descriptor
	mutation                  model.ObjectID
	metadataOnly              *model.Descriptor
	noOp                      bool
	cascade                   bool
	validation                model.ObjectID
	indexAddition             *model.Descriptor
	indexDrops                []*model.Descriptor
	expectedVersion           uint64
	auxiliaryUpdates          []ObjectMutation
	transactionalCreates      []*model.Descriptor
	transactionalIndexCreates int
	hierarchyIndex            *BoundObjectCommand
	transactionalValidations  []model.ObjectID
	sequences                 []*BoundSequenceDefinition
	dependencies              []model.Dependency
	warnings                  []model.Error
}

func (d *BoundSchemaDDL) Source() *model.Descriptor        { return d.source }
func (d *BoundSchemaDDL) Definition() schema.JobDefinition { return d.definition }
func (d *BoundSchemaDDL) MutationID() model.ObjectID       { return d.mutation }
func (d *BoundSchemaDDL) Definitions() []schema.JobDefinition {
	if d == nil {
		return nil
	}
	if len(d.definitions) != 0 {
		return slices.Clone(d.definitions)
	}
	if d.definition.SourceTable != nil {
		return []schema.JobDefinition{d.definition}
	}
	return nil
}
func (d *BoundSchemaDDL) ReindexHierarchy() bool {
	return d != nil && len(d.definitions) != 0
}
func (d *BoundSchemaDDL) MetadataOnly() *model.Descriptor  { return d.metadataOnly }
func (d *BoundSchemaDDL) NoOp() bool                       { return d != nil && d.noOp }
func (d *BoundSchemaDDL) ValidationID() model.ObjectID     { return d.validation }
func (d *BoundSchemaDDL) IndexAddition() *model.Descriptor { return d.indexAddition }
func (d *BoundSchemaDDL) IndexDrops() []*model.Descriptor  { return slices.Clone(d.indexDrops) }
func (d *BoundSchemaDDL) AdditionalCreates() []*model.Descriptor {
	return slices.Clone(d.transactionalCreates)
}
func (d *BoundSchemaDDL) ValidationIDs() []model.ObjectID {
	return slices.Clone(d.transactionalValidations)
}
func (d *BoundSchemaDDL) PendingSequences() []*BoundSequenceDefinition {
	return slices.Clone(d.sequences)
}
func (d *BoundSchemaDDL) HierarchyIndexCommand() *BoundObjectCommand {
	if d == nil {
		return nil
	}
	return d.hierarchyIndex
}
func (d *BoundSchemaDDL) ExpectedVersion() uint64 {
	if d.expectedVersion != 0 {
		return d.expectedVersion
	}
	if d.source != nil {
		return d.source.Version()
	}
	return 0
}
func schemaDDLBinder(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, node *zpgparse.Node) (*binder, error) {
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, bindError("22023", "schema DDL binding requires a catalog", 0)
	}
	b, err := newExpressionBinder(ctx, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	remaining, payload := options.Bind.Limits.MaxNodes, options.Bind.Limits.MaxBytes
	if err := b.checkAST(node.ProtoReflect(), 0, &remaining, &payload); err != nil {
		return nil, err
	}
	if err := ValidateStatementFeatures(ctx, node); err != nil {
		return nil, err
	}
	return b, nil
}

func (d *BoundSchemaDDL) Dependencies() []model.Dependency { return slices.Clone(d.dependencies) }
func (d *BoundSchemaDDL) Warnings() []model.Error          { return slices.Clone(d.warnings) }

func bindAlterTable(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions, versionReserve uint64) (*BoundSchemaDDL, error) {
	if statement == nil || statement.Relation == nil || statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE || len(statement.Cmds) == 0 {
		return nil, bindError("42601", "missing ALTER TABLE ONLINE declaration", 0)
	}
	b, err := schemaDDLBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTableStmt{AlterTableStmt: statement}})
	if err != nil {
		return nil, err
	}
	rangeVar := statement.Relation
	if rangeVar.Catalogname != "" || rangeVar.Alias != nil || !rangeVar.Inh && versionReserve != 1 {
		return nil, missing("ALTER TABLE catalog, alias, or ONLY variant", int(rangeVar.Location)+1)
	}
	parts := []string{rangeVar.Relname}
	if rangeVar.Schemaname != "" {
		parts = []string{rangeVar.Schemaname, rangeVar.Relname}
	}
	table, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(rangeVar.Location)+1)
	if err != nil {
		if statement.MissingOk && isMissingObject(err) {
			return &BoundSchemaDDL{noOp: true, warnings: []model.Error{{Code: "42P01", Message: "relation does not exist, skipping"}}}, nil
		}
		return nil, err
	}
	if versionReserve == 0 || table.State() != model.StatePublic || table.Version() > math.MaxUint64-versionReserve || table.Physical().Generation == math.MaxUint64 {
		return nil, bindError("55000", "table is not available for an online schema change", int(rangeVar.Location)+1)
	}
	b.dependency(table, 0)
	command := statement.Cmds[0].GetAlterTableCmd()
	if command == nil {
		return nil, bindError("42601", "invalid ALTER TABLE command", 0)
	}
	if len(statement.Cmds) == 1 {
		switch command.Subtype {
		case zpgparse.AlterTableType_AT_AddColumn:
			return bindOnlineAddColumn(ctx, b, snapshot, options, table, command, versionReserve == 1)
		case zpgparse.AlterTableType_AT_DropColumn:
			return bindOnlineDropColumn(ctx, b, snapshot, options, table, command)
		case zpgparse.AlterTableType_AT_AlterColumnType:
			return bindOnlineColumnType(ctx, b, snapshot, options, table, command)
		case zpgparse.AlterTableType_AT_AddConstraint, zpgparse.AlterTableType_AT_DropConstraint,
			zpgparse.AlterTableType_AT_AlterConstraint, zpgparse.AlterTableType_AT_ValidateConstraint:
			return bindOnlineConstraint(ctx, b, snapshot, options, table, command)
		}
	}
	return bindOnlinePrimaryKey(ctx, b, snapshot, options, table, statement.Cmds)
}
func BindAlterTableOnline(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSchemaDDL, error) {
	if !supportedOnlineAlter(statement) {
		return nil, missing("ALTER TABLE ONLINE command combination", 0)
	}
	return bindAlterTable(ctx, statement, snapshot, options, 3)
}

func supportedOnlineAlter(statement *zpgparse.AlterTableStmt) bool {
	if statement == nil || len(statement.Cmds) == 0 {
		return false
	}
	if len(statement.Cmds) == 1 {
		command := statement.Cmds[0].GetAlterTableCmd()
		if command == nil {
			return false
		}
		switch command.Subtype {
		case zpgparse.AlterTableType_AT_AddColumn, zpgparse.AlterTableType_AT_DropColumn, zpgparse.AlterTableType_AT_AlterColumnType:
			return true
		case zpgparse.AlterTableType_AT_AddConstraint:
			constraint := command.Def.GetConstraint()
			return constraint != nil && constraint.Contype == zpgparse.ConstrType_CONSTR_PRIMARY
		default:
			return false
		}
	}
	if len(statement.Cmds) != 2 {
		return false
	}
	var drop, add bool
	for _, node := range statement.Cmds {
		command := node.GetAlterTableCmd()
		if command == nil {
			return false
		}
		switch command.Subtype {
		case zpgparse.AlterTableType_AT_DropConstraint:
			drop = !drop
			if !drop {
				return false
			}
		case zpgparse.AlterTableType_AT_AddConstraint:
			constraint := command.Def.GetConstraint()
			add = !add && constraint != nil && constraint.Contype == zpgparse.ConstrType_CONSTR_PRIMARY
			if !add {
				return false
			}
		default:
			return false
		}
	}
	return drop && add
}

func BindAlterTableTransactional(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSchemaDDL, error) {
	bound, err := bindAlterTable(ctx, statement, snapshot, options, 1)
	if err != nil || bound.noOp {
		return bound, err
	}
	if bound.metadataOnly != nil {
		if bound.validation != 0 && !slices.Contains(bound.transactionalValidations, bound.validation) {
			bound.transactionalValidations = append(bound.transactionalValidations, bound.validation)
		}
		if bound.indexAddition != nil {
			return promoteTransactionalConstraintIndex(ctx, bound, snapshot, options)
		}
		target, err := addPersistentSchemaDependencies(ctx, bound.metadataOnly, bound.dependencies, snapshot, options)
		if err != nil {
			return nil, err
		}
		bound.metadataOnly = target
		if err := attachTransactionalRelationTypes(ctx, bound, target, snapshot, options); err != nil {
			return nil, err
		}
		return bound, nil
	}
	if bound.definition.Operation == schema.SchemaDropIndex {
		definition := bound.definition
		if definition.SourceTable == nil || definition.TargetTable == nil || len(definition.SourceIndexes) != 1 {
			return nil, model.ErrInvalidDescriptor
		}
		spec, err := definition.TargetTable.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if definition.SourceTable.Version() == math.MaxUint64 {
			return nil, model.ErrDescriptorLimit
		}
		spec.Version = definition.SourceTable.Version() + 1
		target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		target, err = addPersistentSchemaDependencies(ctx, target, bound.dependencies, snapshot, options)
		if err != nil {
			return nil, err
		}
		bound.definition = schema.JobDefinition{}
		bound.metadataOnly = target
		bound.indexDrops = append(bound.indexDrops, definition.SourceIndexes[0])
		if err := attachTransactionalRelationTypes(ctx, bound, target, snapshot, options); err != nil {
			return nil, err
		}
		return bound, nil
	}
	definition := bound.definition
	if definition.SourceTable == nil || definition.TargetTable == nil {
		return nil, model.ErrInvalidDescriptor
	}
	switch definition.Operation {
	case schema.SchemaAddColumn, schema.SchemaDropColumn, schema.SchemaAlterColumnType, schema.SchemaRewritePrimaryKey:
	default:
		return nil, missing("ordinary transactional schema rewrite", 0)
	}
	sourceSpec, err := definition.SourceTable.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	targetSpec, err := definition.TargetTable.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	bindingSnapshot := snapshot
	if len(bound.transactionalCreates) != 0 {
		bindingSnapshot = descriptorBindingOverlay{CatalogSnapshot: snapshot, descriptors: bound.transactionalCreates}
	}
	if definition.Operation == schema.SchemaAddColumn || definition.Operation == schema.SchemaAlterColumnType {
		for _, dependency := range bound.dependencies {
			if dependency.Object == targetSpec.ID {
				continue
			}
			published, resolveErr := bindingSnapshot.Descriptor(ctx, dependency.Object)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if published.Descriptor.Kind() != model.ObjectIndex {
				targetSpec.Dependencies = appendIndexDependency(targetSpec.Dependencies, dependency)
			}
		}
	}
	if definition.Operation == schema.SchemaAlterColumnType {
		if err := pruneUnusedTableTypeDependencies(ctx, &targetSpec, bindingSnapshot); err != nil {
			return nil, err
		}
	}
	if sourceSpec.Table == nil || targetSpec.Table == nil || sourceSpec.Version == math.MaxUint64 || sourceSpec.Physical.Generation == math.MaxUint64 {
		return nil, model.ErrDescriptorLimit
	}
	targetSpec.Version = sourceSpec.Version + 1
	targetSpec.Physical.Generation = sourceSpec.Physical.Generation + 1
	target, err := rebindTargetTableExpressions(ctx, definition.SourceTable, &targetSpec, bindingSnapshot, options)
	if err != nil {
		return nil, err
	}
	sourceIndexes := definition.SourceIndexes
	if definition.Operation == schema.SchemaAddColumn {
		sourceIndexes = make([]*model.Descriptor, 0, len(sourceSpec.Table.Indexes))
		for _, id := range sourceSpec.Table.Indexes {
			published, resolveErr := snapshot.Descriptor(ctx, id)
			if resolveErr != nil {
				return nil, resolveErr
			}
			index := published.Descriptor
			if index.Kind() != model.ObjectIndex || index.State() != model.StatePublic {
				return nil, model.ErrInvalidDescriptor
			}
			sourceIndexes = append(sourceIndexes, index)
			bound.dependencies = appendSchemaDependency(bound.dependencies, index, 0)
		}
		column := slices.IndexFunc(targetSpec.Table.Columns, func(column model.ColumnDescriptor) bool {
			return model.ObjectID(column.ID) == bound.mutation
		})
		if column < 0 {
			return nil, model.ErrInvalidDescriptor
		}
		if targetSpec.Table.Columns[column].Default != nil {
			definition.Transforms = []schema.GenerationTransform{{Column: targetSpec.Table.Columns[column].ID, Expression: targetSpec.Table.Columns[column].Default}}
		}
	}
	changedColumn := model.ColumnID(0)
	if definition.Operation == schema.SchemaAlterColumnType {
		changedColumn = model.ColumnID(bound.mutation)
	}
	targetIndexes, err := onlineTargetIndexes(ctx, bindingSnapshot, options, sourceIndexes, target, changedColumn)
	if err != nil {
		return nil, err
	}
	if len(bound.transactionalCreates) != 0 {
		rebasedCreates := make([]*model.Descriptor, len(bound.transactionalCreates))
		for i, descriptor := range bound.transactionalCreates {
			spec, specErr := descriptor.SpecWithin(ctx, options.Descriptors)
			if specErr != nil {
				return nil, specErr
			}
			for j := range spec.Dependencies {
				if spec.Dependencies[j].Object == target.ID() {
					spec.Dependencies[j].Version = target.Version()
				}
			}
			created, createErr := model.NewDescriptor(ctx, spec, options.Descriptors)
			if createErr != nil {
				return nil, createErr
			}
			rebasedCreates[i] = created
			if created.Kind() == model.ObjectIndex {
				index, ok := created.Index()
				if !ok || index.Table != target.ID() {
					return nil, model.ErrInvalidDescriptor
				}
				targetIndexes = append(targetIndexes, created)
			}
		}
		bound.transactionalCreates = rebasedCreates
	}
	definition.SourceIndexes, definition.TargetIndexes = sourceIndexes, targetIndexes
	definition.TargetTable = target
	if err := attachTransactionalRelationTypes(ctx, bound, target, snapshot, options); err != nil {
		return nil, err
	}
	bound.definition = definition
	return bound, ctx.Err()
}
func promoteTransactionalConstraintIndex(ctx context.Context, bound *BoundSchemaDDL, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSchemaDDL, error) {
	source, target, addition := bound.source, bound.metadataOnly, bound.indexAddition
	if source == nil || target == nil || addition == nil || source.ID() != target.ID() {
		return nil, model.ErrInvalidDescriptor
	}
	sourceSpec, err := source.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	targetSpec, err := target.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if sourceSpec.Table == nil || targetSpec.Table == nil || sourceSpec.Version == math.MaxUint64 || sourceSpec.Physical.Generation == math.MaxUint64 {
		return nil, model.ErrDescriptorLimit
	}
	targetSpec.Version = sourceSpec.Version + 1
	targetSpec.Physical.Generation = sourceSpec.Physical.Generation + 1
	for _, dependency := range bound.dependencies {
		if dependency.Object == targetSpec.ID {
			continue
		}
		published, resolveErr := snapshot.Descriptor(ctx, dependency.Object)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if published.Descriptor.Kind() != model.ObjectIndex {
			targetSpec.Dependencies = appendIndexDependency(targetSpec.Dependencies, dependency)
		}
	}
	slices.SortFunc(targetSpec.Dependencies, compareDependency)
	targetSpec.Dependencies = slices.Compact(targetSpec.Dependencies)
	target, err = rebindTargetTableExpressions(ctx, source, &targetSpec, snapshot, options)
	if err != nil {
		return nil, err
	}
	indexBinder := &binder{dependencySet: make(map[model.Dependency]struct{})}
	sourceIndexes, err := onlineSourceIndexes(ctx, indexBinder, snapshot, options, sourceSpec.Table.Indexes)
	if err != nil {
		return nil, err
	}
	bound.dependencies = append(bound.dependencies, indexBinder.dependencies...)
	targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, sourceIndexes, target, 0)
	if err != nil {
		return nil, err
	}
	additionSpec, err := addition.SpecWithin(ctx, options.Descriptors)
	if err != nil || additionSpec.Index == nil {
		return nil, errors.Join(model.ErrInvalidDescriptor, err)
	}
	additionSpec.State = model.StatePublic
	additionSpec.Index.Invalid = false
	additionSpec.Index.NotReady = false
	additionSpec.Index.NotLive = false
	for i := range additionSpec.Dependencies {
		if additionSpec.Dependencies[i].Object == target.ID() {
			additionSpec.Dependencies[i].Version = target.Version()
		}
	}
	addition, err = model.NewDescriptor(ctx, additionSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if pending := additionSpec.Index.PendingConstraint; pending != nil && pending.Validated && !slices.Contains(bound.transactionalValidations, pending.ID) {
		bound.transactionalValidations = append(bound.transactionalValidations, pending.ID)
	}
	bound.metadataOnly = nil
	bound.indexAddition = addition
	bound.transactionalCreates = append(bound.transactionalCreates, addition)
	bound.transactionalIndexCreates++
	bound.definition = schema.JobDefinition{
		Operation: schema.SchemaCreateIndexTransactional, SourceTable: source, TargetTable: target,
		SourceIndexes: sourceIndexes, TargetIndexes: append(targetIndexes, addition),
	}
	if err := attachTransactionalRelationTypes(ctx, bound, target, snapshot, options); err != nil {
		return nil, err
	}
	return bound, ctx.Err()
}

func pruneUnusedTableTypeDependencies(ctx context.Context, spec *model.DescriptorSpec, snapshot CatalogSnapshot) error {
	if spec == nil || spec.Table == nil {
		return model.ErrInvalidDescriptor
	}
	usedTypes := make(map[model.OID]bool)
	var visitLayout func(model.TypeLayout)
	visitLayout = func(layout model.TypeLayout) {
		if layout.Type.ID != 0 {
			usedTypes[model.OID(layout.Type.ID)] = true
		}
		for _, child := range layout.Children {
			visitLayout(child)
		}
	}
	usedObjects := map[model.ObjectID]bool{spec.Table.RowType: true, spec.Table.TypedOf: spec.Table.TypedOf != 0}
	visitExpression := func(definition *model.ExpressionDefinition) {
		if definition == nil {
			return
		}
		for _, dependency := range definition.Dependencies {
			usedObjects[dependency.Object] = true
		}
	}
	for _, column := range spec.Table.Columns {
		visitLayout(column.Layout)
		visitExpression(column.Default)
		visitExpression(column.Generated)
	}
	for _, key := range spec.Table.PrimaryKey {
		visitLayout(model.TypeLayout{Type: key.Type, Children: key.Children})
		visitExpression(key.Expression)
	}
	for _, constraint := range spec.Table.Constraints {
		visitExpression(constraint.Expression)
		if constraint.ReferencedObject != 0 {
			usedObjects[constraint.ReferencedObject] = true
		}
	}
	for _, trigger := range spec.Table.Triggers {
		visitExpression(trigger.When)
	}
	visitExpression(spec.Table.Partition)
	filtered := spec.Dependencies[:0]
	for _, dependency := range spec.Dependencies {
		published, err := snapshot.Descriptor(ctx, dependency.Object)
		if err != nil {
			return err
		}
		descriptor := published.Descriptor
		if descriptor.Kind() == model.ObjectType && !usedTypes[descriptor.OID()] && !usedObjects[descriptor.ID()] {
			continue
		}
		filtered = append(filtered, dependency)
	}
	spec.Dependencies = filtered
	return nil
}

func attachTransactionalRelationTypes(ctx context.Context, bound *BoundSchemaDDL, target *model.Descriptor, snapshot CatalogSnapshot, options CreateDeclarationOptions) error {
	updates, err := updateImplicitRelationRowTypes(ctx, snapshot, target, options.Descriptors)
	if err != nil {
		return err
	}
	for _, update := range updates {
		bound.dependencies = appendSchemaDependency(bound.dependencies, update.Base, 0)
	}
	bound.auxiliaryUpdates = updates
	return nil
}

func addPersistentSchemaDependencies(ctx context.Context, target *model.Descriptor, dependencies []model.Dependency, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*model.Descriptor, error) {
	spec, err := target.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	for _, dependency := range dependencies {
		if dependency.Object == spec.ID {
			continue
		}
		published, resolveErr := snapshot.Descriptor(ctx, dependency.Object)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if published.Descriptor.Kind() != model.ObjectIndex {
			spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
		}
	}
	slices.SortFunc(spec.Dependencies, compareDependency)
	spec.Dependencies = slices.Compact(spec.Dependencies)
	return model.NewDescriptor(ctx, spec, options.Descriptors)
}

func appendSchemaDependency(dependencies []model.Dependency, descriptor *model.Descriptor, column model.ColumnID) []model.Dependency {
	for _, dependency := range dependencies {
		if dependency.Object == descriptor.ID() && dependency.Column == column {
			return dependencies
		}
	}
	return append(dependencies, model.Dependency{Object: descriptor.ID(), Version: descriptor.Version(), Column: column})
}
func (d *BoundSchemaDDL) TransactionalSchemaCommand() (*BoundObjectCommand, error) {
	if d == nil || d.definition.SourceTable == nil || d.definition.TargetTable == nil || len(d.definition.TargetIndexes) != len(d.definition.SourceIndexes)+d.transactionalIndexCreates {
		return nil, model.ErrInvalidDescriptor
	}
	switch d.definition.Operation {
	case schema.SchemaAddColumn, schema.SchemaDropColumn, schema.SchemaAlterColumnType, schema.SchemaRewritePrimaryKey, schema.SchemaCreateIndexTransactional:
	default:
		return nil, model.ErrInvalidDescriptor
	}
	updates := make([]ObjectMutation, 0, len(d.definition.SourceIndexes)+1)
	updates = append(updates, ObjectMutation{Base: d.definition.SourceTable, Next: d.definition.TargetTable})
	for i := range d.definition.SourceIndexes {
		updates = append(updates, ObjectMutation{Base: d.definition.SourceIndexes[i], Next: d.definition.TargetIndexes[i]})
	}
	updates = append(updates, d.auxiliaryUpdates...)
	return &BoundObjectCommand{
		creates: slices.Clone(d.transactionalCreates), updates: updates, drops: slices.Clone(d.indexDrops),
		validations: slices.Clone(d.transactionalValidations), dependencies: slices.Clone(d.dependencies),
		warnings: slices.Clone(d.warnings), cascade: d.cascade, tag: "ALTER TABLE",
	}, nil
}
func (d *BoundSchemaDDL) TransactionalMetadataCommand() (*BoundObjectCommand, error) {
	if d == nil || d.source == nil || d.metadataOnly == nil || d.source.ID() != d.metadataOnly.ID() {
		return nil, model.ErrInvalidDescriptor
	}
	updates := make([]ObjectMutation, 0, len(d.auxiliaryUpdates)+1)
	updates = append(updates, ObjectMutation{Base: d.source, Next: d.metadataOnly})
	updates = append(updates, d.auxiliaryUpdates...)
	return &BoundObjectCommand{
		creates: slices.Clone(d.transactionalCreates), updates: updates, drops: slices.Clone(d.indexDrops),
		validations: slices.Clone(d.transactionalValidations), dependencies: slices.Clone(d.dependencies),
		warnings: slices.Clone(d.warnings), cascade: d.cascade, tag: "ALTER TABLE",
	}, nil
}

func bindOnlineAddColumn(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, command *zpgparse.AlterTableCmd, transactional bool) (*BoundSchemaDDL, error) {
	column := command.Def.GetColumnDef()
	if column == nil || command.Name != "" || command.Recurse || command.Behavior == zpgparse.DropBehavior_DROP_CASCADE {
		return nil, bindError("42601", "invalid ADD COLUMN declaration", 0)
	}
	if err := ordinaryColumn(column); err != nil {
		return nil, err
	}
	if err := declarationName(column.Colname); err != nil {
		return nil, err
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	for _, existing := range spec.Table.Columns {
		if existing.Name == column.Colname {
			if command.MissingOk {
				return &BoundSchemaDDL{source: table, noOp: true, dependencies: b.dependencies, warnings: append(b.warnings, model.Error{Code: "42701", Message: "column already exists, skipping"})}, nil
			}
			return nil, bindError("42701", "column already exists", int(column.Location)+1)
		}
	}
	if spec.Table.ColumnIDHighWatermark == model.ColumnID(math.MaxUint32) || len(spec.Table.Columns) >= options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	layout, serial, err := b.declarationColumnType(column.TypeName)
	if err != nil {
		return nil, err
	}
	if column.CollClause != nil {
		layout, err = b.indexColumnCollation(layout, column.CollClause.Collname)
		if err != nil {
			return nil, err
		}
	}
	declaration := model.ColumnDescriptor{ID: spec.Table.ColumnIDHighWatermark + 1, Name: column.Colname, State: model.StatePublic, Layout: layout, Nullable: true, Local: true}
	var defaultNode *zpgparse.Node
	var relationalConstraints []*zpgparse.Constraint
	var generated *zpgparse.Constraint
	var identity *zpgparse.Constraint
	explicitNull := false
	for _, node := range column.Constraints {
		constraint := node.GetConstraint()
		if constraint == nil {
			return nil, bindError("42601", "invalid ADD COLUMN constraint", int(column.Location)+1)
		}
		if err := ordinaryConstraint(constraint); err != nil {
			return nil, err
		}
		switch constraint.Contype {
		case zpgparse.ConstrType_CONSTR_NULL:
			declaration.Nullable = true
			explicitNull = true
		case zpgparse.ConstrType_CONSTR_NOTNULL:
			declaration.Nullable = false
		case zpgparse.ConstrType_CONSTR_DEFAULT:
			if defaultNode != nil || constraint.RawExpr == nil {
				return nil, bindError("42601", "column has more than one default", int(constraint.Location)+1)
			}
			defaultNode = constraint.RawExpr
		case zpgparse.ConstrType_CONSTR_GENERATED:
			if generated != nil || constraint.RawExpr == nil {
				return nil, bindError("42601", "column has more than one generation expression", int(constraint.Location)+1)
			}
			generated = constraint
		case zpgparse.ConstrType_CONSTR_IDENTITY:
			if identity != nil {
				return nil, bindError("42601", "column has more than one identity declaration", int(constraint.Location)+1)
			}
			identity = constraint
		default:
			relationalConstraints = append(relationalConstraints, constraint)
		}
	}
	if explicitNull && slices.ContainsFunc(relationalConstraints, func(constraint *zpgparse.Constraint) bool {
		return constraint != nil && constraint.Contype == zpgparse.ConstrType_CONSTR_PRIMARY
	}) {
		return nil, bindError("42601", "conflicting NULL and PRIMARY KEY declarations", int(column.Location)+1)
	}
	if serial || identity != nil {
		if !transactional {
			return nil, missing("ONLINE ADD COLUMN owned sequence", int(column.Location)+1)
		}
		if explicitNull || defaultNode != nil || generated != nil || serial && identity != nil {
			return nil, bindError("42601", "multiple or conflicting default or generation declarations", int(column.Location)+1)
		}
		return bindOwnedSequenceAddColumn(ctx, b, snapshot, options, table, column, spec, declaration, serial, identity, relationalConstraints)
	}
	if defaultNode != nil && generated != nil {
		return nil, bindError("42601", "multiple default or generation declarations", int(column.Location)+1)
	}
	if generated != nil && !transactional {
		return nil, missing("ONLINE ADD COLUMN generated expression", int(column.Location)+1)
	}
	if len(relationalConstraints) != 0 && !transactional {
		return nil, missing("ONLINE ADD COLUMN inline relational constraints", int(column.Location)+1)
	}
	metadataOnly := defaultNode == nil && declaration.Nullable
	if defaultNode != nil {
		bound, err := defineDefault(ctx, defaultNode, layout, snapshot, options.Bind, true)
		if err != nil {
			return nil, err
		}
		b.warnings = append(b.warnings, bound.Warnings...)
		definition := bound.Definition
		declaration.Default = &definition
		if bound.Constant != nil {
			value := bound.Constant.Clone()
			declaration.ConstantDefault = &value
			metadataOnly = true
		}
	}
	if generated != nil {
		stored := generated.GeneratedKind == "s"
		if !stored && generated.GeneratedKind != "v" {
			return nil, bindError("42601", "invalid generated column kind", int(generated.Location)+1)
		}
		pendingSpec := spec
		pendingTable := *spec.Table
		pendingSpec.Table = &pendingTable
		pendingSpec.Table.Columns = append(slices.Clone(spec.Table.Columns), declaration)
		pendingSpec.Table.ColumnIDHighWatermark = declaration.ID
		pendingSpec.Version = table.Version() + 3
		pending, pendingErr := model.NewDescriptor(ctx, pendingSpec, options.Descriptors)
		if pendingErr != nil {
			return nil, pendingErr
		}
		definition, _, definitionErr := defineRowExpression(ctx, generated.RawExpr, pending, pendingSpec.Table.Columns, snapshot, options.Bind, rowStoredGenerated, declaration.ID, &declaration.Layout, 0)
		if definitionErr != nil {
			return nil, definitionErr
		}
		declaration.Generated, declaration.GeneratedStored = definition, stored
		for _, dependency := range definition.Dependencies {
			b.dependencies = appendIndexDependency(b.dependencies, dependency)
		}
		metadataOnly = !stored && declaration.Nullable
	}
	if declaration.Default != nil || declaration.Generated != nil {
		_, defaultOID, err := options.Identities.AllocateDeclarationIdentity(ctx)
		if err != nil {
			return nil, err
		}
		declaration.DefaultOID = defaultOID
	}
	if !declaration.Nullable {
		if declaration.ConstantDefault != nil && declaration.ConstantDefault.IsNull {
			return nil, bindError("23502", "column default is null", int(column.Location)+1)
		}
		if len(spec.Table.Constraints) >= options.Descriptors.MaxItems {
			return nil, model.ErrDescriptorLimit
		}
		state := declarationState{
			b: b, options: options, parent: table.Name().Parent, tableName: table.Name().Name,
			names: make(map[string]bool), reserved: make(map[string]bool),
			ids: map[model.ObjectID]bool{table.ID(): true}, oids: map[model.OID]bool{table.OID(): true},
		}
		for _, constraint := range spec.Table.Constraints {
			state.reserved[constraint.Name] = constraint.Name != ""
			state.ids[constraint.ID], state.oids[constraint.OID] = true, true
		}
		name, err := state.constraintName("", declaration.Name, "not_null", false)
		if err != nil {
			return nil, err
		}
		id, oid, err := state.allocate()
		if err != nil {
			return nil, err
		}
		spec.Table.Constraints = append(spec.Table.Constraints, model.ConstraintDescriptor{
			ID: id, OID: oid, Name: name, Kind: model.ConstraintNotNull, Columns: []model.ColumnID{declaration.ID},
			Validated: true, Enforced: true, Local: true,
		})
	}
	spec.Table.Columns = append(spec.Table.Columns, declaration)
	spec.Table.ColumnIDHighWatermark = declaration.ID
	spec.Version = table.Version() + 3
	creates, validations, err := bindAddedColumnConstraints(ctx, b, snapshot, options, table, &spec, declaration.ID, relationalConstraints)
	if err != nil {
		return nil, err
	}
	if len(creates) != 0 {
		metadataOnly = false
	}
	if metadataOnly {
		spec.Version = table.Version() + 1
	}
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	result := &BoundSchemaDDL{
		source: table, mutation: model.ObjectID(declaration.ID), dependencies: b.dependencies, warnings: b.warnings,
		transactionalCreates: creates, transactionalIndexCreates: len(creates), transactionalValidations: validations,
	}
	if metadataOnly {
		result.metadataOnly = target
		return result, nil
	}
	result.definition = schema.JobDefinition{Operation: schema.SchemaAddColumn, SourceTable: table, TargetTable: target, TargetIndexes: slices.Clone(creates)}
	return result, nil
}

func bindOnlineDropColumn(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, command *zpgparse.AlterTableCmd) (*BoundSchemaDDL, error) {
	if command.Name == "" || command.Def != nil || command.Recurse {
		return nil, missing("online DROP COLUMN transformed variants", 0)
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	position := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool { return column.Name == command.Name && !column.SystemIdentity })
	if position < 0 {
		if command.MissingOk {
			return &BoundSchemaDDL{source: table, noOp: true, dependencies: b.dependencies, warnings: append(b.warnings, model.Error{Code: "42703", Message: "column does not exist, skipping"})}, nil
		}
		return nil, bindError("42703", "column does not exist", 0)
	}
	column := spec.Table.Columns[position]
	if !column.Local || slices.ContainsFunc(spec.Table.TypedColumns, func(mapping model.TypedColumnMapping) bool { return mapping.Column == column.ID }) {
		return nil, bindError("42809", "cannot drop an inherited or typed-table column", 0)
	}
	cascade := command.Behavior == zpgparse.DropBehavior_DROP_CASCADE
	spec.Table.Constraints = slices.DeleteFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
		ownNotNull := constraint.Kind == model.ConstraintNotNull && len(constraint.Columns) == 1 && constraint.Columns[0] == column.ID
		if ownNotNull {
			return true
		}
		return cascade && (slices.Contains(constraint.Columns, column.ID) ||
			constraint.ReferencedObject == spec.ID && slices.Contains(constraint.ReferencedColumns, column.ID) ||
			expressionReferencesColumn(constraint.Expression, column.ID))
	})
	if tableMetadataReferencesColumn(spec.ID, spec.Table, column.ID) {
		return nil, bindError("2BP01", "cannot drop column because another object depends on it", 0)
	}
	indexes, err := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
	if err != nil {
		return nil, err
	}
	retained := make([]*model.Descriptor, 0, len(indexes))
	var indexDrops []*model.Descriptor
	for _, index := range indexes {
		indexSpec, err := index.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if indexMetadataReferencesColumn(indexSpec.Index, column.ID) {
			if !cascade {
				return nil, bindError("2BP01", "cannot drop column because an index depends on it", 0)
			}
			indexDrops = append(indexDrops, index)
			spec.Table.Indexes = slices.DeleteFunc(spec.Table.Indexes, func(id model.ObjectID) bool { return id == index.ID() })
			spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool { return dependency.Object == index.ID() })
			continue
		}
		retained = append(retained, index)
	}
	indexes = retained
	spec.Table.Columns = slices.Delete(spec.Table.Columns, position, position+1)
	spec.Version = table.Version() + 3
	spec.Physical.Generation++
	target, err := rebindTargetTableExpressions(ctx, table, &spec, snapshot, options)
	if err != nil {
		return nil, err
	}
	targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, indexes, target, 0)
	if err != nil {
		return nil, err
	}
	return &BoundSchemaDDL{source: table, definition: schema.JobDefinition{Operation: schema.SchemaDropColumn, SourceTable: table, TargetTable: target, SourceIndexes: indexes, TargetIndexes: targetIndexes}, mutation: model.ObjectID(column.ID), indexDrops: indexDrops, dependencies: b.dependencies, warnings: b.warnings, cascade: cascade}, nil
}

func bindOnlineColumnType(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, command *zpgparse.AlterTableCmd) (*BoundSchemaDDL, error) {
	columnDef := command.Def.GetColumnDef()
	if command.Name == "" || columnDef == nil || columnDef.TypeName == nil || command.Recurse || command.MissingOk {
		return nil, bindError("42601", "invalid ALTER COLUMN TYPE declaration", 0)
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	position := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool { return column.Name == command.Name && !column.SystemIdentity })
	if position < 0 {
		return nil, bindError("42703", "column does not exist", 0)
	}
	layout, serial, err := b.declarationColumnType(columnDef.TypeName)
	if err != nil {
		return nil, err
	}
	if serial {
		return nil, missing("serial online type rewrite", int(columnDef.Location)+1)
	}
	if columnDef.CollClause != nil {
		layout, err = b.indexColumnCollation(layout, columnDef.CollClause.Collname)
		if err != nil {
			return nil, err
		}
	}
	column := &spec.Table.Columns[position]
	if !column.Local || slices.ContainsFunc(spec.Table.TypedColumns, func(mapping model.TypedColumnMapping) bool { return mapping.Column == column.ID }) {
		return nil, bindError("42809", "cannot alter the type of an inherited or typed-table column", 0)
	}
	if column.Default != nil {
		definition, err := redefineColumnDefault(ctx, column.Default, column.Layout, layout, snapshot, options.Bind)
		if err != nil {
			return nil, err
		}
		column.Default = definition
	}
	if column.ConstantDefault != nil {
		definition, err := redefineConstantDefault(ctx, *column.ConstantDefault, column.Layout, layout, snapshot, options.Bind)
		if err != nil {
			return nil, err
		}
		column.Default = definition
		column.ConstantDefault = nil
	}
	if column.IdentitySequence != 0 {
		integer := layout.Type.Storage == model.TypeInt64 && (layout.Type.ID == 20 || layout.Type.ID == 21 || layout.Type.ID == 23)
		if !integer {
			return nil, bindError("22023", "identity column requires an integer type", int(columnDef.Location)+1)
		}
	}
	column = &spec.Table.Columns[position]
	ast := columnDef.RawDefault
	if ast == nil {
		ast = &zpgparse.Node{Node: &zpgparse.Node_TypeCast{TypeCast: &zpgparse.TypeCast{Arg: columnReference(column.Name), TypeName: proto.Clone(columnDef.TypeName).(*zpgparse.TypeName), Location: columnDef.Location}}}
	}
	definition, _, err := DefineGenerationTransform(ctx, ast, table, spec.Table.Columns, column.ID, layout, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	column.Layout = layout
	for i := range spec.Table.PrimaryKey {
		if spec.Table.PrimaryKey[i].Column == column.ID {
			spec.Table.PrimaryKey[i].Type, spec.Table.PrimaryKey[i].Children = layout.Type, slices.Clone(layout.Children)
		}
	}
	indexes, err := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
	if err != nil {
		return nil, err
	}
	spec.Version = table.Version() + 3
	spec.Physical.Generation++
	target, err := rebindTargetTableExpressions(ctx, table, &spec, snapshot, options)
	if err != nil {
		return nil, err
	}
	targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, indexes, target, column.ID)
	if err != nil {
		return nil, err
	}
	return &BoundSchemaDDL{source: table, definition: schema.JobDefinition{Operation: schema.SchemaAlterColumnType, SourceTable: table, TargetTable: target, SourceIndexes: indexes, TargetIndexes: targetIndexes, Transforms: []schema.GenerationTransform{{Column: column.ID, Expression: definition}}}, mutation: model.ObjectID(column.ID), dependencies: b.dependencies, warnings: b.warnings}, nil
}

func BindCompositeTypedTableCascade(ctx context.Context, table, before, after *model.Descriptor, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSchemaDDL, error) {
	if table == nil || before == nil || after == nil || snapshot == nil || table.Kind() != model.ObjectTable || before.Kind() != model.ObjectType || after.Kind() != model.ObjectType || before.ID() != after.ID() {
		return nil, model.ErrInvalidDescriptor
	}
	tableSpec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	beforeSpec, err := before.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	afterSpec, err := after.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if tableSpec.Table == nil || tableSpec.Table.TypedOf != before.ID() || beforeSpec.Type == nil || afterSpec.Type == nil || beforeSpec.Type.DeclaredKind() != model.TypeKindComposite || afterSpec.Type.DeclaredKind() != model.TypeKindComposite {
		return nil, model.ErrInvalidDescriptor
	}
	b, err := newExpressionBinder(ctx, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	b.dependency(table, 0)
	beforeFields := make(map[model.ColumnID]model.ColumnDescriptor, len(beforeSpec.Type.Fields))
	afterFields := make(map[model.ColumnID]model.ColumnDescriptor, len(afterSpec.Type.Fields))
	for _, field := range beforeSpec.Type.Fields {
		beforeFields[field.ID] = field
	}
	for _, field := range afterSpec.Type.Fields {
		afterFields[field.ID] = field
	}
	columns := make(map[model.ColumnID]model.ColumnDescriptor, len(tableSpec.Table.Columns))
	for _, column := range tableSpec.Table.Columns {
		columns[column.ID] = column
	}
	mapped := make(map[model.ColumnID]model.ColumnID, len(tableSpec.Table.TypedColumns))
	changedColumns := make(map[model.ColumnID]model.TypeLayout)
	droppedColumns := make(map[model.ColumnID]bool)
	rewrite := false
	for _, mapping := range tableSpec.Table.TypedColumns {
		field, existed := beforeFields[mapping.Field]
		nextField, remains := afterFields[mapping.Field]
		column, found := columns[mapping.Column]
		if !existed || !found || column.SystemIdentity || field.Name != column.Name || !sameType(field.Layout, column.Layout) {
			return nil, model.ErrInvalidDescriptor
		}
		if !remains {
			droppedColumns[column.ID] = true
			delete(columns, column.ID)
			continue
		}
		mapped[mapping.Field] = mapping.Column
		column.Name = nextField.Name
		if !sameType(column.Layout, nextField.Layout) {
			column.Layout = nextField.Layout
			columns[column.ID] = column
			changedColumns[column.ID] = nextField.Layout
			rewrite = true
			continue
		}
		columns[column.ID] = column
	}
	nextMappings := make([]model.TypedColumnMapping, 0, len(afterSpec.Type.Fields))
	for _, field := range afterSpec.Type.Fields {
		columnID, exists := mapped[field.ID]
		if !exists {
			if tableSpec.Table.ColumnIDHighWatermark == model.ColumnID(math.MaxUint32) {
				return nil, model.ErrDescriptorLimit
			}
			tableSpec.Table.ColumnIDHighWatermark++
			columnID = tableSpec.Table.ColumnIDHighWatermark
			columns[columnID] = model.ColumnDescriptor{ID: columnID, Name: field.Name, State: model.StatePublic, Layout: field.Layout, Nullable: true}
		}
		nextMappings = append(nextMappings, model.TypedColumnMapping{Field: field.ID, Column: columnID})
	}
	tableSpec.Table.Columns = tableSpec.Table.Columns[:0]
	for _, column := range columns {
		tableSpec.Table.Columns = append(tableSpec.Table.Columns, column)
	}
	slices.SortFunc(tableSpec.Table.Columns, func(left, right model.ColumnDescriptor) int {
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	tableSpec.Table.TypedColumns = nextMappings
	tableSpec.Dependencies = replaceTypeDependency(tableSpec.Dependencies, after.ID(), after.Version())
	return finishCompositeTableCascade(ctx, b, snapshot, options, table, tableSpec, changedColumns, droppedColumns, rewrite)
}

func finishCompositeTableCascade(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, tableSpec model.DescriptorSpec, changedColumns map[model.ColumnID]model.TypeLayout, droppedColumns map[model.ColumnID]bool, rewrite bool) (*BoundSchemaDDL, error) {
	indexes, indexDrops, keyRewrite, err := prepareTypedColumnDrops(ctx, b, snapshot, options, &tableSpec, droppedColumns)
	if err != nil {
		return nil, err
	}
	rewrite = rewrite || keyRewrite
	if table.Version() == math.MaxUint64 {
		return nil, model.ErrDescriptorLimit
	}
	tableSpec.Version = table.Version() + 1
	if !rewrite {
		target, err := model.NewDescriptor(ctx, tableSpec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		return &BoundSchemaDDL{source: table, metadataOnly: target, indexDrops: indexDrops, dependencies: b.dependencies, warnings: b.warnings}, nil
	}
	if tableSpec.Physical.Generation == math.MaxUint64 {
		return nil, model.ErrDescriptorLimit
	}
	sourceSpec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	sourceColumns := slices.Clone(sourceSpec.Table.Columns)
	transforms := make([]schema.GenerationTransform, 0, len(changedColumns))
	for columnID, targetLayout := range changedColumns {
		source := tableColumn(sourceColumns, columnID)
		if source.ID == 0 {
			return nil, model.ErrInvalidDescriptor
		}
		definition, _, err := DefineGenerationTransform(ctx, columnReference(source.Name), table, sourceColumns, columnID, targetLayout, snapshot, options.Bind)
		if err != nil {
			return nil, err
		}
		transforms = append(transforms, schema.GenerationTransform{Column: columnID, Expression: definition})
	}
	slices.SortFunc(transforms, func(left, right schema.GenerationTransform) int {
		switch {
		case left.Column < right.Column:
			return -1
		case left.Column > right.Column:
			return 1
		default:
			return 0
		}
	})
	for i := range tableSpec.Table.PrimaryKey {
		if layout, changed := changedColumns[tableSpec.Table.PrimaryKey[i].Column]; changed {
			tableSpec.Table.PrimaryKey[i].Type = layout.Type
			tableSpec.Table.PrimaryKey[i].Children = slices.Clone(layout.Children)
		}
	}
	tableSpec.Physical.Generation++
	target, err := rebindTargetTableExpressions(ctx, table, &tableSpec, snapshot, options)
	if err != nil {
		return nil, err
	}
	if len(droppedColumns) == 0 {
		indexes, err = onlineSourceIndexes(ctx, b, snapshot, options, sourceSpec.Table.Indexes)
		if err != nil {
			return nil, err
		}
	}
	targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, indexes, target, 0)
	if err != nil {
		return nil, err
	}
	mutation := model.ObjectID(1)
	if len(transforms) != 0 {
		mutation = model.ObjectID(transforms[0].Column)
	}
	return &BoundSchemaDDL{
		source:       table,
		mutation:     mutation,
		definition:   schema.JobDefinition{Operation: schema.SchemaAlterColumnType, SourceTable: table, TargetTable: target, SourceIndexes: indexes, TargetIndexes: targetIndexes, Transforms: transforms},
		indexDrops:   indexDrops,
		dependencies: b.dependencies,
		warnings:     b.warnings,
	}, nil
}

func prepareTypedColumnDrops(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, spec *model.DescriptorSpec, dropped map[model.ColumnID]bool) ([]*model.Descriptor, []*model.Descriptor, bool, error) {
	if len(dropped) == 0 {
		return nil, nil, false, nil
	}
	for changed := true; changed; {
		changed = false
		for _, column := range spec.Table.Columns {
			if dropped[column.ID] || !expressionReferencesAnyColumn(column.Default, dropped) && !expressionReferencesAnyColumn(column.Generated, dropped) {
				continue
			}
			if slices.ContainsFunc(spec.Table.TypedColumns, func(mapping model.TypedColumnMapping) bool { return mapping.Column == column.ID }) {
				return nil, nil, false, bindError("2BP01", "cannot drop a typed column required by another typed attribute", 0)
			}
			dropped[column.ID] = true
			changed = true
		}
	}
	if expressionReferencesAnyColumn(spec.Table.Partition, dropped) || slices.ContainsFunc(spec.Table.PartitionKeys, func(key model.KeyElement) bool {
		return dropped[key.Column] || expressionReferencesAnyColumn(key.Expression, dropped)
	}) {
		return nil, nil, false, bindError("2BP01", "cannot drop a partition key column", 0)
	}
	keyRewrite := slices.ContainsFunc(spec.Table.PrimaryKey, func(key model.KeyElement) bool {
		return dropped[key.Column] || expressionReferencesAnyColumn(key.Expression, dropped)
	})
	if keyRewrite {
		spec.Table.PrimaryKey = nil
	}
	spec.Table.Columns = slices.DeleteFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool { return dropped[column.ID] })
	droppedConstraints := make(map[model.ObjectID]bool)
	spec.Table.Constraints = slices.DeleteFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
		referenced := constraint.ReferencedObject == spec.ID && slices.ContainsFunc(constraint.ReferencedColumns, func(column model.ColumnID) bool { return dropped[column] })
		dependent := slices.ContainsFunc(constraint.Columns, func(column model.ColumnID) bool { return dropped[column] }) || referenced || expressionReferencesAnyColumn(constraint.Expression, dropped)
		if dependent {
			droppedConstraints[constraint.ID] = true
		}
		return dependent
	})
	spec.Table.Triggers = slices.DeleteFunc(spec.Table.Triggers, func(trigger model.TriggerDescriptor) bool {
		return droppedConstraints[trigger.ConstraintID] || slices.ContainsFunc(trigger.Columns, func(column model.ColumnID) bool { return dropped[column] }) || expressionReferencesAnyColumn(trigger.When, dropped)
	})
	spec.Table.ExtendedStatistics = slices.DeleteFunc(spec.Table.ExtendedStatistics, func(statistic model.ExtendedStatisticDescriptor) bool {
		if slices.ContainsFunc(statistic.Keys, func(column model.ColumnID) bool { return dropped[column] }) {
			return true
		}
		return slices.ContainsFunc(statistic.Expressions, func(expression *model.ExpressionDefinition) bool {
			return expressionReferencesAnyColumn(expression, dropped)
		})
	})
	spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
		return dependency.Object == spec.ID && dropped[dependency.Column]
	})
	indexes, err := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
	if err != nil {
		return nil, nil, false, err
	}
	kept := make([]*model.Descriptor, 0, len(indexes))
	drops := make([]*model.Descriptor, 0)
	spec.Table.Indexes = spec.Table.Indexes[:0]
	for _, index := range indexes {
		indexSpec, err := index.SpecWithin(ctx, options.Descriptors)
		if err != nil || indexSpec.Index == nil {
			return nil, nil, false, errors.Join(model.ErrInvalidDescriptor, err)
		}
		dependent := slices.ContainsFunc(indexSpec.Index.Include, func(column model.ColumnID) bool { return dropped[column] }) ||
			expressionReferencesAnyColumn(indexSpec.Index.Predicate, dropped) ||
			slices.ContainsFunc(indexSpec.Index.Keys, func(key model.KeyElement) bool {
				return dropped[key.Column] || expressionReferencesAnyColumn(key.Expression, dropped)
			})
		if dependent {
			drops = append(drops, index)
			continue
		}
		kept = append(kept, index)
		spec.Table.Indexes = append(spec.Table.Indexes, index.ID())
	}
	droppedIndexes := make(map[model.ObjectID]bool, len(drops))
	for _, index := range drops {
		droppedIndexes[index.ID()] = true
	}
	spec.Table.Triggers = slices.DeleteFunc(spec.Table.Triggers, func(trigger model.TriggerDescriptor) bool {
		return droppedIndexes[trigger.ConstraintIndex]
	})
	return kept, drops, keyRewrite, nil
}

func expressionReferencesAnyColumn(definition *model.ExpressionDefinition, columns map[model.ColumnID]bool) bool {
	if definition == nil {
		return false
	}
	return slices.ContainsFunc(definition.Dependencies, func(dependency model.Dependency) bool {
		return columns[dependency.Column]
	})
}

func BindInheritedTableCascade(ctx context.Context, table *model.Descriptor, parentsBefore, parentsAfter []*model.Descriptor, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSchemaDDL, error) {
	if table == nil || table.Kind() != model.ObjectTable || snapshot == nil || len(parentsBefore) == 0 || len(parentsBefore) != len(parentsAfter) {
		return nil, model.ErrInvalidDescriptor
	}
	tableSpec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if tableSpec.Table == nil || tableSpec.Table.TypedOf != 0 {
		return nil, model.ErrInvalidDescriptor
	}
	b, err := newExpressionBinder(ctx, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	b.dependency(table, 0)
	columns := make(map[model.ColumnID]model.ColumnDescriptor, len(tableSpec.Table.Columns))
	original := make(map[model.ColumnID]model.ColumnDescriptor, len(tableSpec.Table.Columns))
	for _, column := range tableSpec.Table.Columns {
		columns[column.ID] = column
		original[column.ID] = column
	}
	proposed := make(map[model.ColumnID]model.ColumnDescriptor)
	affectedParents := make(map[model.ObjectID]bool, len(parentsBefore))
	for position := range parentsBefore {
		before, after := parentsBefore[position], parentsAfter[position]
		if before == nil || after == nil || before.Kind() != model.ObjectTable || after.Kind() != model.ObjectTable || before.ID() != after.ID() {
			return nil, model.ErrInvalidDescriptor
		}
		if !slices.Contains(tableSpec.Table.Parents, before.ID()) {
			return nil, model.ErrInvalidDescriptor
		}
		affectedParents[before.ID()] = true
		beforeSpec, err := before.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		afterSpec, err := after.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if beforeSpec.Table == nil || afterSpec.Table == nil {
			return nil, model.ErrInvalidDescriptor
		}
		beforeColumns := make(map[model.ColumnID]model.ColumnDescriptor, len(beforeSpec.Table.Columns))
		afterColumns := make(map[model.ColumnID]model.ColumnDescriptor, len(afterSpec.Table.Columns))
		for _, column := range beforeSpec.Table.Columns {
			beforeColumns[column.ID] = column
		}
		for _, column := range afterSpec.Table.Columns {
			afterColumns[column.ID] = column
		}
		for id, child := range original {
			for _, inherited := range child.InheritedFrom {
				if inherited.Table != before.ID() {
					continue
				}
				parent, exists := beforeColumns[inherited.Column]
				if !exists || parent.Name != child.Name || !sameType(parent.Layout, child.Layout) {
					return nil, model.ErrInvalidDescriptor
				}
				next := columns[id]
				nextParent, remains := afterColumns[inherited.Column]
				if !remains {
					next.InheritedFrom = slices.DeleteFunc(next.InheritedFrom, func(source model.ColumnInheritance) bool {
						return source == inherited
					})
					columns[id] = next
					continue
				}
				if proposal, exists := proposed[id]; exists && (proposal.Name != nextParent.Name || !sameType(proposal.Layout, nextParent.Layout)) {
					return nil, bindError("42804", "inherited column changes conflict", 0)
				}
				next.Name = nextParent.Name
				next.Layout = nextParent.Layout
				proposed[id] = next
			}
		}
		for id, parent := range afterColumns {
			if _, existed := beforeColumns[id]; existed || parent.SystemIdentity {
				continue
			}
			var childID model.ColumnID
			for id, child := range columns {
				if child.Name == parent.Name {
					childID = id
					break
				}
			}
			if childID == 0 {
				if tableSpec.Table.ColumnIDHighWatermark == model.ColumnID(math.MaxUint32) {
					return nil, model.ErrDescriptorLimit
				}
				tableSpec.Table.ColumnIDHighWatermark++
				childID = tableSpec.Table.ColumnIDHighWatermark
				columns[childID] = model.ColumnDescriptor{ID: childID, Name: parent.Name, State: model.StatePublic, Layout: parent.Layout, Nullable: parent.Nullable}
			}
			child := columns[childID]
			if !sameType(child.Layout, parent.Layout) {
				return nil, bindError("42804", "inherited column has incompatible type", 0)
			}
			source := model.ColumnInheritance{Table: before.ID(), Column: parent.ID}
			if !slices.Contains(child.InheritedFrom, source) {
				child.InheritedFrom = append(child.InheritedFrom, source)
			}
			columns[childID] = child
		}
		tableSpec.Dependencies = replaceTypeDependency(tableSpec.Dependencies, after.ID(), after.Version())
	}
	for id, proposal := range proposed {
		for _, inherited := range original[id].InheritedFrom {
			if affectedParents[inherited.Table] {
				continue
			}
			published, err := snapshot.Descriptor(ctx, inherited.Table)
			if err != nil {
				return nil, err
			}
			parentSpec, err := published.Descriptor.SpecWithin(ctx, options.Descriptors)
			if err != nil || parentSpec.Table == nil {
				return nil, errors.Join(model.ErrInvalidDescriptor, err)
			}
			parent := tableColumn(parentSpec.Table.Columns, inherited.Column)
			if parent.ID == 0 || parent.Name != proposal.Name || !sameType(parent.Layout, proposal.Layout) {
				return nil, bindError("42804", "inherited column changes conflict", 0)
			}
		}
	}
	changedColumns := make(map[model.ColumnID]model.TypeLayout)
	rewrite := false
	for id, proposal := range proposed {
		current := columns[id]
		if !sameType(current.Layout, proposal.Layout) {
			changedColumns[id] = proposal.Layout
			rewrite = true
		}
		current.Name = proposal.Name
		current.Layout = proposal.Layout
		columns[id] = current
	}
	droppedColumns := make(map[model.ColumnID]bool)
	for id, column := range columns {
		if len(column.InheritedFrom) == 0 && !column.Local {
			droppedColumns[id] = true
			delete(columns, id)
		}
	}
	tableSpec.Table.Columns = tableSpec.Table.Columns[:0]
	for _, column := range columns {
		tableSpec.Table.Columns = append(tableSpec.Table.Columns, column)
	}
	slices.SortFunc(tableSpec.Table.Columns, func(left, right model.ColumnDescriptor) int {
		switch {
		case left.ID < right.ID:
			return -1
		case left.ID > right.ID:
			return 1
		default:
			return 0
		}
	})
	return finishCompositeTableCascade(ctx, b, snapshot, options, table, tableSpec, changedColumns, droppedColumns, rewrite)
}

func columnReference(name string) *zpgparse.Node {
	return &zpgparse.Node{Node: &zpgparse.Node_ColumnRef{ColumnRef: &zpgparse.ColumnRef{Fields: []*zpgparse.Node{{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: name}}}}, Location: -1}}}
}

func onlineSourceIndexes(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, ids []model.ObjectID) ([]*model.Descriptor, error) {
	owned := slices.Clone(ids)
	slices.Sort(owned)
	indexes := make([]*model.Descriptor, len(owned))
	for i, id := range owned {
		entry, err := snapshot.Descriptor(ctx, id)
		if err != nil {
			return nil, err
		}
		if entry.Descriptor.Kind() != model.ObjectIndex || entry.Descriptor.State() != model.StatePublic {
			return nil, bindError("55000", "table index is not public", 0)
		}
		b.dependency(entry.Descriptor, 0)
		indexes[i] = entry.Descriptor
	}
	return indexes, nil
}

func rebindTargetTableExpressions(ctx context.Context, source *model.Descriptor, spec *model.DescriptorSpec, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*model.Descriptor, error) {
	if source == nil || spec == nil || spec.Table == nil {
		return nil, model.ErrInvalidDescriptor
	}
	for i := range spec.Dependencies {
		if spec.Dependencies[i].Object == spec.ID {
			spec.Dependencies[i].Version = spec.Version
		}
	}
	sourceSpec, err := source.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if sourceSpec.Table == nil {
		return nil, model.ErrInvalidDescriptor
	}
	pending, err := model.NewDescriptor(ctx, *spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	for i := range spec.Table.Columns {
		column := &spec.Table.Columns[i]
		if column.Default != nil {
			for _, dependency := range column.Default.Dependencies {
				spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
			}
		}
		if column.Generated == nil {
			continue
		}
		layout := column.Layout
		definition, _, err := redefineRowExpression(ctx, column.Generated, pending, spec.Table.Columns, snapshot, options.Bind, rowStoredGenerated, column.ID, &layout, 0)
		if err != nil {
			return nil, err
		}
		column.Generated = definition
		for _, dependency := range definition.Dependencies {
			spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
		}
	}
	for i := range spec.Table.Constraints {
		constraint := &spec.Table.Constraints[i]
		if constraint.Kind != model.ConstraintCheck || constraint.Expression == nil {
			continue
		}
		definition, _, err := redefineRowExpression(ctx, constraint.Expression, pending, spec.Table.Columns, snapshot, options.Bind, rowCheck, 0, nil, constraint.ID)
		if err != nil {
			return nil, err
		}
		constraint.Expression = definition
		for _, dependency := range definition.Dependencies {
			spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
		}
	}
	for i := range spec.Table.Triggers {
		trigger := &spec.Table.Triggers[i]
		if trigger.When == nil {
			continue
		}
		definition, err := rebindTriggerWhen(ctx, trigger.When, sourceSpec.Table.Columns, spec.Table.Columns, snapshot, options.Bind)
		if err != nil {
			return nil, err
		}
		trigger.When = definition
		for _, dependency := range definition.Dependencies {
			spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
		}
	}
	slices.SortFunc(spec.Dependencies, compareDependency)
	spec.Dependencies = slices.Compact(spec.Dependencies)
	return model.NewDescriptor(ctx, *spec, options.Descriptors)
}

func onlineTargetIndexes(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, source []*model.Descriptor, table *model.Descriptor, _ model.ColumnID) ([]*model.Descriptor, error) {
	tableSpec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	result := make([]*model.Descriptor, len(source))
	for i, descriptor := range source {
		spec, err := descriptor.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if spec.Version == math.MaxUint64 || spec.Physical.Generation == math.MaxUint64 {
			return nil, model.ErrDescriptorLimit
		}
		spec.Version++
		spec.Physical.Generation++
		for j := range spec.Dependencies {
			if spec.Dependencies[j].Object == table.ID() {
				spec.Dependencies[j].Version = table.Version()
			}
		}
		for j := range spec.Index.Keys {
			key := &spec.Index.Keys[j]
			if key.Expression == nil {
				column := tableColumn(tableSpec.Table.Columns, key.Column)
				if column.ID == 0 {
					return nil, bindError("2BP01", "cannot rewrite an index whose column was removed", 0)
				}
				key.Type, key.Children = column.Layout.Type, slices.Clone(column.Layout.Children)
				continue
			}
			definition, binding, err := redefineRowExpression(ctx, key.Expression, table, tableSpec.Table.Columns, snapshot, options.Bind, rowIndexKey, 0, nil, 0)
			if err != nil {
				return nil, err
			}
			key.Expression = definition
			key.Type, key.Children, key.Collation = binding.Result.Type, slices.Clone(binding.Result.Children), binding.Result.Type.Collation
			for _, dependency := range definition.Dependencies {
				spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
			}
		}
		if spec.Index.Predicate != nil {
			definition, _, err := redefineRowExpression(ctx, spec.Index.Predicate, table, tableSpec.Table.Columns, snapshot, options.Bind, rowIndexPredicate, 0, nil, 0)
			if err != nil {
				return nil, err
			}
			spec.Index.Predicate = definition
			for _, dependency := range definition.Dependencies {
				spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
			}
		}
		slices.SortFunc(spec.Dependencies, compareDependency)
		spec.Dependencies = slices.Compact(spec.Dependencies)
		result[i], err = model.NewDescriptor(ctx, spec, options.Descriptors)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func compareDependency(a, b model.Dependency) int {
	if a.Object != b.Object {
		return int64Compare(uint64(a.Object), uint64(b.Object))
	}
	if a.Version != b.Version {
		return int64Compare(a.Version, b.Version)
	}
	return int64Compare(uint64(a.Column), uint64(b.Column))
}

func int64Compare(a, b uint64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func tableColumn(columns []model.ColumnDescriptor, id model.ColumnID) model.ColumnDescriptor {
	position := sort.Search(len(columns), func(i int) bool { return columns[i].ID >= id })
	if position < len(columns) && columns[position].ID == id {
		return columns[position]
	}
	return model.ColumnDescriptor{}
}

func tableMetadataReferencesColumn(tableID model.ObjectID, table *model.TableMetadata, id model.ColumnID) bool {
	for _, key := range table.PrimaryKey {
		if key.Column == id || expressionReferencesColumn(key.Expression, id) {
			return true
		}
	}
	for _, constraint := range table.Constraints {
		referenced := constraint.ReferencedObject == tableID && slices.Contains(constraint.ReferencedColumns, id)
		if slices.Contains(constraint.Columns, id) || referenced || expressionReferencesColumn(constraint.Expression, id) {
			return true
		}
	}
	for _, column := range table.Columns {
		if expressionReferencesColumn(column.Default, id) || expressionReferencesColumn(column.Generated, id) {
			return true
		}
	}
	for _, trigger := range table.Triggers {
		if slices.Contains(trigger.Columns, id) || expressionReferencesColumn(trigger.When, id) {
			return true
		}
	}
	return expressionReferencesColumn(table.Partition, id)
}

func indexMetadataReferencesColumn(index *model.IndexMetadata, id model.ColumnID) bool {
	if index == nil || slices.Contains(index.Include, id) || expressionReferencesColumn(index.Predicate, id) {
		return true
	}
	for _, key := range index.Keys {
		if key.Column == id || expressionReferencesColumn(key.Expression, id) {
			return true
		}
	}
	return false
}

func expressionReferencesColumn(definition *model.ExpressionDefinition, id model.ColumnID) bool {
	if definition == nil {
		return false
	}
	return slices.ContainsFunc(definition.Dependencies, func(dependency model.Dependency) bool { return dependency.Column == id })
}

func bindOnlinePrimaryKey(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, nodes []*zpgparse.Node) (*BoundSchemaDDL, error) {
	if len(nodes) != 2 {
		return nil, missing("online primary-key rewrite command combination", 0)
	}
	var drop *zpgparse.AlterTableCmd
	var add *zpgparse.Constraint
	for _, node := range nodes {
		command := node.GetAlterTableCmd()
		if command == nil {
			return nil, bindError("42601", "invalid primary-key rewrite command", 0)
		}
		switch command.Subtype {
		case zpgparse.AlterTableType_AT_DropConstraint:
			if drop != nil || command.Name == "" || command.MissingOk || command.Behavior == zpgparse.DropBehavior_DROP_CASCADE {
				return nil, missing("online primary-key DROP CONSTRAINT variant", 0)
			}
			drop = command
		case zpgparse.AlterTableType_AT_AddConstraint:
			if add != nil {
				return nil, bindError("42601", "multiple primary keys", 0)
			}
			add = command.Def.GetConstraint()
		default:
			return nil, missing("online primary-key rewrite command combination", 0)
		}
	}
	if drop == nil || add == nil || add.Contype != zpgparse.ConstrType_CONSTR_PRIMARY {
		return nil, missing("online primary-key rewrite requires DROP and ADD PRIMARY KEY", 0)
	}
	if err := ordinaryConstraint(add); err != nil {
		return nil, err
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	constraintPosition := slices.IndexFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
		return constraint.Name == drop.Name && constraint.Kind == model.ConstraintPrimaryKey
	})
	if constraintPosition < 0 {
		return nil, bindError("42704", "primary-key constraint does not exist", 0)
	}
	constraint := &spec.Table.Constraints[constraintPosition]
	if constraint.Index == 0 || add.Conname != "" && add.Conname != constraint.Name {
		return nil, missing("renaming an online primary-key constraint", int(add.Location)+1)
	}
	columnNames := make(map[string]model.ColumnID, len(spec.Table.Columns))
	for _, column := range spec.Table.Columns {
		if !column.SystemIdentity && column.State == model.StatePublic {
			columnNames[column.Name] = column.ID
		}
	}
	ids, err := declarationColumns(add.Keys, 0, columnNames)
	if err != nil {
		return nil, err
	}
	keys := make([]model.KeyElement, len(ids))
	for i, id := range ids {
		position := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool { return column.ID == id })
		if position < 0 {
			return nil, bindError("42703", "primary-key column does not exist", 0)
		}
		column := &spec.Table.Columns[position]
		column.Nullable = false
		keys[i] = model.KeyElement{Column: id, Type: column.Layout.Type, Children: slices.Clone(column.Layout.Children), Collation: column.Layout.Type.Collation}
	}
	constraint.Columns = slices.Clone(ids)
	constraint.Validated, constraint.Enforced = true, true
	spec.Table.PrimaryKey = keys
	indexes, err := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
	if err != nil {
		return nil, err
	}
	spec.Version = table.Version() + 3
	spec.Physical.Generation++
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	targetIndexes, err := onlineTargetIndexes(ctx, snapshot, options, indexes, target, 0)
	if err != nil {
		return nil, err
	}
	indexPosition := slices.IndexFunc(targetIndexes, func(index *model.Descriptor) bool { return index.ID() == constraint.Index })
	if indexPosition < 0 {
		return nil, bindError("55000", "primary-key backing index is missing", 0)
	}
	indexSpec, err := targetIndexes[indexPosition].SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	indexSpec.Index.Keys = slices.Clone(keys)
	indexSpec.Index.Unique = true
	indexSpec.Index.NullsNotDistinct = false
	targetIndexes[indexPosition], err = model.NewDescriptor(ctx, indexSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundSchemaDDL{source: table, definition: schema.JobDefinition{Operation: schema.SchemaRewritePrimaryKey, SourceTable: table, TargetTable: target, SourceIndexes: indexes, TargetIndexes: targetIndexes}, mutation: constraint.ID, dependencies: b.dependencies, warnings: b.warnings}, nil
}

func BindDropIndexConcurrently(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundSchemaDDL, error) {
	if statement == nil || !statement.Concurrent || statement.RemoveType != zpgparse.ObjectType_OBJECT_INDEX || len(statement.Objects) != 1 || statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE {
		return nil, missing("DROP INDEX CONCURRENTLY variant", 0)
	}
	b, err := schemaDDLBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	list := statement.Objects[0].GetList()
	if list == nil {
		return nil, bindError("42601", "invalid DROP INDEX name", 0)
	}
	parts, err := names(list.Items)
	if err != nil {
		return nil, err
	}
	index, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectIndex}, "42704", 0)
	if err != nil {
		if statement.MissingOk && isMissingObject(err) {
			return &BoundSchemaDDL{noOp: true, warnings: []model.Error{{Code: "42704", Message: "index does not exist, skipping"}}}, nil
		}
		return nil, err
	}
	indexSpec, err := index.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if indexSpec.Index == nil {
		return nil, model.ErrInvalidDescriptor
	}
	if indexSpec.Index.ParentIndex != 0 {
		return nil, bindError("2BP01", "cannot drop an attached index partition directly", 0)
	}
	tableEntry, err := snapshot.Descriptor(ctx, indexSpec.Index.Table)
	if err != nil {
		return nil, err
	}
	table := tableEntry.Descriptor
	if table == nil || table.State() != model.StatePublic || table.Version() > math.MaxUint64-3 || index.State() != model.StatePublic ||
		!indexSpec.Index.Ready() || !indexSpec.Index.Live() {
		return nil, bindError("55000", "index is not available for a concurrent drop", 0)
	}
	tableSpec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if tableSpec.Table == nil {
		return nil, model.ErrInvalidDescriptor
	}
	if tableSpec.Table.PartitionStrategy != 0 {
		return nil, bindError("0A000", "cannot drop a partitioned index concurrently", 0)
	}
	if slices.ContainsFunc(tableSpec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Index == index.ID() }) {
		return nil, bindError("2BP01", "cannot drop index because a constraint requires it", 0)
	}
	position := slices.Index(tableSpec.Table.Indexes, index.ID())
	if position < 0 {
		return nil, bindError("55000", "index is not linked to its table", 0)
	}
	tableSpec.Table.Indexes = slices.Delete(tableSpec.Table.Indexes, position, position+1)
	tableSpec.Version = table.Version() + 3
	target, err := model.NewDescriptor(ctx, tableSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	b.dependency(table, 0)
	b.dependency(index, 0)
	return &BoundSchemaDDL{source: table, definition: schema.JobDefinition{Operation: schema.SchemaDropIndex, SourceTable: table, TargetTable: target, SourceIndexes: []*model.Descriptor{index}}, mutation: index.ID(), dependencies: b.dependencies, warnings: b.warnings}, nil
}
