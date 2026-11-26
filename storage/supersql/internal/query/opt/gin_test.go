package opt

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type ginAccessStub struct {
	opClass string
	colType model.Type
}

func (s ginAccessStub) Access(ctx context.Context, req AccessRequest) (Access, error) {
	int64Type := model.Type{ID: 20, Storage: model.TypeInt64}
	byteaType := model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}
	baseRef := storage.ObjectRef{ObjectID: req.Object, Generation: req.Generation, DescriptorVersion: req.DescriptorVersion, EncodingVersion: 1}
	idxRef := storage.ObjectRef{ObjectID: 99, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	baseLayout := []storage.ColumnLayout{
		{ID: 1, Type: int64Type},
		{ID: 2, Type: s.colType, Nullable: true},
	}
	baseKeys := []storage.KeyColumn{{ColumnID: 1, Type: int64Type}}
	baseMap := []physical.ScanColumn{{Source: 1, Target: 1}, {Source: 2, Target: 2}}
	ginLayout := []storage.ColumnLayout{
		{ID: 1, Type: byteaType},
		{ID: 2, Type: byteaType},
		{ID: 3, Type: byteaType},
	}
	ginKeys := []storage.KeyColumn{{ColumnID: 1, Type: byteaType}, {ColumnID: 2, Type: byteaType}}
	return Access{
		Target: baseRef,
		Layout: baseLayout,
		Paths: []AccessPath{
			{
				Target:    baseRef,
				Layout:    baseLayout,
				ColumnMap: baseMap,
				Algorithm: physical.TableScan,
				Keys:      baseKeys,
				Direction: storage.ScanForward,
			},
			{
				Target:    idxRef,
				Layout:    ginLayout,
				Algorithm: physical.SecondaryScan,
				Index:     model.IndexID(idxRef.ObjectID),
				Keys:      ginKeys,
				Direction: storage.ScanForward,
				Lookup: LookupAccess{
					Enabled:     true,
					OwnerColumn: 3,
					Target:      baseRef,
					Layout:      baseLayout,
					ColumnMap:   baseMap,
					Keys:        baseKeys,
				},
				GIN: GINAccess{
					Enabled:   true,
					IndexName: "idx_docs_gin",
					TableName: "docs",
					OpClass:   s.opClass,
					Column:    2,
					Layout:    model.TypeLayout{Type: s.colType},
				},
			},
		},
	}, nil
}

func (s ginAccessStub) Negotiate(ctx context.Context, req storage.PushdownRequest, maxBytes uint64) (physical.ScanSpec, error) {
	return physical.ScanSpec{
		CapabilityEpoch: 1,
		Request:         req,
		Projection:      req.ColumnIDs,
		Estimate:        storage.ScanEstimate{Rows: 100, DecodeBytes: 16384},
	}, nil
}

func TestGINOptimizerSelection(t *testing.T) {
	jsonbType := model.Type{ID: 3802, Modifier: -1, Storage: model.TypeJSONB}
	textType := model.Type{ID: 25, Modifier: -1, Storage: model.TypeString}

	cases := []struct {
		name     string
		opClass  string
		opID     model.OperatorID
		rhsType  model.Type
		rhsVal   model.Value
		wantGIN  bool
		wantOpID model.OperatorID
	}{
		{"jsonb_ops_contains", "jsonb_ops", 3246, jsonbType, model.Value{Type: model.TypeJSONB, StrVal: `{"a":1}`}, true, 3246},
		{"jsonb_ops_exists", "jsonb_ops", 3247, textType, model.StringVal("a"), true, 3247},
		{"jsonb_path_ops_contains", "jsonb_path_ops", 3246, jsonbType, model.Value{Type: model.TypeJSONB, StrVal: `{"a":1}`}, true, 3246},
		{"jsonb_path_ops_rejects_exists", "jsonb_path_ops", 3247, textType, model.StringVal("a"), false, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			docLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: jsonbType})
			if err != nil {
				t.Fatal(err)
			}
			rhsLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: tc.rhsType})
			if err != nil {
				t.Fatal(err)
			}
			idSlot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Object: 1, Column: 1})
			if err != nil {
				t.Fatal(err)
			}
			docSlot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: docLayout, Object: 1, Column: 2, Nullable: true})
			if err != nil {
				t.Fatal(err)
			}
			scan := f.rel(ir.RelSpec{
				Rel:     ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: 1, Generation: 1, DescriptorVersion: 1},
				Outputs: []model.SlotID{idSlot, docSlot},
			})
			lhs := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: docLayout, Nullable: true, Slot: docSlot}})
			rhs := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: rhsLayout}, Value: tc.rhsVal})
			sig, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{
				Signature:  ir.Signature{Operator: tc.opID, Result: f.boolean, Strict: true},
				Parameters: []ir.LayoutID{docLayout, rhsLayout},
			})
			if err != nil {
				t.Fatal(err)
			}
			pred := f.scalar(ir.ScalarSpec{
				Scalar:   ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Nullable: true, Signature: sig, Operator: tc.opID, Strict: true},
				Operands: []ir.ScalarID{lhs, rhs},
			})
			filter := f.rel(ir.RelSpec{
				Rel:     ir.Rel{Kind: ir.Filter, Scope: f.scope, Predicate: pred},
				Inputs:  []ir.RelID{scan},
				Outputs: []model.SlotID{idSlot, docSlot},
			})
			graph := f.build(filter)

			opts := testOptions()
			opts.Access = ginAccessStub{opClass: tc.opClass, colType: jsonbType}
			res, err := Optimize(t.Context(), graph, opts)
			if err != nil {
				t.Fatalf("Optimize: %v", err)
			}
			plan := res.Plan
			foundGIN := false
			for i := range plan.NodeCount() {
				node, err := plan.CloneNode(t.Context(), physical.NodeID(i+1), 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				if node.Scan.GIN.Enabled {
					foundGIN = true
					if node.Scan.GIN.OperatorID != tc.wantOpID || node.Scan.GIN.IndexName != "idx_docs_gin" || node.Scan.GIN.TableName != "docs" {
						t.Fatalf("unexpected GINScanSpec: %+v", node.Scan.GIN)
					}
				}
			}
			if foundGIN != tc.wantGIN {
				t.Fatalf("foundGIN = %v, want %v", foundGIN, tc.wantGIN)
			}
		})
	}
}
