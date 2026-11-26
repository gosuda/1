package expr_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (f *fixture) coercionType(typ model.Type) {
	f.t.Helper()
	id, err := f.b.AddLayout(f.t.Context(), model.TypeLayout{Type: typ})
	if err != nil {
		f.t.Fatal(err)
	}
	f.layouts[typ.ID] = id
}

func (f *fixture) arrayElement(id model.TypeID) ir.ScalarID {
	return f.add(ir.Scalar{Kind: ir.ArrayElement, Layout: f.layouts[id], Nullable: true}, nil, model.Value{})
}

func (f *fixture) arrayCoerce(source ir.ScalarID, result model.TypeID, template ir.ScalarID) ir.ScalarID {
	return f.add(ir.Scalar{Kind: ir.ArrayCoerce, Layout: f.layouts[result], Nullable: f.nodes[source].Nullable, CastContext: ir.ExplicitCast, Effects: ir.Effects{MayError: true}}, []ir.ScalarID{source, template}, model.Value{})
}

func (f *fixture) numericInput(element ir.ScalarID) ir.ScalarID {
	f.coercionType(model.Type{ID: 2275, Storage: model.TypeString})
	f.coercionType(model.Type{ID: 26, Storage: model.TypeInt64})
	f.coercionType(model.Type{ID: 23, Storage: model.TypeInt64})
	f.coercionType(model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: -1})
	text := f.call("textout", 2275, element)
	return f.call("numeric_in", 1700, text, f.constant(26, model.Int64Val(0)), f.constant(23, model.Int64Val(-1)))
}

func TestArrayCoerceIntegerWideningPreservesBounds(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		integer := model.Type{ID: 23, Storage: model.TypeInt64}
		f.coercionType(integer)
		f.coercionType(model.Type{ID: 1700, Storage: model.TypeNumeric})
		f.arrayLayout(1007, integer)
		f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		f.arrayLayout(1231, model.Type{ID: 1700, Storage: model.TypeNumeric})
		dimensions := []model.ArrayDimension{{Length: 2, LowerBound: -3}, {Length: 2, LowerBound: 5}}
		elements := []model.Value{model.Int64Val(math.MinInt32), model.NullVal(model.TypeInt64), model.Int64Val(math.MaxInt32), model.Int64Val(7)}
		source := f.constant(1007, stackArray(dimensions, elements...))
		element := f.arrayElement(23)
		widen := f.call("int8", 20, element)
		cast := f.nodes[widen]
		cast.Kind, cast.CastContext = ir.Cast, ir.ImplicitCast
		widen = f.add(cast, []ir.ScalarID{element}, model.Value{})
		numeric := f.call("numeric", 1700, element)
		roots := []ir.ScalarID{f.arrayCoerce(source, 1016, widen), f.arrayCoerce(source, 1231, numeric)}
		s := state(t, compile(t, f.graph(), roots, f.options()))
		input, out := batch(1), columnar.Batch{}
		if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &out, 0, 0), dimensions, elements)
		requireStack(t, value(t, &out, 1, 0), dimensions, []model.Value{
			{Type: model.TypeNumeric, StrVal: "-2147483648"}, model.NullVal(model.TypeNumeric),
			{Type: model.TypeNumeric, StrVal: "2147483647"}, {Type: model.TypeNumeric, StrVal: "7"},
		})
	}
}

func TestArrayCoerceIOThenNumericTypmod(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.arrayLayout(1009, model.Type{ID: 25, Storage: model.TypeString})
		dimensions := []model.ArrayDimension{{Length: 3, LowerBound: 0}}
		source := f.constant(1009, stackArray(dimensions, model.StringVal("1.235"), model.StringVal("-8.765"), model.NullVal(model.TypeString)))
		parsed := f.numericInput(f.arrayElement(25))
		typmod := int32((4 << 16) + 2 + 4)
		numeric := model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: typmod}
		f.coercionType(numeric)
		f.arrayLayout(1231, numeric)
		rounded := f.function(1703, 1700, true, ir.Effects{MayError: true}, parsed, f.constant(23, model.Int64Val(int64(typmod))))
		root := f.arrayCoerce(source, 1231, rounded)
		p := compile(t, f.graph(), []ir.ScalarID{root}, f.options())
		input, out := batch(1), columnar.Batch{}
		if err := state(t, p).Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &out, 0, 0), dimensions, []model.Value{{Type: model.TypeNumeric, StrVal: "1.24"}, {Type: model.TypeNumeric, StrVal: "-8.77"}, model.NullVal(model.TypeNumeric)})
		if got := p.OutputLayouts()[0].Children[0].Type.Modifier; got != typmod {
			t.Fatalf("numeric result modifier = %d, want %d", got, typmod)
		}
	}
}

