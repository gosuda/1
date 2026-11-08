package physical

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func testLimits() Limits {
	return Limits{MaxNodes: 64, MaxOperands: 512, MaxProperties: 512, MaxDepth: 32, MaxBytes: 1 << 20}
}
func single() Properties {
	return Properties{Distribution: Distribution{Kind: SingleNode, Partitions: 1}, Rewindability: OnePass}
}

type fixture struct {
	t               *testing.T
	b               *ir.Builder
	scope           ir.ScopeID
	layout, boolean ir.LayoutID
	typ             model.Type
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 256, MaxOperands: 1024, MaxSlots: 64, MaxDepth: 32, MaxBytes: 1 << 20, SourceBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	f := fixture{t: t, b: b, typ: model.Type{ID: 20, Storage: model.TypeInt64}}
	f.scope, err = b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.layout, err = b.AddLayout(t.Context(), model.TypeLayout{Type: f.typ})
	if err != nil {
		t.Fatal(err)
	}
	f.boolean, err = b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func (f fixture) rel(s ir.RelSpec) ir.RelID {
	f.t.Helper()
	id, err := f.b.AddRel(f.t.Context(), s)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f fixture) scalar(s ir.ScalarSpec) ir.ScalarID {
	f.t.Helper()
	id, err := f.b.AddScalar(f.t.Context(), s)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f fixture) scan(object model.ObjectID) NodeSpec {
	f.t.Helper()
	slot, err := f.b.AddSlot(f.t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Object: object, Column: 1})
	if err != nil {
		f.t.Fatal(err)
	}
	r := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: object, Generation: 1, DescriptorVersion: 1}, Outputs: []model.SlotID{slot}})
	return NodeSpec{Logical: RelRef{1, r}, Algorithm: TableScan, Output: []Slot{{1, slot, f.layout, false}}, Delivered: single(), Break: Streaming,
		Scan: ScanSpec{Source: storage.ObjectRef{ObjectID: object, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, ColumnMap: []ScanColumn{{Source: 1, Target: 1}}, CapabilityEpoch: 1, Request: storage.PushdownRequest{Target: storage.ObjectRef{ObjectID: object, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Layout: []storage.ColumnLayout{{ID: 1, Type: f.typ}}, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: f.typ}}, ColumnIDs: []model.ColumnID{1}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}, Projection: []model.ColumnID{1}}}
}
func (f fixture) spec(nodes ...NodeSpec) Spec {
	f.t.Helper()
	g, err := f.b.Build(f.t.Context(), nodes[len(nodes)-1].Logical.Rel)
	if err != nil {
		f.t.Fatal(err)
	}
	return Spec{Graphs: []*ir.Graph{g}, Nodes: nodes, Root: NodeID(len(nodes))}
}
func requireInvalid(t *testing.T, spec Spec) {
	t.Helper()
	plan, err := NewPlan(t.Context(), spec, testLimits())
	if plan != nil || !errors.Is(err, ErrInvalid) {
		t.Fatalf("plan = %v, error = %v; want invalid plan", plan, err)
	}
}

