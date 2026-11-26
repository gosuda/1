package expr_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestStateEvalReusesWorkspaceAndBuilderAcrossBatches(t *testing.T) {
	q := allocationQuery(t)
	f := newFixture(t)

	// Add a second string input column to exercise both fixed-width and variable-width registers.
	strSlot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layouts[25], Nullable: true, Object: 1, Column: 2})
	if err != nil {
		t.Fatal(err)
	}
	strScalar := f.add(ir.Scalar{Kind: ir.Slot, Layout: f.layouts[25], Slot: strSlot, Nullable: true}, nil, model.Value{})

	// Output 0: COALESCE(x + 10, -1)  (exercises masks, once-like branching, nulls, fixed-width int64)
	ten := f.constant(20, model.Int64Val(10))
	minusOne := f.constant(20, model.Int64Val(-1))
	plusTen := f.call("int8pl", 20, f.x, ten)
	coalesced := f.add(ir.Scalar{Kind: ir.Coalesce, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{plusTen, minusOne}, model.Value{})

	// Output 1: CASE WHEN x > 0 THEN s ELSE 'fallback' END (exercises variable-width strings + masks)
	zero := f.constant(20, model.Int64Val(0))
	gtZero := f.call("int8gt", 16, f.x, zero)
	fallback := f.constant(25, model.StringVal("fallback"))
	caseExpr := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[25], Nullable: true}, []ir.ScalarID{gtZero, strScalar, fallback}, model.Value{})

	// Output 2: Once(x + 10) referenced twice (exercises opOnceEnter/opOnceStore/opOnceRead across batches)
	onceExpr := f.add(ir.Scalar{Kind: ir.Once, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{plusTen}, model.Value{})

	opts := f.options()
	opts.Inputs = []expr.Input{
		{Slot: f.slot, Column: 0},
		{Slot: strSlot, Column: 1},
	}
	prog := compile(t, f.graph(), []ir.ScalarID{coalesced, caseExpr, onceExpr}, opts)

	s, err := expr.NewState(prog, expr.StateOptions{
		WorkAllocator:   q.Allocator(runtime.WorkMemory),
		OutputAllocator: q.Allocator(runtime.OutputMemory),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	type rowSpec struct {
		xNull bool
		x     int64
		sNull bool
		s     string
	}
	batches := [][]rowSpec{
		// Batch 1: 4 rows with mixed nulls, positives, zero, and 2
		{
			{x: 1, s: "alpha"},
			{xNull: true, s: "ignored"},
			{x: 2, s: "beta-long-string-payload"},
			{x: 0, s: "zero"},
		},
		// Batch 2: smaller batch (1 row) after larger batch — verify no stale bits/values bleed through
		{
			{x: 5, s: "tiny"},
		},
		// Batch 3: empty batch (0 rows)
		{},
		// Batch 4: larger batch (6 rows) forcing register slice growth + builder growth
		{
			{x: 2, s: "hit-once"},
			{x: -3, s: "neg"},
			{x: 7, sNull: true},
			{xNull: true, sNull: true},
			{x: 100, s: "hundred"},
			{x: 2, s: "second-two"},
		},
		// Batch 5: back down to 2 rows
		{
			{x: 42, s: "answer"},
			{x: -1, s: "neg-one"},
		},
	}

	intLayout := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	strLayout := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString}}
	inBuilder, err := columnar.NewBuilder([]model.TypeLayout{intLayout, strLayout}, columnar.BuilderOptions{MaxRows: 16, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer inBuilder.Close()

	var inBatch, outBatch columnar.Batch
	var firstOutputLive int64
	for bi, spec := range batches {
		inBuilder.Reset()
		for _, r := range spec {
			xv := model.Int64Val(r.x)
			if r.xNull {
				xv = model.NullVal(model.TypeInt64)
			}
			sv := model.StringVal(r.s)
			if r.sNull {
				sv = model.NullVal(model.TypeString)
			}
			if err := inBuilder.Append(t.Context(), []model.Value{xv, sv}); err != nil {
				t.Fatal(err)
			}
		}
		if err := inBuilder.Publish(&inBatch); err != nil {
			t.Fatal(err)
		}
		if err := s.Eval(t.Context(), &inBatch, nil, &outBatch, expr.Direct); err != nil {
			t.Fatalf("batch %d eval failed: %v", bi, err)
		}
		if outBatch.Rows() != len(spec) {
			t.Fatalf("batch %d rows = %d, want %d", bi, outBatch.Rows(), len(spec))
		}
		for i, r := range spec {
			// Check Output 0: COALESCE(x + 10, -1)
			want0 := int64(-1)
			if !r.xNull {
				want0 = r.x + 10
			}
			requireInt(t, value(t, &outBatch, 0, i), want0, false)

			// Check Output 1: CASE WHEN x > 0 THEN s ELSE 'fallback' END
			got1 := value(t, &outBatch, 1, i)
			if !r.xNull && r.x > 0 {
				if got1.IsNull != r.sNull || (!r.sNull && got1.StrVal != r.s) {
					t.Fatalf("batch %d row %d col 1 = %+v, want (null=%v, %q)", bi, i, got1, r.sNull, r.s)
				}
			} else {
				if got1.IsNull || got1.StrVal != "fallback" {
					t.Fatalf("batch %d row %d col 1 = %+v, want %q", bi, i, got1, "fallback")
				}
			}

			// Check Output 2: Once(x + 10)
			requireInt(t, value(t, &outBatch, 2, i), r.x+10, r.xNull)
		}

		// Verify exact memory accounting after every batch:
		// Work memory must be completely released, and Output memory must reuse a single builder reservation.
		usage := q.Usage()
		if usage.Work.Live != 0 || usage.Work.Reservations != 0 {
			t.Fatalf("batch %d retained work memory: %+v", bi, usage.Work)
		}
		if usage.Output.Reservations != 1 {
			t.Fatalf("batch %d output reservations = %d, want 1 (reused builder)", bi, usage.Output.Reservations)
		}
		if bi == 0 {
			firstOutputLive = usage.Output.Live
			if firstOutputLive == 0 {
				t.Fatalf("expected non-zero live output bytes after batch 0")
			}
		}
	}

	// Verify canceled Eval cleanly releases both work and reused output builder.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Eval(ctx, &inBatch, nil, &outBatch, expr.Direct); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled eval = %v, want context.Canceled", err)
	}
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("canceled eval leaked claims: %+v", usage)
	}

	// Verify State remains usable after error and cleanly releases everything on Close().
	if err := s.Eval(t.Context(), &inBatch, nil, &outBatch, expr.Direct); err != nil {
		t.Fatalf("eval after cancel failed: %v", err)
	}
	if outBatch.Rows() != len(batches[len(batches)-1]) {
		t.Fatalf("rows after recovery = %d, want %d", outBatch.Rows(), len(batches[len(batches)-1]))
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("close leaked claims: %+v", usage)
	}
	_ = fmt.Sprint(firstOutputLive)
}
