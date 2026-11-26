package bind

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// DeclarationIdentityAllocator reserves identities in the caller's statement.
// Binding never publishes; the caller owns rollback of reservations on failure.
type DeclarationIdentityAllocator interface {
	AllocateDeclarationIdentity(context.Context) (model.ObjectID, model.OID, error)
}

type CreateDeclarationOptions struct {
	Bind                    Options
	Descriptors             model.DescriptorLimits
	Identities              DeclarationIdentityAllocator
	Owner                   model.OID
	Physical                model.PhysicalIdentity
	Encoder                 storage.KeyEncoder
	SchemaContext           string
	CurrentSetting          func(string) (string, error)
	IdentitySequenceAltered bool
}

// BoundDeclaration owns immutable descriptors. Existing denotes an IF NOT EXISTS
// outcome, in which case Descriptors is empty. Base pins the descriptor replaced
// by a declaration whose sole descriptor is its next version.
type BoundDeclaration struct {
	descriptors         []*model.Descriptor
	existing            *model.Descriptor
	base                *model.Descriptor
	updates             []ObjectMutation
	dependencies        []model.Dependency
	warnings            []model.Error
	pending             *pendingTableDeclaration
	sequences           []*BoundSequenceDefinition
	validations         []model.ObjectID
	partitionValidation bool
	identity            *pendingIdentityDeclaration
}

func (d *BoundDeclaration) RequiresPartitionValidation() bool {
	return d != nil && d.partitionValidation
}

func (d *BoundDeclaration) Descriptors() []*model.Descriptor { return slices.Clone(d.descriptors) }
func (d *BoundDeclaration) Existing() *model.Descriptor      { return d.existing }
func (d *BoundDeclaration) Base() *model.Descriptor          { return d.base }
func (d *BoundDeclaration) Updates() []ObjectMutation        { return slices.Clone(d.updates) }
func (d *BoundDeclaration) Dependencies() []model.Dependency { return slices.Clone(d.dependencies) }
func (d *BoundDeclaration) Validations() []model.ObjectID    { return slices.Clone(d.validations) }
func (d *BoundDeclaration) Warnings() []model.Error          { return slices.Clone(d.warnings) }

func declarationBinder(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, node *zpgparse.Node) (*binder, error) {
	return declarationBinderOptions(ctx, snapshot, options, node, false)
}

// declarationBinderDeclaredTypes binds a declaration whose type names may
// include a pseudo-type identity. PostgreSQL permits that in exactly two
// declaration positions: a routine parameter or result list, and an aggregate's
// argument, state, or result type. Operand, column, and cast layouts stay
// strict, so a pseudo-type can never acquire a storage class through a
// declaration that has no polymorphic meaning.
func declarationBinderDeclaredTypes(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, node *zpgparse.Node) (*binder, error) {
	return declarationBinderOptions(ctx, snapshot, options, node, true)
}

func declarationBinderOptions(ctx context.Context, snapshot CatalogSnapshot, options CreateDeclarationOptions, node *zpgparse.Node, declaredTypes bool) (*binder, error) {
	if err := options.Descriptors.Validate(); err != nil {
		return nil, err
	}
	if snapshot == nil || options.Identities == nil || options.Owner == 0 {
		return nil, bindError("22023", "declaration binding requires catalog, identity allocation, and owner", 0)
	}
	bindOptions := options.Bind
	bindOptions.allowPseudoTypes = declaredTypes
	b, err := newExpressionBinder(ctx, snapshot, bindOptions)
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

func declarationName(name string) error {
	if name == "" || len(name) > 63 || !utf8.ValidString(name) || strings.IndexByte(name, 0) >= 0 {
		return bindError("42622", "invalid declaration identifier", 0)
	}
	return nil
}

func BindCreateSchema(ctx context.Context, statement *zpgparse.CreateSchemaStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundDeclaration, error) {
	if statement == nil {
		return nil, bindError("42601", "missing CREATE SCHEMA", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreateSchemaStmt{CreateSchemaStmt: statement}})
	if err != nil {
		return nil, err
	}
	if statement.Authrole != nil {
		return nil, missing("CREATE SCHEMA AUTHORIZATION", 0)
	}
	if len(statement.SchemaElts) != 0 {
		return nil, missing("CREATE SCHEMA embedded declarations", 0)
	}
	if err := declarationName(statement.Schemaname); err != nil {
		return nil, err
	}
	name := model.NamespaceName{Kind: model.ObjectSchema, Name: statement.Schemaname}
	existing, err := snapshot.Resolve(ctx, name)
	if err == nil {
		if !statement.IfNotExists {
			return nil, bindError("42P06", "schema already exists", 0)
		}
		b.dependency(existing.Descriptor, 0)
		return &BoundDeclaration{existing: existing.Descriptor, dependencies: b.dependencies, warnings: []model.Error{{Code: "42P06", Message: "schema already exists, skipping"}}}, ctx.Err()
	}
	if !isMissingObject(err) {
		return nil, err
	}
	id, oid, err := options.Identities.AllocateDeclarationIdentity(ctx)
	if err != nil {
		return nil, err
	}
	d, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: id, OID: oid, Name: name, Version: 1, State: model.StatePublic, Schema: &model.SchemaMetadata{Owner: options.Owner}}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundDeclaration{descriptors: []*model.Descriptor{d}}, ctx.Err()
}

type tableConstraint struct {
	raw    *zpgparse.Constraint
	column model.ColumnID
	// deferrability and timing record which deferrability attributes have already
	// been folded into raw, so a repeat of either axis is rejected.
	deferrability, timing bool
}