func TestScanOwnershipSurvivesCallerMutation(t *testing.T) {
	f := newFixture(t)
	node := f.scan(10)
	node.Scan.Request.Spans[0].Lower = storage.KeyBound{Values: []model.Value{model.Int64Val(7)}, Inclusive: true}
	spec := f.spec(node)
	plan, err := NewPlan(t.Context(), spec, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	spec.Nodes[0].Output[0].ID = 999
	spec.Nodes[0].Scan.Request.Spans[0].Lower.Values[0].IntVal = 99
	cloned, err := plan.CloneNode(t.Context(), 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if cloned.Scan.Request.Spans[0].Lower.Values[0].IntVal != 7 {
		t.Fatal("caller changed owned key bound")
	}
	cloned.Scan.Request.Spans[0].Lower.Values[0].IntVal = -1
	cloned.Output[0].ID = 999
	view, ok := plan.Node(1)
	if !ok {
		t.Fatal("missing scan")
	}
	output, ok := view.Output(0)
	if !ok || output.ID == 999 {
		t.Fatal("mutable output alias escaped")
	}
	again, err := plan.CloneNode(t.Context(), 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if again.Scan.Request.Spans[0].Lower.Values[0].IntVal != 7 {
		t.Fatal("mutable scan clone alias escaped")
	}
	if _, ok := view.Output(math.MaxUint32); ok {
		t.Fatal("accepted invalid output index")
	}
	if _, ok := plan.Node(0); ok {
		t.Fatal("accepted absent node")
	}
}

func TestScanRejectsStaleIdentityAndTypeClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*NodeSpec)
	}{
		{"generation", func(n *NodeSpec) { n.Scan.Request.Target.Generation++ }},
		{"descriptor", func(n *NodeSpec) { n.Scan.Request.Target.DescriptorVersion++ }},
		{"source missing", func(n *NodeSpec) { n.Scan.Source = storage.ObjectRef{} }},
		{"source generation", func(n *NodeSpec) { n.Scan.Source.Generation++ }},
		{"mapping missing", func(n *NodeSpec) { n.Scan.ColumnMap = nil }},
		{"mapping duplicate", func(n *NodeSpec) { n.Scan.ColumnMap = append(n.Scan.ColumnMap, n.Scan.ColumnMap[0]) }},
		{"mapping target absent", func(n *NodeSpec) { n.Scan.ColumnMap[0].Target = 2 }},
		{"mapping source absent", func(n *NodeSpec) { n.Scan.ColumnMap[0].Source = 2 }},
		{"requested projection missing", func(n *NodeSpec) { n.Scan.Request.ColumnIDs = nil }},
		{"synthetic secondary identity", func(n *NodeSpec) { n.Algorithm = SecondaryScan; n.Scan.Request.IndexID = 7 }},
		{"output layout", func(n *NodeSpec) { n.Output[0].Layout++ }},
		{"output nullability", func(n *NodeSpec) { n.Output[0].Nullable = true }},
		{"projection", func(n *NodeSpec) { n.Scan.Projection = nil }},
		{"key type", func(n *NodeSpec) { n.Scan.Request.KeyLayout[0].Type.ID = 23 }},
		{"NaN estimate", func(n *NodeSpec) { n.Estimate.Rows = math.NaN() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			n := f.scan(10)
			spec := f.spec(n)
			tc.change(&spec.Nodes[0])
			requireInvalid(t, spec)
		})
	}
}

