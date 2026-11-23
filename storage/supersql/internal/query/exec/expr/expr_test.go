package expr_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type fixture struct {
	t       *testing.T
	b       *ir.Builder
	scope   ir.ScopeID
	layouts map[model.TypeID]ir.LayoutID
	nodes   map[ir.ScalarID]ir.Scalar
	x       ir.ScalarID
	slot    model.SlotID
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	b, err := ir.NewBuilder(ir.Limits{MaxNodes: 4096, MaxOperands: 16384, MaxSlots: 256, MaxDepth: 64, MaxBytes: 8 << 20, SourceBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, b: b, layouts: map[model.TypeID]ir.LayoutID{}, nodes: map[ir.ScalarID]ir.Scalar{}}
	f.scope, err = b.AddScope(t.Context(), 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, typ := range []model.Type{{ID: 20, Storage: model.TypeInt64}, {ID: 16, Storage: model.TypeBoolean}, {ID: 701, Storage: model.TypeFloat64}, {ID: 25, Storage: model.TypeString}} {
		f.layouts[typ.ID], err = b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
	}
	f.slot, err = b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layouts[20], Nullable: true, Object: 1, Column: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.x = f.add(ir.Scalar{Kind: ir.Slot, Layout: f.layouts[20], Slot: f.slot, Nullable: true}, nil, model.Value{})
	return f
}
func (f *fixture) add(n ir.Scalar, args []ir.ScalarID, value model.Value) ir.ScalarID {
	f.t.Helper()
	n.Scope = f.scope
	for _, arg := range args {
		n.Effects = n.Effects.Union(f.nodes[arg].Effects)
	}
	id, err := f.b.AddScalar(f.t.Context(), ir.ScalarSpec{Scalar: n, Operands: args, Value: value})
	if err != nil {
		f.t.Fatal(err)
	}
	f.nodes[id] = n
	return id
}
func (f *fixture) constant(id model.TypeID, value model.Value) ir.ScalarID {
	return f.add(ir.Scalar{Kind: ir.Const, Layout: f.layouts[id], Nullable: value.IsNull}, nil, value)
}
func (f *fixture) call(name string, result model.TypeID, args ...ir.ScalarID) ir.ScalarID {
	f.t.Helper()
	var selected builtin.Signature
	for sig := range builtin.SignaturesNamed(name) {
		if sig.Result != result || int(sig.ArgumentCount) != len(args) {
			continue
		}
		match := true
		for i, arg := range args {
			if f.nodes[arg].Layout != f.layouts[sig.Arguments[i]] {
				match = false
			}
		}
		if match {
			selected = sig
			break
		}
	}
	if selected.ID == 0 {
		f.t.Fatalf("missing signature %s", name)
	}
	return f.function(selected.ID, result, selected.Strict, ir.Effects{MayError: true}, args...)
}
func (f *fixture) function(id model.FunctionID, result model.TypeID, strict bool, effects ir.Effects, args ...ir.ScalarID) ir.ScalarID {
	f.t.Helper()
	parameters := make([]ir.LayoutID, len(args))
	for i, arg := range args {
		parameters[i] = f.nodes[arg].Layout
	}
	signature, err := f.b.AddSignature(f.t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: id, Result: f.layouts[result], Strict: strict, Nullable: true, Effects: effects}, Parameters: parameters})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.add(ir.Scalar{Kind: ir.Call, Layout: f.layouts[result], Signature: signature, Function: id, Strict: strict, Nullable: true, Effects: effects}, args, model.Value{})
}
func (f *fixture) graph() *ir.Graph {
	f.t.Helper()
	root, err := f.b.AddRel(f.t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Scan, Scope: f.scope, Object: 1, DescriptorVersion: 1, Generation: 1}, Outputs: []model.SlotID{f.slot}})
	if err != nil {
		f.t.Fatal(err)
	}
	graph, err := f.b.Build(f.t.Context(), root)
	if err != nil {
		f.t.Fatal(err)
	}
	return graph
}
func (f *fixture) options() expr.Options {
	return expr.Options{Limits: expr.DefaultLimits(), Inputs: []expr.Input{{Slot: f.slot, Column: 0}}}
}
func compile(t *testing.T, graph *ir.Graph, roots []ir.ScalarID, options expr.Options) *expr.Program {
	t.Helper()
	p, err := expr.Compile(t.Context(), graph, roots, options)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func state(t *testing.T, p *expr.Program) *expr.State {
	t.Helper()
	s, err := expr.NewState(p, expr.StateOptions{})
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
func batch(rows int) columnar.Batch {
	v := columnar.Vector{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: rows, Int64: make([]int64, rows), Nulls: make([]byte, (rows+7)/8)}
	for i := range rows {
		if i%3 == 1 {
			v.Int64[i] = 2
		}
		if i%3 == 2 {
			v.Nulls[i/8] |= 1 << uint(i%8)
		}
	}
	return columnar.Batch{Columns: []columnar.Vector{v}, Length: rows}
}
func value(t *testing.T, b *columnar.Batch, col, row int) model.Value {
	t.Helper()
	v, _, err := columnar.CopyValue(t.Context(), &b.Columns[col], b.Lane(row), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func requireInt(t *testing.T, v model.Value, want int64, null bool) {
	t.Helper()
	if v.IsNull != null || (!null && v.IntVal != want) {
		t.Fatalf("value=%+v want %d null=%v", v, want, null)
	}
}

type resolverFunc func(expr.Function) (expr.ScalarCall, error)

func (r resolverFunc) Resolve(f expr.Function) (expr.ScalarCall, error) { return r(f) }

func TestVectorMaskedEvaluation(t *testing.T) {
	for _, rows := range []int{1, 17, 1024} {
		for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
			t.Run(fmt.Sprintf("rows%d/mode%d", rows, mode), func(t *testing.T) {
				f := newFixture(t)
				zero, ten := f.constant(20, model.Int64Val(0)), f.constant(20, model.Int64Val(10))
				condition := f.call("int8eq", 16, f.x, zero)
				divide := f.call("int8div", 20, ten, f.x)
				guarded := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{condition, zero, divide}, model.Value{})
				volatile := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile}, f.x)
				counted := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{condition, volatile, zero}, model.Value{})
				coalesce := f.add(ir.Scalar{Kind: ir.Coalesce, Layout: f.layouts[20]}, []ir.ScalarID{f.x, ten}, model.Value{})
				nullBool := f.constant(16, model.NullVal(model.TypeBoolean))
				falseBool, trueBool := f.constant(16, model.BoolVal(false)), f.constant(16, model.BoolVal(true))
				and := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.And, Layout: f.layouts[16], Nullable: true}, []ir.ScalarID{nullBool, falseBool}, model.Value{})
				or := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.Or, Layout: f.layouts[16], Nullable: true}, []ir.ScalarID{nullBool, trueBool}, model.Value{})
				nullAnd := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.And, Layout: f.layouts[16], Nullable: true}, []ir.ScalarID{trueBool, nullBool}, model.Value{})
				bad := f.call("int8div", 20, ten, zero)
				badBool := f.call("int8eq", 16, bad, zero)
				shortAnd := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.And, Layout: f.layouts[16], Nullable: true}, []ir.ScalarID{falseBool, badBool}, model.Value{})
				shortOr := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.Or, Layout: f.layouts[16], Nullable: true}, []ir.ScalarID{trueBool, badBool}, model.Value{})
				shortCoalesce := f.add(ir.Scalar{Kind: ir.Coalesce, Layout: f.layouts[20]}, []ir.ScalarID{ten, bad}, model.Value{})
				calls := 0
				options := f.options()
				options.Functions = resolverFunc(func(fn expr.Function) (expr.ScalarCall, error) {
					if fn.ID != 900000 {
						t.Fatalf("unexpected function %d", fn.ID)
					}
					return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
						calls++
						return model.Int64Val(args[0].IntVal + 7), nil
					}, nil
				})
				roots := []ir.ScalarID{guarded, counted, coalesce, and, or, nullAnd, shortAnd, shortOr, shortCoalesce}
				p := compile(t, f.graph(), roots, options)
				input, out := batch(rows), columnar.Batch{}
				if err := state(t, p).Eval(t.Context(), &input, nil, &out, mode); err != nil {
					t.Fatal(err)
				}
				if calls != (rows+2)/3 {
					t.Fatalf("volatile calls=%d want %d", calls, (rows+2)/3)
				}
				expected := [...]struct {
					quotient     int64
					quotientNull bool
					counted      int64
					coalesced    int64
				}{
					{quotient: 0, counted: 7, coalesced: 0},
					{quotient: 5, counted: 0, coalesced: 2},
					{quotientNull: true, counted: 0, coalesced: 10},
				}
				for i := range rows {
					want := expected[i%len(expected)]
					requireInt(t, value(t, &out, 0, i), want.quotient, want.quotientNull)
					requireInt(t, value(t, &out, 1, i), want.counted, false)
					requireInt(t, value(t, &out, 2, i), want.coalesced, false)
					requireInt(t, value(t, &out, 3, i), 0, false)
					requireInt(t, value(t, &out, 4, i), 1, false)
					requireInt(t, value(t, &out, 5, i), 0, true)
					requireInt(t, value(t, &out, 6, i), 0, false)
					requireInt(t, value(t, &out, 7, i), 1, false)
					requireInt(t, value(t, &out, 8, i), 10, false)
				}
			})
		}
	}
}

