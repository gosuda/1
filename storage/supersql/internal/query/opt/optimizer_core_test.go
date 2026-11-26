package opt

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func findPlanScans(t *testing.T, plan *physical.Plan, maxBytes uint64) map[model.ObjectID]physical.NodeSpec {
	t.Helper()
	scans := make(map[model.ObjectID]physical.NodeSpec)
	for id := physical.NodeID(1); id <= physical.NodeID(plan.NodeCount()); id++ {
		node, err := plan.CloneNode(t.Context(), id, maxBytes)
		if err != nil {
			t.Fatal(err)
		}
		if node.Algorithm >= physical.TableScan && node.Algorithm <= physical.SecondaryScan {
			scans[node.Scan.Source.ObjectID] = node
		}
	}
	return scans
}

func planAlgorithms(t *testing.T, plan *physical.Plan, maxBytes uint64) []physical.Algorithm {
	t.Helper()
	algs := make([]physical.Algorithm, 0, plan.NodeCount())
	for id := physical.NodeID(1); id <= physical.NodeID(plan.NodeCount()); id++ {
		node, err := plan.CloneNode(t.Context(), id, maxBytes)
		if err != nil {
			t.Fatal(err)
		}
		algs = append(algs, node.Algorithm)
	}
	return algs
}

func nullableComparison(f *fixture, left model.SlotID, leftNullable bool, right ir.ScalarID, rightNullable bool, operator model.OperatorID) ir.ScalarID {
	f.t.Helper()
	lhs := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Nullable: leftNullable, Slot: left}})
	nullable := leftNullable || rightNullable
	signature, err := f.b.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: operator, Result: f.boolean, Strict: true, Nullable: nullable}, Parameters: []ir.LayoutID{f.layout, f.layout}})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Nullable: nullable, Signature: signature, Operator: operator, Strict: true}, Operands: []ir.ScalarID{lhs, right}})
}

