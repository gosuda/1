package opt

import (
	"context"
	"errors"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func prepareGraph(ctx context.Context, graph *ir.Graph, options Options) (*ir.Graph, error) {
	if graph.CTECount() == 0 {
		return graph, nil
	}
	// Reserve source-sized bookkeeping before following definition roots.
	if uint64(graph.CTECount())*16+uint64(graph.RelCount())*2+32 > options.MaxBytes {
		return nil, ErrLimit
	}
	used := make([]bool, uint64(graph.CTECount())+1)
	producers := make([]bool, uint64(graph.CTECount())+1)
	seen := make([]bool, uint64(graph.RelCount())+1)
	work := uint64(0)
	var visit func(ir.RelID, uint32) error
	var visitScalar func(ir.ScalarID, uint32) error
	visitScalar = func(id ir.ScalarID, depth uint32) error {
		if id == 0 {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth >= options.IR.MaxDepth || work >= options.MaxWork {
			return ErrLimit
		}
		work++
		s, _ := graph.Scalar(id)
		if s.Kind == ir.Subquery {
			if err := visit(s.Relation, depth+1); err != nil {
				return err
			}
		}
		for i := range s.OperandCount() {
			child, _ := graph.Operand(id, i)
			if err := visitScalar(child, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	visit = func(id ir.RelID, depth uint32) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if depth >= options.IR.MaxDepth || work == options.MaxWork {
			return ErrLimit
		}
		work++
		if seen[id] {
			return nil
		}
		seen[id] = true
		r, _ := graph.Rel(id)
		if r.Kind == ir.CTEConsumer && !r.RecursiveReference {
			used[r.CTE] = true
			cte, _ := graph.CTE(r.CTE)
			if err := visit(cte.Body, depth+1); err != nil {
				return err
			}
		}
		for i := range r.InputCount() {
			child, _ := graph.Input(id, i)
			if err := visit(child, depth+1); err != nil {
				return err
			}
		}
		var scalarErr error
		walker := optimizer{graph: graph}
		walker.relationScalars(&r, id, func(scalar ir.ScalarID) {
			if scalarErr == nil {
				scalarErr = visitScalar(scalar, depth+1)
			}
		})
		if scalarErr != nil {
			return scalarErr
		}
		return nil
	}
	if err := visit(graph.Root(), 0); err != nil {
		return nil, err
	}
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r, _ := graph.Rel(id)
		if r.Kind == ir.CTEProducer {
			producers[r.CTE] = true
		}
	}
	for id := ir.CTEID(1); id <= ir.CTEID(graph.CTECount()); id++ {
		cte, _ := graph.CTE(id)
		body, _ := graph.Rel(cte.Body)
		if body.Kind == ir.Modify {
			used[id] = true
			if err := visit(cte.Body, 0); err != nil {
				return nil, err
			}
		}
	}
	needsCopy := false
	for id := ir.CTEID(1); id <= ir.CTEID(graph.CTECount()); id++ {
		needsCopy = needsCopy || used[id] && !producers[id]
	}
	if !needsCopy {
		return graph, nil
	}
	if options.IR.MaxBytes > options.MaxBytes/2 {
		return nil, ErrLimit
	}
	builder, err := ir.Extend(ctx, graph, options.IR)
	if err != nil {
		return nil, err
	}
	for id := ir.CTEID(1); id <= ir.CTEID(graph.CTECount()); id++ {
		if !used[id] || producers[id] {
			continue
		}
		cte, _ := graph.CTE(id)
		body, _ := graph.Rel(cte.Body)
		if body.OutputCount() > options.MaxOutputColumns {
			return nil, ErrLimit
		}
		outputs := make([]model.SlotID, body.OutputCount())
		for i := range outputs {
			outputs[i], _ = graph.Output(cte.Body, uint32(i))
		}
		_, err := builder.AddRel(ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEProducer, Scope: body.Scope, CTE: id, Effects: cte.Effects}, Inputs: []ir.RelID{cte.Body}, Outputs: outputs})
		if err != nil {
			return nil, err
		}
	}
	return builder.Build(ctx, graph.Root())
}

// joinKeyCommutator reports whether scalarID is a reversed hash/merge-joinable
// slot equality (LHS in rightSlots, RHS in leftSlots) and returns the commuted
// operator OID when reversible.
func joinKeyCommutator(graph *ir.Graph, scalarID ir.ScalarID, leftSlots, rightSlots []model.SlotID) (model.OperatorID, bool) {
	s, ok := graph.Scalar(scalarID)
	if !ok || s.Kind != ir.Call || s.OperandCount() != 2 || !s.Strict || s.Effects.Volatility != ir.Immutable || s.Effects.MayError || s.Operator == 0 {
		return 0, false
	}
	op, ok := builtin.LookupOperator(s.Operator)
	if !ok || (!op.HashJoinable && !op.MergeJoinable) || op.Commutator == 0 {
		return 0, false
	}
	lhsID, _ := graph.Operand(scalarID, 0)
	rhsID, _ := graph.Operand(scalarID, 1)
	lhs, _ := graph.Scalar(lhsID)
	rhs, _ := graph.Scalar(rhsID)
	if lhs.Kind != ir.Slot || rhs.Kind != ir.Slot {
		return 0, false
	}
	if !slices.Contains(rightSlots, lhs.Slot) || !slices.Contains(leftSlots, rhs.Slot) {
		return 0, false
	}
	if slices.Contains(leftSlots, lhs.Slot) && slices.Contains(rightSlots, rhs.Slot) {
		return 0, false
	}
	lType, lOK := graph.Type(lhs.Layout)
	rType, rOK := graph.Type(rhs.Layout)
	if !lOK || !rOK || lType != rType || lType.Collation != rType.Collation {
		return 0, false
	}
	comm, ok := builtin.LookupOperator(op.Commutator)
	if !ok || comm.Left != rType.ID || comm.Right != lType.ID || comm.Result != 16 || (!comm.HashJoinable && !comm.MergeJoinable) {
		return 0, false
	}
	return comm.ID, true
}

func hasReversedJoinKey(graph *ir.Graph, scalarID ir.ScalarID, leftSlots, rightSlots []model.SlotID, maxDepth, depth uint32) bool {
	if scalarID == 0 || depth >= maxDepth {
		return false
	}
	s, ok := graph.Scalar(scalarID)
	if !ok {
		return false
	}
	if s.Kind == ir.Bool && s.BoolOp == ir.And {
		for i := range s.OperandCount() {
			child, _ := graph.Operand(scalarID, i)
			if hasReversedJoinKey(graph, child, leftSlots, rightSlots, maxDepth, depth+1) {
				return true
			}
		}
		return false
	}
	_, reversed := joinKeyCommutator(graph, scalarID, leftSlots, rightSlots)
	return reversed
}

func relOutputSlots(graph *ir.Graph, id ir.RelID) []model.SlotID {
	r, _ := graph.Rel(id)
	slots := make([]model.SlotID, r.OutputCount())
	for i := range slots {
		slots[i], _ = graph.Output(id, uint32(i))
	}
	return slots
}

// commuteJoinPredicate rewrites top-level AND conjuncts in id whose equality
// operands are reversed relative to (leftSlots, rightSlots), returning the
// normalized scalar ID in b.
func commuteJoinPredicate(ctx context.Context, graph *ir.Graph, b *ir.Builder, id ir.ScalarID, leftSlots, rightSlots []model.SlotID, maxDepth, depth uint32) (ir.ScalarID, bool, error) {
	if id == 0 || depth >= maxDepth {
		return id, false, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, false, err
	}
	s, ok := graph.Scalar(id)
	if !ok {
		return id, false, nil
	}
	if s.Kind == ir.Bool && s.BoolOp == ir.And {
		operands := make([]ir.ScalarID, s.OperandCount())
		changed := false
		for i := range operands {
			child, _ := graph.Operand(id, uint32(i))
			updated, childChanged, err := commuteJoinPredicate(ctx, graph, b, child, leftSlots, rightSlots, maxDepth, depth+1)
			if err != nil {
				return 0, false, err
			}
			operands[i] = updated
			changed = changed || childChanged
		}
		if !changed {
			return id, false, nil
		}
		next, err := b.AddScalar(ctx, ir.ScalarSpec{Scalar: s.Metadata(), Operands: operands})
		return next, true, err
	}
	comm, reversed := joinKeyCommutator(graph, id, leftSlots, rightSlots)
	if !reversed {
		return id, false, nil
	}
	sig, ok := graph.Signature(s.Signature)
	if !ok {
		return id, false, nil
	}
	lhsID, _ := graph.Operand(id, 0)
	rhsID, _ := graph.Operand(id, 1)
	lhs, _ := graph.Scalar(lhsID)
	rhs, _ := graph.Scalar(rhsID)
	sigMeta := sig.Metadata()
	sigMeta.Operator = comm
	if sigMeta.Function != 0 {
		commOp, _ := builtin.LookupOperator(comm)
		sigMeta.Function = commOp.Function
	}
	newSig, err := b.AddSignature(ctx, ir.SignatureSpec{Signature: sigMeta, Parameters: []ir.LayoutID{rhs.Layout, lhs.Layout}})
	if err != nil {
		return 0, false, err
	}
	callMeta := s.Metadata()
	callMeta.Operator = comm
	callMeta.Function = sigMeta.Function
	callMeta.Signature = newSig
	next, err := b.AddScalar(ctx, ir.ScalarSpec{Scalar: callMeta, Operands: []ir.ScalarID{rhsID, lhsID}})
	return next, true, err
}

// normalizeJoinPredicates commutes reversed equality predicates (e.g. b.id = a.id
// on a JOIN b) into canonical (left, right) operand order so keyed physical joins
// satisfy physical.Plan validation without modifying the original graph.
func normalizeJoinPredicates(ctx context.Context, graph *ir.Graph, options Options) (*ir.Graph, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seen := make([]bool, uint64(graph.RelCount())+1)
	needsRewrite := false
	var scanRel func(ir.RelID, uint32)
	var scanScalar func(ir.ScalarID, uint32)
	scanScalar = func(id ir.ScalarID, depth uint32) {
		if id == 0 || needsRewrite || depth >= options.IR.MaxDepth {
			return
		}
		s, _ := graph.Scalar(id)
		if s.Kind == ir.Subquery {
			scanRel(s.Relation, depth+1)
		}
		for i := range s.OperandCount() {
			child, _ := graph.Operand(id, i)
			scanScalar(child, depth+1)
		}
	}
	scanRel = func(id ir.RelID, depth uint32) {
		if id == 0 || needsRewrite || seen[id] || depth >= options.IR.MaxDepth {
			return
		}
		seen[id] = true
		r, _ := graph.Rel(id)
		if r.Kind == ir.Join && r.Predicate != 0 && r.JoinKind != ir.CrossJoin && r.JoinKind != ir.MarkJoin && r.InputCount() == 2 && r.Effects.Volatility == ir.Immutable && !r.Effects.MayError {
			leftID, _ := graph.Input(id, 0)
			rightID, _ := graph.Input(id, 1)
			if hasReversedJoinKey(graph, r.Predicate, relOutputSlots(graph, leftID), relOutputSlots(graph, rightID), options.IR.MaxDepth, 0) {
				needsRewrite = true
				return
			}
		}
		for i := range r.InputCount() {
			child, _ := graph.Input(id, i)
			scanRel(child, depth+1)
		}
		walker := optimizer{graph: graph}
		walker.relationScalars(&r, id, func(scalar ir.ScalarID) { scanScalar(scalar, depth+1) })
	}
	scanRel(graph.Root(), 0)
	if !needsRewrite {
		return graph, nil
	}
	if options.IR.MaxBytes > options.MaxBytes/2 {
		return graph, nil
	}
	b, err := ir.Extend(ctx, graph, options.IR)
	if err != nil {
		if errors.Is(err, ir.ErrLimit) {
			return graph, nil
		}
		return nil, err
	}
	relMemo := make([]ir.RelID, uint64(graph.RelCount())+1)
	scalarMemo := make([]ir.ScalarID, uint64(graph.ScalarCount())+1)
	var remapRel func(ir.RelID, uint32) (ir.RelID, error)
	var remapScalar func(ir.ScalarID, uint32) (ir.ScalarID, error)
	remapScalar = func(id ir.ScalarID, depth uint32) (ir.ScalarID, error) {
		if id == 0 {
			return 0, nil
		}
		if mapped := scalarMemo[id]; mapped != 0 {
			return mapped, nil
		}
		if depth >= options.IR.MaxDepth {
			return 0, ir.ErrLimit
		}
		s, _ := graph.Scalar(id)
		changed := false
		var newRel ir.RelID
		if s.Kind == ir.Subquery {
			var err error
			newRel, err = remapRel(s.Relation, depth+1)
			if err != nil {
				return 0, err
			}
			changed = newRel != s.Relation
		}
		operands := make([]ir.ScalarID, s.OperandCount())
		for i := range operands {
			child, _ := graph.Operand(id, uint32(i))
			updated, err := remapScalar(child, depth+1)
			if err != nil {
				return 0, err
			}
			operands[i] = updated
			changed = changed || updated != child
		}
		if !changed {
			scalarMemo[id] = id
			return id, nil
		}
		spec, err := graph.CloneScalarSpec(ctx, id, options.IR.MaxBytes)
		if err != nil {
			return 0, err
		}
		if s.Kind == ir.Subquery {
			spec.Relation = newRel
		}
		spec.Operands = operands
		next, err := b.AddScalar(ctx, spec)
		if err != nil {
			return 0, err
		}
		scalarMemo[id] = next
		return next, nil
	}
	remapRel = func(id ir.RelID, depth uint32) (ir.RelID, error) {
		if id == 0 {
			return 0, nil
		}
		if mapped := relMemo[id]; mapped != 0 {
			return mapped, nil
		}
		if depth >= options.IR.MaxDepth {
			return 0, ir.ErrLimit
		}
		spec, err := graph.CloneRelSpec(ctx, id, options.IR.MaxBytes)
		if err != nil {
			return 0, err
		}
		changed := false
		for i, input := range spec.Inputs {
			updated, err := remapRel(input, depth+1)
			if err != nil {
				return 0, err
			}
			if updated != input {
				spec.Inputs[i] = updated
				changed = true
			}
		}
		remapField := func(target *ir.ScalarID) error {
			if *target == 0 {
				return nil
			}
			updated, err := remapScalar(*target, depth+1)
			if err != nil {
				return err
			}
			if updated != *target {
				*target = updated
				changed = true
			}
			return nil
		}
		if spec.Kind == ir.Join && spec.Predicate != 0 && spec.JoinKind != ir.CrossJoin && spec.JoinKind != ir.MarkJoin && len(spec.Inputs) == 2 && spec.Effects.Volatility == ir.Immutable && !spec.Effects.MayError {
			origLeft, _ := graph.Input(id, 0)
			origRight, _ := graph.Input(id, 1)
			leftSlots, rightSlots := relOutputSlots(graph, origLeft), relOutputSlots(graph, origRight)
			var remapJoinPred func(ir.ScalarID, uint32) (ir.ScalarID, bool, error)
			remapJoinPred = func(predID ir.ScalarID, d uint32) (ir.ScalarID, bool, error) {
				if predID == 0 || d >= options.IR.MaxDepth {
					return predID, false, nil
				}
				ps, ok := graph.Scalar(predID)
				if !ok {
					return predID, false, nil
				}
				if ps.Kind == ir.Bool && ps.BoolOp == ir.And {
					ops := make([]ir.ScalarID, ps.OperandCount())
					anyChanged := false
					for i := range ops {
						child, _ := graph.Operand(predID, uint32(i))
						u, ch, err := remapJoinPred(child, d+1)
						if err != nil {
							return 0, false, err
						}
						ops[i] = u
						anyChanged = anyChanged || ch
					}
					if !anyChanged {
						return predID, false, nil
					}
					next, err := b.AddScalar(ctx, ir.ScalarSpec{Scalar: ps.Metadata(), Operands: ops})
					return next, true, err
				}
				if _, reversed := joinKeyCommutator(graph, predID, leftSlots, rightSlots); reversed {
					return commuteJoinPredicate(ctx, graph, b, predID, leftSlots, rightSlots, options.IR.MaxDepth, d)
				}
				remapped, err := remapScalar(predID, d+1)
				return remapped, remapped != predID, err
			}
			origRel, _ := graph.Rel(id)
			updated, predChanged, err := remapJoinPred(origRel.Predicate, 0)
			if err != nil {
				return 0, err
			}
			if predChanged {
				spec.Predicate = updated
				changed = true
			}
		} else if err := remapField(&spec.Predicate); err != nil {
			return 0, err
		}
		for _, field := range [...]*ir.ScalarID{&spec.Offset, &spec.Count, &spec.ConflictPredicate, &spec.RLSInsertCheck, &spec.RLSUpdateCheck, &spec.Frame.Start.Offset, &spec.Frame.End.Offset, &spec.Recursion.CycleValue, &spec.Recursion.CycleDefault, &spec.Recursion.CycleContinue} {
			if err := remapField(field); err != nil {
				return 0, err
			}
		}
		for i := range spec.Expressions {
			if err := remapField(&spec.Expressions[i]); err != nil {
				return 0, err
			}
		}
		for i := range spec.Bindings {
			if err := remapField(&spec.Bindings[i].Expr); err != nil {
				return 0, err
			}
		}
		for i := range spec.Assignments {
			if err := remapField(&spec.Assignments[i].Expr); err != nil {
				return 0, err
			}
		}
		for i := range spec.ConflictAssignments {
			if err := remapField(&spec.ConflictAssignments[i].Expr); err != nil {
				return 0, err
			}
		}
		for i := range spec.Order {
			if err := remapField(&spec.Order[i].Expr); err != nil {
				return 0, err
			}
		}
		for i := range spec.Partition {
			if err := remapField(&spec.Partition[i]); err != nil {
				return 0, err
			}
		}
		for i := range spec.Aggregates {
			if err := remapField(&spec.Aggregates[i].Filter); err != nil {
				return 0, err
			}
			for j := range spec.Aggregates[i].Args {
				if err := remapField(&spec.Aggregates[i].Args[j]); err != nil {
					return 0, err
				}
			}
			for j := range spec.Aggregates[i].Direct {
				if err := remapField(&spec.Aggregates[i].Direct[j]); err != nil {
					return 0, err
				}
			}
			for j := range spec.Aggregates[i].Order {
				if err := remapField(&spec.Aggregates[i].Order[j].Expr); err != nil {
					return 0, err
				}
			}
		}
		for i := range spec.Windows {
			if err := remapField(&spec.Windows[i].Filter); err != nil {
				return 0, err
			}
			for j := range spec.Windows[i].Args {
				if err := remapField(&spec.Windows[i].Args[j]); err != nil {
					return 0, err
				}
			}
		}
		for i := range spec.Actions {
			if err := remapField(&spec.Actions[i].Condition); err != nil {
				return 0, err
			}
			for j := range spec.Actions[i].Assignments {
				if err := remapField(&spec.Actions[i].Assignments[j].Expr); err != nil {
					return 0, err
				}
			}
		}
		for i := range spec.ViewMutationSteps {
			for j := range spec.ViewMutationSteps[i].Projections {
				if err := remapField(&spec.ViewMutationSteps[i].Projections[j]); err != nil {
					return 0, err
				}
			}
		}
		if !changed {
			relMemo[id] = id
			return id, nil
		}
		next, err := b.AddRel(ctx, spec)
		if err != nil {
			return 0, err
		}
		relMemo[id] = next
		return next, nil
	}
	newRoot, err := remapRel(graph.Root(), 0)
	if err != nil {
		if errors.Is(err, ir.ErrLimit) {
			return graph, nil
		}
		return nil, err
	}
	rebuilt, err := b.Build(ctx, newRoot)
	if err != nil {
		if errors.Is(err, ir.ErrLimit) {
			return graph, nil
		}
		return nil, err
	}
	return rebuilt, nil
}

func (o *optimizer) relationScalars(n *ir.Rel, id ir.RelID, add func(ir.ScalarID)) {
	for _, scalar := range [...]ir.ScalarID{n.Predicate, n.Offset, n.Count} {
		add(scalar)
	}
	for i := range n.ExpressionCount() {
		scalar, _ := o.graph.Expression(id, i)
		add(scalar)
	}
	for i := range n.BindingCount() {
		binding, _ := o.graph.Binding(id, i)
		add(binding.Expr)
	}
	for i := range n.OrderCount() {
		key, _ := o.graph.Order(id, i)
		add(key.Expr)
	}
	for i := range n.PartitionCount() {
		scalar, _ := o.graph.Partition(id, i)
		add(scalar)
	}
	for i := range n.AggregateCount() {
		call, _ := o.graph.Aggregate(id, i)
		add(call.Filter)
		for j := range call.ArgumentCount() {
			scalar, _ := o.graph.AggregateArgument(id, i, j)
			add(scalar)
		}
		for j := range call.OrderCount() {
			order, _ := o.graph.AggregateOrder(id, i, j)
			add(order.Expr)
		}
	}
	for i := range n.WindowCount() {
		call, _ := o.graph.Window(id, i)
		add(call.Filter)
		for j := range call.ArgumentCount() {
			scalar, _ := o.graph.WindowArgument(id, i, j)
			add(scalar)
		}
	}
	for i := range n.AssignmentCount() {
		binding, _ := o.graph.Assignment(id, i)
		add(binding.Expr)
	}
	for i := range n.ConflictAssignmentCount() {
		binding, _ := o.graph.ConflictAssignment(id, i)
		add(binding.Expr)
	}
	for i := range n.ActionCount() {
		action, _ := o.graph.MergeAction(id, i)
		add(action.Condition)
		for j := range action.AssignmentCount() {
			binding, _ := o.graph.MergeAssignment(id, i, j)
			add(binding.Expr)
		}
	}
	add(n.Frame.Start.Offset)
	add(n.Frame.End.Offset)
	add(n.Recursion.CycleValue)
	add(n.Recursion.CycleDefault)
	add(n.Recursion.CycleContinue)
	add(n.ConflictPredicate)
	add(n.RLSInsertCheck)
	add(n.RLSUpdateCheck)
}