func TestVectorFloatNaNAndOverflow(t *testing.T) {
	for _, rows := range []int{1, 17, 1024} {
		for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
			t.Run(fmt.Sprintf("rows%d/mode%d", rows, mode), func(t *testing.T) {
				f := newFixture(t)
				nan := f.constant(701, model.Float64Val(math.NaN()))
				inf := f.constant(701, model.Float64Val(math.Inf(1)))
				eq, gt := f.call("float8eq", 16, nan, nan), f.call("float8gt", 16, nan, inf)
				maxInt, one := f.constant(20, model.Int64Val(math.MaxInt64)), f.constant(20, model.Int64Val(1))
				overflow := f.call("int8pl", 20, maxInt, one)
				maxFloat := f.constant(701, model.Float64Val(math.MaxFloat64))
				floatOverflow := f.call("float8mul", 701, maxFloat, maxFloat)
				g, options := f.graph(), f.options()
				input, out := batch(rows), columnar.Batch{}
				if err := state(t, compile(t, g, []ir.ScalarID{eq, gt}, options)).Eval(t.Context(), &input, nil, &out, mode); err != nil {
					t.Fatal(err)
				}
				for row := range input.Rows() {
					requireInt(t, value(t, &out, 0, row), 1, false)
					requireInt(t, value(t, &out, 1, row), 1, false)
				}
				for _, root := range []ir.ScalarID{overflow, floatOverflow} {
					err := state(t, compile(t, g, []ir.ScalarID{root}, options)).Eval(t.Context(), &input, nil, &out, mode)
					var sql *model.Error
					if !errors.As(err, &sql) || sql.Code != "22003" {
						t.Fatalf("overflow error=%v", err)
					}
					if out.Rows() != 0 || len(out.Columns) != 0 {
						t.Fatal("failed evaluation published partial output")
					}
				}
			})
		}
	}
}

