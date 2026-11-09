package builtin_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func nullCountLayout(id model.TypeID, storage model.DataType) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: storage}}
}

func resolveNullCounter(t *testing.T, id model.FunctionID, arguments []model.TypeLayout, array bool, budget uint64) builtin.Callable {
	t.Helper()
	resolve := builtin.ResolveTyped
	if array {
		resolve = builtin.ResolveVariadic
	}
	function, err := resolve(t.Context(), id, arguments, nullCountLayout(23, model.TypeInt64), builtin.ResolveOptions{}, budget)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireNullCount(t *testing.T, function builtin.Callable, values []model.Value, want int64, null bool) {
	t.Helper()
	got, err := function.Call(t.Context(), values, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	wrongCount := !null && got.IntVal != want
	if err != nil || got.Type != model.TypeInt64 || got.IsNull != null || wrongCount {
		t.Fatalf("%s = %v, %v; want count %d, NULL %t", function.Signature().Name, got, err, want, null)
	}
}

func TestNullCountUsesActualArgumentWidth(t *testing.T) {
	integer := nullCountLayout(23, model.TypeInt64)
	for _, tc := range []struct {
		name  string
		width int
	}{
		{"past inline width", 9},
		{"maximum passed arguments", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arguments := make([]model.TypeLayout, tc.width)
			types := make([]model.Type, tc.width)
			values := make([]model.Value, tc.width)
			for i := range arguments {
				arguments[i], types[i], values[i] = integer, integer.Type, model.Int64Val(int64(i))
			}
			values[0], values[8], values[tc.width-2] = model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)
			for _, id := range []model.FunctionID{438, 440} {
				function := resolveNullCounter(t, id, arguments, false, 1<<20)
				if function.ArgumentCount() != tc.width {
					t.Fatalf("argument count = %d, want %d", function.ArgumentCount(), tc.width)
				}
				if err := function.ValidateTypes(types, integer.Type); err != nil {
					t.Fatal(err)
				}
				if function.VectorKernel() != nil {
					t.Fatal("NULL counter exposed a primitive vector kernel")
				}
				want := int64(3)
				if id == 440 {
					want = int64(tc.width - 3)
				}
				requireNullCount(t, function, values, want, false)
				types[tc.width-1].Modifier = 1
				requireSQLState(t, function.ValidateTypes(types, integer.Type), "42804")
				types[tc.width-1] = integer.Type
				_, err := function.Call(t.Context(), values[:tc.width-1], builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "42883")
				last := values[tc.width-1]
				values[tc.width-1] = model.NullVal(model.TypeString)
				_, err = function.Call(t.Context(), values, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "42804")
				values[tc.width-1] = last
			}
		})
	}
}

func TestNullCountArrayModeCountsElements(t *testing.T) {
	integer := nullCountLayout(23, model.TypeInt64)
	array := nullCountLayout(1007, model.TypeArray)
	array.Children = []model.TypeLayout{integer}
	values := make([]model.Value, 120)
	for i := range values {
		values[i] = model.Int64Val(int64(i))
	}
	for _, i := range []int{0, 39, 40, 119} {
		values[i] = model.NullVal(model.TypeInt64)
	}
	input := model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: -4}, {Length: 40, LowerBound: 7}}}
	for _, id := range []model.FunctionID{438, 440} {
		expanded := resolveNullCounter(t, id, []model.TypeLayout{array}, true, 1<<20)
		scalar := resolveNullCounter(t, id, []model.TypeLayout{array}, false, 1<<20)
		wantElements, wantDatum, wantNullDatum := int64(4), int64(0), int64(1)
		if id == 440 {
			wantElements, wantDatum, wantNullDatum = 116, 1, 0
		}
		requireNullCount(t, expanded, []model.Value{input}, wantElements, false)
		requireNullCount(t, scalar, []model.Value{input}, wantDatum, false)
		requireNullCount(t, expanded, []model.Value{model.NullVal(model.TypeArray)}, 0, true)
		requireNullCount(t, scalar, []model.Value{model.NullVal(model.TypeArray)}, wantNullDatum, false)
		requireNullCount(t, expanded, []model.Value{{Type: model.TypeArray}}, 0, false)
		if expanded.ArgumentCount() != 1 {
			t.Fatalf("explicit-array argument count = %d, want 1", expanded.ArgumentCount())
		}
	}
	if input.Dimensions[0].LowerBound != -4 || input.ArrayVal[41].IntVal != 41 || !input.ArrayVal[40].IsNull {
		t.Fatal("NULL counting mutated its borrowed array")
	}
}

