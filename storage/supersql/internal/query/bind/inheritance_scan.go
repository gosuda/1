package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) inheritanceRelation(root *model.Descriptor, rootTable *model.TableMetadata, inherit bool, scope *scope, position int, occurrence model.ScanOccurrence) (relation, error) {
	logicalColumns := make([]model.ColumnDescriptor, 0, len(rootTable.Columns))
	for _, descriptorColumn := range rootTable.Columns {
		if descriptorColumn.State == model.StatePublic {
			logicalColumns = append(logicalColumns, descriptorColumn)
		}
	}
	descriptors := []*model.Descriptor{root}
	if inherit && rootTable.InheritanceChildren != 0 {
		var err error
		descriptors, err = b.inheritanceDescriptors(root, position)
		if err != nil {
			return relation{}, err
		}
	}
	branches := make([]relation, 0, len(descriptors))
	for _, descriptor := range descriptors {
		metadata, ok := descriptor.Table()
		if !ok {
			return relation{}, bindError("XX000", "inheritance member is not a table", position)
		}
		if metadata.PartitionStrategy != 0 {
			continue
		}
		branch, err := b.inheritanceBranch(rootTable, descriptor, &metadata, scope, position, occurrence)
		if err != nil {
			return relation{}, err
		}
		branches = append(branches, branch)
	}
	if len(branches) == 0 {
		empty, err := b.emptyInheritanceRelation(root, rootTable, scope)
		empty.occurrence = occurrence
		return empty, err
	}
	combined := branches[0]
	for _, branch := range branches[1:] {
		if len(combined.columns) != len(branch.columns) {
			return relation{}, bindError("XX000", "inheritance relation width changed", position)
		}
		columns := slices.Clone(combined.columns)
		for i := range columns {
			columns[i].Nullable = columns[i].Nullable || branch.columns[i].Nullable
			var object model.ObjectID
			var columnID model.ColumnID
			if i < len(logicalColumns) {
				object = root.ID()
				columnID = logicalColumns[i].ID
			}
			slot, err := b.slot(scope, columns[i].Layout, columns[i].Nullable, object, columnID)
			if err != nil {
				return relation{}, err
			}
			columns[i].Slot = slot
		}
		effects := combined.effects.Union(branch.effects)
		id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.SetOp, Scope: scope.id, SetKind: ir.Union, All: true, Effects: effects}, Inputs: []ir.RelID{combined.id, branch.id}, Outputs: outputSlots(columns)})
		if err != nil {
			return relation{}, err
		}
		combined = relation{id: id, columns: columns, effects: effects, occurrence: occurrence}
	}
	return combined, nil
}

