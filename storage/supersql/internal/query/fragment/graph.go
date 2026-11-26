// Package fragment lowers immutable physical plans into local scheduling metadata.
// It neither opens storage nor establishes that an execution algorithm is available.
package fragment

import (
	"context"
	"errors"
	"fmt"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type PipelineID uint32
type EdgeID uint32

// NodeRole identifies the executable phase owned by a pipeline. A stateful
// producer's fill phase is an Edge sink; its emit phase has one owning node.
type NodeRole uint8

const (
	NativeSource NodeRole = iota + 1
	Transform
	HashProbe
	StateSource
	ExchangeSource
	CTEReader
	FeedbackSource
)

type Limits struct {
	MaxNodes, MaxEdges, MaxDepth uint32
	MaxWork, MaxBytes            uint64
}

var (
	ErrInvalid = errors.New("invalid fragment graph")
	ErrLimit   = errors.New("fragment graph resource limit exceeded")
)

// EdgeKind specifies the producer sink and the consumer's activation condition.
type EdgeKind uint8

const (
	// Stream connects shared output to a reader. Activate both ends together;
	// completing the producer first could deadlock a bounded fan-out queue.
	Stream EdgeKind = iota + 1
	// HashBuild consumes Input into the hash state identified by Output. The
	// consumer (including its probe sources) starts only after build completion.
	HashBuild
	// MaterializedInput fills Output's state from Input before Output emits rows.
	MaterializedInput
	// CTEDemand advances a shared materialization only as its consumer requests
	// another prefix; waiting for producer completion first can drain unbounded input.
	CTEDemand
	// LocalExchange connects a send sink to Output's receive source. Both ends
	// run concurrently under the physical node's bounded local queue contract.
	LocalExchange
	// RecursiveSeed initializes Output's worktable once. RecursiveStep runs once
	// per delta, after seed completion; its EOF is iteration EOF, not query EOF.
	RecursiveSeed
	RecursiveStep
	// ApplyInput opens Input once per owned outer row, never eagerly.
	ApplyInput
	// ScalarInput opens Input only when the expression demands its scalar value.
	// Port indexes Output's scalar-subplan descriptors, not relational children.
	ScalarInput
	// StatementInput completes a mandatory writer before root output. Port
	// indexes the root's statement dependencies, not relational properties.
	StatementInput
)

// Edge always points producer -> consumer. HashBuild, MaterializedInput and
// StatementInput require completion; CTEDemand is pull-driven.
// RecursiveSeed/Step are controlled by Output's fixpoint driver.
// Input's physical output is the sink schema. Output/Port identifies the required
// input property; CTEDemand instead uses the consumer's remapped output schema.
// Stateful sinks refer to Output without taking ownership of that physical node.
type Edge struct {
	Producer, Consumer PipelineID
	Input, Output      physical.NodeID
	Port               uint32
	Kind               EdgeKind
}

// Feedback is not a DAG edge. The driver initializes Union's state, runs Anchor,
// then repeatedly binds the previous delta to Scan and runs Member until empty.
// Pipelines with Iteration == Union run per delta; all others run in their own
// enclosing invocation. No pipeline may be scheduled just because its indegree
// is zero without first checking its Iteration domain.
type Feedback struct {
	Union, Scan    physical.NodeID
	Anchor, Member PipelineID
}

type extent struct{ start, count uint32 }

type pipeline struct {
	members, incoming, outgoing extent
	iteration                   physical.NodeID
	parallel                    bool
}

// FragmentGraph borrows its immutable Plan and owns only contiguous typed
// metadata. IDs are one-based. Accessors return values or checked immutable views.
type FragmentGraph struct {
	plan      *physical.Plan
	pipelines []pipeline
	members   []physical.NodeID
	owners    []PipelineID
	roles     []NodeRole
	edges     []Edge
	links     []EdgeID
	feedback  []Feedback
	order     []PipelineID
	root      PipelineID
	bytes     uint64
}

type Pipeline struct {
	graph *FragmentGraph
	id    PipelineID
}

func (g *FragmentGraph) Plan() *physical.Plan  { return g.plan }
func (g *FragmentGraph) Root() PipelineID      { return g.root }
func (g *FragmentGraph) PipelineCount() uint32 { return uint32(len(g.pipelines)) }
func (g *FragmentGraph) EdgeCount() uint32     { return uint32(len(g.edges)) }
func (g *FragmentGraph) FeedbackCount() uint32 { return uint32(len(g.feedback)) }
func (g *FragmentGraph) OwnedBytes() uint64    { return g.bytes }
func (g *FragmentGraph) Owner(id physical.NodeID) (PipelineID, bool) {
	return byID(g.owners, uint32(id))
}
func (g *FragmentGraph) Role(id physical.NodeID) (NodeRole, bool) { return byID(g.roles, uint32(id)) }
func (g *FragmentGraph) Edge(id EdgeID) (Edge, bool)              { return byID(g.edges, uint32(id)) }
func (g *FragmentGraph) Feedback(index uint32) (Feedback, bool)   { return byIndex(g.feedback, index) }

// Topological returns a producer-before-consumer ordering, not an activation
// schedule: exchanges and streams overlap, and recursion has iteration gates.
func (g *FragmentGraph) Topological(index uint32) (PipelineID, bool) { return byIndex(g.order, index) }
func (g *FragmentGraph) Pipeline(id PipelineID) (Pipeline, bool) {
	if _, ok := byID(g.pipelines, uint32(id)); !ok {
		return Pipeline{}, false
	}
	return Pipeline{g, id}, true
}
func (p Pipeline) NodeCount() uint32 { return p.graph.pipelines[p.id-1].members.count }

// Node returns physical nodes in input-before-output order within this pipeline.
func (p Pipeline) Node(index uint32) (physical.NodeID, bool) {
	r := p.graph.pipelines[p.id-1].members
	if index >= r.count {
		return 0, false
	}
	return p.graph.members[r.start+index], true
}
func (p Pipeline) IncomingCount() uint32 { return p.graph.pipelines[p.id-1].incoming.count }
func (p Pipeline) OutgoingCount() uint32 { return p.graph.pipelines[p.id-1].outgoing.count }
func (p Pipeline) Incoming(index uint32) (EdgeID, bool) {
	return p.link(p.graph.pipelines[p.id-1].incoming, index)
}
func (p Pipeline) Outgoing(index uint32) (EdgeID, bool) {
	return p.link(p.graph.pipelines[p.id-1].outgoing, index)
}
func (p Pipeline) link(r extent, index uint32) (EdgeID, bool) {
	if index >= r.count {
		return 0, false
	}
	return p.graph.links[r.start+index], true
}
func (p Pipeline) Iteration() physical.NodeID { return p.graph.pipelines[p.id-1].iteration }

// ParallelEligible permits independent morsels, never duplicate execution of a
// source. The caller must still prove disjoint morsels and enforce worker limits.
// Only immutable, non-erroring scan/filter/project pipelines qualify; stateful
// algorithms without an exposed parallel contract remain serial.
func (p Pipeline) ParallelEligible() bool { return p.graph.pipelines[p.id-1].parallel }

func byIndex[T any](values []T, index uint32) (T, bool) {
	if uint64(index) >= uint64(len(values)) {
		var zero T
		return zero, false
	}
	return values[index], true
}
func byID[T any](values []T, id uint32) (T, bool) {
	if id == 0 {
		var zero T
		return zero, false
	}
	return byIndex(values, id-1)
}
func invalid(reason string) error { return fmt.Errorf("%w: %s", ErrInvalid, reason) }

type nodeWork struct {
	consumers, depth    uint32
	representative      physical.NodeID
	iteration           physical.NodeID
	worktableSearch     physical.NodeID
	worktableDependency bool
	seen                bool
}
type builder struct {
	ctx         context.Context
	limits      Limits
	work, bytes uint64
	graph       *FragmentGraph
	nodes       []nodeWork
}

func (b *builder) step() error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if b.work == b.limits.MaxWork {
		return ErrLimit
	}
	b.work++
	return nil
}
func (b *builder) charge(count, size uint64) error {
	if err := b.step(); err != nil {
		return err
	}
	if size != 0 && (count > (b.limits.MaxBytes-b.bytes)/size || count > uint64(^uint(0)>>1)/size) {
		return ErrLimit
	}
	b.bytes += count * size
	return nil
}