func TestArrayCoerceCompletesElementBeforeStartingNext(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.arrayLayout(1009, model.Type{ID: 25, Storage: model.TypeString})
		source := f.constant(1009, stackArray([]model.ArrayDimension{{Length: 2, LowerBound: 1}}, model.StringVal("1000"), model.StringVal("not numeric")))
		parsed := f.numericInput(f.arrayElement(25))
		typmod := int32((3 << 16) + 1 + 4)
		numeric := model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: typmod}
		f.coercionType(numeric)
		f.arrayLayout(1231, numeric)
		rounded := f.function(1703, 1700, true, ir.Effects{MayError: true}, parsed, f.constant(23, model.Int64Val(int64(typmod))))
		root := f.arrayCoerce(source, 1231, rounded)
		input, out := batch(1), columnar.Batch{}
		err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, f.options())).Eval(t.Context(), &input, nil, &out, mode)
		requireStackSQLState(t, err, "22003")
		if out.Rows() != 0 || len(out.Columns) != 0 {
			t.Fatal("failed conversion published partial output")
		}
	}
}

func TestArrayCoerceInvokesNonStrictTemplateForNullElements(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		template := f.function(900101, 20, false, ir.Effects{MayError: true}, f.arrayElement(20))
		roots := []ir.ScalarID{
			f.arrayCoerce(f.constant(1016, model.NullVal(model.TypeArray)), 1016, template),
			f.arrayCoerce(f.constant(1016, stackArray(nil)), 1016, template),
			f.arrayCoerce(f.constant(1016, stackArray([]model.ArrayDimension{{Length: 1, LowerBound: -2}}, model.NullVal(model.TypeInt64))), 1016, template),
		}
		calls := 0
		options := f.options()
		options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
			return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
				calls++
				return model.Int64Val(42), nil
			}, nil
		})
		input, out := batch(1), columnar.Batch{}
		if err := state(t, compile(t, f.graph(), roots, options)).Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		if got := value(t, &out, 0, 0); !got.IsNull || got.Type != model.TypeArray {
			t.Fatalf("NULL source became %+v", got)
		}
		requireStack(t, value(t, &out, 1, 0), nil, nil)
		requireStack(t, value(t, &out, 2, 0), []model.ArrayDimension{{Length: 1, LowerBound: -2}}, []model.Value{model.Int64Val(42)})
		if calls != 1 {
			t.Fatalf("element conversion calls = %d; want one NULL element and no NULL/empty array calls", calls)
		}
	}
}

func TestArrayCoerceResolvesEmptyTemplateAndRejectsEscapedElement(t *testing.T) {
	f := newFixture(t)
	f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
	element := f.arrayElement(20)
	template := f.function(900102, 20, true, ir.Effects{MayError: true}, element)
	root := f.arrayCoerce(f.constant(1016, stackArray(nil)), 1016, template)
	graph := f.graph()
	_, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root}, f.options())
	var unknown *builtin.UnknownFunctionError
	if !errors.As(err, &unknown) || unknown.ID != 900102 {
		t.Fatalf("empty array hid unresolved conversion: %v", err)
	}
	if _, err := expr.Compile(t.Context(), graph, []ir.ScalarID{element}, f.options()); !errors.Is(err, expr.ErrInvalid) {
		t.Fatalf("row evaluation accepted array element: %v", err)
	}
}

func TestArrayCoerceHonorsLazyMasks(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.arrayLayout(1009, model.Type{ID: 25, Storage: model.TypeString})
		source := f.constant(1009, stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.StringVal("bad")))
		parsed := f.numericInput(f.arrayElement(25))
		f.arrayLayout(1231, model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: -1})
		bad := f.arrayCoerce(source, 1231, parsed)
		empty := f.constant(1231, stackArray(nil))
		condition := f.call("int8eq", 16, f.x, f.constant(20, model.Int64Val(0)))
		guarded := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[1231]}, []ir.ScalarID{condition, empty, bad}, model.Value{})
		coalesce := f.add(ir.Scalar{Kind: ir.Coalesce, Layout: f.layouts[1231]}, []ir.ScalarID{empty, bad}, model.Value{})
		null := f.add(ir.Scalar{Kind: ir.NullTest, Layout: f.layouts[16]}, []ir.ScalarID{bad}, model.Value{})
		and := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.And, Layout: f.layouts[16]}, []ir.ScalarID{f.constant(16, model.BoolVal(false)), null}, model.Value{})
		or := f.add(ir.Scalar{Kind: ir.Bool, BoolOp: ir.Or, Layout: f.layouts[16]}, []ir.ScalarID{f.constant(16, model.BoolVal(true)), null}, model.Value{})
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
		requireStackSQLState(t, s.Eval(t.Context(), &input, nil, &out, mode), "22P02")
	}
}

