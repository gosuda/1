package normalize_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func buildPredicateJoinGraph(t *testing.T, subKind ir.SubqueryKind, correlated, negated, barrier bool) *ir.Graph {
	t.Helper()
	f := newFixture(t)
	one := f.constant(f.integer, model.Int64Val(1))
	leftSlot := f.slot(f.integer, false)
	left := f.rel(ir.RelSpec{
		Rel:         ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1},
		Outputs:     []model.SlotID{leftSlot},
		Expressions: []ir.ScalarID{one},
	})

	var captures []model.SlotID
	if correlated {
		captures = []model.SlotID{leftSlot}
	}
	subScope, err := f.b.AddScope(t.Context(), f.scope, captures)
	if err != nil {
		t.Fatal(err)
	}
	rightSlot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: subScope, Layout: f.integer})
	if err != nil {
		t.Fatal(err)
	}
	subOne := f.scalar(ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Const, Scope: subScope, Layout: f.integer},
		Value:  model.Int64Val(1),
	})
	right := f.rel(ir.RelSpec{
		Rel:         ir.Rel{Kind: ir.Values, Scope: subScope, Rows: 1},
		Outputs:     []model.SlotID{rightSlot},
		Expressions: []ir.ScalarID{subOne},
	})
	if correlated {
		sig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
			Signature:  ir.Signature{Operator: 96, Result: f.boolean, Strict: true},
			Parameters: []ir.LayoutID{f.integer, f.integer},
		})
		if err != nil {
			t.Fatal(err)
		}
		outerRef := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: subScope, Layout: f.integer, Slot: leftSlot}})
		innerRef := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: subScope, Layout: f.integer, Slot: rightSlot}})
		eq := f.scalar(ir.ScalarSpec{
			Scalar:   ir.Scalar{Kind: ir.Call, Scope: subScope, Layout: f.boolean, Signature: sig, Operator: 96, Strict: true},
			Operands: []ir.ScalarID{innerRef, outerRef},
		})
		right = f.rel(ir.RelSpec{
			Rel:       ir.Rel{Kind: ir.Filter, Scope: subScope, Predicate: eq},
			Inputs:    []ir.RelID{right},
			Outputs:   []model.SlotID{rightSlot},
		})
	}

	var predicate ir.ScalarID
	switch subKind {
	case ir.ExistsSubquery:
		predicate = f.scalar(ir.ScalarSpec{
			Scalar: ir.Scalar{
				Kind:         ir.Subquery,
				Scope:        f.scope,
				Layout:       f.boolean,
				SubqueryKind: ir.ExistsSubquery,
				Relation:     right,
			},
		})
	case ir.AnySubquery, ir.AllSubquery:
		sig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
			Signature:  ir.Signature{Operator: 96, Result: f.boolean, Strict: true},
			Parameters: []ir.LayoutID{f.integer, f.integer},
		})
		if err != nil {
			t.Fatal(err)
		}
		lhs := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: leftSlot}})
		predicate = f.scalar(ir.ScalarSpec{
			Scalar: ir.Scalar{
				Kind:         ir.Subquery,
				Scope:        f.scope,
				Layout:       f.boolean,
				Nullable:     true,
				SubqueryKind: subKind,
				Relation:     right,
			},
			Operands:    []ir.ScalarID{lhs},
			Comparisons: []ir.SignatureID{sig},
		})
	default:
		t.Fatalf("unsupported subquery kind %d", subKind)
	}

	if negated {
		predicate = f.scalar(ir.ScalarSpec{
			Scalar: ir.Scalar{
				Kind:     ir.Bool,
				BoolOp:   ir.Not,
				Scope:    f.scope,
				Layout:   f.boolean,
				Nullable: subKind != ir.ExistsSubquery,
			},
			Operands: []ir.ScalarID{predicate},
		})
	}

	root := f.rel(ir.RelSpec{
		Rel: ir.Rel{
			Kind:            ir.Filter,
			Scope:           f.scope,
			SecurityBarrier: barrier,
			Predicate:       predicate,
		},
		Inputs:  []ir.RelID{left},
		Outputs: []model.SlotID{leftSlot},
	})
	return f.freeze(root)
}

