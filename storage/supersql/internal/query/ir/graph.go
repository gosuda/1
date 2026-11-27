package ir

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func (g *Graph) Root() RelID { return g.root }

// OwnedBytes is the retained arena/constant charge enforced by Limits.MaxBytes.
// It excludes the fixed Graph header and allocator bookkeeping.
func (g *Graph) OwnedBytes() uint64 { return g.bytes }

func (g *Graph) ScalarCount() uint32 { return uint32(len(g.scalars)) }
func (g *Graph) RelCount() uint32    { return uint32(len(g.rels)) }
func (g *Graph) SlotCount() uint32   { return uint32(len(g.slots)) }
func (g *Graph) LayoutCount() uint32 { return uint32(len(g.layouts)) }
func (g *Graph) ScopeCount() uint32  { return uint32(len(g.scopes)) }
func (g *Graph) CTECount() uint32    { return uint32(len(g.ctes)) }
func atID[I ~uint32, T any](values []T, id I) (T, bool) {
	if !validID(id, values) {
		var zero T
		return zero, false
	}
	return values[id-1], true
}
func atRange[T any](values []T, r extent, index uint32) (T, bool) {
	if index >= r.count || !validExtent(r, values) {
		var zero T
		return zero, false
	}
	return values[uint64(r.start)+uint64(index)], true
}
func (g *Graph) Scalar(id ScalarID) (Scalar, bool)     { return atID(g.scalars, id) }
func (g *Graph) Rel(id RelID) (Rel, bool)              { return atID(g.rels, id) }
func (g *Graph) Slot(id model.SlotID) (SlotInfo, bool) { return atID(g.slots, id) }
func (g *Graph) CTE(id CTEID) (CTE, bool)              { return atID(g.ctes, id) }
func (g *Graph) Type(id LayoutID) (model.Type, bool)   { n, ok := atID(g.layouts, id); return n.typ, ok }

// Builder scalar accessors expose immutable copies already appended to the
// arena. They support binder-owned rewrites without transferring arena slices.
func (b *Builder) Scalar(id ScalarID) (Scalar, bool) {
	return b.graph.Scalar(id)
}
func (b *Builder) Slot(id model.SlotID) (SlotInfo, bool) {
	return b.graph.Slot(id)
}

func (b *Builder) Operand(id ScalarID, index uint32) (ScalarID, bool) {
	return b.graph.Operand(id, index)
}
func (b *Builder) Comparison(id ScalarID, index uint32) (SignatureID, bool) {
	return b.graph.Comparison(id, index)
}


func (b *Builder) Signature(id SignatureID) (Signature, bool) {
	return atID(b.graph.signatures, id)
}

func (b *Builder) Type(id LayoutID) (model.Type, bool) {
	return b.graph.Type(id)
}

// Layout returns the complete physical layout, including nested children, so a
// binder-owned rewrite can resolve a function signature against the exact bound
// argument layouts without transferring arena slices. Depth is bounded by the
// IR's own layout limit, so the recursion terminates.
func (b *Builder) Layout(id LayoutID) (model.TypeLayout, bool) {
	return b.layout(id, 0)
}

func (b *Builder) layout(id LayoutID, depth uint32) (model.TypeLayout, bool) {
	n, ok := atID(b.graph.layouts, id)
	if !ok || depth > 64 {
		return model.TypeLayout{}, false
	}
	children := make([]model.TypeLayout, n.children.count)
	for i := range children {
		child, childOK := atRange[LayoutID](b.graph.layoutRefs, n.children, uint32(i))
		if !childOK {
			return model.TypeLayout{}, false
		}
		resolved, resolvedOK := b.layout(child, depth+1)
		if !resolvedOK {
			return model.TypeLayout{}, false
		}
		children[i] = resolved
	}
	return model.TypeLayout{Type: n.typ, Children: children}, true
}

// CloneConstant copies a constant leaf's value within maxBytes.
func (b *Builder) CloneConstant(ctx context.Context, id ScalarID, maxBytes uint64) (model.Value, error) {
	return b.graph.CloneConstant(ctx, id, maxBytes)
}

func (b *Builder) LayoutChildCount(id LayoutID) uint32 {
	return b.graph.LayoutChildCount(id)
}