func TestParameterizedStoragePredicatePushdown(t *testing.T) {
	// Test col = $1 and col > $1 push both storage.Predicate and narrow KeySpan.
	for _, tc := range []struct {
		name     string
		operator model.OperatorID
		value    int64
	}{
		{"eq", 410, 42},
		{"gt", 413, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			scan, slot := f.source(10)
			param := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Param, Scope: f.scope, Layout: f.layout, Nullable: true, Parameter: 1}})
			pred := nullableComparison(&f, slot, false, param, true, tc.operator)
			filter := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: pred}, Inputs: []ir.RelID{scan}, Outputs: []model.SlotID{slot}})
			graph := f.build(filter)

			// Without Parameters: storage predicate must NOT be populated, span stays unbounded.
			genericOpts := testOptions()
			genericOpts.Access = accessStub{paths: []physical.Algorithm{physical.PrimaryKeyScan}}
			genericRes, err := Optimize(t.Context(), graph, genericOpts)
			if err != nil {
				t.Fatal(err)
			}
			genericScan := findPlanScans(t, genericRes.Plan, genericOpts.Plan.MaxBytes)[10]
			if genericScan.Scan.Request.Predicate.NodeCount() != 0 {
				t.Fatalf("generic plan unexpectedly pushed unbound param predicate: %+v", genericScan.Scan.Request.Predicate)
			}
			if len(genericScan.Scan.Request.Spans) != 1 || !genericScan.Scan.Request.Spans[0].Lower.Unbounded {
				t.Fatalf("generic plan unexpectedly narrowed span: %+v", genericScan.Scan.Request.Spans)
			}

			// With bound Parameter: storage predicate substitutes literal AND span narrows.
			boundOpts := testOptions()
			boundOpts.Access = accessStub{paths: []physical.Algorithm{physical.PrimaryKeyScan}}
			boundOpts.Parameters = []span.Parameter{{
				Number: 1,
				Layout: model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}},
				Value:  model.Int64Val(tc.value),
			}}
			boundRes, err := Optimize(t.Context(), graph, boundOpts)
			if err != nil {
				t.Fatal(err)
			}
			boundScan := findPlanScans(t, boundRes.Plan, boundOpts.Plan.MaxBytes)[10]
			storagePred := boundScan.Scan.Request.Predicate
			if storagePred.NodeCount() != 3 {
				t.Fatalf("expected 3-node parameterized storage predicate, got nodes=%d", storagePred.NodeCount())
			}
			rootNode := storagePred.Node(storagePred.Root())
			if rootNode.Kind != storage.PredicateComparison || rootNode.Operator != tc.operator {
				t.Fatalf("storage predicate root = %+v, want operator %d", rootNode, tc.operator)
			}
			constNode := storagePred.Node(rootNode.Right)
			if constNode.Kind != storage.PredicateConstant {
				t.Fatalf("expected constant RHS node, got %+v", constNode)
			}
			c := storagePred.Constant(constNode.ConstantIndex)
			if c.IsNull || c.IntVal != tc.value {
				t.Fatalf("storage predicate constant = %+v, want %d", c, tc.value)
			}
			if len(boundScan.Scan.Request.Spans) != 1 || boundScan.Scan.Request.Spans[0].Lower.Unbounded || boundScan.Scan.Request.Spans[0].Lower.Values[0].IntVal != tc.value {
				t.Fatalf("parameterized span not narrowed to %d: %+v", tc.value, boundScan.Scan.Request.Spans)
			}
			lower(t, boundRes.Plan)
		})
	}

	// Non-nullable ir.Param must NOT substitute a NULL parameter value into storage predicate.
	f := newFixture(t)
	scan, slot := f.source(10)
	param := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Param, Scope: f.scope, Layout: f.layout, Nullable: false, Parameter: 1}})
	pred := f.comparison(slot, param, 410)
	filter := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: pred}, Inputs: []ir.RelID{scan}, Outputs: []model.SlotID{slot}})
	graph := f.build(filter)
	opts := testOptions()
	opts.Access = accessStub{paths: []physical.Algorithm{physical.TableScan}}
	opts.Parameters = []span.Parameter{{
		Number: 1,
		Layout: model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}},
		Value:  model.NullVal(model.TypeInt64),
	}}
	res, err := Optimize(t.Context(), graph, opts)
	if err != nil {
		t.Fatal(err)
	}
	sc := findPlanScans(t, res.Plan, opts.Plan.MaxBytes)[10]
	if sc.Scan.Request.Predicate.NodeCount() != 0 {
		t.Fatalf("non-nullable param substituted NULL into storage predicate")
	}
}

func TestReversedJoinKeysPickHashJoin(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind ir.JoinKind
	}{
		{"inner", ir.InnerJoin},
		{"left", ir.LeftJoin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			left, l := f.source(10)
			right, r := f.source(20)
			// Reversed join equality: b.id = a.id (RHS slot first, LHS slot second).
			lhsSlot := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: l}})
			predicate := f.comparison(r, lhsSlot, 410)
			outR := r
			if tc.kind == ir.LeftJoin {
				var err error
				outR, err = f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true})
				if err != nil {
					t.Fatal(err)
				}
			}
			join := f.rel(ir.RelSpec{
				Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: tc.kind, Predicate: predicate},
				Inputs:  []ir.RelID{left, right},
				Outputs: []model.SlotID{l, outR},
			})
			options := testOptions()
			options.Access = accessStub{}
			result, err := Optimize(t.Context(), f.build(join), options)
			if err != nil {
				t.Fatal(err)
			}
			algs := planAlgorithms(t, result.Plan, options.Plan.MaxBytes)
			hasHash := false
			for _, alg := range algs {
				if alg == physical.HashJoin {
					hasHash = true
				}
				if alg == physical.NestedLoopJoin {
					t.Fatalf("reversed join equality fell back to NestedLoopJoin: %v", algs)
				}
			}
			if !hasHash {
				t.Fatalf("expected HashJoin for reversed join equality, got algorithms %v", algs)
			}
			lower(t, result.Plan)
		})
	}
}

