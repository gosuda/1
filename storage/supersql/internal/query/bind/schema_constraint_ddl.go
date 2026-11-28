package bind

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func bindOnlineConstraint(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, command *zpgparse.AlterTableCmd) (*BoundSchemaDDL, error) {
	if command == nil || command.Recurse {
		return nil, &model.Error{Code: "0A000", Message: "constraint recursion or CASCADE requires inherited constraint planning", FeatureID: string(WriteDescriptorExpansion)}
	}
	if command.Behavior == zpgparse.DropBehavior_DROP_CASCADE && command.Subtype != zpgparse.AlterTableType_AT_DropConstraint {
		return nil, &model.Error{Code: "0A000", Message: "constraint recursion or CASCADE requires inherited constraint planning", FeatureID: string(WriteDescriptorExpansion)}
	}
	spec, err := table.SpecWithin(ctx, options.Descriptors)
	if err != nil || spec.Table == nil {
		return nil, err
	}
	if command.Behavior == zpgparse.DropBehavior_DROP_CASCADE && (spec.Table.InheritanceChildren != 0 || spec.Table.PartitionStrategy != 0 || len(spec.Table.Parents) != 0) {
		return nil, &model.Error{Code: "0A000", Message: "constraint recursion or CASCADE requires inherited constraint planning", FeatureID: string(WriteDescriptorExpansion)}
	}
	switch command.Subtype {
	case zpgparse.AlterTableType_AT_AddConstraint:
		return bindOnlineAddConstraint(ctx, b, snapshot, options, table, spec, command)
	case zpgparse.AlterTableType_AT_DropConstraint:
		return bindOnlineDropConstraint(ctx, b, snapshot, options, table, spec, command)
	case zpgparse.AlterTableType_AT_AlterConstraint:
		return bindOnlineAlterConstraint(ctx, b, snapshot, options, table, spec, command)
	case zpgparse.AlterTableType_AT_ValidateConstraint:
		return bindOnlineValidateConstraint(ctx, b, options, table, spec, command)
	default:
		return nil, bindError("42601", "invalid ALTER TABLE constraint command", 0)
	}
}

func constraintDeclarationState(b *binder, options CreateDeclarationOptions, table *model.Descriptor, metadata *model.TableMetadata) *declarationState {
	state := &declarationState{b: b, options: options, parent: table.Name().Parent, tableName: table.Name().Name, names: make(map[string]bool), reserved: make(map[string]bool), ids: make(map[model.ObjectID]bool), oids: make(map[model.OID]bool)}
	state.ids[table.ID()] = true
	state.oids[table.OID()] = true
	for _, constraint := range metadata.Constraints {
		state.names[constraint.Name] = true
		state.ids[constraint.ID] = true
		state.oids[constraint.OID] = true
	}
	return state
}

func publicConstraintColumns(columns []model.ColumnDescriptor) map[string]model.ColumnID {
	result := make(map[string]model.ColumnID, len(columns))
	for _, column := range columns {
		if column.State == model.StatePublic && !column.SystemIdentity {
			result[column.Name] = column.ID
		}
	}
	return result
}