func TestSecondaryScanMapsLogicalOutputToStoredColumns(t *testing.T) {
	f := newFixture(t)
	node := f.scan(10)
	node.Algorithm = SecondaryScan
	node.Scan.Request.Target.ObjectID = 20
	node.Scan.Request.IndexID = 20
	node.Scan.ColumnMap = []ScanColumn{{Source: 1, Target: 7}}
	node.Scan.Request.Layout[0].ID = 7
	node.Scan.Request.KeyLayout[0].ColumnID = 7
	node.Scan.Request.ColumnIDs[0] = 7
	node.Scan.Projection[0] = 7
	plan, err := NewPlan(t.Context(), f.spec(node), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	node.Scan.ColumnMap[0].Target = 99
	owned, err := plan.CloneNode(t.Context(), 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if owned.Scan.PhysicalColumn(1) != 7 || owned.Scan.LogicalColumn(7) != 1 || owned.Scan.LogicalColumn(99) != 0 {
		t.Fatalf("scan mapping changed: %+v", owned.Scan.ColumnMap)
	}
}

func TestExchangeRequiresRepresentedPartitioning(t *testing.T) {
	f := newFixture(t)
	scan := f.scan(10)
	key := Key{Slot: scan.Output[0], Type: f.typ}
	partitioned := single()
	partitioned.Distribution = Distribution{Kind: HashPartitioned, Keys: []Key{key}, Partitions: 4, Function: 1, Epoch: 7}
	exchange := NodeSpec{Logical: scan.Logical, Algorithm: LocalHashRepartition, Children: []NodeID{1}, Output: scan.Output, Required: []Properties{single()}, Delivered: partitioned, Break: ExchangeBoundary, Memory: Memory{PeakBytes: 4096}, Exchange: ExchangeSpec{Inputs: 1, QueueBytes: 4096}}
	spec := f.spec(scan, exchange)
	plan, err := NewPlan(t.Context(), spec, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	view, _ := plan.Node(plan.Root())
	kind, partitions, _, epoch := view.Delivered().Distribution()
	if kind != HashPartitioned || partitions != 4 || epoch != 7 {
		t.Fatal("partition property lost")
	}
	spec.Nodes[1].Delivered.Distribution.Epoch = 0
	requireInvalid(t, spec)
	spec.Nodes[1].Delivered.Distribution.Epoch = 7
	spec.Nodes[1].Delivered.Distribution.Keys[0].Collation = 100
	requireInvalid(t, spec)
	spec.Nodes[1].Delivered.Distribution.Keys[0].Collation = 0
	spec.Nodes[1].Delivered.Distribution.Kind = RangePartitioned
	requireInvalid(t, spec)
}

func TestOrderedMergeRejectsUnorderedInput(t *testing.T) {
	f := newFixture(t)
	scan := f.scan(10)
	ordered := single()
	ordered.Ordering = []OrderKey{{Key: Key{Slot: scan.Output[0], Type: f.typ}, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 412}}
	merge := NodeSpec{Logical: scan.Logical, Algorithm: LocalOrderedMerge, Children: []NodeID{1}, Output: scan.Output, Required: []Properties{single()}, Delivered: ordered, Break: ExchangeBoundary, Memory: Memory{PeakBytes: 1024}, Exchange: ExchangeSpec{Inputs: 1, QueueBytes: 1024}}
	requireInvalid(t, f.spec(scan, merge))
}

func TestLimitsBoundConstructionAndCancellation(t *testing.T) {
	f := newFixture(t)
	scan := f.scan(10)
	materialize := NodeSpec{Logical: scan.Logical, Algorithm: Materialize, Children: []NodeID{1}, Output: scan.Output, Required: []Properties{single()}, Delivered: single(), Break: MaterializeInput, Memory: Memory{PeakBytes: 1024}}
	spec := f.spec(scan, materialize)
	for _, tc := range []struct {
		name   string
		change func(*Limits)
	}{
		{"nodes", func(l *Limits) { l.MaxNodes = 1 }},
		{"operands", func(l *Limits) { l.MaxOperands = 1 }},
		{"properties", func(l *Limits) { l.MaxProperties = 1 }},
		{"depth", func(l *Limits) { l.MaxDepth = 1 }},
		{"bytes", func(l *Limits) { l.MaxBytes = 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := testLimits()
			tc.change(&limits)
			plan, err := NewPlan(t.Context(), spec, limits)
			if plan != nil || !errors.Is(err, ErrLimit) {
				t.Fatalf("plan = %v, error = %v; want resource limit", plan, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewPlan(ctx, spec, testLimits()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
	spec.Nodes[1].Children[0] = 2
	requireInvalid(t, spec)
}

func TestPushedLimitRequiresMatchingGlobalDemand(t *testing.T) {
	f := newFixture(t)
	scan := f.scan(10)
	count := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(5)})
	logical := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Limit, Scope: f.scope, Count: count, Effects: ir.Effects{MayError: true}}, Inputs: []ir.RelID{scan.Logical.Rel}, Outputs: []model.SlotID{scan.Output[0].ID}})
	scan.Scan.Request.SafeLimit = storage.ScanLimit{Enabled: true, Count: 5}
	scan.Scan.AppliedLimit = scan.Scan.Request.SafeLimit
	scan.Scan.LimitOwner = 2
	limit := NodeSpec{Logical: RelRef{1, logical}, Algorithm: Limit, Children: []NodeID{1}, Output: scan.Output, Required: []Properties{single()}, Delivered: single(), Break: Streaming, Limit: LimitSpec{Scope: GlobalLimit, Count: 5}}
	spec := f.spec(scan, limit)
	plan, err := NewPlan(t.Context(), spec, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if plan.Root() != 2 {
		t.Fatal("global limit lost")
	}
	spec.Nodes[0].Scan.Request.SafeLimit.Count = 4
	spec.Nodes[0].Scan.AppliedLimit.Count = 4
	requireInvalid(t, spec)
	spec.Nodes[0].Scan.Request.SafeLimit.Count = 5
	spec.Nodes[0].Scan.AppliedLimit.Count = 5
	spec.Nodes[1].Delivered.Distribution = Distribution{Kind: Replicated, Partitions: 2, Epoch: 1}
	requireInvalid(t, spec)
}

func TestHashJoinRequiresExactTypedConjunct(t *testing.T) {
	f := newFixture(t)
	left, right := f.scan(10), f.scan(20)
	l := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: left.Output[0].ID}})
	r := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: right.Output[0].ID}})
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 410, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{f.layout, f.layout}})
	if err != nil {
		t.Fatal(err)
	}
	predicate := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Signature: signature, Operator: 410, Strict: true}, Operands: []ir.ScalarID{l, r}})
	logical := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: predicate}, Inputs: []ir.RelID{left.Logical.Rel, right.Logical.Rel}, Outputs: []model.SlotID{left.Output[0].ID, right.Output[0].ID}})
	join := NodeSpec{Logical: RelRef{1, logical}, Algorithm: HashJoin, Children: []NodeID{1, 2}, Output: []Slot{left.Output[0], right.Output[0]}, Required: []Properties{single(), single()}, Delivered: single(), Break: BuildRight, Memory: Memory{PeakBytes: 1024}, Join: JoinSpec{Keys: []JoinKey{{Left: Key{Slot: left.Output[0], Type: f.typ}, Right: Key{Slot: right.Output[0], Type: f.typ}, Equality: 410, Nulls: NullsUnequal, Predicate: ScalarRef{1, predicate}}}}}
	spec := f.spec(left, right, join)
	if _, err := NewPlan(t.Context(), spec, testLimits()); err != nil {
		t.Fatal(err)
	}
	t.Run("partition epochs", func(t *testing.T) {
		leftProperties, rightProperties := single(), single()
		leftProperties.Distribution = Distribution{Kind: HashPartitioned, Keys: []Key{join.Join.Keys[0].Left}, Partitions: 2, Function: 1, Epoch: 7}
		rightProperties.Distribution = Distribution{Kind: HashPartitioned, Keys: []Key{join.Join.Keys[0].Right}, Partitions: 2, Function: 1, Epoch: 7}
		leftExchange := NodeSpec{Logical: left.Logical, Algorithm: LocalHashRepartition, Children: []NodeID{1}, Output: left.Output, Required: []Properties{single()}, Delivered: leftProperties, Break: ExchangeBoundary, Memory: Memory{PeakBytes: 1024}, Exchange: ExchangeSpec{Inputs: 1, QueueBytes: 1024}}
		rightExchange := NodeSpec{Logical: right.Logical, Algorithm: LocalHashRepartition, Children: []NodeID{3}, Output: right.Output, Required: []Properties{single()}, Delivered: rightProperties, Break: ExchangeBoundary, Memory: Memory{PeakBytes: 1024}, Exchange: ExchangeSpec{Inputs: 1, QueueBytes: 1024}}
		partitionJoin := join
		partitionJoin.Children = []NodeID{2, 4}
		partitionJoin.Required = []Properties{leftProperties, rightProperties}
		partitionJoin.Delivered = leftProperties
		partitioned := Spec{Graphs: spec.Graphs, Nodes: []NodeSpec{left, leftExchange, right, rightExchange, partitionJoin}, Root: 5}
		if _, err := NewPlan(t.Context(), partitioned, testLimits()); err != nil {
			t.Fatal(err)
		}
		partitioned.Nodes[3].Delivered.Distribution.Epoch = 8
		partitioned.Nodes[4].Required[1].Distribution.Epoch = 8
		requireInvalid(t, partitioned)
	})
	spec.Nodes[2].Join.Keys[0].Nulls = NullsEqual
	requireInvalid(t, spec)
	spec.Nodes[2].Join.Keys[0].Nulls = NullsUnequal
	spec.Nodes[2].Join.Keys[0].Right.Type.ID = 23
	requireInvalid(t, spec)
	spec.Nodes[2].Join.Keys = nil
	requireInvalid(t, spec)
}

