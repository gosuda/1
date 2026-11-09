package exec

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func scanDecision(ctx context.Context, plan storage.PushdownPlan, limit uint64) ([32]byte, error) {
	// Estimates and snapshot bindings are invocation-specific, not decisions.
	plan.Estimate = storage.ScanEstimate{}
	plan.SnapshotTS = 0
	plan.FenceID = [16]byte{}
	return storage.HashPushdownPlan(ctx, plan, limit)
}

func (c *invocation) scan(node physical.NodeSpec) (storage.BatchCursor, error) {
	p := c.portal
	spec := node.Scan
	decision, err := scanDecision(p.ctx, storage.PushdownPlan{Request: spec.Request, Accepted: spec.Accepted, Residual: spec.Residual, Projection: spec.Projection, Ordering: spec.Ordering, AppliedLimit: spec.AppliedLimit}, p.env.Limits.MetadataBytes)
	if err != nil {
		return nil, err
	}
	input, err := c.scanInput(node, decision)
	if err != nil {
		return nil, err
	}
	if !spec.Residual.Empty() {
		state, layouts, err := c.residual(node)
		if err != nil {
			return nil, errors.Join(err, input.Close())
		}
		input = &expressionCursor{input: input, state: state, env: &p.env, layouts: layouts, filter: true,
			source:    columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))},
			evaluated: columnar.Batch{Columns: make([]columnar.Vector, 0, 1)},
			compact:   columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))},
			selection: make([]uint32, 0, p.env.Limits.BatchRows)}
	}
	if spec.Lookup.Enabled {
		return c.secondaryIndexLookup(node, input)
	}
	if err := c.charge(uint64(len(node.Output)) * uint64(unsafe.Sizeof(int(0)))); err != nil {
		return nil, errors.Join(err, input.Close())
	}
	mapping := make([]int, len(node.Output))
	for i, slot := range node.Output {
		graph, _ := c.plan.Graph(slot.Graph)
		info, _ := graph.Slot(slot.ID)
		mapping[i] = slices.Index(spec.Projection, spec.PhysicalColumn(info.Column))
		if mapping[i] < 0 {
			return nil, errors.Join(ErrCapability, input.Close())
		}
	}
	return &expressionCursor{input: input, env: &p.env, projection: mapping, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(spec.Projection))}}, nil
}

type residualLowerer struct {
	ctx              context.Context
	builder          *ir.Builder
	scope            ir.ScopeID
	predicate        storage.Predicate
	columns          []model.ColumnID
	slots            []physical.Slot
	layouts          []model.TypeLayout
	roots            []ir.ScalarID
	types            []ir.LayoutID
	boolean          ir.LayoutID
	collation        model.CollationID
	defaultCollation model.CollationID
}