func bindOnlineAddConstraint(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, spec model.DescriptorSpec, command *zpgparse.AlterTableCmd) (*BoundSchemaDDL, error) {
	raw := command.Def.GetConstraint()
	if raw == nil || command.Name != "" || command.MissingOk {
		return nil, bindError("42601", "invalid ADD CONSTRAINT declaration", 0)
	}
	state := constraintDeclarationState(b, options, table, spec.Table)
	columns := publicConstraintColumns(spec.Table.Columns)
	var constraint model.ConstraintDescriptor
	var err error
	var checkRawExpr *zpgparse.Node
	switch raw.Contype {
	case zpgparse.ConstrType_CONSTR_CHECK:
		if raw.RawExpr == nil {
			return nil, bindError("42601", "CHECK constraint requires an expression", int(raw.Location)+1)
		}
		if err := ordinaryConstraint(raw); err != nil {
			return nil, err
		}
		constraint, err = declarationConstraintAttributes(raw, model.ConstraintCheck)
		if err != nil {
			return nil, err
		}
		constraint.Local = true
		constraint.ID, constraint.OID, err = state.allocate()
		if err != nil {
			return nil, err
		}
		pendingSpec := spec
		pending, err := model.NewDescriptor(ctx, pendingSpec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		constraint.Expression, err = DefineCheck(ctx, raw.RawExpr, pending, constraint.ID, snapshot, options.Bind)
		if err != nil {
			return nil, err
		}
		checkCols := make([]model.ColumnID, 0, len(constraint.Expression.Dependencies))
		for _, dependency := range constraint.Expression.Dependencies {
			if dependency.Object == table.ID() && dependency.Column != 0 && !slices.Contains(checkCols, dependency.Column) {
				checkCols = append(checkCols, dependency.Column)
			}
			if dependency.Object != table.ID() {
				spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
			}
		}
		constraint.Name, err = state.constraintName(raw.Conname, declarationKeyName(spec.Table.Columns, checkCols), "check", false)
		if err != nil {
			return nil, err
		}
		checkRawExpr = raw.RawExpr
	case zpgparse.ConstrType_CONSTR_FOREIGN:
		if err := ordinaryConstraint(raw); err != nil {
			return nil, err
		}
		var referenced *model.Descriptor
		constraint, referenced, err = state.bindForeignKey(ctx, &spec, tableConstraint{raw: raw}, columns)
		if err != nil {
			return nil, err
		}
		if referenced != nil {
			constraint.ReferencedObject = referenced.ID()
		}
		constraint.Name, err = state.constraintName(raw.Conname, declarationKeyName(spec.Table.Columns, constraint.Columns), "fkey", false)
		if err != nil {
			return nil, err
		}
		constraint.ID, constraint.OID, err = state.allocate()
		if err != nil {
			return nil, err
		}
		for _, dependency := range b.dependencies {
			if dependency.Object != table.ID() {
				spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
			}
		}
	case zpgparse.ConstrType_CONSTR_PRIMARY, zpgparse.ConstrType_CONSTR_UNIQUE:
		return bindOnlineConstraintUsingIndex(ctx, b, snapshot, options, table, spec, state, raw)
	case zpgparse.ConstrType_CONSTR_EXCLUSION:
		return bindOnlineExclusionWithNewIndex(ctx, b, options, table, spec, state, raw, columns)
	default:
		return nil, missing("online ADD CONSTRAINT "+raw.Contype.String(), int(raw.Location)+1)
	}
	spec.Table.Constraints = append(spec.Table.Constraints, constraint)
	spec.Version = table.Version() + 1
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	result := &BoundSchemaDDL{source: table, mutation: constraint.ID, metadataOnly: target, dependencies: b.dependencies, warnings: b.warnings}
	if constraint.Validated {
		result.validation = constraint.ID
		result.transactionalValidations = []model.ObjectID{constraint.ID}
	}
	if checkRawExpr != nil && !constraint.NoInherit && spec.Table.InheritanceChildren != 0 {
		childUpdates, childValidations, propErr := propagateInheritedCheckAdd(ctx, b, snapshot, options, table.ID(), constraint, checkRawExpr)
		if propErr != nil {
			return nil, propErr
		}
		result.auxiliaryUpdates = append(result.auxiliaryUpdates, childUpdates...)
		result.transactionalValidations = append(result.transactionalValidations, childValidations...)
		result.dependencies = b.dependencies
	}
	return result, ctx.Err()
}

func bindOnlineExclusionWithNewIndex(ctx context.Context, b *binder, options CreateDeclarationOptions, table *model.Descriptor, spec model.DescriptorSpec, state *declarationState, raw *zpgparse.Constraint, columns map[string]model.ColumnID) (*BoundSchemaDDL, error) {
	constraint, metadata, err := state.bindExclusion(ctx, &spec, tableConstraint{raw: raw}, columns)
	if err != nil {
		return nil, err
	}
	constraint.Name, err = state.constraintName(raw.Conname, declarationKeyName(spec.Table.Columns, constraint.Columns), "excl", true)
	if err != nil {
		return nil, err
	}
	constraint.ID, constraint.OID, err = state.allocate()
	if err != nil {
		return nil, err
	}
	indexID, indexOID, err := state.allocate()
	if err != nil {
		return nil, err
	}
	constraint.Index = indexID
	pending := constraint
	metadata.PendingConstraint = &pending
	metadata.Invalid = true
	metadata.NotReady = true
	for _, dependency := range b.dependencies {
		if dependency.Object == table.ID() {
			continue
		}
		added := !slices.Contains(spec.Dependencies, dependency)
		spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
		if added {
			metadata.PendingDependencies = appendIndexDependency(metadata.PendingDependencies, dependency)
		}
	}
	dependencies := []model.Dependency{{Object: table.ID(), Version: table.Version()}}
	for _, column := range constraint.Columns {
		dependencies = append(dependencies, model.Dependency{Object: table.ID(), Version: table.Version(), Column: column})
	}
	for _, dependency := range metadata.PendingDependencies {
		dependencies = appendIndexDependency(dependencies, dependency)
	}
	index, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: indexID, OID: indexOID, Name: model.NamespaceName{Parent: table.Name().Parent, Kind: model.ObjectIndex, Name: constraint.Name}, Version: 1, State: model.StateAbsent, Physical: options.Physical, Dependencies: dependencies, Index: metadata}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	spec.Table.Constraints = append(spec.Table.Constraints, constraint)
	spec.Table.Indexes = append(spec.Table.Indexes, indexID)
	spec.Version = table.Version() + 3
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundSchemaDDL{source: table, mutation: indexID, metadataOnly: target, indexAddition: index, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
}

func bindOnlineConstraintUsingIndex(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, spec model.DescriptorSpec, state *declarationState, raw *zpgparse.Constraint) (*BoundSchemaDDL, error) {
	if raw.Indexname == "" {
		return bindOnlineConstraintWithNewIndex(ctx, b, options, table, spec, state, raw)
	}
	if len(raw.Keys) != 0 || len(raw.Including) != 0 || raw.WhereClause != nil || raw.AccessMethod != "" || raw.Indexspace != "" || len(raw.Options) != 0 || raw.SkipValidation || raw.IsNoInherit {
		return nil, bindError("42601", "invalid USING INDEX constraint attributes", int(raw.Location)+1)
	}
	published, err := snapshot.Resolve(ctx, model.NamespaceName{Parent: table.Name().Parent, Kind: model.ObjectIndex, Name: raw.Indexname})
	if err != nil {
		return nil, err
	}
	index := published.Descriptor
	indexSpec, err := index.SpecWithin(ctx, options.Descriptors)
	if err != nil || indexSpec.Index == nil {
		return nil, err
	}
	metadata := indexSpec.Index
	if index.State() != model.StatePublic || metadata.Table != table.ID() || !metadata.Valid() || !metadata.Unique || metadata.Predicate != nil || !slices.Contains(spec.Table.Indexes, index.ID()) {
		return nil, bindError("42809", "index is not a valid unique constraint index", int(raw.Location)+1)
	}
	if slices.ContainsFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Index == index.ID() }) {
		return nil, bindError("55000", "index is already owned by a constraint", int(raw.Location)+1)
	}
	columns := make([]model.ColumnID, len(metadata.Keys))
	for i, key := range metadata.Keys {
		if key.Column == 0 || key.Expression != nil {
			return nil, bindError("42809", "constraint index must use plain table columns", int(raw.Location)+1)
		}
		columns[i] = key.Column
	}
	kind, label := model.ConstraintUnique, "key"
	if raw.Contype == zpgparse.ConstrType_CONSTR_PRIMARY {
		kind, label = model.ConstraintPrimaryKey, "pkey"
		if slices.ContainsFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Kind == model.ConstraintPrimaryKey }) {
			return nil, bindError("42P16", "multiple primary keys", int(raw.Location)+1)
		}
		for _, column := range columns {
			declaration, found := declarationColumn(spec.Table.Columns, column)
			if !found {
				return nil, model.ErrInvalidDescriptor
			}
			declaration.Nullable = false
		}
	}
	constraint, err := declarationConstraintAttributes(raw, kind)
	if err != nil {
		return nil, err
	}
	constraint.Columns, constraint.Index = columns, index.ID()
	constraint.Name, err = state.constraintName(raw.Conname, declarationKeyName(spec.Table.Columns, columns), label, true)
	if err != nil {
		return nil, err
	}
	constraint.ID, constraint.OID, err = state.allocate()
	if err != nil {
		return nil, err
	}
	spec.Table.Constraints = append(spec.Table.Constraints, constraint)
	spec.Version = table.Version() + 1
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	b.dependency(index, 0)
	if constraint.Deferrable {
		indexes, sourceErr := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
		if sourceErr != nil {
			return nil, sourceErr
		}
		spec.Version = table.Version() + 3
		spec.Physical.Generation++
		target, createErr := model.NewDescriptor(ctx, spec, options.Descriptors)
		if createErr != nil {
			return nil, createErr
		}
		targetIndexes, targetErr := onlineTargetIndexes(ctx, snapshot, options, indexes, target, 0)
		if targetErr != nil {
			return nil, targetErr
		}
		indexPosition := slices.IndexFunc(targetIndexes, func(candidate *model.Descriptor) bool { return candidate.ID() == index.ID() })
		if indexPosition < 0 {
			return nil, model.ErrInvalidDescriptor
		}
		targetIndexSpec, specErr := targetIndexes[indexPosition].SpecWithin(ctx, options.Descriptors)
		if specErr != nil || targetIndexSpec.Index == nil {
			return nil, errors.Join(model.ErrInvalidDescriptor, specErr)
		}
		targetIndexSpec.Index.DeferredUnique = true
		targetIndexes[indexPosition], createErr = model.NewDescriptor(ctx, targetIndexSpec, options.Descriptors)
		if createErr != nil {
			return nil, createErr
		}
		return &BoundSchemaDDL{source: table, definition: schema.JobDefinition{Operation: schema.SchemaRewritePrimaryKey, SourceTable: table, TargetTable: target, SourceIndexes: indexes, TargetIndexes: targetIndexes}, mutation: constraint.ID, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
	}
	result := &BoundSchemaDDL{source: table, mutation: constraint.ID, metadataOnly: target, dependencies: b.dependencies, warnings: b.warnings}
	if kind == model.ConstraintPrimaryKey {
		result.validation = constraint.ID
	}
	return result, ctx.Err()
}

