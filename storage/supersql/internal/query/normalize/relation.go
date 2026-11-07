package normalize

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (n *normalizer) scalarSlots(id ir.ScalarID, slots []bool, depth uint32) {
	if id == 0 || depth >= n.o.Limits.MaxDepth || !n.step(1) {
		if depth >= n.o.Limits.MaxDepth {
			n.err = resourceError()
		}
		return
	}
	scalar, ok := n.g.Scalar(id)
	if !ok {
		n.err = ErrInvalid
		return
	}
	if scalar.Kind == ir.Slot {
		slots[scalar.Slot] = true
	}
	for i := range scalar.OperandCount() {
		child, _ := n.g.Operand(id, i)
		n.scalarSlots(child, slots, depth+1)
	}
}

func (n *normalizer) projectedScalarSlots(id ir.ScalarID, projection ir.RelID, slots []bool, depth uint32) {
	if id == 0 || depth >= n.o.Limits.MaxDepth || !n.step(1) {
		if depth >= n.o.Limits.MaxDepth {
			n.err = resourceError()
		}
		return
	}
	scalar, ok := n.g.Scalar(id)
	if !ok {
		n.err = ErrInvalid
		return
	}
	if scalar.Kind == ir.Slot {
		expression := n.projectedExpression(projection, scalar.Slot)
		projected, _ := n.g.Scalar(expression)
		if expression != 0 && (projected.Kind != ir.Slot || projected.Slot != scalar.Slot) {
			n.scalarSlots(expression, slots, depth+1)
			return
		}
		slots[scalar.Slot] = true
	}
	for i := range scalar.OperandCount() {
		child, _ := n.g.Operand(id, i)
		n.projectedScalarSlots(child, projection, slots, depth+1)
	}
}

func (n *normalizer) requireScanOutputs(scanID ir.RelID, scan ir.Rel, required []bool) {
	// One direct consumer, but several consulters: the consumer itself and any
	// projection chain composed past it. Record the union; a later verdict that
	// needs every output must also retract an earlier narrower prune, or the
	// consumer keeps binding an output the scan no longer produces.
	if n.pruneScan[scanID] {
		for _, slot := range n.scanOutputs[scanID] {
			required[slot] = true
		}
	}
	count := 0
	for i := range scan.OutputCount() {
		slot, _ := n.g.Output(scanID, i)
		if required[slot] {
			count++
		}
	}
	if count == 0 || count == int(scan.OutputCount()) {
		n.scanOutputs[scanID], n.pruneScan[scanID] = nil, false
		return
	}
	outputs := allocate[model.SlotID](n, uint64(count))
	at := 0
	for i := range scan.OutputCount() {
		slot, _ := n.g.Output(scanID, i)
		if required[slot] {
			outputs[at] = slot
			at++
		}
	}
	n.scanOutputs[scanID], n.pruneScan[scanID] = outputs, true
}

func (n *normalizer) prepareProjectScan(id ir.RelID, source ir.Rel, depth uint32) {
	if source.Kind != ir.Project || source.SecurityBarrier || source.InputCount() != 1 || source.BindingCount() != source.OutputCount() {
		return
	}
	input, _ := n.g.Input(id, 0)
	child, _ := n.g.Rel(input)
	scanID, scan := input, child
	projected := ir.RelID(0)
	if child.Kind == ir.Project {
		for i := range source.BindingCount() {
			binding, _ := n.g.Binding(id, i)
			safe, _ := n.projectionScalar(binding.Expr, source.Scope, input, depth+1)
			if !safe {
				return
			}
		}
		if n.relUses[input] != 1 || !n.simpleProjectionInput(input, source.Scope, depth+1) {
			return
		}
		scanID, _ = n.g.Input(input, 0)
		scan, _ = n.g.Rel(scanID)
		projected = input
	}
	if scan.Kind != ir.Scan || n.relUses[scanID] != 1 {
		return
	}
	required := allocate[bool](n, uint64(n.g.SlotCount())+1)
	for i := range source.BindingCount() {
		binding, _ := n.g.Binding(id, i)
		if projected == 0 {
			n.scalarSlots(binding.Expr, required, depth+1)
		} else {
			n.projectedScalarSlots(binding.Expr, projected, required, depth+1)
		}
	}
	if n.err == nil {
		n.requireScanOutputs(scanID, scan, required)
	}
}