func TestArrayCoerceReleasesPerElementScratch(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.coercionType(model.Type{ID: 17, Storage: model.TypeBytes})
		f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		elements := make([]model.Value, 32)
		for i := range elements {
			elements[i] = model.Int64Val(int64(i))
		}
		dimensions := []model.ArrayDimension{{Length: 32, LowerBound: 1}}
		source := f.constant(1016, stackArray(dimensions, elements...))
		encoded := f.function(900103, 17, true, ir.Effects{MayError: true}, f.arrayElement(20))
		decoded := f.function(900104, 20, true, ir.Effects{MayError: true}, encoded)
		root := f.arrayCoerce(source, 1016, decoded)
		options := f.options()
		options.Limits.MaxStateBytes = 16 << 10
		options.Functions = resolverFunc(func(function expr.Function) (expr.ScalarCall, error) {
			if function.ID == 900103 {
				return func(_ context.Context, args []model.Value, budget builtin.CallBudget) (model.Value, error) {
					if budget.OutputBytes < 4096 {
						return model.Value{}, expr.ErrLimit
					}
					return model.BytesVal(bytes.Repeat([]byte{byte(args[0].IntVal)}, 4096)), nil
				}, nil
			}
			return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
				return model.Int64Val(int64(args[0].BytesVal[4095])), nil
			}, nil
		})
		graph := f.graph()
		input, out := batch(1), columnar.Batch{}
		if err := state(t, compile(t, graph, []ir.ScalarID{root}, options)).Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &out, 0, 0), dimensions, elements)
		options.Limits.MaxStateBytes = 8 << 10
		err := state(t, compile(t, graph, []ir.ScalarID{root}, options)).Eval(t.Context(), &input, nil, &out, mode)
		if !errors.Is(err, expr.ErrLimit) || out.Rows() != 0 || len(out.Columns) != 0 {
			t.Fatalf("scratch overlap bypassed budget: error=%v rows=%d", err, out.Rows())
		}
	}
}

func TestArrayCoerceRelabelBorrowsUntilOutputOwnsPayload(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.coercionType(model.Type{ID: 17, Storage: model.TypeBytes})
		sourceLayout := f.arrayLayout(1001, model.Type{ID: 17, Storage: model.TypeBytes})
		source := f.add(ir.Scalar{Kind: ir.Param, Parameter: 1, Layout: f.layouts[1001], Nullable: true}, nil, model.Value{})
		element := f.arrayElement(17)
		f.coercionType(model.Type{ID: 17, Storage: model.TypeBytes, Modifier: -1})
		f.arrayLayout(1001, model.Type{ID: 17, Storage: model.TypeBytes, Modifier: -1})
		relabel := f.add(ir.Scalar{Kind: ir.Relabel, Layout: f.layouts[17], Nullable: true, CastContext: ir.ExplicitCast}, []ir.ScalarID{element}, model.Value{})
		root := f.arrayCoerce(source, 1001, relabel)
		options := f.options()
		options.Parameters = []expr.Parameter{{Number: 1, Layout: sourceLayout}}
		options.Limits.MaxStateBytes = 8 << 10
		p := compile(t, f.graph(), []ir.ScalarID{root}, options)
		s := state(t, p)
		payload := bytes.Repeat([]byte{'a'}, 64<<10)
		dimensions := []model.ArrayDimension{{Length: 1, LowerBound: -7}}
		parameters := []expr.ParameterValue{{Number: 1, Layout: sourceLayout, Value: stackArray(dimensions, model.BytesVal(payload))}}
		input, out := batch(1), columnar.Batch{}
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		payload[0] = 'b'
		parameters[0].Value.Dimensions[0].LowerBound = 9
		requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 1, LowerBound: -7}}, []model.Value{model.BytesVal(bytes.Repeat([]byte{'a'}, 64<<10))})
		retained, err := expr.Retain(t.Context(), &out, p.OutputLayouts(), expr.RetainOptions{MaxRows: 1, MaxBytes: 1 << 20})
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
		requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 1, LowerBound: 9}}, []model.Value{model.BytesVal(payload)})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		var held columnar.Batch
		if err := retained.Publish(&held); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &held, 0, 0), []model.ArrayDimension{{Length: 1, LowerBound: -7}}, []model.Value{model.BytesVal(bytes.Repeat([]byte{'a'}, 64<<10))})
	}
}

