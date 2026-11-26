package expr_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

var errIndexForNullSource = errors.New("index evaluated for null source")

func arrayFetchFixture(t *testing.T, arrayID model.TypeID, element model.Type) (*fixture, model.TypeLayout) {
	t.Helper()
	f := newFixture(t)
	for _, typ := range []model.Type{{ID: 23, Storage: model.TypeInt64}, element} {
		id, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
		if err != nil {
			t.Fatal(err)
		}
		f.layouts[typ.ID] = id
	}
	return f, f.arrayLayout(arrayID, element)
}

func (f *fixture) arrayIndex(result model.TypeID, source ir.ScalarID, indexes ...ir.ScalarID) ir.ScalarID {
	return f.add(ir.Scalar{Kind: ir.ArrayIndex, Layout: f.layouts[result], Nullable: true}, append([]ir.ScalarID{source}, indexes...), model.Value{})
}

func (f *fixture) arraySlice(source ir.ScalarID, subscripts ir.ArraySubscripts, bounds ...ir.ScalarID) ir.ScalarID {
	return f.add(ir.Scalar{Kind: ir.ArraySlice, Layout: f.nodes[source].Layout, Nullable: true, Subscripts: subscripts}, append([]ir.ScalarID{source}, bounds...), model.Value{})
}

func TestArrayIndexRanksAndBounds(t *testing.T) {
	one := stackArray([]model.ArrayDimension{{Length: 3, LowerBound: -2}}, model.Int64Val(11), model.NullVal(model.TypeInt64), model.Int64Val(33))
	matrix := stackArray([]model.ArrayDimension{{Length: 2, LowerBound: -1}, {Length: 3, LowerBound: 4}}, model.Int64Val(1), model.Int64Val(2), model.Int64Val(3), model.Int64Val(4), model.Int64Val(5), model.Int64Val(6))
	six := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: -5}, {Length: 1, LowerBound: 4}, {Length: 1, LowerBound: 3}, {Length: 1, LowerBound: 2}, {Length: 1, LowerBound: 0}, {Length: 2, LowerBound: 10}}, model.Int64Val(7), model.Int64Val(8))
	for _, tc := range []struct {
		name      string
		source    model.Value
		indexes   []int64
		nullIndex bool
		want      int64
		null      bool
	}{
		{name: "negative lower bound", source: one, indexes: []int64{-2}, want: 11},
		{name: "last element", source: one, indexes: []int64{0}, want: 33},
		{name: "null element", source: one, indexes: []int64{-1}, null: true},
		{name: "row major offset", source: matrix, indexes: []int64{0, 5}, want: 5},
		{name: "six dimensions", source: six, indexes: []int64{-5, 4, 3, 2, 0, 11}, want: 8},
		{name: "too few indexes", source: matrix, indexes: []int64{-1}, null: true},
		{name: "too many indexes", source: one, indexes: []int64{-2, 1}, null: true},
		{name: "below bounds", source: one, indexes: []int64{math.MinInt32}, null: true},
		{name: "above bounds", source: one, indexes: []int64{math.MaxInt32}, null: true},
		{name: "empty", source: stackArray(nil), indexes: []int64{1}, null: true},
		{name: "null source", source: model.NullVal(model.TypeArray), indexes: []int64{1}, null: true},
		{name: "null index", source: one, indexes: []int64{0}, nullIndex: true, null: true},
	} {
		for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
			t.Run(fmt.Sprintf("%s/mode%d", tc.name, mode), func(t *testing.T) {
				f, layout := arrayFetchFixture(t, 1016, model.Type{ID: 20, Storage: model.TypeInt64})
				source := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1, Nullable: true}, nil, model.Value{})
				indexes := make([]ir.ScalarID, len(tc.indexes))
				for i, n := range tc.indexes {
					v := model.Int64Val(n)
					if tc.nullIndex {
						v = model.NullVal(model.TypeInt64)
					}
					indexes[i] = f.constant(23, v)
				}
				root := f.arrayIndex(20, source, indexes...)
				options := f.options()
				options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
				s := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options))
				input, out := batch(1), columnar.Batch{}
				if err := s.Eval(t.Context(), &input, []expr.ParameterValue{{Number: 1, Layout: layout, Value: tc.source}}, &out, mode); err != nil {
					t.Fatal(err)
				}
				got := value(t, &out, 0, 0)
				if got.Type != model.TypeInt64 {
					t.Fatalf("result storage = %v", got.Type)
				}
				requireInt(t, got, tc.want, tc.null)
			})
		}
	}
}