func (b *binder) inheritanceDescriptors(root *model.Descriptor, position int) ([]*model.Descriptor, error) {
	if b.catalog == nil {
		return nil, bindError("55000", "catalog snapshot is not configured", position)
	}
	var schemas []*model.Descriptor
	var scanned uint64
	if err := b.catalog.ScanNamespace(b.ctx, 0, model.ObjectSchema, func(published catalog.PublishedDescriptor) error {
		if published.Descriptor == nil || published.Descriptor.State() != model.StatePublic {
			return nil
		}
		scanned++
		if scanned > uint64(b.options.Limits.MaxOperands) {
			return bindError("54000", "inheritance catalog scan limit exceeded", position)
		}
		schemas = append(schemas, published.Descriptor)
		return nil
	}); err != nil {
		return nil, err
	}
	children := make(map[model.ObjectID][]*model.Descriptor)
	for _, schema := range schemas {
		if err := b.catalog.ScanNamespace(b.ctx, schema.ID(), model.ObjectTable, func(published catalog.PublishedDescriptor) error {
			descriptor := published.Descriptor
			if descriptor == nil || descriptor.State() != model.StatePublic {
				return nil
			}
			scanned++
			if scanned > uint64(b.options.Limits.MaxOperands) {
				return bindError("54000", "inheritance catalog scan limit exceeded", position)
			}
			metadata, ok := descriptor.Table()
			if !ok {
				return bindError("XX000", "table descriptor has no table metadata", position)
			}
			for _, parent := range metadata.Parents {
				if metadata.PartitionBound != nil && metadata.PartitionState == model.PartitionDetachPending {
					continue
				}
				children[parent] = append(children[parent], descriptor)
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	for parent := range children {
		slices.SortFunc(children[parent], func(left, right *model.Descriptor) int {
			return compareObjectID(left.ID(), right.ID())
		})
	}
	result := []*model.Descriptor{root}
	visiting := make(map[model.ObjectID]bool)
	visited := map[model.ObjectID]bool{root.ID(): true}
	var visit func(model.ObjectID) error
	visit = func(parent model.ObjectID) error {
		if visiting[parent] {
			return bindError("42P17", "circular inheritance hierarchy", position)
		}
		visiting[parent] = true
		for _, descriptor := range children[parent] {
			if visiting[descriptor.ID()] {
				return bindError("42P17", "circular inheritance hierarchy", position)
			}
			if visited[descriptor.ID()] {
				continue
			}
			visited[descriptor.ID()] = true
			result = append(result, descriptor)
			if uint64(len(result)) > uint64(b.options.Limits.MaxOperands) {
				return bindError("54000", "inheritance member limit exceeded", position)
			}
			if err := visit(descriptor.ID()); err != nil {
				return err
			}
		}
		delete(visiting, parent)
		return nil
	}
	if err := visit(root.ID()); err != nil {
		return nil, err
	}
	return result, nil
}

func compareObjectID(left, right model.ObjectID) int {
	if left < right {
		return -1
	}
	if left > right {
		return 1
	}
	return 0
}

func (b *binder) inheritanceBranch(rootTable *model.TableMetadata, descriptor *model.Descriptor, table *model.TableMetadata, scope *scope, position int, occurrence model.ScanOccurrence) (relation, error) {
	type columnMapping struct {
		wanted model.ColumnDescriptor
		actual model.ColumnDescriptor
	}
	mappings := make([]columnMapping, 0, len(rootTable.Columns))
	hasVirtual := false
	for _, wanted := range rootTable.Columns {
		if wanted.State != model.StatePublic {
			continue
		}
		index := slices.IndexFunc(table.Columns, func(candidate model.ColumnDescriptor) bool {
			if candidate.State != model.StatePublic {
				return false
			}
			if wanted.SystemIdentity {
				return candidate.SystemIdentity && sameType(candidate.Layout, wanted.Layout)
			}
			return !candidate.SystemIdentity && candidate.Name == wanted.Name && sameType(candidate.Layout, wanted.Layout)
		})
		if index < 0 {
			return relation{}, bindError("42804", "inherited column differs from parent", position)
		}
		actual := table.Columns[index]
		hasVirtual = hasVirtual || actual.Generated != nil && !actual.GeneratedStored
		mappings = append(mappings, columnMapping{wanted: wanted, actual: actual})
	}
	scanned := make([]model.ColumnDescriptor, 0, len(table.Columns))
	if hasVirtual {
		for _, column := range table.Columns {
			if column.State == model.StatePublic && (column.Generated == nil || column.GeneratedStored) {
				scanned = append(scanned, column)
			}
		}
		slices.SortFunc(scanned, func(left, right model.ColumnDescriptor) int {
			if left.ID < right.ID {
				return -1
			}
			if left.ID > right.ID {
				return 1
			}
			return 0
		})
	} else {
		for _, mapping := range mappings {
			scanned = append(scanned, mapping.actual)
		}
	}
	scanColumns := make([]column, 0, len(scanned))
	scanByID := make(map[model.ColumnID]column, len(scanned))
	physicalLayouts := make([]storage.ColumnLayout, 0, len(scanned))
	for _, descriptorColumn := range scanned {
		slot, err := b.slot(scope, descriptorColumn.Layout, descriptorColumn.Nullable, descriptor.ID(), descriptorColumn.ID)
		if err != nil {
			return relation{}, err
		}
		boundColumn := column{ResultColumn: ResultColumn{Name: descriptorColumn.Name, Layout: descriptorColumn.Layout, Nullable: descriptorColumn.Nullable, Slot: slot}, internal: descriptorColumn.SystemIdentity}
		scanColumns = append(scanColumns, boundColumn)
		scanByID[descriptorColumn.ID] = boundColumn
		physicalLayouts = append(physicalLayouts, storage.ColumnLayout{ID: descriptorColumn.ID, Type: descriptorColumn.Layout.Type, Children: descriptorColumn.Layout.Children, Nullable: descriptorColumn.Nullable})
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: scope.id, Occurrence: occurrence, Object: descriptor.ID(), DescriptorVersion: descriptor.Version(), Generation: descriptor.Physical().Generation, EncodingVersion: descriptor.Physical().EncodingVersion, Span: sourceSpan(position)}, Outputs: outputSlots(scanColumns)})
	if err != nil {
		return relation{}, err
	}
	branch := relation{id: id, columns: scanColumns, occurrence: occurrence}
	if hasVirtual {
		columns := make([]column, 0, len(mappings))
		bindings := make([]ir.Binding, 0, len(mappings))
		effects := branch.effects
		for _, mapping := range mappings {
			var value expression
			if mapping.actual.Generated != nil && !mapping.actual.GeneratedStored {
				bound, err := BindStoredGenerated(b.ctx, mapping.actual.Generated, mapping.actual.ID, physicalLayouts, mapping.actual.Layout, b.catalog, b.options)
				if err != nil {
					return relation{}, err
				}
				for _, dependency := range bound.Dependencies {
					published, err := b.catalog.Descriptor(b.ctx, dependency.Object)
					if err != nil {
						return relation{}, err
					}
					b.dependency(published.Descriptor, dependency.Column)
				}
				value, err = b.importColumnExpression(bound, scanByID, scope, position)
				if err != nil {
					return relation{}, err
				}
			} else {
				input, ok := scanByID[mapping.actual.ID]
				if !ok {
					return relation{}, model.ErrInvalidDescriptor
				}
				value, err = b.reference(scope, input, position)
				if err != nil {
					return relation{}, err
				}
			}
			slot, err := b.slot(scope, mapping.wanted.Layout, value.nullable, descriptor.ID(), mapping.actual.ID)
			if err != nil {
				return relation{}, err
			}
			columns = append(columns, column{ResultColumn: ResultColumn{Name: mapping.wanted.Name, Layout: mapping.wanted.Layout, Nullable: value.nullable, Slot: slot}, internal: mapping.wanted.SystemIdentity})
			bindings = append(bindings, ir.Binding{Slot: slot, Expr: value.id})
			effects = effects.Union(value.effects)
		}
		id, err = b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: scope.id, Effects: effects}, Inputs: []ir.RelID{branch.id}, Outputs: outputSlots(columns), Bindings: bindings})
		if err != nil {
			return relation{}, err
		}
		branch = relation{id: id, columns: columns, effects: effects, occurrence: occurrence}
	}
	for _, mapping := range mappings {
		b.dependency(descriptor, mapping.actual.ID)
	}
	branch, err = b.projectRelationIdentity(branch, scope, descriptor)
	if err != nil {
		return relation{}, err
	}
	b.dependency(descriptor, 0)
	return branch, nil
}