func TestHashJoinRejectsOrderedComparisonAsEquality(t *testing.T) {
	f := newFixture(t)
	left, right := f.scan(10), f.scan(20)
	l := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: left.Output[0].ID}})
	r := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: right.Output[0].ID}})
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 412, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{f.layout, f.layout}})
	if err != nil {
		t.Fatal(err)
	}
	predicate := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Signature: signature, Operator: 412, Strict: true}, Operands: []ir.ScalarID{l, r}})
	logical := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: predicate}, Inputs: []ir.RelID{left.Logical.Rel, right.Logical.Rel}, Outputs: []model.SlotID{left.Output[0].ID, right.Output[0].ID}})
	join := NodeSpec{Logical: RelRef{1, logical}, Algorithm: HashJoin, Children: []NodeID{1, 2}, Output: []Slot{left.Output[0], right.Output[0]}, Required: []Properties{single(), single()}, Delivered: single(), Break: BuildRight, Memory: Memory{PeakBytes: 1024}, Join: JoinSpec{Keys: []JoinKey{{Left: Key{Slot: left.Output[0], Type: f.typ}, Right: Key{Slot: right.Output[0], Type: f.typ}, Equality: 412, Nulls: NullsUnequal, Predicate: ScalarRef{1, predicate}}}}}
	requireInvalid(t, f.spec(left, right, join))
}