func TestArraySliceRectangles(t *testing.T) {
	matrix := stackArray([]model.ArrayDimension{{Length: 3, LowerBound: -1}, {Length: 4, LowerBound: 5}}, model.Int64Val(1), model.Int64Val(2), model.Int64Val(3), model.Int64Val(4), model.Int64Val(5), model.Int64Val(6), model.Int64Val(7), model.Int64Val(8), model.Int64Val(9), model.Int64Val(10), model.Int64Val(11), model.Int64Val(12))
	six := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: -5}, {Length: 1, LowerBound: 4}, {Length: 1, LowerBound: 3}, {Length: 1, LowerBound: 2}, {Length: 1, LowerBound: 0}, {Length: 2, LowerBound: 10}}, model.Int64Val(7), model.Int64Val(8))
	cube := stackArray([]model.ArrayDimension{{Length: 2, LowerBound: -1}, {Length: 3, LowerBound: 5}, {Length: 4, LowerBound: 9}})
	for n := int64(1); n <= 24; n++ {
		cube.ArrayVal = append(cube.ArrayVal, model.Int64Val(n))
	}
	for _, tc := range []struct {
		name       string
		source     model.Value
		subscripts ir.ArraySubscripts
		bounds     []model.Value
		lengths    []int32
		want       []int64
		null       bool
	}{
		{name: "rectangle with inner gaps", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 2, Upper: 3, Lower: 3}, bounds: []model.Value{model.Int64Val(1), model.Int64Val(7), model.Int64Val(0), model.Int64Val(6)}, lengths: []int32{2, 2}, want: []int64{6, 7, 10, 11}},
		{name: "outer dimension rollover", source: cube, subscripts: ir.ArraySubscripts{Dimensions: 3, Upper: 7, Lower: 7}, bounds: []model.Value{model.Int64Val(0), model.Int64Val(6), model.Int64Val(11), model.Int64Val(-1), model.Int64Val(5), model.Int64Val(10)}, lengths: []int32{2, 2, 2}, want: []int64{2, 3, 6, 7, 14, 15, 18, 19}},
		{name: "missing trailing dimension", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 1, Upper: 1, Lower: 1}, bounds: []model.Value{model.Int64Val(0), model.Int64Val(0)}, lengths: []int32{1, 4}, want: []int64{5, 6, 7, 8}},
		{name: "omitted bounds", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 2}, lengths: []int32{3, 4}, want: []int64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}},
		{name: "clamp bounds", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 2, Upper: 3, Lower: 3}, bounds: []model.Value{model.Int64Val(math.MaxInt32), model.Int64Val(6), model.Int64Val(math.MinInt32), model.Int64Val(math.MinInt32)}, lengths: []int32{3, 2}, want: []int64{1, 2, 5, 6, 9, 10}},
		{name: "independent masks", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 2, Upper: 1, Lower: 2}, bounds: []model.Value{model.Int64Val(0), model.Int64Val(7)}, lengths: []int32{2, 2}, want: []int64{3, 4, 7, 8}},
		{name: "inverted bounds", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 1, Upper: 1, Lower: 1}, bounds: []model.Value{model.Int64Val(-1), model.Int64Val(1)}},
		{name: "no overlap", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 1, Lower: 1}, bounds: []model.Value{model.Int64Val(2)}},
		{name: "too many dimensions", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 3}},
		{name: "empty", source: stackArray(nil), subscripts: ir.ArraySubscripts{Dimensions: 1}},
		{name: "null source", source: model.NullVal(model.TypeArray), subscripts: ir.ArraySubscripts{Dimensions: 1}, null: true},
		{name: "null bound on empty", source: stackArray(nil), subscripts: ir.ArraySubscripts{Dimensions: 1, Upper: 1}, bounds: []model.Value{model.NullVal(model.TypeInt64)}, null: true},
		{name: "null bound before rank mismatch", source: matrix, subscripts: ir.ArraySubscripts{Dimensions: 3, Lower: 4}, bounds: []model.Value{model.NullVal(model.TypeInt64)}, null: true},
		{name: "six dimensions", source: six, subscripts: ir.ArraySubscripts{Dimensions: 6, Lower: 32}, bounds: []model.Value{model.Int64Val(11)}, lengths: []int32{1, 1, 1, 1, 1, 1}, want: []int64{8}},
	} {
		for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
			t.Run(fmt.Sprintf("%s/mode%d", tc.name, mode), func(t *testing.T) {
				f, layout := arrayFetchFixture(t, 1016, model.Type{ID: 20, Storage: model.TypeInt64})
				source := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1, Nullable: true}, nil, model.Value{})
				bounds := make([]ir.ScalarID, len(tc.bounds))
				for i, bound := range tc.bounds {
					bounds[i] = f.constant(23, bound)
				}
				root := f.arraySlice(source, tc.subscripts, bounds...)
				options := f.options()
				options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
				s := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options))
				input, out := batch(1), columnar.Batch{}
				if err := s.Eval(t.Context(), &input, []expr.ParameterValue{{Number: 1, Layout: layout, Value: tc.source}}, &out, mode); err != nil {
					t.Fatal(err)
				}
				got := value(t, &out, 0, 0)
				if tc.null {
					if !got.IsNull || got.Type != model.TypeArray {
						t.Fatalf("result = %+v, want typed NULL", got)
					}
					return
				}
				dimensions := make([]model.ArrayDimension, len(tc.lengths))
				for i, length := range tc.lengths {
					dimensions[i] = model.ArrayDimension{Length: length, LowerBound: 1}
				}
				want := make([]model.Value, len(tc.want))
				for i, n := range tc.want {
					want[i] = model.Int64Val(n)
				}
				requireStack(t, got, dimensions, want)
			})
		}
	}
}