func bindOnlineConstraintWithNewIndex(ctx context.Context, b *binder, options CreateDeclarationOptions, table *model.Descriptor, spec model.DescriptorSpec, state *declarationState, raw *zpgparse.Constraint) (*BoundSchemaDDL, error) {
	if len(raw.Keys) == 0 || raw.WhereClause != nil || raw.AccessMethod != "" && raw.AccessMethod != "btree" || raw.Indexspace != "" || len(raw.Options) != 0 || raw.SkipValidation || raw.IsNoInherit {
		return nil, bindError("42601", "invalid UNIQUE or PRIMARY KEY constraint attributes", int(raw.Location)+1)
	}
	columns, err := declarationColumns(raw.Keys, 0, publicConstraintColumns(spec.Table.Columns))
	if err != nil {
		return nil, err
	}
	include, err := declarationColumnsOptional(raw.Including, publicConstraintColumns(spec.Table.Columns))
	if err != nil {
		return nil, err
	}
	for _, column := range include {
		if slices.Contains(columns, column) {
			return nil, bindError("42701", "included column overlaps constraint key", int(raw.Location)+1)
		}
	}
	kind, label := model.ConstraintUnique, "key"
	if raw.Contype == zpgparse.ConstrType_CONSTR_PRIMARY {
		kind, label = model.ConstraintPrimaryKey, "pkey"
		if raw.NullsNotDistinct {
			return nil, bindError("42601", "NULLS NOT DISTINCT is not valid for a primary key", int(raw.Location)+1)
		}
		if slices.ContainsFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Kind == model.ConstraintPrimaryKey }) {
			return nil, bindError("42P16", "multiple primary keys", int(raw.Location)+1)
		}
	}
	constraint, err := declarationConstraintAttributes(raw, kind)
	if err != nil {
		return nil, err
	}
	constraint.Columns = columns
	constraint.Name, err = state.constraintName(raw.Conname, declarationKeyName(spec.Table.Columns, columns), label, true)
	if err != nil {
		return nil, err
	}
	constraint.ID, constraint.OID, err = state.allocate()
	if err != nil {
		return nil, err
	}
	indexID, indexOID, err := state.allocate()
	if err != nil {
		return nil, err
	}
	keys := make([]model.KeyElement, len(columns))
	for i, columnID := range columns {
		column, found := declarationColumn(spec.Table.Columns, columnID)
		if !found {
			return nil, model.ErrInvalidDescriptor
		}
		opClass, opErr := state.b.defaultIndexOperatorClass("btree", column.Layout)
		if opErr != nil {
			return nil, opErr
		}
		keys[i] = model.KeyElement{Column: columnID, Type: column.Layout.Type, Children: slices.Clone(column.Layout.Children), Collation: column.Layout.Type.Collation, OperatorClass: opClass}
		if kind == model.ConstraintPrimaryKey {
			column.Nullable = false
		}
	}
	constraint.Index = indexID
	pending := constraint
	indexMetadata := &model.IndexMetadata{Owner: options.Owner, Table: table.ID(), Keys: keys, Include: include, Unique: true, NullsNotDistinct: raw.NullsNotDistinct, DeferredUnique: constraint.Deferrable, PendingConstraint: &pending}
	indexMetadata.Invalid = true
	indexMetadata.NotReady = true
	dependencies := []model.Dependency{{Object: table.ID(), Version: table.Version()}}
	for _, column := range columns {
		dependencies = append(dependencies, model.Dependency{Object: table.ID(), Version: table.Version(), Column: column})
	}
	for _, column := range include {
		dependencies = append(dependencies, model.Dependency{Object: table.ID(), Version: table.Version(), Column: column})
	}
	index, err := model.NewDescriptor(ctx, model.DescriptorSpec{ID: indexID, OID: indexOID, Name: model.NamespaceName{Parent: table.Name().Parent, Kind: model.ObjectIndex, Name: constraint.Name}, Version: 1, State: model.StateAbsent, Physical: options.Physical, Dependencies: dependencies, Index: indexMetadata}, options.Descriptors)
	if err != nil {
		return nil, err
	}
	spec.Table.Constraints = append(spec.Table.Constraints, constraint)
	spec.Table.Indexes = append(spec.Table.Indexes, indexID)
	spec.Version = table.Version() + 3
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundSchemaDDL{source: table, mutation: indexID, metadataOnly: target, indexAddition: index, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
}

