package opt

import (
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func testOptions() Options {
	return Options{IR: ir.Limits{MaxNodes: 4096, MaxOperands: 16384, MaxSlots: 256, MaxDepth: 64, MaxBytes: 4 << 20, SourceBytes: 4096}, Plan: physical.Limits{MaxNodes: 512, MaxOperands: 8192, MaxProperties: 8192, MaxDepth: 128, MaxBytes: 2 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Hour}
}

type fixture struct {
	t               *testing.T
	b               *ir.Builder
	scope           ir.ScopeID
	layout, boolean ir.LayoutID
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	b, err := ir.NewBuilder(testOptions().IR)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	boolean, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{t: t, b: b, scope: scope, layout: layout, boolean: boolean}
}
func (f fixture) scalar(spec ir.ScalarSpec) ir.ScalarID {
	f.t.Helper()
	id, err := f.b.AddScalar(f.t.Context(), spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f fixture) rel(spec ir.RelSpec) ir.RelID {
	f.t.Helper()
	id, err := f.b.AddRel(f.t.Context(), spec)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}
func (f fixture) build(root ir.RelID) *ir.Graph {
	f.t.Helper()
	graph, err := f.b.Build(f.t.Context(), root)
	if err != nil {
		f.t.Fatal(err)
	}
	return graph
}
func (f fixture) source(object model.ObjectID) (ir.RelID, model.SlotID) {
	f.t.Helper()
	slot, err := f.b.AddSlot(f.t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Object: object, Column: 1})
	if err != nil {
		f.t.Fatal(err)
	}
	id := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: object, Generation: 1, DescriptorVersion: 1}, Outputs: []model.SlotID{slot}})
	return id, slot
}
func (f fixture) values(rows uint64) (ir.RelID, model.SlotID) {
	f.t.Helper()
	slot, err := f.b.AddSlot(f.t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
	if err != nil {
		f.t.Fatal(err)
	}
	constant := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(1)})
	cells := make([]ir.ScalarID, rows)
	for i := range cells {
		cells[i] = constant
	}
	return f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: rows}, Outputs: []model.SlotID{slot}, Expressions: cells}), slot
}
func (f fixture) cross(left, right ir.RelID, outputs ...model.SlotID) ir.RelID {
	return f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.CrossJoin}, Inputs: []ir.RelID{left, right}, Outputs: outputs})
}
func (f fixture) comparison(left model.SlotID, right ir.ScalarID, operator model.OperatorID) ir.ScalarID {
	f.t.Helper()
	lhs := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: left}})
	signature, err := f.b.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: operator, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{f.layout, f.layout}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Signature: signature, Operator: operator, Strict: true}, Operands: []ir.ScalarID{lhs, right}})
}

type accessStub struct {
	paths   []physical.Algorithm
	cancel  context.CancelFunc
	failure error
}

func (s accessStub) Access(ctx context.Context, request AccessRequest) (Access, error) {
	if err := ctx.Err(); err != nil {
		return Access{}, err
	}
	if s.failure != nil {
		return Access{}, s.failure
	}
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	result := Access{Target: storage.ObjectRef{ObjectID: request.Object, Generation: request.Generation, DescriptorVersion: request.DescriptorVersion, EncodingVersion: 1}, Layout: []storage.ColumnLayout{{ID: 1, Type: typ}}}
	algorithms := s.paths
	if len(algorithms) == 0 {
		algorithms = []physical.Algorithm{physical.TableScan}
	}
	for _, algorithm := range algorithms {
		path := AccessPath{Target: result.Target, Layout: slices.Clone(result.Layout), ColumnMap: []physical.ScanColumn{{Source: 1, Target: 1}}, Algorithm: algorithm, Keys: []storage.KeyColumn{{ColumnID: 1, Type: typ}}, Operators: []model.OperatorID{412}}
		if algorithm == physical.TableScan {
			path.Keys, path.Operators = nil, nil
		}
		if algorithm == physical.SecondaryScan {
			path.Index = 7
			path.Target.ObjectID = 7
		}
		result.Paths = append(result.Paths, path)
	}
	return result, nil
}
func (s accessStub) Negotiate(ctx context.Context, request storage.PushdownRequest, maxBytes uint64) (physical.ScanSpec, error) {
	if err := ctx.Err(); err != nil {
		return physical.ScanSpec{}, err
	}
	if s.cancel != nil {
		s.cancel()
	}
	result := physical.ScanSpec{Request: request, CapabilityEpoch: 1, Projection: slices.Clone(request.ColumnIDs)}
	if len(request.KeyLayout) > 0 {
		key := request.KeyLayout[0]
		result.Ordering = []storage.ScanOrdering{{KeyOrdinal: 0, Type: key.Type, NullsFirst: key.NullsFirst, Descending: key.Descending}}
	}
	return result, nil
}

