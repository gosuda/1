package builtin

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func inputRecordLayout(fields ...model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: 81000, Storage: model.TypeComposite, Modifier: -1}, Children: fields}
}

func inputRecordCallable(t *testing.T, layout model.TypeLayout) Callable {
	t.Helper()
	args := []model.TypeLayout{typedQuoteLayout(2275, model.TypeString), typedQuoteLayout(26, model.TypeInt64), typedQuoteLayout(23, model.TypeInt64)}
	function, err := ResolveTyped(t.Context(), 2290, args, layout, ResolveOptions{Collation: 950, DefaultCollation: 950}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func readRecord(t *testing.T, function Callable, layout model.TypeLayout, text string, budget int64) (model.Value, error) {
	t.Helper()
	return function.Call(t.Context(), []model.Value{model.StringVal(text), model.Int64Val(int64(layout.Type.ID)), model.Int64Val(int64(layout.Type.Modifier))}, CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
}

func TestRecordInputPreservesNullEmptyAndLiteralNullFields(t *testing.T) {
	text := typedQuoteLayout(25, model.TypeString)
	layout := inputRecordLayout(text, text, text, text)
	function := inputRecordCallable(t, layout)
	got, err := readRecord(t, function, layout, ` (,"",NULL, ab ) `, 4096)
	want := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.NullVal(model.TypeString), model.StringVal(""), model.StringVal("NULL"), model.StringVal(" ab ")}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("record=%v,%v want %v", got, err, want)
	}
}

func TestRecordInputDecodesQuotesEscapesAndAdjacentText(t *testing.T) {
	text := typedQuoteLayout(25, model.TypeString)
	layout := inputRecordLayout(text, text, text)
	function := inputRecordCallable(t, layout)
	got, err := readRecord(t, function, layout, `("a""b",c\,d,a"b"c)`, 4096)
	want := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal(`a"b`), model.StringVal("c,d"), model.StringVal("abc")}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded record=%v,%v want %v", got, err, want)
	}
}

func TestRecordInputComposesNamedRecordAndArrayCodecs(t *testing.T) {
	nested := inputRecordLayout(typedQuoteLayout(25, model.TypeString), typedQuoteLayout(23, model.TypeInt64))
	nested.Type.ID = 81001
	array := replacementLayout(t, 23, model.TypeInt64)
	layout := inputRecordLayout(nested, array)
	function := inputRecordCallable(t, layout)
	got, err := readRecord(t, function, layout, `("(x,2)","{1,NULL,3}")`, 16384)
	want := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal("x"), model.Int64Val(2)}}, replacementArray(model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3))}}
	want.ArrayVal[1].Dimensions[0].LowerBound = 1
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("nested input=%v,%v want %v", got, err, want)
	}
}

func TestRecordInputEarlierConversionErrorPrecedesLaterSyntax(t *testing.T) {
	layout := inputRecordLayout(typedQuoteLayout(23, model.TypeInt64), typedQuoteLayout(25, model.TypeString))
	function := inputRecordCallable(t, layout)
	_, err := readRecord(t, function, layout, `(2147483648,"unterminated)`, 4096)
	requireReplacementSQLState(t, err, "22003")
	character := typedQuoteLayout(1043, model.TypeString)
	character.Type.Modifier = 5
	layout = inputRecordLayout(character, typedQuoteLayout(25, model.TypeString))
	function = inputRecordCallable(t, layout)
	_, err = readRecord(t, function, layout, `(long,"unterminated)`, 4096)
	requireReplacementSQLState(t, err, "22001")
}

func TestRecordInputFieldCountAndTrailingSyntax(t *testing.T) {
	layout := inputRecordLayout(typedQuoteLayout(23, model.TypeInt64), typedQuoteLayout(25, model.TypeString))
	function := inputRecordCallable(t, layout)
	for _, text := range []string{`1,x)`, `(1)`, `(1,x,y)`, `(1,x)junk`, `(1,"x)`, `(1,x\)`} {
		_, err := readRecord(t, function, layout, text, 4096)
		requireReplacementSQLState(t, err, "22P02")
	}
}

func TestRecordInputAnonymousIdentityAndEmptyNamedRecord(t *testing.T) {
	layout := inputRecordLayout()
	function := inputRecordCallable(t, layout)
	got, err := readRecord(t, function, layout, "()", 0)
	if err != nil || got.Type != model.TypeComposite || got.IsNull || len(got.ArrayVal) != 0 {
		t.Fatalf("empty record=%v,%v", got, err)
	}
	layout.Type.ID = 2249
	function = inputRecordCallable(t, layout)
	_, err = readRecord(t, function, layout, "malformed", 0)
	requireReplacementSQLState(t, err, "0A000")
}

func TestRecordInputAdmitsHeadersAndDecodedTokenOverlap(t *testing.T) {
	layout := inputRecordLayout(typedQuoteLayout(25, model.TypeString))
	function := inputRecordCallable(t, layout)
	got, err := readRecord(t, function, layout, `(x)`, arrayValueBytes+1)
	if err != nil || got.ArrayVal[0].StrVal != "x" {
		t.Fatalf("borrowed token=%v,%v", got, err)
	}
	got, err = readRecord(t, function, layout, `("x")`, arrayValueBytes+2)
	if err != nil || got.ArrayVal[0].StrVal != "x" {
		t.Fatalf("decoded token=%v,%v", got, err)
	}
	_, err = readRecord(t, function, layout, `("x")`, arrayValueBytes+1)
	requireReplacementSQLState(t, err, "54000")
}

func TestRecordInputChecksOIDButIgnoresNamedTypmodArgument(t *testing.T) {
	layout := inputRecordLayout(typedQuoteLayout(23, model.TypeInt64))
	function := inputRecordCallable(t, layout)
	values := []model.Value{model.StringVal("(7)"), model.Int64Val(int64(layout.Type.ID)), model.Int64Val(123)}
	got, err := function.Call(t.Context(), values, CallBudget{OutputBytes: arrayValueBytes, MemoryBytes: 64 << 20})
	if err != nil || got.ArrayVal[0].IntVal != 7 {
		t.Fatalf("named typmod=%v,%v", got, err)
	}
	values[1] = model.Int64Val(81001)
	_, err = function.Call(t.Context(), values, CallBudget{OutputBytes: arrayValueBytes, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "42804")
}

func TestRecordInputCancellationAndStrictNull(t *testing.T) {
	layout := inputRecordLayout(typedQuoteLayout(25, model.TypeString))
	function := inputRecordCallable(t, layout)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	values := []model.Value{model.StringVal("(x)"), model.Int64Val(81000), model.Int64Val(-1)}
	_, err := function.Call(ctx, values, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled input=%v", err)
	}
	values[0] = model.NullVal(model.TypeString)
	got, err := function.Call(t.Context(), values, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !got.IsNull || got.Type != model.TypeComposite {
		t.Fatalf("NULL record=%v,%v", got, err)
	}
}
