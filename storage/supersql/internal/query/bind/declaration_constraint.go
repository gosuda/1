package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func declarationConstraintAttributes(c *zpgparse.Constraint, kind model.ConstraintKind) (model.ConstraintDescriptor, error) {
	constraint := model.ConstraintDescriptor{
		Kind:              kind,
		Deferrable:        c.Deferrable,
		InitiallyDeferred: c.Initdeferred,
		Validated:         !c.SkipValidation,
		Enforced:          true,
		NoInherit:         c.IsNoInherit,
	}
	if c.Initdeferred && !c.Deferrable {
		return model.ConstraintDescriptor{}, bindError("42601", "INITIALLY DEFERRED requires DEFERRABLE", int(c.Location)+1)
	}
	switch kind {
	case model.ConstraintCheck, model.ConstraintNotNull:
		if c.Deferrable || c.Initdeferred {
			return model.ConstraintDescriptor{}, bindError("0A000", "CHECK and NOT NULL constraints cannot be DEFERRABLE", int(c.Location)+1)
		}
		if c.SkipValidation && kind == model.ConstraintNotNull {
			return model.ConstraintDescriptor{}, bindError("0A000", "NOT NULL constraint does not support NOT VALID", int(c.Location)+1)
		}
	case model.ConstraintPrimaryKey, model.ConstraintUnique, model.ConstraintExclude:
		if c.SkipValidation {
			return model.ConstraintDescriptor{}, bindError("0A000", "constraint does not support NOT VALID", int(c.Location)+1)
		}
	}
	if c.IsNoInherit && kind != model.ConstraintCheck && kind != model.ConstraintNotNull {
		return model.ConstraintDescriptor{}, bindError("42601", "NO INHERIT is not valid for this constraint", int(c.Location)+1)
	}
	if kind == model.ConstraintCheck || kind == model.ConstraintForeignKey {
		constraint.Enforced = c.IsEnforced
		if !constraint.Enforced && constraint.Validated {
			constraint.Validated = false
		}
	}
	return constraint, nil
}

func foreignKeyAction(code string) (string, error) {
	switch code {
	case "", "a":
		return "no action", nil
	case "r":
		return "restrict", nil
	case "c":
		return "cascade", nil
	case "n":
		return "set null", nil
	case "d":
		return "set default", nil
	default:
		return "", bindError("42601", "invalid foreign key action", 0)
	}
}

func foreignKeyMatch(code string) (string, error) {
	switch code {
	case "", "s":
		return "simple", nil
	case "f":
		return "full", nil
	case "p":
		return "", bindError("0A000", "MATCH PARTIAL is not implemented", 0)
	default:
		return "", bindError("42601", "invalid foreign key match type", 0)
	}
}

func (s *declarationState) bindRelationalConstraints(ctx context.Context, spec *model.DescriptorSpec, raw []tableConstraint, columnNames map[string]model.ColumnID) ([]*model.Descriptor, error) {
	var indexes []*model.Descriptor
	for _, item := range raw {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var descriptor *model.Descriptor
		var constraint model.ConstraintDescriptor
		var index *model.IndexMetadata
		var label string
		var err error
		switch item.raw.Contype {
		case zpgparse.ConstrType_CONSTR_FOREIGN:
			constraint, descriptor, err = s.bindForeignKey(ctx, spec, item, columnNames)
			label = "fkey"
		case zpgparse.ConstrType_CONSTR_EXCLUSION:
			constraint, index, err = s.bindExclusion(ctx, spec, item, columnNames)
			label = "excl"
		default:
			return nil, bindError("42601", "invalid relational constraint kind", int(item.raw.Location)+1)
		}
		if err != nil {
			return nil, err
		}
		addition := declarationKeyName(spec.Table.Columns, constraint.Columns)
		constraint.Name, err = s.constraintName(item.raw.Conname, addition, label, item.raw.Contype == zpgparse.ConstrType_CONSTR_EXCLUSION)
		if err != nil {
			return nil, err
		}
		constraint.ID, constraint.OID, err = s.allocate()
		if err != nil {
			return nil, err
		}
		if descriptor != nil {
			constraint.ReferencedObject = descriptor.ID()
		}
		if index != nil {
			indexID, indexOID, allocateErr := s.allocate()
			if allocateErr != nil {
				return nil, allocateErr
			}
			parentVersion := uint64(0)
			for _, dependency := range s.b.dependencies {
				if dependency.Object == s.parent {
					parentVersion = dependency.Version
					break
				}
			}
			if parentVersion == 0 {
				return nil, model.ErrInvalidDescriptor
			}
			dependencies := []model.Dependency{{Object: spec.ID, Version: spec.Version}, {Object: s.parent, Version: parentVersion}}
			for _, column := range constraint.Columns {
				dependencies = append(dependencies, model.Dependency{Object: spec.ID, Version: spec.Version, Column: column})
			}
			created, createErr := model.NewDescriptor(ctx, model.DescriptorSpec{
				ID: indexID, OID: indexOID,
				Name: model.NamespaceName{Parent: s.parent, Kind: model.ObjectIndex, Name: constraint.Name},
				Version: 1, State: model.StatePublic, Physical: s.options.Physical,
				Dependencies: dependencies, Index: index,
			}, s.options.Descriptors)
			if createErr != nil {
				return nil, createErr
			}
			constraint.Index = indexID
			spec.Table.Indexes = append(spec.Table.Indexes, indexID)
			indexes = append(indexes, created)
		}
		spec.Table.Constraints = append(spec.Table.Constraints, constraint)
	}
	return indexes, nil
}