func bindOnlineDropConstraint(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, spec model.DescriptorSpec, command *zpgparse.AlterTableCmd) (*BoundSchemaDDL, error) {
	if command.Name == "" || command.Def != nil {
		return nil, bindError("42601", "invalid DROP CONSTRAINT declaration", 0)
	}
	position := slices.IndexFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Name == command.Name })
	if position < 0 {
		if command.MissingOk {
			return &BoundSchemaDDL{source: table, noOp: true, dependencies: b.dependencies, warnings: []model.Error{{Code: "42704", Message: "constraint does not exist, skipping"}}}, nil
		}
		return nil, bindError("42704", "constraint does not exist", 0)
	}
	constraint := spec.Table.Constraints[position]
	if !constraint.Local && constraint.InheritanceCount > 0 {
		return nil, bindError("42P16", "cannot drop inherited constraint "+constraint.Name, 0)
	}
	if err := ensureConstraintUnreferenced(ctx, b, snapshot, table.ID(), constraint.ID); err != nil {
		return nil, err
	}
	spec.Table.Constraints = slices.Delete(spec.Table.Constraints, position, position+1)
	if constraint.Local && constraint.InheritanceCount > 0 {
		// Dropping a locally-defined constraint that is also inherited keeps the
		// inherited copy with Local = false (matching PostgreSQL).
		kept := constraint
		kept.Local = false
		spec.Table.Constraints = slices.Insert(spec.Table.Constraints, position, kept)
	}
	if constraint.Kind == model.ConstraintNotNull && len(constraint.Columns) == 1 {
		column, found := declarationColumn(spec.Table.Columns, constraint.Columns[0])
		if !found {
			return nil, model.ErrInvalidDescriptor
		}
		stillRequired := slices.ContainsFunc(spec.Table.Constraints, func(candidate model.ConstraintDescriptor) bool {
			return candidate.Enforced && (candidate.Kind == model.ConstraintNotNull && slices.Contains(candidate.Columns, column.ID) || candidate.Kind == model.ConstraintPrimaryKey && slices.Contains(candidate.Columns, column.ID))
		})
		if !stillRequired {
			column.Nullable = true
		}
	}
	if constraint.Index != 0 {
		published, lookupErr := snapshot.Descriptor(ctx, constraint.Index)
		if lookupErr != nil || published.Descriptor == nil || published.Descriptor.Kind() != model.ObjectIndex {
			return nil, errors.Join(model.ErrInvalidDescriptor, lookupErr)
		}
		indexSpec, specErr := published.Descriptor.SpecWithin(ctx, options.Descriptors)
		if specErr != nil || indexSpec.Index == nil {
			return nil, errors.Join(model.ErrInvalidDescriptor, specErr)
		}
		spec.Table.Indexes = slices.DeleteFunc(spec.Table.Indexes, func(id model.ObjectID) bool { return id == constraint.Index })
		spec.Version = table.Version() + 3
		target, createErr := model.NewDescriptor(ctx, spec, options.Descriptors)
		if createErr != nil {
			return nil, createErr
		}
		b.dependency(published.Descriptor, 0)
		return &BoundSchemaDDL{source: table, mutation: constraint.Index, definition: schema.JobDefinition{Operation: schema.SchemaDropIndex, SourceTable: table, TargetTable: target, SourceIndexes: []*model.Descriptor{published.Descriptor}}, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
	}
	spec.Version = table.Version() + 1
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	result := &BoundSchemaDDL{source: table, mutation: constraint.ID, metadataOnly: target, dependencies: b.dependencies, warnings: b.warnings}
	if constraint.Kind == model.ConstraintCheck && !constraint.NoInherit && spec.Table.InheritanceChildren != 0 && constraint.InheritanceCount == 0 {
		childUpdates, propErr := propagateInheritedCheckDrop(ctx, b, snapshot, options, table.ID(), constraint.Name)
		if propErr != nil {
			return nil, propErr
		}
		result.auxiliaryUpdates = append(result.auxiliaryUpdates, childUpdates...)
		result.dependencies = b.dependencies
	}
	return result, ctx.Err()
}