func (c *invocation) residual(node physical.NodeSpec) (*expr.State, []model.TypeLayout, error) {
	p := c.portal
	prepared := &c.prepared.nodes[c.current]
	if !c.preparing {
		program, err := c.programIn(node.Logical.Graph, nil, nil, nil)
		if err != nil {
			return nil, nil, err
		}
		state, err := c.state(program)
		return state, prepared.residualLayouts, err
	}
	limits := p.env.Limits.Expressions
	count := node.Scan.Residual.NodeCount()
	if count > limits.MaxInstructions || uint64(limits.MaxInstructions) > math.MaxUint32 {
		return nil, nil, expr.ErrLimit
	}
	if err := c.charge(uint64(count)*(uint64(unsafe.Sizeof(ir.ScalarID(0)))+uint64(unsafe.Sizeof(ir.LayoutID(0)))+1) + uint64(len(node.Scan.Projection))*(uint64(unsafe.Sizeof(physical.Slot{}))+uint64(unsafe.Sizeof(model.TypeLayout{})))); err != nil {
		return nil, nil, err
	}
	claim, err := p.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return nil, nil, err
	}
	defer claim.Release()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: uint32(limits.MaxInstructions), MaxOperands: uint32(limits.MaxInstructions), MaxSlots: uint32(limits.MaxInstructions), MaxDepth: uint32(limits.MaxDepth), MaxBytes: limits.MaxBytes, SourceBytes: 1})
	if err != nil {
		return nil, nil, err
	}
	scope, err := b.AddScope(p.ctx, 0, nil)
	if err != nil {
		return nil, nil, err
	}
	l := residualLowerer{ctx: p.ctx, builder: b, scope: scope, predicate: node.Scan.Residual, columns: node.Scan.Projection,
		slots: make([]physical.Slot, len(node.Scan.Projection)), layouts: make([]model.TypeLayout, len(node.Scan.Projection)), roots: make([]ir.ScalarID, count), types: make([]ir.LayoutID, count), collation: p.env.Builtin.Collation, defaultCollation: p.env.Builtin.DefaultCollation}
	l.boolean, err = b.AddLayout(p.ctx, model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}})
	if err != nil {
		return nil, nil, err
	}
	for i, id := range l.columns {
		for _, column := range node.Scan.Request.Layout {
			if column.ID == id {
				l.layouts[i] = model.TypeLayout{Type: column.Type, Children: column.Children}
				break
			}
		}
		layout, err := b.AddLayout(p.ctx, l.layouts[i])
		if err != nil {
			return nil, nil, err
		}
		slot, err := b.AddSlot(p.ctx, ir.SlotInfo{Scope: scope, Layout: layout, Nullable: true})
		if err != nil {
			return nil, nil, err
		}
		l.slots[i] = physical.Slot{Graph: node.Logical.Graph, ID: slot, Layout: layout}
	}
	// Subtree predicates retain unrelated nodes. Only lower the reachable DAG.
	reachable := make([]bool, count)
	reachable[l.predicate.Root()] = true
	for i := count - 1; i >= 0; i-- {
		if err := p.ctx.Err(); err != nil {
			return nil, nil, err
		}
		if !reachable[i] {
			continue
		}
		n := l.predicate.Node(uint32(i))
		switch n.Kind {
		case storage.PredicateComparison, storage.PredicateAnd, storage.PredicateOr:
			reachable[n.Left], reachable[n.Right] = true, true
		case storage.PredicateNullTest, storage.PredicateIn:
			reachable[n.Left] = true
		}
		if n.Kind == storage.PredicateIn {
			if uint64(n.ArgumentCount) > uint64(limits.MaxInstructions) {
				return nil, nil, expr.ErrLimit
			}
			if err := c.charge(uint64(n.ArgumentCount) * uint64(unsafe.Sizeof(ir.ScalarID(0)))); err != nil {
				return nil, nil, err
			}
			for j := uint32(0); j < n.ArgumentCount; j++ {
				reachable[l.predicate.Argument(n.FirstArgument+j)] = true
			}
		}
	}
	for i := range count {
		if reachable[i] {
			if err := l.lower(uint32(i)); err != nil {
				return nil, nil, err
			}
		}
	}
	root, err := b.AddRel(p.ctx, ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: scope}})
	if err != nil {
		return nil, nil, err
	}
	graph, err := b.Build(p.ctx, root)
	if err != nil {
		return nil, nil, err
	}
	c.residualProgram = true
	defer func() { c.residualProgram = false }()
	program, err := c.programIn(node.Logical.Graph, graph, []ir.ScalarID{l.roots[l.predicate.Root()]}, l.slots)
	if err != nil {
		return nil, nil, err
	}
	prepared.residualLayouts = l.layouts
	state, err := c.state(program)
	return state, l.layouts, err
}