func TestPredicateJoinPreservesSecurityBarrier(t *testing.T) {
	cases := []struct {
		name       string
		subKind    ir.SubqueryKind
		correlated bool
		negated    bool
		wantOpKind ir.RelKind
	}{
		{name: "uncorrelated EXISTS", subKind: ir.ExistsSubquery, wantOpKind: ir.Join},
		{name: "uncorrelated NOT EXISTS", subKind: ir.ExistsSubquery, negated: true, wantOpKind: ir.Join},
		{name: "correlated EXISTS", subKind: ir.ExistsSubquery, correlated: true, wantOpKind: ir.Apply},
		{name: "correlated NOT EXISTS", subKind: ir.ExistsSubquery, correlated: true, negated: true, wantOpKind: ir.Apply},
		{name: "uncorrelated IN/ANY", subKind: ir.AnySubquery, wantOpKind: ir.Join},
		{name: "uncorrelated NOT IN", subKind: ir.AnySubquery, negated: true, wantOpKind: ir.Join},
		{name: "uncorrelated ALL", subKind: ir.AllSubquery, wantOpKind: ir.Join},
		{name: "correlated IN/ANY", subKind: ir.AnySubquery, correlated: true, wantOpKind: ir.Apply},
		{name: "correlated ALL", subKind: ir.AllSubquery, correlated: true, wantOpKind: ir.Apply},
	}

	for _, tc := range cases {
		for _, barrier := range []bool{true, false} {
			name := tc.name + "/barrier=false"
			if barrier {
				name = tc.name + "/barrier=true"
			}
			t.Run(name, func(t *testing.T) {
				graph := normalized(t, buildPredicateJoinGraph(t, tc.subKind, tc.correlated, tc.negated, barrier))

				projID := graph.Root()
				proj, ok := graph.Rel(projID)
				if !ok || proj.Kind != ir.Project {
					t.Fatalf("root = %+v (ok=%v), want Project from predicateJoin", proj, ok)
				}
				if proj.SecurityBarrier != barrier {
					t.Fatalf("wrapping Project SecurityBarrier = %v, want %v", proj.SecurityBarrier, barrier)
				}

				filterID, ok := graph.Input(projID, 0)
				if !ok {
					t.Fatal("Project missing Filter input")
				}
				filter, ok := graph.Rel(filterID)
				if !ok || filter.Kind != ir.Filter {
					t.Fatalf("Project input = %+v (ok=%v), want Filter from predicateJoin", filter, ok)
				}
				if filter.SecurityBarrier != barrier {
					t.Fatalf("rewritten Filter SecurityBarrier = %v, want %v", filter.SecurityBarrier, barrier)
				}

				opID, ok := graph.Input(filterID, 0)
				if !ok {
					t.Fatal("Filter missing Join/Apply input")
				}
				op, ok := graph.Rel(opID)
				if !ok || op.Kind != tc.wantOpKind {
					t.Fatalf("Filter input = %+v (ok=%v), want kind %d", op, ok, tc.wantOpKind)
				}
				if op.SecurityBarrier {
					t.Fatalf("Join/Apply must not set SecurityBarrier: %+v", op)
				}
			})
		}
	}
}

func TestPredicateJoinSQLNonBarrierKeepsFalse(t *testing.T) {
	queries := []string{
		`SELECT x FROM (VALUES(1),(2)) t(x) WHERE EXISTS (SELECT 1 FROM (VALUES(1)) r(y))`,
		`SELECT x FROM (VALUES(1),(2)) t(x) WHERE NOT EXISTS (SELECT 1 FROM (VALUES(1)) r(y) WHERE r.y = t.x)`,
		`SELECT x FROM (VALUES(1),(2)) t(x) WHERE x IN (SELECT y FROM (VALUES(2)) r(y))`,
		`SELECT x FROM (VALUES(1),(2)) t(x) WHERE x NOT IN (SELECT y FROM (VALUES(2)) r(y))`,
		`SELECT x FROM (VALUES(1),(2)) t(x) WHERE x = ANY (SELECT y FROM (VALUES(2)) r(y))`,
		`SELECT x FROM (VALUES(1),(2)) t(x) WHERE x <> ALL (SELECT y FROM (VALUES(2)) r(y))`,
	}
	for _, sql := range queries {
		t.Run(sql, func(t *testing.T) {
			graph := normalized(t, bound(t, sql))
			foundMarkOp := false
			for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()); id++ {
				rel, _ := graph.Rel(id)
				if rel.SecurityBarrier {
					t.Fatalf("non-barrier SQL query emitted SecurityBarrier=true on rel %d: %+v", id, rel)
				}
				if (rel.Kind == ir.Join && rel.JoinKind == ir.MarkJoin) || (rel.Kind == ir.Apply && (rel.ApplyKind == ir.ExistsApply || rel.ApplyKind == ir.ScalarApply)) {
					foundMarkOp = true
				}
			}
			if !foundMarkOp {
				t.Fatalf("expected predicateJoin lowering for %s", sql)
			}
		})
	}
}

type barrierOptAccessStub struct{}

