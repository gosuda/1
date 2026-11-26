package bind

import (
	"context"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type inheritedColumnSource struct {
	parent *model.Descriptor
	column model.ColumnDescriptor
	child  model.ColumnID
}

type inheritedConstraintSource struct {
	parent     *model.Descriptor
	constraint model.ConstraintDescriptor
	columns    []model.ColumnID
	targetIndex int
	reused      bool
	sources    uint32
}

type tableInheritancePlan struct {
	parents          []*model.Descriptor
	partitionParent  *model.Descriptor
	columns          []inheritedColumnSource
	constraints      []inheritedConstraintSource
}

func (b *binder) prepareTableInheritance(ctx context.Context, statement *zpgparse.CreateStmt, snapshot CatalogSnapshot, options CreateDeclarationOptions, metadata *model.TableMetadata, columnNames map[string]model.ColumnID) (*tableInheritancePlan, error) {
	if len(statement.InhRelations) == 0 {
		if statement.Partbound != nil {
			return nil, bindError("42601", "PARTITION OF requires a parent table", int(statement.Partbound.Location)+1)
		}
		return nil, nil
	}
	if len(statement.InhRelations) > options.Descriptors.MaxItems {
		return nil, model.ErrDescriptorLimit
	}
	partition := statement.Partbound != nil
	if partition && len(statement.InhRelations) != 1 {
		return nil, bindError("42601", "PARTITION OF requires exactly one parent table", int(statement.Partbound.Location)+1)
	}
	plan := &tableInheritancePlan{}
	seen := make(map[model.ObjectID]bool, len(statement.InhRelations))
	for _, node := range statement.InhRelations {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rangeVar := node.GetRangeVar()
		if rangeVar == nil || rangeVar.Catalogname != "" || rangeVar.Relname == "" {
			return nil, bindError("42601", "invalid inherited table reference", 0)
		}
		parts := []string{rangeVar.Relname}
		if rangeVar.Schemaname != "" {
			parts = []string{rangeVar.Schemaname, rangeVar.Relname}
		}
		parent, _, err := resolveNamespaceObject(ctx, snapshot, options.Bind.SearchPath, parts, []model.ObjectKind{model.ObjectTable}, "42P01", int(rangeVar.Location)+1)
		if err != nil {
			return nil, err
		}
		if seen[parent.ID()] {
			return nil, bindError("42712", "inherited table specified more than once", int(rangeVar.Location)+1)
		}
		seen[parent.ID()] = true
		parentSpec, err := parent.SpecWithin(ctx, options.Descriptors)
		if err != nil {
			return nil, err
		}
		if parentSpec.Table == nil {
			return nil, bindError("42809", "inherited object is not a table", int(rangeVar.Location)+1)
		}
		if partition {
			if parentSpec.Table.PartitionStrategy == 0 || len(parentSpec.Table.PartitionKeys) == 0 {
				return nil, bindError("42809", "partition parent is not partitioned", int(rangeVar.Location)+1)
			}
			plan.partitionParent = parent
		} else if parentSpec.Table.PartitionStrategy != 0 || parentSpec.Table.PartitionBound != nil {
			return nil, bindError("42809", "declarative partition tables cannot participate in ordinary inheritance", int(rangeVar.Location)+1)
		}
		if parentSpec.Table.Persistence != metadata.Persistence {
			return nil, bindError("42P16", "temporary and permanent tables cannot share an inheritance hierarchy", int(rangeVar.Location)+1)
		}
		b.dependency(parent, 0)
		plan.parents = append(plan.parents, parent)
		metadata.Parents = append(metadata.Parents, parent.ID())
		mapping := make(map[model.ColumnID]model.ColumnID)
		for _, inherited := range parentSpec.Table.Columns {
			if inherited.State != model.StatePublic {
				continue
			}
			if inherited.SystemIdentity {
				if !partition || len(metadata.Columns) >= options.Descriptors.MaxItems {
					if partition {
						return nil, model.ErrDescriptorLimit
					}
					continue
				}
				child := inherited
				child.ID = inherited.ID
				child.InheritedFrom = nil
				child.Local = false
				metadata.Columns = append(metadata.Columns, child)
				if child.ID > metadata.ColumnIDHighWatermark {
					metadata.ColumnIDHighWatermark = child.ID
				}
				mapping[inherited.ID] = child.ID
				b.dependency(parent, inherited.ID)
				continue
			}
			childID, exists := columnNames[inherited.Name]
			if exists {
				child, _ := declarationColumn(metadata.Columns, childID)
				if child == nil || !sameType(child.Layout, inherited.Layout) || child.GeneratedStored != inherited.GeneratedStored {
					return nil, bindError("42804", "inherited columns with the same name have incompatible definitions", int(rangeVar.Location)+1)
				}
				if child.Local && (child.Generated != nil) != (inherited.Generated != nil) {
					return nil, bindError("42804", "inherited columns disagree about generation", int(rangeVar.Location)+1)
				}
				child.Nullable = child.Nullable && inherited.Nullable
				child.InheritedFrom = append(child.InheritedFrom, model.ColumnInheritance{Table: parent.ID(), Column: inherited.ID})
			} else {
				if len(metadata.Columns) >= options.Descriptors.MaxItems {
					return nil, model.ErrDescriptorLimit
				}
				if partition {
					childID = inherited.ID
					if childID > metadata.ColumnIDHighWatermark {
						metadata.ColumnIDHighWatermark = childID
					}
				} else {
					childID = metadata.ColumnIDHighWatermark + 1
					metadata.ColumnIDHighWatermark = childID
				}
				child := inherited
				child.ID = childID
				child.State = model.StatePublic
				child.Local = false
				child.InheritedFrom = []model.ColumnInheritance{{Table: parent.ID(), Column: inherited.ID}}
				child.Default = nil
				child.ConstantDefault = nil
				child.DefaultOID = 0
				child.Generated = nil
				child.IdentitySequence = 0
				child.IdentityAlways = false
				metadata.Columns = append(metadata.Columns, child)
				columnNames[child.Name] = childID
			}
			mapping[inherited.ID] = childID
			plan.columns = append(plan.columns, inheritedColumnSource{parent: parent, column: inherited, child: childID})
			b.dependency(parent, inherited.ID)
		}
		if partition {
			for _, key := range parentSpec.Table.PrimaryKey {
				if key.Column == 0 || key.Expression != nil {
					return nil, missing("partitioned expression primary key inheritance", int(rangeVar.Location)+1)
				}
				childColumn := mapping[key.Column]
				if childColumn == 0 {
					continue
				}
				key.Column = childColumn
				metadata.PrimaryKey = append(metadata.PrimaryKey, key)
			}
		}
		for _, inherited := range parentSpec.Table.Constraints {
			if inherited.NoInherit || inherited.Kind != model.ConstraintCheck && inherited.Kind != model.ConstraintNotNull &&
				(!partition || inherited.Kind != model.ConstraintPrimaryKey && inherited.Kind != model.ConstraintUnique && inherited.Kind != model.ConstraintForeignKey) {
				continue
			}
			columns := make([]model.ColumnID, len(inherited.Columns))
			for i, parentColumn := range inherited.Columns {
				columns[i] = mapping[parentColumn]
				if columns[i] == 0 {
					return nil, model.ErrInvalidDescriptor
				}
			}
			constraint := inherited
			constraint.Columns = slices.Clone(columns)
			constraint.OnDeleteColumns = slices.Clone(inherited.OnDeleteColumns)
			for i, column := range constraint.OnDeleteColumns {
				constraint.OnDeleteColumns[i] = mapping[column]
				if constraint.OnDeleteColumns[i] == 0 {
					return nil, model.ErrInvalidDescriptor
				}
			}
			constraint.OnUpdateColumns = slices.Clone(inherited.OnUpdateColumns)
			for i, column := range constraint.OnUpdateColumns {
				constraint.OnUpdateColumns[i] = mapping[column]
				if constraint.OnUpdateColumns[i] == 0 {
					return nil, model.ErrInvalidDescriptor
				}
			}
			plan.constraints = append(plan.constraints, inheritedConstraintSource{parent: parent, constraint: constraint, columns: columns, sources: 1})
		}
	}
	return plan, nil
}

func sameInheritedConstraint(left, right model.ConstraintDescriptor) bool {
	return left.Kind == right.Kind &&
		slices.Equal(left.Columns, right.Columns) &&
		left.ReferencedObject == right.ReferencedObject &&
		slices.Equal(left.ReferencedColumns, right.ReferencedColumns) &&
		left.ReferencedConstraint == right.ReferencedConstraint &&
		left.ReferencedIndex == right.ReferencedIndex &&
		slices.Equal(left.ExcludeOperators, right.ExcludeOperators) &&
		slices.Equal(left.ParentForeignEqualityOperators, right.ParentForeignEqualityOperators) &&
		slices.Equal(left.ParentEqualityOperators, right.ParentEqualityOperators) &&
		slices.Equal(left.ForeignEqualityOperators, right.ForeignEqualityOperators) &&
		left.Deferrable == right.Deferrable &&
		left.InitiallyDeferred == right.InitiallyDeferred &&
		left.Validated == right.Validated &&
		left.Enforced == right.Enforced &&
		left.Mutation == right.Mutation &&
		!left.NoInherit &&
		left.OnDelete == right.OnDelete &&
		slices.Equal(left.OnDeleteColumns, right.OnDeleteColumns) &&
		left.OnUpdate == right.OnUpdate &&
		slices.Equal(left.OnUpdateColumns, right.OnUpdateColumns) &&
		left.Match == right.Match
}

func (state *declarationState) installInheritedConstraints(metadata *model.TableMetadata, plan *tableInheritancePlan) error {
	if plan == nil {
		return nil
	}
	for i := range plan.constraints {
		source := &plan.constraints[i]
		existingIndex := slices.IndexFunc(metadata.Constraints, func(existing model.ConstraintDescriptor) bool {
			return existing.Name == source.constraint.Name
		})
		if existingIndex >= 0 {
			existing := &metadata.Constraints[existingIndex]
			if !sameInheritedConstraint(*existing, source.constraint) ||
				source.sources == 0 || existing.InheritanceCount > ^uint32(0)-source.sources {
				return bindError("42710", "inherited constraint name conflicts with another constraint", 0)
			}
			existing.InheritanceCount += source.sources
			if existing.ParentTable == 0 {
				existing.ParentTable = source.parent.ID()
				existing.ParentConstraint = source.constraint.ID
			}
			if source.constraint.Kind == model.ConstraintPrimaryKey || source.constraint.Kind == model.ConstraintUnique {
				existing.Index = source.constraint.Index
			}
			source.reused = true
			source.targetIndex = existingIndex
			continue
		}
		constraint := source.constraint
		var err error
		constraint.ID, constraint.OID, err = state.allocate()
		if err != nil {
			return err
		}
		constraint.Columns = slices.Clone(source.columns)
		constraint.ParentTable = source.parent.ID()
		constraint.ParentConstraint = source.constraint.ID
		constraint.InheritanceCount = source.sources
		constraint.Local = false
		constraint.NoInherit = false
		if constraint.Kind != model.ConstraintPrimaryKey && constraint.Kind != model.ConstraintUnique {
			constraint.Index = 0
		}
		if constraint.Kind == model.ConstraintCheck {
			constraint.Expression = nil
		}
		source.targetIndex = len(metadata.Constraints)
		metadata.Constraints = append(metadata.Constraints, constraint)
		state.names[constraint.Name] = true
		if constraint.Kind == model.ConstraintNotNull {
			for _, columnID := range constraint.Columns {
				column, found := declarationColumn(metadata.Columns, columnID)
				if !found {
					return model.ErrInvalidDescriptor
				}
				column.Nullable = false
			}
		}
	}
	return nil
}

func (state *declarationState) installInheritedDefaultOIDs(metadata *model.TableMetadata, plan *tableInheritancePlan, local map[model.ColumnID]bool) error {
	if plan == nil {
		return nil
	}
	allocated := make(map[model.ColumnID]bool)
	for _, source := range plan.columns {
		if local[source.child] {
			continue
		}
		column, found := declarationColumn(metadata.Columns, source.child)
		if !found {
			return model.ErrInvalidDescriptor
		}
		inheritDefault := column.Default == nil && source.column.Default != nil
		inheritGenerated := column.Generated == nil && source.column.Generated != nil
		if !inheritDefault && !inheritGenerated || allocated[source.child] {
			continue
		}
		_, oid, err := state.allocate()
		if err != nil {
			return err
		}
		column.DefaultOID = oid
		allocated[source.child] = true
	}
	return nil
}

func rebindInheritedExpressions(ctx context.Context, target *model.Descriptor, metadata *model.TableMetadata, plan *tableInheritancePlan, snapshot CatalogSnapshot, options CreateDeclarationOptions) error {
	if plan == nil {
		return nil
	}
	seenColumns := make(map[model.ColumnID]bool)
	explicitDefaults := make(map[model.ColumnID]bool)
	explicitGenerated := make(map[model.ColumnID]bool)
	inheritedDefaults := make(map[model.ColumnID]bool)
	inheritedGenerated := make(map[model.ColumnID]bool)
	for _, source := range plan.columns {
		targetColumn, found := declarationColumn(metadata.Columns, source.child)
		if !found {
			return model.ErrInvalidDescriptor
		}
		if !seenColumns[source.child] {
			seenColumns[source.child] = true
			explicitDefaults[source.child] = targetColumn.Default != nil
			explicitGenerated[source.child] = targetColumn.Generated != nil
		}
		if !explicitDefaults[source.child] {
			var definition *model.ExpressionDefinition
			if source.column.Default != nil {
				var err error
				definition, err = redefineColumnDefault(ctx, source.column.Default, source.column.Layout, targetColumn.Layout, snapshot, options.Bind)
				if err != nil {
					return err
				}
			}
			if inheritedDefaults[source.child] && !sameStructuralExpression(targetColumn.Default, definition) {
				return bindError("42804", "inherited columns disagree about default values", 0)
			}
			if !inheritedDefaults[source.child] {
				targetColumn.Default = definition
				if source.column.ConstantDefault != nil {
					value := source.column.ConstantDefault.Clone()
					targetColumn.ConstantDefault = &value
				}
				inheritedDefaults[source.child] = true
			}
		}
		if !explicitGenerated[source.child] {
			var definition *model.ExpressionDefinition
			if source.column.Generated != nil {
				var err error
				definition, err = transplantRowExpression(ctx, source.column.Generated, source.parent, target, source.column.ID, targetColumn.ID, 0, 0, source.column.Layout, metadata.Columns, snapshot, options.Bind, rowStoredGenerated)
				if err != nil {
					return err
				}
			}
			if inheritedGenerated[source.child] && !sameStructuralExpression(targetColumn.Generated, definition) {
				return bindError("42804", "inherited columns disagree about generation", 0)
			}
			if !inheritedGenerated[source.child] {
				targetColumn.Generated = definition
				targetColumn.GeneratedStored = source.column.GeneratedStored
				inheritedGenerated[source.child] = true
			}
		}
	}
	for _, source := range plan.constraints {
		if source.constraint.Kind != model.ConstraintCheck || source.constraint.Expression == nil || source.targetIndex < 0 || source.targetIndex >= len(metadata.Constraints) {
			continue
		}
		targetConstraint := &metadata.Constraints[source.targetIndex]
		definition, err := transplantRowExpression(ctx, source.constraint.Expression, source.parent, target, 0, 0, source.constraint.ID, targetConstraint.ID, model.TypeLayout{}, metadata.Columns, snapshot, options.Bind, rowCheck)
		if err != nil {
			return err
		}
		if source.reused && !sameStructuralExpression(targetConstraint.Expression, definition) {
			return bindError("42804", "inherited check constraint differs from parent", 0)
		}
		targetConstraint.Expression = definition
	}
	return nil
}

func transplantRowExpression(ctx context.Context, definition *model.ExpressionDefinition, source, target *model.Descriptor, sourceColumn, targetColumn model.ColumnID, sourceConstraint, targetConstraint model.ObjectID, result model.TypeLayout, targetColumns []model.ColumnDescriptor, snapshot CatalogSnapshot, options Options, purpose rowExpressionPurpose) (*model.ExpressionDefinition, error) {
	format, version := purpose.encoding()
	if definition == nil || definition.SemanticVersion != version || definition.SQL != "" || source == nil || target == nil {
		return nil, missing("inherited row expression semantic encoding", 0)
	}
	sourceSpec, err := source.SpecWithin(ctx, descriptorLimits(options.Limits))
	if err != nil {
		return nil, err
	}
	if sourceSpec.Table == nil {
		return nil, model.ErrInvalidDescriptor
	}
	encoding, ast, err := decodeRowExpression(ctx, definition.Encoding, options.Limits, format, version)
	if err != nil {
		return nil, err
	}
	if encoding.Table != source.ID() || encoding.TargetColumn != sourceColumn || encoding.ConstraintID != sourceConstraint {
		return nil, bindError("42804", "inherited row expression identity changed", 0)
	}
	if err := rewriteIndexAST(ctx, ast.ProtoReflect(), func(node *zpgparse.Node) error {
		parameter := node.GetParamRef()
		if parameter == nil {
			return nil
		}
		index := int(parameter.Number) - 1
		if index < 0 || index >= len(encoding.Columns) {
			return bindError("42P02", "inherited expression parameter metadata changed", 0)
		}
		sourceColumn := tableColumn(sourceSpec.Table.Columns, encoding.Columns[index].Column)
		if sourceColumn.ID == 0 {
			return bindError("42703", "inherited expression column does not exist", 0)
		}
		node.Node = columnReference(sourceColumn.Name).Node
		return nil
	}); err != nil {
		return nil, err
	}
	options.SearchPath = slices.Clone(encoding.SearchPath)
	options.Collation = encoding.Collation
	options.DefaultCollation = encoding.DefaultCollation
	options.Builtin = encoding.Builtin
	var targetLayout *model.TypeLayout
	if purpose.hasTarget() {
		targetLayout = &result
	}
	rebuilt, _, err := defineRowExpression(ctx, ast, target, targetColumns, snapshot, options, purpose, targetColumn, targetLayout, targetConstraint)
	return rebuilt, err
}

func inheritanceParentUpdates(ctx context.Context, plan *tableInheritancePlan, child *model.Descriptor, limits model.DescriptorLimits) ([]ObjectMutation, error) {
	if plan == nil {
		return nil, nil
	}
	updates := make([]ObjectMutation, 0, len(plan.parents))
	childMetadata, ok := child.Table()
	if !ok {
		return nil, model.ErrInvalidDescriptor
	}
	for _, parent := range plan.parents {
		spec, err := parent.SpecWithin(ctx, limits)
		if err != nil {
			return nil, err
		}
		if spec.Table == nil || spec.Version == ^uint64(0) || spec.Table.InheritanceChildren == ^uint32(0) {
			return nil, model.ErrInvalidDescriptor
		}
		metadata := *spec.Table
		metadata.InheritanceChildren++
		if childMetadata.PartitionBound != nil && childMetadata.PartitionBound.Default {
			if metadata.PartitionDefault != 0 && metadata.PartitionDefault != child.ID() {
				return nil, bindError("42P17", "default partition already exists", 0)
			}
			metadata.PartitionDefault = child.ID()
		}
		spec.Table = &metadata
		spec.Version++
		next, err := model.NewDescriptor(ctx, spec, limits)
		if err != nil {
			return nil, err
		}
		if err := parent.ValidateSuccessor(ctx, next); err != nil {
			return nil, err
		}
		updates = append(updates, ObjectMutation{Base: parent, Next: next})
	}
	return updates, nil
}

func futureInheritanceDependencies(dependencies []model.Dependency, parents []*model.Descriptor) []model.Dependency {
	result := slices.Clone(dependencies)
	for _, parent := range parents {
		replaced := false
		for i := range result {
			if result[i].Object == parent.ID() {
				result[i].Version = parent.Version() + 1
				replaced = true
			}
		}
		if !replaced {
			result = append(result, model.Dependency{Object: parent.ID(), Version: parent.Version() + 1})
		}
	}
	return result
}

func currentInheritanceDependency(dependency model.Dependency, plan *tableInheritancePlan) model.Dependency {
	if plan == nil {
		return dependency
	}
	for _, parent := range plan.parents {
		if dependency.Object == parent.ID() {
			dependency.Version = parent.Version()
			return dependency
		}
	}
	return dependency
}

func removeInheritanceDependency(dependencies []model.Dependency, parent model.ObjectID) []model.Dependency {
	return slices.DeleteFunc(slices.Clone(dependencies), func(dependency model.Dependency) bool {
		return dependency.Object == parent
	})
}