func TestVectorMaskedNumericOverflow(t *testing.T) {
	for _, rows := range []int{1, 17, 1024} {
		for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
			t.Run(fmt.Sprintf("rows%d/mode%d", rows, mode), func(t *testing.T) {
				f := newFixture(t)
				for _, typ := range []model.Type{{ID: 23, Storage: model.TypeInt64}, {ID: 1700, Storage: model.TypeNumeric}} {
					layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
					if err != nil {
						t.Fatal(err)
					}
					f.layouts[typ.ID] = layout
				}
				zero := f.constant(20, model.Int64Val(0))
				condition := f.call("int8eq", 16, f.x, zero)
				valid := f.constant(1700, model.Value{Type: model.TypeNumeric, StrVal: "0.00"})
				inputNumber := f.constant(1700, model.Value{Type: model.TypeNumeric, StrVal: "9.995"})
				typmod := f.constant(23, model.Int64Val((3<<16)+2+4))
				overflow := f.function(1703, 1700, true, ir.Effects{MayError: true}, inputNumber, typmod)
				guarded := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[1700], Nullable: true}, []ir.ScalarID{condition, valid, overflow}, model.Value{})
				program := compile(t, f.graph(), []ir.ScalarID{guarded}, f.options())
				state := state(t, program)
				input, output := batch(rows), columnar.Batch{}
				clear(input.Columns[0].Int64)
				clear(input.Columns[0].Nulls)
				if err := state.Eval(t.Context(), &input, nil, &output, mode); err != nil {
					t.Fatal(err)
				}
				for i := range rows {
					got := value(t, &output, 0, i)
					if got.IsNull || got.StrVal != "0.00" {
						t.Fatalf("masked numeric row=%+v", got)
					}
				}
				input.Columns[0].Int64[rows-1] = 1
				err := state.Eval(t.Context(), &input, nil, &output, mode)
				var sqlErr *model.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != "22003" {
					t.Fatalf("active numeric overflow=%v", err)
				}
				if output.Rows() != 0 {
					t.Fatal("numeric overflow published a partial batch")
				}
			})
		}
	}
}