func TestFilterPushdownThroughJoin(t *testing.T) {
	buildJoinWithFilter := func(t *testing.T, kind ir.JoinKind, filterOnA, filterOnB bool, useOr bool) (*physical.Plan, Options) {
		t.Helper()
		f := newFixture(t)
		a, as := f.source(10)
		b, bs := f.source(20)
		bSlot := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: bs}})
		joinPred := f.comparison(as, bSlot, 410)
		joinKind := kind
		if kind == ir.CrossJoin {
			joinPred = 0
		}
		outA, outB := as, bs
		nullA := kind == ir.RightJoin || kind == ir.FullJoin
		nullB := kind == ir.LeftJoin || kind == ir.FullJoin
		if nullA {
			var err error
			outA, err = f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true})
			if err != nil {
				t.Fatal(err)
			}
		}
		if nullB {
			var err error
			outB, err = f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Nullable: true})
			if err != nil {
				t.Fatal(err)
			}
		}
		join := f.rel(ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: joinKind, Predicate: joinPred},
			Inputs:  []ir.RelID{a, b},
			Outputs: []model.SlotID{outA, outB},
		})
		var conjuncts []ir.ScalarID
		predNullable := false
		if filterOnA {
			cA := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(42)})
			conjuncts = append(conjuncts, nullableComparison(&f, outA, nullA, cA, false, 410))
			predNullable = predNullable || nullA
		}
		if filterOnB {
			cB := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(7)})
			conjuncts = append(conjuncts, nullableComparison(&f, outB, nullB, cB, false, 410))
			predNullable = predNullable || nullB
		}
		pred := conjuncts[0]
		if len(conjuncts) > 1 {
			op := ir.And
			if useOr {
				op = ir.Or
			}
			pred = f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: f.scope, Layout: f.boolean, Nullable: predNullable, BoolOp: op}, Operands: conjuncts})
		}
		filter := f.rel(ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: pred},
			Inputs:  []ir.RelID{join},
			Outputs: []model.SlotID{outA, outB},
		})
		opts := testOptions()
		opts.Access = accessStub{paths: []physical.Algorithm{physical.PrimaryKeyScan}}
		res, err := Optimize(t.Context(), f.build(filter), opts)
		if err != nil {
			t.Fatal(err)
		}
		lower(t, res.Plan)
		return res.Plan, opts
	}

	isNarrowedTo := func(scan physical.NodeSpec, want int64) bool {
		if len(scan.Scan.Request.Spans) != 1 || scan.Scan.Request.Spans[0].Lower.Unbounded {
			return false
		}
		return scan.Scan.Request.Spans[0].Lower.Values[0].IntVal == want && scan.Scan.Request.Predicate.NodeCount() > 0
	}

	// 1. InnerJoin: WHERE a.x = 42 AND b.y = 7 pushes to BOTH a and b.
	plan, opts := buildJoinWithFilter(t, ir.InnerJoin, true, true, false)
	scans := findPlanScans(t, plan, opts.Plan.MaxBytes)
	if !isNarrowedTo(scans[10], 42) || !isNarrowedTo(scans[20], 7) {
		t.Fatalf("InnerJoin failed to push filters to both scans: a=%+v b=%+v", scans[10].Scan.Request.Spans, scans[20].Scan.Request.Spans)
	}

	// 2. CrossJoin: WHERE a.x = 42 pushes to a.
	plan, opts = buildJoinWithFilter(t, ir.CrossJoin, true, false, false)
	scans = findPlanScans(t, plan, opts.Plan.MaxBytes)
	if !isNarrowedTo(scans[10], 42) || !scans[20].Scan.Request.Spans[0].Lower.Unbounded {
		t.Fatalf("CrossJoin pushdown mismatch: a=%+v b=%+v", scans[10].Scan.Request.Spans, scans[20].Scan.Request.Spans)
	}

	// 3. LeftJoin: WHERE a.x = 42 AND b.y = 7 pushes to a (preserved) ONLY, NOT b (nullable).
	plan, opts = buildJoinWithFilter(t, ir.LeftJoin, true, true, false)
	scans = findPlanScans(t, plan, opts.Plan.MaxBytes)
	if !isNarrowedTo(scans[10], 42) {
		t.Fatalf("LeftJoin failed to push preserved-side filter to a: %+v", scans[10].Scan.Request.Spans)
	}
	if !scans[20].Scan.Request.Spans[0].Lower.Unbounded || scans[20].Scan.Request.Predicate.NodeCount() != 0 {
		t.Fatalf("LeftJoin unsafely pushed WHERE filter onto nullable side b: %+v", scans[20].Scan.Request)
	}

	// 4. RightJoin: WHERE a.x = 42 AND b.y = 7 pushes to b (preserved) ONLY, NOT a (nullable).
	plan, opts = buildJoinWithFilter(t, ir.RightJoin, true, true, false)
	scans = findPlanScans(t, plan, opts.Plan.MaxBytes)
	if !isNarrowedTo(scans[20], 7) {
		t.Fatalf("RightJoin failed to push preserved-side filter to b: %+v", scans[20].Scan.Request.Spans)
	}
	if !scans[10].Scan.Request.Spans[0].Lower.Unbounded || scans[10].Scan.Request.Predicate.NodeCount() != 0 {
		t.Fatalf("RightJoin unsafely pushed WHERE filter onto nullable side a: %+v", scans[10].Scan.Request)
	}

	// 5. FullJoin: pushes to NEITHER side.
	plan, opts = buildJoinWithFilter(t, ir.FullJoin, true, true, false)
	scans = findPlanScans(t, plan, opts.Plan.MaxBytes)
	if !scans[10].Scan.Request.Spans[0].Lower.Unbounded || !scans[20].Scan.Request.Spans[0].Lower.Unbounded {
		t.Fatalf("FullJoin unsafely pushed WHERE filter: a=%+v b=%+v", scans[10].Scan.Request.Spans, scans[20].Scan.Request.Spans)
	}

	// 6. Disjunction (a.x = 42 OR b.y = 7) above InnerJoin: must NOT push to either side.
	plan, opts = buildJoinWithFilter(t, ir.InnerJoin, true, true, true)
	scans = findPlanScans(t, plan, opts.Plan.MaxBytes)
	if !scans[10].Scan.Request.Spans[0].Lower.Unbounded || !scans[20].Scan.Request.Spans[0].Lower.Unbounded {
		t.Fatalf("OR across join sides was unsafely pushed: a=%+v b=%+v", scans[10].Scan.Request.Spans, scans[20].Scan.Request.Spans)
	}
}

