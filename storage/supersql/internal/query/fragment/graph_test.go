package fragment

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func buildLimits() Limits {
	return Limits{MaxNodes: 64, MaxEdges: 128, MaxDepth: 32, MaxWork: 4096, MaxBytes: 1 << 20}
}
func single() physical.Properties {
	return physical.Properties{Distribution: physical.Distribution{Kind: physical.SingleNode, Partitions: 1}, Rewindability: physical.OnePass}
}

type fixture struct {
	t                *testing.T
	b                *ir.Builder
	scope            ir.ScopeID
	integer, boolean ir.LayoutID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 256, MaxOperands: 1024, MaxSlots: 64, MaxDepth: 32, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{t: t, b: b}
	f.scope, err = b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.integer, err = b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	f.boolean, err = b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f fixture) slot() physical.Slot {
	f.t.Helper()
	id, err := f.b.AddSlot(f.t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.integer})
	if err != nil {
		f.t.Fatal(err)
	}
	return physical.Slot{Graph: 1, ID: id, Layout: f.integer}
}
func (f fixture) scalar(spec ir.ScalarSpec) ir.ScalarID {
	f.t.Helper()
	id, err := f.b.AddScalar(f.t.Context(), spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f fixture) rel(spec ir.RelSpec) physical.RelRef {
	f.t.Helper()
	id, err := f.b.AddRel(f.t.Context(), spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return physical.RelRef{Graph: 1, Rel: id}
}
func (f fixture) values() physical.NodeSpec {
	f.t.Helper()
	out := f.slot()
	constant := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.integer}, Value: model.Int64Val(7)})
	ref := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out.ID}, Expressions: []ir.ScalarID{constant}})
	return physical.NodeSpec{Logical: ref, Algorithm: physical.Values, Output: []physical.Slot{out}, Delivered: single(), Break: physical.Streaming, Expressions: []physical.ScalarRef{{Graph: 1, Scalar: constant}}}
}
func (f fixture) scan() physical.NodeSpec {
	f.t.Helper()
	id, err := f.b.AddSlot(f.t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.integer, Object: 10, Column: 1})
	if err != nil {
		f.t.Fatal(err)
	}
	out := physical.Slot{Graph: 1, ID: id, Layout: f.integer}
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	ref := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: 10, Generation: 1, DescriptorVersion: 1}, Outputs: []model.SlotID{id}})
	return physical.NodeSpec{Logical: ref, Algorithm: physical.TableScan, Output: []physical.Slot{out}, Delivered: single(), Break: physical.Streaming,
		Scan: physical.ScanSpec{Source: storage.ObjectRef{ObjectID: 10, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, ColumnMap: []physical.ScanColumn{{Source: 1, Target: 1}}, CapabilityEpoch: 1, Request: storage.PushdownRequest{Target: storage.ObjectRef{ObjectID: 10, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Layout: []storage.ColumnLayout{{ID: 1, Type: typ}}, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: typ}}, ColumnIDs: []model.ColumnID{1}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}, Projection: []model.ColumnID{1}}}
}
func (f fixture) plan(nodes ...physical.NodeSpec) *physical.Plan {
	f.t.Helper()
	g, err := f.b.Build(f.t.Context(), nodes[len(nodes)-1].Logical.Rel)
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := physical.NewPlan(f.t.Context(), physical.Spec{Graphs: []*ir.Graph{g}, Nodes: nodes, Root: physical.NodeID(len(nodes))}, physical.Limits{MaxNodes: 64, MaxOperands: 1024, MaxProperties: 1024, MaxDepth: 32, MaxBytes: 1 << 20})
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}
func graphFor(t *testing.T, plan *physical.Plan) *FragmentGraph {
	t.Helper()
	g, err := Build(t.Context(), plan, buildLimits())
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func onlyEdge(t *testing.T, g *FragmentGraph, kind EdgeKind, input, output physical.NodeID) Edge {
	t.Helper()
	if g.EdgeCount() != 1 {
		t.Fatalf("edges = %d, want one scheduling dependency", g.EdgeCount())
	}
	e, ok := g.Edge(1)
	if !ok || e.Kind != kind || e.Input != input || e.Output != output || e.Producer == e.Consumer {
		t.Fatalf("edge = %+v, want kind %v from physical %d to %d", e, kind, input, output)
	}
	return e
}
func assertOrder(t *testing.T, g *FragmentGraph) {
	t.Helper()
	positions := make(map[PipelineID]uint32)
	for i := uint32(0); i < g.PipelineCount(); i++ {
		id, ok := g.Topological(i)
		if !ok {
			t.Fatalf("missing topological position %d", i)
		}
		if _, exists := positions[id]; exists {
			t.Fatalf("pipeline %d scheduled twice", id)
		}
		positions[id] = i
	}
	for id := EdgeID(1); uint32(id) <= g.EdgeCount(); id++ {
		e, _ := g.Edge(id)
		if positions[e.Producer] >= positions[e.Consumer] {
			t.Fatalf("dependency %+v violates producer order", e)
		}
	}
}

func chain(t *testing.T) *physical.Plan {
	t.Helper()
	f := newFixture(t)
	scan := f.scan()
	yes := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.boolean}, Value: model.BoolVal(true)})
	filterRel := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: yes}, Inputs: []ir.RelID{scan.Logical.Rel}, Outputs: []model.SlotID{scan.Output[0].ID}})
	filter := physical.NodeSpec{Logical: filterRel, Algorithm: physical.Filter, Children: []physical.NodeID{1}, Output: scan.Output, Required: []physical.Properties{single()}, Delivered: single(), Break: physical.Streaming, Expressions: []physical.ScalarRef{{Graph: 1, Scalar: yes}}}
	out := f.slot()
	expr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: scan.Output[0].ID}})
	projectRel := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope}, Inputs: []ir.RelID{filterRel.Rel}, Outputs: []model.SlotID{out.ID}, Bindings: []ir.Binding{{Slot: out.ID, Expr: expr}}})
	project := physical.NodeSpec{Logical: projectRel, Algorithm: physical.Project, Children: []physical.NodeID{2}, Output: []physical.Slot{out}, Required: []physical.Properties{single()}, Delivered: single(), Break: physical.Streaming, Expressions: []physical.ScalarRef{{Graph: 1, Scalar: expr}}}
	return f.plan(scan, filter, project)
}
func TestStreamingChainHasOneOwner(t *testing.T) {
	plan := chain(t)
	g := graphFor(t, plan)
	if g.PipelineCount() != 1 || g.EdgeCount() != 0 {
		t.Fatalf("chain cut into %d pipelines with %d edges", g.PipelineCount(), g.EdgeCount())
	}
	p, ok := g.Pipeline(g.Root())
	if !ok || p.NodeCount() != 3 {
		t.Fatal("chain lost nodes")
	}
	if !p.ParallelEligible() {
		t.Fatal("immutable scan/filter/project chain lost its morsel contract")
	}
	if source, _ := g.Role(1); source != NativeSource {
		t.Fatalf("scan role = %v", source)
	}
	if project, _ := g.Role(3); project != Transform {
		t.Fatalf("project role = %v", project)
	}
	for i := uint32(0); i < 3; i++ {
		id, _ := p.Node(i)
		owner, _ := g.Owner(id)
		if id != physical.NodeID(i+1) || owner != g.Root() {
			t.Fatalf("member %d = %d, owner %d", i, id, owner)
		}
	}
	if g.Plan() != plan {
		t.Fatal("immutable physical plan was cloned")
	}
	if _, ok := g.Owner(0); ok {
		t.Fatal("zero physical ID accepted")
	}
	if _, ok := p.Node(3); ok {
		t.Fatal("out-of-range member accepted")
	}
	if _, ok := g.Pipeline(2); ok {
		t.Fatal("out-of-range pipeline accepted")
	}
}