func TestAllMarkRejectsEqualityPartitionedWitnesses(t *testing.T) {
	f := newFixture(t)
	left, right := f.scan(10), f.scan(20)
	l := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: left.Output[0].ID}})
	r := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: right.Output[0].ID}})
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 410, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{f.layout, f.layout}})
	if err != nil {
		t.Fatal(err)
	}
	predicate := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Signature: signature, Operator: 410, Strict: true}, Operands: []ir.ScalarID{l, r}})
	mark, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.boolean, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	logical := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.MarkJoin, MarkKind: ir.AllSubquery, MarkSlot: mark, Predicate: predicate}, Inputs: []ir.RelID{left.Logical.Rel, right.Logical.Rel}, Outputs: []model.SlotID{left.Output[0].ID, mark}})
	key := JoinKey{Left: Key{Slot: left.Output[0], Type: f.typ}, Right: Key{Slot: right.Output[0], Type: f.typ}, Equality: 410, Nulls: NullsUnequal, Predicate: ScalarRef{1, predicate}}
	join := NodeSpec{Logical: RelRef{1, logical}, Algorithm: HashJoin, Children: []NodeID{1, 2}, Output: []Slot{left.Output[0], {Graph: 1, ID: mark, Layout: f.boolean, Nullable: true}}, Required: []Properties{single(), single()}, Delivered: single(), Break: BuildRight, Memory: Memory{PeakBytes: 1024}, Join: JoinSpec{MarkKind: ir.AllSubquery, NullAware: true, Keys: []JoinKey{key}}}
	spec := f.spec(left, right, join)
	if _, err := NewPlan(t.Context(), spec, testLimits()); err != nil {
		t.Fatal(err)
	}
	leftProperties, rightProperties := single(), single()
	leftProperties.Distribution = Distribution{Kind: HashPartitioned, Keys: []Key{key.Left}, Partitions: 2, Function: 1, Epoch: 7}
	rightProperties.Distribution = Distribution{Kind: HashPartitioned, Keys: []Key{key.Right}, Partitions: 2, Function: 1, Epoch: 7}
	leftExchange := NodeSpec{Logical: left.Logical, Algorithm: LocalHashRepartition, Children: []NodeID{1}, Output: left.Output, Required: []Properties{single()}, Delivered: leftProperties, Break: ExchangeBoundary, Memory: Memory{PeakBytes: 1024}, Exchange: ExchangeSpec{Inputs: 1, QueueBytes: 1024}}
	rightExchange := NodeSpec{Logical: right.Logical, Algorithm: LocalHashRepartition, Children: []NodeID{3}, Output: right.Output, Required: []Properties{single()}, Delivered: rightProperties, Break: ExchangeBoundary, Memory: Memory{PeakBytes: 1024}, Exchange: ExchangeSpec{Inputs: 1, QueueBytes: 1024}}
	join.Children = []NodeID{2, 4}
	join.Required = []Properties{leftProperties, rightProperties}
	join.Delivered = leftProperties
	requireInvalid(t, Spec{Graphs: spec.Graphs, Nodes: []NodeSpec{left, leftExchange, right, rightExchange, join}, Root: 5})
}

