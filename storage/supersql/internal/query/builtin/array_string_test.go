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

func arrayToStringCallable(t *testing.T, id model.FunctionID, elementID model.TypeID, storage model.DataType, options builtin.ResolveOptions) builtin.Callable {
	t.Helper()
	info, ok := builtin.LookupType(elementID)
	if !ok {
		t.Fatalf("missing element type %d", elementID)
	}
	text := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Modifier: -1, Collation: 100}}
	arguments := []model.TypeLayout{
		{
			Type:     model.Type{ID: info.Array, Storage: model.TypeArray, Modifier: -1, Collation: info.Collation},
			Children: []model.TypeLayout{{Type: model.Type{ID: elementID, Storage: storage, Modifier: -1, Collation: info.Collation}}},
		},
		text,
	}
	if id == 384 {
		arguments = append(arguments, text)
	}
	function, err := builtin.ResolveTyped(t.Context(), id, arguments, text, options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func arrayToStringInput(values ...model.Value) model.Value {
	value := model.Value{Type: model.TypeArray, ArrayVal: values}
	if len(values) != 0 {
		value.Dimensions = []model.ArrayDimension{{LowerBound: 1, Length: int32(len(values))}}
	}
	return value
}

func requireArrayToString(t *testing.T, function builtin.Callable, arguments []model.Value, want string) model.Value {
	t.Helper()
	got, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || got.Type != model.TypeString || got.IsNull || got.StrVal != want {
		t.Fatalf("array_to_string = %q, %v; want %q", got.StrVal, err, want)
	}
	measured, err := model.ValueBytes(t.Context(), got, uint64(len(want)))
	if err != nil || measured != uint64(len(want)) {
		t.Fatalf("returned payload = %d, %v; want %d", measured, err, len(want))
	}
	if len(want) != 0 {
		_, err = function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54000")
	}
	return got
}

func TestArrayToStringSeparatorsAndNulls(t *testing.T) {
	null := model.Value{Type: model.TypeString, IsNull: true, StrVal: "ignored\x00"}
	input := arrayToStringInput(null, model.StringVal(""), null, model.StringVal("x"), model.StringVal(""), null)
	for _, tc := range []struct {
		name      string
		id        model.FunctionID
		input     model.Value
		delimiter string
		marker    model.Value
		want      string
	}{
		{"absent marker skips nulls", 395, input, "::", model.Value{}, "::x::"},
		{"null marker skips nulls", 384, input, "::", null, "::x::"},
		{"marker replaces nulls", 384, input, "::", model.StringVal("∅"), "∅::::∅::x::::∅"},
		{"empty marker participates", 384, input, "|", model.StringVal(""), "|||x||"},
		{"empty separator", 384, input, "", model.StringVal("∅"), "∅∅x∅"},
		{"empty two argument array", 395, arrayToStringInput(), "unused", model.Value{}, ""},
		{"empty three argument array", 384, arrayToStringInput(), "unused", model.StringVal("unused"), ""},
		{"all skipped", 395, arrayToStringInput(null, null), "unused", model.Value{}, ""},
		{"all empty markers", 384, arrayToStringInput(null, null), "日", model.StringVal(""), "日"},
		{"one empty element", 395, arrayToStringInput(model.StringVal("")), "unused", model.Value{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayToStringCallable(t, tc.id, 25, model.TypeString, builtin.ResolveOptions{})
			arguments := []model.Value{tc.input, model.StringVal(tc.delimiter)}
			if tc.id == 384 {
				arguments = append(arguments, tc.marker)
			}
			requireArrayToString(t, function, arguments, tc.want)
		})
	}
}

func TestArrayToStringFlattensNondefaultBounds(t *testing.T) {
	input := arrayToStringInput(model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4), model.Int64Val(5), model.Int64Val(6))
	input.Dimensions = []model.ArrayDimension{{LowerBound: -7, Length: 2}, {LowerBound: 0, Length: 3}}
	function := arrayToStringCallable(t, 384, 23, model.TypeInt64, builtin.ResolveOptions{})
	requireArrayToString(t, function, []model.Value{input, model.StringVal("/"), model.StringVal("?")}, "1/?/3/4/5/6")
}