func (barrierOptAccessStub) Access(ctx context.Context, request opt.AccessRequest) (opt.Access, error) {
	if err := ctx.Err(); err != nil {
		return opt.Access{}, err
	}
	typ := model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}
	target := storage.ObjectRef{
		ObjectID:          request.Object,
		Generation:        request.Generation,
		DescriptorVersion: request.DescriptorVersion,
		EncodingVersion:   1,
	}
	layout := []storage.ColumnLayout{{ID: 1, Type: typ}}
	return opt.Access{
		Target: target,
		Layout: layout,
		Paths: []opt.AccessPath{
			{
				Target:    target,
				Layout:    slices.Clone(layout),
				ColumnMap: []physical.ScanColumn{{Source: 1, Target: 1}},
				Algorithm: physical.PrimaryKeyScan,
				Keys:      []storage.KeyColumn{{ColumnID: 1, Type: typ}},
				Operators: []model.OperatorID{412},
			},
		},
	}, nil
}

func (barrierOptAccessStub) Negotiate(ctx context.Context, request storage.PushdownRequest, _ uint64) (physical.ScanSpec, error) {
	if err := ctx.Err(); err != nil {
		return physical.ScanSpec{}, err
	}
	result := physical.ScanSpec{
		Request:         request,
		CapabilityEpoch: 1,
		Projection:      slices.Clone(request.ColumnIDs),
	}
	if len(request.KeyLayout) > 0 {
		key := request.KeyLayout[0]
		result.Ordering = []storage.ScanOrdering{{KeyOrdinal: 0, Type: key.Type, NullsFirst: key.NullsFirst, Descending: key.Descending}}
	}
	return result, nil
}