// Build validates limits before allocation. MaxBytes bounds metadata plus build
// scratch (not the borrowed plan), MaxWork bounds loop visits, and MaxEdges bounds
// all physical child/CTE references plus feedback, even when fused internally.
// Failure returns no graph. There are no runtime resources or storage reads.
func Build(ctx context.Context, plan *physical.Plan, limits Limits) (*FragmentGraph, error) {
	if ctx == nil {
		return nil, invalid("nil context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limits.MaxNodes == 0 || limits.MaxEdges == 0 || limits.MaxDepth == 0 || limits.MaxWork == 0 || limits.MaxBytes == 0 {
		return nil, invalid("positive limits required")
	}
	if plan == nil || plan.NodeCount() == 0 || plan.Root() == 0 {
		return nil, invalid("unbuilt physical plan")
	}
	n := plan.NodeCount()
	if n > limits.MaxNodes || n == math.MaxUint32 {
		return nil, ErrLimit
	}
	b := builder{ctx: ctx, limits: limits}
	if err := b.charge(1, uint64(unsafe.Sizeof(FragmentGraph{}))); err != nil {
		return nil, err
	}
	if err := b.charge(uint64(n), uint64(unsafe.Sizeof(nodeWork{}))+uint64(unsafe.Sizeof(PipelineID(0)))+uint64(unsafe.Sizeof(physical.NodeID(0)))+uint64(unsafe.Sizeof(NodeRole(0)))); err != nil {
		return nil, err
	}
	g := &FragmentGraph{plan: plan, owners: make([]PipelineID, n), members: make([]physical.NodeID, n), roles: make([]NodeRole, n)}
	b.graph, b.nodes = g, make([]nodeWork, n)
	if err := b.inspect(); err != nil {
		return nil, err
	}
	if err := b.partition(); err != nil {
		return nil, err
	}
	if err := b.connect(); err != nil {
		return nil, err
	}
	if err := b.order(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.bytes = b.bytes - uint64(n)*uint64(unsafe.Sizeof(nodeWork{}))
	return g, nil
}

func (b *builder) inspect() error {
	g := b.graph
	var references uint64
	for i := range b.nodes {
		if err := b.step(); err != nil {
			return err
		}
		id := physical.NodeID(i + 1)
		node, ok := g.plan.Node(id)
		if !ok {
			return invalid("physical node ID")
		}
		meta := node.Metadata()
		if err := shape(meta, node.ChildCount()); err != nil {
			return err
		}
		g.roles[i] = role(meta, node.ChildCount())
		b.nodes[i].representative = id
		b.nodes[i].iteration = physical.NodeID(math.MaxUint32)
		b.nodes[i].depth = 1
		count := node.ChildCount() + node.ScalarSubplanCount() + node.StatementDependencyCount()
		if meta.Algorithm == physical.CTEScan || meta.Algorithm == physical.WorktableScan {
			count++
		}
		references += uint64(count)
		if references > uint64(b.limits.MaxEdges) {
			return ErrLimit
		}
		for j := uint32(0); j < count; j++ {
			if err := b.step(); err != nil {
				return err
			}
			child := physicalDependency(node, j)
			if child == 0 || uint64(child) > uint64(len(b.nodes)) {
				return invalid("dependency ID")
			}
			if meta.Algorithm == physical.WorktableScan && j == count-1 {
				continue
			}
			if child >= id {
				return invalid("cyclic or unordered dependency")
			}
			b.nodes[child-1].consumers++
			b.nodes[i].depth = max(b.nodes[i].depth, b.nodes[child-1].depth+1)
		}
		if b.nodes[i].depth > b.limits.MaxDepth {
			return ErrLimit
		}
	}
	root := g.plan.Root()
	if uint64(root) > uint64(len(b.nodes)) {
		return invalid("root ID")
	}
	b.nodes[root-1].seen, b.nodes[root-1].iteration = true, 0
	for i := len(b.nodes) - 1; i >= 0; i-- {
		if err := b.step(); err != nil {
			return err
		}
		if !b.nodes[i].seen {
			return invalid("unreachable physical node")
		}
		node, _ := g.plan.Node(physical.NodeID(i + 1))
		meta := node.Metadata()
		for j := uint32(0); j < node.ChildCount(); j++ {
			if err := b.step(); err != nil {
				return err
			}
			child, _ := node.Child(j)
			scope := b.nodes[i].iteration
			if meta.Algorithm == physical.RecursiveUnion && j == 1 {
				scope = physical.NodeID(i + 1)
			}
			if err := b.mark(child, scope); err != nil {
				return err
			}
		}
		for j := range node.ScalarSubplanCount() {
			if err := b.step(); err != nil {
				return err
			}
			subplan, _ := node.ScalarSubplan(j)
			if err := b.mark(subplan.Root, b.nodes[i].iteration); err != nil {
				return err
			}
		}
		for j := range node.StatementDependencyCount() {
			if err := b.step(); err != nil {
				return err
			}
			dependency, _ := node.StatementDependency(j)
			if err := b.mark(dependency, 0); err != nil {
				return err
			}
		}
		if meta.Algorithm == physical.CTEScan {
			scope := b.nodes[i].iteration
			if scope != 0 {
				dependent, err := b.usesWorktable(meta.Worktable.Producer, scope)
				if err != nil {
					return err
				}
				if !dependent {
					scope = 0
				}
			}
			if err := b.mark(meta.Worktable.Producer, scope); err != nil {
				return err
			}
		}
		if meta.Algorithm == physical.WorktableScan && b.nodes[i].iteration != meta.Worktable.Producer {
			return invalid("feedback outside its recursive member")
		}
	}
	return nil
}

func physicalDependency(node physical.Node, index uint32) physical.NodeID {
	if child, ok := node.Child(index); ok {
		return child
	}
	index -= node.ChildCount()
	if subplan, ok := node.ScalarSubplan(index); ok {
		return subplan.Root
	}
	index -= node.ScalarSubplanCount()
	if dependency, ok := node.StatementDependency(index); ok {
		return dependency
	}
	return node.Metadata().Worktable.Producer
}
func (b *builder) mark(id physical.NodeID, iteration physical.NodeID) error {
	n := &b.nodes[id-1]
	if n.seen && n.iteration != iteration {
		return invalid("shared ordinary node crosses recursive invocation domains")
	}
	n.seen, n.iteration = true, iteration
	return nil
}

func (b *builder) usesWorktable(id, producer physical.NodeID) (bool, error) {
	if err := b.step(); err != nil {
		return false, err
	}
	work := &b.nodes[id-1]
	if work.worktableSearch == producer {
		return work.worktableDependency, nil
	}
	node, _ := b.graph.plan.Node(id)
	meta := node.Metadata()
	dependent := meta.Algorithm == physical.WorktableScan && meta.Worktable.Producer == producer
	for i := uint32(0); i < node.ChildCount() && !dependent; i++ {
		child, _ := node.Child(i)
		var err error
		dependent, err = b.usesWorktable(child, producer)
		if err != nil {
			return false, err
		}
	}
	for i := uint32(0); i < node.ScalarSubplanCount() && !dependent; i++ {
		subplan, _ := node.ScalarSubplan(i)
		var err error
		dependent, err = b.usesWorktable(subplan.Root, producer)
		if err != nil {
			return false, err
		}
	}
	for i := uint32(0); i < node.StatementDependencyCount() && !dependent; i++ {
		dependency, _ := node.StatementDependency(i)
		var err error
		dependent, err = b.usesWorktable(dependency, producer)
		if err != nil {
			return false, err
		}
	}
	if meta.Algorithm == physical.CTEScan && !dependent {
		var err error
		dependent, err = b.usesWorktable(meta.Worktable.Producer, producer)
		if err != nil {
			return false, err
		}
	}
	work.worktableSearch, work.worktableDependency = producer, dependent
	return dependent, nil
}

func shape(meta physical.Metadata, children uint32) error {
	var want uint32 = 1
	switch meta.Algorithm {
	case physical.TableScan, physical.PrimaryKeyScan, physical.SecondaryScan, physical.CatalogScan, physical.Values, physical.GenerateSeries, physical.GenerateSubscripts, physical.Unnest, physical.CTEScan, physical.WorktableScan:
		want = 0
	case physical.LookupJoin, physical.NestedLoopJoin, physical.Apply, physical.HashJoin, physical.MergeJoin, physical.RecursiveUnion, physical.UnionAll, physical.HashSetOp:
		want = 2
	case physical.RowsFrom:
		if children < 2 {
			return invalid("ROWS FROM requires at least two inputs")
		}
		want = children
	}
	if meta.Algorithm < physical.TableScan || meta.Algorithm > physical.CatalogScan || children != want {
		return invalid("source/transform arity")
	}
	wantBreak := physical.ExpectedBreak(meta.Algorithm)
	if meta.Break != wantBreak {
		return invalid("sink/boundary shape")
	}
	return nil
}

func (b *builder) partition() error {
	g := b.graph
	for i := len(b.nodes) - 1; i >= 0; i-- {
		if err := b.step(); err != nil {
			return err
		}
		node, _ := g.plan.Node(physical.NodeID(i + 1))
		meta := node.Metadata()
		for j := uint32(0); j < node.ChildCount(); j++ {
			if err := b.step(); err != nil {
				return err
			}
			child, _ := node.Child(j)
			streaming := meta.Break == physical.Streaming || meta.Break == physical.BuildRight && j == 0
			if meta.Algorithm == physical.Apply {
				streaming = j == 0
			}
			if streaming && b.nodes[child-1].consumers == 1 && b.nodes[child-1].iteration == b.nodes[i].iteration {
				b.nodes[child-1].representative = b.nodes[i].representative
			}
		}
	}
	var count uint32
	for i := range b.nodes {
		if err := b.step(); err != nil {
			return err
		}
		if b.nodes[i].representative == physical.NodeID(i+1) {
			count++
			g.owners[i] = PipelineID(count)
		}
	}
	if err := b.charge(uint64(count), uint64(unsafe.Sizeof(pipeline{}))); err != nil {
		return err
	}
	g.pipelines = make([]pipeline, count)
	for i := range b.nodes {
		if err := b.step(); err != nil {
			return err
		}
		owner := g.owners[b.nodes[i].representative-1]
		if owner == 0 {
			return invalid("missing node owner")
		}
		g.owners[i] = owner
		p := &g.pipelines[owner-1]
		p.members.count++
		p.iteration = b.nodes[i].iteration
		p.parallel = true
	}
	var start uint32
	for i := range g.pipelines {
		if err := b.step(); err != nil {
			return err
		}
		g.pipelines[i].members.start = start
		start += g.pipelines[i].members.count
		b.nodes[i].consumers = 0
	}
	for i := range b.nodes {
		if err := b.step(); err != nil {
			return err
		}
		owner := g.owners[i]
		p := &g.pipelines[owner-1]
		cursor := &b.nodes[owner-1].consumers
		g.members[p.members.start+*cursor] = physical.NodeID(i + 1)
		*cursor++
		node, _ := g.plan.Node(physical.NodeID(i + 1))
		meta := node.Metadata()
		logical, ok := g.plan.Graph(meta.Logical.Graph)
		if !ok {
			return invalid("schema graph ID")
		}
		relation, ok := logical.Rel(meta.Logical.Rel)
		if !ok {
			return invalid("logical relation ID")
		}
		safe := meta.Algorithm == physical.TableScan || meta.Algorithm == physical.PrimaryKeyScan || meta.Algorithm == physical.SecondaryScan || meta.Algorithm == physical.Filter || meta.Algorithm == physical.Project
		p.parallel = p.parallel && safe && relation.Effects == (ir.Effects{}) && node.Delivered().ParameterCount() == 0 && p.iteration == 0
	}
	g.root = g.owners[g.plan.Root()-1]
	return nil
}

func role(meta physical.Metadata, children uint32) NodeRole {
	switch meta.Algorithm {
	case physical.Apply:
		return Transform
	case physical.CTEScan:
		return CTEReader
	case physical.WorktableScan:
		return FeedbackSource
	}
	switch meta.Break {
	case physical.BuildRight:
		return HashProbe
	case physical.MaterializeInput, physical.MaterializeBoth:
		return StateSource
	case physical.ExchangeBoundary:
		return ExchangeSource
	}
	if children == 0 {
		return NativeSource
	}
	return Transform
}

func edgeKind(meta physical.Metadata, port uint32) EdgeKind {
	if meta.Algorithm == physical.Apply {
		if port == 1 {
			return ApplyInput
		}
		return Stream
	}
	switch meta.Break {
	case physical.BuildRight:
		if port == 1 {
			return HashBuild
		}
	case physical.MaterializeInput:
		return MaterializedInput
	case physical.MaterializeBoth:
		if meta.Algorithm != physical.RecursiveUnion {
			return MaterializedInput
		}
		if port == 0 {
			return RecursiveSeed
		}
		return RecursiveStep
	case physical.ExchangeBoundary:
		return LocalExchange
	}
	return Stream
}

func (b *builder) connect() error {
	g := b.graph
	var edges, feedback uint32
	for i := range b.nodes {
		if err := b.step(); err != nil {
			return err
		}
		node, _ := g.plan.Node(physical.NodeID(i + 1))
		meta := node.Metadata()
		for j := uint32(0); j < node.ChildCount(); j++ {
			if err := b.step(); err != nil {
				return err
			}
			child, _ := node.Child(j)
			if g.owners[child-1] != g.owners[i] {
				edges++
			}
		}
		edges += node.ScalarSubplanCount() + node.StatementDependencyCount()
		if meta.Algorithm == physical.CTEScan {
			edges++
		}
		if meta.Algorithm == physical.WorktableScan {
			feedback++
		}
	}
	if uint64(edges)*2 > math.MaxUint32 {
		return ErrLimit
	}
	if err := b.charge(uint64(edges), uint64(unsafe.Sizeof(Edge{}))+2*uint64(unsafe.Sizeof(EdgeID(0)))); err != nil {
		return err
	}
	if err := b.charge(uint64(feedback), uint64(unsafe.Sizeof(Feedback{}))); err != nil {
		return err
	}
	g.edges, g.links, g.feedback = make([]Edge, edges), make([]EdgeID, edges*2), make([]Feedback, feedback)
	var next, nextFeedback uint32
	for i := range b.nodes {
		if err := b.step(); err != nil {
			return err
		}
		id := physical.NodeID(i + 1)
		node, _ := g.plan.Node(id)
		meta := node.Metadata()
		for j := uint32(0); j < node.ChildCount(); j++ {
			if err := b.step(); err != nil {
				return err
			}
			child, _ := node.Child(j)
			if g.owners[child-1] == g.owners[i] {
				continue
			}
			g.edges[next] = Edge{g.owners[child-1], g.owners[i], child, id, j, edgeKind(meta, j)}
			next++
		}
		for j := range node.ScalarSubplanCount() {
			if err := b.step(); err != nil {
				return err
			}
			subplan, _ := node.ScalarSubplan(j)
			g.edges[next] = Edge{g.owners[subplan.Root-1], g.owners[i], subplan.Root, id, j, ScalarInput}
			next++
		}
		for j := range node.StatementDependencyCount() {
			if err := b.step(); err != nil {
				return err
			}
			dependency, _ := node.StatementDependency(j)
			g.edges[next] = Edge{g.owners[dependency-1], g.owners[i], dependency, id, j, StatementInput}
			next++
		}
		if meta.Algorithm == physical.CTEScan {
			producer := meta.Worktable.Producer
			g.edges[next] = Edge{g.owners[producer-1], g.owners[i], producer, id, 0, CTEDemand}
			next++
		}
		if meta.Algorithm == physical.WorktableScan {
			producer, _ := g.plan.Node(meta.Worktable.Producer)
			anchor, aok := producer.Child(0)
			member, mok := producer.Child(1)
			if !aok || !mok || producer.Metadata().Algorithm != physical.RecursiveUnion {
				return invalid("feedback producer shape")
			}
			g.feedback[nextFeedback] = Feedback{meta.Worktable.Producer, id, g.owners[anchor-1], g.owners[member-1]}
			nextFeedback++
		}
	}
	for _, edge := range g.edges {
		if err := b.step(); err != nil {
			return err
		}
		if edge.Producer == edge.Consumer {
			return invalid("self dependency after pipeline fusion")
		}
		g.pipelines[edge.Producer-1].outgoing.count++
		g.pipelines[edge.Consumer-1].incoming.count++
		// Producers feeding stateful/shared sinks cannot assume independently
		// mergeable sink states merely because their source is morsel-safe.
		if edge.Kind != LocalExchange {
			g.pipelines[edge.Producer-1].parallel = false
		}
	}
	var start uint32
	for i := range g.pipelines {
		if err := b.step(); err != nil {
			return err
		}
		p := &g.pipelines[i]
		p.incoming.start = start
		start += p.incoming.count
		p.outgoing.start = start
		start += p.outgoing.count
		b.nodes[i].consumers, b.nodes[i].depth = 0, 0
	}
	for i, edge := range g.edges {
		if err := b.step(); err != nil {
			return err
		}
		from, to := &g.pipelines[edge.Producer-1], &g.pipelines[edge.Consumer-1]
		g.links[from.outgoing.start+b.nodes[edge.Producer-1].consumers] = EdgeID(i + 1)
		g.links[to.incoming.start+b.nodes[edge.Consumer-1].depth] = EdgeID(i + 1)
		b.nodes[edge.Producer-1].consumers++
		b.nodes[edge.Consumer-1].depth++
	}
	return nil
}

func (b *builder) order() error {
	g := b.graph
	if err := b.charge(uint64(len(g.pipelines)), uint64(unsafe.Sizeof(PipelineID(0)))); err != nil {
		return err
	}
	g.order = make([]PipelineID, len(g.pipelines))
	var count uint32
	for i, p := range g.pipelines {
		if err := b.step(); err != nil {
			return err
		}
		b.nodes[i].consumers = p.incoming.count
		b.nodes[i].depth = 1
		if p.incoming.count == 0 {
			g.order[count] = PipelineID(i + 1)
			count++
		}
	}
	for cursor := uint32(0); cursor < count; cursor++ {
		if err := b.step(); err != nil {
			return err
		}
		id := g.order[cursor]
		p := g.pipelines[id-1]
		for j := uint32(0); j < p.outgoing.count; j++ {
			if err := b.step(); err != nil {
				return err
			}
			edge := g.edges[g.links[p.outgoing.start+j]-1]
			next := &b.nodes[edge.Consumer-1]
			next.depth = max(next.depth, b.nodes[id-1].depth+1)
			if next.depth > b.limits.MaxDepth {
				return ErrLimit
			}
			next.consumers--
			if next.consumers == 0 {
				g.order[count] = edge.Consumer
				count++
			}
		}
	}
	if uint64(count) != uint64(len(g.pipelines)) {
		return invalid("pipeline dependency cycle")
	}
	return nil
}