func BindCreateTable(ctx context.Context, statement *zpgparse.CreateStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundDeclaration, error) {
	if statement == nil || statement.Relation == nil {
		return nil, bindError("42601", "missing CREATE TABLE relation", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_CreateStmt{CreateStmt: statement}})
	if err != nil {
		return nil, err
	}
	if err := ordinaryTable(statement); err != nil {
		return nil, err
	}
	statement = proto.Clone(statement).(*zpgparse.CreateStmt)
	options.Bind.SearchPath = slices.Clone(options.Bind.SearchPath)
	r := statement.Relation
	if err := declarationName(r.Relname); err != nil {
		return nil, err
	}
	path := options.Bind.SearchPath
	if r.Schemaname != "" {
		path = []string{r.Schemaname}
	}
	var parent *model.Descriptor
	for _, name := range path {
		p, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: name})
		if isMissingObject(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if p.Descriptor.State() == model.StatePublic {
			parent = p.Descriptor
			break
		}
	}
	if parent == nil {
		return nil, bindError("3F000", "no schema selected for creation", 0)
	}
	b.dependency(parent, 0)
	existing, err := b.indexNameConflict(parent.ID(), r.Relname)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !statement.IfNotExists {
			return nil, bindError("42P07", "relation already exists", 0)
		}
		b.dependency(existing, 0)
		return &BoundDeclaration{existing: existing, dependencies: b.dependencies, warnings: []model.Error{{Code: "42P07", Message: "relation already exists, skipping"}}}, ctx.Err()
	}
	if len(statement.TableElts) > options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	meta := &model.TableMetadata{Owner: options.Owner, Persistence: "p"}
	if statement.Relation.Relpersistence == "t" {
		meta.Persistence = "t"
	}
	switch statement.Oncommit {
	case zpgparse.OnCommitAction_ON_COMMIT_ACTION_UNDEFINED, zpgparse.OnCommitAction_ONCOMMIT_NOOP:
		if meta.Persistence == "t" {
			meta.OnCommit = "preserve"
		}
	case zpgparse.OnCommitAction_ONCOMMIT_PRESERVE_ROWS:
		if meta.Persistence != "t" {
			return nil, bindError("42P16", "ON COMMIT can only be used on temporary tables", 0)
		}
		meta.OnCommit = "preserve"
	case zpgparse.OnCommitAction_ONCOMMIT_DELETE_ROWS:
		if meta.Persistence != "t" {
			return nil, bindError("42P16", "ON COMMIT can only be used on temporary tables", 0)
		}
		meta.OnCommit = "delete"
	case zpgparse.OnCommitAction_ONCOMMIT_DROP:
		if meta.Persistence != "t" {
			return nil, bindError("42P16", "ON COMMIT can only be used on temporary tables", 0)
		}
		meta.OnCommit = "drop"
	default:
		return nil, bindError("42601", "invalid ON COMMIT action", 0)
	}
	var constraints []tableConstraint
	columnNames := make(map[string]model.ColumnID)
	serials := make(map[model.ColumnID]bool)
	typedOptions := make(map[model.ColumnID]bool)
	like := &tableLikePlan{}
	if statement.OfTypename != nil {
		parts, err := names(statement.OfTypename.Names)
		if err != nil {
			return nil, err
		}
		typ, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectType}, "42704", int(statement.OfTypename.Location)+1)
		if err != nil {
			return nil, err
		}
		typeSpec, err := typ.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if typeSpec.Type == nil || typeSpec.Type.DeclaredKind() != model.TypeKindComposite {
			return nil, bindError("42809", "typed table requires a composite type", int(statement.OfTypename.Location)+1)
		}
		if typeSpec.Type.Relation == 0 {
			return nil, bindError("42809", "typed table requires a standalone composite type", int(statement.OfTypename.Location)+1)
		}
		relation, err := snapshot.Descriptor(ctx, typeSpec.Type.Relation)
		if err != nil {
			return nil, err
		}
		if relation.Descriptor.Kind() != model.ObjectCompositeRelation {
			return nil, bindError("42809", "typed table requires a standalone composite type", int(statement.OfTypename.Location)+1)
		}
		b.dependency(relation.Descriptor, 0)
		meta.TypedOf = typ.ID()
		meta.Columns = make([]model.ColumnDescriptor, len(typeSpec.Type.Fields))
		meta.TypedColumns = make([]model.TypedColumnMapping, len(typeSpec.Type.Fields))
		for i, field := range typeSpec.Type.Fields {
			columnID := model.ColumnID(i + 1)
			column := field
			column.ID = columnID
			meta.Columns[i] = column
			meta.TypedColumns[i] = model.TypedColumnMapping{Field: field.ID, Column: columnID}
			meta.ColumnIDHighWatermark = columnID
			columnNames[column.Name] = columnID
		}
	}
	for _, node := range statement.TableElts {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if c := node.GetConstraint(); c != nil {
			constraints = append(constraints, tableConstraint{raw: c})
			continue
		}
		if clause := node.GetTableLikeClause(); clause != nil {
			if statement.Partbound != nil {
				return nil, bindError("42601", "PARTITION OF does not accept LIKE clauses", int(clause.Relation.Location)+1)
			}
			if meta.TypedOf != 0 {
				return nil, bindError("42P16", "typed tables do not accept LIKE clauses", int(clause.Relation.Location)+1)
			}
			if err := b.appendTableLike(ctx, clause, snapshot, options, meta, columnNames, like); err != nil {
				return nil, err
			}
			continue
		}
		c := node.GetColumnDef()
		if c == nil {
			return nil, bindError("42601", "invalid CREATE TABLE element", 0)
		}
		if statement.Partbound != nil {
			return nil, bindError("42601", "PARTITION OF does not accept column definitions", int(c.Location)+1)
		}
		if meta.TypedOf != 0 {
			id, found := columnNames[c.Colname]
			if !found || c.TypeName != nil || c.CollClause != nil {
				return nil, bindError("42703", "typed table column options require an attribute of the composite type", int(c.Location)+1)
			}
			if typedOptions[id] {
				return nil, bindError("42701", "typed table column options specified more than once", int(c.Location)+1)
			}
			typedOptions[id] = true
			if err := ordinaryColumn(c); err != nil {
				return nil, err
			}
			constraints, err = appendColumnConstraints(constraints, c.Constraints, id, options.Descriptors.MaxItems)
			if err != nil {
				return nil, err
			}
			continue
		}
		if err := ordinaryColumn(c); err != nil {
			return nil, err
		}
		if err := declarationName(c.Colname); err != nil {
			return nil, err
		}
		if _, found := columnNames[c.Colname]; found {
			return nil, bindError("42701", "column specified more than once", int(c.Location)+1)
		}
		layout, serial, err := b.declarationColumnType(c.TypeName)
		if err != nil {
			return nil, err
		}
		if c.CollClause != nil {
			layout, err = b.indexColumnCollation(layout, c.CollClause.Collname)
			if err != nil {
				return nil, err
			}
		}
		meta.ColumnIDHighWatermark++
		id := meta.ColumnIDHighWatermark
		serials[id] = serial
		columnNames[c.Colname] = id
		meta.Columns = append(meta.Columns, model.ColumnDescriptor{ID: id, Name: c.Colname, State: model.StatePublic, Layout: layout, Nullable: true})
		constraints, err = appendColumnConstraints(constraints, c.Constraints, id, options.Descriptors.MaxItems)
		if err != nil {
			return nil, err
		}
	}
	for i := range meta.Columns {
		meta.Columns[i].Local = true
	}
	inheritance, err := b.prepareTableInheritance(ctx, statement, snapshot, options, meta, columnNames)
	if err != nil {
		return nil, err
	}
	if inheritance != nil && meta.TypedOf != 0 {
		return nil, bindError("42P16", "typed tables cannot participate in inheritance", 0)
	}
	if len(constraints) > options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	state := declarationState{b: b, options: options, parent: parent.ID(), tableName: r.Relname, names: make(map[string]bool), reserved: make(map[string]bool), ids: make(map[model.ObjectID]bool), oids: make(map[model.OID]bool)}
	for _, item := range constraints {
		if name := item.raw.Conname; name != "" {
			if err := declarationName(name); err != nil {
				return nil, err
			}
			if state.reserved[name] {
				return nil, bindError("42710", "constraint name specified more than once", 0)
			}
			state.reserved[name] = true
		}
	}
	for _, typeName := range []string{r.Relname, arrayTypeName(r.Relname)} {
		existingType, resolveErr := snapshot.Resolve(ctx, model.NamespaceName{Parent: parent.ID(), Kind: model.ObjectType, Name: typeName})
		if resolveErr == nil {
			b.dependency(existingType.Descriptor, 0)
			return nil, bindError("42710", "relation row type name already exists", 0)
		}
		if !isMissingObject(resolveErr) {
			return nil, resolveErr
		}
	}
	id, oid, err := state.allocate()
	if err != nil {
		return nil, err
	}
	rowTypeID, rowTypeOID, err := state.allocate()
	if err != nil {
		return nil, err
	}
	arrayTypeID, arrayTypeOID, err := state.allocate()
	if err != nil {
		return nil, err
	}
	meta.RowType = rowTypeID
	spec := model.DescriptorSpec{ID: id, OID: oid, Name: model.NamespaceName{Parent: parent.ID(), Kind: model.ObjectTable, Name: r.Relname}, Version: 1, State: model.StatePublic, Physical: options.Physical, Table: meta}
	if err := state.installTableLikeMetadata(ctx, &spec, like, snapshot); err != nil {
		return nil, err
	}
	defaults := make(map[model.ColumnID]*zpgparse.Node)
	generated := make(map[model.ColumnID]generatedColumnDefinition)
	nulls := make(map[model.ColumnID]bool)
	sequences, constraints, err := state.ownedSequences(spec, serials, constraints, defaults)
	if err != nil {
		return nil, err
	}
	likeSequences, err := state.tableLikeIdentitySequences(ctx, spec, like, snapshot)
	if err != nil {
		return nil, err
	}
	sequences = append(sequences, likeSequences...)
	var checks []tableConstraint
	var checkIndexes []int
	var indexes []*model.Descriptor
	var relational []tableConstraint
	for _, item := range constraints {
		c := item.raw
		if err := ordinaryConstraint(c); err != nil {
			return nil, err
		}
		switch c.Contype {
		case zpgparse.ConstrType_CONSTR_IDENTITY:
			continue
		case zpgparse.ConstrType_CONSTR_DEFAULT, zpgparse.ConstrType_CONSTR_GENERATED:
			if item.column == 0 || c.RawExpr == nil {
				return nil, bindError("42601", "column expression requires a column", 0)
			}
			column, found := declarationColumn(meta.Columns, item.column)
			_, hasGenerated := generated[item.column]
			if !found || defaults[item.column] != nil || hasGenerated || column.IdentitySequence != 0 {
				return nil, bindError("42601", "multiple default or generation clauses", 0)
			}
			if c.Contype == zpgparse.ConstrType_CONSTR_DEFAULT {
				defaults[item.column] = c.RawExpr
				continue
			}
			stored := false
			switch c.GeneratedKind {
			case "s":
				stored = true
			case "v":
			default:
				return nil, bindError("42601", "invalid generated column kind", int(c.Location)+1)
			}
			generated[item.column] = generatedColumnDefinition{raw: c.RawExpr, stored: stored}
			continue
		case zpgparse.ConstrType_CONSTR_NULL:
			if item.column == 0 {
				return nil, bindError("42601", "NULL requires a column", 0)
			}
			if prior, ok := nulls[item.column]; ok && !prior {
				return nil, bindError("42601", "conflicting NULL and NOT NULL", 0)
			}
			nulls[item.column] = true
			continue
		case zpgparse.ConstrType_CONSTR_NOTNULL:
			if item.column == 0 {
				columns, err := declarationColumns(c.Keys, 0, columnNames)
				if err != nil {
					return nil, err
				}
				if len(columns) != 1 {
					return nil, bindError("42601", "NOT NULL requires one column", 0)
				}
				item.column = columns[0]
			}
			if prior, ok := nulls[item.column]; ok && prior {
				return nil, bindError("42601", "conflicting NULL and NOT NULL", 0)
			}
			nulls[item.column] = false
			column, found := declarationColumn(meta.Columns, item.column)
			if !found {
				return nil, model.ErrInvalidDescriptor
			}
			column.Nullable = false
		case zpgparse.ConstrType_CONSTR_CHECK, zpgparse.ConstrType_CONSTR_PRIMARY, zpgparse.ConstrType_CONSTR_UNIQUE:
		case zpgparse.ConstrType_CONSTR_FOREIGN, zpgparse.ConstrType_CONSTR_EXCLUSION:
			relational = append(relational, item)
			continue
		default:
			return nil, missing("CREATE TABLE constraint "+c.Contype.String(), int(c.Location)+1)
		}
		constraintKind := model.ConstraintCheck
		if c.Contype == zpgparse.ConstrType_CONSTR_NOTNULL {
			constraintKind = model.ConstraintNotNull
		} else if c.Contype == zpgparse.ConstrType_CONSTR_PRIMARY {
			constraintKind = model.ConstraintPrimaryKey
		} else if c.Contype == zpgparse.ConstrType_CONSTR_UNIQUE {
			constraintKind = model.ConstraintUnique
		}
		constraint, err := declarationConstraintAttributes(c, constraintKind)
		if err != nil {
			return nil, err
		}
		label := "check"
		switch c.Contype {
		case zpgparse.ConstrType_CONSTR_CHECK:
		case zpgparse.ConstrType_CONSTR_NOTNULL:
			constraint.Kind, label = model.ConstraintNotNull, "not_null"
			constraint.Columns = []model.ColumnID{item.column}
		case zpgparse.ConstrType_CONSTR_PRIMARY, zpgparse.ConstrType_CONSTR_UNIQUE:
			constraint.Kind, label = model.ConstraintUnique, "key"
			if c.Contype == zpgparse.ConstrType_CONSTR_PRIMARY {
				constraint.Kind, label = model.ConstraintPrimaryKey, "pkey"
				if len(meta.PrimaryKey) != 0 {
					return nil, bindError("42P16", "multiple primary keys", 0)
				}
			}
			constraint.Columns, err = declarationColumns(c.Keys, item.column, columnNames)
			if err != nil {
				return nil, err
			}
		}
		addition := ""
		if item.column != 0 && label != "pkey" {
			column, found := declarationColumn(meta.Columns, item.column)
			if !found {
				return nil, model.ErrInvalidDescriptor
			}
			addition = column.Name
		}
		if len(constraint.Columns) > 0 && label != "pkey" {
			addition = declarationKeyName(meta.Columns, constraint.Columns)
		}
		constraint.Name, err = state.constraintName(c.Conname, addition, label, constraint.Kind == model.ConstraintPrimaryKey || constraint.Kind == model.ConstraintUnique)
		if err != nil {
			return nil, err
		}
		constraint.ID, constraint.OID, err = state.allocate()
		if err != nil {
			return nil, err
		}
		if constraint.Kind == model.ConstraintPrimaryKey || constraint.Kind == model.ConstraintUnique {
			indexID, indexOID, err := state.allocate()
			if err != nil {
				return nil, err
			}
			index := &model.IndexMetadata{Owner: options.Owner, Table: id, Unique: true, NullsNotDistinct: c.NullsNotDistinct}
			deps := []model.Dependency{{Object: id, Version: 1}, {Object: parent.ID(), Version: parent.Version()}}
			for _, column := range constraint.Columns {
				key, err := declarationKey(meta.Columns, nulls, column, constraint.Kind == model.ConstraintPrimaryKey)
				if err != nil {
					return nil, err
				}
				index.Keys = append(index.Keys, key)
				deps = append(deps, model.Dependency{Object: id, Version: 1, Column: column})
			}
			if len(c.Including) > 0 {
				index.Include, err = declarationColumns(c.Including, 0, columnNames)
				if err != nil {
					return nil, err
				}
				for _, column := range index.Include {
					deps = appendIndexDependency(deps, model.Dependency{Object: id, Version: 1, Column: column})
				}
			}
			if constraint.Kind == model.ConstraintPrimaryKey && !constraint.Deferrable {
				meta.PrimaryKey = slices.Clone(index.Keys)
			}
			descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: indexID, OID: indexOID, Name: model.NamespaceName{Parent: parent.ID(), Kind: model.ObjectIndex, Name: constraint.Name}, Version: 1, State: model.StatePublic, Physical: options.Physical, Dependencies: deps, Index: index}, options.Descriptors)
			if err != nil {
				return nil, err
			}
			indexes = append(indexes, descriptor)
			constraint.Index = indexID
			meta.Indexes = append(meta.Indexes, indexID)
		}
		if constraint.Kind == model.ConstraintCheck {
			checks = append(checks, item)
			checkIndexes = append(checkIndexes, len(meta.Constraints))
		}
		meta.Constraints = append(meta.Constraints, constraint)
	}
	if err := state.installInheritedConstraints(meta, inheritance); err != nil {
		return nil, err
	}
	localExpressions := make(map[model.ColumnID]bool, len(defaults)+len(generated))
	for column := range defaults {
		localExpressions[column] = true
	}
	for column := range generated {
		localExpressions[column] = true
	}
	if err := state.installInheritedDefaultOIDs(meta, inheritance, localExpressions); err != nil {
		return nil, err
	}
	relationalIndexes, err := state.bindRelationalConstraints(ctx, &spec, relational, columnNames)
	if err != nil {
		return nil, err
	}
	indexes = append(indexes, relationalIndexes...)
	for _, key := range meta.PrimaryKey {
		if nullable, explicit := nulls[key.Column]; explicit && nullable {
			return nil, bindError("42601", "conflicting NULL and primary key", 0)
		}
	}
	if len(meta.PrimaryKey) == 0 {
		if len(meta.Columns) >= options.Descriptors.MaxItems {
			return nil, model.ErrDescriptorLimit
		}
		name := "__supersql_row_identity"
		for suffix := 1; columnNames[name] != 0; suffix++ {
			name = "__supersql_row_identity_" + strconv.Itoa(suffix)
		}
		column := meta.ColumnIDHighWatermark + 1
		layout := model.TypeLayout{Type: model.Type{ID: 17, Storage: model.TypeBytes, Modifier: -1}}
		meta.Columns = append(meta.Columns, model.ColumnDescriptor{ID: column, Name: name, State: model.StatePublic, Layout: layout, SystemIdentity: true})
		meta.ColumnIDHighWatermark = column
		meta.PrimaryKey = []model.KeyElement{{Column: column, Type: layout.Type}}
	}
	draft := &pendingTableDeclaration{spec: spec, defaults: defaults, generated: generated, checks: checks, checkIndexes: checkIndexes, indexes: indexes, inheritance: inheritance, like: like, partitionSpec: statement.Partspec, partitionBound: statement.Partbound, options: options, rowTypeID: rowTypeID, rowTypeOID: rowTypeOID, arrayTypeID: arrayTypeID, arrayTypeOID: arrayTypeOID, ids: state.ids, dependencies: b.dependencies, warnings: b.warnings}
	if len(sequences) != 0 {
		return &BoundDeclaration{pending: draft, sequences: sequences, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
	}
	return draft.compile(ctx, snapshot, nil)
}