func (b *Builder) LayoutChild(id LayoutID, index uint32) (LayoutID, bool) {
	return b.graph.LayoutChild(id, index)
}

func (b *Builder) LayoutFieldNameCount(id LayoutID) uint32 {
	return b.graph.LayoutFieldNameCount(id)
}

func (b *Builder) LayoutFieldName(id LayoutID, index uint32) (string, bool) {
	return b.graph.LayoutFieldName(id, index)
}
func (g *Graph) LayoutChildCount(id LayoutID) uint32 {
	n, _ := atID(g.layouts, id)
	return n.children.count
}
func (g *Graph) LayoutChild(id LayoutID, index uint32) (LayoutID, bool) {
	n, ok := atID(g.layouts, id)
	if !ok {
		return 0, false
	}
	return atRange(g.layoutRefs, n.children, index)
}

func (g *Graph) LayoutFieldNameCount(id LayoutID) uint32 {
	n, _ := atID(g.layouts, id)
	return n.fieldNames.count
}

func (g *Graph) LayoutFieldName(id LayoutID, index uint32) (string, bool) {
	n, ok := atID(g.layouts, id)
	if !ok {
		return "", false
	}
	return atRange(g.fieldNames, n.fieldNames, index)
}

// SameLayout reports structural layout equality across distinct IDs: two
// composite layouts minted separately compare equal when their types and
// children match recursively. Field names are presentation metadata and do
// not participate.
func (g *Graph) SameLayout(a, b LayoutID) bool {
	return validID(a, g.layouts) && validID(b, g.layouts) && g.sameLayout(a, b)
}
func (g *Graph) ScopeParent(id ScopeID) (ScopeID, bool) {
	n, ok := atID(g.scopes, id)
	return n.parent, ok
}
func (g *Graph) CaptureCount(id ScopeID) uint32 { n, _ := atID(g.scopes, id); return n.captures.count }
func (g *Graph) Capture(id ScopeID, index uint32) (model.SlotID, bool) {
	n, ok := atID(g.scopes, id)
	if !ok {
		return 0, false
	}
	return atRange(g.slotRefs, n.captures, index)
}
func (n Scalar) OperandCount() uint32 { return n.operands.count }
func (g *Graph) Operand(id ScalarID, index uint32) (ScalarID, bool) {
	n, ok := g.Scalar(id)
	if !ok {
		return 0, false
	}
	return atRange(g.scalarRefs, n.operands, index)
}
func (n Scalar) ComparisonCount() uint32 { return n.comparisons.count }
func (g *Graph) Comparison(id ScalarID, index uint32) (SignatureID, bool) {
	n, ok := g.Scalar(id)
	if !ok {
		return 0, false
	}
	return atRange(g.comparisonRefs, n.comparisons, index)
}
func (n Rel) InputCount() uint32              { return n.inputs.count }
func (n Rel) ExpressionCount() uint32         { return n.expressions.count }
func (n Rel) OutputCount() uint32             { return n.outputs.count }
func (n Rel) BindingCount() uint32            { return n.bindings.count }
func (n Rel) OrderCount() uint32              { return n.order.count }
func (n Rel) GroupingSetCount() uint32        { return n.groups.count }
func (n Rel) AggregateCount() uint32          { return n.aggregates.count }
func (n Rel) GroupingCount() uint32           { return n.groupings.count }
func (n Rel) WindowCount() uint32             { return n.windows.count }
func (n Rel) PartitionCount() uint32          { return n.partition.count }
func (n Rel) LockCount() uint32               { return n.locks.count }
func (n RowLock) KeyCount() uint32            { return n.keys.count }
func (n Rel) ActionCount() uint32             { return n.actions.count }
func (n Rel) JSONTableColumnCount() uint32    { return n.jsonTableColumns.count }
func (n Rel) ViewMutationStepCount() uint32 { return n.viewMutationSteps.count }
func (n ViewMutationStep) InputCount() uint32 { return n.inputs.count }
func (n ViewMutationStep) ProjectionCount() uint32 { return n.projections.count }
func (n ViewMutationStep) OutputColumnCount() uint32 { return n.outputColumns.count }
func (n CTE) OutputCount() uint32             { return n.outputs.count }
func (n AggregateCall) DirectArgumentCount() uint32 { return n.direct.count }
func (n AggregateCall) ArgumentCount() uint32       { return n.args.count }
func (n AggregateCall) OrderCount() uint32          { return n.order.count }
func (n GroupingCall) ArgumentCount() uint32  { return n.args.count }
func (n WindowCall) ArgumentCount() uint32    { return n.args.count }
func (n MergeAction) AssignmentCount() uint32 { return n.assignments.count }
func (n Recursion) SearchKeyCount() uint32    { return n.searchKeys.count }
func (n Recursion) CycleKeyCount() uint32     { return n.cycleKeys.count }
func (g *Graph) JSONTableColumn(id RelID, index uint32) (JSONTableColumn, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return JSONTableColumn{}, false
	}
	return atRange(g.jsonTableColumns, n.jsonTableColumns, index)
}
func (g *Graph) Input(id RelID, index uint32) (RelID, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	return atRange(g.relRefs, n.inputs, index)
}
func (g *Graph) Expression(id RelID, index uint32) (ScalarID, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	return atRange(g.scalarRefs, n.expressions, index)
}
func (g *Graph) Output(id RelID, index uint32) (model.SlotID, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	return atRange(g.slotRefs, n.outputs, index)
}
func (g *Graph) Binding(id RelID, index uint32) (Binding, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return Binding{}, false
	}
	return atRange(g.bindings, n.bindings, index)
}
func (g *Graph) Order(id RelID, index uint32) (OrderKey, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return OrderKey{}, false
	}
	return atRange(g.orders, n.order, index)
}
func (g *Graph) GroupingSetWidth(id RelID, set uint32) (uint32, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	r, ok := atRange(g.groups, n.groups, set)
	return r.count, ok
}
func (g *Graph) GroupingKey(id RelID, set, index uint32) (uint32, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	r, ok := atRange(g.groups, n.groups, set)
	if !ok {
		return 0, false
	}
	return atRange(g.indices, r, index)
}
func (g *Graph) Aggregate(id RelID, index uint32) (AggregateCall, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return AggregateCall{}, false
	}
	return atRange(g.aggregates, n.aggregates, index)
}
func (g *Graph) AggregateDirectArgument(id RelID, call, index uint32) (ScalarID, bool) {
	n, ok := g.Aggregate(id, call)
	if !ok {
		return 0, false
	}
	return atRange(g.scalarRefs, n.direct, index)
}
func (g *Graph) AggregateArgument(id RelID, call, index uint32) (ScalarID, bool) {
	n, ok := g.Aggregate(id, call)
	if !ok {
		return 0, false
	}
	return atRange(g.scalarRefs, n.args, index)
}
func (g *Graph) AggregateOrder(id RelID, call, index uint32) (OrderKey, bool) {
	n, ok := g.Aggregate(id, call)
	if !ok {
		return OrderKey{}, false
	}
	return atRange(g.orders, n.order, index)
}
func (g *Graph) Grouping(id RelID, index uint32) (GroupingCall, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return GroupingCall{}, false
	}
	return atRange(g.groupings, n.groupings, index)
}
func (g *Graph) GroupingArgument(id RelID, call, index uint32) (uint32, bool) {
	n, ok := g.Grouping(id, call)
	if !ok {
		return 0, false
	}
	return atRange(g.indices, n.args, index)
}
func (g *Graph) Window(id RelID, index uint32) (WindowCall, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return WindowCall{}, false
	}
	return atRange(g.windows, n.windows, index)
}
func (g *Graph) WindowArgument(id RelID, call, index uint32) (ScalarID, bool) {
	n, ok := g.Window(id, call)
	if !ok {
		return 0, false
	}
	return atRange(g.scalarRefs, n.args, index)
}
func (g *Graph) Partition(id RelID, index uint32) (ScalarID, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	return atRange(g.scalarRefs, n.partition, index)
}
func (g *Graph) RowLock(id RelID, index uint32) (RowLock, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return RowLock{}, false
	}
	return atRange(g.locks, n.locks, index)
}
func (g *Graph) ViewMutationStep(id RelID, index uint32) (ViewMutationStep, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return ViewMutationStep{}, false
	}
	return atRange(g.viewMutationSteps, n.viewMutationSteps, index)
}