func (b *binder) projectRelationIdentity(input relation, scope *scope, descriptor *model.Descriptor) (relation, error) {
	physical := descriptor.Physical()
	identity := []struct {
		name   string
		layout model.TypeLayout
		value  int64
		system bool
	}{
		{name: "tableoid", layout: b.primitive(26), value: int64(descriptor.OID()), system: true},
		{layout: b.primitive(20), value: int64(descriptor.ID())},
		{name: "__supersql_descriptor_version", layout: b.primitive(20), value: int64(descriptor.Version())},
		{name: "__supersql_generation", layout: b.primitive(20), value: int64(physical.Generation)},
		{name: "__supersql_encoding_version", layout: b.primitive(20), value: int64(physical.EncodingVersion)},
	}
	columns := make([]column, 0, len(input.columns)+len(identity))
	bindings := make([]ir.Binding, 0, len(input.columns)+len(identity))
	for _, existing := range input.columns {
		reference, err := b.reference(scope, existing, 0)
		if err != nil {
			return relation{}, err
		}
		columns = append(columns, existing)
		bindings = append(bindings, ir.Binding{Slot: existing.Slot, Expr: reference.id})
	}
	for _, item := range identity {
		value, err := b.addScalar(scope, expression{typ: item.layout, value: model.Int64Val(item.value)}, ir.Scalar{Kind: ir.Const}, nil)
		if err != nil {
			return relation{}, err
		}
		slot, err := b.slot(scope, item.layout, false, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns = append(columns, column{ResultColumn: ResultColumn{Name: item.name, Layout: item.layout, Slot: slot}, system: item.system, internal: !item.system})
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: value.id})
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: scope.id, Effects: input.effects}, Inputs: []ir.RelID{input.id}, Outputs: outputSlots(columns), Bindings: bindings})
	if err != nil {
		return relation{}, err
	}
	return relation{id: id, columns: columns, effects: input.effects, occurrence: input.occurrence}, nil
}

func (b *binder) emptyInheritanceRelation(root *model.Descriptor, table *model.TableMetadata, scope *scope) (relation, error) {
	columns := make([]column, 0, len(table.Columns)+5)
	for _, descriptorColumn := range table.Columns {
		if descriptorColumn.State != model.StatePublic {
			continue
		}
		slot, err := b.slot(scope, descriptorColumn.Layout, descriptorColumn.Nullable, root.ID(), descriptorColumn.ID)
		if err != nil {
			return relation{}, err
		}
		columns = append(columns, column{ResultColumn: ResultColumn{Name: descriptorColumn.Name, Layout: descriptorColumn.Layout, Nullable: descriptorColumn.Nullable, Slot: slot}, internal: descriptorColumn.SystemIdentity})
	}
	for _, system := range []struct {
		name     string
		layout   model.TypeLayout
		tableoid bool
	}{
		{name: "tableoid", layout: b.primitive(26), tableoid: true},
		{layout: b.primitive(20)},
		{name: "__supersql_descriptor_version", layout: b.primitive(20)},
		{name: "__supersql_generation", layout: b.primitive(20)},
		{name: "__supersql_encoding_version", layout: b.primitive(20)},
	} {
		slot, err := b.slot(scope, system.layout, false, 0, 0)
		if err != nil {
			return relation{}, err
		}
		columns = append(columns, column{ResultColumn: ResultColumn{Name: system.name, Layout: system.layout, Slot: slot}, system: system.tableoid, internal: !system.tableoid})
	}
	id, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope.id, Rows: 0}, Outputs: outputSlots(columns)})
	return relation{id: id, columns: columns}, err
}