func TestNullCountDistinguishesNullRecordFromNullFields(t *testing.T) {
	record := nullCountLayout(2249, model.TypeComposite)
	record.Children = []model.TypeLayout{nullCountLayout(23, model.TypeInt64), nullCountLayout(25, model.TypeString)}
	allNull := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.NullVal(model.TypeInt64), model.NullVal(model.TypeString)}}
	array := nullCountLayout(2287, model.TypeArray)
	array.Children = []model.TypeLayout{record}
	values := []model.Value{model.NullVal(model.TypeComposite), allNull, allNull}
	for _, id := range []model.FunctionID{438, 440} {
		want := int64(1)
		if id == 440 {
			want = 2
		}
		ordinary := resolveNullCounter(t, id, []model.TypeLayout{record, record, record}, false, 1<<20)
		requireNullCount(t, ordinary, values, want, false)
		expanded := resolveNullCounter(t, id, []model.TypeLayout{array}, true, 1<<20)
		requireNullCount(t, expanded, []model.Value{{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: 0}}}}, want, false)
	}
}

func TestNullCountNeedsNoOutputCodecOrPayloadInspection(t *testing.T) {
	json := nullCountLayout(114, model.TypeJSONB)
	opaque := nullCountLayout(900001, model.TypeString)
	integer := nullCountLayout(21, model.TypeInt64)
	arguments := []model.TypeLayout{json, opaque, integer, opaque}
	values := []model.Value{{Type: model.TypeJSONB, BytesVal: []byte{255}}, model.StringVal("\xff\x00"), model.Int64Val(math.MaxInt64), model.NullVal(model.TypeString)}
	array := nullCountLayout(199, model.TypeArray)
	array.Children = []model.TypeLayout{json}
	for _, id := range []model.FunctionID{438, 440} {
		function := resolveNullCounter(t, id, arguments, false, 1<<20)
		want := int64(1)
		if id == 440 {
			want = 3
		}
		requireNullCount(t, function, values, want, false)
		expanded := resolveNullCounter(t, id, []model.TypeLayout{array}, true, 1<<20)
		requireNullCount(t, expanded, []model.Value{{Type: model.TypeArray, ArrayVal: []model.Value{values[0], model.NullVal(model.TypeJSONB)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}}}, 1, false)
	}
}

func TestNullCountResolutionRejectsInvalidDeclarations(t *testing.T) {
	integer := nullCountLayout(23, model.TypeInt64)
	text := nullCountLayout(25, model.TypeString)
	array := nullCountLayout(1007, model.TypeArray)
	array.Children = []model.TypeLayout{integer}
	wrongElement := array
	wrongElement.Children = []model.TypeLayout{text}
	wrongNested := nullCountLayout(2249, model.TypeComposite)
	wrongNested.Children = []model.TypeLayout{nullCountLayout(23, model.TypeString)}
	unknownNested := nullCountLayout(2249, model.TypeComposite)
	unknownNested.Children = []model.TypeLayout{nullCountLayout(2276, model.TypeString)}
	for _, tc := range []struct {
		name      string
		arguments []model.TypeLayout
		result    model.TypeLayout
		array     bool
		code      string
	}{
		{"zero arguments", nil, integer, false, "42883"},
		{"too many arguments", make([]model.TypeLayout, 101), integer, false, "42883"},
		{"multiple explicit arguments", []model.TypeLayout{array, array}, integer, true, "42883"},
		{"missing identity", []model.TypeLayout{{}}, integer, false, "42804"},
		{"unresolved any", []model.TypeLayout{nullCountLayout(2276, model.TypeString)}, integer, false, "42P18"},
		{"unresolved polymorphic type", []model.TypeLayout{nullCountLayout(2283, model.TypeString)}, integer, false, "42804"},
		{"unresolved nested any", []model.TypeLayout{unknownNested}, integer, false, "42P18"},
		{"mismatched storage", []model.TypeLayout{nullCountLayout(23, model.TypeString)}, integer, false, "42804"},
		{"mismatched nested storage", []model.TypeLayout{wrongNested}, integer, false, "42804"},
		{"missing array element", []model.TypeLayout{nullCountLayout(1007, model.TypeArray)}, integer, true, "42804"},
		{"wrong array element", []model.TypeLayout{wrongElement}, integer, true, "42804"},
		{"scalar explicit argument", []model.TypeLayout{integer}, integer, true, "42804"},
		{"non-int4 result", []model.TypeLayout{integer}, nullCountLayout(20, model.TypeInt64), false, "42804"},
		{"wrong result storage", []model.TypeLayout{integer}, nullCountLayout(23, model.TypeString), false, "42804"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve := builtin.ResolveTyped
			if tc.array {
				resolve = builtin.ResolveVariadic
			}
			for _, id := range []model.FunctionID{438, 440} {
				_, err := resolve(t.Context(), id, tc.arguments, tc.result, builtin.ResolveOptions{}, 1<<20)
				requireSQLState(t, err, tc.code)
			}
		})
	}
	for _, id := range []model.FunctionID{438, 440} {
		_, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if !errors.Is(err, builtin.ErrTypeBindingRequired) {
			t.Fatalf("raw Resolve(%d) = %v, want concrete binding requirement", id, err)
		}
		_, err = builtin.ResolveTyped(t.Context(), id, make([]model.TypeLayout, 101), integer, builtin.ResolveOptions{}, 0)
		requireSQLState(t, err, "42883")
	}
}

