package normalize

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// Relational clones own new local slots to avoid self-join aliases. Scalar-only
// capture clones keep outer slot identities and declare them in the target scope.
type scopeClone struct {
	n              *normalizer
	source, target ir.ScopeID
	relations      []ir.RelID
	scalars        []ir.ScalarID
	slots          []model.SlotID
	captureInputs  bool
}

func (c *scopeClone) supported(id ir.RelID, depth uint32) bool {
	if !c.n.step(1) {
		return false
	}
	if depth >= c.n.o.Limits.MaxDepth {
		c.n.err = resourceError()
		return false
	}
	r, ok := c.n.g.Rel(id)
	if !ok || r.Scope != c.source || r.Effects.Volatility != ir.Immutable {
		return false
	}
	switch r.Kind {
	case ir.Values, ir.Scan, ir.Project, ir.ProjectSet, ir.Filter, ir.Sort, ir.Limit, ir.Join, ir.Distinct, ir.SetOp, ir.GenerateSeries, ir.GenerateSubscripts, ir.Unnest, ir.RowsFrom:
	default:
		return false
	}
	for i := range r.InputCount() {
		child, _ := c.n.g.Input(id, i)
		if !c.supported(child, depth+1) {
			return false
		}
	}
	for _, scalar := range [...]ir.ScalarID{r.Predicate, r.Count, r.Offset} {
		if !c.scalarSupported(scalar, depth+1) {
			return false
		}
	}
	for i := range r.ExpressionCount() {
		scalar, _ := c.n.g.Expression(id, i)
		if !c.scalarSupported(scalar, depth+1) {
			return false
		}
	}
	for i := range r.BindingCount() {
		binding, _ := c.n.g.Binding(id, i)
		if !c.scalarSupported(binding.Expr, depth+1) {
			return false
		}
	}
	for i := range r.OrderCount() {
		order, _ := c.n.g.Order(id, i)
		if !c.scalarSupported(order.Expr, depth+1) {
			return false
		}
	}
	return true
}

func (c *scopeClone) scalarSupported(id ir.ScalarID, depth uint32) bool {
	if id == 0 {
		return true
	}
	if !c.n.step(1) {
		return false
	}
	if depth >= c.n.o.Limits.MaxDepth {
		c.n.err = resourceError()
		return false
	}
	s, ok := c.n.g.Scalar(id)
	if !ok || s.Scope != c.source || s.Relation != 0 {
		return false
	}
	if !c.captureInputs && s.Effects.Volatility != ir.Immutable {
		return false
	}
	if c.captureInputs && s.Effects.Volatility == ir.Volatile {
		return false
	}
	for i := range s.OperandCount() {
		child, _ := c.n.g.Operand(id, i)
		if !c.scalarSupported(child, depth+1) {
			return false
		}
	}
	return true
}

func (c *scopeClone) initialize() {
	if !c.captureInputs {
		c.relations = allocate[ir.RelID](c.n, uint64(c.n.g.RelCount())+1)
	}
	c.scalars = allocate[ir.ScalarID](c.n, uint64(c.n.g.ScalarCount())+1)
	c.slots = allocate[model.SlotID](c.n, uint64(c.n.g.SlotCount())+1)
}

func (c *scopeClone) slot(id model.SlotID) model.SlotID {
	if id == 0 || !c.n.step(1) {
		return 0
	}
	if c.slots[id] != 0 {
		return c.slots[id]
	}
	if c.captureInputs {
		c.n.err = c.n.b.AddScopeCapture(c.n.ctx, c.target, id)
		c.slots[id] = id
		return id
	}
	info, _ := c.n.g.Slot(id)
	info.Scope, info.Layout = c.target, c.n.layouts[info.Layout]
	result, err := c.n.b.AddSlot(c.n.ctx, info)
	c.n.err = err
	c.slots[id] = result
	return result
}

