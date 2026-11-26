package bind

import (
	"context"
	"slices"
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

const (
	tableLikeComments uint32 = 1 << iota
	tableLikeCompression
	tableLikeConstraints
	tableLikeDefaults
	tableLikeGenerated
	tableLikeIdentity
	tableLikeIndexes
	tableLikeStatistics
	tableLikeStorage
)

const (
	tableLikeKnownOptions = tableLikeComments | tableLikeCompression | tableLikeConstraints | tableLikeDefaults | tableLikeGenerated | tableLikeIdentity | tableLikeIndexes | tableLikeStatistics | tableLikeStorage
	tableLikeAllOptions   = uint32(2147483647)
)

type tableLikeColumnSource struct {
	table   *model.Descriptor
	column  model.ColumnDescriptor
	target  model.ColumnID
	options uint32
}

type tableLikeConstraintSource struct {
	table       *model.Descriptor
	constraint  model.ConstraintDescriptor
	targetIndex int
	options     uint32
}

type tableLikeIndexSource struct {
	table       *model.Descriptor
	descriptor  *model.Descriptor
	constraint  *model.ConstraintDescriptor
	metadata    model.IndexMetadata
	id          model.ObjectID
	oid         model.OID
	name        string
	description string
	options     uint32
}

type tableLikeStatisticSource struct {
	table       *model.Descriptor
	statistic   model.ExtendedStatisticDescriptor
	targetIndex int
	options     uint32
}

type tableLikePlan struct {
	columns        []tableLikeColumnSource
	constraints    []tableLikeConstraintSource
	indexes        []tableLikeIndexSource
	statistics     []tableLikeStatisticSource
	sourceObjects  map[model.ObjectID]bool
	dependencies   []model.Dependency
	statisticNames map[string]bool
}

func (b *binder) appendTableLike(ctx context.Context, clause *zpgparse.TableLikeClause, snapshot CatalogSnapshot, options CreateDeclarationOptions, metadata *model.TableMetadata, columnNames map[string]model.ColumnID, plan *tableLikePlan) error {
	if clause == nil || clause.Relation == nil || clause.Relation.Catalogname != "" || clause.Relation.Relname == "" {
		return bindError("42601", "invalid CREATE TABLE LIKE relation", 0)
	}
	likeOptions := clause.Options
	unknown := likeOptions &^ tableLikeKnownOptions
	if unknown != 0 {
		if unknown != tableLikeAllOptions&^tableLikeKnownOptions {
			return bindError("42601", "invalid CREATE TABLE LIKE option", int(clause.Relation.Location)+1)
		}
		likeOptions &= tableLikeKnownOptions
	}
	if likeOptions&(tableLikeCompression|tableLikeStorage) != 0 {
		return missing("CREATE TABLE LIKE physical storage options", int(clause.Relation.Location)+1)
	}
	parts := []string{clause.Relation.Relname}
	if clause.Relation.Schemaname != "" {
		parts = []string{clause.Relation.Schemaname, clause.Relation.Relname}
	}
	source, _, err := resolveNamespaceObject(ctx, snapshot, options.Bind.SearchPath, parts, []model.ObjectKind{model.ObjectTable, model.ObjectView, model.ObjectMaterializedView}, "42P01", int(clause.Relation.Location)+1)
	if err != nil {
		return err
	}
	spec, err := source.SpecWithin(ctx, options.Descriptors)
	if err != nil {
		return err
	}
	sourceColumns := []model.ColumnDescriptor(nil)
	if spec.Table != nil {
		sourceColumns = spec.Table.Columns
	} else if spec.View != nil {
		sourceColumns = spec.View.Columns
	}
	if sourceColumns == nil {
		return bindError("42809", "CREATE TABLE LIKE source is not a relation", int(clause.Relation.Location)+1)
	}
	if plan.sourceObjects == nil {
		plan.sourceObjects = make(map[model.ObjectID]bool)
	}
	if plan.statisticNames == nil {
		plan.statisticNames = make(map[string]bool)
	}
	plan.sourceObjects[source.ID()] = true
	b.dependency(source, 0)
	mapping := make(map[model.ColumnID]model.ColumnID)
	for _, original := range sourceColumns {
		if original.State != model.StatePublic || original.SystemIdentity {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(metadata.Columns) >= options.Descriptors.MaxItems {
			return model.ErrDescriptorLimit
		}
		if _, exists := columnNames[original.Name]; exists {
			return bindError("42701", "column specified more than once", int(clause.Relation.Location)+1)
		}
		metadata.ColumnIDHighWatermark++
		copied := original
		copied.ID = metadata.ColumnIDHighWatermark
		copied.State = model.StatePublic
		copied.InheritedFrom = nil
		copied.Local = true
		copied.SystemIdentity = false
		copied.ConstantDefault = nil
		copied.Default = nil
		copied.DefaultOID = 0
		copied.Generated = nil
		copied.GeneratedStored = false
		copied.IdentitySequence = 0
		copied.IdentityAlways = false
		if likeOptions&tableLikeComments == 0 {
			copied.Description = ""
		}
		metadata.Columns = append(metadata.Columns, copied)
		columnNames[copied.Name] = copied.ID
		mapping[original.ID] = copied.ID
		plan.columns = append(plan.columns, tableLikeColumnSource{table: source, column: original, target: copied.ID, options: likeOptions})
		b.dependency(source, original.ID)
	}
	if len(plan.columns) > options.Descriptors.MaxItems {
		return model.ErrDescriptorLimit
	}
	if err := collectTableLikeTypeDependencies(ctx, snapshot, spec, sourceColumns, plan); err != nil {
		return err
	}
	if spec.Table == nil {
		return nil
	}
	return plan.collectMetadata(ctx, source, spec.Table, mapping, likeOptions, snapshot, options)
}

func collectTableLikeTypeDependencies(ctx context.Context, snapshot CatalogSnapshot, source model.DescriptorSpec, columns []model.ColumnDescriptor, plan *tableLikePlan) error {
	for _, dependency := range source.Dependencies {
		if err := ctx.Err(); err != nil {
			return err
		}
		published, err := snapshot.Descriptor(ctx, dependency.Object)
		if err != nil {
			return err
		}
		if published.Descriptor.Kind() != model.ObjectType {
			continue
		}
		oid := model.TypeID(published.Descriptor.OID())
		for _, column := range columns {
			if tableLikeLayoutContainsType(column.Layout, oid) {
				plan.dependencies = appendIndexDependency(plan.dependencies, dependency)
				break
			}
		}
	}
	return nil
}

func tableLikeLayoutContainsType(layout model.TypeLayout, id model.TypeID) bool {
	if layout.Type.ID == id {
		return true
	}
	for _, child := range layout.Children {
		if tableLikeLayoutContainsType(child, id) {
			return true
		}
	}
	return false
}

func (plan *tableLikePlan) collectMetadata(ctx context.Context, source *model.Descriptor, metadata *model.TableMetadata, mapping map[model.ColumnID]model.ColumnID, options uint32, snapshot CatalogSnapshot, declaration CreateDeclarationOptions) error {
	for _, original := range metadata.Constraints {
		if err := ctx.Err(); err != nil {
			return err
		}
		copyConstraint := original.Kind == model.ConstraintNotNull || options&tableLikeConstraints != 0 && original.Kind == model.ConstraintCheck
		if !copyConstraint {
			continue
		}
		mapped := original
		mapped.Columns = remapTableLikeColumns(original.Columns, mapping)
		if len(mapped.Columns) != len(original.Columns) {
			return model.ErrInvalidDescriptor
		}
		plan.constraints = append(plan.constraints, tableLikeConstraintSource{table: source, constraint: mapped, targetIndex: -1, options: options})
	}
	if options&tableLikeIndexes != 0 {
		for _, id := range metadata.Indexes {
			published, err := snapshot.Descriptor(ctx, id)
			if err != nil {
				return err
			}
			descriptor := published.Descriptor
			spec, err := descriptor.SpecWithin(ctx, declaration.Descriptors)
			if err != nil {
				return err
			}
			if spec.Index == nil || spec.Index.Table != source.ID() {
				return model.ErrInvalidDescriptor
			}
			index := *spec.Index
			index.Keys = slices.Clone(index.Keys)
			for i := range index.Keys {
				if index.Keys[i].Column != 0 {
					index.Keys[i].Column = mapping[index.Keys[i].Column]
					if index.Keys[i].Column == 0 {
						return model.ErrInvalidDescriptor
					}
				}
			}
			index.Include = remapTableLikeColumns(index.Include, mapping)
			if len(index.Include) != len(spec.Index.Include) {
				return model.ErrInvalidDescriptor
			}
			var sourceConstraint *model.ConstraintDescriptor
			for i := range metadata.Constraints {
				candidate := &metadata.Constraints[i]
				if candidate.Index == descriptor.ID() && (candidate.Kind == model.ConstraintPrimaryKey || candidate.Kind == model.ConstraintUnique || candidate.Kind == model.ConstraintExclude) {
					copy := *candidate
					sourceConstraint = &copy
					break
				}
			}
			plan.indexes = append(plan.indexes, tableLikeIndexSource{table: source, descriptor: descriptor, constraint: sourceConstraint, metadata: index, description: spec.Description, options: options})
		}
	}
	if options&tableLikeStatistics != 0 {
		for _, original := range metadata.ExtendedStatistics {
			mapped := original
			mapped.Keys = remapTableLikeColumns(original.Keys, mapping)
			if len(mapped.Keys) != len(original.Keys) {
				return model.ErrInvalidDescriptor
			}
			mapped.Expressions = slices.Clone(original.Expressions)
			plan.statistics = append(plan.statistics, tableLikeStatisticSource{table: source, statistic: mapped, targetIndex: -1, options: options})
		}
	}
	return nil
}

func remapTableLikeColumns(columns []model.ColumnID, mapping map[model.ColumnID]model.ColumnID) []model.ColumnID {
	result := make([]model.ColumnID, 0, len(columns))
	for _, source := range columns {
		if target := mapping[source]; target != 0 {
			result = append(result, target)
		}
	}
	return result
}

func (state *declarationState) installTableLikeMetadata(ctx context.Context, spec *model.DescriptorSpec, plan *tableLikePlan, snapshot CatalogSnapshot) error {
	if plan == nil {
		return nil
	}
	metadata := spec.Table
	for i := range plan.constraints {
		source := &plan.constraints[i]
		constraint := source.constraint
		constraint.ID, constraint.OID = 0, 0
		constraint.Index = 0
		constraint.ParentTable, constraint.ParentConstraint, constraint.InheritanceCount = 0, 0, 0
		constraint.Local = true
		constraint.Expression = nil
		if source.options&tableLikeComments == 0 {
			constraint.Description = ""
		}
		if source.constraint.Kind == model.ConstraintNotNull {
			constraint.Name = ""
			addition := declarationKeyName(metadata.Columns, constraint.Columns)
			name, err := state.constraintName("", addition, "not_null", false)
			if err != nil {
				return err
			}
			constraint.Name = name
		} else {
			name, err := state.constraintName("", "", "check", false)
			if err != nil {
				return err
			}
			constraint.Name = name
		}
		id, oid, err := state.allocate()
		if err != nil {
			return err
		}
		constraint.ID, constraint.OID = id, oid
		source.targetIndex = len(metadata.Constraints)
		metadata.Constraints = append(metadata.Constraints, constraint)
	}
	for i := range plan.indexes {
		source := &plan.indexes[i]
		index := &source.metadata
		index.Table = spec.ID
		index.ParentIndex = 0
		index.PendingConstraint = nil
		var copiedConstraint *model.ConstraintDescriptor
		if source.constraint != nil {
			constraint := *source.constraint
			constraint.ID, constraint.OID, constraint.Index = 0, 0, 0
			constraint.Columns = indexKeyColumns(index.Keys)
			constraint.ParentTable, constraint.ParentConstraint, constraint.InheritanceCount = 0, 0, 0
			constraint.Local = true
			if source.options&tableLikeComments == 0 {
				constraint.Description = ""
			}
			label := "key"
			if constraint.Kind == model.ConstraintPrimaryKey {
				label = "pkey"
			} else if constraint.Kind == model.ConstraintExclude {
				label = "excl"
			}
			name, err := state.constraintName("", declarationKeyName(metadata.Columns, constraint.Columns), label, true)
			if err != nil {
				return err
			}
			constraint.Name = name
			constraint.ID, constraint.OID, err = state.allocate()
			if err != nil {
				return err
			}
			copiedConstraint = &constraint
		}
		name := ""
		if copiedConstraint != nil {
			name = copiedConstraint.Name
		} else {
			addition := declarationKeyName(metadata.Columns, indexKeyColumns(index.Keys))
			if addition == "" {
				addition = strconv.Itoa(i + 1)
			}
			var err error
			name, err = state.constraintName("", addition, "idx", true)
			if err != nil {
				return err
			}
		}
		id, oid, err := state.allocate()
		if err != nil {
			return err
		}
		source.id, source.oid, source.name = id, oid, name
		metadata.Indexes = append(metadata.Indexes, id)
		if copiedConstraint != nil {
			copiedConstraint.Index = id
			metadata.Constraints = append(metadata.Constraints, *copiedConstraint)
			if copiedConstraint.Kind == model.ConstraintPrimaryKey && !copiedConstraint.Deferrable {
				if len(metadata.PrimaryKey) != 0 {
					return bindError("42P16", "multiple primary keys", 0)
				}
				metadata.PrimaryKey = slices.Clone(index.Keys)
			}
		}
	}
	for i := range plan.statistics {
		source := &plan.statistics[i]
		statistic := source.statistic
		_, oid, err := state.allocate()
		if err != nil {
			return err
		}
		statistic.OID = oid
		statistic.Owner = state.options.Owner
		statistic.Namespace = state.parent
		if source.options&tableLikeComments == 0 {
			statistic.Description = ""
		}
		base := automaticStatisticsName(state.tableName, statistic.Keys, statistic.Expressions, metadata.Columns)
		name, err := plan.tableLikeStatisticName(ctx, snapshot, state.parent, base, state.options)
		if err != nil {
			return err
		}
		statistic.Name = name
		source.targetIndex = len(metadata.ExtendedStatistics)
		metadata.ExtendedStatistics = append(metadata.ExtendedStatistics, statistic)
	}
	return nil
}


func indexKeyColumns(keys []model.KeyElement) []model.ColumnID {
	columns := make([]model.ColumnID, 0, len(keys))
	for _, key := range keys {
		if key.Column != 0 {
			columns = append(columns, key.Column)
		}
	}
	return columns
}


func (plan *tableLikePlan) tableLikeStatisticName(ctx context.Context, snapshot CatalogSnapshot, namespace model.ObjectID, base string, options CreateDeclarationOptions) (string, error) {
	for suffix := 0; ; suffix++ {
		name := base
		if suffix != 0 {
			name = clipIndexIdentifier(base, 58) + "_" + strconv.Itoa(suffix)
		}
		if plan.statisticNames[name] {
			continue
		}
		found, err := findStatisticsInSchema(ctx, snapshot, namespace, name, options.Descriptors, options.Bind.Limits.MaxNodes)
		if err != nil {
			return "", err
		}
		if found.table == nil {
			plan.statisticNames[name] = true
			return name, nil
		}
	}
}

func (state *declarationState) tableLikeIdentitySequences(ctx context.Context, spec model.DescriptorSpec, plan *tableLikePlan, snapshot CatalogSnapshot) ([]*BoundSequenceDefinition, error) {
	if plan == nil {
		return nil, nil
	}
	var result []*BoundSequenceDefinition
	for _, source := range plan.columns {
		if source.options&tableLikeIdentity == 0 || source.column.IdentitySequence == 0 {
			continue
		}
		published, err := snapshot.Descriptor(ctx, source.column.IdentitySequence)
		if err != nil {
			return nil, err
		}
		sequenceSpec, err := published.Descriptor.SpecWithin(ctx, state.options.Descriptors)
		if err != nil {
			return nil, err
		}
		if sequenceSpec.Sequence == nil {
			return nil, model.ErrInvalidDescriptor
		}
		column, found := declarationColumn(spec.Table.Columns, source.target)
		if !found {
			return nil, model.ErrInvalidDescriptor
		}
		name, err := state.constraintName("", column.Name, "seq", true)
		if err != nil {
			return nil, err
		}
		id, oid, err := state.allocate()
		if err != nil {
			return nil, err
		}
		configuration := *sequenceSpec.Sequence
		configuration.Owner = state.options.Owner
		configuration.Identity = 0
		configuration.Incarnation = 0
		configuration.OwnerTable = spec.ID
		configuration.OwnerColumn = column.ID
		configuration.Persistence = spec.Table.Persistence
		definition := &BoundSequenceDefinition{
			limits: state.options.Descriptors,
			draft: model.DescriptorSpec{
				ID: id, OID: oid,
				Name: model.NamespaceName{Parent: state.parent, Kind: model.ObjectSequence, Name: name},
				Version: 1, State: model.StatePublic, Physical: state.options.Physical,
				Dependencies: []model.Dependency{{Object: spec.ID, Version: spec.Version, Column: column.ID}},
				Sequence: &configuration,
			},
		}
		column.Nullable = false
		column.IdentitySequence = id
		column.IdentityAlways = source.column.IdentityAlways
		result = append(result, definition)
	}
	return result, nil
}

func rebindTableLikeExpressions(ctx context.Context, target *model.Descriptor, metadata *model.TableMetadata, plan *tableLikePlan, snapshot CatalogSnapshot, options CreateDeclarationOptions, remaining *uint64, dependencies *[]model.Dependency) error {
	if plan == nil {
		return nil
	}
	for _, source := range plan.columns {
		column, found := declarationColumn(metadata.Columns, source.target)
		if !found {
			return model.ErrInvalidDescriptor
		}
		if source.options&tableLikeDefaults != 0 {
			if source.column.ConstantDefault != nil {
				value := source.column.ConstantDefault.Clone()
				column.ConstantDefault = &value
			}
			if source.column.Default != nil {
				definition, err := redefineColumnDefault(ctx, source.column.Default, source.column.Layout, column.Layout, snapshot, options.Bind)
				if err != nil {
					return err
				}
				column.Default = definition
			}
		}
		if source.options&tableLikeGenerated != 0 && source.column.Generated != nil {
			definition, err := transplantRowExpression(ctx, source.column.Generated, source.table, target, source.column.ID, column.ID, 0, 0, source.column.Layout, metadata.Columns, snapshot, options.Bind, rowStoredGenerated)
			if err != nil {
				return err
			}
			column.Generated = definition
			column.GeneratedStored = source.column.GeneratedStored
		}
	}
	for _, source := range plan.constraints {
		if source.constraint.Kind != model.ConstraintCheck || source.constraint.Expression == nil || source.targetIndex < 0 || source.targetIndex >= len(metadata.Constraints) {
			continue
		}
		constraint := &metadata.Constraints[source.targetIndex]
		definition, err := transplantRowExpression(ctx, source.constraint.Expression, source.table, target, 0, 0, source.constraint.ID, constraint.ID, model.TypeLayout{}, metadata.Columns, snapshot, options.Bind, rowCheck)
		if err != nil {
			return err
		}
		if err := admitIndexDefinition(definition, remaining); err != nil {
			return err
		}
		constraint.Expression = definition
		for _, dependency := range definition.Dependencies {
			*dependencies = appendIndexDependency(*dependencies, dependency)
		}
	}
	for _, source := range plan.statistics {
		if source.targetIndex < 0 || source.targetIndex >= len(metadata.ExtendedStatistics) {
			return model.ErrInvalidDescriptor
		}
		targetStatistic := &metadata.ExtendedStatistics[source.targetIndex]
		targetStatistic.Expressions = make([]*model.ExpressionDefinition, len(source.statistic.Expressions))
		for i, expression := range source.statistic.Expressions {
			format, version := rowStatistics.encoding()
			encoding, _, err := decodeRowExpression(ctx, expression.Encoding, options.Bind.Limits, format, version)
			if err != nil {
				return err
			}
			if encoding.Target == nil {
				return model.ErrInvalidDescriptor
			}
			definition, err := transplantRowExpression(ctx, expression, source.table, target, 0, 0, 0, 0, *encoding.Target, metadata.Columns, snapshot, options.Bind, rowStatistics)
			if err != nil {
				return err
			}
			if err := admitIndexDefinition(definition, remaining); err != nil {
				return err
			}
			targetStatistic.Expressions[i] = definition
			for _, dependency := range definition.Dependencies {
				*dependencies = appendIndexDependency(*dependencies, dependency)
			}
		}
	}
	return nil
}

func cloneTableLikeIndexes(ctx context.Context, target *model.Descriptor, metadata *model.TableMetadata, plan *tableLikePlan, snapshot CatalogSnapshot, options CreateDeclarationOptions, remaining *uint64, dependencies *[]model.Dependency) ([]*model.Descriptor, error) {
	if plan == nil {
		return nil, nil
	}
	result := make([]*model.Descriptor, 0, len(plan.indexes))
	for _, source := range plan.indexes {
		index := source.metadata
		index.Keys = slices.Clone(index.Keys)
		for i := range index.Keys {
			if index.Keys[i].Expression == nil {
				continue
			}
			definition, err := transplantRowExpression(ctx, index.Keys[i].Expression, source.table, target, 0, 0, 0, 0, model.TypeLayout{}, metadata.Columns, snapshot, options.Bind, rowIndexKey)
			if err != nil {
				return nil, err
			}
			if err := admitIndexDefinition(definition, remaining); err != nil {
				return nil, err
			}
			index.Keys[i].Expression = definition
			for _, dependency := range definition.Dependencies {
				*dependencies = appendIndexDependency(*dependencies, dependency)
			}
		}
		if index.Predicate != nil {
			definition, err := transplantRowExpression(ctx, index.Predicate, source.table, target, 0, 0, 0, 0, model.TypeLayout{}, metadata.Columns, snapshot, options.Bind, rowIndexPredicate)
			if err != nil {
				return nil, err
			}
			if err := admitIndexDefinition(definition, remaining); err != nil {
				return nil, err
			}
			index.Predicate = definition
			for _, dependency := range definition.Dependencies {
				*dependencies = appendIndexDependency(*dependencies, dependency)
			}
		}
		indexDependencies := []model.Dependency{{Object: target.ID(), Version: target.Version()}}
		for _, key := range index.Keys {
			if key.Column != 0 {
				indexDependencies = appendIndexDependency(indexDependencies, model.Dependency{Object: target.ID(), Version: target.Version(), Column: key.Column})
			}
			if key.Expression != nil {
				for _, dependency := range key.Expression.Dependencies {
					indexDependencies = appendIndexDependency(indexDependencies, dependency)
				}
			}
		}
		for _, column := range index.Include {
			indexDependencies = appendIndexDependency(indexDependencies, model.Dependency{Object: target.ID(), Version: target.Version(), Column: column})
		}
		if index.Predicate != nil {
			for _, dependency := range index.Predicate.Dependencies {
				indexDependencies = appendIndexDependency(indexDependencies, dependency)
			}
		}
		description := source.description
		if source.options&tableLikeComments == 0 {
			description = ""
		}
		descriptor, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: source.id, OID: source.oid, Name: model.NamespaceName{Parent: target.Name().Parent, Kind: model.ObjectIndex, Name: source.name}, Description: description, Version: 1, State: model.StatePublic, Physical: options.Physical, Dependencies: indexDependencies, Index: &index}, options.Descriptors)
		if err != nil {
			return nil, err
		}
		result = append(result, descriptor)
	}
	return result, nil
}

func tableLikePersistentDependencies(dependencies []model.Dependency, plan *tableLikePlan) []model.Dependency {
	if plan == nil {
		return slices.Clone(dependencies)
	}
	result := slices.DeleteFunc(slices.Clone(dependencies), func(dependency model.Dependency) bool {
		return plan.sourceObjects[dependency.Object]
	})
	for _, dependency := range plan.dependencies {
		result = appendIndexDependency(result, dependency)
	}
	return result
}
