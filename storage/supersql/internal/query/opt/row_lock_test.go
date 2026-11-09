package opt

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type rowLockAccessPlanner struct {
	requests []storage.PushdownRequest
}

func (planner *rowLockAccessPlanner) Access(ctx context.Context, request AccessRequest) (Access, error) {
	if err := ctx.Err(); err != nil {
		return Access{}, err
	}
	typ := model.Type{ID: 20, Storage: model.TypeInt64}
	bytesType := model.Type{ID: 17, Storage: model.TypeBytes}
	base := storage.ObjectRef{ObjectID: request.Object, Generation: request.Generation, DescriptorVersion: request.DescriptorVersion, EncodingVersion: 1}
	index := storage.ObjectRef{ObjectID: request.Object + 700, Generation: request.Generation, DescriptorVersion: request.DescriptorVersion, EncodingVersion: 1}
	lookupIndex := storage.ObjectRef{ObjectID: request.Object + 701, Generation: request.Generation, DescriptorVersion: request.DescriptorVersion, EncodingVersion: 1}
	layout := []storage.ColumnLayout{{ID: 1, Type: typ}}
	mapping := []physical.ScanColumn{{Source: 1, Target: 1}}
	keys := []storage.KeyColumn{{ColumnID: 1, Type: typ}}
	lookupLayout := []storage.ColumnLayout{{ID: 2, Type: bytesType}}
	lookupKeys := []storage.KeyColumn{{ColumnID: 2, Type: bytesType}}
	return Access{
		Target: base,
		Layout: layout,
		Paths: []AccessPath{
			{Target: base, Layout: layout, ColumnMap: mapping, Algorithm: physical.PrimaryKeyScan, Keys: keys, Operators: []model.OperatorID{412}},
			{Target: index, Layout: layout, ColumnMap: mapping, Algorithm: physical.SecondaryScan, Index: model.IndexID(index.ObjectID), Keys: keys, Operators: []model.OperatorID{412}},
			{
				Target: lookupIndex, Layout: lookupLayout, Algorithm: physical.SecondaryScan, Index: model.IndexID(lookupIndex.ObjectID),
				Keys: lookupKeys, Operators: []model.OperatorID{1957},
				Lookup: LookupAccess{Enabled: true, OwnerColumn: 2, Target: base, Layout: layout, ColumnMap: mapping, Keys: keys},
			},
		},
	}, nil
}

func (planner *rowLockAccessPlanner) Negotiate(ctx context.Context, request storage.PushdownRequest, _ uint64) (physical.ScanSpec, error) {
	if err := ctx.Err(); err != nil {
		return physical.ScanSpec{}, err
	}
	planner.requests = append(planner.requests, request)
	result := physical.ScanSpec{Request: request, CapabilityEpoch: 1, Projection: append([]model.ColumnID(nil), request.ColumnIDs...)}
	if len(request.KeyLayout) != 0 {
		key := request.KeyLayout[0]
		result.Ordering = []storage.ScanOrdering{{KeyOrdinal: 0, Type: key.Type, Descending: key.Descending, NullsFirst: key.NullsFirst}}
	}
	return result, nil
}

