package model

import (
	"context"
	"slices"
)

func (k TypeKind) Valid() bool {
	return k >= TypeKindDomain && k <= TypeKindShell
}

func (m TypeMetadata) DeclaredKind() TypeKind {
	return m.Kind
}

func (m TypeMetadata) EnumElement(label string) (OID, int, bool) {
	index := slices.Index(m.Elements, label)
	if index < 0 || index >= len(m.ElementOIDs) {
		return 0, 0, false
	}
	return m.ElementOIDs[index], index, true
}

func (m TypeMetadata) EnumLabel(oid OID) (string, int, bool) {
	index := slices.Index(m.ElementOIDs, oid)
	if index < 0 || index >= len(m.Elements) {
		return "", 0, false
	}
	return m.Elements[index], index, true
}

func (m TypeMetadata) EnumElementUnsafe(oid OID) bool {
	return slices.Contains(m.UnsafeElementOIDs, oid)
}

func RelationRowTypeFields(ctx context.Context, relation *Descriptor, rowType ObjectID) ([]ColumnDescriptor, ColumnID, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if relation == nil || rowType == 0 {
		return nil, 0, ErrInvalidDescriptor
	}
	spec, err := relation.Spec(ctx)
	if err != nil {
		return nil, 0, err
	}
	var columns []ColumnDescriptor
	var highWatermark ColumnID
	switch relation.Kind() {
	case ObjectTable:
		if spec.Table == nil || spec.Table.RowType != rowType {
			return nil, 0, ErrInvalidDescriptor
		}
		columns = spec.Table.Columns
		highWatermark = spec.Table.ColumnIDHighWatermark
	case ObjectView:
		if spec.View == nil || spec.View.RowType != rowType {
			return nil, 0, ErrInvalidDescriptor
		}
		columns = spec.View.Columns
	case ObjectMaterializedView:
		if spec.Table == nil || spec.View == nil || spec.Table.RowType != rowType || spec.View.RowType != rowType {
			return nil, 0, ErrInvalidDescriptor
		}
		columns = spec.View.Columns
		highWatermark = spec.Table.ColumnIDHighWatermark
	case ObjectCompositeRelation:
		if spec.CompositeRelation == nil || spec.CompositeRelation.RowType != rowType {
			return nil, 0, ErrInvalidDescriptor
		}
		columns = spec.CompositeRelation.Columns
		highWatermark = spec.CompositeRelation.FieldIDHighWatermark
	default:
		return nil, 0, ErrInvalidDescriptor
	}
	fields := make([]ColumnDescriptor, 0, len(columns))
	if highWatermark == 0 {
		for _, column := range columns {
			if column.ID > highWatermark {
				highWatermark = column.ID
			}
		}
	}
	for _, column := range columns {
		if column.SystemIdentity || column.State != StatePublic {
			continue
		}
		fields = append(fields, ColumnDescriptor{
			ID:          column.ID,
			Name:        column.Name,
			Description: column.Description,
			State:       column.State,
			Layout:      column.Layout,
			Nullable:    true,
		})
	}
	return fields, highWatermark, nil
}

func NewImplicitRelationRowTypes(ctx context.Context, relation *Descriptor, rowTypeID ObjectID, rowTypeOID OID, arrayID ObjectID, arrayOID OID, arrayName string, owner OID, limits DescriptorLimits) (*Descriptor, *Descriptor, error) {
	if relation == nil || rowTypeID == 0 || rowTypeOID == 0 || arrayID == 0 || arrayOID == 0 || rowTypeID == arrayID || rowTypeOID == arrayOID || arrayName == "" || owner == 0 {
		return nil, nil, ErrInvalidDescriptor
	}
	fields, highWatermark, err := RelationRowTypeFields(ctx, relation, rowTypeID)
	if err != nil {
		return nil, nil, err
	}
	children := make([]TypeLayout, len(fields))
	for i := range fields {
		children[i] = fields[i].Layout
	}
	layout := TypeLayout{Type: Type{ID: TypeID(rowTypeOID), Modifier: -1, Storage: TypeComposite}, Children: children}
	parent := relation.Name().Parent
	relationSpec, err := relation.Spec(ctx)
	if err != nil {
		return nil, nil, err
	}
	parentVersion := uint64(0)
	for _, dependency := range relationSpec.Dependencies {
		if dependency.Object == parent && dependency.Column == 0 {
			parentVersion = dependency.Version
			break
		}
	}
	if parentVersion == 0 {
		return nil, nil, ErrInvalidDescriptor
	}
	dependencies := []Dependency{{Object: parent, Version: parentVersion}, {Object: relation.ID(), Version: relation.Version()}}
	slices.SortFunc(dependencies, func(left, right Dependency) int {
		if left.Object < right.Object {
			return -1
		}
		if left.Object > right.Object {
			return 1
		}
		return 0
	})
	rowType, err := NewDescriptor(ctx, DescriptorSpec{
		ID: rowTypeID, OID: rowTypeOID, Version: 1, State: StatePublic,
		Name:         NamespaceName{Parent: parent, Kind: ObjectType, Name: relation.Name().Name},
		Dependencies: dependencies,
		Type: &TypeMetadata{
			Kind:                 TypeKindComposite,
			Owner:                owner,
			Layout:               layout,
			Array:                arrayID,
			Relation:             relation.ID(),
			FieldIDHighWatermark: highWatermark,
			Fields:               fields,
		},
	}, limits)
	if err != nil {
		return nil, nil, err
	}
	arrayLayout := TypeLayout{Type: Type{ID: TypeID(arrayOID), Modifier: -1, Storage: TypeArray}, Children: []TypeLayout{layout}}
	arrayDependencies := []Dependency{{Object: parent, Version: parentVersion}, {Object: rowTypeID, Version: 1}}
	slices.SortFunc(arrayDependencies, func(left, right Dependency) int {
		if left.Object < right.Object {
			return -1
		}
		if left.Object > right.Object {
			return 1
		}
		return 0
	})
	array, err := NewDescriptor(ctx, DescriptorSpec{
		ID: arrayID, OID: arrayOID, Version: 1, State: StatePublic,
		Name:         NamespaceName{Parent: parent, Kind: ObjectType, Name: arrayName},
		Dependencies: arrayDependencies,
		Type:         &TypeMetadata{Kind: TypeKindArray, Owner: owner, Layout: arrayLayout, Base: rowTypeID, BaseLayout: layout},
	}, limits)
	if err != nil {
		return nil, nil, err
	}
	return rowType, array, nil
}

