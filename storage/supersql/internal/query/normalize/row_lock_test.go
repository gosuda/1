package normalize_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestRowLockOccurrenceAndKeysSurviveNormalization(t *testing.T) {
	f := newFixture(t)
	rowLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{
		Type:     model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite},
		Children: []model.TypeLayout{{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	scanSlot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.integer, Object: 31, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	scan := f.rel(ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.Scan, Scope: f.scope, Occurrence: 7, Object: 31, DescriptorVersion: 3, Generation: 2, EncodingVersion: 1},
		Outputs: []model.SlotID{scanSlot},
	})
	value := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: f.integer, Slot: scanSlot}})
	identity := [4]model.SlotID{f.slot(f.integer, false), f.slot(f.integer, false), f.slot(f.integer, false), f.slot(f.integer, false)}
	// The scan column is forwarded by slot exactly as the real binder does.
	bindings := []ir.Binding{{Slot: scanSlot, Expr: value}}
	for i, slot := range identity {
		constant := f.constant(f.integer, model.Int64Val(int64(i+1)))
		bindings = append(bindings, ir.Binding{Slot: slot, Expr: constant})
	}
	rowSlot := f.slot(rowLayout, false)
	row := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Row, Scope: f.scope, Layout: rowLayout}, Operands: []ir.ScalarID{value}})
	bindings = append(bindings, ir.Binding{Slot: rowSlot, Expr: row})
	outputs := []model.SlotID{scanSlot, identity[0], identity[1], identity[2], identity[3], rowSlot}
	project := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope}, Inputs: []ir.RelID{scan}, Outputs: outputs, Bindings: bindings})
	root := f.rel(ir.RelSpec{
		Rel:     ir.Rel{Kind: ir.LockRows, Scope: f.scope, Effects: ir.Effects{Volatility: ir.Volatile, MayError: true}},
		Inputs:  []ir.RelID{project},
		Outputs: outputs,
		Locks: []ir.RowLockSpec{{
			RowLock: ir.RowLock{
				Object: 31, Occurrence: 7, ObjectSlot: identity[0], DescriptorVersionSlot: identity[1],
				GenerationSlot: identity[2], EncodingVersionSlot: identity[3], RowSlot: rowSlot,
				Strength: ir.NoKeyUpdate, Wait: ir.SkipLocked,
			},
			Keys: []ir.RowLockKey{{Descending: true, NullsFirst: true}},
		}},
	})
	graph := normalized(t, f.freeze(root))
	_, normalizedScan := findRelation(t, graph, ir.Scan)
	if normalizedScan.Occurrence != 7 {
		t.Fatalf("normalized scan occurrence = %d, want 7", normalizedScan.Occurrence)
	}
	lockID, lockRows := findRelation(t, graph, ir.LockRows)
	if lockRows.LockCount() != 1 {
		t.Fatalf("normalized lock count = %d, want 1", lockRows.LockCount())
	}
	lock, ok := graph.RowLock(lockID, 0)
	if !ok || lock.Object != 31 || lock.Occurrence != 7 || lock.ObjectSlot != identity[0] || lock.DescriptorVersionSlot != identity[1] || lock.GenerationSlot != identity[2] || lock.EncodingVersionSlot != identity[3] || lock.RowSlot != rowSlot || lock.Strength != ir.NoKeyUpdate || lock.Wait != ir.SkipLocked || lock.KeyCount() != 1 {
		t.Fatalf("normalized row lock = %#v", lock)
	}
	key, ok := graph.RowLockKey(lockID, 0, 0)
	if !ok || !key.Descending || !key.NullsFirst {
		t.Fatalf("normalized row-lock key = %#v", key)
	}
}
