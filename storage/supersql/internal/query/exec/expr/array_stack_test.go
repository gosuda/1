package expr_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (f *fixture) arrayLayout(arrayID model.TypeID, element model.Type) model.TypeLayout {
	f.t.Helper()
	layout := model.TypeLayout{Type: model.Type{ID: arrayID, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: element}}}
	id, err := f.b.AddLayout(f.t.Context(), layout)
	if err != nil {
		f.t.Fatal(err)
	}
	f.layouts[arrayID] = id
	return layout
}

func (f *fixture) arrayStack(args ...ir.ScalarID) ir.ScalarID {
	return f.add(ir.Scalar{Kind: ir.ArrayStack, Layout: f.nodes[args[0]].Layout, Effects: ir.Effects{MayError: true}}, args, model.Value{})
}

func stackArray(dimensions []model.ArrayDimension, elements ...model.Value) model.Value {
	return model.Value{Type: model.TypeArray, Dimensions: dimensions, ArrayVal: elements}
}

func requireStack(t *testing.T, got model.Value, dimensions []model.ArrayDimension, elements []model.Value) {
	t.Helper()
	if got.IsNull || got.Type != model.TypeArray || !slices.Equal(got.Dimensions, dimensions) || len(got.ArrayVal) != len(elements) {
		t.Fatalf("stack shape = %v, null=%v, elements=%d; want %v, nonnull, elements=%d", got.Dimensions, got.IsNull, len(got.ArrayVal), dimensions, len(elements))
	}
	for i, want := range elements {
		actual := got.ArrayVal[i]
		wrongType := actual.Type != want.Type || actual.IsNull != want.IsNull
		wrongPayload := !want.IsNull && (actual.IntVal != want.IntVal || actual.StrVal != want.StrVal || !bytes.Equal(actual.BytesVal, want.BytesVal))
		if wrongType || wrongPayload {
			t.Fatalf("element %d = %+v, want %+v", i, actual, want)
		}
	}
}

func requireStackSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func TestArrayStackShapeTransitions(t *testing.T) {
	inner := []model.ArrayDimension{{Length: 2, LowerBound: -3}}
	left := stackArray(inner, model.Int64Val(1), model.NullVal(model.TypeInt64))
	right := stackArray(inner, model.Int64Val(3), model.Int64Val(4))
	flat := []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4)}
	want := []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 2, LowerBound: -3}}
	empty, null := stackArray(nil), model.NullVal(model.TypeArray)
	rank5 := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: -1}, {Length: 1, LowerBound: 2}, {Length: 1, LowerBound: 3}, {Length: 1, LowerBound: 4}, {Length: 1, LowerBound: math.MaxInt32 - 1}}, model.Int64Val(7))
	rank6 := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}}, model.Int64Val(8))
	maxLast := stackArray([]model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32 - 1}}, model.Int64Val(0), model.Int64Val(1))
	maxLastRank5 := rank5.Clone()
	maxLastRank5.Dimensions[4].LowerBound = math.MaxInt32
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		layout := f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		a := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1, Nullable: true}, nil, model.Value{})
		b := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 2, Nullable: true}, nil, model.Value{})
		root := f.arrayStack(a, b)
		options := f.options()
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}, {Number: 2, Layout: layout}}
		q := allocationQuery(t)
		s, err := expr.NewState(compile(t, f.graph(), []ir.ScalarID{root}, options), expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		input, out := batch(1), columnar.Batch{}
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout}, {Number: 2, Layout: layout}}
		for _, test := range []struct {
			name        string
			left, right model.Value
			dimensions  []model.ArrayDimension
			elements    []model.Value
			code        string
		}{
			{name: "nondefault bounds and null element", left: left, right: right, dimensions: want, elements: flat},
			{name: "all empty", left: empty, right: empty},
			{name: "all null", left: null, right: null},
			{name: "empty and null", left: empty, right: null},
			{name: "empty before nonempty", left: empty, right: right, code: "2202E"},
			{name: "null after nonempty", left: left, right: null, code: "2202E"},
			{name: "different lengths", left: left, right: stackArray([]model.ArrayDimension{{Length: 1, LowerBound: -3}}, model.Int64Val(2)), code: "2202E"},
			{name: "different lower bounds", left: left, right: stackArray([]model.ArrayDimension{{Length: 2, LowerBound: 1}}, model.Int64Val(2), model.Int64Val(3)), code: "2202E"},
			{name: "different ranks", left: left, right: stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 2, LowerBound: -3}}, model.Int64Val(2), model.Int64Val(3)), code: "2202E"},
			{name: "six dimensional result", left: rank5, right: rank5, dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 1, LowerBound: -1}, {Length: 1, LowerBound: 2}, {Length: 1, LowerBound: 3}, {Length: 1, LowerBound: 4}, {Length: 1, LowerBound: math.MaxInt32 - 1}}, elements: []model.Value{model.Int64Val(7), model.Int64Val(7)}},
			{name: "representable maximum last subscript", left: maxLast, right: maxLast, code: "54000"},
			{name: "maximum last subscript in inner dimension", left: maxLastRank5, right: maxLastRank5, code: "54000"},
			{name: "shape mismatch before constructor bounds", left: maxLast, right: right, code: "2202E"},
			{name: "empty before constructor bounds", left: empty, right: maxLast, code: "2202E"},
			{name: "first rank six", left: rank6, right: rank6, code: "54000"},
			{name: "empty before first rank six", left: empty, right: rank6, code: "54000"},
			{name: "later rank six", left: left, right: rank6, code: "2202E"},
		} {
			t.Run(test.name, func(t *testing.T) {
				parameters[0].Value, parameters[1].Value = test.left, test.right
				err := s.Eval(t.Context(), &input, parameters, &out, mode)
				if test.code == "" {
					if err != nil {
						t.Fatal(err)
					}
					requireStack(t, value(t, &out, 0, 0), test.dimensions, test.elements)
				} else {
					requireStackSQLState(t, err, test.code)
					if out.Rows() != 0 || len(out.Columns) != 0 {
						t.Fatal("failed stack published output")
					}
					if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
						t.Fatalf("failed stack retained claims: %+v", usage)
					}
				}
				parameters[0].Value, parameters[1].Value = left, right
				if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
					t.Fatalf("state reuse: %v", err)
				}
				requireStack(t, value(t, &out, 0, 0), want, flat)
			})
		}
	}
}

