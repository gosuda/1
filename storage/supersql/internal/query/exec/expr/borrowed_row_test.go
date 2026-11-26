package expr_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func requireReleasedBorrowedState(t *testing.T, q *runtime.Query) {
	t.Helper()
	if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("released state retained allocations: %+v", usage)
	}
}

func TestBorrowedRowAvoidsArrayPublicationCopy(t *testing.T) {
	q := allocationQuery(t)
	f := newFixture(t)
	layout := f.arrayLayout(1009, model.Type{ID: 25, Storage: model.TypeString})
	root := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1009], Parameter: 1}, nil, model.Value{})
	opts := f.options()
	opts.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
	opts.Limits.MaxOutputBytes = 1
	program := compile(t, f.graph(), []ir.ScalarID{root}, opts)
	s, err := expr.NewState(program, expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	payload := strings.Repeat("x", 64<<10)
	parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -7}}, ArrayVal: []model.Value{model.StringVal(payload), model.StringVal(payload)}}}}
	input := columnar.Batch{Length: 1}
	row := make([]model.Value, 1)
	if err := s.EvalBorrowedRow(t.Context(), &input, parameters, row, expr.Direct); err != nil {
		t.Fatal(err)
	}
	if row[0].IsNull || len(row[0].Dimensions) != 1 || row[0].Dimensions[0] != (model.ArrayDimension{Length: 2, LowerBound: -7}) || len(row[0].ArrayVal) != 2 || row[0].ArrayVal[1].StrVal != payload {
		t.Fatalf("borrowed array changed: dimensions=%v elements=%d", row[0].Dimensions, len(row[0].ArrayVal))
	}
	if usage := q.Usage(); usage.Work.Live <= 0 || usage.Work.Live >= 32<<10 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
		t.Fatalf("borrowed array was copied or its work was released before consumption: %+v", usage)
	}
	clear(row)
	var published columnar.Batch
	if err := s.Eval(t.Context(), &input, parameters, &published, expr.Direct); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("normal evaluation ignored publication budget: %v", err)
	}
	requireReleasedBorrowedState(t, q)
	if err := s.EvalBorrowedRow(t.Context(), &input, parameters, row, expr.Direct); err != nil {
		t.Fatalf("failed publication made the state unusable: %v", err)
	}
	clear(row)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	requireReleasedBorrowedState(t, q)
}

func TestBorrowedRowUsesOneLogicalLane(t *testing.T) {
	q := allocationQuery(t)
	f := newFixture(t)
	program := compile(t, f.graph(), []ir.ScalarID{f.x}, f.options())
	s, err := expr.NewState(program, expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory)})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	input := columnar.Batch{Length: 3, Selection: []uint32{2}, Columns: []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 3, Int64: []int64{4, 5, 6}}}}
	row := make([]model.Value, 1)
	if err := s.EvalBorrowedRow(t.Context(), &input, nil, row, expr.Direct); err != nil {
		t.Fatal(err)
	}
	requireInt(t, row[0], 6, false)
	for _, tc := range []struct {
		name    string
		input   columnar.Batch
		outputs int
	}{
		{"no logical rows", columnar.Batch{}, 1},
		{"multiple logical rows", batch(2), 1},
		{"missing output", input, 0},
		{"extra output", input, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := make([]model.Value, tc.outputs)
			for i := range out {
				out[i] = model.StringVal("previous payload")
			}
			if err := s.EvalBorrowedRow(t.Context(), &tc.input, nil, out, expr.Direct); !errors.Is(err, expr.ErrInvalid) {
				t.Fatalf("invalid borrowed row request returned %v", err)
			}
			for _, value := range out {
				if !reflect.DeepEqual(value, model.Value{}) {
					t.Fatalf("failed borrowed row retained caller value: %+v", value)
				}
			}
			requireReleasedBorrowedState(t, q)
		})
	}
}

func TestBorrowedRowEagerErrorClearsOutput(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		q := allocationQuery(t)
		f := newFixture(t)
		roots := make([]ir.ScalarID, 3)
		for i := range roots {
			roots[i] = f.function(model.FunctionID(900000+i), 25, false, ir.Effects{Volatility: ir.Volatile, MayError: true})
		}
		failure := errors.New("last argument failed")
		var calls []model.FunctionID
		opts := f.options()
		opts.Functions = resolverFunc(func(fn expr.Function) (expr.ScalarCall, error) {
			return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
				calls = append(calls, fn.ID)
				switch fn.ID {
				case 900000:
					return model.NullVal(model.TypeString), nil
				case 900001:
					return model.StringVal("middle"), nil
				default:
					return model.Value{}, failure
				}
			}, nil
		})
		program := compile(t, f.graph(), roots, opts)
		s, err := expr.NewState(program, expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		input := columnar.Batch{Length: 1}
		out := []model.Value{model.StringVal("previous"), model.StringVal("payload"), model.StringVal("references")}
		if err := s.EvalBorrowedRow(t.Context(), &input, nil, out, mode); !errors.Is(err, failure) {
			t.Fatalf("mode %d suppressed later error: %v", mode, err)
		}
		if !slices.Equal(calls, []model.FunctionID{900000, 900001, 900002}) {
			t.Fatalf("mode %d evaluation order=%v", mode, calls)
		}
		for _, value := range out {
			if !reflect.DeepEqual(value, model.Value{}) {
				t.Fatalf("mode %d failed evaluation published or retained %+v", mode, value)
			}
		}
		requireReleasedBorrowedState(t, q)
	}
}