func TestHashBuildCompletesBeforeProbeChain(t *testing.T) {
	f := newFixture(t)
	left, right := f.values(), f.values()
	l := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: left.Output[0].ID}})
	r := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: right.Output[0].ID}})
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 410, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{f.integer, f.integer}})
	if err != nil {
		t.Fatal(err)
	}
	predicate := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Signature: signature, Operator: 410, Strict: true}, Operands: []ir.ScalarID{l, r}})
	ref := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: predicate}, Inputs: []ir.RelID{left.Logical.Rel, right.Logical.Rel}, Outputs: []model.SlotID{left.Output[0].ID, right.Output[0].ID}})
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	join := physical.NodeSpec{Logical: ref, Algorithm: physical.HashJoin, Children: []physical.NodeID{1, 2}, Output: []physical.Slot{left.Output[0], right.Output[0]}, Required: []physical.Properties{single(), single()}, Delivered: single(), Break: physical.BuildRight, Memory: physical.Memory{PeakBytes: 1024}, Join: physical.JoinSpec{Keys: []physical.JoinKey{{Left: physical.Key{Slot: left.Output[0], Type: typ}, Right: physical.Key{Slot: right.Output[0], Type: typ}, Equality: 410, Nulls: physical.NullsUnequal, Predicate: physical.ScalarRef{Graph: 1, Scalar: predicate}}}}}
	g := graphFor(t, f.plan(left, right, join))
	e := onlyEdge(t, g, HashBuild, 2, 3)
	leftOwner, _ := g.Owner(1)
	joinOwner, _ := g.Owner(3)
	if leftOwner != joinOwner || e.Port != 1 || e.Consumer != joinOwner {
		t.Fatal("probe chain separated from hash state consumer")
	}
	p, _ := g.Pipeline(joinOwner)
	if p.ParallelEligible() {
		t.Fatal("hash state lacks a parallel build contract")
	}
	assertOrder(t, g)
}