func collectInheritanceDescendants(ctx context.Context, snapshot CatalogSnapshot, rootID model.ObjectID, limits model.DescriptorLimits) ([]*model.Descriptor, error) {
	byParent := make(map[model.ObjectID][]*model.Descriptor)
	err := snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schemaDesc catalog.PublishedDescriptor) error {
		if schemaDesc.Descriptor == nil || schemaDesc.Descriptor.State() != model.StatePublic {
			return nil
		}
		return snapshot.ScanNamespace(ctx, schemaDesc.Descriptor.ID(), model.ObjectTable, func(tbl catalog.PublishedDescriptor) error {
			if tbl.Descriptor == nil || tbl.Descriptor.State() != model.StatePublic {
				return nil
			}
			meta, ok := tbl.Descriptor.Table()
			if !ok || len(meta.Parents) == 0 {
				return nil
			}
			for _, p := range meta.Parents {
				byParent[p] = append(byParent[p], tbl.Descriptor)
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	var result []*model.Descriptor
	seen := map[model.ObjectID]bool{rootID: true}
	queue := []model.ObjectID{rootID}
	for len(queue) > 0 {
		parentID := queue[0]
		queue = queue[1:]
		for _, child := range byParent[parentID] {
			if seen[child.ID()] {
				continue
			}
			seen[child.ID()] = true
			result = append(result, child)
			if len(result) > limits.MaxItems {
				return nil, model.ErrDescriptorLimit
			}
			queue = append(queue, child.ID())
		}
	}
	return result, nil
}

func propagateInheritedCheckAdd(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, parentID model.ObjectID, parentConstraint model.ConstraintDescriptor, rawExpr *zpgparse.Node) ([]ObjectMutation, []model.ObjectID, error) {
	descendants, err := collectInheritanceDescendants(ctx, snapshot, parentID, options.Descriptors)
	if err != nil {
		return nil, nil, err
	}
	var updates []ObjectMutation
	var validations []model.ObjectID
	for _, child := range descendants {
		b.dependency(child, 0)
		childSpec, err := child.SpecWithin(ctx, options.Descriptors)
		if err != nil || childSpec.Table == nil {
			return nil, nil, errors.Join(model.ErrInvalidDescriptor, err)
		}
		childSpec.Table = cloneTableMetadata(childSpec.Table)
		existingIdx := slices.IndexFunc(childSpec.Table.Constraints, func(c model.ConstraintDescriptor) bool {
			return c.Kind == model.ConstraintCheck && c.Name == parentConstraint.Name
		})
		if existingIdx >= 0 {
			childSpec.Table.Constraints[existingIdx].InheritanceCount++
			if childSpec.Table.Constraints[existingIdx].ParentTable == 0 {
				childSpec.Table.Constraints[existingIdx].ParentTable = parentID
				childSpec.Table.Constraints[existingIdx].ParentConstraint = parentConstraint.ID
			}
		} else {
			childID, childOID, allocErr := options.Identities.AllocateDeclarationIdentity(ctx)
			if allocErr != nil {
				return nil, nil, allocErr
			}
			pendingChild, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
			if err != nil {
				return nil, nil, err
			}
			exprDef, err := DefineCheck(ctx, rawExpr, pendingChild, childID, snapshot, options.Bind)
			if err != nil {
				return nil, nil, err
			}
			childConstraint := model.ConstraintDescriptor{
				ID:               childID,
				OID:              childOID,
				Name:             parentConstraint.Name,
				Kind:             model.ConstraintCheck,
				Validated:        parentConstraint.Validated,
				Enforced:         parentConstraint.Enforced,
				Local:            false,
				InheritanceCount: 1,
				ParentTable:      parentID,
				ParentConstraint: parentConstraint.ID,
				Expression:       exprDef,
			}
			for _, dep := range exprDef.Dependencies {
				if dep.Object != child.ID() {
					childSpec.Dependencies = appendIndexDependency(childSpec.Dependencies, dep)
				}
			}
			childSpec.Table.Constraints = append(childSpec.Table.Constraints, childConstraint)
			if childConstraint.Validated {
				validations = append(validations, childConstraint.ID)
			}
		}
		if len(childSpec.Dependencies) != 0 {
			slices.SortFunc(childSpec.Dependencies, compareDependency)
			childSpec.Dependencies = slices.Compact(childSpec.Dependencies)
		}
		childSpec.Version = child.Version() + 1
		nextChild, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
		if err != nil {
			return nil, nil, err
		}
		updates = append(updates, ObjectMutation{Base: child, Next: nextChild})
	}
	return updates, validations, nil
}

func propagateInheritedCheckDrop(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, parentID model.ObjectID, constraintName string) ([]ObjectMutation, error) {
	descendants, err := collectInheritanceDescendants(ctx, snapshot, parentID, options.Descriptors)
	if err != nil {
		return nil, err
	}
	var updates []ObjectMutation
	for _, child := range descendants {
		b.dependency(child, 0)
		childSpec, err := child.SpecWithin(ctx, options.Descriptors)
		if err != nil || childSpec.Table == nil {
			return nil, errors.Join(model.ErrInvalidDescriptor, err)
		}
		idx := slices.IndexFunc(childSpec.Table.Constraints, func(c model.ConstraintDescriptor) bool {
			return c.Kind == model.ConstraintCheck && c.Name == constraintName
		})
		if idx < 0 {
			continue
		}
		childSpec.Table = cloneTableMetadata(childSpec.Table)
		if childSpec.Table.Constraints[idx].InheritanceCount > 0 {
			childSpec.Table.Constraints[idx].InheritanceCount--
		}
		if childSpec.Table.Constraints[idx].InheritanceCount == 0 {
			if !childSpec.Table.Constraints[idx].Local {
				childSpec.Table.Constraints = slices.Delete(childSpec.Table.Constraints, idx, idx+1)
			} else {
				childSpec.Table.Constraints[idx].ParentTable = 0
				childSpec.Table.Constraints[idx].ParentConstraint = 0
			}
		}
		if len(childSpec.Dependencies) != 0 {
			slices.SortFunc(childSpec.Dependencies, compareDependency)
			childSpec.Dependencies = slices.Compact(childSpec.Dependencies)
		}
		childSpec.Version = child.Version() + 1
		nextChild, err := model.NewDescriptor(ctx, childSpec, options.Descriptors)
		if err != nil {
			return nil, err
		}
		updates = append(updates, ObjectMutation{Base: child, Next: nextChild})
	}
	return updates, nil
}

func bindOnlineAlterConstraint(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, spec model.DescriptorSpec, command *zpgparse.AlterTableCmd) (*BoundSchemaDDL, error) {
	alter := command.Def.GetAtalterConstraint()
	if alter == nil || alter.Conname == "" || command.Name != "" || command.MissingOk {
		return nil, bindError("42601", "invalid ALTER CONSTRAINT declaration", 0)
	}
	position := slices.IndexFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Name == alter.Conname })
	if position < 0 {
		return nil, bindError("42704", "constraint does not exist", 0)
	}
	constraint := &spec.Table.Constraints[position]
	if alter.AlterDeferrability {
		switch constraint.Kind {
		case model.ConstraintForeignKey, model.ConstraintExclude:
			constraint.Deferrable, constraint.InitiallyDeferred = alter.Deferrable, alter.Initdeferred
		case model.ConstraintPrimaryKey, model.ConstraintUnique:
			if constraint.Deferrable != alter.Deferrable {
				constraint.Deferrable, constraint.InitiallyDeferred = alter.Deferrable, alter.Initdeferred
				if constraint.InitiallyDeferred && !constraint.Deferrable {
					return nil, bindError("42601", "INITIALLY DEFERRED requires DEFERRABLE", 0)
				}
				indexes, indexErr := onlineSourceIndexes(ctx, b, snapshot, options, spec.Table.Indexes)
				if indexErr != nil {
					return nil, indexErr
				}
				spec.Version = table.Version() + 3
				spec.Physical.Generation++
				target, createErr := model.NewDescriptor(ctx, spec, options.Descriptors)
				if createErr != nil {
					return nil, createErr
				}
				targetIndexes, targetErr := onlineTargetIndexes(ctx, snapshot, options, indexes, target, 0)
				if targetErr != nil {
					return nil, targetErr
				}
				indexPosition := slices.IndexFunc(targetIndexes, func(index *model.Descriptor) bool { return index.ID() == constraint.Index })
				if indexPosition < 0 {
					return nil, model.ErrInvalidDescriptor
				}
				indexSpec, specErr := targetIndexes[indexPosition].SpecWithin(ctx, options.Descriptors)
				if specErr != nil || indexSpec.Index == nil {
					return nil, errors.Join(model.ErrInvalidDescriptor, specErr)
				}
				indexSpec.Index.DeferredUnique = constraint.Deferrable
				targetIndexes[indexPosition], createErr = model.NewDescriptor(ctx, indexSpec, options.Descriptors)
				if createErr != nil {
					return nil, createErr
				}
				return &BoundSchemaDDL{source: table, definition: schema.JobDefinition{Operation: schema.SchemaRewritePrimaryKey, SourceTable: table, TargetTable: target, SourceIndexes: indexes, TargetIndexes: targetIndexes}, mutation: constraint.ID, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
			}
			constraint.InitiallyDeferred = alter.Initdeferred
		default:
			return nil, bindError("0A000", "constraint type is not deferrable", 0)
		}
		if constraint.InitiallyDeferred && !constraint.Deferrable {
			return nil, bindError("42601", "INITIALLY DEFERRED requires DEFERRABLE", 0)
		}
	}
	var validationID model.ObjectID
	if alter.AlterEnforceability {
		if constraint.Kind != model.ConstraintCheck && constraint.Kind != model.ConstraintForeignKey {
			return nil, bindError("0A000", "constraint enforceability cannot be altered", 0)
		}
		wasEnforced := constraint.Enforced
		constraint.Enforced = alter.IsEnforced
		if !constraint.Enforced {
			constraint.Validated = false
		} else if !wasEnforced {
			constraint.Validated = true
			validationID = constraint.ID
		}
	}
	if alter.AlterInheritability {
		if constraint.Kind != model.ConstraintCheck && constraint.Kind != model.ConstraintNotNull {
			return nil, bindError("42601", "constraint inheritability cannot be altered", 0)
		}
		constraint.NoInherit = alter.Noinherit
	}
	spec.Version = table.Version() + 1
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}
	return &BoundSchemaDDL{source: table, mutation: constraint.ID, metadataOnly: target, validation: validationID, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
}

func bindOnlineValidateConstraint(ctx context.Context, b *binder, options CreateDeclarationOptions, table *model.Descriptor, spec model.DescriptorSpec, command *zpgparse.AlterTableCmd) (*BoundSchemaDDL, error) {
	if command.Name == "" || command.Def != nil || command.MissingOk {
		return nil, bindError("42601", "invalid VALIDATE CONSTRAINT declaration", 0)
	}
	position := slices.IndexFunc(spec.Table.Constraints, func(constraint model.ConstraintDescriptor) bool { return constraint.Name == command.Name })
	if position < 0 {
		return nil, bindError("42704", "constraint does not exist", 0)
	}
	constraint := &spec.Table.Constraints[position]
	if constraint.Validated {
		return nil, bindError("55000", "constraint is already validated", 0)
	}
	if !constraint.Enforced {
		return nil, bindError("55000", "constraint is not enforced", 0)
	}
	switch constraint.Kind {
	case model.ConstraintCheck, model.ConstraintForeignKey:
	default:
		return nil, bindError("42809", "constraint kind cannot be validated", 0)
	}
	constraint.Validated = true
	spec.Version = table.Version() + 1
	target, err := model.NewDescriptor(ctx, spec, options.Descriptors)
	if err != nil {
		return nil, err
	}

	return &BoundSchemaDDL{source: table, mutation: constraint.ID, metadataOnly: target, validation: constraint.ID, dependencies: b.dependencies, warnings: b.warnings}, ctx.Err()
}
func ensureConstraintUnreferenced(ctx context.Context, b *binder, snapshot CatalogSnapshot, object, constraint model.ObjectID) error {
	var visited uint32
	return snapshot.ScanNamespace(ctx, 0, model.ObjectSchema, func(schema catalog.PublishedDescriptor) error {
		if schema.Descriptor == nil || schema.Descriptor.State() != model.StatePublic {
			return nil
		}
		return snapshot.ScanNamespace(ctx, schema.Descriptor.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			if visited == b.options.Limits.MaxOperands {
				return bindError("54000", "constraint dependency scan limit exceeded", 0)
			}
			visited++
			if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
				return nil
			}
			spec, err := published.Descriptor.SpecWithin(ctx, b.descriptorLimits())
			if err != nil || spec.Table == nil {
				return err
			}
			for _, candidate := range spec.Table.Constraints {
				if candidate.Kind == model.ConstraintForeignKey && candidate.ReferencedObject == object && candidate.ReferencedConstraint == constraint {
					return bindError("2BP01", "constraint is required by a foreign key", 0)
				}
			}
			return nil
		})
	})
}