func TestNullCountRejectsMalformedArrays(t *testing.T) {
	array := nullCountLayout(1007, model.TypeArray)
	array.Children = []model.TypeLayout{nullCountLayout(23, model.TypeInt64)}
	for _, id := range []model.FunctionID{438, 440} {
		function := resolveNullCounter(t, id, []model.TypeLayout{array}, true, 1<<20)
		for _, tc := range []struct {
			name       string
			values     []model.Value
			dimensions []model.ArrayDimension
			code       string
		}{
			{"missing dimensions", []model.Value{model.Int64Val(1)}, nil, "2202E"},
			{"count mismatch", []model.Value{model.Int64Val(1)}, []model.ArrayDimension{{Length: 2}}, "2202E"},
			{"subscript overflow", []model.Value{model.Int64Val(1), model.Int64Val(2)}, []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32}}, "2202E"},
			{"product overflow", nil, []model.ArrayDimension{{Length: math.MaxInt32}, {Length: math.MaxInt32}}, "2202E"},
			{"wrong element storage", []model.Value{model.StringVal("1")}, []model.ArrayDimension{{Length: 1}}, "42804"},
			{"wrong null storage", []model.Value{model.NullVal(model.TypeString)}, []model.ArrayDimension{{Length: 1}}, "42804"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := function.Call(t.Context(), []model.Value{{Type: model.TypeArray, ArrayVal: tc.values, Dimensions: tc.dimensions}}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, tc.code)
			})
		}
	}
}

func TestNullCountOwnsOnlyRequiredMetadata(t *testing.T) {
	for _, id := range []model.FunctionID{438, 440} {
		for _, explicit := range []bool{false, true} {
			record := nullCountLayout(2249, model.TypeComposite)
			record.Children = make([]model.TypeLayout, 20)
			fields := make([]model.Value, len(record.Children))
			for i := range record.Children {
				record.Children[i] = nullCountLayout(114, model.TypeJSONB)
				fields[i] = model.NullVal(model.TypeJSONB)
			}
			argument := record
			value := model.Value{Type: model.TypeComposite, ArrayVal: fields}
			if explicit {
				argument = nullCountLayout(2287, model.TypeArray)
				argument.Children = []model.TypeLayout{record}
				value = model.Value{Type: model.TypeArray, ArrayVal: []model.Value{value}, Dimensions: []model.ArrayDimension{{Length: 1}}}
			}
			arguments := []model.TypeLayout{argument}
			function := resolveNullCounter(t, id, arguments, explicit, 1<<20)
			resolve := builtin.ResolveTyped
			if explicit {
				resolve = builtin.ResolveVariadic
			}
			_, err := resolve(t.Context(), id, arguments, nullCountLayout(23, model.TypeInt64), builtin.ResolveOptions{}, function.OwnedBytes()-1)
			requireSQLState(t, err, "54000")
			bounded := resolveNullCounter(t, id, arguments, explicit, function.OwnedBytes())
			types := []model.Type{argument.Type}
			arguments[0].Type.ID = 1009
			record.Children[0] = nullCountLayout(23, model.TypeString)
			if err := bounded.ValidateTypes(types, nullCountLayout(23, model.TypeInt64).Type); err != nil {
				t.Fatal(err)
			}
			want := int64(0)
			if id == 440 {
				want = 1
			}
			requireNullCount(t, bounded, []model.Value{value}, want, false)
			ctx := t.Context()
			inputs := []model.Value{value}
			allocations := testing.AllocsPerRun(20, func() {
				got, err := bounded.Call(ctx, inputs, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || got.IsNull || got.IntVal != want {
					t.Fatalf("allocation probe count = %v, %v", got, err)
				}
			})
			if allocations != 0 {
				t.Fatalf("NULL counter allocated %g times per call", allocations)
			}
		}
	}
}

type nullCountCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *nullCountCancelContext) Err() error {
	c.remaining--
	if c.remaining == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestNullCountCancelsBeforeReachingInvalidElement(t *testing.T) {
	integer := nullCountLayout(23, model.TypeInt64)
	array := nullCountLayout(1007, model.TypeArray)
	array.Children = []model.TypeLayout{integer}
	values := make([]model.Value, 16384)
	for i := range values {
		values[i] = model.NullVal(model.TypeInt64)
	}
	values[len(values)-1] = model.StringVal("invalid storage after canceled scan")
	inputs := []model.Value{{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{Length: int32(len(values))}}}}
	for _, id := range []model.FunctionID{438, 440} {
		function := resolveNullCounter(t, id, []model.TypeLayout{array}, true, 1<<20)
		ctx, cancel := context.WithCancel(t.Context())
		scanning := &nullCountCancelContext{Context: ctx, cancel: cancel, remaining: 3}
		_, err := function.Call(scanning, inputs, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled array scan = %v", err)
		}
		_, err = builtin.ResolveTyped(ctx, id, []model.TypeLayout{integer}, integer, builtin.ResolveOptions{}, 1<<20)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled ordinary resolution = %v", err)
		}
		_, err = builtin.ResolveVariadic(ctx, id, []model.TypeLayout{array}, integer, builtin.ResolveOptions{}, 1<<20)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled array resolution = %v", err)
		}
	}
}