func TestArrayStackNestedConstructors(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		one, null := f.constant(20, model.Int64Val(1)), f.constant(20, model.NullVal(model.TypeInt64))
		child := f.add(ir.Scalar{Kind: ir.Array, Layout: f.layouts[1016]}, []ir.ScalarID{one, null}, model.Value{})
		matrix := f.arrayStack(child, child)
		cube := f.arrayStack(matrix)
		input, out := batch(1), columnar.Batch{}
		if err := state(t, compile(t, f.graph(), []ir.ScalarID{cube}, f.options())).Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 2, LowerBound: 1}, {Length: 2, LowerBound: 1}}, []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(1), model.NullVal(model.TypeInt64)})
	}
}

func TestArrayStackMasksShapeErrors(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		empty := f.constant(1016, stackArray(nil))
		one := f.constant(1016, stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.Int64Val(1)))
		bad := f.arrayStack(empty, one)
		condition := f.call("int8eq", 16, f.x, f.constant(20, model.Int64Val(0)))
		guarded := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[1016]}, []ir.ScalarID{condition, empty, bad}, model.Value{})
		coalesce := f.add(ir.Scalar{Kind: ir.Coalesce, Layout: f.layouts[1016]}, []ir.ScalarID{empty, bad}, model.Value{})
		nullTest := f.add(ir.Scalar{Kind: ir.NullTest, Layout: f.layouts[16]}, []ir.ScalarID{bad}, model.Value{})
		and := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.And, Layout: f.layouts[16]}, []ir.ScalarID{f.constant(16, model.BoolVal(false)), nullTest}, model.Value{})
		or := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.Or, Layout: f.layouts[16]}, []ir.ScalarID{f.constant(16, model.BoolVal(true)), nullTest}, model.Value{})
		s := state(t, compile(t, f.graph(), []ir.ScalarID{guarded, coalesce, and, or}, f.options()))
		input, out := batch(1), columnar.Batch{}
		if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &out, 0, 0), nil, nil)
		requireStack(t, value(t, &out, 1, 0), nil, nil)
		requireInt(t, value(t, &out, 2, 0), 0, false)
		requireInt(t, value(t, &out, 3, 0), 1, false)
		input = batch(2)
		requireStackSQLState(t, s.Eval(t.Context(), &input, nil, &out, mode), "2202E")
		if out.Rows() != 0 || len(out.Columns) != 0 {
			t.Fatal("required shape error published partial rows")
		}
	}
}

func TestArrayStackEvaluatesChildrenBeforeCheckingShape(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		empty := f.constant(1016, stackArray(nil))
		one := f.constant(1016, stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.Int64Val(1)))
		divide := f.call("int8div", 20, f.constant(20, model.Int64Val(1)), f.constant(20, model.Int64Val(0)))
		last := f.add(ir.Scalar{Kind: ir.Array, Layout: f.layouts[1016]}, []ir.ScalarID{divide}, model.Value{})
		root := f.arrayStack(empty, one, last)
		input, out := batch(1), columnar.Batch{}
		requireStackSQLState(t, state(t, compile(t, f.graph(), []ir.ScalarID{root}, f.options())).Eval(t.Context(), &input, nil, &out, mode), "22012")
		if out.Rows() != 0 || len(out.Columns) != 0 {
			t.Fatal("child error published output")
		}
	}
}