func TestGenerateSeriesRejectsUnprovenDelivery(t *testing.T) {
	f := newFixture(t)
	value, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
	if err != nil {
		t.Fatal(err)
	}
	ordinalType := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	ordinalLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: ordinalType})
	if err != nil {
		t.Fatal(err)
	}
	ordinal, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: ordinalLayout})
	if err != nil {
		t.Fatal(err)
	}
	start := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(1)})
	stop := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Param, Scope: f.scope, Layout: f.layout, Parameter: 1}})
	logical := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.GenerateSeries, Scope: f.scope, Effects: ir.Effects{MayError: true}}, Outputs: []model.SlotID{value, ordinal}, Expressions: []ir.ScalarID{start, stop}})
	output := []Slot{{Graph: 1, ID: value, Layout: f.layout}, {Graph: 1, ID: ordinal, Layout: ordinalLayout}}
	source := NodeSpec{Logical: RelRef{1, logical}, Algorithm: GenerateSeries, Output: output, Delivered: single(), Break: Streaming, Expressions: []ScalarRef{{1, start}, {1, stop}}}
	spec := f.spec(source)
	if _, err := NewPlan(t.Context(), spec, testLimits()); err != nil {
		t.Fatal(err)
	}
	ordinalKey := Key{Slot: output[1], Type: ordinalType}
	ordered := single()
	ordered.Ordering = []OrderKey{{Key: ordinalKey, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 412}}
	partitioned := single()
	partitioned.Distribution = Distribution{Kind: HashPartitioned, Keys: []Key{ordinalKey}, Partitions: 2, Function: 1, Epoch: 1}
	rewindable := single()
	rewindable.Rewindability = Rewindable
	for _, test := range []struct {
		name       string
		properties Properties
	}{
		{name: "ordinal ordering", properties: ordered},
		{name: "partitioning", properties: partitioned},
		{name: "reevaluation", properties: rewindable},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec.Nodes[0].Delivered = test.properties
			requireInvalid(t, spec)
		})
	}
}

func TestCTEMaterializationPreservesNestedCompositeLayout(t *testing.T) {
	spec := materializedCompositeSpec(t, model.Type{ID: 1043, Modifier: 11, Collation: 100, Storage: model.TypeString})
	if spec.Nodes[1].Output[0].Layout == spec.Nodes[2].Output[0].Layout {
		t.Fatal("fixture must allocate producer and consumer layouts independently")
	}
	if _, err := NewPlan(t.Context(), spec, testLimits()); err != nil {
		t.Fatalf("materializing an equivalent nested composite layout: %v", err)
	}
}

