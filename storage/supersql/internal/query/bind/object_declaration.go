package bind

import (
	"context"
	"errors"
	"math"
	"slices"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type ObjectMutation struct {
	Base *model.Descriptor
	Next *model.Descriptor
}

type BoundObjectCommand struct {
	creates      []*model.Descriptor
	updates      []ObjectMutation
	drops        []*model.Descriptor
	dependencies []model.Dependency
	validations  []model.ObjectID
	warnings     []model.Error
	query        *BoundStatement
	tag          string
	cascade      bool
	populate     bool
}

func (c *BoundObjectCommand) Creates() []*model.Descriptor { return slices.Clone(c.creates) }
func (c *BoundObjectCommand) Updates() []ObjectMutation    { return slices.Clone(c.updates) }
func (c *BoundObjectCommand) Drops() []*model.Descriptor   { return slices.Clone(c.drops) }
func (c *BoundObjectCommand) Dependencies() []model.Dependency {
	return slices.Clone(c.dependencies)
}
func (c *BoundObjectCommand) Validations() []model.ObjectID { return slices.Clone(c.validations) }
func (c *BoundObjectCommand) Warnings() []model.Error       { return slices.Clone(c.warnings) }
func (c *BoundObjectCommand) CommandTag() string            { return c.tag }
func (c *BoundObjectCommand) Cascade() bool                 { return c.cascade }
func (c *BoundObjectCommand) Query() *BoundStatement        { return c.query }
func (c *BoundObjectCommand) Populate() bool                { return c.populate }
func (c *BoundObjectCommand) Declaration() (*BoundDeclaration, bool) {
	if c == nil || len(c.drops) != 0 {
		return nil, false
	}
	if len(c.creates) != 0 && len(c.updates) == 0 {
		return &BoundDeclaration{descriptors: slices.Clone(c.creates), dependencies: slices.Clone(c.dependencies), warnings: slices.Clone(c.warnings)}, true
	}
	if len(c.creates) == 0 && len(c.updates) == 1 {
		update := c.updates[0]
		return &BoundDeclaration{descriptors: []*model.Descriptor{update.Next}, base: update.Base, dependencies: slices.Clone(c.dependencies), warnings: slices.Clone(c.warnings)}, true
	}
	return nil, false
}

func BindObjectCommand(ctx context.Context, node *zpgparse.Node, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, bool, error) {
	if node == nil {
		return nil, false, bindError("42601", "missing object command", 0)
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_ViewStmt:
		command, err := bindCreateView(ctx, statement.ViewStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_CreateTableAsStmt:
		command, err := bindCreateTableAs(ctx, statement.CreateTableAsStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_RefreshMatViewStmt:
		command, err := bindRefreshMaterializedView(ctx, statement.RefreshMatViewStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_AlterTableStmt:
		if TriggerStateAlter(statement.AlterTableStmt) {
			command, err := bindTriggerStateAlter(ctx, statement.AlterTableStmt, snapshot, options)
			return command, true, err
		}
		if hierarchyAlter(statement.AlterTableStmt) {
			command, err := bindHierarchyAlter(ctx, statement.AlterTableStmt, snapshot, options)
			return command, true, err
		}
		if OrdinaryObjectAlter(statement.AlterTableStmt) {
			command, err := bindOrdinaryObjectAlter(ctx, statement.AlterTableStmt, snapshot, options)
			return command, true, err
		}
		return nil, false, nil
	case *zpgparse.Node_AlterFunctionStmt:
		command, err := bindAlterRoutine(ctx, statement.AlterFunctionStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_DropStmt:
		if TextSearchLifecycleKind(node) {
			command, _, err := BindTextSearchLifecycle(ctx, node, snapshot, options)
			return command, true, err
		}
		if routineObjectKind(statement.DropStmt.RemoveType) {
			command, err := bindDropRoutines(ctx, statement.DropStmt, snapshot, options)
			return command, true, err
		}
		if statement.DropStmt.RemoveType == zpgparse.ObjectType_OBJECT_TRIGGER {
			command, err := bindDropTrigger(ctx, statement.DropStmt, snapshot, options)
			return command, true, err
		}
		if statement.DropStmt.RemoveType == zpgparse.ObjectType_OBJECT_INDEX && !statement.DropStmt.Concurrent {
			command, err := bindDropIndexes(ctx, statement.DropStmt, snapshot, options)
			return command, true, err
		}
		if !ordinaryDropKind(statement.DropStmt.RemoveType) || statement.DropStmt.Concurrent {
			return nil, false, nil
		}
		command, err := bindDropObjects(ctx, statement.DropStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_RenameStmt:
		if TextSearchLifecycleKind(node) {
			command, _, err := BindTextSearchLifecycle(ctx, node, snapshot, options)
			return command, true, err
		}
		if statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_TABCONSTRAINT {
			command, err := bindRenameTableConstraint(ctx, statement.RenameStmt, snapshot, options)
			return command, true, err
		}
		if routineObjectKind(statement.RenameStmt.RenameType) {
			command, err := bindRenameRoutine(ctx, statement.RenameStmt, snapshot, options)
			return command, true, err
		}
		if statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_TRIGGER {
			command, err := bindRenameTrigger(ctx, statement.RenameStmt, snapshot, options)
			return command, true, err
		}
		if !ordinaryRenameKind(statement.RenameStmt.RenameType) {
			return nil, false, nil
		}
		command, err := bindRenameObject(ctx, statement.RenameStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_AlterObjectSchemaStmt:
		if TextSearchLifecycleKind(node) {
			command, _, err := BindTextSearchLifecycle(ctx, node, snapshot, options)
			return command, true, err
		}
		if routineObjectKind(statement.AlterObjectSchemaStmt.ObjectType) {
			command, err := bindSetRoutineSchema(ctx, statement.AlterObjectSchemaStmt, snapshot, options)
			return command, true, err
		}
		if !ordinarySetSchemaKind(statement.AlterObjectSchemaStmt.ObjectType) {
			return nil, false, nil
		}
		command, err := bindSetObjectSchema(ctx, statement.AlterObjectSchemaStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_CommentStmt:
		if DescribedObjectCommentKind(statement.CommentStmt.Objtype) {
			command, err := bindCommentDescribedObject(ctx, statement.CommentStmt, snapshot, options)
			return command, true, err
		}
		if !ordinaryCommentKind(statement.CommentStmt.Objtype) {
			return nil, false, nil
		}
		command, err := bindCommentObject(ctx, statement.CommentStmt, snapshot, options)
		return command, true, err
	default:
		return nil, false, nil
	}
}

func bindCreateView(ctx context.Context, statement *zpgparse.ViewStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.View == nil || statement.Query == nil || statement.View.Catalogname != "" {
		return nil, bindError("42601", "invalid CREATE VIEW", 0)
	}
	if statement.View.Relpersistence != "" && statement.View.Relpersistence != "p" && statement.View.Relpersistence != "t" {
		return nil, missing("unlogged view", int(statement.View.Location)+1)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_ViewStmt{ViewStmt: statement}})
	if err != nil {
		return nil, err
	}
	parent, name, err := declarationParent(ctx, b, statement.View, options.Bind.SearchPath)
	if err != nil {
		return nil, err
	}
	existing, err := relationNameConflict(ctx, snapshot, parent.ID(), name)
	if err != nil {
		return nil, err
	}
	if existing != nil && (!statement.Replace || existing.Kind() != model.ObjectView) {
		return nil, bindError("42P07", "relation already exists", int(statement.View.Location)+1)
	}
	if existing == nil {
		if err := implicitRelationTypeNamesAvailable(ctx, snapshot, parent.ID(), name); err != nil {
			return nil, err
		}
	}
	query, metadata, err := bindViewDefinition(ctx, statement.Query, statement.Aliases, statement.WithCheckOption, snapshot, options)
	if err != nil {
		return nil, err
	}
	if err := applyCreateViewOptions(metadata, statement.Options); err != nil {
		return nil, err
	}
	if metadata.CheckOption != "" && (metadata.Automatic == nil || len(metadata.Automatic.Columns) == 0) {
		return nil, bindError("0A000", "WITH CHECK OPTION is supported only on automatically updatable views", int(statement.View.Location)+1)
	}
	metadata.Owner = options.Owner
	metadata.Persistence = statement.View.Relpersistence
	if metadata.Persistence == "" {
		metadata.Persistence = "p"
	}
	metadata.Recursive = recursiveViewQuery(statement.Query)
	for _, dependency := range query.Dependencies() {
		published, resolveErr := snapshot.Descriptor(ctx, dependency.Object)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if metadata.Persistence != "t" {
			temporary, err := temporaryDescriptor(ctx, published.Descriptor, options.Descriptors)
			if err != nil {
				return nil, err
			}
			if temporary {
				return nil, bindError("42P16", "permanent view cannot reference a temporary relation", int(statement.View.Location)+1)
			}
		}
		b.dependency(published.Descriptor, dependency.Column)
	}
	b.dependency(parent, 0)
	command := &BoundObjectCommand{tag: "CREATE VIEW", query: query}
	if existing == nil {
		id, oid, allocateErr := options.Identities.AllocateDeclarationIdentity(ctx)
		if allocateErr != nil {
			return nil, allocateErr
		}
		rowTypeID, rowTypeOID, allocateErr := options.Identities.AllocateDeclarationIdentity(ctx)
		if allocateErr != nil {
			return nil, allocateErr
		}
		arrayTypeID, arrayTypeOID, allocateErr := options.Identities.AllocateDeclarationIdentity(ctx)
		if allocateErr != nil {
			return nil, allocateErr
		}
		metadata.RowType = rowTypeID
		descriptor, createErr := model.NewDescriptor(ctx, model.DescriptorSpec{ID: id, OID: oid, Name: model.NamespaceName{Parent: parent.ID(), Kind: model.ObjectView, Name: name}, Version: 1, State: model.StatePublic, Dependencies: b.dependencies, View: metadata}, options.Descriptors)
		if createErr != nil {
			return nil, createErr
		}
		rowType, arrayType, createErr := model.NewImplicitRelationRowTypes(ctx, descriptor, rowTypeID, rowTypeOID, arrayTypeID, arrayTypeOID, arrayTypeName(name), options.Owner, options.Descriptors)
		if createErr != nil {
			return nil, createErr
		}
		command.creates = []*model.Descriptor{descriptor, rowType, arrayType}
	} else {
		if existing.Version() == math.MaxUint64 {
			return nil, model.ErrInvalidDescriptor
		}
		spec, specErr := existing.SpecWithin(ctx, options.Descriptors)
		if specErr != nil {
			return nil, specErr
		}
		if spec.View == nil || spec.View.RowType == 0 {
			return nil, model.ErrInvalidDescriptor
		}
		if err := replacementViewColumns(spec.View.Columns, metadata.Columns); err != nil {
			return nil, err
		}
		for _, column := range metadata.Columns {
			if column.Default == nil {
				continue
			}
			for _, dependency := range column.Default.Dependencies {
				published, dependencyErr := snapshot.Descriptor(ctx, dependency.Object)
				if dependencyErr != nil {
					return nil, dependencyErr
				}
				b.dependency(published.Descriptor, dependency.Column)
			}
		}
		b.dependency(existing, 0)
		metadata.RowType = spec.View.RowType
		spec.Version++
		spec.Dependencies = descriptorDependencies(b.dependencies, existing.ID())
		spec.View = metadata
		next, replaceErr := model.NewDescriptor(ctx, spec, options.Descriptors)
		if replaceErr != nil {
			return nil, replaceErr
		}
		typeUpdates, replaceErr := updateImplicitRelationRowTypes(ctx, snapshot, next, options.Descriptors)
		if replaceErr != nil {
			return nil, replaceErr
		}
		command.updates = append([]ObjectMutation{{Base: existing, Next: next}}, typeUpdates...)
	}
	command.dependencies = b.dependencies
	command.warnings = b.warnings
	return command, ctx.Err()
}

func bindViewDefinition(ctx context.Context, queryNode *zpgparse.Node, aliases []*zpgparse.Node, check zpgparse.ViewCheckOption, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundStatement, *model.ViewMetadata, error) {
	if queryNode == nil || queryNode.GetSelectStmt() == nil {
		return nil, nil, bindError("42601", "view definition must be a SELECT", 0)
	}
	raw := &zpgparse.RawStmt{Stmt: proto.Clone(queryNode).(*zpgparse.Node)}
	bound, err := Bind(ctx, raw, snapshot, options.Bind, nil)
	if err != nil {
		return nil, nil, err
	}
	if len(bound.Parameters()) != 0 {
		return nil, nil, bindError("42P02", "view definition cannot contain parameters", 0)
	}
	columns := bound.Columns()
	aliasNames, err := names(aliases)
	if err != nil {
		return nil, nil, err
	}
	if len(aliasNames) > len(columns) {
		return nil, nil, bindError("42P16", "more view column names than columns", 0)
	}
	seen := make(map[string]struct{}, len(columns))
	metadataColumns := make([]model.ColumnDescriptor, len(columns))
	for i, column := range columns {
		name := column.Name
		if i < len(aliasNames) {
			name = aliasNames[i]
		}
		if err := declarationName(name); err != nil {
			return nil, nil, err
		}
		if _, duplicate := seen[name]; duplicate {
			return nil, nil, bindError("42701", "view column name specified more than once", 0)
		}
		seen[name] = struct{}{}
		metadataColumns[i] = model.ColumnDescriptor{ID: model.ColumnID(i + 1), Name: name, State: model.StatePublic, Layout: column.Layout, Nullable: column.Nullable}
	}
	encoding, err := (proto.MarshalOptions{Deterministic: true}).Marshal(raw)
	if err != nil {
		return nil, nil, err
	}
	definitionSQL, err := zpgparse.Deparse(&zpgparse.ParseResult{Stmts: []*zpgparse.RawStmt{raw}})
	if err != nil {
		return nil, nil, err
	}
	checkOption := ""
	switch check {
	case zpgparse.ViewCheckOption_VIEW_CHECK_OPTION_UNDEFINED, zpgparse.ViewCheckOption_NO_CHECK_OPTION:
	case zpgparse.ViewCheckOption_LOCAL_CHECK_OPTION:
		checkOption = "local"
	case zpgparse.ViewCheckOption_CASCADED_CHECK_OPTION:
		checkOption = "cascaded"
	default:
		return nil, nil, bindError("42601", "invalid view check option", 0)
	}
	automatic, err := analyzeAutomaticView(ctx, queryNode.GetSelectStmt(), bound, snapshot, options.Bind.SearchPath)
	if err != nil {
		return nil, nil, err
	}
	if checkOption != "" && (automatic == nil || len(automatic.Columns) == 0) {
		return nil, nil, bindError("0A000", "WITH CHECK OPTION is supported only on automatically updatable views", 0)
	}
	definition := model.ExpressionDefinition{SemanticVersion: 1, Encoding: encoding, Dependencies: bound.Dependencies()}
	return bound, &model.ViewMetadata{Columns: metadataColumns, Query: definition, Definition: definitionSQL, CheckOption: checkOption, Automatic: automatic}, nil
}

func replacementViewColumns(old, next []model.ColumnDescriptor) error {
	if len(next) < len(old) {
		return bindError("42P16", "cannot drop columns from view", 0)
	}
	for i := range old {
		if old[i].Name != next[i].Name || !sameType(old[i].Layout, next[i].Layout) {
			return bindError("42P16", "cannot change name or type of a view column", 0)
		}
		next[i].ID = old[i].ID
		next[i].Description = old[i].Description
		next[i].ConstantDefault = old[i].ConstantDefault
		next[i].Default = old[i].Default
		next[i].DefaultOID = old[i].DefaultOID
	}
	return nil
}

func recursiveViewQuery(node *zpgparse.Node) bool {
	selectNode := node.GetSelectStmt()
	return selectNode != nil && selectNode.WithClause != nil && selectNode.WithClause.Recursive
}

func bindCreateTableAs(ctx context.Context, statement *zpgparse.CreateTableAsStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Query == nil || statement.Into == nil || statement.Into.Rel == nil {
		return nil, bindError("42601", "invalid CREATE TABLE AS", 0)
	}
	if statement.Objtype != zpgparse.ObjectType_OBJECT_TABLE && statement.Objtype != zpgparse.ObjectType_OBJECT_MATVIEW {
		return nil, missing("CREATE TABLE AS object kind", 0)
	}
	into := statement.Into
	if into.AccessMethod != "" || len(into.Options) != 0 || into.TableSpaceName != "" {
		return nil, missing("CREATE TABLE AS storage options", int(into.Rel.Location)+1)
	}
	if into.Rel.Catalogname != "" || into.Rel.Relpersistence == "u" {
		return nil, missing("cross-database or unlogged CREATE TABLE AS", int(into.Rel.Location)+1)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreateTableAsStmt{CreateTableAsStmt: statement}})
	if err != nil {
		return nil, err
	}
	parent, name, err := declarationParent(ctx, b, into.Rel, options.Bind.SearchPath)
	if err != nil {
		return nil, err
	}
	existing, err := relationNameConflict(ctx, snapshot, parent.ID(), name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if statement.IfNotExists {
			b.dependency(existing, 0)
			return &BoundObjectCommand{tag: createTableAsTag(statement.Objtype), dependencies: b.dependencies, warnings: []model.Error{{Code: "42P07", Message: "relation already exists, skipping"}}}, nil
		}
		return nil, bindError("42P07", "relation already exists", int(into.Rel.Location)+1)
	}
	if err := implicitRelationTypeNamesAvailable(ctx, snapshot, parent.ID(), name); err != nil {
		return nil, err
	}
	persistence := into.Rel.Relpersistence
	if persistence == "" {
		persistence = "p"
	}
	query, view, err := bindViewDefinition(ctx, statement.Query, into.ColNames, zpgparse.ViewCheckOption_NO_CHECK_OPTION, snapshot, options)
	if err != nil {
		return nil, err
	}
	for _, dependency := range query.Dependencies() {
		published, resolveErr := snapshot.Descriptor(ctx, dependency.Object)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if statement.Objtype == zpgparse.ObjectType_OBJECT_MATVIEW && persistence != "t" {
			temporary, err := temporaryDescriptor(ctx, published.Descriptor, options.Descriptors)
			if err != nil {
				return nil, err
			}
			if temporary {
				return nil, bindError("42P16", "permanent materialized view cannot reference a temporary relation", int(into.Rel.Location)+1)
			}
		}
		b.dependency(published.Descriptor, dependency.Column)
	}
	b.dependency(parent, 0)
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	rowTypeID, rowTypeOID, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	arrayTypeID, arrayTypeOID, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	table := materializedTable(view.Columns, options.Owner, persistence)
	switch into.OnCommit {
	case zpgparse.OnCommitAction_ON_COMMIT_ACTION_UNDEFINED, zpgparse.OnCommitAction_ONCOMMIT_NOOP:
		if persistence == "t" {
			table.OnCommit = "preserve"
		}
	case zpgparse.OnCommitAction_ONCOMMIT_PRESERVE_ROWS:
		if persistence != "t" {
			return nil, bindError("42P16", "ON COMMIT can only be used on temporary tables", 0)
		}
		table.OnCommit = "preserve"
	case zpgparse.OnCommitAction_ONCOMMIT_DELETE_ROWS:
		if persistence != "t" {
			return nil, bindError("42P16", "ON COMMIT can only be used on temporary tables", 0)
		}
		table.OnCommit = "delete"
	case zpgparse.OnCommitAction_ONCOMMIT_DROP:
		if persistence != "t" {
			return nil, bindError("42P16", "ON COMMIT can only be used on temporary tables", 0)
		}
		table.OnCommit = "drop"
	default:
		return nil, bindError("42601", "invalid ON COMMIT action", 0)
	}
	table.RowType = rowTypeID
	kind := model.ObjectTable
	var definition *model.ViewMetadata
	if statement.Objtype == zpgparse.ObjectType_OBJECT_MATVIEW {
		kind = model.ObjectMaterializedView
		view.Owner = options.Owner
		view.Persistence = persistence
		view.Populated = !into.SkipData
		view.RowType = rowTypeID
		// A materialized view stores rows in its own backing table, so it is
		// never automatically updatable: the automatic-update mapping bound for
		// the view definition (and rejected by descriptor validation on a
		// materialized view) is dropped here.
		view.Automatic = nil
		definition = view
	}
	descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: id, OID: oid, Name: model.NamespaceName{Parent: parent.ID(), Kind: kind, Name: name}, Version: 1, State: model.StatePublic, Physical: options.Physical, Dependencies: b.dependencies, Table: table, View: definition}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	rowType, arrayType, err := model.NewImplicitRelationRowTypes(ctx, descriptor, rowTypeID, rowTypeOID, arrayTypeID, arrayTypeOID, arrayTypeName(name), options.Owner, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundObjectCommand{creates: []*model.Descriptor{descriptor, rowType, arrayType}, dependencies: b.dependencies, warnings: b.warnings, query: query, tag: createTableAsTag(statement.Objtype), populate: !into.SkipData}, ctx.Err()
}

func materializedTable(columns []model.ColumnDescriptor, owner model.OID, persistence string) *model.TableMetadata {
	physical := slices.Clone(columns)
	identity := model.ColumnID(len(physical) + 1)
	layout := model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes, Modifier: -1}}
	physical = append(physical, model.ColumnDescriptor{ID: identity, Name: "__supersql_row_identity", State: model.StatePublic, Layout: layout, SystemIdentity: true})
	return &model.TableMetadata{Owner: owner, ColumnIDHighWatermark: identity, Columns: physical, PrimaryKey: []model.KeyElement{{Column: identity, Type: layout.Type}}, Persistence: persistence}
}

func createTableAsTag(kind zpgparse.ObjectType) string {
	if kind == zpgparse.ObjectType_OBJECT_MATVIEW {
		return "CREATE MATERIALIZED VIEW"
	}
	return "CREATE TABLE AS"
}

func bindRefreshMaterializedView(ctx context.Context, statement *zpgparse.RefreshMatViewStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Relation == nil || statement.Relation.Catalogname != "" {
		return nil, bindError("42601", "invalid REFRESH MATERIALIZED VIEW", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RefreshMatViewStmt{RefreshMatViewStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	base, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectMaterializedView}, "42P01", int(statement.Relation.Location)+1)
	if err != nil {
		return nil, err
	}
	if base.Version() == math.MaxUint64 || base.Physical().Generation == math.MaxUint64 {
		return nil, model.ErrInvalidDescriptor
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.View == nil || spec.Table == nil {
		return nil, model.ErrInvalidDescriptor
	}
	if statement.Concurrent {
		if statement.SkipData || !spec.View.Populated || !materializedUniqueIndex(spec.Table, snapshot, ctx) {
			return nil, bindError("55000", "CONCURRENTLY requires a populated materialized view with a unique index", 0)
		}
	}
	raw := &zpgparse.RawStmt{}
	if err := proto.Unmarshal(spec.View.Query.Encoding, raw); err != nil || raw.Stmt == nil {
		return nil, model.ErrInvalidDescriptor
	}
	query, err := Bind(ctx, raw, snapshot, options.Bind, nil)
	if err != nil {
		return nil, err
	}
	columns := query.Columns()
	if len(columns) != len(spec.View.Columns) {
		return nil, bindError("42804", "materialized view query result changed", 0)
	}
	for i := range columns {
		if !sameType(columns[i].Layout, spec.View.Columns[i].Layout) {
			return nil, bindError("42804", "materialized view query result type changed", 0)
		}
	}
	for _, dependency := range query.Dependencies() {
		published, resolveErr := snapshot.Descriptor(ctx, dependency.Object)
		if resolveErr != nil {
			return nil, resolveErr
		}
		b.dependency(published.Descriptor, dependency.Column)
	}
	spec.Version++
	spec.Physical.Generation++
	spec.Dependencies = descriptorDependencies(b.dependencies, base.ID())
	spec.View.Populated = !statement.SkipData
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	typeUpdates, err := updateImplicitRelationRowTypes(ctx, snapshot, next, options.Descriptors)
	if err != nil {
		return nil, err
	}
	indexUpdates, err := materializedIndexGenerations(ctx, snapshot, next, options.Descriptors)
	if err != nil {
		return nil, err
	}
	updates := append([]ObjectMutation{{Base: base, Next: next}}, typeUpdates...)
	updates = append(updates, indexUpdates...)
	return &BoundObjectCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, query: query, tag: "REFRESH MATERIALIZED VIEW", populate: !statement.SkipData}, ctx.Err()
}

func materializedUniqueIndex(table *model.TableMetadata, snapshot CatalogSnapshot, ctx context.Context) bool {
	for _, id := range table.Indexes {
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil || published.Descriptor.State() != model.StatePublic {
			continue
		}
		metadata, ok := published.Descriptor.Index()
		simple := ok && !slices.ContainsFunc(metadata.Keys, func(key model.KeyElement) bool { return key.Column == 0 })
		if simple && metadata.Valid() && metadata.Unique && metadata.Predicate == nil {
			return true
		}
	}
	return false
}

func materializedIndexGenerations(ctx context.Context, snapshot CatalogSnapshot, relation *model.Descriptor, limits model.DescriptorLimits) ([]ObjectMutation, error) {
	spec, err := relation.SpecWithin(ctx, limits)
	if err != nil {
		return nil, err
	}
	if spec.Table == nil {
		return nil, model.ErrInvalidDescriptor
	}
	updates := make([]ObjectMutation, 0, len(spec.Table.Indexes))
	for _, id := range spec.Table.Indexes {
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil {
			return nil, err
		}
		base := published.Descriptor
		if base.Kind() != model.ObjectIndex || base.Version() == math.MaxUint64 || base.Physical().Generation == math.MaxUint64 {
			return nil, model.ErrInvalidDescriptor
		}
		indexSpec, err := base.SpecWithin(ctx, limits)
		if err != nil {
			return nil, err
		}
		indexSpec.Version++
		indexSpec.Physical.Generation++
		for i := range indexSpec.Dependencies {
			if indexSpec.Dependencies[i].Object == relation.ID() {
				indexSpec.Dependencies[i].Version = relation.Version()
			}
		}
		next, err := model.NewDescriptor(ctx, indexSpec, limits)
		if err != nil {
			return nil, err
		}
		updates = append(updates, ObjectMutation{Base: base, Next: next})
	}
	return updates, nil
}

func declarationParent(ctx context.Context, b *binder, relation *zpgparse.RangeVar, searchPath []string) (*model.Descriptor, string, error) {
	if relation == nil || relation.Relname == "" {
		return nil, "", bindError("42601", "missing relation name", 0)
	}
	if err := declarationName(relation.Relname); err != nil {
		return nil, "", err
	}
	path := searchPath
	if relation.Schemaname != "" {
		path = []string{relation.Schemaname}
	}
	for _, schemaName := range path {
		published, err := b.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if published.Descriptor.State() == model.StatePublic {
			b.dependency(published.Descriptor, 0)
			return published.Descriptor, relation.Relname, nil
		}
	}
	return nil, "", bindError("3F000", "no schema selected for creation", int(relation.Location)+1)
}

func relationNameConflict(ctx context.Context, snapshot CatalogSnapshot, parent model.ObjectID, name string) (*model.Descriptor, error) {
	for _, kind := range []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation} {
		published, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: parent, Kind: kind, Name: name})
		if err == nil {
			return published.Descriptor, nil
		}
		if !isMissingObject(err) {
			return nil, err
		}
	}
	return nil, nil
}

func ordinaryDropKind(kind zpgparse.ObjectType) bool {
	switch kind {
	case zpgparse.ObjectType_OBJECT_SCHEMA, zpgparse.ObjectType_OBJECT_TABLE, zpgparse.ObjectType_OBJECT_INDEX, zpgparse.ObjectType_OBJECT_SEQUENCE, zpgparse.ObjectType_OBJECT_VIEW, zpgparse.ObjectType_OBJECT_MATVIEW:
		return true
	default:
		return false
	}
}

func ordinaryRenameKind(kind zpgparse.ObjectType) bool {
	return ordinaryDropKind(kind) || kind == zpgparse.ObjectType_OBJECT_COLUMN
}

func ordinarySetSchemaKind(kind zpgparse.ObjectType) bool {
	switch kind {
	case zpgparse.ObjectType_OBJECT_TABLE, zpgparse.ObjectType_OBJECT_SEQUENCE, zpgparse.ObjectType_OBJECT_VIEW, zpgparse.ObjectType_OBJECT_MATVIEW:
		return true
	default:
		return false
	}
}

func ordinaryCommentKind(kind zpgparse.ObjectType) bool {
	switch kind {
	case zpgparse.ObjectType_OBJECT_SCHEMA, zpgparse.ObjectType_OBJECT_TABLE, zpgparse.ObjectType_OBJECT_INDEX, zpgparse.ObjectType_OBJECT_SEQUENCE, zpgparse.ObjectType_OBJECT_VIEW, zpgparse.ObjectType_OBJECT_MATVIEW, zpgparse.ObjectType_OBJECT_COLUMN:
		return true
	default:
		return false
	}
}

func objectKinds(kind zpgparse.ObjectType) []model.ObjectKind {
	switch kind {
	case zpgparse.ObjectType_OBJECT_SCHEMA:
		return []model.ObjectKind{model.ObjectSchema}
	case zpgparse.ObjectType_OBJECT_TABLE:
		return []model.ObjectKind{model.ObjectTable}
	case zpgparse.ObjectType_OBJECT_COLUMN:
		return []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView}
	case zpgparse.ObjectType_OBJECT_INDEX:
		return []model.ObjectKind{model.ObjectIndex}
	case zpgparse.ObjectType_OBJECT_SEQUENCE:
		return []model.ObjectKind{model.ObjectSequence}
	case zpgparse.ObjectType_OBJECT_VIEW:
		return []model.ObjectKind{model.ObjectView}
	case zpgparse.ObjectType_OBJECT_MATVIEW:
		return []model.ObjectKind{model.ObjectMaterializedView}
	default:
		return nil
	}
}

func objectCommandTag(prefix string, kind zpgparse.ObjectType) string {
	name := map[zpgparse.ObjectType]string{
		zpgparse.ObjectType_OBJECT_SCHEMA:    "SCHEMA",
		zpgparse.ObjectType_OBJECT_TABLE:     "TABLE",
		zpgparse.ObjectType_OBJECT_INDEX:     "INDEX",
		zpgparse.ObjectType_OBJECT_SEQUENCE:  "SEQUENCE",
		zpgparse.ObjectType_OBJECT_VIEW:      "VIEW",
		zpgparse.ObjectType_OBJECT_MATVIEW:   "MATERIALIZED VIEW",
		zpgparse.ObjectType_OBJECT_COLUMN:    "TABLE",
		zpgparse.ObjectType_OBJECT_FUNCTION:  "FUNCTION",
		zpgparse.ObjectType_OBJECT_PROCEDURE: "PROCEDURE",
		zpgparse.ObjectType_OBJECT_ROUTINE:   "ROUTINE",
		zpgparse.ObjectType_OBJECT_TRIGGER:   "TRIGGER",
	}[kind]
	return prefix + " " + name
}

func bindDropObjects(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	command := &BoundObjectCommand{tag: objectCommandTag("DROP", statement.RemoveType), cascade: statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE}
	seen := make(map[model.ObjectID]bool)
	for _, node := range statement.Objects {
		list := node.GetList()
		if list == nil {
			return nil, bindError("42601", "invalid DROP object name", 0)
		}
		parts, nameErr := names(list.Items)
		if nameErr != nil {
			return nil, nameErr
		}
		descriptor, resolveErr := resolveOrdinaryObject(ctx, b, parts, objectKinds(statement.RemoveType), "42704")
		if resolveErr != nil {
			var diagnostic *model.Error
			if statement.MissingOk && errors.As(resolveErr, &diagnostic) && (diagnostic.Code == "42704" || diagnostic.Code == "42P01" || diagnostic.Code == "3F000") {
				command.warnings = append(command.warnings, model.Error{Code: diagnostic.Code, Message: "object does not exist, skipping"})
				continue
			}
			return nil, resolveErr
		}
		if !seen[descriptor.ID()] {
			seen[descriptor.ID()] = true
			command.drops = append(command.drops, descriptor)
			b.dependency(descriptor, 0)
		}
		if descriptor.Kind() == model.ObjectTable || descriptor.Kind() == model.ObjectView || descriptor.Kind() == model.ObjectMaterializedView {
			owned, ownedErr := ownedRelationObjects(ctx, snapshot, descriptor, options.Descriptors)
			if ownedErr != nil {
				return nil, ownedErr
			}
			for _, child := range owned {
				if !seen[child.ID()] {
					seen[child.ID()] = true
					command.drops = append(command.drops, child)
					b.dependency(child, 0)
				}
			}
			types, typeErr := implicitRelationTypes(ctx, snapshot, descriptor, options.Descriptors)
			if typeErr != nil {
				return nil, typeErr
			}
			for _, child := range types {
				if !seen[child.ID()] {
					seen[child.ID()] = true
					command.drops = append(command.drops, child)
					b.dependency(child, 0)
				}
			}
		}
	}
	if err := addInheritanceDropUpdates(ctx, command, snapshot, options.Descriptors); err != nil {
		return nil, err
	}
	command.dependencies = b.dependencies
	command.warnings = append(command.warnings, b.warnings...)
	return command, ctx.Err()
}

func ownedRelationObjects(ctx context.Context, snapshot CatalogSnapshot, descriptor *model.Descriptor, limits model.DescriptorLimits) ([]*model.Descriptor, error) {
	spec, err := descriptor.SpecWithin(ctx, limits)
	if err != nil {
		return nil, err
	}
	if spec.Table == nil {
		return nil, nil
	}
	ids := slices.Clone(spec.Table.Indexes)
	for _, column := range spec.Table.Columns {
		if column.IdentitySequence != 0 {
			ids = append(ids, column.IdentitySequence)
		}
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	result := make([]*model.Descriptor, 0, len(ids))
	for _, id := range ids {
		published, err := snapshot.Descriptor(ctx, id)
		if err != nil {
			return nil, err
		}
		result = append(result, published.Descriptor)
	}
	return result, nil
}

func implicitRelationTypes(ctx context.Context, snapshot CatalogSnapshot, descriptor *model.Descriptor, limits model.DescriptorLimits) ([]*model.Descriptor, error) {
	spec, err := descriptor.SpecWithin(ctx, limits)
	if err != nil {
		return nil, err
	}
	var rowTypeID model.ObjectID
	if spec.View != nil {
		rowTypeID = spec.View.RowType
	}
	if spec.Table != nil {
		if rowTypeID != 0 && rowTypeID != spec.Table.RowType {
			return nil, model.ErrInvalidDescriptor
		}
		rowTypeID = spec.Table.RowType
	}
	if rowTypeID == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	publishedRow, err := snapshot.Descriptor(ctx, rowTypeID)
	if err != nil {
		return nil, err
	}
	rowSpec, err := publishedRow.Descriptor.SpecWithin(ctx, limits)
	if err != nil {
		return nil, err
	}
	if rowSpec.Type == nil || rowSpec.Type.Relation != descriptor.ID() || rowSpec.Type.Array == 0 {
		return nil, model.ErrInvalidDescriptor
	}
	publishedArray, err := snapshot.Descriptor(ctx, rowSpec.Type.Array)
	if err != nil {
		return nil, err
	}
	return []*model.Descriptor{publishedRow.Descriptor, publishedArray.Descriptor}, nil
}

func resolveOrdinaryObject(ctx context.Context, b *binder, parts []string, kinds []model.ObjectKind, code string) (*model.Descriptor, error) {
	if len(kinds) == 1 && kinds[0] == model.ObjectSchema {
		if len(parts) != 1 {
			return nil, bindError("42601", "schema name cannot be qualified", 0)
		}
		published, err := b.catalog.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: parts[0]})
		if err != nil {
			if isMissingObject(err) {
				return nil, bindError(code, "schema does not exist", 0)
			}
			return nil, err
		}
		b.dependency(published.Descriptor, 0)
		return published.Descriptor, nil
	}
	descriptor, _, err := b.resolve(parts, kinds, code, 0)
	return descriptor, err
}