func (c *scopeClone) scalar(id ir.ScalarID, depth uint32) ir.ScalarID {
	if id == 0 || !c.n.step(1) {
		return 0
	}
	if depth >= c.n.o.Limits.MaxDepth {
		c.n.err = resourceError()
		return 0
	}
	if c.scalars[id] != 0 {
		return c.scalars[id]
	}
	spec, err := c.n.g.CloneScalarSpec(c.n.ctx, id, c.n.o.MaxBytes-c.n.bytes)
	if err != nil {
		c.n.err = err
		return 0
	}
	if !c.n.charge(1, uint64(unsafe.Sizeof(spec))) || !c.n.charge(uint64(len(spec.Operands)), uint64(unsafe.Sizeof(ir.ScalarID(0)))) {
		return 0
	}
	if spec.Kind == ir.Const {
		c.n.valueBytes(spec.Value)
	}
	spec.Scope, spec.Layout = c.target, c.n.layouts[spec.Layout]
	spec.Slot = c.slot(spec.Slot)
	for i, operand := range spec.Operands {
		spec.Operands[i] = c.scalar(operand, depth+1)
	}
	if c.n.err != nil {
		return 0
	}
	result, err := c.n.b.AddScalar(c.n.ctx, spec)
	c.n.err = err
	c.scalars[id] = result
	return result
}

func (c *scopeClone) relation(id ir.RelID, depth uint32) ir.RelID {
	if !c.n.step(1) {
		return 0
	}
	if depth >= c.n.o.Limits.MaxDepth {
		c.n.err = resourceError()
		return 0
	}
	if c.relations[id] != 0 {
		return c.relations[id]
	}
	spec, err := c.n.g.CloneRelSpec(c.n.ctx, id, c.n.o.MaxBytes-c.n.bytes)
	if err != nil {
		c.n.err = err
		return 0
	}
	if !c.chargeRelation(spec) {
		return 0
	}
	spec.Scope = c.target
	spec.RowResult = c.n.layouts[spec.RowResult]
	for i := range spec.JSONTableColumns {
		if spec.JSONTableColumns[i].Layout != 0 {
			spec.JSONTableColumns[i].Layout = c.n.layouts[spec.JSONTableColumns[i].Layout]
		}
	}
	for i, input := range spec.Inputs {
		spec.Inputs[i] = c.relation(input, depth+1)
	}
	for i, output := range spec.Outputs {
		spec.Outputs[i] = c.slot(output)
	}
	spec.MarkSlot = c.slot(spec.MarkSlot)
	spec.OldRow, spec.NewRow = c.slot(spec.OldRow), c.slot(spec.NewRow)
	spec.ExcludedRow = c.slot(spec.ExcludedRow)
	spec.SourceObject = c.slot(spec.SourceObject)
	spec.OldTableOID, spec.NewTableOID = c.slot(spec.OldTableOID), c.slot(spec.NewTableOID)
	spec.MergeTableOID = c.slot(spec.MergeTableOID)
	spec.MergeResultRow, spec.MergeActionSlot = c.slot(spec.MergeResultRow), c.slot(spec.MergeActionSlot)
	spec.MergeSourcePresent, spec.MergeTargetPresent = c.slot(spec.MergeSourcePresent), c.slot(spec.MergeTargetPresent)
	for i := range spec.TargetKeys {
		spec.TargetKeys[i] = c.slot(spec.TargetKeys[i])
	}
	for _, bindings := range [][]ir.Binding{spec.Assignments, spec.ConflictAssignments} {
		for i := range bindings {
			bindings[i].Slot = c.slot(bindings[i].Slot)
			bindings[i].Expr = c.scalar(bindings[i].Expr, depth+1)
		}
	}
	spec.ConflictPredicate = c.scalar(spec.ConflictPredicate, depth+1)
	for i := range spec.Actions {
		action := &spec.Actions[i]
		action.Condition = c.scalar(action.Condition, depth+1)
		for j := range action.Assignments {
			action.Assignments[j].Slot = c.slot(action.Assignments[j].Slot)
			action.Assignments[j].Expr = c.scalar(action.Assignments[j].Expr, depth+1)
		}
	}
	for i := range spec.ViewMutationSteps {
		step := &spec.ViewMutationSteps[i]
		step.Predicate = c.scalar(step.Predicate, depth+1)
		for j := range step.Inputs {
			step.Inputs[j] = c.slot(step.Inputs[j])
		}
		for j := range step.Projections {
			step.Projections[j] = c.scalar(step.Projections[j], depth+1)
		}
	}
	for i, expression := range spec.Expressions {
		spec.Expressions[i] = c.scalar(expression, depth+1)
	}
	for i := range spec.Bindings {
		spec.Bindings[i].Slot = c.slot(spec.Bindings[i].Slot)
		spec.Bindings[i].Expr = c.scalar(spec.Bindings[i].Expr, depth+1)
	}
	for i := range spec.Order {
		spec.Order[i].Expr = c.scalar(spec.Order[i].Expr, depth+1)
	}
	spec.Predicate = c.scalar(spec.Predicate, depth+1)
	spec.Count = c.scalar(spec.Count, depth+1)
	spec.Offset = c.scalar(spec.Offset, depth+1)
	if c.n.err != nil {
		return 0
	}
	// The clone rewrites every scalar and every input it points at, so its
	// declared effects must be widened to cover both, exactly as relation()
	// does for the non-cloned path. See relationScalarEffects for why a rewrite
	// can raise a scalar's effects.
	for _, input := range spec.Inputs {
		if rel, ok := c.n.g.Rel(input); ok {
			spec.Effects = spec.Effects.Union(rel.Effects)
		}
	}
	spec.Effects = c.n.relationScalarEffects(spec, spec.Effects)
	result, err := c.n.b.AddRel(c.n.ctx, spec)
	c.n.err = err
	c.relations[id] = result
	return result
}

