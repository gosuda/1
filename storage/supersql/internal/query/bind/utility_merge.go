package bind

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (b *binder) merge(n *zpgparse.MergeStmt, s *scope) (relation, error) {
	if n.WithClause != nil {
		if err := b.with(n.WithClause, s); err != nil {
			return relation{}, err
		}
	}
	target, err := b.modificationTarget(n.Relation)
	if err != nil {
		return relation{}, err
	}
	target, err = b.prepareMutationTarget(target, mergeMutationEvents(n.MergeWhenClauses))
	if err != nil {
		return relation{}, err
	}
	previousProvenance := b.mutationProvenance
	if target.automatic() {
		b.mutationProvenance = target.executionDescriptor.ID()
	}
	left, err := b.from(&zpgparse.Node{Node: &zpgparse.Node_RangeVar{RangeVar: n.Relation}}, s)
	b.mutationProvenance = previousProvenance
	if err != nil {
		return relation{}, err
	}
	right, err := b.from(n.SourceRelation, s)
	if err != nil {
		return relation{}, err
	}
	if err := disjointRelations(left, right); err != nil {
		return relation{}, err
	}
	left, err = b.mergePresence(left, s)
	if err != nil {
		return relation{}, err
	}
	right, err = b.mergePresence(right, s)
	if err != nil {
		return relation{}, err
	}
	local := *s
	local.columns = append(slices.Clone(left.columns), right.columns...)
	local.namespaces = append(slices.Clone(left.namespaces), right.namespaces...)
	predicate, err := b.scalar(n.JoinCondition, &local)
	if err != nil {
		return relation{}, err
	}
	predicate, err = b.coerce(&local, predicate, b.primitive(16), ir.ImplicitCast)
	if err != nil {
		return relation{}, err
	}
	columns := slices.Clone(local.columns)
	for i := range columns {
		if !columns[i].Nullable {
			columns[i].Slot, err = b.slot(s, columns[i].Layout, true, 0, 0)
			if err != nil {
				return relation{}, err
			}
			columns[i].Nullable = true
		}
	}
	effects := left.effects.Union(right.effects).Union(predicate.effects)
	join, err := b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: s.id, JoinKind: ir.FullJoin, Predicate: predicate.id, Effects: effects}, Inputs: []ir.RelID{left.id, right.id}, Outputs: outputSlots(columns)})
	if err != nil {
		return relation{}, err
	}
	namespaces, err := b.joinNamespaces(left, right, columns)
	if err != nil {
		return relation{}, err
	}
	s.columns, s.namespaces = columns, namespaces
	spec, output, err := b.returning(s, target, ir.Merge, n.ReturningClause)
	if err != nil {
		return relation{}, err
	}
	spec.MergeTargetPresent = columns[len(left.columns)-1].Slot
	spec.MergeSourcePresent = columns[len(columns)-1].Slot
	if target.logicalTrigger {
		for _, descriptor := range publicColumns(target.executionTable) {
			for i, candidate := range left.columns {
				if candidate.Name == descriptor.Name && target.qualifier.matches(candidate) {
					spec.TargetKeys = append(spec.TargetKeys, columns[i].Slot)
					break
				}
			}
		}
	} else {
		for _, key := range target.executionTable.PrimaryKey {
			for i, candidate := range left.columns {
				info, ok := b.builder.Slot(candidate.Slot)
				if ok && info.Object == target.executionDescriptor.ID() && info.Column == key.Column {
					spec.TargetKeys = append(spec.TargetKeys, columns[i].Slot)
					break
				}
			}
		}
	}
	var terminal [4]bool
	for _, node := range n.MergeWhenClauses {
		clause := node.GetMergeWhenClause()
		if clause == nil {
			return relation{}, bindError("42601", "invalid MERGE WHEN clause", 0)
		}
		var action ir.MergeActionSpec
		switch clause.MatchKind {
		case zpgparse.MergeMatchKind_MERGE_WHEN_MATCHED:
			action.Match = ir.Matched
		case zpgparse.MergeMatchKind_MERGE_WHEN_NOT_MATCHED_BY_TARGET:
			action.Match = ir.NotMatchedByTarget
		case zpgparse.MergeMatchKind_MERGE_WHEN_NOT_MATCHED_BY_SOURCE:
			action.Match = ir.NotMatchedBySource
		default:
			return relation{}, bindError("42601", "invalid MERGE match kind", 0)
		}
		if terminal[action.Match] {
			return relation{}, bindError("42601", "unreachable WHEN clause specified after unconditional WHEN clause", 0)
		}
		terminal[action.Match] = clause.Condition == nil
		actionScope := *s
		if action.Match == ir.NotMatchedByTarget {
			actionScope.columns = columns[len(left.columns):]
			actionScope.namespaces = namespaces[len(left.namespaces):]
		} else if action.Match == ir.NotMatchedBySource {
			actionScope.columns = columns[:len(left.columns)]
			actionScope.namespaces = namespaces[:len(left.namespaces)]
		}
		if clause.Condition != nil {
			condition, err := b.scalar(clause.Condition, &actionScope)
			if err != nil {
				return relation{}, err
			}
			condition, err = b.coerce(&actionScope, condition, b.primitive(16), ir.ImplicitCast)
			if err != nil {
				return relation{}, err
			}
			action.Condition = condition.id
			effects = effects.Union(condition.effects)
		}
		var actionEffects ir.Effects
		var updated []model.ColumnID
		switch clause.CommandType {
		case zpgparse.CmdType_CMD_UPDATE:
			action.Action = ir.Update
			action.Assignments, updated, actionEffects, err = b.updateAssignments(clause.TargetList, &actionScope, target)
		case zpgparse.CmdType_CMD_INSERT:
			action.Action = ir.Insert
			action.Assignments, actionEffects, err = b.mergeInsertAssignments(clause, &actionScope, target)
		case zpgparse.CmdType_CMD_DELETE:
			action.Action = ir.Delete
		case zpgparse.CmdType_CMD_NOTHING:
			action.Action = ir.DoNothing
		default:
			return relation{}, bindError("42601", "invalid MERGE action", 0)
		}
		if err != nil {
			return relation{}, err
		}
		effects = effects.Union(actionEffects)
		for _, column := range updated {
			if !slices.Contains(spec.UpdatedColumns, column) {
				spec.UpdatedColumns = append(spec.UpdatedColumns, column)
			}
		}
		spec.Actions = append(spec.Actions, action)
	}
	d := target.executionDescriptor
	viewSteps, viewEffects, err := b.bindViewMutationSteps(s, target)
	if err != nil {
		return relation{}, err
	}
	spec.Kind, spec.Scope, spec.ModifyKind, spec.ModifyTarget = ir.Modify, s.id, ir.Merge, ir.PhysicalModifyTarget
	spec.Object, spec.DescriptorVersion = d.ID(), d.Version()
	spec.Generation, spec.EncodingVersion = d.Physical().Generation, d.Physical().EncodingVersion
	if target.logicalTrigger {
		spec.ModifyTarget = ir.LogicalViewModifyTarget
		spec.Generation, spec.EncodingVersion = 0, 0
	}
	if target.automatic() {
		spec.LogicalView = target.descriptor.ID()
		spec.ViewMutationSteps = viewSteps
		spec.ModifyTarget = ir.AutomaticViewModifyTarget
		if target.logicalTrigger {
			spec.ModifyTarget = ir.AutomaticViewTriggerModifyTarget
		}
	}
	spec.Effects = effects.Union(spec.Effects).Union(viewEffects).Union(ir.Effects{Volatility: ir.Volatile, MayError: true})
	spec.Inputs = []ir.RelID{join}
	id, err := b.builder.AddRel(b.ctx, spec)
	return relation{id: id, columns: output, effects: spec.Effects}, err
}