func TestVectorEncodingsAndSelection(t *testing.T) {
	f := newFixture(t)
	one := f.constant(20, model.Int64Val(1))
	root := f.call("int8pl", 20, f.x, one)
	p := compile(t, f.graph(), []ir.ScalarID{root}, f.options())
	s := state(t, p)
	for _, test := range []struct {
		name      string
		vector    columnar.Vector
		selection []uint32
		want      []int64
		null      []bool
	}{
		{"flat offset", columnar.Vector{Encoding: columnar.Flat, Offset: 1, Length: 3, Int64: []int64{99, 2, 5, 9}}, []uint32{2, 0}, []int64{10, 3}, []bool{false, false}},
		{"dictionary", columnar.Vector{Encoding: columnar.Dictionary, Length: 3, Int64: []int64{2, 9}, Indices: []uint32{0, 1, 0}, Nulls: []byte{4}}, []uint32{1, 2, 0}, []int64{10, 0, 3}, []bool{false, true, false}},
		{"constant", columnar.Vector{Encoding: columnar.Constant, Length: 3, Int64: []int64{2}}, []uint32{2, 0}, []int64{3, 3}, []bool{false, false}},
		{"empty selection", columnar.Vector{Encoding: columnar.Constant, Length: 3, Int64: []int64{2}}, []uint32{}, nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			test.vector.Type = model.Type{ID: 20, Storage: model.TypeInt64}
			input := columnar.Batch{Columns: []columnar.Vector{test.vector}, Length: 3, Selection: test.selection}
			out := columnar.Batch{}
			if err := s.Eval(t.Context(), &input, nil, &out, expr.Direct); err != nil {
				t.Fatal(err)
			}
			if out.Rows() != len(test.want) {
				t.Fatalf("rows=%d want %d", out.Rows(), len(test.want))
			}
			for row, want := range test.want {
				requireInt(t, value(t, &out, 0, row), want, test.null[row])
			}
		})
	}
}

func TestCompileRejectsInvalidBindings(t *testing.T) {
	f := newFixture(t)
	param := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[20], Parameter: 1, Nullable: true}, nil, model.Value{})
	g := f.graph()
	if _, err := expr.NewState(&expr.Program{}, expr.StateOptions{}); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("zero program=%v", err)
	}
	if _, err := expr.Compile(t.Context(), g, []ir.ScalarID{999999}, f.options()); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("unknown root=%v", err)
	}
	options := f.options()
	options.Inputs = nil
	if _, err := expr.Compile(t.Context(), g, []ir.ScalarID{f.x}, options); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("unmapped slot=%v", err)
	}
	if _, err := expr.Compile(t.Context(), g, []ir.ScalarID{param}, f.options()); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("unmapped param=%v", err)
	}
	options = f.options()
	options.Inputs = append(options.Inputs, options.Inputs[0])
	if _, err := expr.Compile(t.Context(), g, []ir.ScalarID{f.x}, options); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("duplicate slot=%v", err)
	}
	options = f.options()
	options.Limits.MaxInstructions = 1
	if _, err := expr.Compile(t.Context(), g, []ir.ScalarID{f.x}, options); !errors.Is(err, expr.ErrLimit) {
		t.Fatalf("instruction bound=%v", err)
	}
}