func (s *declarationState) bindForeignKey(ctx context.Context, spec *model.DescriptorSpec, item tableConstraint, columnNames map[string]model.ColumnID) (model.ConstraintDescriptor, *model.Descriptor, error) {
	c := item.raw
	constraint, err := declarationConstraintAttributes(c, model.ConstraintForeignKey)
	if err != nil {
		return constraint, nil, err
	}
	constraint.Columns, err = declarationColumns(c.FkAttrs, item.column, columnNames)
	if err != nil {
		return constraint, nil, err
	}
	constraint.Match, err = foreignKeyMatch(c.FkMatchtype)
	if err != nil {
		return constraint, nil, err
	}
	constraint.OnUpdate, err = foreignKeyAction(c.FkUpdAction)
	if err != nil {
		return constraint, nil, err
	}
	constraint.OnDelete, err = foreignKeyAction(c.FkDelAction)
	if err != nil {
		return constraint, nil, err
	}
	constraint.OnDeleteColumns, err = declarationColumnsOptional(c.FkDelSetCols, columnNames)
	if err != nil {
		return constraint, nil, err
	}
	if constraint.OnDelete != "set null" && constraint.OnDelete != "set default" && len(constraint.OnDeleteColumns) != 0 {
		return constraint, nil, bindError("42601", "column list is only valid for SET NULL or SET DEFAULT", int(c.Location)+1)
	}
	for _, column := range constraint.OnDeleteColumns {
		if !slices.Contains(constraint.Columns, column) {
			return constraint, nil, bindError("42703", "foreign key action column is not constrained", int(c.Location)+1)
		}
	}
	if c.Pktable == nil || c.Pktable.Catalogname != "" || c.Pktable.Relname == "" {
		return constraint, nil, bindError("42601", "foreign key requires a referenced table", int(c.Location)+1)
	}
	self := c.Pktable.Relname == s.tableName && (c.Pktable.Schemaname == "" || c.Pktable.Schemaname == declarationSchemaName(s.b, spec.Name.Parent))
	var referenced *model.Descriptor
	var referencedTable *model.TableMetadata
	var referencedVersion uint64
	if self {
		constraint.ReferencedObject = spec.ID
		referencedTable, referencedVersion = spec.Table, spec.Version
	} else {
		parts := []string{c.Pktable.Relname}
		if c.Pktable.Schemaname != "" {
			parts = []string{c.Pktable.Schemaname, c.Pktable.Relname}
		}
		referenced, _, err = s.b.resolve(parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(c.Pktable.Location)+1)
		if err != nil {
			return constraint, nil, err
		}
		referencedSpec, specErr := referenced.SpecWithin(ctx, s.options.Descriptors)
		if specErr != nil {
			return constraint, nil, specErr
		}
		if referencedSpec.Table == nil {
			return constraint, nil, model.ErrInvalidDescriptor
		}
		referencedTable, referencedVersion = referencedSpec.Table, referenced.Version()
	}
	refNames := make(map[string]model.ColumnID, len(referencedTable.Columns))
	for _, column := range referencedTable.Columns {
		if column.State == model.StatePublic && !column.SystemIdentity {
			refNames[column.Name] = column.ID
		}
	}
	if len(c.PkAttrs) == 0 {
		for _, candidate := range referencedTable.Constraints {
			if candidate.Kind == model.ConstraintPrimaryKey {
				constraint.ReferencedColumns = slices.Clone(candidate.Columns)
				break
			}
		}
		if len(constraint.ReferencedColumns) == 0 {
			return constraint, nil, bindError("42830", "referenced table has no primary key", int(c.Location)+1)
		}
	} else {
		constraint.ReferencedColumns, err = declarationColumns(c.PkAttrs, 0, refNames)
		if err != nil {
			return constraint, nil, err
		}
	}
	if len(constraint.Columns) != len(constraint.ReferencedColumns) {
		return constraint, nil, bindError("42830", "foreign key column count does not match referenced key", int(c.Location)+1)
	}
	for _, candidate := range referencedTable.Constraints {
		if candidate.Kind != model.ConstraintPrimaryKey && candidate.Kind != model.ConstraintUnique || !slices.Equal(candidate.Columns, constraint.ReferencedColumns) {
			continue
		}
		if candidate.Deferrable {
			return constraint, nil, bindError("55000", "cannot use a deferrable unique constraint for referenced keys", int(c.Location)+1)
		}
		constraint.ReferencedConstraint, constraint.ReferencedIndex = candidate.ID, candidate.Index
		break
	}
	if constraint.ReferencedConstraint == 0 || constraint.ReferencedIndex == 0 {
		return constraint, nil, bindError("42830", "there is no unique constraint matching given keys", int(c.Location)+1)
	}
	for i, localID := range constraint.Columns {
		localPosition := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool { return column.ID == localID })
		if localPosition < 0 {
			return constraint, nil, bindError("42703", "foreign key column does not exist", int(c.Location)+1)
		}
		local := spec.Table.Columns[localPosition]
		referencedID := constraint.ReferencedColumns[i]
		position := slices.IndexFunc(referencedTable.Columns, func(column model.ColumnDescriptor) bool { return column.ID == referencedID })
		if position < 0 {
			return constraint, nil, bindError("42804", "foreign key columns have incompatible types", int(c.Location)+1)
		}
		parent := referencedTable.Columns[position]
		if !foreignKeyEqualityCompatible(s.b, local.Layout, parent.Layout) {
			return constraint, nil, bindError("42804", "foreign key columns have incompatible B-tree equality", int(c.Location)+1)
		}
		parentForeign, resolveErr := s.b.resolveOperator("=", []expression{{typ: parent.Layout}, {typ: local.Layout}}, int(c.Location)+1)
		if resolveErr != nil {
			return constraint, nil, resolveErr
		}
		parentParent, resolveErr := s.b.resolveOperator("=", []expression{{typ: parent.Layout}, {typ: parent.Layout}}, int(c.Location)+1)
		if resolveErr != nil {
			return constraint, nil, resolveErr
		}
		foreignForeign, resolveErr := s.b.resolveOperator("=", []expression{{typ: local.Layout}, {typ: local.Layout}}, int(c.Location)+1)
		if resolveErr != nil {
			return constraint, nil, resolveErr
		}
		constraint.ParentForeignEqualityOperators = append(constraint.ParentForeignEqualityOperators, parentForeign.operator)
		constraint.ParentEqualityOperators = append(constraint.ParentEqualityOperators, parentParent.operator)
		constraint.ForeignEqualityOperators = append(constraint.ForeignEqualityOperators, foreignForeign.operator)
	}
	if !self {
		s.b.dependencies = appendIndexDependency(s.b.dependencies, model.Dependency{Object: referenced.ID(), Version: referencedVersion})
		for _, column := range constraint.ReferencedColumns {
			s.b.dependencies = appendIndexDependency(s.b.dependencies, model.Dependency{Object: referenced.ID(), Version: referencedVersion, Column: column})
		}
		index, indexErr := s.b.catalog.Descriptor(ctx, constraint.ReferencedIndex)
		if indexErr != nil {
			return constraint, nil, indexErr
		}
		if index.Descriptor == nil || index.Descriptor.Kind() != model.ObjectIndex || index.Descriptor.State() != model.StatePublic {
			return constraint, nil, bindError("42830", "referenced unique index is unavailable", int(c.Location)+1)
		}
		indexMetadata, ok := index.Descriptor.Index()
		if !ok || !indexMetadata.Valid() || indexMetadata.Table != referenced.ID() {
			return constraint, nil, bindError("42830", "referenced unique index is unavailable", int(c.Location)+1)
		}
		s.b.dependency(index.Descriptor, 0)
	}
	return constraint, referenced, nil
}

