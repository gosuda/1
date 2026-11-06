package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func arrayOutputCallable(t *testing.T, elementID model.TypeID, storage model.DataType, options builtin.ResolveOptions) builtin.Callable {
	t.Helper()
	info, ok := builtin.LookupType(elementID)
	if !ok {
		t.Fatalf("missing element type %d", elementID)
	}
	layout := model.TypeLayout{
		Type:     model.Type{ID: info.Array, Storage: model.TypeArray, Modifier: -1, Collation: info.Collation},
		Children: []model.TypeLayout{{Type: model.Type{ID: elementID, Storage: storage, Modifier: -1, Collation: info.Collation}}},
	}
	result := model.TypeLayout{Type: model.Type{ID: 2275, Storage: model.TypeString, Modifier: -1}}
	function, err := builtin.ResolveTyped(t.Context(), 751, []model.TypeLayout{layout}, result, options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireArrayOutput(t *testing.T, function builtin.Callable, input model.Value, want string) model.Value {
	t.Helper()
	got, err := function.Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || got.Type != model.TypeString || got.IsNull || got.StrVal != want {
		t.Fatalf("array_out = %q, %v; want %q", got.StrVal, err, want)
	}
	_, err = function.Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	return got
}

func TestArrayOutputPreservesNestedBounds(t *testing.T) {
	function := arrayOutputCallable(t, 23, model.TypeInt64, builtin.ResolveOptions{})
	values := []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4), model.Int64Val(5), model.Int64Val(6)}
	for _, tc := range []struct {
		name       string
		dimensions []model.ArrayDimension
		want       string
	}{
		{"default bounds", []model.ArrayDimension{{LowerBound: 1, Length: 2}, {LowerBound: 1, Length: 3}}, "{{1,NULL,3},{4,5,6}}"},
		{"outer lower bound", []model.ArrayDimension{{LowerBound: -2, Length: 2}, {LowerBound: 1, Length: 3}}, "[-2:-1][1:3]={{1,NULL,3},{4,5,6}}"},
		{"inner lower bound", []model.ArrayDimension{{LowerBound: 1, Length: 3}, {LowerBound: 0, Length: 2}}, "[1:3][0:1]={{1,NULL},{3,4},{5,6}}"},
		{"singleton middle dimension", []model.ArrayDimension{{LowerBound: 1, Length: 2}, {LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 3}}, "{{{1,NULL,3}},{{4,5,6}}}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireArrayOutput(t, function, model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: tc.dimensions}, tc.want)
		})
	}
	dimensions := make([]model.ArrayDimension, 6)
	for i := range dimensions {
		dimensions[i] = model.ArrayDimension{LowerBound: math.MinInt32, Length: 1}
	}
	requireArrayOutput(t, function, model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}, Dimensions: dimensions}, strings.Repeat("[-2147483648:-2147483648]", 6)+"={{{{{{1}}}}}}")
}

func TestArrayOutputQuotesOnlyArrayMetacharacters(t *testing.T) {
	function := arrayOutputCallable(t, 25, model.TypeString, builtin.ResolveOptions{})
	for _, tc := range []struct{ name, text, want string }{
		{"empty", "", `{""}`},
		{"literal null", "nUlL", `{"nUlL"}`},
		{"null prefix", "NULLx", `{NULLx}`},
		{"delimiter", "a,b", `{"a,b"}`},
		{"braces", "{a}", `{"{a}"}`},
		{"quote and backslash", "a\"b\\c", `{"a\"b\\c"}`},
		{"all ASCII whitespace", " \t\n\v\f\r", "{\" \t\n\v\f\r\"}"},
		{"non ASCII whitespace", "a\u00a0\u2003b", "{a\u00a0\u2003b}"},
		{"dimension punctuation", "[1:2]=x", `{[1:2]=x}`},
		{"UTF8", "é日", `{é日}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal(tc.text)}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}}
			requireArrayOutput(t, function, input, tc.want)
		})
	}
}

func TestArrayOutputUsesElementCodecs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      model.TypeID
		storage model.DataType
		values  []model.Value
		options builtin.ResolveOptions
		want    string
	}{
		{name: "boolean", id: 16, storage: model.TypeBoolean, values: []model.Value{model.BoolVal(true), model.BoolVal(false)}, want: "{t,f}"},
		{name: "int2", id: 21, storage: model.TypeInt64, values: []model.Value{model.Int64Val(math.MinInt16), model.Int64Val(math.MaxInt16)}, want: "{-32768,32767}"},
		{name: "int8", id: 20, storage: model.TypeInt64, values: []model.Value{model.Int64Val(math.MinInt64), model.Int64Val(math.MaxInt64)}, want: "{-9223372036854775808,9223372036854775807}"},
		{name: "float4", id: 700, storage: model.TypeFloat64, values: []model.Value{model.Float64Val(0.5), model.Float64Val(math.Inf(-1))}, options: builtin.ResolveOptions{ExtraFloatDigits: 1}, want: "{0.5,-Infinity}"},
		{name: "float8", id: 701, storage: model.TypeFloat64, values: []model.Value{model.Float64Val(math.Copysign(0, -1)), model.Float64Val(1e20), model.Float64Val(math.NaN())}, options: builtin.ResolveOptions{ExtraFloatDigits: 1}, want: "{-0,1e+20,NaN}"},
		{name: "bpchar", id: 1042, storage: model.TypeString, values: []model.Value{model.StringVal("a  ")}, want: `{"a  "}`},
		{name: "varchar", id: 1043, storage: model.TypeString, values: []model.Value{model.StringVal("a,b")}, want: `{"a,b"}`},
		{name: "numeric", id: 1700, storage: model.TypeNumeric, values: []model.Value{numericDatum("12.300"), numericDatum("NaN"), numericDatum("-Infinity")}, want: "{12.300,NaN,-Infinity}"},
		{name: "bytea hex", id: 17, storage: model.TypeBytes, values: []model.Value{model.BytesVal([]byte{0, 255, '"', '\\'}), model.BytesVal(nil)}, want: `{"\\x00ff225c","\\x"}`},
		{name: "bytea escape", id: 17, storage: model.TypeBytes, values: []model.Value{model.BytesVal([]byte{0, 255, '"', '\\'}), model.BytesVal(nil)}, options: builtin.ResolveOptions{ByteaEscape: true}, want: `{"\\000\\377\"\\\\",""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayOutputCallable(t, tc.id, tc.storage, tc.options)
			input := model.Value{Type: model.TypeArray, ArrayVal: tc.values, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: int32(len(tc.values))}}}
			requireArrayOutput(t, function, input, tc.want)
		})
	}
}