func bindRenameObject(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Newname == "" {
		return nil, bindError("42601", "invalid RENAME", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	if statement.RenameType == zpgparse.ObjectType_OBJECT_COLUMN {
		return bindRenameColumn(ctx, b, statement, snapshot, options)
	}
	parts, err := renameObjectParts(statement)
	if err != nil {
		return nil, err
	}
	base, err := resolveOrdinaryObject(ctx, b, parts, objectKinds(statement.RenameType), "42704")
	if err != nil {
		if statement.MissingOk {
			var diagnostic *model.Error
			if errors.As(err, &diagnostic) && (diagnostic.Code == "42704" || diagnostic.Code == "42P01") {
				return &BoundObjectCommand{tag: objectCommandTag("ALTER", statement.RenameType), warnings: []model.Error{{Code: diagnostic.Code, Message: "object does not exist, skipping"}}}, nil
			}
		}
		return nil, err
	}
	if base.Kind() == model.ObjectSchema {
		if _, conflictErr := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: statement.Newname}); conflictErr == nil {
			return nil, bindError("42P06", "schema already exists", 0)
		} else if !isMissingObject(conflictErr) {
			return nil, conflictErr
		}
	} else if conflict, conflictErr := relationNameConflict(ctx, snapshot, base.Name().Parent, statement.Newname); conflictErr != nil {
		return nil, conflictErr
	} else if conflict != nil {
		return nil, bindError("42P07", "relation already exists", 0)
	}
	var relationTypes []*model.Descriptor
	if base.Kind() == model.ObjectTable || base.Kind() == model.ObjectView || base.Kind() == model.ObjectMaterializedView {
		relationTypes, err = implicitRelationTypes(ctx, snapshot, base, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if err := implicitRelationTypeNamesAvailable(ctx, snapshot, base.Name().Parent, statement.Newname, relationTypes[0].ID(), relationTypes[1].ID()); err != nil {
			return nil, err
		}
	}
	next, err := nextObjectDescriptor(ctx, base, options, func(spec *model.DescriptorSpec) error {
		spec.Name.Name = statement.Newname
		return nil
	})
	if err != nil {
		return nil, err
	}
	b.dependency(base, 0)
	updates := []ObjectMutation{{Base: base, Next: next}}
	if base.Kind() == model.ObjectIndex {
		metadata, ok := base.Index()
		if !ok || metadata.Table == 0 {
			return nil, model.ErrInvalidDescriptor
		}
		published, resolveErr := snapshot.Descriptor(ctx, metadata.Table)
		if resolveErr != nil {
			return nil, resolveErr
		}
		table := published.Descriptor
		tableSpec, specErr := table.SpecWithin(ctx, options.Descriptors)
		if specErr != nil {
			return nil, specErr
		}
		if tableSpec.Table == nil {
			return nil, model.ErrInvalidDescriptor
		}
		constraint := slices.IndexFunc(tableSpec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool {
			return constraint.Index == base.ID()
		})
		if constraint >= 0 {
			if slices.ContainsFunc(tableSpec.Table.Constraints, func(candidate model.ConstraintDescriptor) bool {
				return candidate.Name == statement.Newname && candidate.Index != base.ID()
			}) {
				return nil, bindError("42710", "constraint already exists", 0)
			}
			if tableSpec.Version == math.MaxUint64 {
				return nil, model.ErrDescriptorLimit
			}
			tableSpec.Table.Constraints[constraint].Name = statement.Newname
			tableSpec.Version++
			tableNext, nextErr := model.NewDescriptor(ctx, tableSpec, options.Descriptors)
			if nextErr != nil {
				return nil, nextErr
			}
			b.dependency(table, 0)
			updates = append(updates, ObjectMutation{Base: table, Next: tableNext})
		}
	}
	if len(relationTypes) != 0 {
		rowType, arrayType, typeErr := model.NextImplicitRelationRowTypes(ctx, next, relationTypes[0], relationTypes[1], arrayTypeName(next.Name().Name), options.Descriptors)
		if typeErr != nil {
			return nil, typeErr
		}
		updates = append(updates, ObjectMutation{Base: relationTypes[0], Next: rowType}, ObjectMutation{Base: relationTypes[1], Next: arrayType})
	}
	return &BoundObjectCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: objectCommandTag("ALTER", statement.RenameType)}, ctx.Err()
}