func TestMaterializationHasDistinctFillAndReadPipelines(t *testing.T) {
	f := newFixture(t)
	values := f.values()
	materialize := physical.NodeSpec{Logical: values.Logical, Algorithm: physical.Materialize, Children: []physical.NodeID{1}, Output: values.Output, Required: []physical.Properties{single()}, Delivered: single(), Break: physical.MaterializeInput, Memory: physical.Memory{PeakBytes: 1024}}
	g := graphFor(t, f.plan(values, materialize))
	e := onlyEdge(t, g, MaterializedInput, 1, 2)
	owner, _ := g.Owner(2)
	if owner != e.Consumer {
		t.Fatal("materialized state has no read-side owner")
	}
	// Metadata is returned by value; callers cannot mutate the scheduling graph.
	e.Kind = Stream
	again, _ := g.Edge(1)
	if again.Kind != MaterializedInput {
		t.Fatal("edge mutation escaped into graph")
	}
	assertOrder(t, g)
}

func TestLocalExchangeRequiresConcurrentEndpoints(t *testing.T) {
	f := newFixture(t)
	values := f.values()
	exchange := physical.NodeSpec{Logical: values.Logical, Algorithm: physical.LocalGather, Children: []physical.NodeID{1}, Output: values.Output, Required: []physical.Properties{single()}, Delivered: single(), Break: physical.ExchangeBoundary, Memory: physical.Memory{PeakBytes: 4096}, Exchange: physical.ExchangeSpec{Inputs: 1, QueueBytes: 4096}}
	g := graphFor(t, f.plan(values, exchange))
	e := onlyEdge(t, g, LocalExchange, 1, 2)
	receive, _ := g.Plan().Node(e.Output)
	send, _ := g.Plan().Node(e.Input)
	required, ok := receive.Required(e.Port)
	if !ok || required.Rewindability() != send.Delivered().Rewindability() {
		t.Fatal("exchange lost its physical input property linkage")
	}
	if receive.Metadata().Exchange.QueueBytes != 4096 {
		t.Fatal("bounded queue contract lost")
	}
	assertOrder(t, g)
}

func TestSharedCTEProducerExecutesOnce(t *testing.T) {
	f := newFixture(t)
	body := f.values()
	cte, err := f.b.DeclareCTE(t.Context(), ir.CTESpec{Scope: f.scope, Outputs: []model.SlotID{body.Output[0].ID}, Materialization: ir.MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), cte, body.Logical.Rel); err != nil {
		t.Fatal(err)
	}
	producerRel := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEProducer, Scope: f.scope, CTE: cte}, Inputs: []ir.RelID{body.Logical.Rel}, Outputs: []model.SlotID{body.Output[0].ID}})
	producer := physical.NodeSpec{Logical: producerRel, Algorithm: physical.CTEMaterialize, Children: []physical.NodeID{1}, Output: body.Output, Required: []physical.Properties{single()}, Delivered: single(), Break: physical.MaterializeInput, Memory: physical.Memory{PeakBytes: 1024}, Worktable: physical.WorktableSpec{Graph: 1, CTE: cte}}
	consumer := func() physical.NodeSpec {
		out := f.slot()
		ref := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: []model.SlotID{out.ID}})
		return physical.NodeSpec{Logical: ref, Algorithm: physical.CTEScan, Output: []physical.Slot{out}, Delivered: single(), Break: physical.Streaming, Worktable: physical.WorktableSpec{Graph: 1, CTE: cte, Producer: 2}}
	}
	left, right := consumer(), consumer()
	unionRel := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.SetOp, Scope: f.scope, SetKind: ir.Union, All: true}, Inputs: []ir.RelID{left.Logical.Rel, right.Logical.Rel}, Outputs: []model.SlotID{left.Output[0].ID}})
	union := physical.NodeSpec{Logical: unionRel, Algorithm: physical.UnionAll, Children: []physical.NodeID{3, 4}, Output: left.Output, Required: []physical.Properties{single(), single()}, Delivered: single(), Break: physical.Streaming}
	g := graphFor(t, f.plan(body, producer, left, right, union))
	owner, _ := g.Owner(2)
	if g.PipelineCount() != 3 || g.EdgeCount() != 3 {
		t.Fatalf("shared producer graph = %d pipelines, %d edges", g.PipelineCount(), g.EdgeCount())
	}
	readers := 0
	for i := EdgeID(1); uint32(i) <= g.EdgeCount(); i++ {
		e, _ := g.Edge(i)
		if e.Kind == CTEDemand {
			readers++
			if e.Input != 2 || e.Producer != owner {
				t.Fatalf("reader uses cloned producer: %+v", e)
			}
		}
	}
	if readers != 2 {
		t.Fatalf("materialized readers = %d, want 2", readers)
	}
	assertOrder(t, g)
}