func declarationSchemaName(b *binder, id model.ObjectID) string {
	published, err := b.catalog.Descriptor(b.ctx, id)
	if err != nil || published.Descriptor == nil {
		return ""
	}
	return published.Descriptor.Name().Name
}

func foreignKeyEqualityCompatible(b *binder, local, parent model.TypeLayout) bool {
	if local.Type.Storage != parent.Type.Storage || local.Type.Collation != parent.Type.Collation {
		return false
	}
	if local.Type.Storage == model.TypeArray || local.Type.Storage == model.TypeComposite {
		if local.Type.ID != parent.Type.ID || len(local.Children) != len(parent.Children) {
			return false
		}
		for i := range local.Children {
			if !foreignKeyEqualityCompatible(b, local.Children[i], parent.Children[i]) {
				return false
			}
		}
		return true
	}
	localType, err := b.indexOperatorClassType(local)
	if err != nil {
		return false
	}
	parentType, err := b.indexOperatorClassType(parent)
	if err != nil {
		return false
	}
	localClass, localOK := builtin.LookupDefaultOperatorClass("btree", localType.ID)
	parentClass, parentOK := builtin.LookupDefaultOperatorClass("btree", parentType.ID)
	if !localOK || !parentOK || localClass.Family != parentClass.Family {
		return false
	}
	if localClass.KeyType != 0 && localClass.KeyType != localClass.InputType || parentClass.KeyType != 0 && parentClass.KeyType != parentClass.InputType {
		return false
	}
	return builtin.KeyOrderingEquivalent(localType, localClass.InputType) && builtin.KeyOrderingEquivalent(parentType, parentClass.InputType)
}