func TestArrayOutputDistinguishesEmptyNullAndNullElements(t *testing.T) {
	function := arrayOutputCallable(t, 1700, model.TypeNumeric, builtin.ResolveOptions{})
	requireArrayOutput(t, function, model.Value{Type: model.TypeArray}, "{}")
	got, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeArray)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !got.IsNull || got.Type != model.TypeString {
		t.Fatalf("NULL array_out = %v, %v", got, err)
	}
	input := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{{Type: model.TypeNumeric, IsNull: true, StrVal: "not numeric"}, numericDatum("1.00")}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}}
	requireArrayOutput(t, function, input, "{NULL,1.00}")
}

func TestArrayOutputRejectsInvalidShape(t *testing.T) {
	function := arrayOutputCallable(t, 23, model.TypeInt64, builtin.ResolveOptions{})
	for _, tc := range []struct {
		name       string
		values     []model.Value
		dimensions []model.ArrayDimension
	}{
		{"empty shape with datum", []model.Value{model.Int64Val(1)}, nil},
		{"dimensions without elements", nil, []model.ArrayDimension{{LowerBound: 1, Length: 1}}},
		{"mismatched element count", []model.Value{model.Int64Val(1)}, []model.ArrayDimension{{LowerBound: 1, Length: 2}}},
		{"zero dimension", []model.Value{model.Int64Val(1)}, []model.ArrayDimension{{LowerBound: 1, Length: 0}}},
		{"negative dimension", []model.Value{model.Int64Val(1)}, []model.ArrayDimension{{LowerBound: 1, Length: -1}}},
		{"last index exceeds maximum", []model.Value{model.Int64Val(1), model.Int64Val(2)}, []model.ArrayDimension{{LowerBound: math.MaxInt32, Length: 2}}},
		{"too many dimensions", []model.Value{model.Int64Val(1)}, make([]model.ArrayDimension, 7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{{Type: model.TypeArray, ArrayVal: tc.values, Dimensions: tc.dimensions}}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "2202E")
		})
	}
}

func TestArrayOutputRejectsInvalidElementText(t *testing.T) {
	function := arrayOutputCallable(t, 25, model.TypeString, builtin.ResolveOptions{})
	for _, text := range []string{"a\x00b", "a\xffb", strings.Repeat("a", 4095) + "\xc3"} {
		input := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal(text)}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}}
		_, err := function.Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22021")
	}
}

func TestArrayOutputOwnsStorageAcrossGrowthAndLaterCalls(t *testing.T) {
	function := arrayOutputCallable(t, 25, model.TypeString, builtin.ResolveOptions{})
	borrowed := []byte(strings.Repeat("a", 4095) + "\\\"é日")
	text := unsafe.String(unsafe.SliceData(borrowed), len(borrowed))
	values := make([]model.Value, 80)
	for i := range values {
		values[i] = model.StringVal(text)
	}
	input := model.Value{Type: model.TypeArray, ArrayVal: values, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: int32(len(values))}}}
	encoded := `"` + strings.Repeat("a", 4095) + `\\\"é日"`
	want := "{" + strings.Repeat(encoded+",", 79) + encoded + "}"
	got := requireArrayOutput(t, function, input, want)
	clear(borrowed)
	clear(values)
	input.Dimensions[0].LowerBound = -7
	requireArrayOutput(t, function, model.Value{Type: model.TypeArray}, "{}")
	if got.StrVal != want {
		t.Fatal("array_out retained borrowed element storage or reused its result buffer")
	}
}

func TestArrayOutputCancellation(t *testing.T) {
	function := arrayOutputCallable(t, 25, model.TypeString, builtin.ResolveOptions{})
	longText := strings.Repeat("a\\\"é", 32768)
	input := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal(longText)}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := function.Call(ctx, []model.Value{input}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled array_out = %v", err)
	}
	for _, after := range []int{20, 100, 130} {
		t.Run(fmt.Sprint(after), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: after}
			_, err := function.Call(interrupted, []model.Value{input}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted array_out = %v", err)
			}
		})
	}
}