func (n *normalizer) nullWhenSide(id ir.ScalarID, sides []uint8, side uint8, depth uint32) bool {
	if id == 0 || depth >= n.o.Limits.MaxDepth || !n.step(1) {
		if depth >= n.o.Limits.MaxDepth {
			n.err = resourceError()
		}
		return false
	}
	scalar, ok := n.g.Scalar(id)
	if !ok {
		n.err = ErrInvalid
		return false
	}
	if scalar.Kind == ir.Slot {
		return sides[scalar.Slot]&side != 0
	}
	forward := scalar.Kind == ir.Once || scalar.Kind == ir.Collate || scalar.Kind == ir.Relabel || scalar.Kind == ir.Bool && scalar.BoolOp == ir.Not
	if !scalar.Strict && !forward {
		return false
	}
	for i := range scalar.OperandCount() {
		child, _ := n.g.Operand(id, i)
		if n.nullWhenSide(child, sides, side, depth+1) {
			return true
		}
	}
	return false
}

func (n *normalizer) rejectsNullSide(id ir.ScalarID, sides []uint8, side uint8, depth uint32) bool {
	if n.nullWhenSide(id, sides, side, depth) {
		return true
	}
	scalar, ok := n.g.Scalar(id)
	if !ok || n.err != nil {
		return false
	}
	if scalar.Kind == ir.NullTest && scalar.Negated && scalar.OperandCount() == 1 {
		child, _ := n.g.Operand(id, 0)
		return n.nullWhenSide(child, sides, side, depth+1)
	}
	if scalar.Kind != ir.Bool {
		return false
	}
	switch scalar.BoolOp {
	case ir.And:
		for i := range scalar.OperandCount() {
			child, _ := n.g.Operand(id, i)
			if n.rejectsNullSide(child, sides, side, depth+1) {
				return true
			}
		}
	case ir.Or:
		if scalar.OperandCount() == 0 {
			return false
		}
		for i := range scalar.OperandCount() {
			child, _ := n.g.Operand(id, i)
			if !n.rejectsNullSide(child, sides, side, depth+1) {
				return false
			}
		}
		return true
	}
	return false
}

func (n *normalizer) prepareOuterJoinReduction(id ir.RelID, source ir.Rel) {
	if source.Kind != ir.Filter || source.SecurityBarrier || source.InputCount() != 1 || source.Predicate == 0 {
		return
	}
	input, _ := n.g.Input(id, 0)
	join, _ := n.g.Rel(input)
	if n.relUses[input] != 1 || join.SecurityBarrier || join.Kind != ir.Join || (join.JoinKind != ir.LeftJoin && join.JoinKind != ir.RightJoin && join.JoinKind != ir.FullJoin) {
		return
	}
	predicate, _ := n.g.Scalar(source.Predicate)
	if predicate.Effects != (ir.Effects{}) {
		return
	}
	leftID, _ := n.g.Input(input, 0)
	left, _ := n.g.Rel(leftID)
	sides := allocate[uint8](n, uint64(n.g.SlotCount())+1)
	for i := range join.OutputCount() {
		slot, _ := n.g.Output(input, i)
		if i < left.OutputCount() {
			sides[slot] |= 1
		} else {
			sides[slot] |= 2
		}
	}
	rejectsLeft := n.rejectsNullSide(source.Predicate, sides, 1, 0)
	rejectsRight := n.rejectsNullSide(source.Predicate, sides, 2, 0)
	switch join.JoinKind {
	case ir.LeftJoin:
		if rejectsRight {
			n.joinKinds[input] = ir.InnerJoin
		}
	case ir.RightJoin:
		if rejectsLeft {
			n.joinKinds[input] = ir.InnerJoin
		}
	case ir.FullJoin:
		switch {
		case rejectsLeft && rejectsRight:
			n.joinKinds[input] = ir.InnerJoin
		case rejectsLeft:
			n.joinKinds[input] = ir.LeftJoin
		case rejectsRight:
			n.joinKinds[input] = ir.RightJoin
		}
	}
}