func (d *pendingTableDeclaration) compile(ctx context.Context, snapshot CatalogSnapshot, sequences []*model.Descriptor) (*BoundDeclaration, error) {
	options := d.options
	b, err := newExpressionBinder(ctx, snapshot, options.Bind)
	if err != nil {
		return nil, err
	}
	b.dependencies = slices.Clone(d.dependencies)
	if d.inheritance != nil {
		b.dependencies = futureInheritanceDependencies(b.dependencies, d.inheritance.parents)
	}
	for _, dep := range b.dependencies {
		b.dependencySet[dep] = struct{}{}
	}
	b.warnings = slices.Clone(d.warnings)
	for _, sequence := range sequences {
		b.dependency(sequence, 0)
	}
	spec := d.spec
	metadata := *spec.Table
	meta := &metadata
	meta.Columns = slices.Clone(meta.Columns)
	meta.Constraints = slices.Clone(meta.Constraints)
	spec.Table = meta
	defaults, generated, checks, checkIndexes, indexes := d.defaults, d.generated, d.checks, d.checkIndexes, d.indexes
	// Pending expression binding sees every declared type, but no incomplete
	// constraints or generated definitions are admitted into the descriptor.
	pendingMeta := *meta
	pendingMeta.Columns = slices.Clone(meta.Columns)
	for i := range pendingMeta.Columns {
		if pendingMeta.Columns[i].Generated == nil {
			pendingMeta.Columns[i].GeneratedStored = false
		}
	}
	pendingMeta.Constraints = nil
	pendingSpec := spec
	pendingSpec.Table = &pendingMeta
	pendingSpec.Dependencies = b.dependencies
	pending, err := model.NewDescriptor(ctx, pendingSpec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	options.Bind.declaration = &declarationScope{objects: append([]*model.Descriptor{pending}, sequences...)}
	remaining := options.Descriptors.MaxBytes
	if err := rebindTableLikeExpressions(ctx, pending, meta, d.like, snapshot, options, &remaining, &b.dependencies); err != nil {
		return nil, err
	}
	rowColumns := slices.Clone(meta.Columns)
	for column := range generated {
		if generatedColumn, found := declarationColumn(rowColumns, column); found {
			generatedColumn.GeneratedStored = true
		}
	}
	for i := range meta.Columns {
		col := &meta.Columns[i]
		_, hasGenerated := generated[col.ID]
		if defaults[col.ID] == nil && !hasGenerated && col.ConstantDefault == nil && col.Default == nil && col.Generated == nil {
			domain, registered := b.options.CatalogRelations.InformationDomainByType(col.Layout.Type.ID)
			if registered && domain.Definition.DefaultSQL != "" {
				raw, err := b.defaultExpression(domain.Definition.DefaultSQL, 0)
				if err != nil {
					return nil, err
				}
				bound, err := defineDefault(ctx, raw, col.Layout, snapshot, options.Bind, true)
				if err != nil {
					return nil, err
				}
				col.Default = &bound.Definition
				b.warnings = append(b.warnings, bound.Warnings...)
			} else {
				definition, found, err := b.domainDefault(col.Layout)
				if err != nil {
					return nil, err
				}
				if found {
					col.Default = definition
				}
			}
		}
		if raw := defaults[col.ID]; raw != nil {
			bound, err := defineDefault(ctx, raw, col.Layout, snapshot, options.Bind, true)
			if err != nil {
				return nil, err
			}
			col.Default = &bound.Definition
			b.warnings = append(b.warnings, bound.Warnings...)
		}
		if generated, found := generated[col.ID]; found {
			definition, _, err := defineRowExpression(ctx, generated.raw, pending, rowColumns, snapshot, options.Bind, rowStoredGenerated, col.ID, &col.Layout, 0)
			if err != nil {
				return nil, err
			}
			col.Generated, col.GeneratedStored = definition, generated.stored
		}
		for _, definition := range []*model.ExpressionDefinition{col.Default, col.Generated} {
			if definition == nil {
				continue
			}
			if err := admitIndexDefinition(definition, &remaining); err != nil {
				return nil, err
			}
			for _, dep := range definition.Dependencies {
				b.dependencies = appendIndexDependency(b.dependencies, dep)
			}
		}
		if (col.Default != nil || col.Generated != nil) && col.DefaultOID == 0 {
			_, defaultOID, err := options.Identities.AllocateDeclarationIdentity(ctx)
			if err != nil {
				return nil, err
			}
			col.DefaultOID = defaultOID
		}
	}
	if d.partitionSpec != nil {
		strategy, keys, err := b.bindPartitionKeys(ctx, d.partitionSpec, pending, meta.Columns, snapshot, options)
		if err != nil {
			return nil, err
		}
		meta.PartitionStrategy = strategy
		meta.PartitionKeys = keys
	}
	if d.partitionBound != nil {
		if d.inheritance == nil || d.inheritance.partitionParent == nil {
			return nil, model.ErrInvalidDescriptor
		}
		parentMeta, ok := d.inheritance.partitionParent.Table()
		if !ok {
			return nil, model.ErrInvalidDescriptor
		}
		bound, err := b.bindPartitionBound(ctx, d.partitionBound, d.inheritance.partitionParent, &parentMeta)
		if err != nil {
			return nil, err
		}
		meta.PartitionBound = bound
		meta.PartitionState = model.PartitionAttached
	}
	likeIndexes, err := cloneTableLikeIndexes(ctx, pending, meta, d.like, snapshot, options, &remaining, &b.dependencies)
	if err != nil {
		return nil, err
	}
	indexes = append(indexes, likeIndexes...)
	if err := validatePartitionUniqueIndexes(ctx, meta, indexes, options.Descriptors); err != nil {
		return nil, err
	}
	if err := rebindInheritedExpressions(ctx, pending, meta, d.inheritance, snapshot, options); err != nil {
		return nil, err
	}
	partitionIndexes, indexUpdates, err := clonePartitionIndexes(ctx, b, pending, meta, d.inheritance, snapshot, options, d.ids, nil)
	if err != nil {
		return nil, err
	}
	if len(indexUpdates) != 0 {
		return nil, model.ErrInvalidDescriptor
	}
	indexes = append(indexes, partitionIndexes...)
	for i, item := range checks {
		constraint := &meta.Constraints[checkIndexes[i]]
		definition, err := DefineCheck(ctx, item.raw.RawExpr, pending, constraint.ID, snapshot, options.Bind)
		if err != nil {
			return nil, err
		}
		if err := admitIndexDefinition(definition, &remaining); err != nil {
			return nil, err
		}
		constraint.Expression = definition
		for _, dep := range definition.Dependencies {
			b.dependencies = appendIndexDependency(b.dependencies, dep)
		}
	}
	spec.Dependencies = tableLikePersistentDependencies(b.dependencies, d.like)
	table, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if d.partitionBound != nil {
		if err := b.validatePartitionAttachment(ctx, d.inheritance.partitionParent, table, options); err != nil {
			return nil, err
		}
	}
	updates, err := inheritanceParentUpdates(ctx, d.inheritance, table, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if d.partitionBound != nil {
		promotions, err := b.promoteAttachedTableIndexes(ctx, d.inheritance.partitionParent, table, indexes, nil, snapshot, options)
		if err != nil {
			return nil, err
		}
		updates = append(updates, promotions...)
	}
	rowType, arrayType, err := model.NewImplicitRelationRowTypes(ctx, table, d.rowTypeID, d.rowTypeOID, d.arrayTypeID, d.arrayTypeOID, arrayTypeName(table.Name().Name), options.Owner, options.Descriptors)
	if err != nil {
		return nil, err
	}
	deps := make([]model.Dependency, 0, len(b.dependencies))
	for _, dep := range b.dependencies {
		if !d.ids[dep.Object] {
			deps = append(deps, currentInheritanceDependency(dep, d.inheritance))
		}
	}
	partitionValidation := false
	if d.partitionBound != nil && d.inheritance != nil && d.inheritance.partitionParent != nil {
		parent, ok := d.inheritance.partitionParent.Table()
		partitionValidation = ok && parent.PartitionDefault != 0 && meta.PartitionBound != nil && !meta.PartitionBound.Default
	}
	descriptors := append([]*model.Descriptor{table, rowType, arrayType}, indexes...)
	descriptors = append(descriptors, sequences...)
	return &BoundDeclaration{descriptors: descriptors, updates: updates, dependencies: deps, warnings: b.warnings, partitionValidation: partitionValidation}, ctx.Err()
}

func declarationColumn(columns []model.ColumnDescriptor, id model.ColumnID) (*model.ColumnDescriptor, bool) {
	index := slices.IndexFunc(columns, func(column model.ColumnDescriptor) bool { return column.ID == id })
	if index < 0 {
		return nil, false
	}
	return &columns[index], true
}

func declarationKeyName(columns []model.ColumnDescriptor, keys []model.ColumnID) string {
	var names strings.Builder
	for _, id := range keys {
		if names.Len() != 0 {
			names.WriteByte('_')
		}
		column, found := declarationColumn(columns, id)
		if !found {
			return ""
		}
		names.WriteString(column.Name)
		if names.Len() >= 63 {
			break
		}
	}
	return names.String()
}

func declarationKey(columns []model.ColumnDescriptor, nulls map[model.ColumnID]bool, id model.ColumnID, primary bool) (model.KeyElement, error) {
	col, found := declarationColumn(columns, id)
	if !found {
		return model.KeyElement{}, model.ErrInvalidDescriptor
	}
	if primary {
		if nullable, explicit := nulls[id]; explicit && nullable {
			return model.KeyElement{}, bindError("42601", "conflicting NULL and primary key", 0)
		}
		col.Nullable = false
	}
	return model.KeyElement{Column: id, Type: col.Layout.Type, Children: col.Layout.Children, Collation: col.Layout.Type.Collation}, nil
}

func declarationColumns(raw []*zpgparse.Node, inline model.ColumnID, columns map[string]model.ColumnID) ([]model.ColumnID, error) {
	if inline != 0 {
		if len(raw) != 0 {
			return nil, bindError("42601", "inline key has explicit columns", 0)
		}
		return []model.ColumnID{inline}, nil
	}
	if len(raw) == 0 {
		return nil, bindError("42601", "constraint requires columns", 0)
	}
	parts, err := names(raw)
	if err != nil {
		return nil, err
	}
	ids := make([]model.ColumnID, 0, len(parts))
	for _, name := range parts {
		id, ok := columns[name]
		if !ok {
			return nil, bindError("42703", "constraint column does not exist", 0)
		}
		if slices.Contains(ids, id) {
			return nil, bindError("42701", "constraint column specified more than once", 0)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

type declarationState struct {
	b         *binder
	options   CreateDeclarationOptions
	parent    model.ObjectID
	tableName string
	names     map[string]bool
	reserved  map[string]bool
	ids       map[model.ObjectID]bool
	oids      map[model.OID]bool
}

func (s *declarationState) allocate() (model.ObjectID, model.OID, error) {
	if err := s.b.ctx.Err(); err != nil {
		return 0, 0, err
	}
	id, oid, err := s.options.Identities.AllocateDeclarationIdentity(s.b.ctx)
	if err != nil {
		return 0, 0, err
	}
	if id == 0 || oid == 0 || s.ids[id] || s.oids[oid] || id == s.parent {
		return 0, 0, bindError("22023", "allocator returned invalid or repeated identity", 0)
	}
	s.ids[id], s.oids[oid] = true, true
	return id, oid, nil
}

func (s *declarationState) constraintName(explicit, addition, label string, relation bool) (string, error) {
	for attempt := uint32(0); attempt < s.b.options.Limits.MaxNodes; attempt++ {
		if err := s.b.ctx.Err(); err != nil {
			return "", err
		}
		name := explicit
		if name == "" {
			suffix := label
			if attempt > 0 {
				suffix += strconv.FormatUint(uint64(attempt), 10)
			}
			a, b := s.tableName, addition
			for len(a)+len(b)+len(suffix)+2 > 63 {
				if len(a) > len(b) {
					a = clipIndexIdentifier(a, len(a)-1)
				} else {
					b = clipIndexIdentifier(b, len(b)-1)
				}
			}
			name = a + "_" + suffix
			if b != "" {
				name = a + "_" + b + "_" + suffix
			}
		}
		if err := declarationName(name); err != nil {
			return "", err
		}
		conflict := s.names[name] || (explicit == "" && s.reserved[name]) || (relation && name == s.tableName)
		if relation && !conflict {
			existing, err := s.b.indexNameConflict(s.parent, name)
			if err != nil {
				return "", err
			}
			conflict = existing != nil
		}
		if !conflict {
			s.names[name] = true
			return name, nil
		}
		if explicit != "" {
			return "", bindError("42710", "constraint or relation name already exists", 0)
		}
	}
	return "", bindError("54000", "constraint name search limit exceeded", 0)
}

func ordinaryTable(s *zpgparse.CreateStmt) error {
	r := s.Relation
	if r.Catalogname != "" {
		return bindError("0A000", "cross-database reference is not supported", 0)
	}
	if r.Alias != nil || len(s.Constraints) != 0 || len(s.Nnconstraints) != 0 {
		return bindError("42601", "expected untransformed CREATE TABLE", 0)
	}
	if r.Relpersistence != "p" && r.Relpersistence != "" && r.Relpersistence != "t" {
		return missing("unlogged CREATE TABLE", 0)
	}
	if len(s.Options) != 0 || s.AccessMethod != "" || s.Tablespacename != "" {
		return missing("CREATE TABLE storage options", 0)
	}
	return nil
}

func ordinaryColumn(c *zpgparse.ColumnDef) error {
	transformed := c.Inhcount != 0 || c.IsNotNull || c.IsFromType || c.CollOid != 0
	defaulted := c.RawDefault != nil || c.CookedDefault != nil
	generated := c.Identity != "" || c.IdentitySequence != nil || c.Generated != ""
	if transformed || defaulted || generated {
		return bindError("42601", "expected untransformed column declaration", 0)
	}
	if c.Compression != "" || c.Storage != "" || c.StorageName != "" || len(c.Fdwoptions) != 0 {
		return missing("column storage or foreign options", 0)
	}
	return nil
}

func ordinaryConstraint(c *zpgparse.Constraint) error {
	if c == nil || c.CookedExpr != "" || c.ResetDefaultTblspc || len(c.OldConpfeqop) != 0 || c.OldPktableOid != 0 {
		return bindError("42601", "expected untransformed constraint", 0)
	}
	key := c.Contype == zpgparse.ConstrType_CONSTR_PRIMARY || c.Contype == zpgparse.ConstrType_CONSTR_UNIQUE
	foreign := c.Contype == zpgparse.ConstrType_CONSTR_FOREIGN
	exclusion := c.Contype == zpgparse.ConstrType_CONSTR_EXCLUSION
	expression := c.Contype == zpgparse.ConstrType_CONSTR_CHECK || c.Contype == zpgparse.ConstrType_CONSTR_DEFAULT || c.Contype == zpgparse.ConstrType_CONSTR_GENERATED
	wrongKeyOptions := !key && !exclusion && len(c.Including) != 0 || c.NullsNotDistinct && c.Contype != zpgparse.ConstrType_CONSTR_UNIQUE
	wrongExpression := !expression && c.RawExpr != nil
	wrongColumns := !key && c.Contype != zpgparse.ConstrType_CONSTR_NOTNULL && len(c.Keys) != 0
	foreignFields := c.Pktable != nil || len(c.FkAttrs) != 0 || len(c.PkAttrs) != 0 ||
		c.FkMatchtype != "" || c.FkUpdAction != "" || c.FkDelAction != "" || len(c.FkDelSetCols) != 0
	if wrongKeyOptions || wrongExpression || wrongColumns || foreignFields && !foreign || len(c.Exclusions) != 0 && !exclusion {
		return bindError("42601", "constraint fields do not match constraint kind", int(c.Location)+1)
	}
	if c.Contype == zpgparse.ConstrType_CONSTR_GENERATED && c.GeneratedWhen != "a" {
		return bindError("42601", "generated column requires ALWAYS", int(c.Location)+1)
	}
	generation := c.Contype == zpgparse.ConstrType_CONSTR_GENERATED || c.Contype == zpgparse.ConstrType_CONSTR_IDENTITY
	if !generation && (c.GeneratedWhen != "" || c.GeneratedKind != "") {
		return bindError("42601", "unexpected generation attributes", int(c.Location)+1)
	}
	if c.WithoutOverlaps || c.FkWithPeriod || c.PkWithPeriod {
		return missing("temporal constraints", int(c.Location)+1)
	}
	if c.Indexname != "" {
		return bindError("42809", "USING INDEX is only valid while adding a table constraint", int(c.Location)+1)
	}
	if c.Indexspace != "" || c.Contype != zpgparse.ConstrType_CONSTR_IDENTITY && len(c.Options) != 0 {
		return missing("constraint index storage options", int(c.Location)+1)
	}
	if c.AccessMethod != "" && !exclusion || c.WhereClause != nil && !exclusion {
		return bindError("42601", "constraint index fields do not match constraint kind", int(c.Location)+1)
	}
	return nil
}

// constraintAttributeState records which axis a deferrability attribute sets. The
// two axes are independent, so a constraint may carry one of each but repeats of
// the same axis conflict.
type constraintAttributeState struct {
	deferrability, deferrable bool
	timing, deferred          bool
}

func constraintAttribute(c *zpgparse.Constraint) (constraintAttributeState, bool) {
	switch c.Contype {
	case zpgparse.ConstrType_CONSTR_ATTR_DEFERRABLE:
		return constraintAttributeState{deferrability: true, deferrable: true}, true
	case zpgparse.ConstrType_CONSTR_ATTR_NOT_DEFERRABLE:
		return constraintAttributeState{deferrability: true}, true
	case zpgparse.ConstrType_CONSTR_ATTR_DEFERRED:
		return constraintAttributeState{timing: true, deferred: true}, true
	case zpgparse.ConstrType_CONSTR_ATTR_IMMEDIATE:
		return constraintAttributeState{timing: true}, true
	default:
		return constraintAttributeState{}, false
	}
}

// appendColumnConstraints appends one column's constraint items. DEFERRABLE, NOT
// DEFERRABLE, INITIALLY DEFERRED and INITIALLY IMMEDIATE arrive as their own items
// following the constraint they modify and carry no state of their own, so each is
// folded into the constraint just appended, as PostgreSQL does. A column may not
// state deferrability twice for the same constraint.
func appendColumnConstraints(constraints []tableConstraint, raw []*zpgparse.Node, column model.ColumnID, maximum int) ([]tableConstraint, error) {
	for _, node := range raw {
		constraint := node.GetConstraint()
		if constraint == nil {
			return nil, bindError("42601", "invalid column constraint", 0)
		}
		if attribute, ok := constraintAttribute(constraint); ok {
			position := len(constraints) - 1
			if position < 0 || constraints[position].column != column {
				return nil, bindError("42601", "misplaced constraint attribute", int(constraint.Location)+1)
			}
			prior := &constraints[position]
			// A constraint states deferrability at most once and its timing at most
			// once, but the two are independent: UNIQUE DEFERRABLE INITIALLY DEFERRED
			// is two attributes, and repeating either one is the error.
			if attribute.deferrability {
				if prior.deferrability {
					return nil, bindError("42601", "conflicting constraint attributes", int(constraint.Location)+1)
				}
				prior.deferrability = true
				prior.raw.Deferrable = attribute.deferrable
			}
			if attribute.timing {
				if prior.timing {
					return nil, bindError("42601", "conflicting constraint attributes", int(constraint.Location)+1)
				}
				prior.timing = true
				prior.raw.Initdeferred = attribute.deferred
			}
			continue
		}
		if len(constraints)+1 > maximum {
			return nil, model.ErrDescriptorLimit
		}
		constraints = append(constraints, tableConstraint{raw: constraint, column: column})
	}
	return constraints, nil
}