func bindAddedColumnConstraints(ctx context.Context, b *binder, snapshot CatalogSnapshot, options CreateDeclarationOptions, table *model.Descriptor, spec *model.DescriptorSpec, columnID model.ColumnID, raw []*zpgparse.Constraint) ([]*model.Descriptor, []model.ObjectID, error) {
	if b == nil || snapshot == nil || table == nil || spec == nil || spec.Table == nil || columnID == 0 {
		return nil, nil, model.ErrInvalidDescriptor
	}
	column, found := declarationColumn(spec.Table.Columns, columnID)
	if !found || column.SystemIdentity {
		return nil, nil, model.ErrInvalidDescriptor
	}
	indexConstraints := 0
	for _, declaration := range raw {
		if declaration != nil && (declaration.Contype == zpgparse.ConstrType_CONSTR_PRIMARY || declaration.Contype == zpgparse.ConstrType_CONSTR_UNIQUE || declaration.Contype == zpgparse.ConstrType_CONSTR_EXCLUSION) {
			indexConstraints++
		}
	}
	if len(raw) > options.Descriptors.MaxItems-len(spec.Table.Constraints) || indexConstraints > options.Descriptors.MaxItems-len(spec.Table.Indexes) {
		return nil, nil, model.ErrDescriptorLimit
	}
	state := constraintDeclarationState(b, options, table, spec.Table)
	columns := make(map[string]model.ColumnID, len(spec.Table.Columns))
	nulls := make(map[model.ColumnID]bool)
	for _, declaration := range spec.Table.Columns {
		if !declaration.SystemIdentity {
			columns[declaration.Name] = declaration.ID
		}
	}
	var creates []*model.Descriptor
	var validations []model.ObjectID
	for _, declaration := range raw {
		if declaration == nil {
			return nil, nil, bindError("42601", "missing inline constraint", 0)
		}
		if err := ordinaryConstraint(declaration); err != nil {
			return nil, nil, err
		}
		var constraint model.ConstraintDescriptor
		var index *model.IndexMetadata
		var label string
		var err error
		switch declaration.Contype {
		case zpgparse.ConstrType_CONSTR_CHECK:
			if declaration.RawExpr == nil {
				return nil, nil, bindError("42601", "CHECK constraint requires an expression", int(declaration.Location)+1)
			}
			constraint, err = declarationConstraintAttributes(declaration, model.ConstraintCheck)
			label = "check"
		case zpgparse.ConstrType_CONSTR_PRIMARY, zpgparse.ConstrType_CONSTR_UNIQUE:
			if declaration.Indexname != "" || declaration.WhereClause != nil || declaration.AccessMethod != "" && declaration.AccessMethod != "btree" || declaration.Indexspace != "" || len(declaration.Options) != 0 || declaration.IsNoInherit {
				return nil, nil, bindError("42601", "invalid inline key constraint attributes", int(declaration.Location)+1)
			}
			kind := model.ConstraintUnique
			label = "key"
			if declaration.Contype == zpgparse.ConstrType_CONSTR_PRIMARY {
				kind, label = model.ConstraintPrimaryKey, "pkey"
				if kind == model.ConstraintPrimaryKey && declaration.NullsNotDistinct {
					return nil, nil, bindError("42601", "NULLS NOT DISTINCT is not valid for a primary key", int(declaration.Location)+1)
				}
				if slices.ContainsFunc(spec.Table.Constraints, func(existing model.ConstraintDescriptor) bool { return existing.Kind == model.ConstraintPrimaryKey }) {
					return nil, nil, bindError("42P16", "multiple primary keys", int(declaration.Location)+1)
				}
			}
			constraint, err = declarationConstraintAttributes(declaration, kind)
			if err == nil {
				constraint.Columns, err = declarationColumns(declaration.Keys, columnID, columns)
			}
			if err == nil {
				index = &model.IndexMetadata{Owner: options.Owner, Table: table.ID(), Unique: true, NullsNotDistinct: declaration.NullsNotDistinct, DeferredUnique: constraint.Deferrable}
				for _, constrained := range constraint.Columns {
					key, keyErr := declarationKey(spec.Table.Columns, nulls, constrained, kind == model.ConstraintPrimaryKey)
					if keyErr != nil {
						return nil, nil, keyErr
					}
					if col, ok := declarationColumn(spec.Table.Columns, constrained); ok {
						key.OperatorClass = defaultBTreeOperatorClass(col.Layout)
					}
					index.Keys = append(index.Keys, key)
					if kind == model.ConstraintPrimaryKey {
						constrainedColumn, ok := declarationColumn(spec.Table.Columns, constrained)
						if !ok {
							return nil, nil, model.ErrInvalidDescriptor
						}
						constrainedColumn.Nullable = false
						nulls[constrained] = false
					}
				}
				index.Include, err = declarationColumnsOptional(declaration.Including, columns)
				for _, included := range index.Include {
					if slices.Contains(constraint.Columns, included) {
						return nil, nil, bindError("42701", "included column overlaps constraint key", int(declaration.Location)+1)
					}
				}
			}
		case zpgparse.ConstrType_CONSTR_FOREIGN:
			var referenced *model.Descriptor
			constraint, referenced, err = state.bindForeignKey(ctx, spec, tableConstraint{raw: declaration, column: columnID}, columns)
			label = "fkey"
			if referenced != nil {
				constraint.ReferencedObject = referenced.ID()
			}
		case zpgparse.ConstrType_CONSTR_EXCLUSION:
			constraint, index, err = state.bindExclusion(ctx, spec, tableConstraint{raw: declaration, column: columnID}, columns)
			label = "excl"
			if err == nil && (len(constraint.Columns) != 1 || constraint.Columns[0] != columnID) {
				return nil, nil, bindError("42601", "inline exclusion constraint must constrain its column", int(declaration.Location)+1)
			}
		default:
			return nil, nil, bindError("42601", "invalid inline constraint kind", int(declaration.Location)+1)
		}
		if err != nil {
			return nil, nil, err
		}
		addition := column.Name
		if len(constraint.Columns) != 0 && constraint.Kind != model.ConstraintPrimaryKey {
			addition = declarationKeyName(spec.Table.Columns, constraint.Columns)
		}
		constraint.Name, err = state.constraintName(declaration.Conname, addition, label, constraint.Kind == model.ConstraintPrimaryKey || constraint.Kind == model.ConstraintUnique || constraint.Kind == model.ConstraintExclude)
		if err != nil {
			return nil, nil, err
		}
		constraint.ID, constraint.OID, err = state.allocate()
		if err != nil {
			return nil, nil, err
		}
		if constraint.Kind == model.ConstraintCheck {
			pendingSpec := *spec
			pending, createErr := model.NewDescriptor(ctx, pendingSpec, options.Descriptors)
			if createErr != nil {
				return nil, nil, createErr
			}
			constraint.Expression, err = DefineCheck(ctx, declaration.RawExpr, pending, constraint.ID, snapshot, options.Bind)
			if err != nil {
				return nil, nil, err
			}
			for _, dependency := range constraint.Expression.Dependencies {
				if dependency.Object != table.ID() {
					spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
				}
			}
		}
		if index != nil {
			indexID, indexOID, allocateErr := state.allocate()
			if allocateErr != nil {
				return nil, nil, allocateErr
			}
			constraint.Index = indexID
			dependencies := []model.Dependency{{Object: table.ID(), Version: table.Version()}}
			for _, constrained := range constraint.Columns {
				dependencies = appendIndexDependency(dependencies, model.Dependency{Object: table.ID(), Version: table.Version(), Column: constrained})
			}
			for _, included := range index.Include {
				dependencies = appendIndexDependency(dependencies, model.Dependency{Object: table.ID(), Version: table.Version(), Column: included})
			}
			for _, dependency := range b.dependencies {
				if dependency.Object != table.ID() {
					dependencies = appendIndexDependency(dependencies, dependency)
				}
			}
			created, createErr := model.NewDescriptor(ctx, model.DescriptorSpec{
				ID: indexID, OID: indexOID,
				Name: model.NamespaceName{Parent: table.Name().Parent, Kind: model.ObjectIndex, Name: constraint.Name},
				Version: 1, State: model.StatePublic, Physical: options.Physical,
				Dependencies: dependencies, Index: index,
			}, options.Descriptors)
			if createErr != nil {
				return nil, nil, createErr
			}
			spec.Table.Indexes = append(spec.Table.Indexes, indexID)
			creates = append(creates, created)
		}
		spec.Table.Constraints = append(spec.Table.Constraints, constraint)
		if constraint.Validated {
			validations = append(validations, constraint.ID)
		}
	}
	for _, dependency := range b.dependencies {
		if dependency.Object != table.ID() {
			spec.Dependencies = appendIndexDependency(spec.Dependencies, dependency)
		}
	}
	return creates, validations, ctx.Err()
}

