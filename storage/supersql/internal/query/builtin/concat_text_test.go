package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func concatTextCallable(t *testing.T, id model.FunctionID, arguments []model.TypeLayout, options ResolveOptions) Callable {
	t.Helper()
	function, err := ResolveTyped(t.Context(), id, arguments, typedQuoteLayout(25, model.TypeString), options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func concatArrayCallable(t *testing.T, id model.FunctionID, element model.TypeID, storage model.DataType, options ResolveOptions) Callable {
	t.Helper()
	arguments := []model.TypeLayout{replacementLayout(t, element, storage)}
	if id == 3059 {
		arguments = append([]model.TypeLayout{typedQuoteLayout(25, model.TypeString)}, arguments...)
	}
	function, err := ResolveVariadic(t.Context(), id, arguments, typedQuoteLayout(25, model.TypeString), options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireConcatText(t *testing.T, function Callable, arguments []model.Value, budget int64, want string) model.Value {
	t.Helper()
	got, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
	if err != nil || got.IsNull || got.Type != model.TypeString || got.StrVal != want {
		t.Fatalf("concat %d = %v, %v; want %q", function.Signature().ID, got, err, want)
	}
	return got
}

func TestConcatHeterogeneousExpandedArguments(t *testing.T) {
	arguments := []model.Value{
		model.BoolVal(true), model.StringVal("x  "), model.StringVal("v "), model.Int64Val(-7),
		{Type: model.TypeNumeric, StrVal: "12.30"}, model.BytesVal([]byte{0, '\\'}),
		model.Float64Val(1.25), model.BoolVal(false), model.StringVal(""), model.NullVal(model.TypeInt64),
	}
	ids := []model.TypeID{16, 1042, 1043, 20, 1700, 17, 701, 16, 25, 23}
	layouts := make([]model.TypeLayout, len(ids))
	for i, id := range ids {
		layouts[i] = typedQuoteLayout(id, arguments[i].Type)
	}
	function := concatTextCallable(t, 3058, layouts, ResolveOptions{ExtraFloatDigits: 1})
	requireConcatText(t, function, arguments, 4096, `tx  v -712.30\x005c1.25f`)
	layouts = append([]model.TypeLayout{typedQuoteLayout(25, model.TypeString)}, layouts...)
	arguments = append([]model.Value{model.StringVal("|")}, arguments...)
	function = concatTextCallable(t, 3059, layouts, ResolveOptions{ExtraFloatDigits: 1})
	requireConcatText(t, function, arguments, 4096, `t|x  |v |-7|12.30|\x005c|1.25|f|`)
}

func TestConcatOrdinaryArrayUsesArrayOutput(t *testing.T) {
	layout := replacementLayout(t, 16, model.TypeBoolean)
	function := concatTextCallable(t, 3058, []model.TypeLayout{layout}, ResolveOptions{})
	layout.Children[0] = typedQuoteLayout(25, model.TypeString)
	input := replacementArray(model.BoolVal(true), model.NullVal(model.TypeBoolean), model.BoolVal(false), model.BoolVal(true))
	input.Dimensions = []model.ArrayDimension{{LowerBound: -1, Length: 2}, {LowerBound: 0, Length: 2}}
	requireConcatText(t, function, []model.Value{input}, 4096, "[-1:0][0:1]={{t,NULL},{f,t}}")
}

func TestConcatExplicitArrayFlattensRowMajor(t *testing.T) {
	input := replacementArray(model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4), model.Int64Val(5), model.Int64Val(6))
	input.Dimensions = []model.ArrayDimension{{LowerBound: -7, Length: 2}, {LowerBound: 0, Length: 3}}
	function := concatArrayCallable(t, 3058, 23, model.TypeInt64, ResolveOptions{})
	requireConcatText(t, function, []model.Value{input}, 1024, "13456")
	function = concatArrayCallable(t, 3059, 23, model.TypeInt64, ResolveOptions{})
	requireConcatText(t, function, []model.Value{model.StringVal("/"), input}, 1024, "1/3/4/5/6")
}

func TestConcatExplicitArrayExceedsOrdinaryArgumentLimit(t *testing.T) {
	values := make([]model.Value, 257)
	for i := range values {
		values[i] = model.StringVal("a")
	}
	function := concatArrayCallable(t, 3058, 25, model.TypeString, ResolveOptions{})
	requireConcatText(t, function, []model.Value{replacementArray(values...)}, 4096, strings.Repeat("a", 257))
}

func TestConcatExplicitArrayUsesSelectedElementOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      model.TypeID
		storage model.DataType
		values  []model.Value
		options ResolveOptions
		want    string
	}{
		{"boolean", 16, model.TypeBoolean, []model.Value{model.BoolVal(true), model.BoolVal(false)}, ResolveOptions{}, "t|f"},
		{"bpchar padding", 1042, model.TypeString, []model.Value{model.StringVal("a  "), model.StringVal("  b")}, ResolveOptions{}, "a  |  b"},
		{"bytea escape", 17, model.TypeBytes, []model.Value{model.BytesVal([]byte{0, '\\'}), model.BytesVal(nil)}, ResolveOptions{ByteaEscape: true}, `\000\\|`},
		{"float shortest", 701, model.TypeFloat64, []model.Value{model.Float64Val(1.2345678901234567)}, ResolveOptions{ExtraFloatDigits: 1}, "1.2345678901234567"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := concatArrayCallable(t, 3059, tc.id, tc.storage, tc.options)
			requireConcatText(t, function, []model.Value{model.StringVal("|"), replacementArray(tc.values...)}, 4096, tc.want)
		})
	}
}