type statisticsStub struct {
	rows         statistics.Count
	blocks       statistics.Count
	version      float64
	versionKnown bool
	missing      bool
}

func (s statisticsStub) Lookup(ctx context.Context, target storage.ObjectRef) (*statistics.Table, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.missing {
		return nil, false, nil
	}
	return &statistics.Table{Target: target, Epoch: 1, Snapshot: 1, LiveRows: s.rows, Blocks: s.blocks, Confidence: 1, VersionChainWork: s.version, VersionChainWorkKnown: s.versionKnown}, true, nil
}
func lower(t *testing.T, plan *physical.Plan) *fragment.FragmentGraph {
	t.Helper()
	graph, err := fragment.Build(t.Context(), plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 2048, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}
func rootMetadata(t *testing.T, plan *physical.Plan) physical.Metadata {
	t.Helper()
	root, ok := plan.Node(plan.Root())
	if !ok {
		t.Fatal("missing physical root")
	}
	return root.Metadata()
}

func TestTinySearchBudgetReturnsCompleteOriginalOrderSeed(t *testing.T) {
	f := newFixture(t)
	left, l := f.source(10)
	right, r := f.source(20)
	graph := f.build(f.cross(left, right, l, r))
	options := testOptions()
	options.Access = accessStub{}
	options.SearchTime = time.Nanosecond
	result, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	if !result.BudgetExhausted || rootMetadata(t, result.Plan).Algorithm != physical.NestedLoopJoin {
		t.Fatalf("expected complete bounded nested-loop seed: %+v", result)
	}
	root, _ := result.Plan.Node(result.Plan.Root())
	rightID, _ := root.Child(1)
	inner, _ := result.Plan.Node(rightID)
	if inner.Delivered().Rewindability() != physical.Rewindable {
		t.Fatal("nested-loop inner lacks materialization")
	}
	if rootMetadata(t, result.Plan).Logical.Rel != graph.Root() {
		t.Fatal("seed lost original root")
	}
	lower(t, result.Plan)
}
func TestCancellationAfterNegotiationReturnsNoPlan(t *testing.T) {
	f := newFixture(t)
	scan, _ := f.source(10)
	graph := f.build(scan)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	options := testOptions()
	options.Access = accessStub{cancel: cancel}
	result, err := Optimize(ctx, graph, options)
	if !errors.Is(err, context.Canceled) || result.Plan != nil {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}
func TestMissingStatisticsDifferFromKnownEmpty(t *testing.T) {
	f := newFixture(t)
	scan, _ := f.source(10)
	graph := f.build(scan)
	options := testOptions()
	options.Access = accessStub{}
	options.Statistics = statisticsStub{missing: true}
	missing, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	options.Statistics = statisticsStub{rows: statistics.Count{Known: true}, blocks: statistics.Count{Known: true}, versionKnown: true}
	empty, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	unknown, zero := rootMetadata(t, missing.Plan), rootMetadata(t, empty.Plan)
	if unknown.Estimate.Rows != 1000 || unknown.Estimate.Confidence > 0.1 || unknown.Cost.CPUBatches != 10 {
		t.Fatalf("unknown prior=%+v", unknown)
	}
	if zero.Estimate.Rows != 0 || zero.Estimate.UpperRows != 0 || zero.Cost.VersionMerge != 0 {
		t.Fatalf("known-empty=%+v", zero)
	}
}
func TestPhysicalOrderingAndRewindAreEnforced(t *testing.T) {
	f := newFixture(t)
	values, slot := f.values(3)
	graph := f.build(values)
	options := testOptions()
	options.Required = physical.Properties{Rewindability: physical.Rewindable, Ordering: []physical.OrderKey{{Key: physical.Key{Slot: physical.Slot{Graph: 1, ID: slot, Layout: f.layout}, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, Direction: ir.Ascending, Nulls: ir.NullsLast, Operator: 412}}}
	result, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := result.Plan.Node(result.Plan.Root())
	if root.Delivered().OrderingCount() != 1 || root.Delivered().Rewindability() != physical.Rewindable {
		t.Fatal("required root properties not delivered")
	}
	lower(t, result.Plan)
}
func TestAccessAlternativesAreDeterministic(t *testing.T) {
	f := newFixture(t)
	scan, _ := f.source(10)
	graph := f.build(scan)
	options := testOptions()
	options.Access = accessStub{paths: []physical.Algorithm{physical.SecondaryScan, physical.PrimaryKeyScan, physical.TableScan}}
	first, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	options.Access = accessStub{paths: []physical.Algorithm{physical.TableScan, physical.PrimaryKeyScan, physical.SecondaryScan, physical.TableScan}}
	second, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	a, b := rootMetadata(t, first.Plan), rootMetadata(t, second.Plan)
	if a.Algorithm != b.Algorithm || a.Cost != b.Cost || first.MemoExpressions != second.MemoExpressions {
		t.Fatalf("inventory order changed decision: %+v / %+v", a, b)
	}
}
func TestJoinReorderRebuildsGraphAndPreservesOutput(t *testing.T) {
	f := newFixture(t)
	a, as := f.values(100)
	b, bs := f.values(100)
	c, cs := f.values(1)
	ab := f.cross(a, b, as, bs)
	graph := f.build(f.cross(ab, c, as, bs, cs))
	options := testOptions()
	options.SearchTime = time.Nanosecond
	seed, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	options.SearchTime = time.Hour
	reordered, err := Optimize(t.Context(), graph, options)
	if err != nil {
		t.Fatal(err)
	}
	if rootMetadata(t, reordered.Plan).Cost.Total >= rootMetadata(t, seed.Plan).Cost.Total {
		t.Fatalf("no cheaper join order: seed=%g winner=%g", rootMetadata(t, seed.Plan).Cost.Total, rootMetadata(t, reordered.Plan).Cost.Total)
	}
	registered, _ := reordered.Plan.Graph(1)
	if registered == graph || registered.Root() == graph.Root() {
		t.Fatal("join reorder did not register a rebuilt logical root")
	}
	root, _ := reordered.Plan.Node(reordered.Plan.Root())
	for i, want := range []model.SlotID{as, bs, cs} {
		output, _ := root.Output(uint32(i))
		if output.ID != want {
			t.Fatalf("output %d=%d want %d", i, output.ID, want)
		}
	}
	lower(t, reordered.Plan)
}
func TestBoundedRangesRetainFilterAndDoNotPushLimit(t *testing.T) {
	f := newFixture(t)
	scan, slot := f.source(10)
	constant := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(9)})
	predicate := f.comparison(slot, constant, 410)
	filter := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: predicate}, Inputs: []ir.RelID{scan}, Outputs: []model.SlotID{slot}})
	count := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(1)})
	limit := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Limit, Scope: f.scope, Count: count, Effects: ir.Effects{MayError: true}}, Inputs: []ir.RelID{filter}, Outputs: []model.SlotID{slot}})
	options := testOptions()
	options.Access = accessStub{paths: []physical.Algorithm{physical.PrimaryKeyScan}}
	result, err := Optimize(t.Context(), f.build(limit), options)
	if err != nil {
		t.Fatal(err)
	}
	foundFilter := false
	for id := physical.NodeID(1); id <= physical.NodeID(result.Plan.NodeCount()); id++ {
		node, err := result.Plan.CloneNode(t.Context(), id, options.Plan.MaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		if node.Algorithm == physical.Filter {
			foundFilter = true
		}
		if node.Algorithm == physical.PrimaryKeyScan {
			if node.Scan.Request.SafeLimit.Enabled || node.Scan.AppliedLimit.Enabled {
				t.Fatal("unsafe scan limit below residual filter")
			}
			if len(node.Scan.Request.Spans) != 1 || node.Scan.Request.Spans[0].Lower.Unbounded || node.Scan.Request.Spans[0].Lower.Values[0].IntVal != 9 {
				t.Fatalf("typed equality range lost: %+v", node.Scan.Request.Spans)
			}
		}
	}
	if !foundFilter {
		t.Fatal("logical residual filter was removed")
	}
	lower(t, result.Plan)
}
func TestHardLimitsAndInvalidSoftLimitsReturnNoPlan(t *testing.T) {
	f := newFixture(t)
	values, _ := f.values(1)
	graph := f.build(values)
	for _, change := range []func(*Options){func(o *Options) { o.MaxBytes = 1 }, func(o *Options) { o.IR.MaxBytes = 1 }, func(o *Options) { o.MemoBytes = -1 }, func(o *Options) { o.MemoExpressions = -1 }, func(o *Options) { o.SearchTime = -1 }} {
		options := testOptions()
		change(&options)
		result, err := Optimize(t.Context(), graph, options)
		if err == nil || result.Plan != nil {
			t.Fatalf("invalid limits returned plan: %+v, %v", result, err)
		}
	}
	options := testOptions()
	options.MemoBytes, options.MemoExpressions, options.SearchTime = 0, 0, 0
	if _, err := Optimize(t.Context(), graph, options); err != nil {
		t.Fatalf("missing soft budgets should select defaults: %v", err)
	}
}
func TestHashJoinUsesResolvedEquality(t *testing.T) {
	f := newFixture(t)
	left, l := f.source(10)
	right, r := f.source(20)
	rhs := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: r}})
	predicate := f.comparison(l, rhs, 410)
	join := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: predicate}, Inputs: []ir.RelID{left, right}, Outputs: []model.SlotID{l, r}})
	options := testOptions()
	options.Access = accessStub{}
	result, err := Optimize(t.Context(), f.build(join), options)
	if err != nil {
		t.Fatal(err)
	}
	if rootMetadata(t, result.Plan).Algorithm != physical.HashJoin {
		t.Fatalf("expected hash join over two large equality inputs, got %+v", rootMetadata(t, result.Plan))
	}
	lower(t, result.Plan)
}
func TestParameterPolicyUsesFiveCustomObservations(t *testing.T) {
	var policy ParameterPolicy
	if err := policy.ObserveGeneric(110); err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if policy.Mode() != CustomPlan {
			t.Fatalf("generic chosen before custom observation %d", i+1)
		}
		if err := policy.ObserveCustom(100, 20*time.Millisecond, 1000); err != nil {
			t.Fatal(err)
		}
	}
	if policy.Mode() != GenericPlan {
		t.Fatal("generic cost should beat custom plus planning")
	}
	if err := policy.ObserveCustom(math.NaN(), 0, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nonfinite observation error=%v", err)
	}
	if err := policy.ObserveGeneric(121); err != nil {
		t.Fatal(err)
	}
	if policy.Mode() != CustomPlan {
		t.Fatal("expensive generic plan chosen")
	}
}