func TestRecursiveFeedbackIsNotADependencyCycle(t *testing.T) {
	f := newFixture(t)
	anchor := f.values()
	out := anchor.Output[0]
	cte, err := f.b.DeclareCTE(t.Context(), ir.CTESpec{Scope: f.scope, Outputs: []model.SlotID{out.ID}, Recursive: true})
	if err != nil {
		t.Fatal(err)
	}
	memberRel := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEConsumer, Scope: f.scope, CTE: cte, RecursiveReference: true}, Outputs: []model.SlotID{out.ID}})
	unionRel := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.RecursiveUnion, Scope: f.scope, CTE: cte, All: true}, Inputs: []ir.RelID{anchor.Logical.Rel, memberRel.Rel}, Outputs: []model.SlotID{out.ID}})
	if err := f.b.BindCTE(t.Context(), cte, unionRel.Rel); err != nil {
		t.Fatal(err)
	}
	member := physical.NodeSpec{Logical: memberRel, Algorithm: physical.WorktableScan, Output: anchor.Output, Delivered: single(), Break: physical.Streaming, Worktable: physical.WorktableSpec{Graph: 1, CTE: cte, Producer: 3}}
	union := physical.NodeSpec{Logical: unionRel, Algorithm: physical.RecursiveUnion, Children: []physical.NodeID{1, 2}, Output: anchor.Output, Required: []physical.Properties{single(), single()}, Delivered: single(), Break: physical.MaterializeBoth, Memory: physical.Memory{PeakBytes: 1024}, Worktable: physical.WorktableSpec{Graph: 1, CTE: cte}}
	g := graphFor(t, f.plan(anchor, member, union))
	if g.EdgeCount() != 2 || g.FeedbackCount() != 1 {
		t.Fatalf("recursive graph has %d DAG edges and %d feedback links", g.EdgeCount(), g.FeedbackCount())
	}
	feedback, _ := g.Feedback(0)
	if feedback.Union != 3 || feedback.Scan != 2 {
		t.Fatalf("feedback identity = %+v", feedback)
	}
	memberPipeline, _ := g.Pipeline(feedback.Member)
	anchorPipeline, _ := g.Pipeline(feedback.Anchor)
	if memberPipeline.Iteration() != 3 || anchorPipeline.Iteration() != 0 {
		t.Fatal("anchor/member activation domains lost")
	}
	seed, _ := g.Edge(1)
	step, _ := g.Edge(2)
	if seed.Kind != RecursiveSeed || step.Kind != RecursiveStep {
		t.Fatalf("recursive gates = %v, %v", seed.Kind, step.Kind)
	}
	assertOrder(t, g)
}

func TestBuildLimitsRejectBeforePublishing(t *testing.T) {
	plan := chain(t)
	cases := []struct {
		name   string
		change func(*Limits)
	}{
		{"nodes", func(l *Limits) { l.MaxNodes = 2 }},
		{"references", func(l *Limits) { l.MaxEdges = 1 }},
		{"depth", func(l *Limits) { l.MaxDepth = 2 }},
		{"work", func(l *Limits) { l.MaxWork = 1 }},
		{"bytes", func(l *Limits) { l.MaxBytes = 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := buildLimits()
			tc.change(&limits)
			g, err := Build(t.Context(), plan, limits)
			if g != nil || !errors.Is(err, ErrLimit) {
				t.Fatalf("graph = %v, error = %v; want bounded failure", g, err)
			}
		})
	}
	if g, err := Build(t.Context(), plan, Limits{}); g != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero limits: graph %v, error %v", g, err)
	}
}

