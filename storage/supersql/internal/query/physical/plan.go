package physical

import (
	"context"
	"errors"
	"fmt"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

var (
	ErrInvalid = errors.New("invalid physical plan")
	ErrLimit   = errors.New("physical plan resource limit exceeded")
)

func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }

// NewPlan borrows inputs only for this call. Graphs are already immutable and
// shared; every other slice and value payload is copied under the supplied budget.
// Children must precede parents. No reader or per-invocation scan binding is kept.
func NewPlan(ctx context.Context, spec Spec, limits Limits) (*Plan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limits.MaxNodes == 0 || limits.MaxOperands == 0 || limits.MaxProperties == 0 || limits.MaxDepth == 0 || limits.MaxBytes == 0 {
		return nil, invalid("positive limits required")
	}
	if limits.MaxDepth > 512 || limits.MaxNodes == math.MaxUint32 {
		return nil, ErrLimit
	}
	if len(spec.Nodes) == 0 || uint64(len(spec.Nodes)) > uint64(limits.MaxNodes) {
		return nil, ErrLimit
	}
	if spec.Root == 0 || uint64(spec.Root) > uint64(len(spec.Nodes)) {
		return nil, invalid("root index")
	}
	b := copier{ctx: ctx, limits: limits}
	if err := b.charge(1, uint64(unsafe.Sizeof(Plan{}))); err != nil {
		return nil, err
	}
	if err := b.charge(uint64(len(spec.Nodes)), uint64(unsafe.Sizeof(NodeSpec{}))+9); err != nil {
		return nil, err
	}
	graphs, err := ownSlice(&b, spec.Graphs, false)
	if err != nil {
		return nil, err
	}
	if len(graphs) == 0 {
		return nil, invalid("missing logical graphs")
	}
	for _, graph := range graphs {
		if graph == nil || graph.Root() == 0 {
			return nil, invalid("unbuilt logical graph")
		}
	}
	p := &Plan{graphs: graphs, nodes: make([]NodeSpec, len(spec.Nodes)), root: spec.Root}
	depths := make([]uint32, len(spec.Nodes))
	consumers := make([]uint32, len(spec.Nodes))
	for i, source := range spec.Nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if uint64(len(source.Children))+uint64(len(source.ScalarSubplans))+uint64(len(source.StatementDependencies)) > uint64(limits.MaxOperands) {
			return nil, ErrLimit
		}
		depth := uint32(1)
		for _, child := range source.Children {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if child == 0 || uint64(child) > uint64(i) {
				return nil, invalid("children must precede their parent")
			}
			depth = max(depth, depths[child-1]+1)
			consumers[child-1]++
		}
		if len(source.StatementDependencies) != 0 && NodeID(i+1) != spec.Root {
			return nil, invalid("statement dependencies belong only to the root")
		}
		var previous NodeID
		for _, dependency := range source.StatementDependencies {
			if dependency == 0 || uint64(dependency) > uint64(i) || dependency <= previous {
				return nil, invalid("statement dependencies must be unique and precede their owner in dependency order")
			}
			producer := p.nodes[dependency-1]
			if producer.Algorithm != CTEMaterialize || len(producer.Delivered.Parameters) != 0 {
				return nil, invalid("statement dependency must be an uncorrelated CTE producer")
			}
			g, _ := p.Graph(producer.Logical.Graph)
			cte, _ := g.CTE(producer.Worktable.CTE)
			body, _ := g.Rel(cte.Body)
			if body.Kind != ir.Modify {
				return nil, invalid("statement dependency must modify data")
			}
			previous = dependency
			depth = max(depth, depths[dependency-1]+1)
			consumers[dependency-1]++
		}
		for _, subplan := range source.ScalarSubplans {
			if subplan.Root == 0 || uint64(subplan.Root) > uint64(i) {
				return nil, invalid("scalar body must precede owner")
			}
			depth = max(depth, depths[subplan.Root-1]+1)
			consumers[subplan.Root-1]++
		}
		if source.Algorithm == CTEScan {
			producer := source.Worktable.Producer
			if producer == 0 || uint64(producer) > uint64(i) {
				return nil, invalid("CTE producer must precede consumer")
			}
			depth = max(depth, depths[producer-1]+1)
			consumers[producer-1]++
		}
		if depth > limits.MaxDepth {
			return nil, ErrLimit
		}
		depths[i] = depth
		owned, err := b.node(source)
		if err != nil {
			return nil, fmt.Errorf("node %d: %w", i+1, err)
		}
		p.nodes[i] = owned
		if err := p.validateNode(ctx, NodeID(i+1)); err != nil {
			return nil, fmt.Errorf("node %d: %w", i+1, err)
		}
		if err := p.validateScalarSubplans(ctx, owned, &b); err != nil {
			return nil, fmt.Errorf("node %d: %w", i+1, err)
		}
	}
	if len(p.nodes[p.root-1].Delivered.Parameters) != 0 {
		return nil, invalid("unresolved root parameters")
	}
	rootGraph, _ := p.Graph(p.nodes[p.root-1].Logical.Graph)
	if rootGraph.Root() != p.nodes[p.root-1].Logical.Rel {
		return nil, invalid("physical root does not implement the complete logical graph")
	}
	rootNode := p.nodes[p.root-1]
	rootRel, _ := rootGraph.Rel(rootGraph.Root())
	if rootRel.Kind == ir.Sort {
		if err := logicalOrder(rootGraph, rootNode.Logical, rootNode.Delivered.Ordering, 0); err != nil {
			return nil, err
		}
	}
	if err := p.validateStatementDependencies(ctx, &b); err != nil {
		return nil, err
	}
	seen := make([]bool, len(p.nodes))
	seen[p.root-1] = true
	for i := len(p.nodes) - 1; i >= 0; i-- {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !seen[i] {
			return nil, invalid("unreachable physical node")
		}
		for _, child := range p.nodes[i].Children {
			seen[child-1] = true
		}
		for _, subplan := range p.nodes[i].ScalarSubplans {
			seen[subplan.Root-1] = true
		}
		for _, dependency := range p.nodes[i].StatementDependencies {
			seen[dependency-1] = true
		}
		if p.nodes[i].Algorithm == CTEScan {
			seen[p.nodes[i].Worktable.Producer-1] = true
		}
		if err := p.validateWorktable(ctx, NodeID(i+1)); err != nil {
			return nil, err
		}
		if err := p.validateGlobalLimit(ctx, NodeID(i+1), consumers); err != nil {
			return nil, err
		}
	}
	p.bytes = b.used - uint64(len(spec.Nodes))*9
	return p, nil
}

