package opt

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
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
}
