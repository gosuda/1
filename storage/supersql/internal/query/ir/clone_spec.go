package ir

import (
	"context"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// CloneRelSpec preserves all handles and returns owned operand metadata suitable
// for an Extend builder. Handles must be remapped for any unrelated builder.
func (g *Graph) CloneRelSpec(ctx context.Context, id RelID, maxBytes uint64) (RelSpec, error) {
	if err := ctx.Err(); err != nil {
		return RelSpec{}, err
	}
	if g == nil || !validID(id, g.rels) {
		return RelSpec{}, invalid("invalid relation clone handle")
	}
	c := specCopier{ctx: ctx, remaining: maxBytes}
	c.charge(uint64(unsafe.Sizeof(RelSpec{})))
	source := g.rels[id-1]
	result := RelSpec{
		Rel:                 source.Metadata(),
		Inputs:              cloneSpecSlice(&c, sliceAt(g.relRefs, source.inputs)),
		Expressions:         cloneSpecSlice(&c, sliceAt(g.scalarRefs, source.expressions)),
		Outputs:             cloneSpecSlice(&c, sliceAt(g.slotRefs, source.outputs)),
		TargetKeys:          cloneSpecSlice(&c, sliceAt(g.slotRefs, source.targetKeys)),
		UpdatedColumns:      cloneSpecSlice(&c, sliceAt(g.columnRefs, source.updatedColumns)),
		Bindings:            cloneSpecSlice(&c, sliceAt(g.bindings, source.bindings)),
		Assignments:         cloneSpecSlice(&c, sliceAt(g.bindings, source.assignments)),
		ConflictAssignments: cloneSpecSlice(&c, sliceAt(g.bindings, source.conflictAssignments)),
		ConflictArbiters:    cloneSpecSlice(&c, sliceAt(g.conflictArbiters, source.conflictArbiters)),
		Order:               cloneSpecSlice(&c, sliceAt(g.orders, source.order)),
		Partition:           cloneSpecSlice(&c, sliceAt(g.scalarRefs, source.partition)),
		RecursionSpec:       RecursionSpec{Recursion: source.Recursion.Metadata(), SearchKeys: cloneSpecSlice(&c, sliceAt(g.slotRefs, source.Recursion.searchKeys)), CycleKeys: cloneSpecSlice(&c, sliceAt(g.slotRefs, source.Recursion.cycleKeys))},
		JSONTableColumns:    cloneSpecSlice(&c, sliceAt(g.jsonTableColumns, source.jsonTableColumns)),
	}
	result.Locks = makeSpecSlice[RowLockSpec](&c, source.locks.count)
	for i, lock := range sliceAt(g.locks, source.locks) {
		if c.err != nil {
			break
		}
		metadata := lock
		metadata.keys = extent{}
		result.Locks[i] = RowLockSpec{RowLock: metadata, Keys: cloneSpecSlice(&c, sliceAt(g.lockKeys, lock.keys))}
	}
	result.GroupingSets = makeSpecSlice[[]uint32](&c, source.groups.count)
	for i, group := range sliceAt(g.groups, source.groups) {
		if c.err != nil {
			break
		}
		result.GroupingSets[i] = cloneSpecSlice(&c, sliceAt(g.indices, group))
	}
	result.Aggregates = makeSpecSlice[AggregateSpec](&c, source.aggregates.count)
	for i, call := range sliceAt(g.aggregates, source.aggregates) {
		if c.err != nil {
			break
		}
		result.Aggregates[i] = AggregateSpec{
			AggregateCall: call.Metadata(),
			Direct:        cloneSpecSlice(&c, sliceAt(g.scalarRefs, call.direct)),
			Args:          cloneSpecSlice(&c, sliceAt(g.scalarRefs, call.args)),
			Order:         cloneSpecSlice(&c, sliceAt(g.orders, call.order)),
		}
	}
	result.Groupings = makeSpecSlice[GroupingSpec](&c, source.groupings.count)
	for i, call := range sliceAt(g.groupings, source.groupings) {
		if c.err != nil {
			break
		}
		result.Groupings[i] = GroupingSpec{GroupingCall: call.Metadata(), Args: cloneSpecSlice(&c, sliceAt(g.indices, call.args))}
	}
	result.Windows = makeSpecSlice[WindowSpec](&c, source.windows.count)
	for i, call := range sliceAt(g.windows, source.windows) {
		if c.err != nil {
			break
		}
		result.Windows[i] = WindowSpec{WindowCall: call.Metadata(), Args: cloneSpecSlice(&c, sliceAt(g.scalarRefs, call.args))}
	}
	result.Actions = makeSpecSlice[MergeActionSpec](&c, source.actions.count)
	for i, action := range sliceAt(g.actions, source.actions) {
		if c.err != nil {
			break
		}
		result.Actions[i] = MergeActionSpec{MergeAction: action.Metadata(), Assignments: cloneSpecSlice(&c, sliceAt(g.bindings, action.assignments))}
	}
	result.ViewMutationSteps = makeSpecSlice[ViewMutationStepSpec](&c, source.viewMutationSteps.count)
	for i, step := range sliceAt(g.viewMutationSteps, source.viewMutationSteps) {
		if c.err != nil {
			break
		}
		result.ViewMutationSteps[i] = ViewMutationStepSpec{
			ViewMutationStep: step.Metadata(),
			Inputs:           cloneSpecSlice(&c, sliceAt(g.slotRefs, step.inputs)),
			Projections:      cloneSpecSlice(&c, sliceAt(g.scalarRefs, step.projections)),
			OutputColumns:    cloneSpecSlice(&c, sliceAt(g.columnRefs, step.outputColumns)),
		}
	}
	if c.err != nil {
		return RelSpec{}, c.err
	}
	return result, ctx.Err()
}

// CloneScalarSpec owns constant payloads and operands while retaining handles.
func (g *Graph) CloneScalarSpec(ctx context.Context, id ScalarID, maxBytes uint64) (ScalarSpec, error) {
	if err := ctx.Err(); err != nil {
		return ScalarSpec{}, err
	}
	if g == nil || !validID(id, g.scalars) {
		return ScalarSpec{}, invalid("invalid scalar clone handle")
	}
	c := specCopier{ctx: ctx, remaining: maxBytes}
	c.charge(uint64(unsafe.Sizeof(ScalarSpec{})))
	source := g.scalars[id-1]
	result := ScalarSpec{
		Scalar:      source.Metadata(),
		Operands:    cloneSpecSlice(&c, sliceAt(g.scalarRefs, source.operands)),
		Comparisons: cloneSpecSlice(&c, sliceAt(g.comparisonRefs, source.comparisons)),
	}
	if c.err != nil {
		return ScalarSpec{}, c.err
	}
	if source.Kind == Const {
		value, err := g.CloneConstant(ctx, id, c.remaining)
		if err != nil {
			return ScalarSpec{}, err
		}
		result.Value = value
	}
	return result, ctx.Err()
}

type specCopier struct {
	ctx       context.Context
	remaining uint64
	err       error
}

func (c *specCopier) charge(bytes uint64) bool {
	if c.err != nil {
		return false
	}
	if c.err = c.ctx.Err(); c.err != nil {
		return false
	}
	if bytes > c.remaining {
		c.err = limitError()
		return false
	}
	c.remaining -= bytes
	return true
}
func makeSpecSlice[T any](c *specCopier, count uint32) []T {
	var value T
	if !c.charge(uint64(count) * uint64(unsafe.Sizeof(value))) {
		return nil
	}
	return make([]T, int(count))
}
func cloneSpecSlice[T any](c *specCopier, source []T) []T {
	result := makeSpecSlice[T](c, uint32(len(source)))
	copy(result, source)
	return result
}

// CloneLayout returns owned physical and presentation metadata within maxBytes.
func (g *Graph) CloneLayout(ctx context.Context, id LayoutID, maxBytes uint64) (model.TypeLayout, error) {
	if err := ctx.Err(); err != nil {
		return model.TypeLayout{}, err
	}
	if g == nil || !validID(id, g.layouts) {
		return model.TypeLayout{}, invalid("invalid layout clone handle")
	}
	c := specCopier{ctx: ctx, remaining: maxBytes}
	c.charge(uint64(unsafe.Sizeof(model.TypeLayout{})))
	result := g.cloneLayout(&c, id, 0)
	if c.err != nil {
		return model.TypeLayout{}, c.err
	}
	return result, ctx.Err()
}

func (g *Graph) cloneLayout(c *specCopier, id LayoutID, depth uint32) model.TypeLayout {
	if !c.charge(0) {
		return model.TypeLayout{}
	}
	if depth >= 65 {
		c.err = limitError()
		return model.TypeLayout{}
	}
	n := g.layouts[id-1]
	result := model.TypeLayout{Type: n.typ}
	result.Children = makeSpecSlice[model.TypeLayout](c, n.children.count)
	result.FieldNames = makeSpecSlice[string](c, n.fieldNames.count)
	if c.err != nil {
		return model.TypeLayout{}
	}
	for i, name := range sliceAt(g.fieldNames, n.fieldNames) {
		if !c.charge(uint64(len(name))) {
			return model.TypeLayout{}
		}
		result.FieldNames[i] = strings.Clone(name)
	}
	for i, child := range sliceAt(g.layoutRefs, n.children) {
		result.Children[i] = g.cloneLayout(c, child, depth+1)
		if c.err != nil {
			return model.TypeLayout{}
		}
	}
	return result
}