func TestArrayToStringLeavesTextUnquoted(t *testing.T) {
	function := arrayToStringCallable(t, 384, 25, model.TypeString, builtin.ResolveOptions{})
	input := arrayToStringInput(model.StringVal("NULL"), model.StringVal("{a,b}"), model.StringVal("a\"b\\c"), model.StringVal(" \t\n"), model.StringVal("[-1:2]=é日"), model.NullVal(model.TypeString))
	requireArrayToString(t, function, []model.Value{input, model.StringVal("◆"), model.StringVal("{\"null\\}")}, "NULL◆{a,b}◆a\"b\\c◆ \t\n◆[-1:2]=é日◆{\"null\\}")
}

func TestArrayToStringUsesElementCodecs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      model.TypeID
		storage model.DataType
		values  []model.Value
		options builtin.ResolveOptions
		want    string
	}{
		{name: "boolean", id: 16, storage: model.TypeBoolean, values: []model.Value{model.BoolVal(true), model.BoolVal(false)}, want: "t|f"},
		{name: "bpchar spaces", id: 1042, storage: model.TypeString, values: []model.Value{model.StringVal("a  "), model.StringVal("  b")}, want: "a  |  b"},
		{name: "numeric scale and specials", id: 1700, storage: model.TypeNumeric, values: []model.Value{numericDatum("12.300"), numericDatum("NaN"), numericDatum("-Infinity")}, want: "12.300|NaN|-Infinity"},
		{name: "bytea hex", id: 17, storage: model.TypeBytes, values: []model.Value{model.BytesVal([]byte{0, 255, '"', '\\'}), model.BytesVal(nil)}, want: `\x00ff225c|\x`},
		{name: "bytea escape", id: 17, storage: model.TypeBytes, values: []model.Value{model.BytesVal([]byte{0, 255, '"', '\\'}), model.BytesVal(nil)}, options: builtin.ResolveOptions{ByteaEscape: true}, want: `\000\377"\\|`},
		{name: "float shortest", id: 701, storage: model.TypeFloat64, values: []model.Value{model.Float64Val(1.2345678901234567), model.Float64Val(math.Copysign(0, -1))}, options: builtin.ResolveOptions{ExtraFloatDigits: 1}, want: "1.2345678901234567|-0"},
		{name: "float rounded", id: 701, storage: model.TypeFloat64, values: []model.Value{model.Float64Val(1.2345678901234567)}, want: "1.23456789012346"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayToStringCallable(t, 395, tc.id, tc.storage, tc.options)
			requireArrayToString(t, function, []model.Value{arrayToStringInput(tc.values...), model.StringVal("|")}, tc.want)
		})
	}
}

func TestArrayToStringPreservesElementErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      model.TypeID
		storage model.DataType
		value   model.Value
		code    string
	}{
		{"numeric syntax", 1700, model.TypeNumeric, numericDatum("not numeric"), "22P02"},
		{"integer range", 21, model.TypeInt64, model.Int64Val(math.MaxInt16 + 1), "22003"},
		{"element storage", 25, model.TypeString, model.Int64Val(7), "42804"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayToStringCallable(t, 395, tc.id, tc.storage, builtin.ResolveOptions{})
			_, err := function.Call(t.Context(), []model.Value{arrayToStringInput(tc.value), model.StringVal(",")}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestArrayToStringRejectsInvalidText(t *testing.T) {
	function := arrayToStringCallable(t, 384, 25, model.TypeString, builtin.ResolveOptions{})
	for _, field := range []string{"element", "delimiter", "marker"} {
		for i, text := range []string{"a\x00b", "a\xffb", strings.Repeat("a", 4095) + "\xc3"} {
			t.Run(fmt.Sprintf("%s/%d", field, i), func(t *testing.T) {
				arguments := []model.Value{arrayToStringInput(model.StringVal("ok")), model.StringVal(","), model.StringVal("?")}
				switch field {
				case "element":
					arguments[0].ArrayVal[0] = model.StringVal(text)
				case "delimiter":
					arguments[1] = model.StringVal(text)
				case "marker":
					arguments[2] = model.StringVal(text)
				}
				_, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22021")
			})
		}
	}
}

func TestArrayToStringRejectsInvalidShape(t *testing.T) {
	function := arrayToStringCallable(t, 395, 23, model.TypeInt64, builtin.ResolveOptions{})
	for _, tc := range []struct {
		name  string
		input model.Value
	}{
		{"elements without dimensions", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}}},
		{"mismatched cardinality", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}}},
		{"empty array with dimensions", model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{tc.input, model.StringVal(",")}, builtin.CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "2202E")
		})
	}
}