func TestMaterializedDeliveryRequiresNestedSQLType(t *testing.T) {
	field := model.Type{ID: 1043, Modifier: 11, Collation: 100, Storage: model.TypeString}
	input := materializedCompositeSpec(t, field)
	for _, test := range []struct {
		name  string
		field model.Type
	}{
		{name: "type", field: model.Type{ID: 25, Modifier: 11, Collation: 100, Storage: model.TypeString}},
		{name: "typmod", field: model.Type{ID: 1043, Modifier: 12, Collation: 100, Storage: model.TypeString}},
		{name: "collation", field: model.Type{ID: 1043, Modifier: 11, Collation: 101, Storage: model.TypeString}},
	} {
		t.Run(test.name, func(t *testing.T) {
			output := materializedCompositeSpec(t, test.field)
			plan := Plan{graphs: []*ir.Graph{input.Graphs[0], output.Graphs[0]}}
			before, after := input.Nodes[2], output.Nodes[2]
			after.Output[0].Graph = 2
			if before.Output[0].Layout != after.Output[0].Layout {
				t.Fatal("fixture must use equal graph-local layout IDs")
			}
			if err := plan.validateOrdinalDelivery(t.Context(), after, before); !errors.Is(err, ErrInvalid) {
				t.Fatalf("materializing incompatible nested %s: error = %v, want invalid plan", test.name, err)
			}
		})
	}
}

func materializedCompositeSpec(t *testing.T, field model.Type) Spec {
	t.Helper()
	f := newFixture(t)
	layout := model.TypeLayout{
		Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite},
		Children: []model.TypeLayout{{
			Type:     model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite},
			Children: []model.TypeLayout{{Type: field}},
		}},
	}
	slot := func(nullable bool) Slot {
		id, err := f.b.AddLayout(t.Context(), layout)
		if err != nil {
			t.Fatal(err)
		}
		output, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: id, Nullable: nullable})
		if err != nil {
			t.Fatal(err)
		}
		return Slot{Graph: 1, ID: output, Layout: id, Nullable: nullable}
	}
	producerSlot, consumerSlot := slot(false), slot(true)
	value := f.scalar(ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: producerSlot.Layout},
		Value: model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{{
			Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal("value")},
		}}},
	})
	body := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{producerSlot.ID}, Expressions: []ir.ScalarID{value}})
	cte, err := f.b.DeclareCTE(t.Context(), ir.CTESpec{Scope: f.scope, Outputs: []model.SlotID{producerSlot.ID}, Materialization: ir.MaterializeAlways})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.b.BindCTE(t.Context(), cte, body); err != nil {
		t.Fatal(err)
	}
	producer := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEProducer, Scope: f.scope, CTE: cte}, Inputs: []ir.RelID{body}, Outputs: []model.SlotID{producerSlot.ID}})
	consumer := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.CTEConsumer, Scope: f.scope, CTE: cte}, Outputs: []model.SlotID{consumerSlot.ID}})
	return f.spec(
		NodeSpec{Logical: RelRef{1, body}, Algorithm: Values, Output: []Slot{producerSlot}, Expressions: []ScalarRef{{1, value}}, Delivered: single(), Break: Streaming},
		NodeSpec{Logical: RelRef{1, producer}, Algorithm: CTEMaterialize, Children: []NodeID{1}, Output: []Slot{producerSlot}, Required: []Properties{single()}, Delivered: single(), Break: MaterializeInput, Memory: Memory{PeakBytes: 1024}, Worktable: WorktableSpec{Graph: 1, CTE: cte}},
		NodeSpec{Logical: RelRef{1, consumer}, Algorithm: CTEScan, Output: []Slot{consumerSlot}, Delivered: single(), Break: Streaming, Worktable: WorktableSpec{Graph: 1, CTE: cte, Producer: 2}},
	)
}
