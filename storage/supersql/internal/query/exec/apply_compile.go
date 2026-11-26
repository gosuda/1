package exec

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exapply "github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type preparedNode struct {
	node                physical.NodeSpec
	layouts             []model.TypeLayout
	domain              physical.NodeID
	parentDomain        physical.NodeID
	recursion           physical.NodeID
	assigned            bool
	scalarOwner         physical.NodeID
	queryScalar         bool
	scalarBody          bool
	subqueryBody        bool
	captureLayouts      []model.TypeLayout
	captureColumns      []captureColumn
	captureSlots        []physical.Slot
	programs            []*expr.Program
	residualLayouts     []model.TypeLayout
	aggregateProjection *preparedAggregateProjection
	returningSources    []int
	groupingSets        [][]uint32
	groupingMasks       [][]uint32
}

type preparedPlan struct{ nodes []preparedNode }

func (c *invocation) root() physical.NodeID {
	if c.domain != 0 {
		return c.domain
	}
	return c.plan.Root()
}

func (c *invocation) producer(id physical.NodeID) *sharedProducer {
	for owner := c; owner != nil; owner = owner.parent {
		if producer := owner.shared[id]; producer != nil {
			return producer
		}
	}
	return nil
}

func (c *invocation) prepare() (err error) {
	p := c.portal
	claim, err := p.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return err
	}
	p.claims = append(p.claims, claim)
	if err = c.charge(c.plan.OwnedBytes() + uint64(c.plan.NodeCount()+1)*uint64(unsafe.Sizeof(preparedNode{}))); err != nil {
		return err
	}
	c.prepared = &preparedPlan{nodes: make([]preparedNode, c.plan.NodeCount()+1)}
	for id := physical.NodeID(1); id <= physical.NodeID(c.plan.NodeCount()); id++ {
		node, err := c.plan.CloneNode(p.ctx, id, p.env.Limits.MetadataBytes)
		if err != nil {
			return err
		}
		layouts, err := c.layouts(node.Output)
		if err != nil {
			return err
		}
		c.prepared.nodes[id].node, c.prepared.nodes[id].layouts = node, layouts
	}
	if err = c.assign(c.plan.Root(), 0); err != nil {
		return err
	}
	for id := physical.NodeID(1); id <= physical.NodeID(c.plan.NodeCount()); id++ {
		body := &c.prepared.nodes[id]
		if !body.scalarBody && !body.subqueryBody {
			continue
		}
		if body.scalarBody && len(body.captureSlots) == 0 && (!validSpoolLimits(p.env.Spool) || p.env.Spool.MaxRows < 2) {
			return ErrInvalid
		}
		body.captureLayouts, err = c.layouts(body.captureSlots)
		if err != nil {
			return err
		}
		if err = c.charge(uint64(len(body.captureSlots)) * uint64(unsafe.Sizeof(captureColumn{}))); err != nil {
			return err
		}
		body.captureColumns = make([]captureColumn, len(body.captureSlots))
		for i := range body.captureColumns {
			body.captureColumns[i] = captureColumn{left: true, index: i}
		}
	}
	c.preparing = true
	defer func() { c.preparing = false; c.current, c.programIndex, c.domain, c.used = 0, 0, 0, 0 }()
	for id := physical.NodeID(1); id <= physical.NodeID(c.plan.NodeCount()); id++ {
		n := &c.prepared.nodes[id]
		if !n.assigned {
			return ErrInvalid
		}
		c.current, c.programIndex, c.domain = id, 0, n.domain
		for _, slot := range n.node.Delivered.Parameters {
			if n.domain == 0 || joinColumn(c.prepared.nodes[n.domain].captureSlots, slot) < 0 {
				return &RequiredExecutionError{Node: id, Algorithm: n.node.Algorithm, Reason: "capture is not bound by an enclosing Apply"}
			}
		}
		if n.node.Algorithm == physical.CTEMaterialize || n.node.Algorithm == physical.CTEScan {
			continue
		}
		if err = c.charge(uint64(len(n.node.Children)) * uint64(unsafe.Sizeof(storage.BatchCursor(nil)))); err != nil {
			return err
		}
		children := make([]storage.BatchCursor, len(n.node.Children))
		_, err = c.node(id, n.node, n.layouts, children)
		if err != nil {
			return err
		}
	}
	if err = c.prepareQueryScalars(); err != nil {
		return err
	}
	return claim.Resize(c.used)
}