func (n *normalizer) relation(id ir.RelID, depth uint32) relResult {
	if id == 0 || !n.step(1) {
		return relResult{}
	}
	if depth >= n.o.Limits.MaxDepth {
		n.err = resourceError()
		return relResult{}
	}
	if n.rels[id].id != 0 {
		return n.rels[id]
	}
	if n.relActive[id] {
		n.err = ErrInvalid
		return relResult{}
	}
	n.relActive[id] = true
	source, _ := n.g.Rel(id)
	n.prepareProjectScan(id, source, depth)
	n.prepareOuterJoinReduction(id, source)
	if n.err != nil {
		return relResult{}
	}
	spec := ir.RelSpec{Rel: source.Metadata()}
	if n.joinKinds[id] != 0 {
		spec.JoinKind = n.joinKinds[id]
	}
	spec.RowResult = n.layouts[source.RowResult]
	spec.JSONTableColumns = allocate[ir.JSONTableColumn](n, uint64(source.JSONTableColumnCount()))
	for i := range spec.JSONTableColumns {
		spec.JSONTableColumns[i], _ = n.g.JSONTableColumn(id, uint32(i))
		if spec.JSONTableColumns[i].Layout != 0 {
			spec.JSONTableColumns[i].Layout = n.layouts[spec.JSONTableColumns[i].Layout]
		}
	}
	spec.TargetKeys = allocate[model.SlotID](n, uint64(source.TargetKeyCount()))
	for i := range spec.TargetKeys {
		spec.TargetKeys[i], _ = n.g.TargetKey(id, uint32(i))
	}
	spec.UpdatedColumns = allocate[model.ColumnID](n, uint64(source.UpdatedColumnCount()))
	for i := range spec.UpdatedColumns {
		spec.UpdatedColumns[i], _ = n.g.UpdatedColumn(id, uint32(i))
	}
	spec.Inputs = allocate[ir.RelID](n, uint64(source.InputCount()))
	var first relResult
	for i := range spec.Inputs {
		child, _ := n.g.Input(id, uint32(i))
		result := n.relation(child, depth+1)
		spec.Inputs[i] = result.id
		spec.Effects = spec.Effects.Union(result.n.Effects)
		if i == 0 {
			first = result
		}
	}
	if n.err != nil {
		return relResult{}
	}
	previousInputs, previousContext, previousBounds, previousProjection := n.constantInputs, n.scalarContext, n.foldBounds, n.projectInput
	defer func() {
		n.constantInputs, n.scalarContext, n.foldBounds = previousInputs, previousContext, previousBounds
		n.projectInput = previousProjection
	}()
	n.scalarContext, n.foldBounds = id, false
	n.projectInput = 0
	if projected, input := n.composeProjection(id, source, first, depth); projected != 0 {
		n.projectInput, first = projected, input
		project, _ := n.g.Rel(projected)
		n.constantInputs = n.inputConstants(projected, project)
		spec.Inputs[0] = first.id
	} else {
		n.constantInputs = n.inputConstants(id, source)
	}
	if n.pruneScan[id] {
		spec.Outputs = allocate[model.SlotID](n, uint64(len(n.scanOutputs[id])))
		copy(spec.Outputs, n.scanOutputs[id])
	} else {
		spec.Outputs = allocate[model.SlotID](n, uint64(source.OutputCount()))
		for i := range spec.Outputs {
			spec.Outputs[i], _ = n.g.Output(id, uint32(i))
		}
	}
	if result, lowered := n.predicateJoin(source, spec, first, depth); lowered {
		n.relActive[id] = false
		n.rels[id] = result
		return result
	}
	spec.Expressions = n.scalarList(source.ExpressionCount(), func(i uint32) ir.ScalarID { value, _ := n.g.Expression(id, i); return value }, depth)
	spec.Partition = n.scalarList(source.PartitionCount(), func(i uint32) ir.ScalarID { value, _ := n.g.Partition(id, i); return value }, depth)
	spec.Bindings = n.bindings(source.BindingCount(), func(i uint32) ir.Binding { value, _ := n.g.Binding(id, i); return value }, depth)
	spec.Assignments = n.bindings(source.AssignmentCount(), func(i uint32) ir.Binding { value, _ := n.g.Assignment(id, i); return value }, depth)
	spec.ConflictAssignments = n.bindings(source.ConflictAssignmentCount(), func(i uint32) ir.Binding { value, _ := n.g.ConflictAssignment(id, i); return value }, depth)
	spec.ConflictArbiters = allocate[model.ConflictArbiter](n, uint64(source.ConflictArbiterCount()))
	for i := range spec.ConflictArbiters {
		spec.ConflictArbiters[i], _ = n.g.ConflictArbiter(id, uint32(i))
	}
	spec.Order = n.orders(source.OrderCount(), func(i uint32) ir.OrderKey { value, _ := n.g.Order(id, i); return value }, depth)
	spec.Locks = allocate[ir.RowLockSpec](n, uint64(source.LockCount()))
	for i := range spec.Locks {
		lock, _ := n.g.RowLock(id, uint32(i))
		keys := allocate[ir.RowLockKey](n, uint64(lock.KeyCount()))
		for j := range keys {
			keys[j], _ = n.g.RowLockKey(id, uint32(i), uint32(j))
		}
		spec.Locks[i] = ir.RowLockSpec{RowLock: lock.Metadata(), Keys: keys}
	}
	spec.GroupingSets = allocate[[]uint32](n, uint64(source.GroupingSetCount()))
	for i := range spec.GroupingSets {
		width, _ := n.g.GroupingSetWidth(id, uint32(i))
		spec.GroupingSets[i] = allocate[uint32](n, uint64(width))
		for j := range spec.GroupingSets[i] {
			spec.GroupingSets[i][j], _ = n.g.GroupingKey(id, uint32(i), uint32(j))
		}
	}
	predicate := n.scalar(source.Predicate, depth+1)
	spec.Predicate = predicate.id
	spec.Offset = n.scalar(source.Offset, depth+1).id
	spec.Count = n.scalar(source.Count, depth+1).id
	spec.ConflictPredicate = n.scalar(source.ConflictPredicate, depth+1).id
	spec.Frame.Start.Offset = n.scalar(source.Frame.Start.Offset, depth+1).id
	spec.Frame.End.Offset = n.scalar(source.Frame.End.Offset, depth+1).id
	spec.Aggregates = allocate[ir.AggregateSpec](n, uint64(source.AggregateCount()))
	for i := range spec.Aggregates {
		call, _ := n.g.Aggregate(id, uint32(i))
		spec.Aggregates[i] = ir.AggregateSpec{
			AggregateCall: call.Metadata(),
			Direct:        n.scalarList(call.DirectArgumentCount(), func(j uint32) ir.ScalarID { value, _ := n.g.AggregateDirectArgument(id, uint32(i), j); return value }, depth),
			Args:          n.scalarList(call.ArgumentCount(), func(j uint32) ir.ScalarID { value, _ := n.g.AggregateArgument(id, uint32(i), j); return value }, depth),
			Order:         n.orders(call.OrderCount(), func(j uint32) ir.OrderKey { value, _ := n.g.AggregateOrder(id, uint32(i), j); return value }, depth),
		}
		spec.Aggregates[i].Filter = n.scalar(call.Filter, depth+1).id
	}
	spec.Groupings = allocate[ir.GroupingSpec](n, uint64(source.GroupingCount()))
	for i := range spec.Groupings {
		call, _ := n.g.Grouping(id, uint32(i))
		spec.Groupings[i] = ir.GroupingSpec{GroupingCall: call.Metadata(), Args: allocate[uint32](n, uint64(call.ArgumentCount()))}
		for j := range spec.Groupings[i].Args {
			spec.Groupings[i].Args[j], _ = n.g.GroupingArgument(id, uint32(i), uint32(j))
		}
	}
	spec.Windows = allocate[ir.WindowSpec](n, uint64(source.WindowCount()))
	for i := range spec.Windows {
		call, _ := n.g.Window(id, uint32(i))
		spec.Windows[i] = ir.WindowSpec{WindowCall: call.Metadata(), Args: n.scalarList(call.ArgumentCount(), func(j uint32) ir.ScalarID { value, _ := n.g.WindowArgument(id, uint32(i), j); return value }, depth)}
		spec.Windows[i].Filter = n.scalar(call.Filter, depth+1).id
	}
	spec.RecursionSpec.Recursion = source.Recursion.Metadata()
	spec.RecursionSpec.CycleValue = n.scalar(source.Recursion.CycleValue, depth+1).id
	spec.RecursionSpec.CycleDefault = n.scalar(source.Recursion.CycleDefault, depth+1).id
	spec.RecursionSpec.CycleContinue = n.scalar(source.Recursion.CycleContinue, depth+1).id
	spec.RecursionSpec.SearchKeys = allocate[model.SlotID](n, uint64(source.Recursion.SearchKeyCount()))
	for i := range spec.RecursionSpec.SearchKeys {
		spec.RecursionSpec.SearchKeys[i], _ = n.g.SearchKey(id, uint32(i))
	}
	spec.RecursionSpec.CycleKeys = allocate[model.SlotID](n, uint64(source.Recursion.CycleKeyCount()))
	for i := range spec.RecursionSpec.CycleKeys {
		spec.RecursionSpec.CycleKeys[i], _ = n.g.CycleKey(id, uint32(i))
	}
	spec.Actions = allocate[ir.MergeActionSpec](n, uint64(source.ActionCount()))
	for i := range spec.Actions {
		action, _ := n.g.MergeAction(id, uint32(i))
		spec.Actions[i] = ir.MergeActionSpec{MergeAction: action.Metadata(), Assignments: n.bindings(action.AssignmentCount(), func(j uint32) ir.Binding { value, _ := n.g.MergeAssignment(id, uint32(i), j); return value }, depth)}
		spec.Actions[i].Condition = n.scalar(action.Condition, depth+1).id
	}
	spec.ViewMutationSteps = allocate[ir.ViewMutationStepSpec](n, uint64(source.ViewMutationStepCount()))
	for i := range spec.ViewMutationSteps {
		step, _ := n.g.ViewMutationStep(id, uint32(i))
		spec.ViewMutationSteps[i].ViewMutationStep = step.Metadata()
		spec.ViewMutationSteps[i].Predicate = n.scalar(step.Predicate, depth+1).id
		spec.ViewMutationSteps[i].Inputs = allocate[model.SlotID](n, uint64(step.InputCount()))
		for j := range spec.ViewMutationSteps[i].Inputs {
			spec.ViewMutationSteps[i].Inputs[j], _ = n.g.ViewMutationInput(id, uint32(i), uint32(j))
		}
		spec.ViewMutationSteps[i].Projections = n.scalarList(step.ProjectionCount(), func(j uint32) ir.ScalarID {
			value, _ := n.g.ViewMutationProjection(id, uint32(i), j)
			return value
		}, depth)
		spec.ViewMutationSteps[i].OutputColumns = allocate[model.ColumnID](n, uint64(step.OutputColumnCount()))
		for j := range spec.ViewMutationSteps[i].OutputColumns {
			spec.ViewMutationSteps[i].OutputColumns[j], _ = n.g.ViewMutationOutputColumn(id, uint32(i), uint32(j))
		}
	}
	if n.err != nil {
		return relResult{}
	}
	if n.identityProject(id, spec, first) {
		n.relActive[id] = false
		n.rels[id] = first
		return first
	}
	stackedFilters := spec.Kind == ir.Filter && !spec.SecurityBarrier && first.n.Kind == ir.Filter && !first.n.SecurityBarrier
	sameRows := spec.Scope == first.n.Scope && slices.Equal(spec.Outputs, first.outputs)
	purePredicates := predicate.n.Effects == (ir.Effects{}) && first.predicate.n.Effects == (ir.Effects{})
	if stackedFilters && sameRows && purePredicates {
		// AND visits its right operand for NULL left lanes; a stacked filter
		// does not. Only error-free immutable predicates permit that widening.
		meta := ir.Scalar{Kind: ir.Bool, Scope: spec.Scope, Layout: predicate.n.Layout, Nullable: predicate.n.Nullable || first.predicate.n.Nullable, BoolOp: ir.And}
		predicate = n.addScalar(meta, []scalarResult{first.predicate, predicate})
		spec.Predicate = predicate.id
		spec.Inputs[0] = first.input
	}
	if n.err != nil {
		return relResult{}
	}
	// spec.Effects began as the bound relation's own effects and was widened only
	// by its inputs. Normalization rewrites every scalar this relation points at,
	// and a rewrite can RAISE a scalar's effects: lowering a quantified predicate
	// builds a ScalarApply, which ir.relationEffects reports as MayError.
	// Verification requires the relation's declared effects to cover everything
	// ir.relationEffects collects from it (ir/verify_rel.go), so fold the
	// rewritten scalars in before publishing. This must run after every scalar
	// field above is assigned.
	spec.Effects = n.relationScalarEffects(spec, spec.Effects)
	newID, err := n.b.AddRel(n.ctx, spec)
	n.err = err
	result := relResult{id: newID, n: spec.Rel, predicate: predicate, outputs: spec.Outputs}
	result.constants = n.outputConstants(id, source, spec.Outputs)
	if len(spec.Inputs) != 0 {
		result.input = spec.Inputs[0]
	}
	n.relActive[id] = false
	n.rels[id] = result
	return result
}