func NextImplicitRelationRowTypes(ctx context.Context, relation, rowType, array *Descriptor, arrayName string, limits DescriptorLimits) (*Descriptor, *Descriptor, error) {
	if relation == nil || rowType == nil || array == nil || rowType.Kind() != ObjectType || array.Kind() != ObjectType || arrayName == "" || rowType.Version() == ^uint64(0) || array.Version() == ^uint64(0) {
		return nil, nil, ErrInvalidDescriptor
	}
	fields, highWatermark, err := RelationRowTypeFields(ctx, relation, rowType.ID())
	if err != nil {
		return nil, nil, err
	}
	rowSpec, err := rowType.SpecWithin(ctx, limits)
	if err != nil {
		return nil, nil, err
	}
	arraySpec, err := array.SpecWithin(ctx, limits)
	if err != nil {
		return nil, nil, err
	}
	if rowSpec.Type == nil || rowSpec.Type.Relation != relation.ID() || rowSpec.Type.Array != array.ID() || arraySpec.Type == nil || arraySpec.Type.DeclaredKind() != TypeKindArray || arraySpec.Type.Base != rowType.ID() {
		return nil, nil, ErrInvalidDescriptor
	}
	children := make([]TypeLayout, len(fields))
	for i := range fields {
		children[i] = fields[i].Layout
	}
	rowSpec.Version++
	rowSpec.Name.Parent = relation.Name().Parent
	rowSpec.Name.Name = relation.Name().Name
	rowSpec.Type.Fields = fields
	rowSpec.Type.FieldIDHighWatermark = max(rowSpec.Type.FieldIDHighWatermark, highWatermark)
	rowSpec.Type.Layout.Children = children
	rowSpec.Dependencies = replaceDescriptorDependency(rowSpec.Dependencies, relation.ID(), relation.Version())
	nextRowType, err := NewDescriptor(ctx, rowSpec, limits)
	if err != nil {
		return nil, nil, err
	}
	arraySpec.Version++
	arraySpec.Name.Parent = relation.Name().Parent
	arraySpec.Name.Name = arrayName
	arraySpec.Type.BaseLayout = rowSpec.Type.Layout
	arraySpec.Type.Layout.Children = []TypeLayout{rowSpec.Type.Layout}
	arraySpec.Dependencies = replaceDescriptorDependency(arraySpec.Dependencies, rowType.ID(), nextRowType.Version())
	nextArray, err := NewDescriptor(ctx, arraySpec, limits)
	if err != nil {
		return nil, nil, err
	}
	return nextRowType, nextArray, nil
}

func replaceDescriptorDependency(dependencies []Dependency, object ObjectID, version uint64) []Dependency {
	result := slices.Clone(dependencies)
	for i := range result {
		if result[i].Object == object {
			result[i].Version = version
			return result
		}
	}
	result = append(result, Dependency{Object: object, Version: version})
	slices.SortFunc(result, func(left, right Dependency) int {
		if left.Object < right.Object {
			return -1
		}
		if left.Object > right.Object {
			return 1
		}
		if left.Column < right.Column {
			return -1
		}
		if left.Column > right.Column {
			return 1
		}
		return 0
	})
	return result
}
