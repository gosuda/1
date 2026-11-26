package ir

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (g *Graph) validateAggregate(ctx context.Context, n Rel, limits Limits) error {
	if n.groups.count == 0 {
		return invalid("aggregate needs explicit grouping sets, including the empty set")
	}
	for _, set := range sliceAt(g.groups, n.groups) {
		if !validExtent(set, g.indices) {
			return invalid("invalid grouping-set range")
		}
		for i, index := range sliceAt(g.indices, set) {
			if index >= n.expressions.count || contains(sliceAt(g.indices, set)[:i], index) {
				return invalid("invalid grouping-set expression index")
			}
		}
	}
	calls := sliceAt(g.aggregates, n.aggregates)
	for i := range calls {
		if err := g.validateAggregateCall(ctx, &n, &calls[i], calls[:i], limits); err != nil {
			return err
		}
	}
	groupings := sliceAt(g.groupings, n.groupings)
	for i, call := range groupings {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := g.validateGroupingCall(&n, call, groupings[:i]); err != nil {
			return err
		}
	}
	for _, slot := range sliceAt(g.slotRefs, n.outputs) {
		if err := g.validateAggregateOutput(&n, slot, calls); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graph) validateAggregateCall(ctx context.Context, n *Rel, call *AggregateCall, previous []AggregateCall, limits Limits) error {
	if call.Function == 0 || call.Effects.Volatility > Volatile || !contains(sliceAt(g.slotRefs, n.outputs), call.Result) || !validExtent(call.direct, g.scalarRefs) || !validExtent(call.args, g.scalarRefs) {
		return invalid("invalid aggregate call")
	}
	result := g.slots[call.Result-1]
	if err := g.validateSplitInvocation(call.Signature, call.direct, call.args, result.Layout, result.Nullable, call.Effects); err != nil {
		return err
	}
	if signature := g.signatures[call.Signature-1]; signature.Function != call.Function || signature.VariadicArray {
		return invalid("aggregate identity differs from signature")
	}
	for _, prior := range previous {
		if prior.Result == call.Result {
			return invalid("duplicate aggregate result")
		}
	}
	for _, id := range sliceAt(g.scalarRefs, call.direct) {
		if err := g.aggregateDirectExpressionAvailable(ctx, *n, id, 0, limits); err != nil {
			return err
		}
		if call.Effects.Union(g.scalars[id-1].Effects) != call.Effects {
			return invalid("aggregate effects omit direct argument effects")
		}
	}
	for _, id := range sliceAt(g.scalarRefs, call.args) {
		if err := g.expressionAvailable(ctx, *n, id, 0, limits); err != nil {
			return err
		}
		if call.Effects.Union(g.scalars[id-1].Effects) != call.Effects {
			return invalid("aggregate effects omit argument effects")
		}
	}
	if call.Filter != 0 {
		if err := g.expressionAvailable(ctx, *n, call.Filter, 0, limits); err != nil {
			return err
		}
		if !g.boolean(call.Filter) {
			return invalid("aggregate FILTER is not boolean")
		}
	}
	if err := g.validateOrder(ctx, *n, call.order, limits); err != nil {
		return err
	}
	if call.Filter != 0 && call.Effects.Union(g.scalars[call.Filter-1].Effects) != call.Effects {
		return invalid("aggregate effects omit FILTER effects")
	}
	for _, key := range sliceAt(g.orders, call.order) {
		if call.Effects.Union(g.scalars[key.Expr-1].Effects) != call.Effects {
			return invalid("aggregate effects omit ordering effects")
		}
	}
	if call.Distinct {
		for _, key := range sliceAt(g.orders, call.order) {
			if !contains(sliceAt(g.scalarRefs, call.args), key.Expr) {
				return invalid("DISTINCT aggregate order key is not an argument")
			}
		}
	}
	return nil
}
func (g *Graph) aggregateDirectExpressionAvailable(ctx context.Context, n Rel, id ScalarID, depth uint32, limits Limits) error {
	remaining := uint64(limits.MaxNodes) + uint64(limits.MaxOperands)
	var visit func(ScalarID, uint32) error
	available := func(slot model.SlotID) bool {
		outputs := sliceAt(g.slotRefs, n.outputs)
		groups := min(int(n.expressions.count), len(outputs))
		return contains(outputs[:groups], slot) || contains(sliceAt(g.slotRefs, g.scopes[n.Scope-1].captures), slot)
	}
	visit = func(current ScalarID, level uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if level >= limits.MaxDepth || remaining == 0 {
			return limitError()
		}
		remaining--
		if !validID(current, g.scalars) || g.scalars[current-1].Scope != n.Scope {
			return invalid("invalid aggregate direct argument scope or handle")
		}
		scalar := g.scalars[current-1]
		if scalar.Kind == Slot && !available(scalar.Slot) {
			return invalid("aggregate direct argument reads an ungrouped slot")
		}
		if scalar.Kind == Subquery {
			for _, slot := range sliceAt(g.slotRefs, g.scopes[g.rels[scalar.Relation-1].Scope-1].captures) {
				if !available(slot) {
					return invalid("aggregate direct subquery captures an ungrouped slot")
				}
			}
		}
		for _, child := range sliceAt(g.scalarRefs, scalar.operands) {
			if err := visit(child, level+1); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(id, depth)
}

func (g *Graph) validateGroupingCall(n *Rel, call GroupingCall, previous []GroupingCall) error {
	if !validExtent(call.args, g.indices) || call.args.count == 0 || call.args.count > 31 || !contains(sliceAt(g.slotRefs, n.outputs), call.Result) {
		return invalid("invalid GROUPING call")
	}
	result := g.slots[call.Result-1]
	typ := g.layouts[result.Layout-1].typ
	if result.Scope != n.Scope || result.Nullable || typ.ID != 23 || typ.Storage != model.TypeInt64 {
		return invalid("GROUPING result must be nonnullable int4 in aggregate scope")
	}
	for _, index := range sliceAt(g.indices, call.args) {
		if index >= n.expressions.count {
			return invalid("invalid GROUPING expression index")
		}
	}
	for _, prior := range previous {
		if prior.Result == call.Result {
			return invalid("duplicate GROUPING result")
		}
	}
	for _, aggregate := range sliceAt(g.aggregates, n.aggregates) {
		if aggregate.Result == call.Result {
			return invalid("GROUPING result collides with aggregate result")
		}
	}
	for _, binding := range sliceAt(g.bindings, n.bindings) {
		if binding.Slot == call.Result {
			return invalid("GROUPING result collides with grouping key")
		}
	}
	return nil
}

func (g *Graph) validateAggregateOutput(n *Rel, slot model.SlotID, calls []AggregateCall) error {
	found := false
	for _, call := range calls {
		if call.Result == slot {
			found = true
		}
	}
	for _, call := range sliceAt(g.groupings, n.groupings) {
		if call.Result == slot {
			found = true
		}
	}
	for _, binding := range sliceAt(g.bindings, n.bindings) {
		if binding.Slot != slot {
			continue
		}
		if found || !contains(sliceAt(g.scalarRefs, n.expressions), binding.Expr) {
			return invalid("aggregate output is neither grouping key nor aggregate result")
		}
		found = true
		if err := g.validateGroupingOutput(n, binding); err != nil {
			return err
		}
	}
	if !found {
		return invalid("unbound aggregate output")
	}
	return nil
}

func (g *Graph) validateGroupingOutput(n *Rel, binding Binding) error {
	for _, set := range sliceAt(g.groups, n.groups) {
		included := false
		for _, index := range sliceAt(g.indices, set) {
			if g.scalarRefs[n.expressions.start+index] == binding.Expr {
				included = true
			}
		}
		if !included && !g.slots[binding.Slot-1].Nullable {
			return invalid("grouping-set omitted key must be nullable")
		}
	}
	return nil
}
func (g *Graph) validateWindow(ctx context.Context, n Rel, limits Limits) error {
	if n.windows.count == 0 || n.Frame.Mode < RowsFrame || n.Frame.Mode > GroupsFrame || n.Frame.Exclusion > ExcludeTies {
		return invalid("invalid window frame")
	}
	start, end := n.Frame.Start, n.Frame.End
	if start.Kind < UnboundedPreceding || start.Kind > OffsetFollowing || end.Kind < OffsetPreceding || end.Kind > UnboundedFollowing || start.Kind > end.Kind {
		return invalid("invalid window bounds")
	}
	for _, bound := range [...]FrameBound{start, end} {
		offset := bound.Kind == OffsetPreceding || bound.Kind == OffsetFollowing
		if offset != (bound.Offset != 0) {
			return invalid("window bound offset mismatch")
		}
		if !offset || n.Frame.Mode != RangeFrame {
			if bound.InRange != 0 {
				return invalid("non-RANGE window bound has in_range support")
			}
			if offset && g.layouts[g.scalars[bound.Offset-1].Layout-1].typ.Storage != model.TypeInt64 {
				return invalid("ROWS/GROUPS offset must be integral")
			}
			continue
		}
		if n.order.count != 1 || !validID(bound.InRange, g.signatures) {
			return invalid("RANGE offset needs one ordering key and in_range support")
		}
		signature := g.signatures[bound.InRange-1]
		parameters := sliceAt(g.layoutRefs, signature.parameters)
		order := g.orders[n.order.start]
		orderLayout := g.scalars[order.Expr-1].Layout
		offsetLayout := g.scalars[bound.Offset-1].Layout
		validSignature := signature.Function != 0 && signature.Operator == 0 && signature.Strict && !signature.SetReturning && !signature.VariadicArray && len(parameters) == 5
		if !validSignature || !g.sameLayout(parameters[0], orderLayout) || !g.sameLayout(parameters[1], orderLayout) || !g.sameLayout(parameters[2], offsetLayout) {
			return invalid("RANGE in_range support has incompatible input layouts")
		}
		for _, parameter := range parameters[3:] {
			typ := g.layouts[parameter-1].typ
			if typ.ID != 16 || typ.Storage != model.TypeBoolean {
				return invalid("RANGE in_range flags are not boolean")
			}
		}
		result := g.layouts[signature.Result-1].typ
		if result.ID != 16 || result.Storage != model.TypeBoolean || signature.InputCollation != order.Collation {
			return invalid("RANGE in_range result or collation is invalid")
		}
	}
	if n.Frame.Mode == GroupsFrame && n.order.count == 0 {
		return invalid("GROUPS frame needs ordering")
	}
	outputs := sliceAt(g.slotRefs, n.outputs)
	calls := sliceAt(g.windows, n.windows)
	for i, call := range calls {
		if call.Function == 0 || call.Effects.Volatility > Volatile || !contains(outputs, call.Result) || !validExtent(call.args, g.scalarRefs) {
			return invalid("invalid window call")
		}
		result := g.slots[call.Result-1]
		if err := g.validateInvocation(call.Signature, call.args, result.Layout, result.Nullable, call.Effects); err != nil {
			return err
		}
		if signature := g.signatures[call.Signature-1]; signature.Function != call.Function || signature.VariadicArray {
			return invalid("window identity differs from signature")
		}
		for _, previous := range calls[:i] {
			if previous.Result == call.Result {
				return invalid("duplicate window result")
			}
		}
		if call.Filter != 0 {
			if err := g.expressionAvailable(ctx, n, call.Filter, 0, limits); err != nil {
				return err
			}
			if !g.boolean(call.Filter) {
				return invalid("window FILTER is not boolean")
			}
			if call.Effects.Union(g.scalars[call.Filter-1].Effects) != call.Effects {
				return invalid("window effects omit filter effects")
			}
		}
		for _, id := range sliceAt(g.scalarRefs, call.args) {
			if err := g.expressionAvailable(ctx, n, id, 0, limits); err != nil {
				return err
			}
			if call.Effects.Union(g.scalars[id-1].Effects) != call.Effects {
				return invalid("window effects omit argument effects")
			}
		}
	}
	for _, slot := range outputs {
		found := g.inputHas(n, slot)
		for _, call := range calls {
			if call.Result == slot {
				if found {
					return invalid("window overwrites input slot")
				}
				found = true
			}
		}
		for _, binding := range sliceAt(g.bindings, n.bindings) {
			if binding.Slot == slot {
				if found {
					return invalid("duplicate window output binding")
				}
				found = true
			}
		}
		if !found {
			return invalid("unbound window output")
		}
	}
	return nil
}
func (g *Graph) validateRecursion(ctx context.Context, n Rel, limits Limits) error {
	if !validID(n.CTE, g.ctes) || !g.ctes[n.CTE-1].Recursive || n.RecursiveReference {
		return invalid("recursive union needs recursive CTE declaration")
	}
	r := n.Recursion
	outputs := sliceAt(g.slotRefs, n.outputs)
	if !g.compatibleOutputs(g.ctes[n.CTE-1].outputs, n.outputs) {
		return invalid("recursive union differs from CTE schema")
	}
	for _, input := range sliceAt(g.relRefs, n.inputs) {
		slots := sliceAt(g.slotRefs, g.rels[input-1].outputs)
		index := 0
		for _, id := range outputs {
			if id == r.SearchSlot || id == r.CycleMark || id == r.CyclePath {
				continue
			}
			if index >= len(slots) {
				return invalid("recursive input width mismatch")
			}
			out, in := g.slots[id-1], g.slots[slots[index]-1]
			if !g.sameLayout(out.Layout, in.Layout) || in.Nullable && !out.Nullable {
				return invalid("recursive input type mismatch")
			}
			index++
		}
		if index != len(slots) {
			return invalid("recursive input contains generated columns")
		}
	}
	if r.Search > NoSearch {
		if r.Search > DepthFirst || r.searchKeys.count == 0 || !contains(outputs, r.SearchSlot) {
			return invalid("invalid recursive SEARCH metadata")
		}
		storage := g.layouts[g.slots[r.SearchSlot-1].Layout-1].typ.Storage
		invalidBreadthLayout := r.Search == BreadthFirst && storage != model.TypeComposite
		invalidDepthLayout := r.Search == DepthFirst && storage != model.TypeArray
		if invalidBreadthLayout || invalidDepthLayout {
			return invalid("SEARCH output layout mismatch")
		}
	} else if r.SearchSlot != 0 || r.searchKeys.count != 0 {
		return invalid("unexpected SEARCH payload")
	}
	for _, slot := range sliceAt(g.slotRefs, r.searchKeys) {
		if !contains(outputs, slot) || slot == r.SearchSlot || slot == r.CycleMark || slot == r.CyclePath {
			return invalid("invalid SEARCH key")
		}
	}
	if r.Search != NoSearch {
		if err := g.validateRecursiveKeyLayout(g.slots[r.SearchSlot-1].Layout, r.searchKeys, r.Search == BreadthFirst); err != nil {
			return err
		}
	}
	if r.cycleKeys.count == 0 {
		if r.CycleMark != 0 || r.CyclePath != 0 || r.CycleValue != 0 || r.CycleDefault != 0 || r.CycleContinue != 0 {
			return invalid("unexpected CYCLE payload")
		}
		return nil
	}
	missingOutputs := !contains(outputs, r.CycleMark) || !contains(outputs, r.CyclePath)
	aliasedOutputs := r.CycleMark == r.CyclePath || r.CycleMark == r.SearchSlot || r.CyclePath == r.SearchSlot
	missingMarks := r.CycleValue == 0 || r.CycleDefault == 0 || r.CycleContinue == 0
	if missingOutputs || aliasedOutputs || missingMarks {
		return invalid("invalid CYCLE output metadata")
	}
	if g.layouts[g.slots[r.CyclePath-1].Layout-1].typ.Storage != model.TypeArray {
		return invalid("CYCLE path must be an array")
	}
	for _, slot := range sliceAt(g.slotRefs, r.cycleKeys) {
		if !contains(outputs, slot) || slot == r.CycleMark || slot == r.CyclePath || slot == r.SearchSlot {
			return invalid("invalid CYCLE key")
		}
	}
	if err := g.validateRecursiveKeyLayout(g.slots[r.CyclePath-1].Layout, r.cycleKeys, false); err != nil {
		return err
	}
	mark := g.slots[r.CycleMark-1]
	if !g.sameLayout(mark.Layout, g.scalars[r.CycleValue-1].Layout) || !g.sameLayout(mark.Layout, g.scalars[r.CycleDefault-1].Layout) {
		return invalid("CYCLE mark type mismatch")
	}
	if !g.boolean(r.CycleContinue) {
		return invalid("CYCLE continuation must be boolean")
	}
	if (g.scalars[r.CycleValue-1].Nullable || g.scalars[r.CycleDefault-1].Nullable) && !mark.Nullable {
		return invalid("nullable CYCLE constant requires nullable mark slot")
	}
	remaining := uint64(limits.MaxNodes) + uint64(limits.MaxOperands)
	for _, id := range [...]ScalarID{r.CycleValue, r.CycleDefault, r.CycleContinue} {
		if err := g.validateRecursiveConstant(ctx, id, 0, limits, &remaining); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graph) validateRecursiveConstant(ctx context.Context, id ScalarID, depth uint32, limits Limits, remaining *uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if depth >= limits.MaxDepth || *remaining == 0 {
		return limitError()
	}
	*remaining--
	scalar := g.scalars[id-1]
	if scalar.Effects.Volatility != Immutable {
		return invalid("CYCLE values must be immutable")
	}
	switch scalar.Kind {
	case Slot, Subquery, Once, ArrayElement:
		return invalid("CYCLE values must be constant expressions")
	}
	for _, child := range sliceAt(g.scalarRefs, scalar.operands) {
		if err := g.validateRecursiveConstant(ctx, child, depth+1, limits, remaining); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graph) validateRecursiveKeyLayout(id LayoutID, keys extent, breadth bool) error {
	l := g.layouts[id-1]
	if !breadth {
		if l.typ.Storage != model.TypeArray {
			return invalid("recursive path is not an array")
		}
		l = g.layouts[g.layoutRefs[l.children.start]-1]
	}
	depthFields := uint32(0)
	if breadth {
		depthFields = 1
	}
	if l.typ.Storage != model.TypeComposite || uint64(l.children.count) != uint64(keys.count)+uint64(depthFields) {
		return invalid("recursive record width differs from key list")
	}
	if breadth && g.layouts[g.layoutRefs[l.children.start]-1].typ.Storage != model.TypeInt64 {
		return invalid("breadth-first depth must be integral")
	}
	for i, slot := range sliceAt(g.slotRefs, keys) {
		if !g.sameLayout(g.layoutRefs[uint64(l.children.start)+uint64(depthFields)+uint64(i)], g.slots[slot-1].Layout) {
			return invalid("recursive record field differs from key type")
		}
	}
	return nil
}
func (g *Graph) validateModify(ctx context.Context, n Rel, limits Limits) error {
	baseIdentity := n.Object != 0 && n.DescriptorVersion != 0
	physicalIdentity := n.Generation != 0 && n.EncodingVersion != 0
	validTarget := n.ModifyTarget == PhysicalModifyTarget && baseIdentity && physicalIdentity ||
		n.ModifyTarget == LogicalViewModifyTarget && baseIdentity && n.Generation == 0 && n.EncodingVersion == 0 ||
		n.ModifyTarget == AutomaticViewModifyTarget && baseIdentity && physicalIdentity && n.LogicalView != 0 && n.viewMutationSteps.count != 0 ||
		n.ModifyTarget == AutomaticViewTriggerModifyTarget && baseIdentity && n.Generation == 0 && n.EncodingVersion == 0 && n.LogicalView != 0 && n.viewMutationSteps.count != 0
	invalidKind := n.ModifyKind < Insert || n.ModifyKind > Merge
	if !validTarget || invalidKind || !g.validConflictAction(&n) {
		return invalid("invalid modification target or conflict action")
	}
	logicalConflict := n.Conflict != NoConflict && (n.Conflict != ConflictNothing || n.conflictArbiters.count != 0 || n.conflictAssignments.count != 0)
	if (n.ModifyTarget == LogicalViewModifyTarget || n.ModifyTarget == AutomaticViewTriggerModifyTarget) && logicalConflict {
		return invalid("logical view modification has unsupported conflict metadata")
	}
	if n.ModifyTarget != AutomaticViewModifyTarget && n.ModifyTarget != AutomaticViewTriggerModifyTarget && (n.LogicalView != 0 || n.viewMutationSteps.count != 0) {
		return invalid("view mutation metadata on another target")
	}
	nonAssigningKind := n.ModifyKind == Delete || n.ModifyKind == Merge
	unexpectedAssignments := nonAssigningKind && n.assignments.count != 0
	unexpectedConflictAssignments := n.Conflict != ConflictUpdate && n.conflictAssignments.count != 0
	unexpectedArbiters := n.Conflict == NoConflict && n.conflictArbiters.count != 0
	if unexpectedAssignments || unexpectedConflictAssignments || unexpectedArbiters {
		return invalid("unexpected write assignments or conflict arbiters")
	}
	canUpdate := n.ModifyKind == Update || n.ModifyKind == Merge || n.Conflict == ConflictUpdate
	if !canUpdate && n.updatedColumns.count != 0 {
		return invalid("updated columns on modification without an update path")
	}
	for i, column := range sliceAt(g.columnRefs, n.updatedColumns) {
		if column == 0 || contains(sliceAt(g.columnRefs, n.updatedColumns)[:i], column) {
			return invalid("invalid updated column")
		}
	}
	imageObject := n.Object
	if n.ModifyTarget == AutomaticViewModifyTarget || n.ModifyTarget == AutomaticViewTriggerModifyTarget {
		imageObject = n.LogicalView
	}
	for _, slot := range [...]model.SlotID{n.OldRow, n.NewRow, n.ExcludedRow, n.MergeResultRow} {
		if slot == 0 {
			continue
		}
		if !validID(slot, g.slots) || g.slots[slot-1].Scope != n.Scope || g.slots[slot-1].Object != imageObject || g.layouts[g.slots[slot-1].Layout-1].typ.Storage != model.TypeComposite {
			return invalid("invalid OLD/NEW row slot")
		}
	}
	for _, slot := range [...]model.SlotID{n.OldTableOID, n.NewTableOID, n.MergeTableOID} {
		if slot == 0 {
			continue
		}
		if !validID(slot, g.slots) || g.slots[slot-1].Scope != n.Scope || g.slots[slot-1].Object != imageObject || g.layouts[g.slots[slot-1].Layout-1].typ.ID != 26 {
			return invalid("invalid modification tableoid slot")
		}
	}
	if (n.OldTableOID == 0) != (n.NewTableOID == 0) || n.OldTableOID != 0 && (n.OldRow == 0 || n.NewRow == 0 || n.OldTableOID == n.NewTableOID || g.slots[n.OldTableOID-1].Nullable != g.slots[n.OldRow-1].Nullable || g.slots[n.NewTableOID-1].Nullable != g.slots[n.NewRow-1].Nullable) {
		return invalid("OLD and NEW tableoid slots must match their row images")
	}
	if n.SourceObject != 0 {
		if !validID(n.SourceObject, g.slots) || g.slots[n.SourceObject-1].Scope != n.Scope || g.layouts[g.slots[n.SourceObject-1].Layout-1].typ.Storage != model.TypeInt64 || n.ModifyKind == Insert {
			return invalid("invalid modification source object slot")
		}
	}
	if n.OldRow != 0 && n.OldRow == n.NewRow {
		return invalid("OLD and NEW need distinct slots")
	}
	if n.Conflict == ConflictUpdate {
		if n.OldRow == 0 || n.ExcludedRow == 0 {
			return invalid("conflict update requires target and excluded images")
		}
		reusedImage := n.ExcludedRow == n.OldRow || n.ExcludedRow == n.NewRow
		if reusedImage || g.slots[n.ExcludedRow-1].Nullable || !g.sameLayout(g.slots[n.OldRow-1].Layout, g.slots[n.ExcludedRow-1].Layout) {
			return invalid("conflict update requires distinct compatible target and excluded images")
		}
	} else if n.ExcludedRow != 0 {
		return invalid("excluded image without conflict update")
	}
	for i, arbiter := range sliceAt(g.conflictArbiters, n.conflictArbiters) {
		if arbiter.Primary != (arbiter.Index == 0) || arbiter.Version == 0 || arbiter.Generation == 0 {
			return invalid("invalid conflict arbiter identity")
		}
		for _, previous := range sliceAt(g.conflictArbiters, n.conflictArbiters)[:i] {
			if previous.Primary == arbiter.Primary && previous.Index == arbiter.Index {
				return invalid("duplicate conflict arbiter")
			}
		}
	}
	for i, step := range sliceAt(g.viewMutationSteps, n.viewMutationSteps) {
		if step.View == 0 || step.inputs.count == 0 || step.projections.count == 0 || step.projections.count != step.outputColumns.count || i+1 == int(n.viewMutationSteps.count) && step.View != n.LogicalView {
			return invalid("invalid automatic view mutation step")
		}
		for _, slot := range sliceAt(g.slotRefs, step.inputs) {
			if !validID(slot, g.slots) || g.slots[slot-1].Scope != n.Scope || g.slots[slot-1].Object == 0 || g.slots[slot-1].Column == 0 {
				return invalid("invalid automatic view input slot")
			}
		}
		if step.Predicate != 0 {
			if err := g.expressionAvailable(ctx, n, step.Predicate, 0, limits); err != nil {
				return err
			}
			if g.layouts[g.scalars[step.Predicate-1].Layout-1].typ.Storage != model.TypeBoolean {
				return invalid("view check predicate must be boolean")
			}
		} else if step.Check {
			return invalid("checked view step needs a predicate")
		}
		for _, projection := range sliceAt(g.scalarRefs, step.projections) {
			if err := g.expressionAvailable(ctx, n, projection, 0, limits); err != nil {
				return err
			}
		}
		for j, column := range sliceAt(g.columnRefs, step.outputColumns) {
			if column == 0 || contains(sliceAt(g.columnRefs, step.outputColumns)[:j], column) {
				return invalid("invalid automatic view output column")
			}
		}
	}
	source := n
	source.OldRow, source.NewRow, source.ExcludedRow, source.OldTableOID, source.NewTableOID, source.MergeTableOID = 0, 0, 0, 0, 0, 0
	if err := g.validateAssignments(ctx, source, n.assignments, limits); err != nil {
		return err
	}
	action := n
	action.inputs, action.NewRow, action.OldTableOID, action.NewTableOID, action.MergeTableOID = extent{}, 0, 0, 0, 0
	if err := g.validateAssignments(ctx, action, n.conflictAssignments, limits); err != nil {
		return err
	}
	if n.ConflictPredicate != 0 {
		if err := g.expressionAvailable(ctx, action, n.ConflictPredicate, 0, limits); err != nil {
			return err
		}
	}
	returning := n
	returning.ExcludedRow = 0
	for _, binding := range sliceAt(g.bindings, n.bindings) {
		if err := g.expressionAvailable(ctx, returning, binding.Expr, 0, limits); err != nil {
			return err
		}
	}
	if n.bindings.count != n.outputs.count {
		return invalid("RETURNING must bind every output")
	}
	if (n.ModifyKind == Merge) != (n.actions.count != 0) {
		return invalid("MERGE needs typed actions")
	}
	if n.ModifyKind == Merge {
		if n.OldRow == 0 || n.NewRow == 0 || n.MergeResultRow == 0 || n.MergeActionSlot == 0 || n.OldTableOID == 0 || n.NewTableOID == 0 || n.MergeTableOID == 0 || n.targetKeys.count == 0 {
			return invalid("MERGE lacks runtime images, tableoid, or target identity")
		}
		if n.MergeResultRow == n.OldRow || n.MergeResultRow == n.NewRow || !g.slots[n.OldRow-1].Nullable || !g.slots[n.NewRow-1].Nullable {
			return invalid("MERGE requires distinct nullable OLD/NEW images")
		}
		if g.slots[n.MergeResultRow-1].Nullable || !g.sameLayout(g.slots[n.OldRow-1].Layout, g.slots[n.NewRow-1].Layout) || !g.sameLayout(g.slots[n.OldRow-1].Layout, g.slots[n.MergeResultRow-1].Layout) {
			return invalid("MERGE result images have incompatible layouts")
		}
		if n.MergeTableOID == n.OldTableOID || n.MergeTableOID == n.NewTableOID || g.slots[n.MergeTableOID-1].Nullable {
			return invalid("MERGE result tableoid must be distinct and non-null")
		}
		if !validID(n.MergeActionSlot, g.slots) || g.slots[n.MergeActionSlot-1].Scope != n.Scope || g.slots[n.MergeActionSlot-1].Nullable || g.layouts[g.slots[n.MergeActionSlot-1].Layout-1].typ.ID != 25 {
			return invalid("MERGE action slot must be non-null text")
		}
		if n.MergeSourcePresent == n.MergeTargetPresent {
			return invalid("MERGE presence slots must be distinct")
		}
		for _, slot := range [...]model.SlotID{n.MergeSourcePresent, n.MergeTargetPresent} {
			if !validID(slot, g.slots) || !contains(sliceAt(g.slotRefs, g.rels[g.relRefs[n.inputs.start]-1].outputs), slot) || g.layouts[g.slots[slot-1].Layout-1].typ.Storage != model.TypeBoolean {
				return invalid("MERGE presence slot must be an input boolean")
			}
		}
	} else if n.MergeSourcePresent != 0 || n.MergeTargetPresent != 0 || n.MergeResultRow != 0 || n.MergeActionSlot != 0 || n.MergeTableOID != 0 {
		return invalid("MERGE metadata on another modification")
	}
	for i, key := range sliceAt(g.slotRefs, n.targetKeys) {
		if !validID(key, g.slots) || !contains(sliceAt(g.slotRefs, g.rels[g.relRefs[n.inputs.start]-1].outputs), key) || contains(sliceAt(g.slotRefs, n.targetKeys)[:i], key) {
			return invalid("invalid original target key slot")
		}
	}
	candidate := n
	candidate.OldRow, candidate.NewRow, candidate.MergeResultRow, candidate.MergeActionSlot, candidate.OldTableOID, candidate.NewTableOID, candidate.MergeTableOID = 0, 0, 0, 0, 0, 0, 0
	var terminal [4]bool
	for _, action := range sliceAt(g.actions, n.actions) {
		if !g.validMergeAction(action) {
			return invalid("invalid MERGE action")
		}
		if terminal[action.Match] {
			return invalid("unreachable MERGE action")
		}
		terminal[action.Match] = action.Condition == 0
		if action.Condition != 0 {
			if err := g.expressionAvailable(ctx, candidate, action.Condition, 0, limits); err != nil {
				return err
			}
			if !g.boolean(action.Condition) {
				return invalid("MERGE condition must be boolean")
			}
		}
		if (action.Action == Delete || action.Action == DoNothing) && action.assignments.count != 0 {
			return invalid("non-assigning MERGE action has assignments")
		}
		if err := g.validateAssignments(ctx, candidate, action.assignments, limits); err != nil {
			return err
		}
	}
	return nil
}

func (g *Graph) validConflictAction(n *Rel) bool {
	if n.Conflict > ConflictUpdate {
		return false
	}
	if n.Conflict != NoConflict && n.ModifyKind != Insert {
		return false
	}
	if n.ConflictPredicate != 0 && (n.Conflict != ConflictUpdate || !g.boolean(n.ConflictPredicate)) {
		return false
	}
	return n.Conflict != ConflictUpdate || n.conflictArbiters.count != 0
}

func (g *Graph) validMergeAction(action MergeAction) bool {
	invalidMatch := action.Match < Matched || action.Match > NotMatchedBySource
	invalidAction := action.Action < Insert || action.Action > DoNothing || action.Action == Merge
	if invalidMatch || invalidAction || !validExtent(action.assignments, g.bindings) {
		return false
	}
	switch action.Match {
	case Matched, NotMatchedBySource:
		return action.Action != Insert
	case NotMatchedByTarget:
		return action.Action == Insert || action.Action == DoNothing
	default:
		return false
	}
}

func (g *Graph) validateAssignments(ctx context.Context, n Rel, r extent, limits Limits) error {
	assignments := sliceAt(g.bindings, r)
	for i, assignment := range assignments {
		if !validID(assignment.Slot, g.slots) || g.slots[assignment.Slot-1].Scope != n.Scope || g.slots[assignment.Slot-1].Object != n.Object || g.slots[assignment.Slot-1].Column == 0 {
			return invalid("assignment target is not a target column")
		}
		for _, previous := range assignments[:i] {
			if g.slots[previous.Slot-1].Column == g.slots[assignment.Slot-1].Column {
				return invalid("duplicate target column assignment")
			}
		}
		if err := g.expressionAvailable(ctx, n, assignment.Expr, 0, limits); err != nil {
			return err
		}
		if !g.sameLayout(g.slots[assignment.Slot-1].Layout, g.scalars[assignment.Expr-1].Layout) {
			return invalid("assignment type mismatch")
		}
	}
	return nil
}