func TestEvaluationBoundsParametersAndCancellation(t *testing.T) {
	f := newFixture(t)
	param := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[20], Parameter: 1, Nullable: true}, nil, model.Value{})
	g, options := f.graph(), f.options()
	layout := model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
	p := compile(t, g, []ir.ScalarID{f.x, param}, options)
	s, input, out := state(t, p), batch(17), columnar.Batch{}
	params := []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Int64Val(42)}}
	if err := s.Eval(t.Context(), &input, params, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	requireInt(t, value(t, &out, 1, 0), 42, false)
	params[0].Layout.Type.ID = 23
	if err := s.Eval(t.Context(), &input, params, &out, expr.Direct); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("param mismatch=%v", err)
	}
	params[0].Layout = layout
	input.Columns[0].Type.ID = 23
	if err := s.Eval(t.Context(), &input, params, &out, expr.Direct); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("slot mismatch=%v", err)
	}
	input = batch(17)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Eval(ctx, &input, params, &out, expr.Direct); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if out.Rows() != 0 || len(out.Columns) != 0 {
		t.Fatal("error retains output references")
	}
	if err := s.Eval(t.Context(), &input, params, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	options.Limits.MaxStateBytes = 1
	if err := state(t, compile(t, g, []ir.ScalarID{param}, options)).Eval(t.Context(), &input, params, &out, expr.Direct); !errors.Is(err, expr.ErrLimit) {
		t.Fatalf("state budget=%v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Eval(t.Context(), &input, params, &out, expr.Direct); !errors.Is(err, expr.ErrClosed) {
		t.Fatalf("closed eval=%v", err)
	}
}

func TestOutputBudgetAndRetainedNestedOwnership(t *testing.T) {
	f := newFixture(t)
	text := f.constant(25, model.StringVal(strings.Repeat("x", 2048)))
	layout := model.TypeLayout{Type: model.Type{ID: 1009, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 25, Storage: model.TypeString}}}}
	arrayLayout, err := f.b.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	array := f.add(ir.Scalar{Kind: ir.Array, Layout: arrayLayout}, []ir.ScalarID{text}, model.Value{})
	g, options := f.graph(), f.options()
	input, out := batch(1), columnar.Batch{}
	options.Limits.MaxOutputBytes = 1024
	err = state(t, compile(t, g, []ir.ScalarID{array}, options)).Eval(t.Context(), &input, nil, &out, expr.Direct)
	if !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("output budget=%v", err)
	}
	if len(out.Columns) != 0 {
		t.Fatal("failed output remains published")
	}
	options.Limits.MaxOutputBytes = 1 << 20
	p := compile(t, g, []ir.ScalarID{array}, options)
	s := state(t, p)
	if err := s.Eval(t.Context(), &input, nil, &out, expr.Direct); err != nil {
		t.Fatal(err)
	}
	retained, err := expr.Retain(t.Context(), &out, p.OutputLayouts(), expr.RetainOptions{MaxRows: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := retained.Close(); err != nil {
			t.Error(err)
		}
	})
	var held columnar.Batch
	if err := retained.Publish(&held); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- retained.Close() }()
	v := value(t, &held, 0, 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(v.ArrayVal) != 1 || v.ArrayVal[0].StrVal != strings.Repeat("x", 2048) {
		t.Fatalf("retained array=%+v", v)
	}
	if err := retained.Publish(&columnar.Batch{}); !errors.Is(err, expr.ErrClosed) {
		t.Fatalf("closed retained=%v", err)
	}
}

func TestCancellationInsideVolatileCall(t *testing.T) {
	f := newFixture(t)
	root := f.function(900000, 20, false, ir.Effects{Volatility: ir.Volatile})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	options := f.options()
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			calls++
			cancel()
			return model.Int64Val(1), nil
		}, nil
	})
	s := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options))
	input, out := batch(17), columnar.Batch{}
	if err := s.Eval(ctx, &input, nil, &out, expr.Direct); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-call cancellation=%v", err)
	}
	if calls != 1 || len(out.Columns) != 0 {
		t.Fatalf("calls=%d published columns=%d", calls, len(out.Columns))
	}
}