func TestArrayFetchSparseVariableWidthLanes(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f, layout := arrayFetchFixture(t, 1009, model.Type{ID: 25, Storage: model.TypeString, Modifier: -1, Collation: 100})
		source := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1009], Parameter: 1}, nil, model.Value{})
		slot, err := f.b.AddSlot(t.Context(), ir.SlotInfo{Scope: f.scope, Layout: f.layouts[23], Nullable: true, Object: 1, Column: 2})
		if err != nil {
			t.Fatal(err)
		}
		index := f.add(ir.Scalar{Kind: ir.Slot, Layout: f.layouts[23], Slot: slot, Nullable: true}, nil, model.Value{})
		roots := []ir.ScalarID{f.arrayIndex(25, source, index), f.arraySlice(source, ir.ArraySubscripts{Dimensions: 1, Upper: 1}, index)}
		options := f.options()
		options.Inputs = append(options.Inputs, expr.Input{Slot: slot, Column: 1})
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
		program := compile(t, f.graph(), roots, options)
		s := state(t, program)
		input, out := batch(6), columnar.Batch{}
		input.Columns = append(input.Columns, columnar.Vector{Type: model.Type{ID: 23, Storage: model.TypeInt64}, Length: 6, Int64: []int64{-2, 99, 0, -1, 1, 0}, Nulls: []byte{1 << 5}})
		input.Selection = []uint32{4, 0, 3, 2, 5}
		elements := []model.Value{model.StringVal("a"), model.NullVal(model.TypeString), model.StringVal("ccc"), model.StringVal("dddd")}
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: stackArray([]model.ArrayDimension{{Length: 4, LowerBound: -2}}, elements...)}}
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		wantIndexes := []model.Value{elements[3], elements[0], elements[1], elements[2], model.NullVal(model.TypeString)}
		for row, want := range wantIndexes {
			got := value(t, &out, 0, row)
			if got.IsNull != want.IsNull || got.Type != want.Type || got.StrVal != want.StrVal {
				t.Fatalf("row %d index = %+v, want %+v", row, got, want)
			}
		}
		for row, count := range []int{4, 1, 2, 3} {
			requireStack(t, value(t, &out, 1, row), []model.ArrayDimension{{Length: int32(count), LowerBound: 1}}, elements[:count])
		}
		if got := value(t, &out, 1, 4); !got.IsNull {
			t.Fatalf("NULL index slice = %+v", got)
		}
		if typ, ok := program.OutputType(0); !ok || typ != layout.Children[0].Type {
			t.Fatalf("indexed SQL identity = %+v, want %+v", typ, layout.Children[0].Type)
		}
	}
}

