package expr_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func captureSlot(t *testing.T, f *fixture, layout ir.LayoutID, nullable bool) (model.SlotID, ir.ScalarID) {
	t.Helper()
	slot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: layout, Nullable: nullable})
	if err != nil {
		t.Fatal(err)
	}
	root := f.add(ir.Scalar{Kind: ir.Slot, Layout: layout, Slot: slot, Nullable: nullable}, nil, model.Value{})
	return slot, root
}

func captureState(t *testing.T, program *expr.Program, options expr.StateOptions) *expr.State {
	t.Helper()
	s, err := expr.NewState(program, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func TestCaptureProgramSharesOnlyImmutableMetadata(t *testing.T) {
	f := newFixture(t)
	slot, captured := captureSlot(t, f, f.layouts[20], false)
	root := f.call("int8pl", 20, f.x, captured)
	options := f.options()
	options.Captures = []expr.Input{{Slot: slot, Column: 0}}
	program := compile(t, f.graph(), []ir.ScalarID{root}, options)
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, number := range []int64{7, 31} {
			t.Run(fmt.Sprintf("mode%d/capture%d", mode, number), func(t *testing.T) {
				t.Parallel()
				captures := columnar.Batch{Length: 2, Selection: []uint32{1}, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 2, Int64: []int64{-100, number}}}}
				s := captureState(t, program, expr.StateOptions{Captures: &captures})
				input, out := batch(17), columnar.Batch{}
				if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
					t.Fatal(err)
				}
				for row := range input.Rows() {
					want := number
					if row%3 == 1 {
						want += 2
					}
					requireInt(t, value(t, &out, 0, row), want, row%3 == 2)
				}
			})
		}
	}
}

func TestCaptureValueValidationRunsOnlyOnActiveMasks(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			q := allocationQuery(t)
			f := newFixture(t)
			integer := model.Type{ID: 21, Storage: model.TypeInt64}
			layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: integer})
			if err != nil {
				t.Fatal(err)
			}
			f.layouts[21] = layout
			slot, captured := captureSlot(t, f, layout, false)
			zero := f.constant(20, model.Int64Val(0))
			condition := f.call("int8eq", 16, f.x, zero)
			seven := f.constant(21, model.Int64Val(7))
			root := f.add(ir.Scalar{Kind: ir.Case, Layout: layout}, []ir.ScalarID{condition, seven, captured}, model.Value{})
			options := f.options()
			options.Captures = []expr.Input{{Slot: slot, Column: 0}}
			program := compile(t, f.graph(), []ir.ScalarID{root}, options)
			captures := columnar.Batch{Length: 1, Columns: []columnar.Vector{{Type: integer, Length: 1, Int64: []int64{32768}}}}
			s := captureState(t, program, expr.StateOptions{Captures: &captures, WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
			input, out := batch(1), columnar.Batch{}
			if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatalf("masked capture validated its value: %v", err)
			}
			requireInt(t, value(t, &out, 0, 0), 7, false)
			input.Columns[0].Int64[0] = 2
			err = s.Eval(t.Context(), &input, nil, &out, mode)
			var sqlError *model.Error
			if !errors.As(err, &sqlError) || sqlError.Code != "22003" {
				t.Fatalf("active out-of-range capture = %v", err)
			}
			if out.Rows() != 0 {
				t.Fatalf("failed capture published %d rows", out.Rows())
			}
			if usage := q.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
				t.Fatalf("capture failure leaked allocations: %+v", usage)
			}
		})
	}
}

func TestCaptureNestedPayloadCopiesOnceAndRetainsIndependently(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			q := allocationQuery(t)
			f := newFixture(t)
			layout := model.TypeLayout{Type: model.Type{ID: 1009, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 25, Storage: model.TypeString}}}}
			id, err := f.b.AddLayout(t.Context(), layout)
			if err != nil {
				t.Fatal(err)
			}
			slot, captured := captureSlot(t, f, id, true)
			options := f.options()
			options.Captures = []expr.Input{{Slot: slot, Column: 0}}
			options.Limits.MaxStateBytes = 64 << 10
			program := compile(t, f.graph(), []ir.ScalarID{captured}, options)
			builder, err := columnar.NewBuilder([]model.TypeLayout{layout}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: 64 << 10})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := builder.Close(); err != nil {
					t.Error(err)
				}
			})
			text := strings.Repeat("payload", 2048)
			nested := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -3}}, ArrayVal: []model.Value{model.StringVal(text), model.NullVal(model.TypeString)}}
			if err := builder.Append(t.Context(), []model.Value{nested}); err != nil {
				t.Fatal(err)
			}
			var captures columnar.Batch
			if err := builder.Publish(&captures); err != nil {
				t.Fatal(err)
			}
			s := captureState(t, program, expr.StateOptions{Captures: &captures, WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
			input, out := columnar.Batch{Length: 17}, columnar.Batch{}
			if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatalf("one payload should fit independently of lane count: %v", err)
			}
			retained, err := expr.Retain(t.Context(), &out, program.OutputLayouts(), expr.RetainOptions{MaxRows: 17, MaxBytes: 2 << 20})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := retained.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			clear(captures.Columns[0].Children[0].Data)
			if err := builder.Close(); err != nil {
				t.Fatal(err)
			}
			if err := retained.Publish(&out); err != nil {
				t.Fatal(err)
			}
			for row := range out.Rows() {
				got := value(t, &out, 0, row)
				validShape := len(got.Dimensions) == 1 && got.Dimensions[0].LowerBound == -3 && got.Dimensions[0].Length == 2
				validValues := len(got.ArrayVal) == 2 && got.ArrayVal[0].StrVal == text && got.ArrayVal[1].IsNull
				if !validShape || !validValues {
					t.Fatalf("retained capture changed at row %d", row)
				}
			}
			if usage := q.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
				t.Fatalf("closed capture state leaked allocations: %+v", usage)
			}
		})
	}
}