func TestLockedScanOccurrenceUsesOnlyPrimaryPath(t *testing.T) {
	f := newFixture(t)
	scan := func(occurrence model.ScanOccurrence) (ir.RelID, model.SlotID) {
		slot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layout, Object: 31, Column: 1})
		if err != nil {
			t.Fatal(err)
		}
		return f.rel(ir.RelSpec{
			Rel:     ir.Rel{Kind: ir.Scan, Scope: f.scope, Occurrence: occurrence, Object: 31, DescriptorVersion: 3, Generation: 2, EncodingVersion: 1},
			Outputs: []model.SlotID{slot},
		}), slot
	}
	left, leftSlot := scan(1)
	right, rightSlot := scan(2)
	join := f.cross(left, right, leftSlot, rightSlot)
	rowLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{
		Type:     model.Type{ID: 2249, Storage: model.TypeComposite},
		Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	addSlot := func(layout ir.LayoutID) model.SlotID {
		slot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: layout})
		if err != nil {
			t.Fatal(err)
		}
		return slot
	}
	identity := [4]model.SlotID{addSlot(f.layout), addSlot(f.layout), addSlot(f.layout), addSlot(f.layout)}
	bindings := make([]ir.Binding, 0, len(identity)+3)
	// Project must bind every output, so the two pass-through scan columns are
	// forwarded by slot exactly as the real binder does.
	for _, slot := range []model.SlotID{leftSlot, rightSlot} {
		ref := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: slot}})
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: ref})
	}
	for i, slot := range identity {
		constant := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: f.scope, Layout: f.layout}, Value: model.Int64Val(int64(i + 1))})
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: constant})
	}
	leftValue := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.layout, Slot: leftSlot}})
	row := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Row, Scope: f.scope, Layout: rowLayout}, Operands: []ir.ScalarID{leftValue}})
	rowSlot := addSlot(rowLayout)
	bindings = append(bindings, ir.Binding{Slot: rowSlot, Expr: row})
	outputs := []model.SlotID{leftSlot, rightSlot, identity[0], identity[1], identity[2], identity[3], rowSlot}
	project := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope}, Inputs: []ir.RelID{join}, Outputs: outputs, Bindings: bindings})
	root := f.rel(ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.LockRows, Scope: f.scope, Effects: ir.Effects{Volatility: ir.Volatile, MayError: true}},
		Inputs:  []ir.RelID{project},
		Outputs: outputs,
		Locks: []ir.RowLockSpec{{
			RowLock: ir.RowLock{
				Object: 31, Occurrence: 1, ObjectSlot: identity[0], DescriptorVersionSlot: identity[1],
				GenerationSlot: identity[2], EncodingVersionSlot: identity[3], RowSlot: rowSlot,
				Strength: ir.UpdateLock, Wait: ir.SkipLocked,
			},
			Keys: []ir.RowLockKey{{}},
		}},
	})
	planner := &rowLockAccessPlanner{}
	opts := testOptions()
	opts.Access = planner
	result, err := Optimize(t.Context(), f.build(root), opts)
	if err != nil {
		t.Fatal(err)
	}
	lockedPrimary := false
	unlockedIndexes := make(map[model.IndexID]bool)
	for _, request := range planner.requests {
		switch request.Occurrence {
		case 1:
			if request.IndexID != 0 {
				t.Fatalf("locked occurrence negotiated secondary or fused index %d", request.IndexID)
			}
			lockedPrimary = true
		case 2:
			if request.IndexID != 0 {
				unlockedIndexes[request.IndexID] = true
			}
		}
	}
	if !lockedPrimary || !unlockedIndexes[731] || !unlockedIndexes[732] {
		t.Fatalf("negotiated occurrence paths: locked primary=%v unlocked indexes=%v requests=%#v", lockedPrimary, unlockedIndexes, planner.requests)
	}
	lockedPlan := false
	for id := physical.NodeID(1); id <= physical.NodeID(result.Plan.NodeCount()); id++ {
		node, err := result.Plan.CloneNode(t.Context(), id, opts.Plan.MaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		if node.Scan.Request.Occurrence != 1 {
			continue
		}
		if node.Algorithm != physical.PrimaryKeyScan || node.Scan.Lookup.Enabled || node.Scan.Request.IndexID != 0 {
			t.Fatalf("locked occurrence physical path = algorithm %d lookup=%v index=%d", node.Algorithm, node.Scan.Lookup.Enabled, node.Scan.Request.IndexID)
		}
		lockedPlan = true
	}
	if !lockedPlan {
		t.Fatal("physical plan lost the locked scan occurrence")
	}
	fragments := lower(t, result.Plan)
	if fragments.Plan() != result.Plan {
		t.Fatal("fragmentation replaced the occurrence-bearing physical plan")
	}
	fragmentLocked := false
	for id := physical.NodeID(1); id <= physical.NodeID(fragments.Plan().NodeCount()); id++ {
		node, err := fragments.Plan().CloneNode(t.Context(), id, opts.Plan.MaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		fragmentLocked = fragmentLocked || node.Scan.Request.Occurrence == 1
	}
	if !fragmentLocked {
		t.Fatal("fragment graph lost the locked scan occurrence")
	}
}