func TestArrayFetchIntrinsicallySkipsBoundsOnNullSource(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, slice := range []bool{false, true} {
			f, _ := arrayFetchFixture(t, 1016, model.Type{ID: 20, Storage: model.TypeInt64})
			source := f.constant(1016, model.NullVal(model.TypeArray))
			bad := f.call("int4div", 23, f.constant(23, model.Int64Val(1)), f.constant(23, model.Int64Val(0)))
			root := f.arrayIndex(20, source, bad)
			if slice {
				root = f.arraySlice(source, ir.ArraySubscripts{Dimensions: 1, Upper: 1}, bad)
			}
			s := state(t, compile(t, f.graph(), []ir.ScalarID{root}, f.options()))
			input, out := batch(1), columnar.Batch{}
			if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatal(err)
			}
			if got := value(t, &out, 0, 0); !got.IsNull {
				t.Fatalf("NULL source fetch = %+v", got)
			}
		}
	}
}

func TestArraySliceEvaluatesAllUpperThenLowerBounds(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		for _, sourceNull := range []bool{false, true} {
			f, _ := arrayFetchFixture(t, 1016, model.Type{ID: 20, Storage: model.TypeInt64})
			sourceValue := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}}, model.Int64Val(9))
			if sourceNull {
				sourceValue = model.NullVal(model.TypeArray)
			}
			source := f.constant(1016, sourceValue)
			bounds := make([]ir.ScalarID, 4)
			for i := range bounds {
				bounds[i] = f.function(model.FunctionID(900000+i), 23, false, ir.Effects{Volatility: ir.Volatile})
			}
			root := f.arraySlice(source, ir.ArraySubscripts{Dimensions: 2, Upper: 3, Lower: 3}, bounds...)
			var order []model.FunctionID
			options := f.options()
			options.Functions = resolverFunc(func(fn expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					order = append(order, fn.ID)
					if fn.ID == 900000 {
						return model.NullVal(model.TypeInt64), nil
					}
					return model.Int64Val(1), nil
				}, nil
			})
			s := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options))
			input, out := batch(1), columnar.Batch{}
			if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatal(err)
			}
			wantOrder := []model.FunctionID{900000, 900001, 900002, 900003}
			if sourceNull {
				wantOrder = nil
			}
			if !slices.Equal(order, wantOrder) {
				t.Fatalf("bound evaluation order = %v", order)
			}
			if got := value(t, &out, 0, 0); !got.IsNull || got.Type != model.TypeArray {
				t.Fatalf("NULL-bound fetch = %+v", got)
			}
		}
	}
}

func TestArrayFetchCaseMasksSkipIndexesOnNullSource(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f, _ := arrayFetchFixture(t, 1016, model.Type{ID: 20, Storage: model.TypeInt64})
		source := f.function(900000, 1016, false, ir.Effects{Volatility: ir.Volatile}, f.x)
		source = f.add(ir.Scalar{Kind: ir.Once, Layout: f.layouts[1016], Nullable: true}, []ir.ScalarID{source}, model.Value{})
		isNull := f.add(ir.Scalar{Kind: ir.NullTest, Layout: f.layouts[16]}, []ir.ScalarID{source}, model.Value{})
		index := f.function(900001, 23, false, ir.Effects{Volatility: ir.Volatile, MayError: true}, f.x)
		fetch := f.arrayIndex(20, source, index)
		root := f.add(ir.Scalar{Kind: ir.Case, Layout: f.layouts[20], Nullable: true}, []ir.ScalarID{isNull, f.constant(20, model.NullVal(model.TypeInt64)), fetch}, model.Value{})
		calls := 0
		options := f.options()
		options.Functions = resolverFunc(func(fn expr.Function) (expr.ScalarCall, error) {
			return func(_ context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
				if fn.ID == 900000 {
					calls++
					if args[0].IsNull || args[0].IntVal == 0 {
						return model.NullVal(model.TypeArray), nil
					}
					return stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.Int64Val(42)), nil
				}
				if args[0].IsNull || args[0].IntVal == 0 {
					return model.Value{}, errIndexForNullSource
				}
				return model.Int64Val(1), nil
			}, nil
		})
		s := state(t, compile(t, f.graph(), []ir.ScalarID{root}, options))
		input, out := batch(6), columnar.Batch{}
		input.Selection = []uint32{5, 4, 0, 1}
		if err := s.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		if calls != 4 {
			t.Fatalf("source evaluated %d times for four selected rows", calls)
		}
		for row, null := range []bool{true, false, true, false} {
			requireInt(t, value(t, &out, 0, row), 42, null)
		}
	}
}

