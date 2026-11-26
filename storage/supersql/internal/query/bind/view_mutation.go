package bind

import (
	"context"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func analyzeAutomaticView(ctx context.Context, query *zpgparse.SelectStmt, bound *BoundStatement, snapshot CatalogSnapshot, path []string) (*model.ViewMutationMetadata, error) {
	if query == nil || bound == nil || snapshot == nil || query.WithClause != nil || len(query.DistinctClause) != 0 || len(query.GroupClause) != 0 || query.HavingClause != nil || query.LimitOffset != nil || query.LimitCount != nil || len(query.FromClause) != 1 || query.Op != zpgparse.SetOperation_SETOP_NONE && query.Op != zpgparse.SetOperation_SET_OPERATION_UNDEFINED {
		return nil, nil
	}
	rangeVar := query.FromClause[0].GetRangeVar()
	if rangeVar == nil || rangeVar.Catalogname != "" {
		return nil, nil
	}
	graph := bound.Graph()
	if automaticViewLogicalCatalogSource(graph, graph.Root()) {
		return nil, nil
	}
	parts := []string{rangeVar.Relname}
	if rangeVar.Schemaname != "" {
		parts = []string{rangeVar.Schemaname, rangeVar.Relname}
	}
	base, _, err := resolveNamespaceObject(ctx, snapshot, path, parts, []model.ObjectKind{model.ObjectTable, model.ObjectView}, "42P01", int(rangeVar.Location)+1)
	if err != nil {
		return nil, err
	}
	if !automaticViewRelationShape(graph, graph.Root(), base.ID()) {
		return nil, nil
	}
	columns := bound.Columns()
	mappings := make([]model.ViewMutationColumn, 0, len(columns))
	for i, column := range columns {
		origin, ok := automaticViewColumnOrigin(graph, graph.Root(), column.Slot)
		if !ok || origin.Object != base.ID() || origin.Column == 0 {
			continue
		}
		mappings = append(mappings, model.ViewMutationColumn{View: model.ColumnID(i + 1), Base: origin.Column})
	}
	return &model.ViewMutationMetadata{BaseRelation: base.ID(), BaseOID: base.OID(), Columns: mappings}, nil
}

func automaticViewLogicalCatalogSource(graph *ir.Graph, id ir.RelID) bool {
	for id != 0 {
		relation, ok := graph.Rel(id)
		if !ok {
			return false
		}
		if relation.Kind == ir.Apply {
			if relation.InputCount() != 2 {
				return false
			}
			id, _ = graph.Input(id, 0)
			continue
		}
		if relation.InputCount() == 0 {
			return relation.Kind == ir.CatalogScan && relation.Catalog != 0
		}
		if relation.InputCount() != 1 {
			return false
		}
		id, _ = graph.Input(id, 0)
	}
	return false
}

func automaticViewRelationShape(graph *ir.Graph, id ir.RelID, base model.ObjectID) bool {
	for id != 0 {
		relation, ok := graph.Rel(id)
		if !ok {
			return false
		}
		switch relation.Kind {
		case ir.Aggregate, ir.Window, ir.Limit, ir.Distinct, ir.CTEProducer, ir.CTEConsumer, ir.RecursiveUnion, ir.ProjectSet, ir.Sample:
			return false
		case ir.SetOp:
			return relation.SetKind == ir.Union && relation.All && relation.InputCount() == 2 && automaticViewOutputs(graph, id, relation, base)
		case ir.Values:
			return relation.Rows == 0 && relation.InputCount() == 0 && automaticViewOutputs(graph, id, relation, base)
		case ir.Apply:
			if relation.InputCount() != 2 {
				return false
			}
			id, _ = graph.Input(id, 0)
			continue
		}
		if relation.InputCount() == 0 {
			return relation.Kind == ir.Scan
		}
		if relation.InputCount() != 1 {
			return false
		}
		id, _ = graph.Input(id, 0)
	}
	return false
}

func automaticViewOutputs(graph *ir.Graph, id ir.RelID, relation ir.Rel, base model.ObjectID) bool {
	found := false
	for i := range relation.OutputCount() {
		slot, ok := graph.Output(id, i)
		info, infoOK := graph.Slot(slot)
		if !ok || !infoOK || info.Object != 0 && info.Object != base {
			return false
		}
		found = found || info.Object == base && info.Column != 0
	}
	return found
}

func automaticViewColumnOrigin(graph *ir.Graph, relationID ir.RelID, slot model.SlotID) (ir.SlotInfo, bool) {
	for relationID != 0 {
		info, ok := graph.Slot(slot)
		if !ok {
			return ir.SlotInfo{}, false
		}
		if info.Object != 0 {
			return info, true
		}
		relation, ok := graph.Rel(relationID)
		if !ok {
			return ir.SlotInfo{}, false
		}
		if relation.Kind == ir.Apply {
			relationID, _ = graph.Input(relationID, 0)
			continue
		}
		if relation.InputCount() != 1 {
			return ir.SlotInfo{}, false
		}
		if relation.Kind == ir.Project {
			var binding ir.Binding
			found := false
			for i := range relation.BindingCount() {
				candidate, ok := graph.Binding(relationID, i)
				if ok && candidate.Slot == slot {
					binding, found = candidate, true
					break
				}
			}
			if !found {
				return ir.SlotInfo{}, false
			}
			expression, ok := graph.Scalar(binding.Expr)
			if !ok || expression.Kind != ir.Slot {
				return ir.SlotInfo{}, false
			}
			slot = expression.Slot
		}
		relationID, _ = graph.Input(relationID, 0)
	}
	return ir.SlotInfo{}, false
}

type viewMutationLayer struct {
	descriptor *model.Descriptor
	view       model.ViewMetadata
	base       *model.Descriptor
}

func (b *binder) prepareMutationTarget(target mutationTarget, events []string) (mutationTarget, error) {
	target.executionDescriptor = target.descriptor
	target.executionTable = target.table
	if !target.view {
		return target, nil
	}
	target.columnMap = make(map[model.ColumnID]model.ColumnID)
	for _, column := range publicColumns(target.table) {
		target.columnMap[column.ID] = column.ID
	}
	current := target.descriptor
	for depth := uint32(0); depth < 64; depth++ {
		view, ok := current.View()
		if !ok || current.Kind() != model.ObjectView {
			table, tableOK := current.Table()
			if !tableOK || current.Kind() != model.ObjectTable {
				return mutationTarget{}, model.ErrInvalidDescriptor
			}
			target.executionDescriptor = current
			target.executionTable = &table
			return target, nil
		}
		triggered := 0
		for _, event := range events {
			if viewHasInsteadTrigger(view.Triggers, event) {
				triggered++
			}
		}
		if triggered == len(events) {
			target.executionDescriptor = current
			columns := slices.Clone(view.Columns)
			for i := range columns {
				columns[i].Nullable = true
			}
			target.executionTable = &model.TableMetadata{Columns: columns, Triggers: view.Triggers}
			target.logicalTrigger = true
			return target, nil
		}
		if triggered != 0 {
			return mutationTarget{}, bindError("0A000", "cannot mix INSTEAD OF triggers and automatic view updates", 0)
		}
		if view.Automatic == nil {
			return mutationTarget{}, bindError("55000", "view is not automatically updatable and has no applicable INSTEAD OF trigger", 0)
		}
		if len(view.Automatic.Columns) == 0 && slices.ContainsFunc(events, func(event string) bool {
			return strings.EqualFold(event, "insert") || strings.EqualFold(event, "update")
		}) {
			return mutationTarget{}, bindError("55000", "view has no updatable columns", 0)
		}
		published, err := b.catalog.Descriptor(b.ctx, view.Automatic.BaseRelation)
		if err != nil {
			return mutationTarget{}, err
		}
		base := published.Descriptor
		if base == nil || base.State() != model.StatePublic || base.OID() != view.Automatic.BaseOID {
			return mutationTarget{}, model.ErrInvalidDescriptor
		}
		b.dependency(current, 0)
		b.dependency(base, 0)
		target.viewLayers = append(target.viewLayers, viewMutationLayer{descriptor: current, view: view, base: base})
		next := make(map[model.ColumnID]model.ColumnID, len(target.columnMap))
		for logical, column := range target.columnMap {
			for _, mapping := range view.Automatic.Columns {
				if mapping.View == column {
					next[logical] = mapping.Base
					break
				}
			}
		}
		target.columnMap = next
		current = base
	}
	return mutationTarget{}, bindError("54001", "view update recursion limit exceeded", 0)
}

func viewHasInsteadTrigger(triggers []model.TriggerDescriptor, event string) bool {
	return slices.ContainsFunc(triggers, func(trigger model.TriggerDescriptor) bool {
		if !trigger.EnableMode.Fires(false) || !trigger.ForEachRow || !strings.EqualFold(trigger.Timing, "instead") {
			return false
		}
		return slices.ContainsFunc(trigger.Events, func(candidate string) bool {
			return strings.EqualFold(candidate, event)
		})
	})
}

func (target mutationTarget) automatic() bool {
	return len(target.viewLayers) != 0
}

func (target mutationTarget) physicalColumn(viewColumn model.ColumnID) (model.ColumnDescriptor, bool) {
	id, ok := target.columnMap[viewColumn]
	if !ok || target.executionTable == nil {
		return model.ColumnDescriptor{}, false
	}
	for _, column := range target.executionTable.Columns {
		if column.ID == id {
			return column, true
		}
	}
	return model.ColumnDescriptor{}, false
}

func (target mutationTarget) executionColumn(viewColumn model.ColumnID) (model.ColumnDescriptor, bool) {
	return target.physicalColumn(viewColumn)
}

func (target mutationTarget) assignmentColumn(viewColumn model.ColumnDescriptor) model.ColumnDescriptor {
	if !target.automatic() {
		return viewColumn
	}
	id := viewColumn.ID
	for _, layer := range target.viewLayers {
		for _, column := range layer.view.Columns {
			if column.ID == id && (column.Default != nil || column.ConstantDefault != nil) {
				viewColumn.Default = column.Default
				viewColumn.ConstantDefault = column.ConstantDefault
				return viewColumn
			}
		}
		next := model.ColumnID(0)
		for _, mapping := range layer.view.Automatic.Columns {
			if mapping.View == id {
				next = mapping.Base
				break
			}
		}
		if next == 0 {
			return viewColumn
		}
		id = next
	}
	if column, ok := target.physicalColumn(viewColumn.ID); ok {
		viewColumn.Default = column.Default
		viewColumn.ConstantDefault = column.ConstantDefault
		viewColumn.IdentitySequence = column.IdentitySequence
		viewColumn.IdentityAlways = column.IdentityAlways
		viewColumn.Generated = column.Generated
		viewColumn.GeneratedStored = column.GeneratedStored
	}
	return viewColumn
}
func (target mutationTarget) hasViewDefault(viewColumn model.ColumnID) bool {
	id := viewColumn
	for _, layer := range target.viewLayers {
		for _, column := range layer.view.Columns {
			if column.ID == id && (column.Default != nil || column.ConstantDefault != nil) {
				return true
			}
		}
		id = mappedViewColumn(layer.view, id)
		if id == 0 {
			return false
		}
	}
	return false
}

func (target mutationTarget) automaticInsertOwners(columns []model.ColumnDescriptor, supplied func(model.ColumnID) bool) (map[model.ColumnID]model.ColumnID, error) {
	if !target.automatic() {
		return nil, nil
	}
	owners := make(map[model.ColumnID]model.ColumnID)
	explicit := make(map[model.ColumnID]bool)
	defaulted := make(map[model.ColumnID]bool)
	for _, column := range columns {
		destination, writable := target.executionColumn(column.ID)
		if !writable {
			continue
		}
		hasDefault := target.hasViewDefault(column.ID)
		if supplied(column.ID) {
			if explicit[destination.ID] {
				return nil, bindError("42701", "multiple assignments to same base column", 0)
			}
			owners[destination.ID] = column.ID
			explicit[destination.ID] = true
			defaulted[destination.ID] = hasDefault
			continue
		}
		if explicit[destination.ID] {
			continue
		}
		if _, exists := owners[destination.ID]; !exists || hasDefault && !defaulted[destination.ID] {
			owners[destination.ID] = column.ID
			defaulted[destination.ID] = hasDefault
		}
	}
	return owners, nil
}

func mappedViewColumn(view model.ViewMetadata, id model.ColumnID) model.ColumnID {
	for _, mapping := range view.Automatic.Columns {
		if mapping.View == id {
			return mapping.Base
		}
	}
	return 0
}

func (target mutationTarget) conflictTarget() mutationTarget {
	if !target.automatic() {
		return target
	}
	table := *target.executionTable
	table.Columns = make([]model.ColumnDescriptor, 0, len(target.table.Columns))
	for _, logical := range publicColumns(target.table) {
		mapped, ok := target.columnMap[logical.ID]
		if !ok {
			continue
		}
		for _, physical := range target.executionTable.Columns {
			if physical.ID != mapped || physical.State != model.StatePublic || physical.SystemIdentity {
				continue
			}
			column := physical
			column.Name = logical.Name
			table.Columns = append(table.Columns, column)
			break
		}
	}
	target.descriptor = target.executionDescriptor
	target.table = &table
	target.view = false
	return target
}

func (b *binder) bindViewMutationSteps(s *scope, target mutationTarget) ([]ir.ViewMutationStepSpec, ir.Effects, error) {
	checks := make([]bool, len(target.viewLayers))
	cascade := false
	for i, layer := range target.viewLayers {
		checks[i] = cascade || layer.view.CheckOption != ""
		cascade = cascade || layer.view.CheckOption == "cascaded"
	}
	steps := make([]ir.ViewMutationStepSpec, 0, len(target.viewLayers))
	var effects ir.Effects
	for i := len(target.viewLayers) - 1; i >= 0; i-- {
		layer := target.viewLayers[i]
		parsed := &zpgparse.RawStmt{}
		if err := proto.Unmarshal(layer.view.Query.Encoding, parsed); err != nil || parsed.Stmt == nil || parsed.Stmt.GetSelectStmt() == nil {
			return nil, effects, model.ErrInvalidDescriptor
		}
		query := parsed.Stmt.GetSelectStmt()
		if len(query.FromClause) != 1 || query.FromClause[0].GetRangeVar() == nil {
			return nil, effects, model.ErrInvalidDescriptor
		}
		local := *s
		local.columns, local.namespaces = nil, nil
		local.allowAggregate, local.allowWindow = false, false
		rangeVar := query.FromClause[0].GetRangeVar()
		var baseColumns []model.ColumnDescriptor
		if table, ok := layer.base.Table(); ok {
			baseColumns = table.Columns
		} else if view, ok := layer.base.View(); ok {
			baseColumns = view.Columns
		} else {
			return nil, effects, model.ErrInvalidDescriptor
		}
		for _, descriptor := range baseColumns {
			if descriptor.State != model.StatePublic || descriptor.SystemIdentity {
				continue
			}
			slot, err := b.slot(s, descriptor.Layout, descriptor.Nullable, layer.base.ID(), descriptor.ID)
			if err != nil {
				return nil, effects, err
			}
			local.columns = append(local.columns, column{ResultColumn: ResultColumn{Name: descriptor.Name, Layout: descriptor.Layout, Nullable: descriptor.Nullable, Slot: slot}})
		}
		aliased, err := aliasColumns(local.columns, rangeVar.Alias, rangeVar.Relname)
		if err != nil {
			return nil, effects, err
		}
		local.columns = aliased
		qualifier := relationQualifier{namespace: rangeVar.Schemaname, name: rangeVar.Relname}
		if rangeVar.Alias != nil {
			qualifier = relationQualifier{name: rangeVar.Alias.Aliasname}
		}
		for j := range local.columns {
			local.columns[j].qualifier = qualifier.name
			local.columns[j].namespace = qualifier.namespace
		}
		local.namespaces = []relationNamespace{{qualifier: qualifier, descriptor: layer.base, columns: slices.Clone(local.columns), physical: slices.Clone(local.columns)}}
		projections, projectionEffects, err := b.viewMutationProjection(query, &local)
		if err != nil {
			return nil, effects, err
		}
		if len(projections) != len(layer.view.Columns) {
			return nil, effects, model.ErrInvalidDescriptor
		}
		for j, projection := range projections {
			if !sameType(projection.typ, layer.view.Columns[j].Layout) {
				return nil, effects, model.ErrInvalidDescriptor
			}
		}
		var predicate expression
		if checks[i] {
			predicate, err = b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(true)}, ir.Scalar{Kind: ir.Const}, nil)
			if err != nil {
				return nil, effects, err
			}
			if query.WhereClause != nil {
				predicate, err = b.restrictedScalar(query.WhereClause, &local, "WHERE")
				if err == nil {
					predicate, err = b.coerce(&local, predicate, b.primitive(16), ir.ImplicitCast)
				}
				if err != nil {
					return nil, effects, err
				}
			}
		}
		inputs := make([]model.SlotID, len(local.columns))
		for j := range local.columns {
			inputs[j] = local.columns[j].Slot
		}
		roots := make([]ir.ScalarID, len(projections))
		for j := range projections {
			roots[j] = projections[j].id
		}
		outputColumns := make([]model.ColumnID, len(layer.view.Columns))
		for j := range layer.view.Columns {
			outputColumns[j] = layer.view.Columns[j].ID
		}
		step := ir.ViewMutationStepSpec{ViewMutationStep: ir.ViewMutationStep{View: layer.descriptor.ID(), Predicate: predicate.id, Check: checks[i]}, Inputs: inputs, Projections: roots, OutputColumns: outputColumns}
		steps = append(steps, step)
		effects = effects.Union(projectionEffects).Union(predicate.effects)
	}
	return steps, effects, nil
}

