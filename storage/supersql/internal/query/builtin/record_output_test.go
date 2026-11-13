package builtin

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func recordOutputLayout(id model.TypeID, fields ...model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: id, Modifier: -1, Storage: model.TypeComposite}, Children: fields}
}

func recordOutputCallable(t *testing.T, layout model.TypeLayout) Callable {
	t.Helper()
	function, err := ResolveTyped(t.Context(), 2291, []model.TypeLayout{layout}, typedQuoteLayout(2275, model.TypeString), ResolveOptions{}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func recordOutputValue(fields ...model.Value) model.Value {
	return model.Value{Type: model.TypeComposite, ArrayVal: fields}
}

func requireRecordOutput(t *testing.T, function Callable, value model.Value, budget int64, want string) model.Value {
	t.Helper()
	got, err := function.Call(t.Context(), []model.Value{value}, CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
	if err != nil || got.IsNull || got.Type != model.TypeString || got.StrVal != want {
		t.Fatalf("record_out = %q, %v; want %q", got.StrVal, err, want)
	}
	return got
}

func TestRecordOutputQuotesRecordMetacharacters(t *testing.T) {
	function := recordOutputCallable(t, recordOutputLayout(2249, typedQuoteLayout(25, model.TypeString)))
	for _, tc := range []struct{ name, text, want string }{
		{"empty", "", "(\"\")"},
		{"literal NULL", "NULL", `(NULL)`},
		{"comma", "a,b", `("a,b")`},
		{"parentheses", "(a)", `("(a)")`},
		{"quote and backslash", "a\"b\\c", `("a""b\\c")`},
		{"all ASCII whitespace", " \t\n\v\f\r", "(\" \t\n\v\f\r\")"},
		{"non ASCII whitespace", "a\u00a0\u2003b", "(a\u00a0\u2003b)"},
		{"array punctuation", "{a}[1:2]=x", `({a}[1:2]=x)`},
		{"UTF8", "é日", `(é日)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireRecordOutput(t, function, recordOutputValue(model.StringVal(tc.text)), 1024, tc.want)
		})
	}
}

func TestRecordOutputDistinguishesNullEmptyTextAndEmptyRecord(t *testing.T) {
	text := typedQuoteLayout(25, model.TypeString)
	function := recordOutputCallable(t, recordOutputLayout(2249, text, text, text))
	requireRecordOutput(t, function, recordOutputValue(model.NullVal(model.TypeString), model.StringVal(""), model.StringVal("NULL")), 64, `(,"",NULL)`)
	requireRecordOutput(t, recordOutputCallable(t, recordOutputLayout(2249)), recordOutputValue(), 2, `()`)
	null := model.NullVal(model.TypeComposite)
	null.ArrayVal = []model.Value{model.StringVal("ignored")}
	null.Dimensions = []model.ArrayDimension{{Length: 1}}
	got, err := function.Call(t.Context(), []model.Value{null}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !got.IsNull || got.Type != model.TypeString {
		t.Fatalf("strict NULL record_out = %v, %v", got, err)
	}
}

func TestRecordOutputPreservesNamedNestedLayouts(t *testing.T) {
	text := typedQuoteLayout(25, model.TypeString)
	boolean := typedQuoteLayout(16, model.TypeBoolean)
	integer := typedQuoteLayout(23, model.TypeInt64)
	inner := recordOutputLayout(900101, boolean, text)
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	records := model.TypeLayout{Type: model.Type{ID: 900102, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{inner}}
	outer := recordOutputLayout(900103, inner, integers, records)
	function := recordOutputCallable(t, outer)
	if err := function.ValidateTypes([]model.Type{outer.Type}, typedQuoteLayout(2275, model.TypeString).Type); err != nil {
		t.Fatal(err)
	}
	wrong := outer.Type
	wrong.ID++
	requireReplacementSQLState(t, function.ValidateTypes([]model.Type{wrong}, typedQuoteLayout(2275, model.TypeString).Type), "42804")
	value := recordOutputValue(
		recordOutputValue(model.BoolVal(true), model.StringVal("a,b")),
		model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1), model.Int64Val(2)}, Dimensions: []model.ArrayDimension{{LowerBound: -2, Length: 2}}},
		model.Value{Type: model.TypeArray, ArrayVal: []model.Value{recordOutputValue(model.BoolVal(true), model.StringVal("a"))}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}},
	)
	requireRecordOutput(t, function, value, 1024, `("(t,""a,b"")","[-2:-1]={1,2}","{""(t,a)""}")`)
}

func TestRecordOutputUsesRawFieldOutput(t *testing.T) {
	layout := recordOutputLayout(2249, typedQuoteLayout(16, model.TypeBoolean), typedQuoteLayout(1042, model.TypeString), typedQuoteLayout(17, model.TypeBytes))
	function := recordOutputCallable(t, layout)
	value := recordOutputValue(model.BoolVal(true), model.StringVal("a  "), model.BytesVal([]byte{0, 255}))
	requireRecordOutput(t, function, value, 1024, `(t,"a  ","\\x00ff")`)
}

func TestRecordOutputOwnsMetadataAtItsExactBudget(t *testing.T) {
	integer := typedQuoteLayout(23, model.TypeInt64)
	inner := recordOutputLayout(900111, integer)
	array := model.TypeLayout{Type: model.Type{ID: 900112, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{inner}}
	layout := recordOutputLayout(900113, array, inner)
	function := recordOutputCallable(t, layout)
	result := typedQuoteLayout(2275, model.TypeString)
	_, err := ResolveTyped(t.Context(), 2291, []model.TypeLayout{layout}, result, ResolveOptions{}, function.OwnedBytes()-1)
	requireReplacementSQLState(t, err, "54000")
	exact, err := ResolveTyped(t.Context(), 2291, []model.TypeLayout{layout}, result, ResolveOptions{}, function.OwnedBytes())
	if err != nil {
		t.Fatal(err)
	}
	inner.Children[0] = typedQuoteLayout(25, model.TypeString)
	array.Children[0] = typedQuoteLayout(25, model.TypeString)
	layout.Children[1] = typedQuoteLayout(25, model.TypeString)
	value := recordOutputValue(
		model.Value{Type: model.TypeArray, ArrayVal: []model.Value{recordOutputValue(model.Int64Val(7))}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 1}}},
		recordOutputValue(model.Int64Val(8)),
	)
	requireRecordOutput(t, exact, value, 1024, `("{(7)}","(8)")`)
}

func TestRecordOutputAdmitsLiveFieldAndReplacementBytes(t *testing.T) {
	text := typedQuoteLayout(25, model.TypeString)
	for _, tc := range []struct {
		name   string
		layout model.TypeLayout
		value  model.Value
		budget int64
		want   string
	}{
		{"empty record", recordOutputLayout(2249), recordOutputValue(), 2, `()`},
		{"NULL fields", recordOutputLayout(2249, text, text, text), recordOutputValue(model.NullVal(model.TypeString), model.NullVal(model.TypeString), model.NullVal(model.TypeString)), 4, `(,,)`},
		{"empty text", recordOutputLayout(2249, text), recordOutputValue(model.StringVal("")), 6, "(\"\")"},
		{"live field and replacement", recordOutputLayout(2249, text), recordOutputValue(model.StringVal("abc")), 10, `(abc)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := recordOutputCallable(t, tc.layout)
			got := requireRecordOutput(t, function, tc.value, tc.budget, tc.want)
			bytes, err := model.ValueBytes(t.Context(), got, uint64(len(tc.want)))
			if err != nil || bytes != uint64(len(tc.want)) {
				t.Fatalf("owned result bytes = %d, %v; want %d", bytes, err, len(tc.want))
			}
			_, err = function.Call(t.Context(), []model.Value{tc.value}, CallBudget{OutputBytes: tc.budget - 1, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, "54000")
		})
	}
}

func TestRecordOutputOwnsPayloadAcrossGrowthAndLaterCalls(t *testing.T) {
	borrowed := []byte(strings.Repeat("a", 4095) + "\\\"é日")
	text := unsafe.String(unsafe.SliceData(borrowed), len(borrowed))
	fields := make([]model.TypeLayout, 80)
	values := make([]model.Value, len(fields))
	for i := range fields {
		fields[i] = typedQuoteLayout(25, model.TypeString)
		values[i] = model.StringVal(text)
	}
	function := recordOutputCallable(t, recordOutputLayout(2249, fields...))
	encoded := `"` + strings.Repeat("a", 4095) + `\\""é日"`
	want := "(" + strings.Repeat(encoded+",", len(values)-1) + encoded + ")"
	got := requireRecordOutput(t, function, recordOutputValue(values...), 4<<20, want)
	clear(borrowed)
	for i := range values {
		values[i] = model.NullVal(model.TypeString)
	}
	requireRecordOutput(t, function, recordOutputValue(values...), 81, "("+strings.Repeat(",", 79)+")")
	if got.StrVal != want {
		t.Fatal("record_out retained borrowed field storage or reused its result buffer")
	}
}

func TestRecordOutputRejectsMalformedValueLayouts(t *testing.T) {
	integer := typedQuoteLayout(23, model.TypeInt64)
	function := recordOutputCallable(t, recordOutputLayout(2249, integer))
	for _, tc := range []struct {
		name  string
		value model.Value
	}{
		{"wrong root storage", model.Value{Type: model.TypeArray}},
		{"missing field", recordOutputValue()},
		{"extra field", recordOutputValue(model.Int64Val(1), model.Int64Val(2))},
		{"record dimensions", model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(1)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}}},
		{"field storage", recordOutputValue(model.StringVal("1"))},
		{"NULL field storage", recordOutputValue(model.NullVal(model.TypeString))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{tc.value}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, "42804")
		})
	}
	array := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	nested := recordOutputCallable(t, recordOutputLayout(2249, array))
	_, err := nested.Call(t.Context(), []model.Value{recordOutputValue(model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(1)}})}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "2202E")
}