func (c *invocation) assign(id, domain physical.NodeID) error {
	if err := c.portal.ctx.Err(); err != nil {
		return err
	}
	n := &c.prepared.nodes[id]
	if n.assigned {
		if n.domain != domain {
			return &RequiredExecutionError{Node: id, Algorithm: n.node.Algorithm, Reason: "shared relation crosses Apply invocation scopes"}
		}
		return nil
	}
	n.assigned, n.domain = true, domain
	for _, dependency := range n.node.StatementDependencies {
		if err := c.assign(dependency, 0); err != nil {
			return err
		}
	}
	for i, child := range n.node.Children {
		childDomain := domain
		if (n.node.Algorithm == physical.Apply || n.node.Algorithm == physical.RecursiveUnion) && i == 1 {
			childDomain = child
			if err := c.bindInvocationDomain(id, child, domain); err != nil {
				return err
			}
		}
		if err := c.assign(child, childDomain); err != nil {
			return err
		}
	}
	for _, subplan := range n.node.ScalarSubplans {
		body := &c.prepared.nodes[subplan.Root]
		body.captureSlots = body.node.Delivered.Parameters
		body.parentDomain = domain
		graph, _ := c.plan.Graph(subplan.Scalar.Graph)
		scalar, _ := graph.Scalar(subplan.Scalar.Scalar)
		body.scalarBody = scalar.SubqueryKind == ir.ScalarSubquery || scalar.SubqueryKind == ir.RowSubquery
		body.subqueryBody = true
		if err := c.assign(subplan.Root, subplan.Root); err != nil {
			return err
		}
	}
	if n.node.Algorithm == physical.CTEScan {
		producer := n.node.Worktable.Producer
		producerDomain, err := c.producerDomain(producer, domain)
		if err != nil {
			return err
		}
		if err := c.assign(producer, producerDomain); err != nil {
			return err
		}
	}
	return nil
}

func (c *invocation) bindInvocationDomain(id, child, domain physical.NodeID) error {
	node := c.prepared.nodes[id].node
	right := &c.prepared.nodes[child]
	right.captureSlots = right.node.Delivered.Parameters
	right.parentDomain = domain
	if node.Algorithm == physical.Apply {
		graph, _ := c.plan.Graph(node.Logical.Graph)
		relation, _ := graph.Rel(node.Logical.Rel)
		right.scalarBody = relation.ApplyKind == ir.ScalarApply
	}
	outer := c.prepared.nodes[domain].captureSlots
	if node.Algorithm == physical.RecursiveUnion {
		right.recursion = id
		for _, slot := range right.captureSlots {
			if joinColumn(outer, slot) < 0 {
				return &RequiredExecutionError{Node: id, Algorithm: physical.RecursiveUnion, Reason: "recursive capture has no enclosing binding"}
			}
		}
		right.captureSlots = outer
		return nil
	}
	left := c.prepared.nodes[node.Children[0]].node.Output
	for _, slot := range right.captureSlots {
		if joinColumn(left, slot) < 0 && joinColumn(outer, slot) < 0 {
			return &RequiredExecutionError{Node: id, Algorithm: physical.Apply, Reason: "right capture has no enclosing binding"}
		}
	}
	return nil
}

func (c *invocation) producerDomain(producer, domain physical.NodeID) (physical.NodeID, error) {
	root := c.prepared.nodes[c.plan.Root()].node
	for _, dependency := range root.StatementDependencies {
		if dependency == producer {
			return 0, nil
		}
	}
	node := c.prepared.nodes[producer].node
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	for domain != 0 {
		root := &c.prepared.nodes[domain]
		if root.recursion != 0 {
			dependent, err := c.referencesWorktable(producer, root.recursion)
			if err != nil {
				return 0, err
			}
			if dependent {
				return domain, nil
			}
			domain = root.parentDomain
			continue
		}
		if root.node.Logical.Graph == node.Logical.Graph {
			invocation, _ := graph.Rel(root.node.Logical.Rel)
			for scope := relation.Scope; scope != 0; {
				if scope == invocation.Scope {
					return domain, nil
				}
				scope, _ = graph.ScopeParent(scope)
			}
		}
		domain = root.parentDomain
	}
	return 0, nil
}

func (c *invocation) state(program *expr.Program) (*expr.State, error) {
	if c.preparing {
		return nil, nil
	}
	p := c.portal
	return expr.NewState(program, expr.StateOptions{WorkAllocator: p.env.Query.Allocator(runtime.WorkMemory), OutputAllocator: p.env.Query.Allocator(runtime.OutputMemory), Captures: c.captures, Subqueries: c, Sequences: p.env.Sequences, Relations: p.env.Relations, Temporal: p.env.Temporal, Settings: p.env.Settings, TextSearch: p.env.TextSearch, Advisory: p.env.Advisory, Native: p.env.Native})
}