func (n *normalizer) scalarList(count uint32, get func(uint32) ir.ScalarID, depth uint32) []ir.ScalarID {
	result := allocate[ir.ScalarID](n, uint64(count))
	for i := range result {
		result[i] = n.scalar(get(uint32(i)), depth+1).id
	}
	return result
}

// relationScalarEffects returns base widened by the effects of every scalar the
// normalized relation references. Verification requires a relation's declared
// Effects to cover everything ir.Graph.relationEffects collects from it
// (internal/query/ir/effects.go, checked in verify_rel.go). A relation's effects
// are bound once, against the BOUND scalars; normalization then rewrites those
// scalars and can RAISE their effects -- lowering a quantified predicate builds
// a ScalarApply, which ir.relationEffects reports as MayError. Without folding
// the rewritten scalars back in, the declaration goes stale and verification
// rejects the graph.
//
// Scalars are read through the builder, not the normalizer's memo: spec's IDs
// are freshly published builder IDs, and the memo is keyed by the SOURCE graph's
// IDs. Reading the memo would silently see a zero value.
//
// The relation's own structural contributions (Modify/LockRows volatility,
// ScalarApply MayError, Limit and table-function error) are already present in
// base, which starts from the bound relation's metadata, so only the scalar
// references need collecting here. If ir.relationEffects gains a new scalar
// source, this must gain it too.
func (n *normalizer) relationScalarEffects(spec ir.RelSpec, base ir.Effects) ir.Effects {
	effects := base
	union := func(id ir.ScalarID) {
		if id == 0 {
			return
		}
		if scalar, ok := n.b.Scalar(id); ok {
			effects = effects.Union(scalar.Effects)
		}
	}
	for _, id := range spec.Expressions {
		union(id)
	}
	for _, id := range spec.Partition {
		union(id)
	}
	for _, binding := range spec.Bindings {
		union(binding.Expr)
	}
	for _, binding := range spec.ConflictAssignments {
		union(binding.Expr)
	}
	for _, key := range spec.Order {
		union(key.Expr)
	}
	union(spec.Predicate)
	union(spec.ConflictPredicate)
	union(spec.Count)
	union(spec.Offset)
	union(spec.RecursionSpec.CycleValue)
	union(spec.RecursionSpec.CycleDefault)
	union(spec.RecursionSpec.CycleContinue)
	for _, id := range []ir.SignatureID{spec.Frame.Start.InRange, spec.Frame.End.InRange} {
		if signature, ok := n.g.Signature(id); ok {
			effects = effects.Union(signature.Effects)
		}
	}
	for i := range spec.Actions {
		union(spec.Actions[i].Condition)
		for _, binding := range spec.Actions[i].Assignments {
			union(binding.Expr)
		}
	}
	for i := range spec.ViewMutationSteps {
		union(spec.ViewMutationSteps[i].Predicate)
		for _, id := range spec.ViewMutationSteps[i].Projections {
			union(id)
		}
	}
	return effects
}