func buildScanBarrierSubqueryWithOuterFilter(t *testing.T, subKind ir.SubqueryKind, barrier bool) *ir.Graph {
	t.Helper()
	b, err := ir.NewBuilder(options().Limits)
	if err != nil {
		t.Fatal(err)
	}
	scope, err := b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	bigint, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	boolean, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	scanSlot, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: scope, Layout: bigint, Object: 10, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	scan, err := b.AddRel(t.Context(), ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Scan, Scope: scope, Object: 10, Generation: 1, DescriptorVersion: 1},
		Outputs: []model.SlotID{scanSlot},
	})
	if err != nil {
		t.Fatal(err)
	}

	subScope, err := b.AddScope(t.Context(), scope, nil)
	if err != nil {
		t.Fatal(err)
	}
	subSlot, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: subScope, Layout: bigint})
	if err != nil {
		t.Fatal(err)
	}
	subVal, err := b.AddScalar(t.Context(), ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Const, Scope: subScope, Layout: bigint},
		Value:  model.Int64Val(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	subRel, err := b.AddRel(t.Context(), ir.RelSpec{
		Rel:         ir.Rel{Kind: ir.Values, Scope: subScope, Rows: 1},
		Outputs:     []model.SlotID{subSlot},
		Expressions: []ir.ScalarID{subVal},
	})
	if err != nil {
		t.Fatal(err)
	}

	eqSig, err := b.AddSignature(t.Context(), ir.SignatureSpec{
		Signature:  ir.Signature{Operator: 410, Result: boolean, Strict: true},
		Parameters: []ir.LayoutID{bigint, bigint},
	})
	if err != nil {
		t.Fatal(err)
	}

	var barrierPred ir.ScalarID
	switch subKind {
	case ir.ExistsSubquery:
		barrierPred, err = b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar: ir.Scalar{Kind: ir.Subquery, Scope: scope, Layout: boolean, SubqueryKind: ir.ExistsSubquery, Relation: subRel},
		})
	case ir.AnySubquery:
		lhs, lhsErr := b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar: ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: bigint, Slot: scanSlot},
		})
		if lhsErr != nil {
			t.Fatal(lhsErr)
		}
		barrierPred, err = b.AddScalar(t.Context(), ir.ScalarSpec{
			Scalar:      ir.Scalar{Kind: ir.Subquery, Scope: scope, Layout: boolean, Nullable: true, SubqueryKind: ir.AnySubquery, Relation: subRel},
			Operands:    []ir.ScalarID{lhs},
			Comparisons: []ir.SignatureID{eqSig},
		})
	default:
		t.Fatalf("unsupported subquery kind %d", subKind)
	}
	if err != nil {
		t.Fatal(err)
	}

	barrierFilter, err := b.AddRel(t.Context(), ir.RelSpec{
		Rel: ir.Rel{
			Kind:            ir.Filter,
			Scope:           scope,
			SecurityBarrier: barrier,
			Predicate:       barrierPred,
		},
		Inputs:  []ir.RelID{scan},
		Outputs: []model.SlotID{scanSlot},
	})
	if err != nil {
		t.Fatal(err)
	}

	outerSlotRef, err := b.AddScalar(t.Context(), ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Slot, Scope: scope, Layout: bigint, Slot: scanSlot},
	})
	if err != nil {
		t.Fatal(err)
	}
	nine, err := b.AddScalar(t.Context(), ir.ScalarSpec{
		Scalar: ir.Scalar{Kind: ir.Const, Scope: scope, Layout: bigint},
		Value:  model.Int64Val(9),
	})
	if err != nil {
		t.Fatal(err)
	}
	outerPred, err := b.AddScalar(t.Context(), ir.ScalarSpec{
		Scalar:   ir.Scalar{Kind: ir.Call, Scope: scope, Layout: boolean, Signature: eqSig, Operator: 410, Strict: true},
		Operands: []ir.ScalarID{outerSlotRef, nine},
	})
	if err != nil {
		t.Fatal(err)
	}
	outerFilter, err := b.AddRel(t.Context(), ir.RelSpec{
		Rel: ir.Rel{
			Kind:      ir.Filter,
			Scope:     scope,
			Predicate: outerPred,
		},
		Inputs:  []ir.RelID{barrierFilter},
		Outputs: []model.SlotID{scanSlot},
	})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := b.Build(t.Context(), outerFilter)
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func TestPredicateJoinSecurityBarrierBlocksOuterFilterPushdown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		subKind ir.SubqueryKind
	}{
		{name: "EXISTS", subKind: ir.ExistsSubquery},
		{name: "IN", subKind: ir.AnySubquery},
	} {
		for _, barrier := range []bool{true, false} {
			label := tc.name + "/barrier=false"
			if barrier {
				label = tc.name + "/barrier=true"
			}
			t.Run(label, func(t *testing.T) {
				graph := normalized(t, buildScanBarrierSubqueryWithOuterFilter(t, tc.subKind, barrier))

				// Verify normalized graph structure:
				// outer Filter (SecurityBarrier=false) -> Project (SecurityBarrier=barrier) -> Filter (SecurityBarrier=barrier) -> MarkJoin.
				outerID := graph.Root()
				outer, _ := graph.Rel(outerID)
				if outer.Kind != ir.Filter || outer.SecurityBarrier {
					t.Fatalf("outer root = %+v, want non-barrier Filter", outer)
				}
				projID, _ := graph.Input(outerID, 0)
				proj, _ := graph.Rel(projID)
				if proj.Kind != ir.Project || proj.SecurityBarrier != barrier {
					t.Fatalf("lowered Project = %+v, want SecurityBarrier=%v", proj, barrier)
				}
				innerFilterID, _ := graph.Input(projID, 0)
				innerFilter, _ := graph.Rel(innerFilterID)
				if innerFilter.Kind != ir.Filter || innerFilter.SecurityBarrier != barrier {
					t.Fatalf("lowered Filter = %+v, want SecurityBarrier=%v", innerFilter, barrier)
				}
				joinID, _ := graph.Input(innerFilterID, 0)
				joinRel, _ := graph.Rel(joinID)
				if joinRel.Kind != ir.Join || joinRel.JoinKind != ir.MarkJoin {
					t.Fatalf("lowered Join = %+v, want MarkJoin", joinRel)
				}

				// End-to-end optimizer check: outer predicate col = 9 must NOT be pushed
				// past the security barrier into Scan(10), whereas without a barrier it IS.
				optOpts := opt.Options{
					IR:               options().Limits,
					Plan:             physical.Limits{MaxNodes: 512, MaxOperands: 8192, MaxProperties: 8192, MaxDepth: 128, MaxBytes: 2 << 20},
					MaxBytes:         256 << 20,
					MaxWork:          100000000,
					MaxOutputColumns: 256,
					MaxAccessPaths:   8,
					WorkingMemory:    1 << 20,
					SearchTime:       time.Hour,
					Access:           barrierOptAccessStub{},
				}
				result, err := opt.Optimize(t.Context(), graph, optOpts)
				if err != nil {
					t.Fatal(err)
				}
				foundScan := false
				pushedDown := false
				for id := physical.NodeID(1); id <= physical.NodeID(result.Plan.NodeCount()); id++ {
					node, err := result.Plan.CloneNode(t.Context(), id, optOpts.Plan.MaxBytes)
					if err != nil {
						t.Fatal(err)
					}
					if node.Algorithm != physical.PrimaryKeyScan {
						continue
					}
					foundScan = true
					for _, sp := range node.Scan.Request.Spans {
						if !sp.Lower.Unbounded || !sp.Upper.Unbounded {
							pushedDown = true
						}
					}
				}
				if !foundScan {
					t.Fatal("missing PrimaryKeyScan in optimized plan")
				}
				if barrier && pushedDown {
					t.Fatal("outer predicate was pushed down across SecurityBarrier=true predicateJoin")
				}
				if !barrier && !pushedDown {
					t.Fatal("expected outer predicate pushdown when SecurityBarrier=false")
				}
			})
		}
	}
}