func TestHashJoinBuildSideSelectsSmallerSide(t *testing.T) {
	f := newFixture(t)
	small, sSlot := f.values(5)
	big, bSlot := f.values(500)
	bExpr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: bSlot}})
	pred := f.comparison(sSlot, bExpr, 410)
	join := f.rel(ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: pred},
		Inputs:  []ir.RelID{small, big},
		Outputs: []model.SlotID{sSlot, bSlot},
	})
	graph := f.build(join)
	opts := testOptions()

	orig, err := optimizeOriginal(t.Context(), graph, opts)
	if err != nil {
		t.Fatal(err)
	}
	optRes, err := Optimize(t.Context(), graph, opts)
	if err != nil {
		t.Fatal(err)
	}
	if rootMetadata(t, optRes.Plan).Cost.Total >= rootMetadata(t, orig.Plan).Cost.Total {
		t.Fatalf("commuted HashJoin did not beat original build-on-big: orig=%g opt=%g", rootMetadata(t, orig.Plan).Cost.Total, rootMetadata(t, optRes.Plan).Cost.Total)
	}
	// Verify the HashJoin builds on the smaller side (Children[1].Estimate.Rows == 5).
	foundHash := false
	for id := physical.NodeID(1); id <= physical.NodeID(optRes.Plan.NodeCount()); id++ {
		node, _ := optRes.Plan.Node(id)
		if node.Metadata().Algorithm != physical.HashJoin {
			continue
		}
		foundHash = true
		probeID, _ := node.Child(0)
		buildID, _ := node.Child(1)
		probe, _ := optRes.Plan.Node(probeID)
		build, _ := optRes.Plan.Node(buildID)
		if build.Metadata().Estimate.Rows != 5 || probe.Metadata().Estimate.Rows != 500 {
			t.Fatalf("HashJoin build side = %g rows, probe side = %g rows; want build=5, probe=500", build.Metadata().Estimate.Rows, probe.Metadata().Estimate.Rows)
		}
	}
	if !foundHash {
		t.Fatal("expected HashJoin in optimized plan")
	}
	requireJoinOutputs(t, optRes.Plan, []model.SlotID{sSlot, bSlot})
	lower(t, optRes.Plan)
}