// CloneNode returns independent mutable planning data, not a view into the plan.
func (p *Plan) CloneNode(ctx context.Context, id NodeID, maxBytes uint64) (NodeSpec, error) {
	if err := ctx.Err(); err != nil {
		return NodeSpec{}, err
	}
	if id == 0 || uint64(id) > uint64(len(p.nodes)) {
		return NodeSpec{}, invalid("node index")
	}
	b := copier{ctx: ctx, limits: Limits{MaxBytes: maxBytes, MaxOperands: math.MaxUint32, MaxProperties: math.MaxUint32}}
	if err := b.charge(1, uint64(unsafe.Sizeof(NodeSpec{}))); err != nil {
		return NodeSpec{}, err
	}
	return b.node(p.nodes[id-1])
}

type copier struct {
	ctx                        context.Context
	limits                     Limits
	used, operands, properties uint64
}

func (b *copier) charge(count, size uint64) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if size != 0 && count > (b.limits.MaxBytes-b.used)/size {
		return ErrLimit
	}
	b.used += count * size
	return nil
}
func (b *copier) count(n uint64, property bool) error {
	used, limit := &b.operands, b.limits.MaxOperands
	if property {
		used, limit = &b.properties, b.limits.MaxProperties
	}
	if n > uint64(limit)-*used {
		return ErrLimit
	}
	*used += n
	return nil
}
func ownSlice[T any](b *copier, source []T, property bool) ([]T, error) {
	if err := b.count(uint64(len(source)), property); err != nil {
		return nil, err
	}
	var value T
	if err := b.charge(uint64(len(source)), uint64(unsafe.Sizeof(value))); err != nil {
		return nil, err
	}
	if len(source) == 0 {
		return nil, nil
	}
	owned := make([]T, len(source))
	copy(owned, source)
	return owned, nil
}
func (b *copier) propertiesCopy(source Properties) (Properties, error) {
	if err := b.count(1, true); err != nil {
		return Properties{}, err
	}
	result := source
	var err error
	if result.Ordering, err = ownSlice(b, source.Ordering, true); err != nil {
		return Properties{}, err
	}
	if result.Distribution.Keys, err = ownSlice(b, source.Distribution.Keys, true); err != nil {
		return Properties{}, err
	}
	if result.Parameters, err = ownSlice(b, source.Parameters, true); err != nil {
		return Properties{}, err
	}
	return result, nil
}
func (b *copier) request(source storage.PushdownRequest) (storage.PushdownRequest, error) {
	count := uint64(len(source.Layout)) + uint64(len(source.KeyLayout)) + uint64(len(source.ColumnIDs)) + uint64(len(source.Spans)) + uint64(source.Predicate.NodeCount())
	if count > uint64(b.limits.MaxOperands)-b.operands {
		return storage.PushdownRequest{}, ErrLimit
	}
	for _, span := range source.Spans {
		if err := b.ctx.Err(); err != nil {
			return storage.PushdownRequest{}, err
		}
		count += uint64(len(span.Lower.Values)) + uint64(len(span.Upper.Values))
		if count > uint64(b.limits.MaxOperands)-b.operands {
			return storage.PushdownRequest{}, ErrLimit
		}
	}
	for i := range source.Predicate.NodeCount() {
		if err := b.ctx.Err(); err != nil {
			return storage.PushdownRequest{}, err
		}
		node := source.Predicate.Node(uint32(i))
		count += uint64(node.ArgumentCount)
		if node.Kind == storage.PredicateConstant {
			count++
		}
		if count > uint64(b.limits.MaxOperands)-b.operands {
			return storage.PushdownRequest{}, ErrLimit
		}
	}
	if err := b.count(count, false); err != nil {
		return storage.PushdownRequest{}, err
	}
	owned, used, err := storage.CopyPushdownRequest(b.ctx, source, b.limits.MaxBytes-b.used)
	if err != nil {
		if errors.Is(err, model.ErrValueBudget) {
			return storage.PushdownRequest{}, ErrLimit
		}
		return storage.PushdownRequest{}, err
	}
	b.used += used
	return owned, nil
}
func (b *copier) predicate(source storage.Predicate) (storage.Predicate, error) {
	owned, err := b.request(storage.PushdownRequest{Predicate: source})
	return owned.Predicate, err
}
func (b *copier) node(source NodeSpec) (NodeSpec, error) {
	n := source
	var err error
	if n.Children, err = ownSlice(b, source.Children, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Output, err = ownSlice(b, source.Output, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Expressions, err = ownSlice(b, source.Expressions, false); err != nil {
		return NodeSpec{}, err
	}
	if n.ScalarSubplans, err = ownSlice(b, source.ScalarSubplans, false); err != nil {
		return NodeSpec{}, err
	}
	if n.StatementDependencies, err = ownSlice(b, source.StatementDependencies, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Required, err = ownSlice(b, source.Required, true); err != nil {
		return NodeSpec{}, err
	}
	for i, value := range source.Required {
		if n.Required[i], err = b.propertiesCopy(value); err != nil {
			return NodeSpec{}, err
		}
	}
	if n.Delivered, err = b.propertiesCopy(source.Delivered); err != nil {
		return NodeSpec{}, err
	}
	if n.Join.Keys, err = ownSlice(b, source.Join.Keys, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Aggregate.GroupKeys, err = ownSlice(b, source.Aggregate.GroupKeys, true); err != nil {
		return NodeSpec{}, err
	}
	if n.Aggregate.Combine, err = ownSlice(b, source.Aggregate.Combine, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.ColumnMap, err = ownSlice(b, source.Scan.ColumnMap, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Request, err = b.request(source.Scan.Request); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Accepted, err = b.predicate(source.Scan.Accepted); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Residual, err = b.predicate(source.Scan.Residual); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Projection, err = ownSlice(b, source.Scan.Projection, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Lookup.BaseColumnMap, err = ownSlice(b, source.Scan.Lookup.BaseColumnMap, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Lookup.BaseRequest, err = b.request(source.Scan.Lookup.BaseRequest); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Lookup.BaseAccepted, err = b.predicate(source.Scan.Lookup.BaseAccepted); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Lookup.BaseResidual, err = b.predicate(source.Scan.Lookup.BaseResidual); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Lookup.BaseProjection, err = ownSlice(b, source.Scan.Lookup.BaseProjection, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Catalog.Columns, err = ownSlice(b, source.Catalog.Columns, false); err != nil {
		return NodeSpec{}, err
	}
	if n.Scan.Ordering, err = ownSlice(b, source.Scan.Ordering, true); err != nil {
		return NodeSpec{}, err
	}
	return n, nil
}

func (p *Plan) relation(ref RelRef) (*ir.Graph, ir.Rel, error) {
	g, ok := p.Graph(ref.Graph)
	if !ok {
		return nil, ir.Rel{}, invalid("logical graph index")
	}
	r, ok := g.Rel(ref.Rel)
	if !ok {
		return nil, ir.Rel{}, invalid("logical relation index")
	}
	return g, r, nil
}

func (p *Plan) validateStatementDependencies(ctx context.Context, b *copier) error {
	if err := b.charge(uint64(len(p.nodes)), 1); err != nil {
		return err
	}
	mandatory := make([]bool, len(p.nodes))
	defer func() { b.used -= uint64(len(p.nodes)) }()
	for _, id := range p.nodes[p.root-1].StatementDependencies {
		mandatory[id-1] = true
	}
	for graphIndex, g := range p.graphs {
		if err := b.charge(uint64(g.CTECount()), 4); err != nil {
			return err
		}
		producers := make([]NodeID, g.CTECount())
		for i, n := range p.nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n.Algorithm != CTEMaterialize || n.Logical.Graph != GraphID(graphIndex+1) {
				continue
			}
			if producers[n.Worktable.CTE-1] != 0 {
				return invalid("CTE definition has multiple physical producers")
			}
			producers[n.Worktable.CTE-1] = NodeID(i + 1)
		}
		for id := ir.CTEID(1); id <= ir.CTEID(g.CTECount()); id++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			cte, _ := g.CTE(id)
			body, _ := g.Rel(cte.Body)
			if body.Kind == ir.Modify {
				producer := producers[id-1]
				if producer == 0 || !mandatory[producer-1] {
					return invalid("writable CTE missing mandatory statement dependency")
				}
			}
		}
		b.used -= uint64(g.CTECount()) * 4
	}
	return nil
}