func TestArrayToStringNullInputsBypassContents(t *testing.T) {
	for _, id := range []model.FunctionID{384, 395} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := arrayToStringCallable(t, id, 25, model.TypeString, builtin.ResolveOptions{})
			for _, arguments := range [][]model.Value{
				{model.NullVal(model.TypeArray), model.StringVal("\x00")},
				{{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal("\x00")}}, model.NullVal(model.TypeString)},
			} {
				if id == 384 {
					arguments = append(arguments, model.StringVal("\x00"))
				}
				got, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !got.IsNull || got.Type != model.TypeString {
					t.Fatalf("NULL array_to_string = %v, %v", got, err)
				}
			}
		})
	}
}

func TestArrayToStringOwnsGrowingOutput(t *testing.T) {
	function := arrayToStringCallable(t, 384, 25, model.TypeString, builtin.ResolveOptions{})
	borrowed := []byte(strings.Repeat("a", 4095) + "é日")
	delimiter := []byte("|")
	marker := []byte("∅")
	text := unsafe.String(unsafe.SliceData(borrowed), len(borrowed))
	values := make([]model.Value, 80)
	for i := range values {
		values[i] = model.NullVal(model.TypeString)
		if i%2 == 0 {
			values[i] = model.StringVal(text)
		}
	}
	arguments := []model.Value{arrayToStringInput(values...), model.StringVal(unsafe.String(unsafe.SliceData(delimiter), len(delimiter))), model.StringVal(unsafe.String(unsafe.SliceData(marker), len(marker)))}
	want := strings.Repeat(text+"|∅|", 39) + text + "|∅"
	got, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil || got.IsNull || got.Type != model.TypeString || got.StrVal != want {
		t.Fatalf("array_to_string before source mutation: %v", err)
	}
	clear(borrowed)
	clear(delimiter)
	clear(marker)
	clear(values)
	requireArrayToString(t, function, []model.Value{arrayToStringInput(model.StringVal("later")), model.StringVal(","), model.NullVal(model.TypeString)}, "later")
	if got.StrVal != want {
		t.Fatal("array_to_string retained borrowed inputs or reused its result buffer")
	}
}

func TestArrayToStringCancellation(t *testing.T) {
	function := arrayToStringCallable(t, 384, 25, model.TypeString, builtin.ResolveOptions{})
	longText := strings.Repeat("é日", 32768)
	manyNulls := make([]model.Value, 20000)
	for i := range manyNulls {
		manyNulls[i] = model.NullVal(model.TypeString)
	}
	for _, tc := range []struct {
		name      string
		arguments []model.Value
		after     int
	}{
		{"element", []model.Value{arrayToStringInput(model.StringVal(longText)), model.StringVal(","), model.NullVal(model.TypeString)}, 20},
		{"element output copy", []model.Value{arrayToStringInput(model.StringVal(longText)), model.StringVal(","), model.NullVal(model.TypeString)}, 70},
		{"joined output copy", []model.Value{arrayToStringInput(model.StringVal(longText)), model.StringVal(","), model.NullVal(model.TypeString)}, 145},
		{"delimiter", []model.Value{arrayToStringInput(model.StringVal("a"), model.StringVal("b")), model.StringVal(longText), model.NullVal(model.TypeString)}, 20},
		{"marker", []model.Value{arrayToStringInput(model.NullVal(model.TypeString)), model.StringVal(","), model.StringVal(longText)}, 20},
		{"skipped elements", []model.Value{arrayToStringInput(manyNulls...), model.StringVal(","), model.NullVal(model.TypeString)}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: tc.after}
			_, err := function.Call(interrupted, tc.arguments, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted array_to_string = %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := function.Call(ctx, []model.Value{arrayToStringInput(), model.StringVal(","), model.NullVal(model.TypeString)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled array_to_string = %v", err)
	}
}