func (c *invocation) apply(id physical.NodeID, node physical.NodeSpec, layouts []model.TypeLayout, left storage.BatchCursor) (storage.BatchCursor, error) {
	p := c.portal
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	leftNode, rightNode := &c.prepared.nodes[node.Children[0]], &c.prepared.nodes[node.Children[1]]
	once := relation.ApplyKind == ir.ScalarApply && len(rightNode.captureSlots) == 0
	if c.preparing {
		if once && (!validSpoolLimits(p.env.Spool) || p.env.Spool.MaxRows < 2) {
			return nil, ErrInvalid
		}
		return nil, nil
	}
	options := exapply.Options{Kind: relation.ApplyKind, Outer: relation.LateralOuter, LeftLayouts: leftNode.layouts, RightLayouts: rightNode.layouts, OutputLayouts: layouts,
		Query: p.env.Query, BatchRows: p.env.Limits.BatchRows, BatchBytes: p.env.Limits.BatchBytes, MaxRowBytes: p.env.Apply.MaxRowBytes, WorkBytes: p.env.Apply.WorkBytes}
	if err := c.charge(uint64(len(layouts))*uint64(unsafe.Sizeof(exapply.Column{})) + uint64(len(rightNode.captureSlots))*uint64(unsafe.Sizeof(captureColumn{}))); err != nil {
		return nil, err
	}
	options.Output = make([]exapply.Column, len(layouts))
	for i := range options.Output {
		if i < len(leftNode.layouts) {
			options.Output[i] = exapply.Column{Side: exapply.Left, Index: i}
		} else if relation.ApplyKind == ir.ExistsApply {
			options.Output[i] = exapply.Column{Side: exapply.Mark}
		} else {
			options.Output[i] = exapply.Column{Side: exapply.Right, Index: i - len(leftNode.layouts)}
		}
	}
	if rightNode.queryScalar {
		factory, err := c.scalarFactory(node.Children[1])
		if err != nil {
			return nil, err
		}
		options.Factory = factory
		return exapply.New(p.ctx, left, options)
	}
	if once {
		return nil, ErrInvalid
	}
	captureLayouts, err := c.layouts(rightNode.captureSlots)
	if err != nil {
		return nil, err
	}
	factory := &applyFactory{owner: c, root: node.Children[1], layouts: captureLayouts, columns: make([]captureColumn, len(rightNode.captureSlots))}
	for i, slot := range rightNode.captureSlots {
		column := joinColumn(leftNode.node.Output, slot)
		factory.columns[i] = captureColumn{left: column >= 0, index: column}
		if column < 0 {
			factory.columns[i].index = joinColumn(c.prepared.nodes[c.domain].captureSlots, slot)
		}
	}
	options.Factory = factory
	return exapply.New(p.ctx, left, options)
}

type captureColumn struct {
	left  bool
	index int
}
type applyFactory struct {
	owner   *invocation
	root    physical.NodeID
	layouts []model.TypeLayout
	columns []captureColumn
}

func (f *applyFactory) Open(ctx context.Context, row []model.Value) (_ storage.BatchCursor, err error) {
	parent := f.owner
	p := &Portal{env: parent.portal.env, borrowed: true}
	p.env.row = nil
	p.ctx, p.cancel = context.WithCancel(ctx)
	defer func() {
		if err != nil {
			err = errors.Join(err, p.Close())
		}
	}()
	inner := &invocation{portal: p, graph: parent.graph, plan: parent.plan, prepared: parent.prepared, domain: f.root, parent: parent}
	if len(f.columns) > 0 {
		builder, err := columnar.NewBuilder(f.layouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: p.env.Limits.BatchBytes, Allocator: p.env.Query.Allocator(runtime.WorkMemory)})
		if err != nil {
			return nil, err
		}
		p.owned = append(p.owned, builder)
		claim, err := p.env.Query.Allocator(runtime.WorkMemory).Reserve(uint64(len(f.columns)) * (uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(columnar.Vector{}))))
		if err != nil {
			return nil, err
		}
		p.claims = append(p.claims, claim)
		values := make([]model.Value, len(f.columns))
		for i, column := range f.columns {
			if column.index < 0 {
				return nil, ErrInvalid
			}
			if column.left {
				if column.index >= len(row) {
					return nil, ErrInvalid
				}
				values[i] = row[column.index]
			} else {
				if column.index >= len(parent.captureValues) {
					return nil, ErrInvalid
				}
				values[i] = parent.captureValues[column.index]
			}
		}
		if err = builder.Append(ctx, values); err != nil {
			return nil, err
		}
		inner.captures = &columnar.Batch{Columns: make([]columnar.Vector, 0, len(values))}
		inner.captureValues = values
		if err = builder.Publish(inner.captures); err != nil {
			return nil, err
		}
	}
	p.root, err = inner.build()
	if err != nil {
		return nil, err
	}
	return p, nil
}