func TestGenerateSeriesRewindRequiresMaterialization(t *testing.T) {
	f := newFixture(t)
	slot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
	if err != nil {
		t.Fatal(err)
	}
	start := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(math.MinInt64)})
	stop := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Param, Scope: f.scope, Layout: f.layout, Parameter: 1}})
	series := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.GenerateSeries, Scope: f.scope, Effects: ir.Effects{MayError: true}}, Outputs: []model.SlotID{slot}, Expressions: []ir.ScalarID{start, stop}})
	options := testOptions()
	options.Required.Rewindability = physical.Rewindable
	result, err := Optimize(t.Context(), f.build(series), options)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := result.Plan.Node(result.Plan.Root())
	if root.Metadata().Algorithm != physical.Materialize || root.Delivered().Rewindability() != physical.Rewindable {
		t.Fatalf("rewindable series needs materialization, got %+v", root.Metadata())
	}
	sourceID, _ := root.Child(0)
	source, _ := result.Plan.Node(sourceID)
	if source.Metadata().Algorithm != physical.GenerateSeries || source.Delivered().Rewindability() != physical.OnePass {
		t.Fatalf("series source must remain one-pass, got %+v", source.Metadata())
	}
	if estimate := source.Metadata().Estimate; estimate.LowerRows != 0 || estimate.UpperRows < 1<<64 || estimate.Confidence >= 1 {
		t.Fatalf("unknown bounds need a conservative estimate: %+v", estimate)
	}
	fragments := lower(t, result.Plan)
	owner, _ := fragments.Owner(sourceID)
	pipeline, _ := fragments.Pipeline(owner)
	if pipeline.ParallelEligible() {
		t.Fatal("series without a morsel contract cannot run in parallel")
	}
}