func declarationColumnsOptional(raw []*zpgparse.Node, columns map[string]model.ColumnID) ([]model.ColumnID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	return declarationColumns(raw, 0, columns)
}

func (s *declarationState) bindExclusion(ctx context.Context, spec *model.DescriptorSpec, item tableConstraint, columnNames map[string]model.ColumnID) (model.ConstraintDescriptor, *model.IndexMetadata, error) {
	c := item.raw
	constraint, err := declarationConstraintAttributes(c, model.ConstraintExclude)
	if err != nil {
		return constraint, nil, err
	}
	if c.AccessMethod != "" && c.AccessMethod != "btree" {
		return constraint, nil, bindError("0A000", "only B-tree exclusion constraints are supported", int(c.Location)+1)
	}
	if len(c.Exclusions) == 0 {
		return constraint, nil, bindError("42601", "exclusion constraint requires at least one element", int(c.Location)+1)
	}
	index := &model.IndexMetadata{Owner: s.options.Owner, Table: spec.ID}
	for _, raw := range c.Exclusions {
		pair := raw.GetList()
		if pair == nil || len(pair.Items) != 2 {
			return constraint, nil, bindError("42601", "invalid exclusion element", int(c.Location)+1)
		}
		elem := pair.Items[0].GetIndexElem()
		if elem == nil || elem.Name == "" || elem.Expr != nil || elem.Indexcolname != "" || len(elem.Opclassopts) != 0 {
			return constraint, nil, bindError("42601", "B-tree equality exclusion requires a column key", int(c.Location)+1)
		}
		columnID, ok := columnNames[elem.Name]
		if !ok {
			return constraint, nil, bindError("42703", "exclusion column does not exist", int(c.Location)+1)
		}
		if slices.Contains(constraint.Columns, columnID) {
			return constraint, nil, bindError("42701", "exclusion column specified more than once", int(c.Location)+1)
		}
		parts, namesErr := names(pair.Items[1].GetList().GetItems())
		if namesErr != nil || len(parts) == 0 || len(parts) > 2 || len(parts) == 2 && parts[0] != "pg_catalog" || parts[len(parts)-1] != "=" {
			return constraint, nil, bindError("0A000", "only equality exclusion operators are supported", int(c.Location)+1)
		}
		position := slices.IndexFunc(spec.Table.Columns, func(column model.ColumnDescriptor) bool { return column.ID == columnID })
		if position < 0 {
			return constraint, nil, model.ErrInvalidDescriptor
		}
		layout := spec.Table.Columns[position].Layout
		resolved, resolveErr := s.b.resolveOperator("=", []expression{{typ: layout}, {typ: layout}}, int(c.Location)+1)
		if resolveErr != nil {
			return constraint, nil, resolveErr
		}
		key := model.KeyElement{Column: columnID, Type: layout.Type, Children: layout.Children, Collation: layout.Type.Collation}
		if len(elem.Collation) != 0 {
			collated, collateErr := s.b.indexColumnCollation(layout, elem.Collation)
			if collateErr != nil {
				return constraint, nil, collateErr
			}
			key.Type, key.Children, key.Collation = collated.Type, collated.Children, collated.Type.Collation
		}
		if len(elem.Opclass) != 0 {
			operatorType, typeErr := s.b.indexOperatorClassType(layout)
			if typeErr != nil {
				return constraint, nil, typeErr
			}
			key.OperatorClass, err = indexOperatorClass(operatorType, elem.Opclass)
			if err != nil {
				return constraint, nil, err
			}
		}
		switch elem.Ordering {
		case zpgparse.SortByDir_SORTBY_DEFAULT, zpgparse.SortByDir_SORTBY_ASC:
		case zpgparse.SortByDir_SORTBY_DESC:
			key.Descending = true
		default:
			return constraint, nil, bindError("42601", "invalid exclusion ordering", int(c.Location)+1)
		}
		key.NullsFirst = key.Descending
		switch elem.NullsOrdering {
		case zpgparse.SortByNulls_SORTBY_NULLS_DEFAULT:
		case zpgparse.SortByNulls_SORTBY_NULLS_FIRST:
			key.NullsFirst = true
		case zpgparse.SortByNulls_SORTBY_NULLS_LAST:
			key.NullsFirst = false
		default:
			return constraint, nil, bindError("42601", "invalid exclusion NULL ordering", int(c.Location)+1)
		}
		constraint.Columns = append(constraint.Columns, columnID)
		constraint.ExcludeOperators = append(constraint.ExcludeOperators, resolved.operator)
		index.Keys = append(index.Keys, key)
	}
	if len(c.Including) != 0 {
		index.Include, err = declarationColumns(c.Including, 0, columnNames)
		if err != nil {
			return constraint, nil, err
		}
	}
	if c.WhereClause != nil {
		table := *spec.Table
		table.Constraints = nil
		pendingSpec := *spec
		pendingSpec.Table = &table
		pendingSpec.Dependencies = slices.Clone(s.b.dependencies)
		pending, createErr := model.NewDescriptor(ctx, pendingSpec, s.options.Descriptors)
		if createErr != nil {
			return constraint, nil, createErr
		}
		definition, _, defineErr := defineIndexExpression(ctx, c.WhereClause, pending, table.Columns, s.b.catalog, s.options.Bind, true)
		if defineErr != nil {
			return constraint, nil, defineErr
		}
		remaining := s.options.Descriptors.MaxBytes
		if admitErr := admitIndexDefinition(definition, &remaining); admitErr != nil {
			return constraint, nil, admitErr
		}
		index.Predicate = definition
		for _, dependency := range definition.Dependencies {
			s.b.dependencies = appendIndexDependency(s.b.dependencies, dependency)
		}
	}
	return constraint, index, nil
}