func TestArraySliceAdmitsOnlyRetainedHeaders(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f, layout := arrayFetchFixture(t, 1001, model.Type{ID: 17, Storage: model.TypeBytes})
		source := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1001], Parameter: 1}, nil, model.Value{})
		bound := f.constant(23, model.Int64Val(1024))
		root := f.arraySlice(source, ir.ArraySubscripts{Dimensions: 1, Upper: 1, Lower: 1}, bound, bound)
		graph, options := f.graph(), f.options()
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
		options.Limits.MaxOutputBytes = 128 << 10
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
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: stackArray(nil)}}
		if err := probe.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		base := uint64(q.Usage().Work.Peak)
		if err := probe.Close(); err != nil {
			t.Fatal(err)
		}
		discarded := bytes.Repeat([]byte{'d'}, 128<<10)
		retained := bytes.Repeat([]byte{'r'}, 64<<10)
		elements := make([]model.Value, 2048)
		for i := range elements {
			elements[i] = model.BytesVal(discarded)
		}
		elements[1023] = model.BytesVal(retained)
		array := stackArray([]model.ArrayDimension{{Length: 2048, LowerBound: 1}}, elements...)
		cost := uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(model.ArrayDimension{}))
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
			parameters[0].Value = array
			err = s.Eval(t.Context(), &input, parameters, &out, mode)
			if short == 0 {
				if err != nil {
					t.Fatalf("exact retained header budget: %v", err)
				}
				requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 1, LowerBound: 1}}, []model.Value{model.BytesVal(retained)})
				elements[1023] = model.BytesVal(discarded)
				if err := s.Eval(t.Context(), &input, parameters, &out, mode); !errors.Is(err, columnar.ErrBatchBudget) {
					t.Fatalf("retained payload output budget: %v", err)
				}
				elements[1023] = model.BytesVal(retained)
			} else if !errors.Is(err, expr.ErrLimit) {
				t.Fatalf("one-byte-short retained header budget: %v", err)
			}
			if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
				t.Fatalf("failed slice retained claims: %+v", usage)
			}
			parameters[0].Value = stackArray(nil)
			if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
				t.Fatalf("reuse after budget failure: %v", err)
			}
			requireStack(t, value(t, &out, 0, 0), nil, nil)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestArrayFetchOwnsPublishedPayloads(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f, layout := arrayFetchFixture(t, 1001, model.Type{ID: 17, Storage: model.TypeBytes})
		source := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1001], Parameter: 1}, nil, model.Value{})
		bound := f.constant(23, model.Int64Val(4))
		roots := []ir.ScalarID{f.arrayIndex(17, source, bound), f.arraySlice(source, ir.ArraySubscripts{Dimensions: 1, Upper: 1, Lower: 1}, bound, bound)}
		options := f.options()
		options.Limits.MaxStateBytes = 8 << 10
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
		program := compile(t, f.graph(), roots, options)
		s := state(t, program)
		payload := bytes.Repeat([]byte{'x'}, 64<<10)
		array := stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 4}}, model.BytesVal(payload))
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: array}}
		input, out := batch(1), columnar.Batch{}
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatal(err)
		}
		clear(payload)
		array.Dimensions[0].LowerBound = 1
		array.ArrayVal[0] = model.NullVal(model.TypeBytes)
		want := model.BytesVal(bytes.Repeat([]byte{'x'}, 64<<10))
		if got := value(t, &out, 0, 0); got.IsNull || !bytes.Equal(got.BytesVal, want.BytesVal) {
			t.Fatal("indexed payload aliases caller memory")
		}
		requireStack(t, value(t, &out, 1, 0), []model.ArrayDimension{{Length: 1, LowerBound: 1}}, []model.Value{want})
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
		if got := value(t, &out, 0, 0); !got.IsNull {
			t.Fatalf("reused indexed result = %+v, want NULL", got)
		}
		requireStack(t, value(t, &out, 1, 0), nil, nil)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		var held columnar.Batch
		if err := retained.Publish(&held); err != nil {
			t.Fatal(err)
		}
		if got := value(t, &held, 0, 0); got.IsNull || !bytes.Equal(got.BytesVal, want.BytesVal) {
			t.Fatal("retained indexed payload changed after Eval and Close")
		}
		requireStack(t, value(t, &held, 1, 0), []model.ArrayDimension{{Length: 1, LowerBound: 1}}, []model.Value{want})
	}
}