func bindRenameColumn(ctx context.Context, b *binder, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement.Relation == nil || statement.Subname == "" {
		return nil, bindError("42601", "invalid RENAME COLUMN", 0)
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	base, err := resolveOrdinaryObject(ctx, b, parts, objectKinds(statement.RelationType), "42P01")
	if err != nil {
		return nil, err
	}
	next, err := nextObjectDescriptor(ctx, base, options, func(spec *model.DescriptorSpec) error {
		rename := func(columns []model.ColumnDescriptor) error {
			index := slices.IndexFunc(columns, func(column model.ColumnDescriptor) bool {
				return column.Name == statement.Subname && !column.SystemIdentity
			})
			if index < 0 {
				return bindError("42703", "column does not exist", 0)
			}
			if slices.ContainsFunc(columns, func(column model.ColumnDescriptor) bool { return column.Name == statement.Newname }) {
				return bindError("42701", "column already exists", 0)
			}
			columns[index].Name = statement.Newname
			return nil
		}
		if spec.Table != nil {
			if err := rename(spec.Table.Columns); err != nil {
				return err
			}
		}
		if spec.View != nil {
			if err := rename(spec.View.Columns); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	b.dependency(base, 0)
	typeUpdates, typeErr := updateImplicitRelationRowTypes(ctx, snapshot, next, options.Descriptors)
	if typeErr != nil {
		return nil, typeErr
	}
	updates := append([]ObjectMutation{{Base: base, Next: next}}, typeUpdates...)
	return &BoundObjectCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: "ALTER TABLE"}, ctx.Err()
}

func renameObjectParts(statement *zpgparse.RenameStmt) ([]string, error) {
	if statement.Relation != nil {
		parts := []string{statement.Relation.Relname}
		if statement.Relation.Schemaname != "" {
			parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
		}
		return parts, nil
	}
	if statement.Object == nil {
		return nil, bindError("42601", "missing rename object", 0)
	}
	if list := statement.Object.GetList(); list != nil {
		return names(list.Items)
	}
	if value := statement.Object.GetString_(); value != nil {
		return []string{value.Sval}, nil
	}
	return nil, bindError("42601", "invalid rename object", 0)
}

func bindSetObjectSchema(ctx context.Context, statement *zpgparse.AlterObjectSchemaStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Newschema == "" {
		return nil, bindError("42601", "invalid SET SCHEMA", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterObjectSchemaStmt{AlterObjectSchemaStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := alterObjectParts(statement.Relation, statement.Object)
	if err != nil {
		return nil, err
	}
	base, err := resolveOrdinaryObject(ctx, b, parts, objectKinds(statement.ObjectType), "42704")
	if err != nil {
		return nil, err
	}
	baseSpec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if baseSpec.Table != nil && baseSpec.Table.Persistence == "t" || baseSpec.View != nil && baseSpec.View.Persistence == "t" || baseSpec.Sequence != nil && baseSpec.Sequence.Persistence == "t" {
		return nil, bindError("42P16", "temporary objects cannot be moved to another schema", 0)
	}
	destination, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: statement.Newschema})
	if err != nil {
		if isMissingObject(err) {
			return nil, bindError("3F000", "schema does not exist", 0)
		}
		return nil, err
	}
	if base.Kind() == model.ObjectTable || base.Kind() == model.ObjectView || base.Kind() == model.ObjectMaterializedView {
		if err := implicitRelationTypeNamesAvailable(ctx, snapshot, destination.Descriptor.ID(), base.Name().Name); err != nil {
			return nil, err
		}
	}
	move := func(descriptor *model.Descriptor) (*model.Descriptor, error) {
		if conflict, conflictErr := relationNameConflict(ctx, snapshot, destination.Descriptor.ID(), descriptor.Name().Name); conflictErr != nil {
			return nil, conflictErr
		} else if conflict != nil && conflict.ID() != descriptor.ID() {
			return nil, bindError("42P07", "relation already exists in destination schema", 0)
		}
		return nextObjectDescriptor(ctx, descriptor, options, func(spec *model.DescriptorSpec) error {
			spec.Name.Parent = destination.Descriptor.ID()
			spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
				return dependency.Object == descriptor.Name().Parent && dependency.Column == 0
			})
			spec.Dependencies = appendIndexDependency(spec.Dependencies, model.Dependency{Object: destination.Descriptor.ID(), Version: destination.Descriptor.Version()})
			return nil
		})
	}
	next, err := move(base)
	if err != nil {
		return nil, err
	}
	updates := []ObjectMutation{{Base: base, Next: next}}
	b.dependency(base, 0)
	if base.Kind() == model.ObjectTable || base.Kind() == model.ObjectMaterializedView {
		owned, err := ownedRelationObjects(ctx, snapshot, base, options.Descriptors)
		if err != nil {
			return nil, err
		}
		for _, child := range owned {
			nextChild, err := move(child)
			if err != nil {
				return nil, err
			}
			updates = append(updates, ObjectMutation{Base: child, Next: nextChild})
			b.dependency(child, 0)
		}
	}
	if base.Kind() == model.ObjectTable || base.Kind() == model.ObjectView || base.Kind() == model.ObjectMaterializedView {
		typeUpdates, typeErr := updateImplicitRelationRowTypes(ctx, snapshot, next, options.Descriptors)
		if typeErr != nil {
			return nil, typeErr
		}
		updates = append(updates, typeUpdates...)
	}
	b.dependency(destination.Descriptor, 0)
	return &BoundObjectCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: objectCommandTag("ALTER", statement.ObjectType)}, ctx.Err()
}

func alterObjectParts(relation *zpgparse.RangeVar, object *zpgparse.Node) ([]string, error) {
	if relation != nil {
		parts := []string{relation.Relname}
		if relation.Schemaname != "" {
			parts = []string{relation.Schemaname, relation.Relname}
		}
		return parts, nil
	}
	if object != nil {
		return objectNodeParts(object)
	}
	return nil, bindError("42601", "invalid object name", 0)
}

func bindCommentObject(ctx context.Context, statement *zpgparse.CommentStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundObjectCommand, error) {
	if statement == nil || statement.Object == nil {
		return nil, bindError("42601", "invalid COMMENT ON", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CommentStmt{CommentStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := objectNodeParts(statement.Object)
	if err != nil {
		return nil, err
	}
	columnName := ""
	lookup := parts
	if statement.Objtype == zpgparse.ObjectType_OBJECT_COLUMN {
		if len(parts) < 2 || len(parts) > 3 {
			return nil, bindError("42601", "column comment requires relation and column", 0)
		}
		columnName = parts[len(parts)-1]
		lookup = parts[:len(parts)-1]
	}
	base, err := resolveOrdinaryObject(ctx, b, lookup, objectKinds(statement.Objtype), "42704")
	if err != nil {
		return nil, err
	}
	next, err := nextObjectDescriptor(ctx, base, options, func(spec *model.DescriptorSpec) error {
		if columnName == "" {
			spec.Description = statement.Comment
			return nil
		}
		comment := func(columns []model.ColumnDescriptor) error {
			index := slices.IndexFunc(columns, func(column model.ColumnDescriptor) bool { return column.Name == columnName && !column.SystemIdentity })
			if index < 0 {
				return bindError("42703", "column does not exist", 0)
			}
			columns[index].Description = statement.Comment
			return nil
		}
		if spec.Table != nil {
			if err := comment(spec.Table.Columns); err != nil {
				return err
			}
		}
		if spec.View != nil {
			if err := comment(spec.View.Columns); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	b.dependency(base, 0)
	updates := []ObjectMutation{{Base: base, Next: next}}
	if columnName != "" {
		typeUpdates, typeErr := updateImplicitRelationRowTypes(ctx, snapshot, next, options.Descriptors)
		if typeErr != nil {
			return nil, typeErr
		}
		updates = append(updates, typeUpdates...)
	}
	return &BoundObjectCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: "COMMENT"}, ctx.Err()
}

func objectNodeParts(node *zpgparse.Node) ([]string, error) {
	if node == nil {
		return nil, bindError("42601", "missing object name", 0)
	}
	if list := node.GetList(); list != nil {
		return names(list.Items)
	}
	if value := node.GetString_(); value != nil {
		return []string{value.Sval}, nil
	}
	return nil, bindError("42601", "invalid object name", 0)
}

func descriptorDependencies(dependencies []model.Dependency, self model.ObjectID) []model.Dependency {
	return slices.DeleteFunc(slices.Clone(dependencies), func(dependency model.Dependency) bool {
		return dependency.Object == self
	})
}

func nextObjectDescriptor(ctx context.Context, base *model.Descriptor, options CreateDeclarationOptions, mutate func(*model.DescriptorSpec) error) (*model.Descriptor, error) {
	if base == nil || base.Version() == math.MaxUint64 {
		return nil, model.ErrInvalidDescriptor
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	spec.Version++
	if err := mutate(&spec); err != nil {
		return nil, err
	}
	return model.NewDescriptor(ctx, spec, options.Descriptors)
}

func temporaryDescriptor(ctx context.Context, descriptor *model.Descriptor, limits model.DescriptorLimits) (bool, error) {
	if descriptor == nil {
		return false, model.ErrInvalidDescriptor
	}
	spec, err := descriptor.SpecWithin(ctx, limits)
	if err != nil {
		return false, err
	}
	return spec.Table != nil && spec.Table.Persistence == "t" ||
		spec.View != nil && spec.View.Persistence == "t" ||
		spec.Sequence != nil && spec.Sequence.Persistence == "t", nil
}

func implicitRelationTypeNamesAvailable(ctx context.Context, snapshot CatalogSnapshot, parent model.ObjectID, name string, allowed ...model.ObjectID) error {
	for _, typeName := range []string{name, arrayTypeName(name)} {
		published, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: parent, Kind: model.ObjectType, Name: typeName})
		if err == nil {
			if slices.Contains(allowed, published.Descriptor.ID()) {
				continue
			}
			return bindError("42710", "relation row type name already exists", 0)
		}
		if !isMissingObject(err) {
			return err
		}
	}
	return nil
}

func updateImplicitRelationRowTypes(ctx context.Context, snapshot CatalogSnapshot, relation *model.Descriptor, limits model.DescriptorLimits) ([]ObjectMutation, error) {
	types, err := implicitRelationTypes(ctx, snapshot, relation, limits)
	if err != nil {
		return nil, err
	}
	rowType, arrayType, err := model.NextImplicitRelationRowTypes(ctx, relation, types[0], types[1], arrayTypeName(relation.Name().Name), limits)
	if err != nil {
		return nil, err
	}
	return []ObjectMutation{{Base: types[0], Next: rowType}, {Base: types[1], Next: arrayType}}, nil
}