func TestCaptureMappingsRejectAmbiguousSlots(t *testing.T) {
	f := newFixture(t)
	first, captured := captureSlot(t, f, f.layouts[20], true)
	second, _ := captureSlot(t, f, f.layouts[20], true)
	graph := f.graph()
	for _, test := range []struct {
		name     string
		captures []expr.Input
	}{
		{"input overlap", []expr.Input{{Slot: f.slot, Column: 0}}},
		{"duplicate slot", []expr.Input{{Slot: first, Column: 0}, {Slot: first, Column: 1}}},
		{"duplicate column", []expr.Input{{Slot: first, Column: 0}, {Slot: second, Column: 0}}},
		{"negative column", []expr.Input{{Slot: first, Column: -1}}},
		{"unknown slot", []expr.Input{{Slot: 0, Column: 0}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := f.options()
			options.Captures = test.captures
			if _, err := expr.Compile(t.Context(), graph, []ir.ScalarID{captured}, options); !errors.Is(err, expr.ErrInvalid) {
				t.Fatalf("ambiguous capture mapping = %v", err)
			}
		})
	}
}

func TestCaptureBatchRejectsInvalidShapeAndLayout(t *testing.T) {
	f := newFixture(t)
	slot, captured := captureSlot(t, f, f.layouts[20], true)
	options := f.options()
	options.Captures = []expr.Input{{Slot: slot, Column: 0}}
	program := compile(t, f.graph(), []ir.ScalarID{captured}, options)
	integer := model.Type{ID: 20, Storage: model.TypeInt64}
	for _, test := range []struct {
		name  string
		batch *columnar.Batch
	}{
		{"missing", nil},
		{"empty", &columnar.Batch{}},
		{"two rows", &columnar.Batch{Length: 2}},
		{"empty selection", &columnar.Batch{Length: 1, Selection: []uint32{}}},
		{"invalid selection", &columnar.Batch{Length: 1, Selection: []uint32{1}}},
		{"missing column", &columnar.Batch{Length: 1}},
		{"wrong SQL type", &columnar.Batch{Length: 1, Columns: []columnar.Vector{{Type: model.Type{ID: 23, Storage: model.TypeInt64}, Length: 1, Int64: []int64{1}}}}},
		{"short payload", &columnar.Batch{Length: 1, Columns: []columnar.Vector{{Type: integer, Length: 1}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, err := expr.NewState(program, expr.StateOptions{Captures: test.batch})
			if err == nil {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("invalid capture batch accepted")
			}
		})
	}
}

func TestCaptureBroadcastUsesOnlySelectedActiveLanes(t *testing.T) {
	f := newFixture(t)
	slot, captured := captureSlot(t, f, f.layouts[20], false)
	zero := f.constant(20, model.Int64Val(0))
	condition := f.call("int8eq", 16, f.x, zero)
	divide := f.call("int8div", 20, captured, f.x)
	root := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{condition, zero, divide}, model.Value{})
	options := f.options()
	options.Captures = []expr.Input{{Slot: slot, Column: 0}}
	program := compile(t, f.graph(), []ir.ScalarID{root}, options)
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		captures := columnar.Batch{Length: 1, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Encoding: columnar.Dictionary, Length: 1, Indices: []uint32{1}, Int64: []int64{99, 10}}}}
		s := captureState(t, program, expr.StateOptions{Captures: &captures})
		input, out := batch(5), columnar.Batch{}
		input.Selection = []uint32{4, 0, 2, 1}
		if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		for row, want := range []int64{5, 0, 0, 5} {
			requireInt(t, value(t, &out, 0, row), want, row == 2)
		}
	}
}

func TestMaskedCaptureDoesNotAdmitPayload(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			q := allocationQuery(t)
			f := newFixture(t)
			slot, captured := captureSlot(t, f, f.layouts[25], false)
			zero := f.constant(20, model.Int64Val(0))
			condition := f.call("int8eq", 16, f.x, zero)
			safe := f.constant(25, model.StringVal("safe"))
			root := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[25]}, []ir.ScalarID{condition, safe, captured}, model.Value{})
			options := f.options()
			options.Captures = []expr.Input{{Slot: slot, Column: 0}}
			options.Limits.MaxStateBytes = 16 << 10
			program := compile(t, f.graph(), []ir.ScalarID{root}, options)
			captures := columnar.Batch{Length: 1, Columns: []columnar.Vector{{Type: model.Type{ID: 25, Storage: model.TypeString}, Length: 1, Ranges: []columnar.Range{{Length: 32 << 10}}, Data: make([]byte, 32<<10)}}}
			s := captureState(t, program, expr.StateOptions{Captures: &captures, WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
			input, out := batch(1), columnar.Batch{}
			if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatalf("masked payload consumed state budget: %v", err)
			}
			if got := value(t, &out, 0, 0).StrVal; got != "safe" {
				t.Fatalf("masked result = %q", got)
			}
			input.Columns[0].Int64[0] = 2
			if err := s.Eval(t.Context(), &input, nil, &out, mode); !errors.Is(err, columnar.ErrBatchBudget) {
				t.Fatalf("active oversized capture = %v", err)
			}
			if usage := q.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
				t.Fatalf("capture admission failure leaked allocations: %+v", usage)
			}
		})
	}
}