func (b *binder) mergePresence(rel relation, s *scope) (relation, error) {
	value, err := b.addScalar(s, expression{typ: b.primitive(16), value: model.BoolVal(true)}, ir.Scalar{Kind: ir.Const}, nil)
	if err != nil {
		return relation{}, err
	}
	slot, err := b.slot(s, value.typ, false, 0, 0)
	if err != nil {
		return relation{}, err
	}
	bindings := make([]ir.Binding, 0, len(rel.columns)+1)
	for _, c := range rel.columns {
		e, err := b.reference(s, c, 0)
		if err != nil {
			return relation{}, err
		}
		bindings = append(bindings, ir.Binding{Slot: c.Slot, Expr: e.id})
	}
	bindings = append(bindings, ir.Binding{Slot: slot, Expr: value.id})
	rel.columns = append(slices.Clone(rel.columns), column{ResultColumn: ResultColumn{Slot: slot, Layout: value.typ}, internal: true, hidden: true})
	rel.id, err = b.builder.AddRel(b.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: s.id, Effects: rel.effects}, Inputs: []ir.RelID{rel.id}, Outputs: outputSlots(rel.columns), Bindings: bindings})
	return rel, err
}

func (b *binder) mergeInsertAssignments(clause *zpgparse.MergeWhenClause, s *scope, target mutationTarget) ([]ir.Binding, ir.Effects, error) {
	var effects ir.Effects
	columns := publicColumns(target.table)
	targets := columns
	paths := make([][]*zpgparse.Node, len(targets))
	if len(clause.TargetList) != 0 {
		targets = nil
		paths = nil
		var seen []*zpgparse.ResTarget
		for _, node := range clause.TargetList {
			current := node.GetResTarget()
			if current == nil {
				return nil, effects, bindError("42601", "invalid INSERT column", 0)
			}
			duplicate := slices.ContainsFunc(seen, func(previous *zpgparse.ResTarget) bool {
				if previous.Name != current.Name || len(previous.Indirection) != len(current.Indirection) {
					return false
				}
				for i := range previous.Indirection {
					if !equivalentAST(previous.Indirection[i].ProtoReflect(), current.Indirection[i].ProtoReflect()) {
						return false
					}
				}
				return true
			})
			if duplicate {
				return nil, effects, bindError("42701", "column specified more than once", int(current.Location)+1)
			}
			seen = append(seen, current)
			index := slices.IndexFunc(columns, func(column model.ColumnDescriptor) bool { return column.Name == current.Name })
			if index < 0 {
				return nil, effects, bindError("42703", "INSERT column does not exist", int(current.Location)+1)
			}
			targets = append(targets, columns[index])
			paths = append(paths, current.Indirection)
		}
	}
	if len(clause.Values) > len(targets) {
		return nil, effects, bindError("42601", "INSERT has more expressions than target columns", 0)
	}
	if len(clause.TargetList) != 0 && len(clause.Values) < len(targets) {
		return nil, effects, bindError("42601", "INSERT has more target columns than expressions", 0)
	}
	var assignments []ir.Binding
	owners, err := target.automaticInsertOwners(columns, func(column model.ColumnID) bool {
		for i, supplied := range targets {
			if i < len(clause.Values) && supplied.ID == column {
				return true
			}
		}
		return false
	})
	if err != nil {
		return nil, effects, err
	}
	for _, column := range columns {
		if target.automatic() {
			executionColumn, writable := target.executionColumn(column.ID)
			if !writable {
				if slices.ContainsFunc(targets[:min(len(targets), len(clause.Values))], func(supplied model.ColumnDescriptor) bool { return supplied.ID == column.ID }) {
					b.deferWriteError(bindError("0A000", "cannot insert into a non-updatable view column", 0))
				}
				continue
			}
			if owners[executionColumn.ID] != column.ID {
				continue
			}
			column = target.assignmentColumn(column)
		}
		matches := make([]int, 0, 1)
		direct := -1
		for i, supplied := range targets {
			if supplied.ID != column.ID || i >= len(clause.Values) {
				continue
			}
			matches = append(matches, i)
			if len(paths[i]) == 0 {
				if direct >= 0 || len(matches) > 1 {
					return nil, effects, bindError("42701", "column specified more than once", 0)
				}
				direct = i
			} else if direct >= 0 {
				return nil, effects, bindError("42701", "column specified more than once", 0)
			}
		}
		useDefault := len(matches) == 0
		var value expression
		var err error
		if direct >= 0 {
			node := clause.Values[direct]
			useDefault = node == nil || node.GetSetToDefault() != nil
			if !useDefault {
				value, err = b.columnAssignment(node, s, column)
			}
		} else if len(matches) != 0 {
			value, err = b.columnDefault(column, s)
			if err == nil {
				value, err = b.coerceColumnAssignment(value, s, column)
			}
			for _, match := range matches {
				node := clause.Values[match]
				if node == nil || node.GetSetToDefault() != nil {
					return nil, effects, bindError("42601", "DEFAULT is not allowed for a column subfield", 0)
				}
				leaf := column
				position := int(clause.TargetList[match].GetResTarget().Location) + 1
				leaf.Layout, err = b.assignmentTargetLayout(column.Layout, paths[match], position)
				var replacement expression
				if err == nil {
					replacement, err = b.columnAssignment(node, s, leaf)
				}
				if err == nil {
					value, err = b.storeAssignment(s, value, paths[match], replacement, position)
				}
				if err != nil {
					break
				}
			}
		}
		if err != nil {
			return nil, effects, err
		}
		if !useDefault && column.Generated != nil {
			b.deferWriteError(bindError("428C9", "cannot insert a non-DEFAULT value into generated column", 0))
		}
		if !useDefault && column.IdentityAlways && clause.Override != zpgparse.OverridingKind_OVERRIDING_SYSTEM_VALUE && clause.Override != zpgparse.OverridingKind_OVERRIDING_USER_VALUE {
			b.deferWriteError(bindError("428C9", "cannot insert a non-DEFAULT value into identity column", 0))
		}
		if column.Generated != nil {
			continue
		}
		useDefault = useDefault || column.IdentitySequence != 0 && clause.Override == zpgparse.OverridingKind_OVERRIDING_USER_VALUE
		if useDefault {
			if !target.automatic() && !slices.ContainsFunc(target.table.PrimaryKey, func(key model.KeyElement) bool { return key.Column == column.ID }) {
				continue
			}
			value, err = b.columnDefault(column, s)
			if err == nil {
				value, err = b.coerceColumnAssignment(value, s, column)
			}
		}
		if err != nil {
			return nil, effects, err
		}
		destinationDescriptor, destinationColumn := target.descriptor, column.ID
		if target.automatic() {
			executionColumn, _ := target.executionColumn(column.ID)
			destinationDescriptor, destinationColumn = target.executionDescriptor, executionColumn.ID
		}
		slot, err := b.slot(s, column.Layout, column.Nullable, destinationDescriptor.ID(), destinationColumn)
		if err != nil {
			return nil, effects, err
		}
		assignments = append(assignments, ir.Binding{Slot: slot, Expr: value.id})
		effects = effects.Union(value.effects)
		b.dependency(target.descriptor, column.ID)
		b.dependency(destinationDescriptor, destinationColumn)
	}
	if target.automatic() {
		for _, column := range publicColumns(target.executionTable) {
			assigned := slices.ContainsFunc(assignments, func(binding ir.Binding) bool {
				info, ok := b.builder.Slot(binding.Slot)
				return ok && info.Object == target.executionDescriptor.ID() && info.Column == column.ID
			})
			if assigned || column.Generated != nil {
				continue
			}
			value, err := b.columnDefault(column, s)
			if err == nil {
				value, err = b.coerceColumnAssignment(value, s, column)
			}
			if err != nil {
				return nil, effects, err
			}
			slot, err := b.slot(s, column.Layout, column.Nullable, target.executionDescriptor.ID(), column.ID)
			if err != nil {
				return nil, effects, err
			}
			assignments = append(assignments, ir.Binding{Slot: slot, Expr: value.id})
			effects = effects.Union(value.effects)
			b.dependency(target.executionDescriptor, column.ID)
		}
	}
	return assignments, effects, nil
}

func mergeMutationEvents(nodes []*zpgparse.Node) []string {
	var events []string
	for _, node := range nodes {
		clause := node.GetMergeWhenClause()
		if clause == nil {
			continue
		}
		event := ""
		switch clause.CommandType {
		case zpgparse.CmdType_CMD_INSERT:
			event = "insert"
		case zpgparse.CmdType_CMD_UPDATE:
			event = "update"
		case zpgparse.CmdType_CMD_DELETE:
			event = "delete"
		}
		if event != "" && !slices.Contains(events, event) {
			events = append(events, event)
		}
	}
	return events
}