func TestArrayCoerceCancellationReleasesClaimsAndAllowsReuse(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		q := allocationQuery(t)
		f := newFixture(t)
		f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		dimensions := []model.ArrayDimension{{Length: 2, LowerBound: 1}}
		source := f.constant(1016, stackArray(dimensions, model.Int64Val(3), model.Int64Val(5)))
		template := f.function(900105, 20, true, ir.Effects{Volatility: ir.Volatile}, f.arrayElement(20))
		root := f.arrayCoerce(source, 1016, template)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		calls := 0
		options := f.options()
		options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
			return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
				calls++
				if calls == 1 {
					cancel()
				}
				return model.Int64Val(args[0].IntVal + 1), nil
			}, nil
		})
		p := compile(t, f.graph(), []ir.ScalarID{root}, options)
		s, err := expr.NewState(p, expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		input, out := batch(1), columnar.Batch{}
		if err := s.Eval(ctx, &input, nil, &out, mode); !errors.Is(err, context.Canceled) {
			t.Fatalf("mid-element cancellation = %v", err)
		}
		if calls != 1 || out.Rows() != 0 || len(out.Columns) != 0 {
			t.Fatalf("cancellation continued or published: calls=%d rows=%d", calls, out.Rows())
		}
		if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
			t.Fatalf("canceled conversion leaked claims: %+v", usage)
		}
		if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &out, 0, 0), dimensions, []model.Value{model.Int64Val(4), model.Int64Val(6)})
		if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
			t.Fatalf("successful conversion retained scratch: %+v", usage)
		}
	}
}

func TestArrayCoerceOwnsReusableCallbackResults(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f := newFixture(t)
		f.coercionType(model.Type{ID: 17, Storage: model.TypeBytes})
		f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
		f.arrayLayout(1001, model.Type{ID: 17, Storage: model.TypeBytes})
		dimensions := []model.ArrayDimension{{Length: 2, LowerBound: 1}}
		source := f.constant(1016, stackArray(dimensions, model.Int64Val(1), model.Int64Val(2)))
		template := f.function(900106, 17, true, ir.Effects{MayError: true}, f.arrayElement(20))
		root := f.arrayCoerce(source, 1001, template)
		scratch := []byte{0}
		options := f.options()
		options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
			return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
				scratch[0] = byte(args[0].IntVal)
				return model.BytesVal(scratch), nil
			}, nil
		})
		input, out := batch(1), columnar.Batch{}
		if err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options)).Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		requireStack(t, value(t, &out, 0, 0), dimensions, []model.Value{model.BytesVal([]byte{1}), model.BytesVal([]byte{2})})
	}
}

func TestArrayCoerceAdmitsTemporaryOverlapBeforeReleasingPrevious(t *testing.T) {
	q := allocationQuery(t)
	f := newFixture(t)
	f.coercionType(model.Type{ID: 17, Storage: model.TypeBytes})
	f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
	f.arrayLayout(1001, model.Type{ID: 17, Storage: model.TypeBytes})
	source := f.constant(1016, stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.Int64Val(1)))
	first := f.function(900107, 17, true, ir.Effects{MayError: true}, f.arrayElement(20))
	second := f.function(900108, 17, true, ir.Effects{MayError: true}, first)
	root := f.arrayCoerce(source, 1001, second)
	const payloadBytes = 128 << 10
	blocker, err := q.Allocator(runtime.WorkMemory).Reserve(uint64(q.Usage().Work.Limit - 3*payloadBytes))
	if err != nil {
		t.Fatal(err)
	}
	options := f.options()
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(_ context.Context, _ []model.Value, budget builtin.CallBudget) (model.Value, error) {
			if budget.OutputBytes < payloadBytes {
				return model.Value{}, expr.ErrLimit
			}
			return model.BytesVal(bytes.Repeat([]byte{'x'}, payloadBytes)), nil
		}, nil
	})
	p := compile(t, f.graph(), []ir.ScalarID{root}, options)
	s, err := expr.NewState(p, expr.StateOptions{WorkAllocator: q.Allocator(runtime.WorkMemory), OutputAllocator: q.Allocator(runtime.OutputMemory)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if blocker != nil {
			blocker.Release()
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	input, out := batch(1), columnar.Batch{}
	if err := s.Eval(t.Context(), &input, nil, &out, expr.Direct); !errors.Is(err, expr.ErrLimit) {
		t.Fatalf("temporary result overlap bypassed shared budget: %v", err)
	}
	if out.Rows() != 0 || len(out.Columns) != 0 {
		t.Fatal("shared budget failure published output")
	}
	blocker.Release()
	blocker = nil
	if usage := q.Usage().Work; usage.Live != 0 || usage.Reservations != 0 {
		t.Fatalf("failed coercion retained work: %+v", usage)
	}
	if err := s.Eval(t.Context(), &input, nil, &out, expr.ScalarCalls); err != nil {
		t.Fatal(err)
	}
	requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 1, LowerBound: 1}}, []model.Value{model.BytesVal(bytes.Repeat([]byte{'x'}, payloadBytes))})
}

