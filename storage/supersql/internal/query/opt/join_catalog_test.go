package opt

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

// catalogLikeSource builds a scan with explicit effects and layout, modeling a
// catalog scan (stable) or a plain scan (effect-free) over one slot.
func catalogLikeSource(f fixture, object model.ObjectID, layout ir.LayoutID, effects ir.Effects) (ir.RelID, model.SlotID) {
	f.t.Helper()
	slot, err := f.b.AddSlot(f.t.Context(), ir.SlotInfo{Scope: f.scope, Layout: layout, Object: object, Column: 1})
	if err != nil {
		f.t.Fatal(err)
	}
	id := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: object, Generation: 1, DescriptorVersion: 1, Effects: effects}, Outputs: []model.SlotID{slot}})
	return id, slot
}

func slotScalar(f fixture, layout ir.LayoutID, slot model.SlotID) ir.ScalarID {
	f.t.Helper()
	return f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: layout, Slot: slot}})
}

func callScalar(f fixture, layout ir.LayoutID, signature ir.SignatureID, operator model.OperatorID, operands []ir.ScalarID) ir.ScalarID {
	f.t.Helper()
	return f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: layout, Signature: signature, Operator: operator, Strict: true}, Operands: operands})
}

// catalogAccessStub mirrors accessStub but serves paths in per-scan layouts,
// so scans over non-integer type identities (oid, regproc) validate.
type catalogAccessStub struct {
	layouts map[model.ObjectID]model.TypeLayout
}

func (s catalogAccessStub) Access(ctx context.Context, request AccessRequest) (Access, error) {
	if err := ctx.Err(); err != nil {
		return Access{}, err
	}
	typ := s.layouts[request.Object].Type
	result := Access{Target: storage.ObjectRef{ObjectID: request.Object, Generation: request.Generation, DescriptorVersion: request.DescriptorVersion, EncodingVersion: 1}, Layout: []storage.ColumnLayout{{ID: 1, Type: typ}}}
	result.Paths = append(result.Paths, AccessPath{Target: result.Target, Layout: result.Layout, ColumnMap: []physical.ScanColumn{{Source: 1, Target: 1}}, Algorithm: physical.TableScan})
	return result, nil
}

func (s catalogAccessStub) Negotiate(ctx context.Context, request storage.PushdownRequest, maxBytes uint64) (physical.ScanSpec, error) {
	if err := ctx.Err(); err != nil {
		return physical.ScanSpec{}, err
	}
	return physical.ScanSpec{Request: request, CapabilityEpoch: 1, Projection: append([]model.ColumnID(nil), request.ColumnIDs...)}, nil
}

func hashJoinSelected(t *testing.T, plan *physical.Plan, maxBytes uint64) {
	t.Helper()
	algs := planAlgorithms(t, plan, maxBytes)
	hasHash := false
	for _, alg := range algs {
		if alg == physical.HashJoin {
			hasHash = true
		}
		if alg == physical.NestedLoopJoin {
			t.Fatalf("expected hash join, plan fell back to nested loop: %v", algs)
		}
	}
	if !hasHash {
		t.Fatalf("expected hash join in plan, got %v", algs)
	}
}

// A join over stable inputs with a clean same-type equality key must hash.
// Catalog scans are stable, so without this every catalog join nested-loops.
func TestStableJoinInputsPickHashJoin(t *testing.T) {
	f := newFixture(t)
	oidLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 26, Storage: model.TypeInt64}})
	if err != nil {
		t.Fatal(err)
	}
	left, l := catalogLikeSource(f, 10, oidLayout, ir.Effects{Volatility: ir.Stable})
	right, r := catalogLikeSource(f, 20, oidLayout, ir.Effects{Volatility: ir.Stable})
	signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: 607, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{oidLayout, oidLayout}})
	if err != nil {
		t.Fatal(err)
	}
	predicate := callScalar(f, f.boolean, signature, 607, []ir.ScalarID{slotScalar(f, oidLayout, l), slotScalar(f, oidLayout, r)})
	join := f.rel(ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: predicate, Effects: ir.Effects{Volatility: ir.Stable}},
		Inputs:  []ir.RelID{left, right},
		Outputs: []model.SlotID{l, r},
	})
	options := testOptions()
	options.Access = catalogAccessStub{layouts: map[model.ObjectID]model.TypeLayout{10: {Type: model.Type{ID: 26, Storage: model.TypeInt64}}, 20: {Type: model.Type{ID: 26, Storage: model.TypeInt64}}}}
	result, err := Optimize(t.Context(), f.build(join), options)
	if err != nil {
		t.Fatal(err)
	}
	hashJoinSelected(t, result.Plan, options.Plan.MaxBytes)
	lower(t, result.Plan)
}

// A cross-type Relabel must not form a hash key even when the coercion is an
// implicit binary cast: the executor encodes both sides with the left slot
// layout, so text compared as bpchar keeps trailing spaces in the key while
// bpchar equality ignores them, and an int slot compared as oid encodes a
// different range than the resolved oid equality compares.
func TestCrossTypeRelabelJoinKeysStayNestedLoop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		left, op  model.TypeID
		operator  model.OperatorID
		storage   model.DataType
		collation model.CollationID
	}{
		{"regprocToOID", 24, 26, 607, model.TypeInt64, 0},
		{"textToBPChar", 25, 1042, 1054, model.TypeString, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			slotLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: tc.left, Storage: tc.storage, Collation: tc.collation}})
			if err != nil {
				t.Fatal(err)
			}
			opLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: tc.op, Storage: tc.storage, Collation: tc.collation}})
			if err != nil {
				t.Fatal(err)
			}
			left, l := catalogLikeSource(f, 10, slotLayout, ir.Effects{})
			right, r := catalogLikeSource(f, 20, opLayout, ir.Effects{})
			relabel := f.scalar(ir.ScalarSpec{
				Scalar: ir.Scalar{Kind: ir.Relabel, Scope: f.scope, Layout: opLayout, CastContext: ir.ImplicitCast}, Operands: []ir.ScalarID{slotScalar(f, slotLayout, l)},
			})
			signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: tc.operator, Result: f.boolean, Strict: true}, Parameters: []ir.LayoutID{opLayout, opLayout}})
			if err != nil {
				t.Fatal(err)
			}
			predicate := callScalar(f, f.boolean, signature, tc.operator, []ir.ScalarID{relabel, slotScalar(f, opLayout, r)})
			join := f.rel(ir.RelSpec{
				Rel:     ir.Rel{Kind: ir.Join, Scope: f.scope, JoinKind: ir.InnerJoin, Predicate: predicate},
				Inputs:  []ir.RelID{left, right},
				Outputs: []model.SlotID{l, r},
			})
			options := testOptions()
			options.Access = catalogAccessStub{layouts: map[model.ObjectID]model.TypeLayout{10: {Type: model.Type{ID: tc.left, Storage: tc.storage, Collation: tc.collation}}, 20: {Type: model.Type{ID: tc.op, Storage: tc.storage, Collation: tc.collation}}}}
			result, err := Optimize(t.Context(), f.build(join), options)
			if err != nil {
				t.Fatal(err)
			}
			for _, alg := range planAlgorithms(t, result.Plan, options.Plan.MaxBytes) {
				if alg == physical.HashJoin || alg == physical.MergeJoin || alg == physical.LookupJoin {
					t.Fatalf("cross-type relabel must not use a keyed join, got %v", planAlgorithms(t, result.Plan, options.Plan.MaxBytes))
				}
			}
			lower(t, result.Plan)
		})
	}
}