func TestArraySliceCancellationReleasesCopyWork(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f, layout := arrayFetchFixture(t, 1016, model.Type{ID: 20, Storage: model.TypeInt64})
		source := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1}, nil, model.Value{})
		root := f.arraySlice(source, ir.ArraySubscripts{Dimensions: 1})
		options := f.options()
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		interrupt := &arrayStackCancelContext{Context: ctx, cancel: cancel}
		q := allocationQuery(t)
		cost := 1024*uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(model.ArrayDimension{}))
		stateOptions := expr.StateOptions{WorkAllocator: arrayStackCancelAllocator{Allocator: q.Allocator(runtime.WorkMemory), ctx: interrupt, bytes: cost}, OutputAllocator: q.Allocator(runtime.OutputMemory)}
		s, err := expr.NewState(compile(t, f.graph(), []ir.ScalarID{root}, options), stateOptions)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		elements := make([]model.Value, 1024)
		for i := range elements {
			elements[i] = model.Int64Val(int64(i))
		}
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout, Value: stackArray([]model.ArrayDimension{{Length: 1024, LowerBound: -20}}, elements...)}}
		input, out := batch(1), columnar.Batch{}
		if err := s.Eval(interrupt, &input, parameters, &out, mode); !errors.Is(err, context.Canceled) {
			t.Fatalf("slice copy cancellation = %v", err)
		}
		if out.Rows() != 0 || len(out.Columns) != 0 {
			t.Fatal("canceled slice published output")
		}
		if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
			t.Fatalf("canceled slice retained claims: %+v", usage)
		}
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatalf("reuse after slice cancellation: %v", err)
		}
		requireStack(t, value(t, &out, 0, 0), []model.ArrayDimension{{Length: 1024, LowerBound: 1}}, elements)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if usage := q.Usage(); usage.Work.Live != 0 || usage.Work.Reservations != 0 || usage.Output.Live != 0 || usage.Output.Reservations != 0 {
			t.Fatalf("close retained slice claims: %+v", usage)
		}
	}
}

func TestArrayFetchRejectsMalformedSourceShape(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		f, layout := arrayFetchFixture(t, 1016, model.Type{ID: 20, Storage: model.TypeInt64})
		source := f.add(ir.Scalar{Kind: ir.Param, Layout: f.layouts[1016], Parameter: 1}, nil, model.Value{})
		roots := []ir.ScalarID{f.arrayIndex(20, source, f.constant(23, model.Int64Val(1))), f.arraySlice(source, ir.ArraySubscripts{Dimensions: 1})}
		options := f.options()
		options.Parameters = []expr.Parameter{{Number: 1, Layout: layout}}
		s := state(t, compile(t, f.graph(), roots, options))
		input, out := batch(1), columnar.Batch{}
		parameters := []expr.ParameterValue{{Number: 1, Layout: layout}}
		for _, dimensions := range [][]model.ArrayDimension{
			nil,
			{{Length: 0, LowerBound: 1}},
			{{Length: 2, LowerBound: 1}},
			{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}},
		} {
			parameters[0].Value = stackArray(dimensions, model.Int64Val(9))
			if err := s.Eval(t.Context(), &input, parameters, &out, mode); !errors.Is(err, model.ErrInvalidArrayShape) {
				t.Fatalf("source dimensions %v: %v", dimensions, err)
			}
		}
		parameters[0].Value = stackArray([]model.ArrayDimension{{Length: 1, LowerBound: 1}}, model.Int64Val(9))
		if err := s.Eval(t.Context(), &input, parameters, &out, mode); err != nil {
			t.Fatalf("reuse after malformed shape: %v", err)
		}
		requireInt(t, value(t, &out, 0, 0), 9, false)
		requireStack(t, value(t, &out, 1, 0), []model.ArrayDimension{{Length: 1, LowerBound: 1}}, []model.Value{model.Int64Val(9)})
	}
}