func (g *Graph) ViewMutationInput(id RelID, step, index uint32) (model.SlotID, bool) {
	n, ok := g.ViewMutationStep(id, step)
	if !ok {
		return 0, false
	}
	return atRange(g.slotRefs, n.inputs, index)
}

func (g *Graph) ViewMutationProjection(id RelID, step, index uint32) (ScalarID, bool) {
	n, ok := g.ViewMutationStep(id, step)
	if !ok {
		return 0, false
	}
	return atRange(g.scalarRefs, n.projections, index)
}
func (g *Graph) ViewMutationOutputColumn(id RelID, step, index uint32) (model.ColumnID, bool) {
	n, ok := g.ViewMutationStep(id, step)
	if !ok {
		return 0, false
	}
	return atRange(g.columnRefs, n.outputColumns, index)
}
func (g *Graph) RowLockKey(id RelID, lock, index uint32) (RowLockKey, bool) {
	n, ok := g.RowLock(id, lock)
	if !ok {
		return RowLockKey{}, false
	}
	return atRange(g.lockKeys, n.keys, index)
}
func (g *Graph) MergeAction(id RelID, index uint32) (MergeAction, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return MergeAction{}, false
	}
	return atRange(g.actions, n.actions, index)
}
func (g *Graph) MergeAssignment(id RelID, action, index uint32) (Binding, bool) {
	n, ok := g.MergeAction(id, action)
	if !ok {
		return Binding{}, false
	}
	return atRange(g.bindings, n.assignments, index)
}
func (g *Graph) SearchKey(id RelID, index uint32) (model.SlotID, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	return atRange(g.slotRefs, n.Recursion.searchKeys, index)
}
func (g *Graph) CycleKey(id RelID, index uint32) (model.SlotID, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	return atRange(g.slotRefs, n.Recursion.cycleKeys, index)
}
func (g *Graph) CTEOutput(id CTEID, index uint32) (model.SlotID, bool) {
	n, ok := g.CTE(id)
	if !ok {
		return 0, false
	}
	return atRange(g.slotRefs, n.outputs, index)
}
func (n Rel) AssignmentCount() uint32         { return n.assignments.count }
func (n Rel) ConflictAssignmentCount() uint32 { return n.conflictAssignments.count }
func (n Rel) ConflictArbiterCount() uint32    { return n.conflictArbiters.count }
func (g *Graph) ConflictArbiter(id RelID, index uint32) (model.ConflictArbiter, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return model.ConflictArbiter{}, false
	}
	return atRange(g.conflictArbiters, n.conflictArbiters, index)
}
func (n Rel) TargetKeyCount() uint32 { return n.targetKeys.count }
func (g *Graph) TargetKey(id RelID, index uint32) (model.SlotID, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	return atRange(g.slotRefs, n.targetKeys, index)
}
func (n Rel) UpdatedColumnCount() uint32 { return n.updatedColumns.count }
func (g *Graph) UpdatedColumn(id RelID, index uint32) (model.ColumnID, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return 0, false
	}
	return atRange(g.columnRefs, n.updatedColumns, index)
}
func (g *Graph) Assignment(id RelID, index uint32) (Binding, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return Binding{}, false
	}
	return atRange(g.bindings, n.assignments, index)
}
func (g *Graph) ConflictAssignment(id RelID, index uint32) (Binding, bool) {
	n, ok := g.Rel(id)
	if !ok {
		return Binding{}, false
	}
	return atRange(g.bindings, n.conflictAssignments, index)
}

// CloneConstant returns an owned value that may be mutated. Its type layout
// remains separately indexed in the graph.
func (g *Graph) CloneConstant(ctx context.Context, id ScalarID, maxBytes uint64) (model.Value, error) {
	if maxBytes == 0 {
		return model.Value{}, limitError()
	}
	n, ok := g.Scalar(id)
	if !ok || n.Kind != Const {
		return model.Value{}, invalid("not a constant handle")
	}
	value, _, err := model.CopyValue(ctx, g.values[n.constant-1], maxBytes)
	if err == model.ErrValueBudget || err == model.ErrValueDepth {
		return model.Value{}, limitError()
	}
	return value, err
}