func TestArrayStackRejectsInvalidParameterMetadata(t *testing.T) {
	f := newFixture(t)
	layout := f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
	parameter := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1, Nullable: true}, nil, model.Value{})
	root := f.arrayStack(parameter)
	options := f.options()
	options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
	s := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options))
	input, out := batch(1), columnar.Batch{}
	for _, invalid := range []model.Value{
		stackArray(nil, model.Int64Val(1)),
		stackArray([]model.ArrayDimension{{Length: 0, LowerBound: 1}}),
		stackArray([]model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32}}, model.Int64Val(1), model.Int64Val(2)),
		stackArray(make([]model.ArrayDimension, 7), model.Int64Val(1)),
		stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.StringVal("wrong storage")),
	} {
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: invalid}}
		err := s.Eval(t.Context(), &input, parameters, &out, expr.Direct)
		if !errors.Is(err, model.ErrInvalidArrayShape) && !errors.Is(err, expr.ErrInvalid) {
			t.Fatalf("invalid array metadata returned %v", err)
		}
		if out.Rows() != 0 || len(out.Columns) != 0 {
			t.Fatal("invalid parameter published output")
		}
	}
}

func TestArrayStackAdmitsExactHeaderBudget(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		layout := f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		a := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1}, nil, model.Value{})
		b := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 2}, nil, model.Value{})
		root := f.arrayStack(a, b)
		graph, options := f.graph(), f.options()
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}, {Number: 2, Layout: layout}}
		q := allocationQuery(t)
		stateOptions := expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)}
		probe, err := expr.NewState(compile(t, graph, []ir.ScalarID{root}, options), stateOptions)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := probe.Close(); err != nil {
				t.Error(err)
			}
		})
		input, out := batch(1), columnar.Batch{}
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: stackArray(nil)}, {Number: 2, Layout: layout, Value: stackArray(nil)}}
		if err := probe.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		base := uint64(q.Usage().Work.Peak)
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
		one := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.Int64Val(5))
		cost := 2*uint64(unsafe.Sizeof(model.Value{})) + 2*uint64(unsafe.Sizeof(model.ArrayDimension{}))
		for _, short := range []uint64{0, 1} {
			options.Limits.MaxStateBytes = base + cost - short
			s, err := expr.NewState(compile(t, graph, []ir.ScalarID{root}, options), stateOptions)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			parameters[0].Value, parameters[1].Value = one, one
			err = s.Eval(t.Context(), &input, parameters, &out, mode)
			if short == 0 {
				if err != nil {
					t.Fatalf("exact stack budget: %v", err)
				}
				requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 1, LowerBound: 1}}, []model.Value{model.Int64Val(5), model.Int64Val(5)})
			} else if !errors.Is(err, expr.ErrLimit) {
				t.Fatalf("one-byte-short stack budget: %v", err)
			}
			parameters[1].Value = stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 2}}, model.Int64Val(6))
			requireStackSQLState(t, s.Eval(t.Context(), &input, parameters, &out, mode), "2202E")
			if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
				t.Fatalf("rejected stack retained claims: %+v", usage)
			}
			parameters[0].Value, parameters[1].Value = stackArray(nil), stackArray(nil)
			if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
				t.Fatalf("reuse after quota error: %v", err)
			}
			requireStack(t, value(t, &out, 0, 0), nil, nil)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestArrayStackBorrowsPayloadsUntilOwnedPublication(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		layout := f.arrayLayout(1001, model.Type{ID: 17, Storage: model.TypeBytes})
		parameter := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1001], Parameter: 1}, nil, model.Value{})
		root := f.arrayStack(parameter, parameter)
		options := f.options()
		options.Limits.MaxStateBytes = 8 << 10
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
		program := compile(t, f.graph(), []ir.ScalarID{root}, options)
		s := state(t, program)
		payload := bytes.Repeat([]byte{'x'}, 64<<10)
		child := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 4}}, model.BytesVal(payload))
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: child}}
		input, out := batch(1), columnar.Batch{}
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		clear(payload)
		child.Dimensions[0].LowerBound = -7
		child.ArrayVal[0] = model.NullVal(model.TypeBytes)
		want := model.BytesVal(bytes.Repeat([]byte{'x'}, 64<<10))
		requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 1, LowerBound: 4}}, []model.Value{want, want})
		retained, err := expr.Retain(t.Context(), &out, program.OutputLayouts(), expr.RetainOptions{MaxRows: 1, MaxBytes: 512 << 10})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := retained.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 1, LowerBound: -7}}, []model.Value{model.NullVal(model.TypeBytes), model.NullVal(model.TypeBytes)})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		var held columnar.Batch
		if err := retained.Publish(&held); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &held, 0, 0), []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 1, LowerBound: 4}}, []model.Value{want, want})
	}
}

type arrayStackCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *arrayStackCancelContext) Err() error {
	if c.remaining > 0 {
		c.remaining--
		if c.remaining == 0 {
			c.cancel()
		}
	}
	return c.Context.Err()
}

type arrayStackCancelAllocator struct {
	columnar.Allocator
	ctx   *arrayStackCancelContext
	bytes uint64
}

func (a arrayStackCancelAllocator) Reserve(bytes uint64) (columnar.Allocation, error) {
	claim, err := a.Allocator.Reserve(bytes)
	if err != nil {
		return nil, err
	}
	return &arrayStackCancelAllocation{Allocation: claim, allocator: a, bytes: bytes}, nil
}

type arrayStackCancelAllocation struct {
	columnar.Allocation
	allocator arrayStackCancelAllocator
	bytes     uint64
}

func (a *arrayStackCancelAllocation) Resize(bytes uint64) error {
	if err := a.Allocation.Resize(bytes); err != nil {
		return err
	}
	if bytes > a.bytes && bytes-a.bytes == a.allocator.bytes {
		a.allocator.ctx.remaining = 2
	}
	a.bytes = bytes
	return nil
}

func TestArrayStackCancellationReleasesMetadataAndCopyWork(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, phase := range []string{"metadata", "copy"} {
			t.Run(phase, func(t *testing.T) {
				f := newFixture(t)
				layout := f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
				parameter := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1}, nil, model.Value{})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				interrupt := &arrayStackCancelContext{Context: ctx, cancel: cancel}
				q := allocationQuery(t)
				stateOptions := expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)}
				options := f.options()
				options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
				child := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.Int64Val(9))
				var root ir.ScalarID
				if phase == "metadata" {
					last := f.function(900000, 1016, false, ir.Effects{Volatility: ir.Volatile})
					args := make([]ir.ScalarID, 513)
					for i := range args {
						args[i] = parameter
					}
					args[len(args)-1] = last
					root = f.arrayStack(args...)
					options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
						return func(ctx context.Context, _ []model.Value, _ builtin.CallBudget) (model.Value, error) {
							if ctx == interrupt {
								interrupt.remaining = 5
								return model.NullVal(model.TypeArray), nil
							}
							return child, nil
						}, nil
					})
				} else {
					child.ArrayVal = make([]model.Value, 1024)
					for i := range child.ArrayVal {
						child.ArrayVal[i] = model.Int64Val(9)
					}
					child.Dimensions[0].Length = int32(len(child.ArrayVal))
					root = f.arrayStack(parameter, parameter)
					cost := uint64(2*len(child.ArrayVal))*uint64(unsafe.Sizeof(model.Value{})) + 2*uint64(unsafe.Sizeof(model.ArrayDimension{}))
					stateOptions.WorkAllocator = arrayStackCancelAllocator{Allocator: stateOptions.WorkAllocator, ctx: interrupt, bytes: cost}
				}
				s, err := expr.NewState(compile(t, f.graph(), []ir.ScalarID{root}, options), stateOptions)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				})
				input, out := batch(1), columnar.Batch{}
				parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: child}}
				if err := s.Eval(interrupt, &input, parameters, &out, mode); !errors.Is(err, context.Canceled) {
					t.Fatalf("%s cancellation = %v", phase, err)
				}
				if out.Rows() != 0 || len(out.Columns) != 0 {
					t.Fatal("canceled stack published output")
				}
				if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
					t.Fatalf("canceled stack retained claims: %+v", usage)
				}
				if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
					t.Fatalf("state reuse after cancellation: %v", err)
				}
				count := 2048
				dimensions := []model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 1024, LowerBound: 1}}
				if phase == "metadata" {
					count = 513
					dimensions = []model.ArrayDimension{{Length: 513, LowerBound: 1}, {Length: 1, LowerBound: 1}}
				}
				want := make([]model.Value, count)
				for i := range want {
					want[i] = model.Int64Val(9)
				}
				requireStack(t, value(t, &out, 0, 0), dimensions, want)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if usage := q.Usage(); usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Reservations != 0 {
					t.Fatalf("close retained stack claims: %+v", usage)
				}
			})
		}
	}
}