func TestArrayCoerceChargesTemplateConstants(t *testing.T) {
	f := newFixture(t)
	f.coercionType(model.Type{ID: 17, Storage: model.TypeBytes})
	f.arrayLayout(1016, model.Type{ID: 20, Storage: model.TypeInt64})
	source := f.constant(1016, stackArray(nil))
	constant := f.constant(17, model.BytesVal(bytes.Repeat([]byte{'x'}, 4096)))
	template := f.function(900109, 20, true, ir.Effects{}, f.arrayElement(20), constant)
	root := f.arrayCoerce(source, 1016, template)
	graph, options := f.graph(), f.options()
	options.Functions = resolverFunc(func(expr.Function) (expr.ScalarCall, error) {
		return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
			return model.Int64Val(args[0].IntVal + int64(len(args[1].BytesVal))), nil
		}, nil
	})
	baseline := compile(t, graph, []ir.ScalarID{source}, options)
	options.Limits.MaxBytes = baseline.OwnedBytes() + 4095
	if _, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root}, options); !errors.Is(err, ir.ErrLimit) && !errors.Is(err, expr.ErrLimit) {
		t.Fatalf("template constant bypassed metadata bound: %v", err)
	}
	options.Limits.MaxBytes = expr.DefaultLimits().MaxBytes
	options.Limits.MaxConstantBytes = 4096
	if _, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root}, options); !errors.Is(err, ir.ErrLimit) && !errors.Is(err, expr.ErrLimit) {
		t.Fatalf("template constant bypassed constant bound: %v", err)
	}
}

func TestArrayCoerceCharacterTypmodUsesExplicitFlag(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, explicit := range []bool{false, true} {
			f := newFixture(t)
			integer := model.Type{ID: 23, Storage: model.TypeInt64}
			f.coercionType(integer)
			f.coercionType(model.Type{ID: 26, Storage: model.TypeInt64})
			f.coercionType(model.Type{ID: 2275, Storage: model.TypeString})
			f.coercionType(model.Type{ID: 1043, Storage: model.TypeString, Modifier: -1})
			f.arrayLayout(1007, integer)
			dimensions := []model.ArrayDimension{{Length: 1, LowerBound: 1}}
			source := f.constant(1007, stackArray(dimensions, model.Int64Val(12345)))
			text := f.call("int4out", 2275, f.arrayElement(23))
			varchar := f.call("varcharin", 1043, text, f.constant(26, model.Int64Val(0)), f.constant(23, model.Int64Val(-1)))
			resultType := model.Type{ID: 1043, Storage: model.TypeString, Modifier: 7}
			f.coercionType(resultType)
			f.arrayLayout(1015, resultType)
			truncated := f.function(669, 1043, true, ir.Effects{MayError: true}, varchar, f.constant(23, model.Int64Val(7)), f.constant(16, model.BoolVal(explicit)))
			root := f.arrayCoerce(source, 1015, truncated)
			input, out := batch(1), columnar.Batch{}
			err := state(t, compile(t, f.graph(), []ir.ScalarID{root}, f.options())).Eval(t.Context(), &input, nil, &out, mode)
			if !explicit {
				requireStackSQLState(t, err, "22001")
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			requireStack(t, value(t, &out, 0, 0), dimensions, []model.Value{model.StringVal("123")})
		}
	}
}