func TestJoinReorderThroughAggregateLimitDistinctWindowSetOp(t *testing.T) {
	// Helper builds a (1000) JOIN b (1000) ON a=b JOIN c (5) ON b=c.
	buildThreeWayJoin := func(f *fixture) (ir.RelID, model.SlotID, model.SlotID, model.SlotID) {
		a, as := f.values(400)
		b, bs := f.values(400)
		c, cs := f.values(2)
		bExpr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: bs}})
		cExpr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: cs}})
		abPred := f.comparison(as, bExpr, 410)
		bcPred := f.comparison(bs, cExpr, 410)
		ab := f.rel(ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: abPred},
			Inputs:  []ir.RelID{a, b},
			Outputs: []model.SlotID{as, bs},
		})
		abc := f.rel(ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: bcPred},
			Inputs:  []ir.RelID{ab, c},
			Outputs: []model.SlotID{as, bs, cs},
		})
		return abc, as, bs, cs
	}

	t.Run("Aggregate_and_Limit", func(t *testing.T) {
		f := newFixture(t)
		abc, as, _, _ := buildThreeWayJoin(&f)
		groupExpr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: as}})
		aggOut, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
		if err != nil {
			t.Fatal(err)
		}
		agg := f.rel(ir.RelSpec{
			Rel:          ir.Rel{Kind: ir.Aggregate, Scope: f.scope},
			Inputs:       []ir.RelID{abc},
			Outputs:      []model.SlotID{aggOut},
			Expressions:  []ir.ScalarID{groupExpr},
			Bindings:     []ir.Binding{{Slot: aggOut, Expr: groupExpr}},
			GroupingSets: [][]uint32{{0}},
		})
		limitCount := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(10)})
		limit := f.rel(ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.Limit, Scope: f.scope, Count: limitCount, Effects: ir.Effects{MayError: true}},
			Inputs:  []ir.RelID{agg},
			Outputs: []model.SlotID{aggOut},
		})
		graph := f.build(limit)
		opts := testOptions()

		orig, err := optimizeOriginal(t.Context(), graph, opts)
		if err != nil {
			t.Fatal(err)
		}
		reordered, err := Optimize(t.Context(), graph, opts)
		if err != nil {
			t.Fatal(err)
		}
		if rootMetadata(t, reordered.Plan).Cost.Total >= rootMetadata(t, orig.Plan).Cost.Total {
			t.Fatalf("Aggregate+Limit join was not reordered to cheaper plan: orig=%g reordered=%g", rootMetadata(t, orig.Plan).Cost.Total, rootMetadata(t, reordered.Plan).Cost.Total)
		}
		hashCount := 0
		for _, alg := range planAlgorithms(t, reordered.Plan, opts.Plan.MaxBytes) {
			if alg == physical.HashJoin {
				hashCount++
			}
		}
		if hashCount != 2 {
			t.Fatalf("expected 2 HashJoins under Aggregate+Limit, got %d (%v)", hashCount, planAlgorithms(t, reordered.Plan, opts.Plan.MaxBytes))
		}
		lower(t, reordered.Plan)
	})

	t.Run("Distinct", func(t *testing.T) {
		f := newFixture(t)
		abc, as, bs, cs := buildThreeWayJoin(&f)
		distinct := f.rel(ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.Distinct, Scope: f.scope},
			Inputs:  []ir.RelID{abc},
			Outputs: []model.SlotID{as, bs, cs},
		})
		graph := f.build(distinct)
		opts := testOptions()
		orig, err := optimizeOriginal(t.Context(), graph, opts)
		if err != nil {
			t.Fatal(err)
		}
		reordered, err := Optimize(t.Context(), graph, opts)
		if err != nil {
			t.Fatal(err)
		}
		if rootMetadata(t, reordered.Plan).Cost.Total >= rootMetadata(t, orig.Plan).Cost.Total {
			t.Fatalf("Distinct join was not reordered: orig=%g reordered=%g", rootMetadata(t, orig.Plan).Cost.Total, rootMetadata(t, reordered.Plan).Cost.Total)
		}
		lower(t, reordered.Plan)
	})

	t.Run("Window", func(t *testing.T) {
		f := newFixture(t)
		abc, as, bs, cs := buildThreeWayJoin(&f)
		partExpr := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: as}})
		winOut, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
		if err != nil {
			t.Fatal(err)
		}
		winSig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
			Signature: ir.Signature{Function: 3100, Result: f.layout},
		})
		if err != nil {
			t.Fatal(err)
		}
		window := f.rel(ir.RelSpec{
			Rel: ir.Rel{
				Kind:  ir.Window,
				Scope: f.scope,
				Frame: ir.Frame{
					Mode:  ir.RowsFrame,
					Start: ir.FrameBound{Kind: ir.UnboundedPreceding},
					End:   ir.FrameBound{Kind: ir.CurrentRow},
				},
			},
			Inputs:    []ir.RelID{abc},
			Outputs:   []model.SlotID{as, bs, cs, winOut},
			Partition: []ir.ScalarID{partExpr},
			Windows:   []ir.WindowSpec{{WindowCall: ir.WindowCall{Function: 3100, Signature: winSig, Result: winOut}}},
		})
		graph := f.build(window)
		opts := testOptions()
		orig, err := optimizeOriginal(t.Context(), graph, opts)
		if err != nil {
			t.Fatal(err)
		}
		reordered, err := Optimize(t.Context(), graph, opts)
		if err != nil {
			t.Fatal(err)
		}
		if rootMetadata(t, reordered.Plan).Cost.Total >= rootMetadata(t, orig.Plan).Cost.Total {
			t.Fatalf("Window join was not reordered: orig=%g reordered=%g", rootMetadata(t, orig.Plan).Cost.Total, rootMetadata(t, reordered.Plan).Cost.Total)
		}
		lower(t, reordered.Plan)
	})

	t.Run("SetOp", func(t *testing.T) {
		f := newFixture(t)
		abc, as, _, _ := buildThreeWayJoin(&f)
		other, _ := f.values(2)
		projSlot := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: as}})
		proj := f.rel(ir.RelSpec{
			Rel:      ir.Rel{Kind: ir.Project, Scope: f.scope},
			Inputs:   []ir.RelID{abc},
			Outputs:  []model.SlotID{as},
			Bindings: []ir.Binding{{Slot: as, Expr: projSlot}},
		})
		outSlot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout})
		if err != nil {
			t.Fatal(err)
		}
		union := f.rel(ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.SetOp, Scope: f.scope, SetKind: ir.Union, All: true},
			Inputs:  []ir.RelID{proj, other},
			Outputs: []model.SlotID{outSlot},
		})
		graph := f.build(union)
		opts := testOptions()
		orig, err := optimizeOriginal(t.Context(), graph, opts)
		if err != nil {
			t.Fatal(err)
		}
		reordered, err := Optimize(t.Context(), graph, opts)
		if err != nil {
			t.Fatal(err)
		}
		if rootMetadata(t, reordered.Plan).Cost.Total >= rootMetadata(t, orig.Plan).Cost.Total {
			t.Fatalf("SetOp join was not reordered: orig=%g reordered=%g", rootMetadata(t, orig.Plan).Cost.Total, rootMetadata(t, reordered.Plan).Cost.Total)
		}
		lower(t, reordered.Plan)
	})
}