func (l *residualLowerer) lower(index uint32) error {
	n := l.predicate.Node(index)
	s := ir.Scalar{Scope: l.scope, Layout: l.boolean, Nullable: true}
	var args []ir.ScalarID
	var value model.Value
	var err error
	switch n.Kind {
	case storage.PredicateColumn:
		col := slices.Index(l.columns, n.ColumnID)
		if col < 0 || l.layouts[col].Type != n.Type {
			return ErrCapability
		}
		s.Kind, s.Slot, s.Layout = ir.Slot, l.slots[col].ID, l.slots[col].Layout
	case storage.PredicateConstant:
		s.Kind = ir.Const
		value = l.predicate.Constant(n.ConstantIndex)
		s.Nullable = value.IsNull
		s.Layout, err = l.builder.AddLayout(l.ctx, model.TypeLayout{Type: n.Type})
	case storage.PredicateComparison:
		l.roots[index], err = l.comparison(n, n.Left, n.Right)
		l.types[index] = l.boolean
		return err
	case storage.PredicateNullTest:
		s.Kind, s.Nullable, s.Negated = ir.NullTest, false, n.Negated
		args = []ir.ScalarID{l.roots[n.Left]}
	case storage.PredicateAnd, storage.PredicateOr:
		s.Kind, s.BoolOp = ir.Bool, ir.And
		if n.Kind == storage.PredicateOr {
			s.BoolOp = ir.Or
		}
		args = []ir.ScalarID{l.roots[n.Left], l.roots[n.Right]}
	case storage.PredicateIn:
		return l.in(index, n)
	default:
		return ErrCapability
	}
	if err != nil {
		return err
	}
	l.types[index] = s.Layout
	l.roots[index], err = l.builder.AddScalar(l.ctx, ir.ScalarSpec{Scalar: s, Operands: args, Value: value})
	return err
}

func (l *residualLowerer) comparison(n storage.PredicateNode, left, right uint32) (ir.ScalarID, error) {
	op, ok := builtin.LookupOperator(n.Operator)
	if !ok || op.Left != l.predicate.Node(left).Type.ID || op.Right != l.predicate.Node(right).Type.ID || op.Result != 16 {
		return 0, fmt.Errorf("storage residual operator %d: %w", n.Operator, ErrCapability)
	}
	switch op.Name {
	case "=", "<>", "<", "<=", ">", ">=":
	default:
		return 0, ErrCapability
	}
	collation := n.Collation
	if collation == 100 {
		collation = l.defaultCollation
	}
	configured := l.collation
	if configured == 100 {
		configured = l.defaultCollation
	}
	cOrPosix := collation == 950 || collation == 951
	configuredCOrPosix := configured == 950 || configured == 951
	if collation != 0 && collation != configured && !(cOrPosix && configuredCOrPosix) {
		return 0, ErrCapability
	}
	strict := true
	if signature, known := builtin.LookupSignature(op.Function); known {
		strict = signature.Strict
	}
	signature, err := l.builder.AddSignature(l.ctx, ir.SignatureSpec{Signature: ir.Signature{Operator: n.Operator, Result: l.boolean, Nullable: true, Strict: strict}, Parameters: []ir.LayoutID{l.types[left], l.types[right]}})
	if err != nil {
		return 0, err
	}
	return l.builder.AddScalar(l.ctx, ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: l.scope, Layout: l.boolean, Nullable: true, Operator: n.Operator, Signature: signature, Strict: strict}, Operands: []ir.ScalarID{l.roots[left], l.roots[right]}})
}

func (l *residualLowerer) in(index uint32, n storage.PredicateNode) error {
	op, ok := builtin.LookupOperator(n.Operator)
	if !ok || op.Name != "=" {
		return ErrCapability
	}
	args := make([]ir.ScalarID, n.ArgumentCount)
	for j := range n.ArgumentCount {
		var err error
		args[j], err = l.comparison(n, n.Left, l.predicate.Argument(n.FirstArgument+j))
		if err != nil {
			return err
		}
	}
	var root ir.ScalarID
	var err error
	switch len(args) {
	case 0:
		root, err = l.builder.AddScalar(l.ctx, ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: l.scope, Layout: l.boolean}, Value: model.Value{Type: model.TypeBoolean}})
	case 1:
		root = args[0]
	default:
		root, err = l.builder.AddScalar(l.ctx, ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: l.scope, Layout: l.boolean, Nullable: true, BoolOp: ir.Or}, Operands: args})
	}
	if err == nil && n.Negated {
		root, err = l.builder.AddScalar(l.ctx, ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: l.scope, Layout: l.boolean, Nullable: true, BoolOp: ir.Not}, Operands: []ir.ScalarID{root}})
	}
	l.roots[index], l.types[index] = root, l.boolean
	return err
}