func (c *scopeClone) chargeRelation(spec ir.RelSpec) bool {
	if !c.n.charge(1, uint64(unsafe.Sizeof(spec))) {
		return false
	}
	if !c.n.charge(uint64(len(spec.Inputs)), uint64(unsafe.Sizeof(ir.RelID(0)))) {
		return false
	}
	if !c.n.charge(uint64(len(spec.Outputs)), uint64(unsafe.Sizeof(model.SlotID(0)))) {
		return false
	}
	if !c.n.charge(uint64(len(spec.TargetKeys)), uint64(unsafe.Sizeof(model.SlotID(0)))) {
		return false
	}
	if !c.n.charge(uint64(len(spec.Assignments)+len(spec.ConflictAssignments)), uint64(unsafe.Sizeof(ir.Binding{}))) {
		return false
	}
	if !c.n.charge(uint64(len(spec.ConflictArbiters)), uint64(unsafe.Sizeof(model.ConflictArbiter{}))) {
		return false
	}
	if !c.n.charge(uint64(len(spec.Actions)), uint64(unsafe.Sizeof(ir.MergeActionSpec{}))) {
		return false
	}
	for _, action := range spec.Actions {
		if !c.n.charge(uint64(len(action.Assignments)), uint64(unsafe.Sizeof(ir.Binding{}))) {
			return false
		}
	}
	if !c.n.charge(uint64(len(spec.ViewMutationSteps)), uint64(unsafe.Sizeof(ir.ViewMutationStepSpec{}))) {
		return false
	}
	for _, step := range spec.ViewMutationSteps {
		if !c.n.charge(uint64(len(step.Inputs)), uint64(unsafe.Sizeof(model.SlotID(0)))) ||
			!c.n.charge(uint64(len(step.Projections)), uint64(unsafe.Sizeof(ir.ScalarID(0)))) ||
			!c.n.charge(uint64(len(step.OutputColumns)), uint64(unsafe.Sizeof(model.ColumnID(0)))) {
			return false
		}
	}
	if !c.n.charge(uint64(len(spec.Expressions)), uint64(unsafe.Sizeof(ir.ScalarID(0)))) {
		return false
	}
	if !c.n.charge(uint64(len(spec.Bindings)), uint64(unsafe.Sizeof(ir.Binding{}))) {
		return false
	}
	if !c.n.charge(uint64(len(spec.JSONTableColumns)), uint64(unsafe.Sizeof(ir.JSONTableColumn{}))) {
		return false
	}
	return c.n.charge(uint64(len(spec.Order)), uint64(unsafe.Sizeof(ir.OrderKey{})))
}