func TestRecordOutputRejectsInvalidFieldText(t *testing.T) {
	function := recordOutputCallable(t, recordOutputLayout(2249, typedQuoteLayout(25, model.TypeString)))
	for _, text := range []string{"a\x00b", "a\xffb", strings.Repeat("a", 4095) + "\xc3"} {
		_, err := function.Call(t.Context(), []model.Value{recordOutputValue(model.StringVal(text))}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		requireReplacementSQLState(t, err, "22021")
	}
}

func TestRecordOutputDefersUnusedFieldCodecFailures(t *testing.T) {
	field := typedQuoteLayout(900121, model.TypeString)
	function := recordOutputCallable(t, recordOutputLayout(2249, field))
	requireRecordOutput(t, function, recordOutputValue(model.NullVal(field.Type.Storage)), 2, `()`)
	_, err := function.Call(t.Context(), []model.Value{recordOutputValue(model.StringVal("unavailable"))}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "42804")
}

func TestRecordOutputRejectsInvalidConcreteMetadataBeforeDeferral(t *testing.T) {
	for _, field := range []model.TypeLayout{
		typedQuoteLayout(23, model.TypeString),
		typedQuoteLayout(2249, model.TypeString),
		{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{typedQuoteLayout(25, model.TypeString)}},
		{Type: model.Type{ID: 900122, Storage: model.TypeArray}},
		recordOutputLayout(900123, typedQuoteLayout(16, model.TypeString)),
	} {
		_, err := ResolveTyped(t.Context(), 2291, []model.TypeLayout{recordOutputLayout(2249, field)}, typedQuoteLayout(2275, model.TypeString), ResolveOptions{}, 1<<20)
		requireReplacementSQLState(t, err, "42804")
	}
}

func recordOutputCallbacks(callbacks ...scalarKernel) Callable {
	codec := &recordOutputCodec{fields: make([]recordOutputField, len(callbacks))}
	for i, callback := range callbacks {
		codec.fields[i] = recordOutputField{
			typ:      model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64},
			callable: Callable{signature: Signature{ArgumentCount: 1, Arguments: [8]model.TypeID{23}}, argumentStorage: [8]model.DataType{model.TypeInt64}, resultStorage: model.TypeString, kernel: callback},
		}
	}
	return Callable{signature: Signature{ID: 2291, Strict: true, ArgumentCount: 1, Arguments: [8]model.TypeID{2249}, Result: 2275}, argumentStorage: [8]model.DataType{model.TypeComposite}, resultStorage: model.TypeString, kernel: codec.output}
}