func TestConcatSeparatorsSkipOnlyNulls(t *testing.T) {
	null := model.Value{Type: model.TypeString, IsNull: true, StrVal: "ignored\x00"}
	values := []model.Value{null, model.StringVal(""), null, model.StringVal("x"), model.StringVal(""), null}
	layouts := make([]model.TypeLayout, len(values)+1)
	for i := range layouts {
		layouts[i] = typedQuoteLayout(25, model.TypeString)
	}
	ordinary := concatTextCallable(t, 3059, layouts, ResolveOptions{})
	array := concatArrayCallable(t, 3059, 25, model.TypeString, ResolveOptions{})
	for _, tc := range []struct{ name, separator, want string }{
		{"nonempty separator", "::", "::x::"},
		{"empty separator", "", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireConcatText(t, ordinary, append([]model.Value{model.StringVal(tc.separator)}, values...), 1024, tc.want)
			requireConcatText(t, array, []model.Value{model.StringVal(tc.separator), replacementArray(values...)}, 1024, tc.want)
		})
	}
}

func TestConcatEmptyResultsNeedNoBudget(t *testing.T) {
	for _, id := range []model.FunctionID{3058, 3059} {
		function := concatArrayCallable(t, id, 25, model.TypeString, ResolveOptions{})
		for _, input := range []model.Value{replacementArray(), replacementArray(model.NullVal(model.TypeString)), replacementArray(model.StringVal(""))} {
			arguments := []model.Value{input}
			if id == 3059 {
				arguments = append([]model.Value{model.StringVal("unused")}, arguments...)
			}
			requireConcatText(t, function, arguments, 0, "")
		}
		layouts := []model.TypeLayout{typedQuoteLayout(25, model.TypeString)}
		arguments := []model.Value{model.NullVal(model.TypeString)}
		if id == 3059 {
			layouts = append(layouts, layouts[0])
			arguments = append([]model.Value{model.StringVal("unused")}, arguments...)
		}
		requireConcatText(t, concatTextCallable(t, id, layouts, ResolveOptions{}), arguments, 0, "")
	}
}

func TestConcatNullArrayAndSeparatorSkipDecoding(t *testing.T) {
	malformed := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal("invalid element")}}
	for _, tc := range []struct {
		name      string
		id        model.FunctionID
		separator model.Value
		input     model.Value
	}{
		{"NULL concat array", 3058, model.Value{}, model.NullVal(model.TypeArray)},
		{"NULL concat_ws array", 3059, model.StringVal("/"), model.NullVal(model.TypeArray)},
		{"NULL separator", 3059, model.NullVal(model.TypeString), malformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := concatArrayCallable(t, tc.id, 23, model.TypeInt64, ResolveOptions{})
			if tc.input.IsNull {
				tc.input.ArrayVal = malformed.ArrayVal
			}
			arguments := []model.Value{tc.input}
			if tc.id == 3059 {
				arguments = append([]model.Value{tc.separator}, arguments...)
			}
			got, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !got.IsNull || got.Type != model.TypeString {
				t.Fatalf("NULL concat = %v, %v", got, err)
			}
		})
	}
	function := concatArrayCallable(t, 3059, 23, model.TypeInt64, ResolveOptions{})
	_, err := function.Call(t.Context(), []model.Value{model.StringVal("/"), malformed}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "2202E")
	ordinary := concatTextCallable(t, 3059, []model.TypeLayout{typedQuoteLayout(25, model.TypeString), typedQuoteLayout(1700, model.TypeNumeric)}, ResolveOptions{})
	got, err := ordinary.Call(t.Context(), []model.Value{model.NullVal(model.TypeString), {Type: model.TypeNumeric, StrVal: "invalid numeric"}}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !got.IsNull || got.Type != model.TypeString {
		t.Fatalf("NULL separator with unconverted numeric = %v, %v", got, err)
	}
}

func TestConcatRejectsInvalidText(t *testing.T) {
	layout := typedQuoteLayout(25, model.TypeString)
	function := concatTextCallable(t, 3059, []model.TypeLayout{layout, layout}, ResolveOptions{})
	for _, invalid := range []string{"nul\x00", "invalid\xff", strings.Repeat("a", 4095) + "\xc3"} {
		for _, arguments := range [][]model.Value{
			{model.StringVal("/"), model.StringVal(invalid)},
			{model.StringVal(invalid), model.StringVal("valid")},
		} {
			_, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, "22021")
		}
	}
}