func (n *normalizer) bindings(count uint32, get func(uint32) ir.Binding, depth uint32) []ir.Binding {
	result := allocate[ir.Binding](n, uint64(count))
	for i := range result {
		result[i] = get(uint32(i))
		result[i].Expr = n.scalar(result[i].Expr, depth+1).id
	}
	return result
}

func (n *normalizer) orders(count uint32, get func(uint32) ir.OrderKey, depth uint32) []ir.OrderKey {
	result := allocate[ir.OrderKey](n, uint64(count))
	for i := range result {
		result[i] = get(uint32(i))
		result[i].Expr = n.scalar(result[i].Expr, depth+1).id
	}
	return result
}

func (n *normalizer) identityProject(id ir.RelID, spec ir.RelSpec, input relResult) bool {
	if spec.Kind != ir.Project || spec.SecurityBarrier || len(spec.Inputs) != 1 || spec.Scope != input.n.Scope {
		return false
	}
	if spec.Effects != input.n.Effects || !slices.Equal(spec.Outputs, input.outputs) || len(spec.Bindings) != len(spec.Outputs) {
		return false
	}
	for i, binding := range spec.Bindings {
		if !n.step(1) {
			return false
		}
		old, _ := n.g.Binding(id, uint32(i))
		scalar := n.scalars[old.Expr]
		if binding.Slot != spec.Outputs[i] || scalar.n.Kind != ir.Slot || scalar.n.Slot != binding.Slot {
			return false
		}
	}
	return true
}