func (b *binder) viewMutationProjection(query *zpgparse.SelectStmt, s *scope) ([]expression, ir.Effects, error) {
	var projections []expression
	var effects ir.Effects
	for _, node := range query.TargetList {
		target := node.GetResTarget()
		if target == nil {
			return nil, effects, model.ErrInvalidDescriptor
		}
		if reference := starReference(target.Val); reference != nil {
			qualifier, err := starQualifier(reference, true)
			if err != nil {
				return nil, effects, err
			}
			owner := s
			candidates := s.columns
			if qualifier.name != "" {
				owner, err = s.starScope(qualifier, int(reference.Location)+1)
				if err != nil {
					return nil, effects, err
				}
				candidates = owner.columns
			}
			for _, candidate := range candidates {
				if !candidate.visibleToStar(qualifier) {
					continue
				}
				value, err := b.reference(s, owner.starColumn(s, candidate), int(reference.Location)+1)
				if err != nil {
					return nil, effects, err
				}
				projections = append(projections, value)
				effects = effects.Union(value.effects)
			}
			continue
		}
		expanded, _, star, err := b.expandCompositeStar(target.Val, s)
		if err != nil {
			return nil, effects, err
		}
		if star {
			for _, value := range expanded {
				value, err = b.defaultType(s, value)
				if err != nil {
					return nil, effects, err
				}
				projections = append(projections, value)
				effects = effects.Union(value.effects)
			}
			continue
		}
		value, err := b.targetScalar(target.Val, s)
		if err == nil {
			value, err = b.defaultType(s, value)
		}
		if err != nil {
			return nil, effects, err
		}
		projections = append(projections, value)
		effects = effects.Union(value.effects)
	}
	return projections, effects, nil
}

