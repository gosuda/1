package bind

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type TypeMutation struct {
	Base *model.Descriptor
	Next *model.Descriptor
}

type BoundTypeCommand struct {
	creates      []*model.Descriptor
	updates      []TypeMutation
	drops        []*model.Descriptor
	dependencies []model.Dependency
	warnings     []model.Error
	tag          string
	cascade      bool
	domainValidation model.ObjectID
}

func (c *BoundTypeCommand) Creates() []*model.Descriptor { return slices.Clone(c.creates) }
func (c *BoundTypeCommand) Updates() []TypeMutation      { return slices.Clone(c.updates) }
func (c *BoundTypeCommand) Drops() []*model.Descriptor   { return slices.Clone(c.drops) }
func (c *BoundTypeCommand) Dependencies() []model.Dependency {
	return slices.Clone(c.dependencies)
}
func (c *BoundTypeCommand) Warnings() []model.Error { return slices.Clone(c.warnings) }
func (c *BoundTypeCommand) CommandTag() string      { return c.tag }
func (c *BoundTypeCommand) Cascade() bool           { return c.cascade }
func (c *BoundTypeCommand) DomainValidationID() model.ObjectID { return c.domainValidation }

func BindTypeCommand(ctx context.Context, node *zpgparse.Node, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, bool, error) {
	if node == nil {
		return nil, false, bindError("42601", "missing type command", 0)
	}
	switch statement := node.Node.(type) {
	case *zpgparse.Node_DefineStmt:
		if statement.DefineStmt == nil || statement.DefineStmt.Kind != zpgparse.ObjectType_OBJECT_TYPE {
			return nil, false, nil
		}
		command, err := bindCreateShellType(ctx, statement.DefineStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_CreateDomainStmt:
		command, err := bindCreateDomain(ctx, statement.CreateDomainStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_CreateEnumStmt:
		command, err := bindCreateEnum(ctx, statement.CreateEnumStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_CompositeTypeStmt:
		command, err := bindCreateComposite(ctx, statement.CompositeTypeStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_CreateRangeStmt:
		command, err := bindCreateRange(ctx, statement.CreateRangeStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_AlterEnumStmt:
		command, err := bindAlterEnum(ctx, statement.AlterEnumStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_AlterDomainStmt:
		command, err := bindAlterDomain(ctx, statement.AlterDomainStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_AlterTableStmt:
		if statement.AlterTableStmt.Objtype != zpgparse.ObjectType_OBJECT_TYPE {
			return nil, false, nil
		}
		command, err := bindAlterComposite(ctx, statement.AlterTableStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_DropStmt:
		if statement.DropStmt.RemoveType != zpgparse.ObjectType_OBJECT_TYPE && statement.DropStmt.RemoveType != zpgparse.ObjectType_OBJECT_DOMAIN {
			return nil, false, nil
		}
		command, err := bindDropTypes(ctx, statement.DropStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_RenameStmt:
		if statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_DOMCONSTRAINT {
			command, err := bindRenameDomainConstraint(ctx, statement.RenameStmt, snapshot, options)
			return command, true, err
		}
		if statement.RenameStmt.RenameType == zpgparse.ObjectType_OBJECT_ATTRIBUTE {
			command, err := bindRenameCompositeAttribute(ctx, statement.RenameStmt, snapshot, options)
			return command, true, err
		}
		if statement.RenameStmt.RenameType != zpgparse.ObjectType_OBJECT_TYPE && statement.RenameStmt.RenameType != zpgparse.ObjectType_OBJECT_DOMAIN {
			return nil, false, nil
		}
		command, err := bindRenameType(ctx, statement.RenameStmt, snapshot, options)
		return command, true, err
	case *zpgparse.Node_AlterObjectSchemaStmt:
		if statement.AlterObjectSchemaStmt.ObjectType != zpgparse.ObjectType_OBJECT_TYPE && statement.AlterObjectSchemaStmt.ObjectType != zpgparse.ObjectType_OBJECT_DOMAIN {
			return nil, false, nil
		}
		command, err := bindSetTypeSchema(ctx, statement.AlterObjectSchemaStmt, snapshot, options)
		return command, true, err
	default:
		return nil, false, nil
	}
}

type typeDeclarationState struct {
	b       *binder
	options CreateDeclarationOptions
	parent  *model.Descriptor
	alloc   declarationState
}

func newTypeDeclarationState(ctx context.Context, node *zpgparse.Node, parts []string, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*typeDeclarationState, string, error) {
	b, err := declarationBinder(ctx, snapshot, options, node)
	if err != nil {
		return nil, "", err
	}
	if len(parts) < 1 || len(parts) > 2 {
		return nil, "", bindError("42601", "type name requires an optionally qualified name", 0)
	}
	name := parts[len(parts)-1]
	if err := declarationName(name); err != nil {
		return nil, "", err
	}
	path := options.Bind.SearchPath
	if len(parts) == 2 {
		path = []string{parts[0]}
	}
	var parent *model.Descriptor
	for _, schemaName := range path {
		published, resolveErr := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(resolveErr) {
			continue
		}
		if resolveErr != nil {
			return nil, "", resolveErr
		}
		if published.Descriptor.State() == model.StatePublic {
			parent = published.Descriptor
			break
		}
	}
	if parent == nil {
		return nil, "", bindError("3F000", "no schema selected for creation", 0)
	}
	b.dependency(parent, 0)
	state := &typeDeclarationState{b: b, options: options, parent: parent}
	state.alloc = declarationState{b: b, options: options, parent: parent.ID(), ids: make(map[model.ObjectID]bool), oids: make(map[model.OID]bool), names: make(map[string]bool), reserved: make(map[string]bool)}
	return state, name, nil
}

func (s *typeDeclarationState) ensureVacant(name string) error {
	if err := declarationName(name); err != nil {
		return err
	}
	published, err := s.b.catalog.Resolve(s.b.ctx, model.NamespaceName{Parent: s.parent.ID(), Kind: model.ObjectType, Name: name})
	if err == nil {
		s.b.dependency(published.Descriptor, 0)
		return bindError("42710", "type already exists", 0)
	}
	if !isMissingObject(err) {
		return err
	}
	return nil
}

func (s *typeDeclarationState) ensureRelationVacant(name string) error {
	for _, kind := range []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation} {
		published, err := s.b.catalog.Resolve(s.b.ctx, model.NamespaceName{Parent: s.parent.ID(), Kind: kind, Name: name})
		if err == nil {
			s.b.dependency(published.Descriptor, 0)
			return bindError("42710", "relation already exists", 0)
		}
		if !isMissingObject(err) {
			return err
		}
	}
	return nil
}

func ensureRelationTargetVacant(ctx context.Context, b *binder, snapshot CatalogSnapshot, parent model.ObjectID, name string, skip model.ObjectID) error {
	for _, kind := range []model.ObjectKind{model.ObjectTable, model.ObjectIndex, model.ObjectSequence, model.ObjectView, model.ObjectMaterializedView, model.ObjectCompositeRelation} {
		published, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: parent, Kind: kind, Name: name})
		if err == nil {
			if published.Descriptor.ID() == skip {
				continue
			}
			b.dependency(published.Descriptor, 0)
			return bindError("42710", "relation already exists", 0)
		}
		if !isMissingObject(err) {
			return err
		}
	}
	return nil
}

func arrayTypeName(name string) string {
	return "_" + clipIndexIdentifier(name, 62)
}

func multirangeTypeName(name string) string {
	const suffix = "_multirange"
	return clipIndexIdentifier(name, 63-len(suffix)) + suffix
}

func (s *typeDeclarationState) allocatePair(name, arrayName string, layout model.TypeLayout, metadata *model.TypeMetadata, dependencies []model.Dependency) ([]*model.Descriptor, error) {
	if err := s.ensureVacant(name); err != nil {
		return nil, err
	}
	if err := s.ensureVacant(arrayName); err != nil {
		return nil, err
	}
	id, oid, err := s.alloc.allocate()
	if err != nil {
		return nil, err
	}
	arrayID, arrayOID, err := s.alloc.allocate()
	if err != nil {
		return nil, err
	}
	layout.Type.ID = model.TypeID(oid)
	metadata.Owner = s.options.Owner
	metadata.Layout = layout
	metadata.Array = arrayID
	dependencies = typeDependencies(dependencies, model.Dependency{Object: s.parent.ID(), Version: s.parent.Version()})
	base, err := model.NewDescriptor(s.b.ctx, model.DescriptorSpec{ID: id, OID: oid, Name: model.NamespaceName{Parent: s.parent.ID(), Kind: model.ObjectType, Name: name}, Version: 1, State: model.StatePublic, Dependencies: dependencies, Type: metadata}, s.options.Descriptors)
	if err != nil {
		return nil, err
	}
	arrayLayout := model.TypeLayout{Type: model.Type{ID: model.TypeID(arrayOID), Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{layout}}
	arrayMetadata := &model.TypeMetadata{Kind: model.TypeKindArray, Owner: s.options.Owner, Layout: arrayLayout, Base: id, BaseLayout: layout}
	arrayDependencies := typeDependencies([]model.Dependency{{Object: s.parent.ID(), Version: s.parent.Version()}}, model.Dependency{Object: id, Version: 1})
	array, err := model.NewDescriptor(s.b.ctx, model.DescriptorSpec{ID: arrayID, OID: arrayOID, Name: model.NamespaceName{Parent: s.parent.ID(), Kind: model.ObjectType, Name: arrayName}, Version: 1, State: model.StatePublic, Dependencies: arrayDependencies, Type: arrayMetadata}, s.options.Descriptors)
	if err != nil {
		return nil, err
	}
	return []*model.Descriptor{base, array}, nil
}

func typeDependencies(dependencies []model.Dependency, additions ...model.Dependency) []model.Dependency {
	result := append(slices.Clone(dependencies), additions...)
	slices.SortFunc(result, func(a, b model.Dependency) int {
		if order := cmp.Compare(a.Object, b.Object); order != 0 {
			return order
		}
		return cmp.Compare(a.Column, b.Column)
	})
	return slices.Compact(result)
}

func bindCreateShellType(ctx context.Context, statement *zpgparse.DefineStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.Kind != zpgparse.ObjectType_OBJECT_TYPE || len(statement.Definition) != 0 || len(statement.Args) != 0 || statement.Replace {
		return nil, bindError("0A000", "only shell CREATE TYPE declarations are supported", 0)
	}
	parts, err := names(statement.Defnames)
	if err != nil {
		return nil, err
	}
	state, name, err := newTypeDeclarationState(ctx, &zpgparse.Node{Node: &zpgparse.Node_DefineStmt{DefineStmt: statement}}, parts, snapshot, options)
	if err != nil {
		return nil, err
	}
	if err := state.ensureVacant(name); err != nil {
		return nil, err
	}
	id, oid, err := state.alloc.allocate()
	if err != nil {
		return nil, err
	}
	layout := model.TypeLayout{Type: model.Type{ID: model.TypeID(oid), Modifier: -1, Storage: model.TypeBytes}}
	descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: id, OID: oid, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: state.parent.ID(), Kind: model.ObjectType, Name: name},
		Dependencies: []model.Dependency{{Object: state.parent.ID(), Version: state.parent.Version()}},
		Type: &model.TypeMetadata{Kind: model.TypeKindShell, Owner: options.Owner, Layout: layout},
	}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundTypeCommand{creates: []*model.Descriptor{descriptor}, dependencies: state.b.dependencies, warnings: state.b.warnings, tag: "CREATE TYPE"}, nil
}

func bindCreateEnum(ctx context.Context, statement *zpgparse.CreateEnumStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil {
		return nil, bindError("42601", "missing CREATE TYPE AS ENUM", 0)
	}
	parts, err := names(statement.TypeName)
	if err != nil {
		return nil, err
	}
	state, name, err := newTypeDeclarationState(ctx, &zpgparse.Node{Node: &zpgparse.Node_CreateEnumStmt{CreateEnumStmt: statement}}, parts, snapshot, options)
	if err != nil {
		return nil, err
	}
	if len(statement.Vals) == 0 || len(statement.Vals) > options.Descriptors.MaxItems {
		return nil, bindError("42601", "enum requires at least one label within the descriptor limit", 0)
	}
	labels, err := names(statement.Vals)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(labels))
	oids := make([]model.OID, len(labels))
	for i, label := range labels {
		if len(label) > 63 || strings.IndexByte(label, 0) >= 0 {
			return nil, bindError("42622", "invalid enum label", 0)
		}
		if seen[label] {
			return nil, bindError("42710", "enum label specified more than once", 0)
		}
		seen[label] = true
		_, oids[i], err = state.alloc.allocate()
		if err != nil {
			return nil, err
		}
	}
	metadata := &model.TypeMetadata{Kind: model.TypeKindEnum, Elements: labels, ElementOIDs: oids}
	created, err := state.allocatePair(name, arrayTypeName(name), model.TypeLayout{Type: model.Type{Modifier: -1, Storage: model.TypeInt64}}, metadata, nil)
	if err != nil {
		return nil, err
	}
	return &BoundTypeCommand{creates: created, dependencies: state.b.dependencies, warnings: state.b.warnings, tag: "CREATE TYPE"}, nil
}

func bindCreateComposite(ctx context.Context, statement *zpgparse.CompositeTypeStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.Typevar == nil || statement.Typevar.Catalogname != "" {
		return nil, bindError("42601", "missing or invalid CREATE TYPE AS relation", 0)
	}
	parts := []string{statement.Typevar.Relname}
	if statement.Typevar.Schemaname != "" {
		parts = []string{statement.Typevar.Schemaname, statement.Typevar.Relname}
	}
	state, name, err := newTypeDeclarationState(ctx, &zpgparse.Node{Node: &zpgparse.Node_CompositeTypeStmt{CompositeTypeStmt: statement}}, parts, snapshot, options)
	if err != nil {
		return nil, err
	}
	if len(statement.Coldeflist) > options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	fields := make([]model.ColumnDescriptor, 0, len(statement.Coldeflist))
	children := make([]model.TypeLayout, 0, len(statement.Coldeflist))
	fieldNames := make(map[string]bool, len(statement.Coldeflist))
	for _, node := range statement.Coldeflist {
		column := node.GetColumnDef()
		if column == nil || len(column.Constraints) != 0 {
			return nil, bindError("42601", "composite type requires attribute declarations", 0)
		}
		if err := ordinaryColumn(column); err != nil {
			return nil, err
		}
		if err := declarationName(column.Colname); err != nil {
			return nil, err
		}
		if fieldNames[column.Colname] {
			return nil, bindError("42701", "attribute specified more than once", int(column.Location)+1)
		}
		fieldNames[column.Colname] = true
		layout, serial, err := state.b.declarationColumnType(column.TypeName)
		if err != nil {
			return nil, err
		}
		if serial {
			return nil, bindError("0A000", "serial is not valid for a composite attribute", int(column.Location)+1)
		}
		if column.CollClause != nil {
			layout, err = state.b.indexColumnCollation(layout, column.CollClause.Collname)
			if err != nil {
				return nil, err
			}
		}
		id := model.ColumnID(len(fields) + 1)
		fields = append(fields, model.ColumnDescriptor{ID: id, Name: column.Colname, State: model.StatePublic, Layout: layout, Nullable: true})
		children = append(children, layout)
	}
	if err := state.ensureRelationVacant(name); err != nil {
		return nil, err
	}
	relationID, relationOID, err := state.alloc.allocate()
	if err != nil {
		return nil, err
	}
	metadata := &model.TypeMetadata{Kind: model.TypeKindComposite, Fields: fields, FieldIDHighWatermark: model.ColumnID(len(fields)), Relation: relationID}
	dependencies := typeDependencies(state.b.dependencies, model.Dependency{Object: relationID, Version: 1})
	created, err := state.allocatePair(name, arrayTypeName(name), model.TypeLayout{Type: model.Type{Modifier: -1, Storage: model.TypeComposite}, Children: children}, metadata, dependencies)
	if err != nil {
		return nil, err
	}
	relation, err := model.NewDescriptor(ctx, model.DescriptorSpec{
		ID: relationID, OID: relationOID, Version: 1, State: model.StatePublic,
		Name:              model.NamespaceName{Parent: state.parent.ID(), Kind: model.ObjectCompositeRelation, Name: name},
		Dependencies:      []model.Dependency{{Object: state.parent.ID(), Version: state.parent.Version()}},
		CompositeRelation: &model.CompositeRelationMetadata{Owner: options.Owner, RowType: created[0].ID(), FieldIDHighWatermark: model.ColumnID(len(fields)), Columns: fields},
	}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	created = slices.Insert(created, 0, relation)
	return &BoundTypeCommand{creates: created, dependencies: state.b.dependencies, warnings: state.b.warnings, tag: "CREATE TYPE"}, nil
}

func bindCreateDomain(ctx context.Context, statement *zpgparse.CreateDomainStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.TypeName == nil {
		return nil, bindError("42601", "missing CREATE DOMAIN type", 0)
	}
	parts, err := names(statement.Domainname)
	if err != nil {
		return nil, err
	}
	state, name, err := newTypeDeclarationState(ctx, &zpgparse.Node{Node: &zpgparse.Node_CreateDomainStmt{CreateDomainStmt: statement}}, parts, snapshot, options)
	if err != nil {
		return nil, err
	}
	baseLayout, err := state.b.typeName(statement.TypeName)
	if err != nil {
		return nil, err
	}
	if statement.CollClause != nil {
		baseLayout, err = state.b.indexColumnCollation(baseLayout, statement.CollClause.Collname)
		if err != nil {
			return nil, err
		}
	}
	var baseObject model.ObjectID
	if descriptor, err := state.b.findNamedType(baseLayout.Type.ID, 0); err != nil {
		return nil, err
	} else if descriptor != nil {
		baseObject = descriptor.ID()
	}
	metadata := &model.TypeMetadata{Kind: model.TypeKindDomain, Base: baseObject, BaseLayout: baseLayout}
	var checks []*zpgparse.Constraint
	var notNull *zpgparse.Constraint
	if len(statement.Constraints) > options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	constraintNames := make(map[string]bool)
	for _, node := range statement.Constraints {
		constraint := node.GetConstraint()
		if constraint == nil {
			return nil, bindError("42601", "invalid domain constraint", 0)
		}
		if err := ordinaryDomainConstraint(constraint); err != nil {
			return nil, err
		}
		switch constraint.Contype {
		case zpgparse.ConstrType_CONSTR_DEFAULT:
			if metadata.Default != nil || constraint.RawExpr == nil {
				return nil, bindError("42601", "multiple or invalid domain defaults", 0)
			}
			bound, err := defineDefault(ctx, constraint.RawExpr, baseLayout, snapshot, options.Bind, true)
			if err != nil {
				return nil, err
			}
			definition := bound.Definition
			metadata.Default = &definition
			state.b.warnings = append(state.b.warnings, bound.Warnings...)
		case zpgparse.ConstrType_CONSTR_NOTNULL:
			if notNull != nil {
				return nil, bindError("42710", "domain NOT NULL constraint specified more than once", 0)
			}
			if constraint.Conname != "" {
				if err := declarationName(constraint.Conname); err != nil {
					return nil, err
				}
				if constraintNames[constraint.Conname] {
					return nil, bindError("42710", "domain constraint specified more than once", 0)
				}
				constraintNames[constraint.Conname] = true
			}
			notNull = constraint
			metadata.NotNull = true
		case zpgparse.ConstrType_CONSTR_NULL:
			if notNull != nil {
				return nil, bindError("42601", "conflicting NULL and NOT NULL domain constraints", 0)
			}
			metadata.NotNull = false
		case zpgparse.ConstrType_CONSTR_CHECK:
			if constraint.RawExpr == nil {
				return nil, bindError("42601", "domain CHECK requires an expression", 0)
			}
			if constraint.Conname != "" {
				if err := declarationName(constraint.Conname); err != nil {
					return nil, err
				}
				if constraintNames[constraint.Conname] {
					return nil, bindError("42710", "domain constraint specified more than once", 0)
				}
				constraintNames[constraint.Conname] = true
			}
			checks = append(checks, constraint)
		default:
			return nil, bindError("42601", "unsupported domain constraint", int(constraint.Location)+1)
		}
	}
	created, err := state.allocatePair(name, arrayTypeName(name), model.TypeLayout{Type: model.Type{Modifier: baseLayout.Type.Modifier, Collation: baseLayout.Type.Collation, Storage: baseLayout.Type.Storage}, Children: slices.Clone(baseLayout.Children)}, metadata, state.b.dependencies)
	if err != nil {
		return nil, err
	}
	if len(checks) != 0 || notNull != nil {
		baseSpec, err := created[0].SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if notNull != nil {
			id, oid, allocateErr := state.alloc.allocate()
			if allocateErr != nil {
				return nil, allocateErr
			}
			constraintName := notNull.Conname
			if constraintName == "" {
				constraintName = domainNotNullConstraintName(created[0].Name().Name, constraintNames)
			}
			baseSpec.Type.Constraints = append(baseSpec.Type.Constraints, model.ConstraintDescriptor{ID: id, OID: oid, Name: constraintName, Kind: model.ConstraintNotNull, Enforced: true, Validated: true})
		}
		for i, raw := range checks {
			id, oid, allocateErr := state.alloc.allocate()
			if allocateErr != nil {
				return nil, allocateErr
			}
			constraintName := raw.Conname
			if constraintName == "" {
				constraintName = domainConstraintName(created[0].Name().Name, i+1, constraintNames)
			}
			definition, dependencies, defineErr := DefineDomainCheck(ctx, raw.RawExpr, created[0], id, baseLayout, snapshot, options.Bind)
			if defineErr != nil {
				return nil, defineErr
			}
			baseSpec.Type.Constraints = append(baseSpec.Type.Constraints, model.ConstraintDescriptor{ID: id, OID: oid, Name: constraintName, Kind: model.ConstraintCheck, Enforced: raw.IsEnforced, Validated: !raw.SkipValidation, Expression: definition})
			baseSpec.Dependencies = typeDependencies(baseSpec.Dependencies, externalTypeDependencies(dependencies, created[0].ID())...)
		}
		created[0], err = model.NewDescriptor(ctx, baseSpec, options.Descriptors)
		if err != nil {
			return nil, err
		}
	}
	return &BoundTypeCommand{creates: created, dependencies: state.b.dependencies, warnings: state.b.warnings, tag: "CREATE DOMAIN"}, nil
}

func domainConstraintName(domain string, ordinal int, reserved map[string]bool) string {
	base := clipIndexIdentifier(domain, 56) + "_check"
	name := base
	for suffix := ordinal; reserved[name]; suffix++ {
		text := strconv.Itoa(suffix)
		name = clipIndexIdentifier(base, 63-len(text)) + text
	}
	reserved[name] = true
	return name
}
func domainNotNullConstraintName(domain string, reserved map[string]bool) string {
	base := clipIndexIdentifier(domain, 54) + "_not_null"
	name := base
	for suffix := 1; reserved[name]; suffix++ {
		text := strconv.Itoa(suffix)
		name = clipIndexIdentifier(base, 63-len(text)) + text
	}
	reserved[name] = true
	return name
}
func externalTypeDependencies(dependencies []model.Dependency, self model.ObjectID) []model.Dependency {
	return slices.DeleteFunc(slices.Clone(dependencies), func(dependency model.Dependency) bool {
		return dependency.Object == self
	})
}
func ordinaryDomainConstraint(constraint *zpgparse.Constraint) error {
	if constraint == nil {
		return bindError("42601", "missing domain constraint", 0)
	}
	if constraint.Deferrable || constraint.Initdeferred {
		return bindError("0A000", "domain constraints cannot be DEFERRABLE", int(constraint.Location)+1)
	}
	if constraint.IsNoInherit {
		return bindError("42601", "NO INHERIT is not valid for a domain constraint", int(constraint.Location)+1)
	}
	owned := proto.Clone(constraint).(*zpgparse.Constraint)
	owned.SkipValidation = false
	return ordinaryConstraint(owned)
}



func bindCreateRange(ctx context.Context, statement *zpgparse.CreateRangeStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil {
		return nil, bindError("42601", "missing CREATE TYPE AS RANGE", 0)
	}
	parts, err := names(statement.TypeName)
	if err != nil {
		return nil, err
	}
	state, name, err := newTypeDeclarationState(ctx, &zpgparse.Node{Node: &zpgparse.Node_CreateRangeStmt{CreateRangeStmt: statement}}, parts, snapshot, options)
	if err != nil {
		return nil, err
	}
	var shell *model.Descriptor
	published, resolveErr := state.b.catalog.Resolve(ctx, model.NamespaceName{Parent: state.parent.ID(), Kind: model.ObjectType, Name: name})
	if resolveErr == nil {
		spec, err := published.Descriptor.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindShell {
			return nil, bindError("42710", "type already exists", 0)
		}
		shell = published.Descriptor
		state.b.dependency(shell, 0)
	} else if !isMissingObject(resolveErr) {
		return nil, resolveErr
	}
	definitions := make(map[string]*zpgparse.Node, len(statement.Params))
	for _, node := range statement.Params {
		definition := node.GetDefElem()
		if definition == nil || definition.Arg == nil {
			return nil, bindError("42601", "invalid range parameter", 0)
		}
		if definitions[definition.Defname] != nil {
			return nil, bindError("42601", "range parameter specified more than once", int(definition.Location)+1)
		}
		switch definition.Defname {
		case "subtype", "subtype_opclass", "collation", "canonical", "subtype_diff", "multirange_type_name":
			definitions[definition.Defname] = definition.Arg
		default:
			return nil, bindError("22023", "unrecognized range parameter", int(definition.Location)+1)
		}
	}
	subtypeNode := definitions["subtype"]
	if subtypeNode == nil || subtypeNode.GetTypeName() == nil {
		return nil, bindError("42601", "range subtype is required", 0)
	}
	subtype, err := state.b.typeName(subtypeNode.GetTypeName())
	if err != nil {
		return nil, err
	}
	if collation := definitions["collation"]; collation != nil {
		collationName, err := typeOptionName(collation)
		if err != nil {
			return nil, err
		}
		subtype, err = state.b.indexColumnCollation(subtype, stringNodes(collationName))
		if err != nil {
			return nil, err
		}
	}
	classType, err := state.baseComparisonType(subtype)
	if err != nil {
		return nil, err
	}
	var operatorClass model.OID
	if option := definitions["subtype_opclass"]; option != nil {
		className, err := typeOptionName(option)
		if err != nil {
			return nil, err
		}
		operatorClass, err = indexOperatorClass(classType, stringNodes(className))
		if err != nil {
			return nil, err
		}
	} else {
		class, found := builtin.LookupDefaultOperatorClass("btree", classType.ID)
		if !found {
			return nil, bindError("42704", "range subtype has no default B-tree operator class", 0)
		}
		operatorClass = class.ID
	}
	multirangeName := multirangeTypeName(name)
	if option := definitions["multirange_type_name"]; option != nil {
		value, err := typeOptionName(option)
		if err != nil || len(value) != 1 {
			return nil, bindError("42601", "multirange type name must be unqualified", 0)
		}
		multirangeName = value[0]
	}
	generatedNames := make(map[string]bool, 4)
	for _, candidate := range []string{name, arrayTypeName(name), multirangeName, arrayTypeName(multirangeName)} {
		if generatedNames[candidate] {
			return nil, bindError("42710", "range companion type name appears more than once", 0)
		}
		generatedNames[candidate] = true
		if shell != nil && candidate == name {
			continue
		}
		if err := state.ensureVacant(candidate); err != nil {
			return nil, err
		}
	}
	var rangeID model.ObjectID
	var rangeOID model.OID
	if shell != nil {
		rangeID, rangeOID = shell.ID(), shell.OID()
	} else {
		rangeID, rangeOID, err = state.alloc.allocate()
		if err != nil {
			return nil, err
		}
	}
	rangeArrayID, rangeArrayOID, err := state.alloc.allocate()
	if err != nil {
		return nil, err
	}
	multirangeID, multirangeOID, err := state.alloc.allocate()
	if err != nil {
		return nil, err
	}
	multirangeArrayID, multirangeArrayOID, err := state.alloc.allocate()
	if err != nil {
		return nil, err
	}
	rangeLayout := model.TypeLayout{Type: model.Type{ID: model.TypeID(rangeOID), Modifier: -1, Storage: model.TypeBytes}}
	multirangeLayout := model.TypeLayout{Type: model.Type{ID: model.TypeID(multirangeOID), Modifier: -1, Storage: model.TypeBytes}}
	rangeMetadata := &model.RangeTypeMetadata{Subtype: subtype, OperatorClass: operatorClass, Collation: subtype.Type.Collation, Range: rangeID, Multirange: multirangeID}
	if option := definitions["canonical"]; option != nil {
		rangeMetadata.Canonical, rangeMetadata.CanonicalFunction, err = state.resolveRangeRoutine(option, []model.TypeLayout{rangeLayout}, rangeLayout)
		if err != nil {
			return nil, err
		}
	}
	if option := definitions["subtype_diff"]; option != nil {
		float8 := state.b.primitive(701)
		rangeMetadata.Difference, rangeMetadata.DifferenceFunction, err = state.resolveRangeRoutine(option, []model.TypeLayout{subtype, subtype}, float8)
		if err != nil {
			return nil, err
		}
	}
	parentDependency := model.Dependency{Object: state.parent.ID(), Version: state.parent.Version()}
	subtypeDependencies := externalTypeDependencies(state.b.dependencies, rangeID)
	rangeOwner := options.Owner
	if shell != nil {
		shellSpec, err := shell.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		rangeOwner = shellSpec.Type.Owner
	}
	rangeType := &model.TypeMetadata{Kind: model.TypeKindRange, Owner: rangeOwner, Layout: rangeLayout, Array: rangeArrayID, Range: rangeMetadata}
	var rangeDescriptor *model.Descriptor
	if shell == nil {
		rangeDescriptor, err = model.NewDescriptor(ctx, model.DescriptorSpec{ID: rangeID, OID: rangeOID, Name: model.NamespaceName{Parent: state.parent.ID(), Kind: model.ObjectType, Name: name}, Version: 1, State: model.StatePublic, Dependencies: typeDependencies(subtypeDependencies, parentDependency), Type: rangeType}, options.Descriptors)
	} else {
		rangeDescriptor, err = nextTypeDescriptor(ctx, shell, options, func(spec *model.DescriptorSpec) error {
			spec.Dependencies = typeDependencies(subtypeDependencies, parentDependency)
			spec.Type = rangeType
			return nil
		})
	}
	if err != nil {
		return nil, err
	}
	rangeArrayLayout := model.TypeLayout{Type: model.Type{ID: model.TypeID(rangeArrayOID), Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{rangeLayout}}
	rangeArray, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: rangeArrayID, OID: rangeArrayOID, Name: model.NamespaceName{Parent: state.parent.ID(), Kind: model.ObjectType, Name: arrayTypeName(name)}, Version: 1, State: model.StatePublic, Dependencies: typeDependencies([]model.Dependency{parentDependency}, model.Dependency{Object: rangeID, Version: rangeDescriptor.Version()}), Type: &model.TypeMetadata{Kind: model.TypeKindArray, Owner: rangeOwner, Layout: rangeArrayLayout, Base: rangeID, BaseLayout: rangeLayout}}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	multirangeMetadata := *rangeMetadata
	multirangeDescriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: multirangeID, OID: multirangeOID, Name: model.NamespaceName{Parent: state.parent.ID(), Kind: model.ObjectType, Name: multirangeName}, Version: 1, State: model.StatePublic, Dependencies: typeDependencies(subtypeDependencies, parentDependency, model.Dependency{Object: rangeID, Version: rangeDescriptor.Version()}), Type: &model.TypeMetadata{Kind: model.TypeKindMultirange, Owner: rangeOwner, Layout: multirangeLayout, Array: multirangeArrayID, Range: &multirangeMetadata}}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	multirangeArrayLayout := model.TypeLayout{Type: model.Type{ID: model.TypeID(multirangeArrayOID), Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{multirangeLayout}}
	multirangeArray, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: multirangeArrayID, OID: multirangeArrayOID, Name: model.NamespaceName{Parent: state.parent.ID(), Kind: model.ObjectType, Name: arrayTypeName(multirangeName)}, Version: 1, State: model.StatePublic, Dependencies: typeDependencies([]model.Dependency{parentDependency}, model.Dependency{Object: multirangeID, Version: 1}), Type: &model.TypeMetadata{Kind: model.TypeKindArray, Owner: rangeOwner, Layout: multirangeArrayLayout, Base: multirangeID, BaseLayout: multirangeLayout}}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	command := &BoundTypeCommand{creates: []*model.Descriptor{rangeArray, multirangeDescriptor, multirangeArray}, dependencies: state.b.dependencies, warnings: state.b.warnings, tag: "CREATE TYPE"}
	if shell == nil {
		command.creates = append([]*model.Descriptor{rangeDescriptor}, command.creates...)
	} else {
		command.updates = []TypeMutation{{Base: shell, Next: rangeDescriptor}}
	}
	return command, nil
}

func typeOptionName(node *zpgparse.Node) ([]string, error) {
	if node == nil {
		return nil, bindError("42601", "missing type option", 0)
	}
	if typeName := node.GetTypeName(); typeName != nil {
		return names(typeName.Names)
	}
	if list := node.GetList(); list != nil {
		return names(list.Items)
	}
	if value := node.GetString_(); value != nil {
		return []string{value.Sval}, nil
	}
	return nil, bindError("42601", "invalid type option name", 0)
}

func stringNodes(parts []string) []*zpgparse.Node {
	result := make([]*zpgparse.Node, len(parts))
	for i, part := range parts {
		result[i] = &zpgparse.Node{Node: &zpgparse.Node_String_{String_: &zpgparse.String{Sval: part}}}
	}
	return result
}

func (s *typeDeclarationState) baseComparisonType(layout model.TypeLayout) (model.Type, error) {
	for range 64 {
		if info, known := builtin.LookupType(layout.Type.ID); known {
			if info.Base == 0 {
				return layout.Type, nil
			}
			layout.Type.ID = info.Base
			continue
		}
		if informationType, known := s.options.Bind.CatalogRelations.ResolveInformationSchemaType(layout.Type.ID); known {
			switch informationType.Kind {
			case model.TypeKindDomain:
				layout = informationType.BaseLayout
				continue
			case model.TypeKindArray:
				return model.Type{ID: 2277, Modifier: -1, Storage: model.TypeArray}, nil
			default:
				return layout.Type, nil
			}
		}
		descriptor, err := s.b.findNamedType(layout.Type.ID, 0)
		if err != nil || descriptor == nil {
			return layout.Type, err
		}
		spec, err := descriptor.SpecWithin(s.b.ctx, s.options.Descriptors)
		if err != nil {
			return model.Type{}, err
		}
		switch spec.Type.DeclaredKind() {
		case model.TypeKindDomain:
			layout = spec.Type.BaseLayout
		case model.TypeKindEnum:
			return model.Type{ID: 3500, Modifier: -1, Storage: model.TypeInt64}, nil
		case model.TypeKindComposite:
			return model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, nil
		case model.TypeKindArray:
			return model.Type{ID: 2277, Modifier: -1, Storage: model.TypeArray}, nil
		case model.TypeKindRange:
			return model.Type{ID: 3831, Modifier: -1, Storage: model.TypeBytes}, nil
		case model.TypeKindMultirange:
			return model.Type{ID: 4537, Modifier: -1, Storage: model.TypeBytes}, nil
		default:
			return layout.Type, nil
		}
	}
	return model.Type{}, bindError("54000", "domain base chain exceeds limit", 0)
}

func (s *typeDeclarationState) resolveRangeRoutine(node *zpgparse.Node, parameters []model.TypeLayout, result model.TypeLayout) (model.ObjectID, model.FunctionID, error) {
	parts, err := typeOptionName(node)
	if err != nil || len(parts) < 1 || len(parts) > 2 {
		return 0, 0, bindError("42601", "range support function requires an optionally qualified name", 0)
	}
	name := parts[len(parts)-1]
	for signature := range builtin.SignaturesNamed(name) {
		if int(signature.ArgumentCount) != len(parameters) {
			continue
		}
		matches := signature.Result == result.Type.ID || signature.Result == 3831 || signature.Result == 4537
		for i := range parameters {
			matches = matches && (signature.Arguments[i] == parameters[i].Type.ID || signature.Arguments[i] == 3831 || signature.Arguments[i] == 2283)
		}
		if matches {
			return 0, signature.ID, nil
		}
	}
	path := s.options.Bind.SearchPath
	if len(parts) == 2 {
		path = []string{parts[0]}
	}
	wanted := make([]model.RoutineParameter, len(parameters))
	for i := range parameters {
		wanted[i] = model.RoutineParameter{Mode: "in", Layout: parameters[i]}
	}
	for _, schemaName := range path {
		schema, resolveErr := s.b.catalog.Resolve(s.b.ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: schemaName})
		if isMissingObject(resolveErr) {
			continue
		}
		if resolveErr != nil {
			return 0, 0, resolveErr
		}
		routine, err := resolveRoutineOverload(s.b.ctx, s.b.catalog, schema.Descriptor.ID(), name, wanted)
		if err != nil {
			return 0, 0, err
		}
		if routine == nil {
			continue
		}
		spec, err := routine.SpecWithin(s.b.ctx, s.options.Descriptors)
		if err != nil {
			return 0, 0, err
		}
		if spec.Routine.Volatility != "immutable" || !sameRoutineResult(spec.Routine.Result, result) {
			return 0, 0, bindError("42804", "range support function has an invalid result or volatility", 0)
		}
		s.b.dependency(routine, 0)
		return routine.ID(), model.FunctionID(routine.OID()), nil
	}
	return 0, 0, bindError("42883", "range support function does not exist", 0)
}

func typeNameFromNode(node *zpgparse.Node) ([]string, error) {
	if node == nil || node.GetTypeName() == nil {
		return nil, bindError("42601", "invalid type name", 0)
	}
	return names(node.GetTypeName().Names)
}

func resolveTypeDescriptor(ctx context.Context, b *binder, parts []string) (*model.Descriptor, error) {
	descriptor, _, err := b.resolve(parts, []model.ObjectKind{model.ObjectType}, "42704", 0)
	if err != nil {
		return nil, err
	}
	if descriptor.State() != model.StatePublic {
		return nil, bindError("55000", "type is not public", 0)
	}
	b.dependency(descriptor, 0)
	return descriptor, ctx.Err()
}
func isUndefinedType(err error) bool {
	var sql *model.Error
	return errors.As(err, &sql) && sql.Code == "42704"
}


func nextTypeDescriptor(ctx context.Context, base *model.Descriptor, options CreateDeclarationOptions, mutate func(*model.DescriptorSpec) error) (*model.Descriptor, error) {
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Version == ^uint64(0) {
		return nil, bindError("54000", "type version exhausted", 0)
	}
	spec.Version++
	if err := mutate(&spec); err != nil {
		return nil, err
	}
	return model.NewDescriptor(ctx, spec, options.Descriptors)
}
func companionArrayMutation(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, baseNext *model.Descriptor) (*TypeMutation, error) {
	baseSpec, err := baseNext.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if baseSpec.Type == nil || baseSpec.Type.Array == 0 {
		return nil, nil
	}
	published, err := snapshot.Descriptor(ctx, baseSpec.Type.Array)
	if err != nil {
		return nil, err
	}
	arrayBase := published.Descriptor
	arrayNext, err := nextTypeDescriptor(ctx, arrayBase, options, func(spec *model.DescriptorSpec) error {
		spec.Type.BaseLayout = baseSpec.Type.Layout
		spec.Type.Layout.Children = []model.TypeLayout{baseSpec.Type.Layout}
		spec.Dependencies = replaceTypeDependency(spec.Dependencies, baseNext.ID(), baseNext.Version())
		return nil
	})
	if err != nil {
		return nil, err
	}
	b.dependency(arrayBase, 0)
	return &TypeMutation{Base: arrayBase, Next: arrayNext}, nil
}

func companionCompositeRelationMutation(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, typeNext *model.Descriptor) (*TypeMutation, *model.Descriptor, error) {
	typeSpec, err := typeNext.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	if typeSpec.Type == nil || typeSpec.Type.DeclaredKind() != model.TypeKindComposite || typeSpec.Type.Relation == 0 {
		return nil, typeNext, nil
	}
	published, err := snapshot.Descriptor(ctx, typeSpec.Type.Relation)
	if err != nil {
		return nil, nil, err
	}
	base := published.Descriptor
	if base.Kind() != model.ObjectCompositeRelation {
		return nil, nil, bindError("42809", "ALTER TYPE requires a standalone composite type", 0)
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	if spec.CompositeRelation == nil || spec.CompositeRelation.RowType != typeNext.ID() || spec.Version == ^uint64(0) {
		return nil, nil, model.ErrInvalidDescriptor
	}
	spec.Version++
	spec.CompositeRelation.Owner = typeSpec.Type.Owner
	spec.CompositeRelation.FieldIDHighWatermark = typeSpec.Type.FieldIDHighWatermark
	spec.CompositeRelation.Columns = slices.Clone(typeSpec.Type.Fields)
	next, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	typeSpec.Dependencies = replaceTypeDependency(typeSpec.Dependencies, base.ID(), next.Version())
	repaired, err := model.NewDescriptor(ctx, typeSpec, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	b.dependency(base, 0)
	return &TypeMutation{Base: base, Next: next}, repaired, nil
}

func replaceTypeDependency(dependencies []model.Dependency, object model.ObjectID, version uint64) []model.Dependency {
	result := slices.DeleteFunc(slices.Clone(dependencies), func(dependency model.Dependency) bool {
		return dependency.Object == object
	})
	return typeDependencies(result, model.Dependency{Object: object, Version: version})
}


func bindAlterEnum(ctx context.Context, statement *zpgparse.AlterEnumStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil {
		return nil, bindError("42601", "missing ALTER TYPE enum", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterEnumStmt{AlterEnumStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := names(statement.TypeName)
	if err != nil {
		return nil, err
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		return nil, err
	}
	baseSpec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if baseSpec.Type.DeclaredKind() != model.TypeKindEnum {
		return nil, bindError("42809", "ALTER TYPE ADD VALUE requires an enum", 0)
	}
	metadata := baseSpec.Type
	if len(statement.NewVal) > 63 || strings.IndexByte(statement.NewVal, 0) >= 0 {
		return nil, bindError("42622", "invalid enum label", 0)
	}
	if statement.OldVal != "" {
		index := slices.Index(metadata.Elements, statement.OldVal)
		if index < 0 {
			return nil, bindError("22P02", "invalid enum label", 0)
		}
		if slices.Contains(metadata.Elements, statement.NewVal) {
			return nil, bindError("42710", "enum label already exists", 0)
		}
		metadata.Elements[index] = statement.NewVal
	} else {
		if slices.Contains(metadata.Elements, statement.NewVal) {
			if statement.SkipIfNewValExists {
				return &BoundTypeCommand{dependencies: b.dependencies, warnings: []model.Error{{Code: "42710", Message: "enum label already exists, skipping"}}, tag: "ALTER TYPE"}, nil
			}
			return nil, bindError("42710", "enum label already exists", 0)
		}
		position := len(metadata.Elements)
		if statement.NewValNeighbor != "" {
			position = slices.Index(metadata.Elements, statement.NewValNeighbor)
			if position < 0 {
				return nil, bindError("22P02", "enum neighbor label does not exist", 0)
			}
			if statement.NewValIsAfter {
				position++
			}
		}
		state := declarationState{b: b, options: options, parent: base.Name().Parent, ids: make(map[model.ObjectID]bool), oids: make(map[model.OID]bool)}
		_, oid, err := state.allocate()
		if err != nil {
			return nil, err
		}
		if base.ProvisionalBinding() == nil {
			metadata.UnsafeElementOIDs = nil
		}
		metadata.Elements = slices.Insert(metadata.Elements, position, statement.NewVal)
		metadata.ElementOIDs = slices.Insert(metadata.ElementOIDs, position, oid)
		metadata.UnsafeElementOIDs = append(metadata.UnsafeElementOIDs, oid)
	}
	next, err := nextTypeDescriptor(ctx, base, options, func(spec *model.DescriptorSpec) error { spec.Type = metadata; return nil })
	if err != nil {
		return nil, err
	}
	updates := []TypeMutation{{Base: base, Next: next}}
	array, err := companionArrayMutation(ctx, b, snapshot, options, next)
	if err != nil {
		return nil, err
	}
	if array != nil {
		updates = append(updates, *array)
	}
	return &BoundTypeCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: "ALTER TYPE"}, nil
}

func bindAlterDomain(ctx context.Context, statement *zpgparse.AlterDomainStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil {
		return nil, bindError("42601", "missing ALTER DOMAIN", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterDomainStmt{AlterDomainStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := names(statement.TypeName)
	if err != nil {
		return nil, err
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Type.DeclaredKind() != model.TypeKindDomain {
		return nil, bindError("42809", "ALTER DOMAIN requires a domain", 0)
	}
	metadata := spec.Type
	state := declarationState{b: b, options: options, parent: base.Name().Parent, ids: make(map[model.ObjectID]bool), oids: make(map[model.OID]bool)}
	for _, constraint := range metadata.Constraints {
		state.ids[constraint.ID] = true
		state.oids[constraint.OID] = true
	}
	var domainValidation model.ObjectID
	switch statement.Subtype {
	case "T":
		previous := metadata.Default
		if statement.Def == nil {
			metadata.Default = nil
		} else {
			bound, bindErr := defineDefault(ctx, statement.Def, metadata.BaseLayout, snapshot, options.Bind, true)
			if bindErr != nil {
				return nil, bindErr
			}
			definition := bound.Definition
			metadata.Default = &definition
			spec.Dependencies = typeDependencies(spec.Dependencies, externalTypeDependencies(definition.Dependencies, base.ID())...)
			b.warnings = append(b.warnings, bound.Warnings...)
		}
		if previous != nil {
			for _, dependency := range previous.Dependencies {
				if !domainDependencyUsed(metadata, dependency) {
					spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(existing model.Dependency) bool {
						return existing == dependency
					})
				}
			}
		}
	case "N":
		metadata.NotNull = false
		metadata.Constraints = slices.DeleteFunc(metadata.Constraints, func(constraint model.ConstraintDescriptor) bool {
			return constraint.Kind == model.ConstraintNotNull
		})
	case "O":
		if !metadata.NotNull || !slices.ContainsFunc(metadata.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Kind == model.ConstraintNotNull }) {
			id, oid, allocateErr := state.allocate()
			if allocateErr != nil {
				return nil, allocateErr
			}
			reserved := make(map[string]bool, len(metadata.Constraints))
			for _, constraint := range metadata.Constraints {
				reserved[constraint.Name] = true
			}
			metadata.Constraints = append(metadata.Constraints, model.ConstraintDescriptor{ID: id, OID: oid, Name: domainNotNullConstraintName(base.Name().Name, reserved), Kind: model.ConstraintNotNull, Enforced: true, Validated: true})
			metadata.NotNull = true
			domainValidation = id
		}
	case "C":
		raw := statement.Def.GetConstraint()
		if raw == nil || raw.Contype != zpgparse.ConstrType_CONSTR_CHECK || raw.RawExpr == nil {
			return nil, bindError("42601", "ALTER DOMAIN ADD requires CHECK", 0)
		}
		if err := ordinaryDomainConstraint(raw); err != nil {
			return nil, err
		}
		for _, existing := range metadata.Constraints {
			if existing.Name == raw.Conname && raw.Conname != "" {
				return nil, bindError("42710", "domain constraint already exists", 0)
			}
		}
		id, oid, allocateErr := state.allocate()
		if allocateErr != nil {
			return nil, allocateErr
		}
		name := raw.Conname
		if name == "" {
			reserved := make(map[string]bool, len(metadata.Constraints))
			for _, existing := range metadata.Constraints {
				reserved[existing.Name] = true
			}
			name = domainConstraintName(base.Name().Name, 1, reserved)
		}
		definition, dependencies, defineErr := DefineDomainCheck(ctx, raw.RawExpr, base, id, metadata.BaseLayout, snapshot, options.Bind)
		if defineErr != nil {
			return nil, defineErr
		}
		added := model.ConstraintDescriptor{ID: id, OID: oid, Name: name, Kind: model.ConstraintCheck, Enforced: raw.IsEnforced, Validated: !raw.SkipValidation, Expression: definition}
		metadata.Constraints = append(metadata.Constraints, added)
		spec.Dependencies = typeDependencies(spec.Dependencies, externalTypeDependencies(dependencies, base.ID())...)
		if added.Validated {
			domainValidation = id
		}
	case "X":
		index := slices.IndexFunc(metadata.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Name == statement.Name })
		if index < 0 {
			if statement.MissingOk {
				return &BoundTypeCommand{dependencies: b.dependencies, warnings: []model.Error{{Code: "42704", Message: "domain constraint does not exist, skipping"}}, tag: "ALTER DOMAIN"}, nil
			}
			return nil, bindError("42704", "domain constraint does not exist", 0)
		}
		dropped := metadata.Constraints[index]
		if dropped.Kind == model.ConstraintNotNull {
			metadata.NotNull = false
		}
		metadata.Constraints = slices.Delete(metadata.Constraints, index, index+1)
		if dropped.Expression != nil {
			for _, dependency := range dropped.Expression.Dependencies {
				if !domainDependencyUsed(metadata, dependency) {
					spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(existing model.Dependency) bool {
						return existing == dependency
					})
				}
			}
		}
	case "V":
		index := slices.IndexFunc(metadata.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Name == statement.Name })
		if index < 0 {
			return nil, bindError("42704", "domain constraint does not exist", 0)
		}
		constraint := &metadata.Constraints[index]
		if constraint.Kind != model.ConstraintCheck || !constraint.Enforced {
			return nil, bindError("42809", "domain constraint cannot be validated", 0)
		}
		if constraint.Validated {
			return nil, bindError("55000", "domain constraint is already validated", 0)
		}
		constraint.Validated = true
		domainValidation = constraint.ID
	default:
		return nil, bindError("42601", "invalid ALTER DOMAIN command", 0)
	}
	next, err := nextTypeDescriptor(ctx, base, options, func(next *model.DescriptorSpec) error {
		next.Type = metadata
		next.Dependencies = spec.Dependencies
		return nil
	})
	if err != nil {
		return nil, err
	}
	updates := []TypeMutation{{Base: base, Next: next}}
	array, err := companionArrayMutation(ctx, b, snapshot, options, next)
	if err != nil {
		return nil, err
	}
	if array != nil {
		updates = append(updates, *array)
	}
	return &BoundTypeCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: "ALTER DOMAIN", cascade: statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE, domainValidation: domainValidation}, nil
}

func domainDependencyUsed(metadata *model.TypeMetadata, dependency model.Dependency) bool {
	if metadata == nil || metadata.Base == dependency.Object {
		return true
	}
	if metadata.Default != nil && slices.Contains(metadata.Default.Dependencies, dependency) {
		return true
	}
	for _, constraint := range metadata.Constraints {
		if constraint.Expression != nil && slices.Contains(constraint.Expression.Dependencies, dependency) {
			return true
		}
	}
	return false
}

func bindAlterComposite(ctx context.Context, statement *zpgparse.AlterTableStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.Relation == nil || statement.Relation.Catalogname != "" {
		return nil, bindError("42601", "missing or invalid ALTER TYPE name", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterTableStmt{AlterTableStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		if statement.MissingOk && isUndefinedType(err) {
			return &BoundTypeCommand{warnings: []model.Error{{Code: "42704", Message: "type does not exist, skipping"}}, tag: "ALTER TYPE"}, nil
		}
		return nil, err
	}
	baseSpec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if baseSpec.Type.DeclaredKind() != model.TypeKindComposite {
		return nil, bindError("42809", "ALTER TYPE attributes require a composite type", 0)
	}
	metadata := baseSpec.Type
	cascade := true
	for _, node := range statement.Cmds {
		command := node.GetAlterTableCmd()
		if command == nil {
			return nil, bindError("42601", "invalid ALTER TYPE command", 0)
		}
		switch command.Subtype {
		case zpgparse.AlterTableType_AT_AddColumn:
			column := command.Def.GetColumnDef()
			if column == nil || command.Name != "" || command.Recurse || len(column.Constraints) != 0 {
				return nil, bindError("42601", "invalid ADD ATTRIBUTE declaration", 0)
			}
			if slices.ContainsFunc(metadata.Fields, func(field model.ColumnDescriptor) bool { return field.Name == column.Colname }) {
				if command.MissingOk {
					continue
				}
				return nil, bindError("42701", "attribute already exists", 0)
			}
			layout, serial, err := b.declarationColumnType(column.TypeName)
			if err != nil {
				return nil, err
			}
			if serial {
				return nil, bindError("0A000", "serial is not valid for a composite attribute", 0)
			}
			if column.CollClause != nil {
				layout, err = b.indexColumnCollation(layout, column.CollClause.Collname)
				if err != nil {
					return nil, err
				}
			}
			cascade = cascade && command.Behavior == zpgparse.DropBehavior_DROP_CASCADE
			if metadata.FieldIDHighWatermark == model.ColumnID(math.MaxUint32) || len(metadata.Fields) >= options.Descriptors.MaxItems {
				return nil, model.ErrDescriptorLimit
			}
			metadata.FieldIDHighWatermark++
			metadata.Fields = append(metadata.Fields, model.ColumnDescriptor{ID: metadata.FieldIDHighWatermark, Name: column.Colname, State: model.StatePublic, Layout: layout, Nullable: true})
			metadata.Layout.Children = append(metadata.Layout.Children, layout)
		case zpgparse.AlterTableType_AT_DropColumn:
			index := slices.IndexFunc(metadata.Fields, func(field model.ColumnDescriptor) bool { return field.Name == command.Name })
			if index < 0 {
				if command.MissingOk {
					continue
				}
				return nil, bindError("42703", "attribute does not exist", 0)
			}
			cascade = cascade && command.Behavior == zpgparse.DropBehavior_DROP_CASCADE
			metadata.Fields = slices.Delete(metadata.Fields, index, index+1)
			metadata.Layout.Children = slices.Delete(metadata.Layout.Children, index, index+1)
		case zpgparse.AlterTableType_AT_AlterColumnType:
			index := slices.IndexFunc(metadata.Fields, func(field model.ColumnDescriptor) bool { return field.Name == command.Name })
			column := command.Def.GetColumnDef()
			if index < 0 || column == nil {
				return nil, bindError("42703", "attribute does not exist", 0)
			}
			layout, serial, err := b.declarationColumnType(column.TypeName)
			if err != nil {
				return nil, err
			}
			if serial {
				return nil, bindError("0A000", "serial is not valid for a composite attribute", 0)
			}
			if column.CollClause != nil {
				layout, err = b.indexColumnCollation(layout, column.CollClause.Collname)
				if err != nil {
					return nil, err
				}
			}
			cascade = cascade && command.Behavior == zpgparse.DropBehavior_DROP_CASCADE
			metadata.Fields[index].Layout = layout
			metadata.Layout.Children[index] = layout
		default:
			return nil, bindError("0A000", "unsupported ALTER TYPE attribute command", 0)
		}
	}
	baseNext, err := nextTypeDescriptor(ctx, base, options, func(spec *model.DescriptorSpec) error { spec.Type = metadata; return nil })
	if err != nil {
		return nil, err
	}
	updates := []TypeMutation{{Base: base, Next: baseNext}}
	array, err := companionArrayMutation(ctx, b, snapshot, options, baseNext)
	if err != nil {
		return nil, err
	}
	if array != nil {
		updates = append(updates, *array)
	}
	relation, repaired, err := companionCompositeRelationMutation(ctx, b, snapshot, options, baseNext)
	if err != nil {
		return nil, err
	}
	updates[0].Next = repaired
	if relation != nil {
		updates = append(updates, *relation)
	}

	return &BoundTypeCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: "ALTER TYPE", cascade: cascade}, nil
}

func bindDropTypes(ctx context.Context, statement *zpgparse.DropStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.Concurrent {
		return nil, bindError("42601", "invalid DROP TYPE", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_DropStmt{DropStmt: statement}})
	if err != nil {
		return nil, err
	}
	command := &BoundTypeCommand{tag: "DROP TYPE", cascade: statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE}
	if statement.RemoveType == zpgparse.ObjectType_OBJECT_DOMAIN {
		command.tag = "DROP DOMAIN"
	}
	seen := make(map[model.ObjectID]bool)
	for _, node := range statement.Objects {
		parts, err := typeNameFromNode(node)
		if err != nil {
			return nil, err
		}
		descriptor, err := resolveTypeDescriptor(ctx, b, parts)
		if err != nil {
			if statement.MissingOk && isUndefinedType(err) {
				command.warnings = append(command.warnings, model.Error{Code: "42704", Message: "type does not exist, skipping"})
				continue
			}
			return nil, err
		}
		spec, err := descriptor.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if statement.RemoveType == zpgparse.ObjectType_OBJECT_DOMAIN && spec.Type.DeclaredKind() != model.TypeKindDomain {
			return nil, bindError("42809", "DROP DOMAIN target is not a domain", 0)
		}
		if spec.Type.Relation != 0 {
			relation, err := snapshot.Descriptor(ctx, spec.Type.Relation)
			if err != nil {
				return nil, err
			}
			if relation.Descriptor.Kind() != model.ObjectCompositeRelation {
				return nil, bindError("2BP01", "cannot drop a relation row type independently", 0)
			}
		}
		queue := []*model.Descriptor{descriptor}
		for len(queue) != 0 {
			current := queue[0]
			queue = queue[1:]
			if seen[current.ID()] {
				continue
			}
			currentSpec, err := current.SpecWithin(ctx, options.Descriptors)
			if err != nil {
				return nil, err
			}
			seen[current.ID()] = true
			command.drops = append(command.drops, current)
			b.dependency(current, 0)
			for _, id := range typeOwnedObjects(currentSpec.Type) {
				if seen[id] {
					continue
				}
				published, err := snapshot.Descriptor(ctx, id)
				if err != nil {
					return nil, err
				}
				queue = append(queue, published.Descriptor)
			}
		}
	}
	command.dependencies = b.dependencies
	command.warnings = append(command.warnings, b.warnings...)
	return command, nil
}

func typeOwnedObjects(metadata *model.TypeMetadata) []model.ObjectID {
	if metadata == nil {
		return nil
	}
	result := []model.ObjectID{metadata.Array, metadata.Relation}
	if metadata.Range != nil {
		if metadata.DeclaredKind() == model.TypeKindRange {
			result = append(result, metadata.Range.Multirange)
		}
	}
	return slices.DeleteFunc(result, func(id model.ObjectID) bool { return id == 0 })
}

func bindRenameDomainConstraint(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.Object == nil || statement.Relation != nil || statement.Subname == "" {
		return nil, bindError("42601", "invalid ALTER DOMAIN RENAME CONSTRAINT", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := typeNameFromNode(statement.Object)
	if err != nil {
		return nil, err
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindDomain {
		return nil, bindError("42809", "ALTER DOMAIN RENAME CONSTRAINT requires a domain", 0)
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	index := slices.IndexFunc(spec.Type.Constraints, func(constraint model.ConstraintDescriptor) bool {
		return constraint.Name == statement.Subname
	})
	if index < 0 {
		return nil, bindError("42704", "domain constraint does not exist", 0)
	}
	if slices.ContainsFunc(spec.Type.Constraints, func(constraint model.ConstraintDescriptor) bool {
		return constraint.Name == statement.Newname
	}) {
		return nil, bindError("42710", "domain constraint already exists", 0)
	}
	next, err := nextTypeDescriptor(ctx, base, options, func(next *model.DescriptorSpec) error {
		next.Type.Constraints[index].Name = statement.Newname
		return nil
	})
	if err != nil {
		return nil, err
	}
	updates := []TypeMutation{{Base: base, Next: next}}
	array, err := companionArrayMutation(ctx, b, snapshot, options, next)
	if err != nil {
		return nil, err
	}
	if array != nil {
		updates = append(updates, *array)
	}
	return &BoundTypeCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: "ALTER DOMAIN"}, nil
}

func bindRenameCompositeAttribute(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.Relation == nil || statement.Object != nil || statement.Subname == "" || statement.Relation.Catalogname != "" {
		return nil, bindError("42601", "invalid ALTER TYPE RENAME ATTRIBUTE", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts := []string{statement.Relation.Relname}
	if statement.Relation.Schemaname != "" {
		parts = []string{statement.Relation.Schemaname, statement.Relation.Relname}
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		return nil, err
	}
	spec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if spec.Type == nil || spec.Type.DeclaredKind() != model.TypeKindComposite {
		return nil, bindError("42809", "ALTER TYPE RENAME ATTRIBUTE requires a composite type", 0)
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	index := slices.IndexFunc(spec.Type.Fields, func(field model.ColumnDescriptor) bool {
		return field.Name == statement.Subname
	})
	if index < 0 {
		return nil, bindError("42703", "attribute does not exist", 0)
	}
	if slices.ContainsFunc(spec.Type.Fields, func(field model.ColumnDescriptor) bool {
		return field.Name == statement.Newname
	}) {
		return nil, bindError("42701", "attribute already exists", 0)
	}
	next, err := nextTypeDescriptor(ctx, base, options, func(next *model.DescriptorSpec) error {
		next.Type.Fields[index].Name = statement.Newname
		return nil
	})
	if err != nil {
		return nil, err
	}
	updates := []TypeMutation{{Base: base, Next: next}}
	array, err := companionArrayMutation(ctx, b, snapshot, options, next)
	if err != nil {
		return nil, err
	}
	if array != nil {
		updates = append(updates, *array)
	}
	relation, repaired, err := companionCompositeRelationMutation(ctx, b, snapshot, options, next)
	if err != nil {
		return nil, err
	}
	updates[0].Next = repaired
	if relation != nil {
		updates = append(updates, *relation)
	}
	return &BoundTypeCommand{
		updates:      updates,
		dependencies: b.dependencies,
		warnings:     b.warnings,
		tag:          "ALTER TYPE",
		cascade:      statement.Behavior == zpgparse.DropBehavior_DROP_CASCADE,
	}, nil
}

func bindRenameType(ctx context.Context, statement *zpgparse.RenameStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.Object == nil || statement.Subname != "" {
		return nil, bindError("42601", "invalid ALTER TYPE RENAME", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_RenameStmt{RenameStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := typeNameFromNode(statement.Object)
	if err != nil {
		return nil, err
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		return nil, err
	}
	if statement.RenameType == zpgparse.ObjectType_OBJECT_DOMAIN {
		spec, err := base.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if spec.Type.DeclaredKind() != model.TypeKindDomain {
			return nil, bindError("42809", "ALTER DOMAIN target is not a domain", 0)
		}
	}
	if err := declarationName(statement.Newname); err != nil {
		return nil, err
	}
	if _, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: base.Name().Parent, Kind: model.ObjectType, Name: statement.Newname}); err == nil {
		return nil, bindError("42710", "type already exists", 0)
	} else if !isMissingObject(err) {
		return nil, err
	}
	next, err := nextTypeDescriptor(ctx, base, options, func(spec *model.DescriptorSpec) error { spec.Name.Name = statement.Newname; return nil })
	if err != nil {
		return nil, err
	}
	updates := []TypeMutation{{Base: base, Next: next}}
	array, err := companionArrayMutation(ctx, b, snapshot, options, next)
	if err != nil {
		return nil, err
	}
	if array != nil {
		if array.Base.Name().Name == arrayTypeName(base.Name().Name) {
			nextName := arrayTypeName(statement.Newname)
			if _, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: base.Name().Parent, Kind: model.ObjectType, Name: nextName}); err == nil {
				return nil, bindError("42710", "array type name already exists", 0)
			} else if !isMissingObject(err) {
				return nil, err
			}
			arraySpec, err := array.Next.SpecWithin(ctx, options.Descriptors)
			if err != nil {
				return nil, err
			}
			arraySpec.Name.Name = nextName
			array.Next, err = model.NewDescriptor(ctx, arraySpec, options.Descriptors)
			if err != nil {
				return nil, err
			}
		}
		updates = append(updates, *array)
	}
	relation, repaired, err := companionCompositeRelationMutation(ctx, b, snapshot, options, next)
	if err != nil {
		return nil, err
	}
	updates[0].Next = repaired
	if relation != nil {
		if err := ensureRelationTargetVacant(ctx, b, snapshot, base.Name().Parent, statement.Newname, relation.Base.ID()); err != nil {
			return nil, err
		}
		relationSpec, err := relation.Next.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		relationSpec.Name.Name = statement.Newname
		relation.Next, err = model.NewDescriptor(ctx, relationSpec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		updates = append(updates, *relation)
	}
	return &BoundTypeCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: "ALTER TYPE"}, nil
}

func ownedTypeDescriptors(ctx context.Context, snapshot CatalogSnapshot, root *model.Descriptor, options CreateDeclarationOptions) ([]*model.Descriptor, error) {
	queue := []*model.Descriptor{root}
	seen := make(map[model.ObjectID]bool)
	var result []*model.Descriptor
	for len(queue) != 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current.ID()] {
			continue
		}
		if len(result) >= options.Descriptors.MaxItems {
			return nil, model.ErrDescriptorLimit
		}
		spec, err := current.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		seen[current.ID()] = true
		result = append(result, current)
		for _, id := range typeOwnedObjects(spec.Type) {
			if seen[id] {
				continue
			}
			published, err := snapshot.Descriptor(ctx, id)
			if err != nil {
				return nil, err
			}
			queue = append(queue, published.Descriptor)
		}
	}
	return result, nil
}

func bindSetTypeSchema(ctx context.Context, statement *zpgparse.AlterObjectSchemaStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions) (*BoundTypeCommand, error) {
	if statement == nil || statement.Object == nil || statement.Newschema == "" {
		return nil, bindError("42601", "invalid ALTER TYPE SET SCHEMA", 0)
	}
	b, err := declarationBinder(ctx, snapshot, options, &zpgparse.Node{Node: &zpgparse.Node_AlterObjectSchemaStmt{AlterObjectSchemaStmt: statement}})
	if err != nil {
		return nil, err
	}
	parts, err := typeNameFromNode(statement.Object)
	if err != nil {
		return nil, err
	}
	base, err := resolveTypeDescriptor(ctx, b, parts)
	if err != nil {
		if statement.MissingOk && isUndefinedType(err) {
			return &BoundTypeCommand{warnings: []model.Error{{Code: "42704", Message: "type does not exist, skipping"}}, tag: "ALTER TYPE"}, nil
		}
		return nil, err
	}
	if statement.ObjectType == zpgparse.ObjectType_OBJECT_DOMAIN {
		spec, err := base.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if spec.Type.DeclaredKind() != model.TypeKindDomain {
			return nil, bindError("42809", "ALTER DOMAIN target is not a domain", 0)
		}
	}
	baseSpec, err := base.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return nil, err
	}
	if baseSpec.Type.Relation != 0 {
		relation, err := snapshot.Descriptor(ctx, baseSpec.Type.Relation)
		if err != nil {
			return nil, err
		}
		if relation.Descriptor.Kind() != model.ObjectCompositeRelation {
			return nil, bindError("2BP01", "cannot move a relation row type independently", 0)
		}
	}
	parent, err := snapshot.Resolve(ctx, model.NamespaceName{Kind: model.ObjectSchema, Name: statement.Newschema})
	if err != nil {
		return nil, err
	}
	owned, err := ownedTypeDescriptors(ctx, snapshot, base, options)
	if err != nil {
		return nil, err
	}
	for _, descriptor := range owned {
		if descriptor.Kind() == model.ObjectCompositeRelation {
			if err := ensureRelationTargetVacant(ctx, b, snapshot, parent.Descriptor.ID(), descriptor.Name().Name, descriptor.ID()); err != nil {
				return nil, err
			}
			continue
		}
		if _, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: parent.Descriptor.ID(), Kind: descriptor.Kind(), Name: descriptor.Name().Name}); err == nil {
			return nil, bindError("42710", "type already exists in target schema", 0)
		} else if !isMissingObject(err) {
			return nil, err
		}
	}
	b.dependency(parent.Descriptor, 0)
	versions := make(map[model.ObjectID]uint64, len(owned))
	for _, descriptor := range owned {
		if descriptor.Version() == ^uint64(0) {
			return nil, bindError("54000", "type version exhausted", 0)
		}
		versions[descriptor.ID()] = descriptor.Version() + 1
	}
	updates := make([]TypeMutation, 0, len(owned))
	for _, descriptor := range owned {
		next, err := nextTypeDescriptor(ctx, descriptor, options, func(spec *model.DescriptorSpec) error {
			spec.Name.Parent = parent.Descriptor.ID()
			spec.Dependencies = slices.DeleteFunc(spec.Dependencies, func(dependency model.Dependency) bool {
				return dependency.Object == base.Name().Parent
			})
			for object, version := range versions {
				if slices.ContainsFunc(spec.Dependencies, func(dependency model.Dependency) bool { return dependency.Object == object }) {
					spec.Dependencies = replaceTypeDependency(spec.Dependencies, object, version)
				}
			}
			spec.Dependencies = typeDependencies(spec.Dependencies, model.Dependency{Object: parent.Descriptor.ID(), Version: parent.Descriptor.Version()})
			return nil
		})
		if err != nil {
			return nil, err
		}
		updates = append(updates, TypeMutation{Base: descriptor, Next: next})
		b.dependency(descriptor, 0)
	}
	return &BoundTypeCommand{updates: updates, dependencies: b.dependencies, warnings: b.warnings, tag: "ALTER TYPE"}, nil
}