func TestConcatOwnsGrowingResult(t *testing.T) {
	function := concatArrayCallable(t, 3059, 25, model.TypeString, ResolveOptions{})
	borrowed := []byte(strings.Repeat("a", 4095) + "é日")
	separator := []byte("◆")
	text := unsafe.String(unsafe.SliceData(borrowed), len(borrowed))
	values := make([]model.Value, 80)
	for i := range values {
		values[i] = model.StringVal(text)
	}
	input := replacementArray(values...)
	want := strings.Repeat(string(borrowed)+string(separator), 79) + string(borrowed)
	got := requireConcatText(t, function, []model.Value{model.StringVal(unsafe.String(unsafe.SliceData(separator), len(separator))), input}, 4<<20, want)
	clear(borrowed)
	clear(separator)
	clear(values)
	input.Dimensions[0].LowerBound = 99
	requireConcatText(t, function, []model.Value{model.StringVal("/"), replacementArray(model.StringVal("next"))}, 64, "next")
	if got.StrVal != want {
		t.Fatal("concat retained borrowed storage or reused an earlier result")
	}
}

func TestConcatAccountsForConversionOverlap(t *testing.T) {
	function := concatTextCallable(t, 3058, []model.TypeLayout{typedQuoteLayout(23, model.TypeInt64)}, ResolveOptions{})
	arguments := []model.Value{model.Int64Val(123)}
	requireConcatText(t, function, arguments, 6, "123")
	_, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "54000")
}

func TestConcatAccountsForGrowthOverlap(t *testing.T) {
	layout := typedQuoteLayout(25, model.TypeString)
	function := concatTextCallable(t, 3058, []model.TypeLayout{layout, layout}, ResolveOptions{})
	arguments := []model.Value{model.StringVal("abcd"), model.StringVal("efgh")}
	requireConcatText(t, function, arguments, 16, "abcdefgh")
	_, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: 15, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "54000")
}

func TestConcatAccountsForCompactionOverlap(t *testing.T) {
	layout := typedQuoteLayout(25, model.TypeString)
	function := concatTextCallable(t, 3059, []model.TypeLayout{layout, layout, layout}, ResolveOptions{})
	arguments := []model.Value{model.StringVal("/"), model.StringVal("abcd"), model.StringVal("e")}
	requireConcatText(t, function, arguments, 14, "abcd/e")
	_, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: 13, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "54000")
}
func TestConcatSelectedOutputFailureStopsConversion(t *testing.T) {
	failure := &model.Error{Code: "22003", Message: "selected output failed"}
	var seen []int64
	conversion := typedQuoteConversion(model.TypeInt64, func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		seen = append(seen, arguments[0].IntVal)
		if arguments[0].IntVal == 3 {
			return model.Value{}, failure
		}
		return ownedText(ctx, budget, "first")
	})
	codec := variadicTextCodec{
		signature: Signature{ID: 3058},
		outputs:   []variadicTextOutput{{callable: conversion}, {callable: conversion}, {callable: conversion}, {callable: conversion}},
	}
	arguments := []model.Value{model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.Int64Val(4)}
	_, err := compileConcatText(&codec)(t.Context(), arguments, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	if !errors.Is(err, failure) || len(seen) != 2 || seen[0] != 1 || seen[1] != 3 {
		t.Fatalf("concat error = %v, converted inputs = %v; want selected failure after [1 3]", err, seen)
	}
}

func TestConcatBudgetFailureDoesNotRetryOutput(t *testing.T) {
	var seen []int64
	conversion := typedQuoteConversion(model.TypeInt64, func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
		budget := callBudget.OutputBytes
		seen = append(seen, arguments[0].IntVal)
		return ownedText(ctx, budget, "abcd")
	})
	codec := variadicTextCodec{
		signature: Signature{ID: 3058},
		outputs:   []variadicTextOutput{{callable: conversion}, {callable: conversion}},
	}
	_, err := compileConcatText(&codec)(t.Context(), []model.Value{model.Int64Val(1), model.Int64Val(2)}, CallBudget{OutputBytes: 15, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "54000")
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("budget failure converted inputs = %v; want [1 2] without retry", seen)
	}
}

func TestConcatCancellation(t *testing.T) {
	function := concatArrayCallable(t, 3059, 25, model.TypeString, ResolveOptions{})
	arguments := []model.Value{model.StringVal("/"), replacementArray(model.StringVal(strings.Repeat("é日", 32768)), model.StringVal("last"))}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := function.Call(ctx, arguments, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled concat = %v", err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	interrupted := &replacementCancelContext{Context: ctx, cancel: cancel, remaining: 30}
	_, err = function.Call(interrupted, arguments, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted concat = %v", err)
	}
}

func TestConcatCancellationDuringNullScan(t *testing.T) {
	values := make([]model.Value, 32768)
	for i := range values {
		values[i] = model.NullVal(model.TypeString)
	}
	function := concatArrayCallable(t, 3058, 25, model.TypeString, ResolveOptions{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	interrupted := &replacementCancelContext{Context: ctx, cancel: cancel, remaining: 5}
	_, err := function.Call(interrupted, []model.Value{replacementArray(values...)}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled all-NULL concat = %v", err)
	}
}