func TestRecordOutputCallsOnlyNonNullFieldsInOrder(t *testing.T) {
	integer, err := Resolve(43, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var calls []int
	callbacks := make([]scalarKernel, 4)
	for i := range callbacks {
		callbacks[i] = func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			calls = append(calls, i)
			return integer.Call(ctx, arguments, CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
		}
	}
	function := recordOutputCallbacks(callbacks...)
	requireRecordOutput(t, function, recordOutputValue(model.Int64Val(1), model.NullVal(model.TypeInt64), model.Int64Val(3), model.NullVal(model.TypeInt64)), 1024, `(1,,3,)`)
	if !slices.Equal(calls, []int{0, 2}) {
		t.Fatalf("field callbacks = %v; want [0 2]", calls)
	}
}

func TestRecordOutputStopsAtFirstFieldFailure(t *testing.T) {
	integer, err := Resolve(43, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	failure := &model.Error{Code: "22003", Message: "field output failed"}
	var calls []int
	callbacks := make([]scalarKernel, 3)
	for i := range callbacks {
		callbacks[i] = func(ctx context.Context, arguments []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			calls = append(calls, i)
			if i == 1 {
				return model.Value{}, failure
			}
			return integer.Call(ctx, arguments, CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
		}
	}
	_, err = recordOutputCallbacks(callbacks...).Call(t.Context(), []model.Value{recordOutputValue(model.Int64Val(1), model.Int64Val(2), model.Int64Val(3))}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	if !errors.Is(err, failure) || !slices.Equal(calls, []int{0, 1}) {
		t.Fatalf("field failure = %v, callbacks %v; want selected error and [0 1]", err, calls)
	}
}

func TestRecordOutputRejectsMalformedCallbackResults(t *testing.T) {
	for _, tc := range []struct {
		name, code string
		result     model.Value
	}{
		{"NULL cstring", "XX000", model.NullVal(model.TypeString)},
		{"non cstring", "XX000", model.Int64Val(1)},
		{"oversized cstring", "54000", model.StringVal(strings.Repeat("a", 1024))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := recordOutputCallbacks(func(context.Context, []model.Value, CallBudget) (model.Value, error) {
				return tc.result, nil
			})
			_, err := function.Call(t.Context(), []model.Value{recordOutputValue(model.Int64Val(1))}, CallBudget{OutputBytes: 32, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, tc.code)
		})
	}
}

func TestRecordOutputCancellationStopsFieldCallbacks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	function := recordOutputCallbacks(
		func(context.Context, []model.Value, CallBudget) (model.Value, error) {
			cancel()
			return model.StringVal("done"), nil
		},
		func(context.Context, []model.Value, CallBudget) (model.Value, error) {
			t.Fatal("field output ran after cancellation")
			return model.Value{}, nil
		},
	)
	_, err := function.Call(ctx, []model.Value{recordOutputValue(model.Int64Val(1), model.Int64Val(2))}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled record_out = %v", err)
	}
	_, err = ResolveTyped(ctx, 2291, []model.TypeLayout{recordOutputLayout(2249)}, typedQuoteLayout(2275, model.TypeString), ResolveOptions{}, 1<<20)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled record output resolution = %v", err)
	}
}

func TestRecordOutputCancelsLongTextAndNullFieldScans(t *testing.T) {
	text := typedQuoteLayout(25, model.TypeString)
	nullFields := make([]model.TypeLayout, 128)
	nullValues := make([]model.Value, len(nullFields))
	for i := range nullFields {
		nullFields[i] = text
		nullValues[i] = model.NullVal(model.TypeString)
	}
	for _, tc := range []struct {
		name   string
		layout model.TypeLayout
		value  model.Value
	}{
		{"long text", recordOutputLayout(2249, text), recordOutputValue(model.StringVal(strings.Repeat("a\\\"é", 32768)))},
		{"NULL fields", recordOutputLayout(2249, nullFields...), recordOutputValue(nullValues...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := recordOutputCallable(t, tc.layout)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := &arrayPositionCancelContext{Context: ctx, cancel: cancel, remaining: 20}
			_, err := function.Call(interrupted, []model.Value{tc.value}, CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted record_out = %v", err)
			}
		})
	}
}