type cancelAfterChecks struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelAfterChecks) Err() error {
	c.checks--
	if c.checks == 0 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestCancellationDuringBuildReturnsNoGraph(t *testing.T) {
	plan := chain(t)
	for _, checks := range []int{1, 20} {
		ctx, cancel := context.WithCancel(t.Context())
		wrapped := &cancelAfterChecks{Context: ctx, cancel: cancel, checks: checks}
		g, err := Build(wrapped, plan, buildLimits())
		cancel()
		if g != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel after %d checks: graph %v, error %v", checks, g, err)
		}
	}
}

func TestSharedStreamingSourceKeepsOneOwner(t *testing.T) {
	f := newFixture(t)
	values := f.values()
	ref := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.SetOp, Scope: f.scope, SetKind: ir.Union, All: true}, Inputs: []ir.RelID{values.Logical.Rel, values.Logical.Rel}, Outputs: []model.SlotID{values.Output[0].ID}})
	union := physical.NodeSpec{Logical: ref, Algorithm: physical.UnionAll, Children: []physical.NodeID{1, 1}, Output: values.Output, Required: []physical.Properties{single(), single()}, Delivered: single(), Break: physical.Streaming}
	g := graphFor(t, f.plan(values, union))
	if g.PipelineCount() != 2 || g.EdgeCount() != 2 {
		t.Fatal("shared source was duplicated or fused into both readers")
	}
	left, _ := g.Edge(1)
	right, _ := g.Edge(2)
	if left.Kind != Stream || right.Kind != Stream || left.Producer != right.Producer || left.Port != 0 || right.Port != 1 {
		t.Fatalf("shared readers = %+v, %+v", left, right)
	}
	producer, _ := g.Pipeline(left.Producer)
	if producer.NodeCount() != 1 || producer.OutgoingCount() != 2 {
		t.Fatal("shared output does not have one producer and two sinks")
	}
	assertOrder(t, g)
}

func TestVolatilePredicateDisablesMorselParallelism(t *testing.T) {
	f := newFixture(t)
	scan := f.scan()
	effects := ir.Effects{Volatility: ir.Volatile}
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 99, Result: f.boolean, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	predicate := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Signature: signature, Function: 99, Effects: effects}})
	ref := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: predicate, Effects: effects}, Inputs: []ir.RelID{scan.Logical.Rel}, Outputs: []model.SlotID{scan.Output[0].ID}})
	filter := physical.NodeSpec{Logical: ref, Algorithm: physical.Filter, Children: []physical.NodeID{1}, Output: scan.Output, Required: []physical.Properties{single()}, Delivered: single(), Break: physical.Streaming, Expressions: []physical.ScalarRef{{Graph: 1, Scalar: predicate}}}
	g := graphFor(t, f.plan(scan, filter))
	p, _ := g.Pipeline(g.Root())
	if p.ParallelEligible() {
		t.Fatal("volatile filter was admitted to independent morsel execution")
	}
}

func TestSetOperationMaterializesBothInputsWithoutRecursion(t *testing.T) {
	f := newFixture(t)
	left, right := f.values(), f.values()
	ref := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.SetOp, Scope: f.scope, SetKind: ir.Except, All: true}, Inputs: []ir.RelID{left.Logical.Rel, right.Logical.Rel}, Outputs: []model.SlotID{left.Output[0].ID}})
	set := physical.NodeSpec{Logical: ref, Algorithm: physical.HashSetOp, Children: []physical.NodeID{1, 2}, Output: left.Output, Required: []physical.Properties{single(), single()}, Delivered: single(), Break: physical.MaterializeBoth, Memory: physical.Memory{PeakBytes: 1024}}
	g := graphFor(t, f.plan(left, right, set))
	if g.EdgeCount() != 2 || g.FeedbackCount() != 0 {
		t.Fatalf("set dependencies=%d feedback=%d", g.EdgeCount(), g.FeedbackCount())
	}
	ports := uint32(0)
	for id := EdgeID(1); id <= 2; id++ {
		edge, _ := g.Edge(id)
		if edge.Kind != MaterializedInput || edge.Consumer != g.Root() || edge.Output != 3 {
			t.Fatalf("set input has incorrect activation: %+v", edge)
		}
		ports |= 1 << edge.Port
	}
	if ports != 3 {
		t.Fatal("set operator lost one input")
	}
}
