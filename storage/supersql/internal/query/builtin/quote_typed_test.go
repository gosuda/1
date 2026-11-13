package builtin

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func typedQuoteLayout(id model.TypeID, storage model.DataType) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: id, Storage: storage, Modifier: -1}}
}

func typedQuoteCallable(t *testing.T, id model.FunctionID, argument model.TypeLayout, options ResolveOptions) Callable {
	t.Helper()
	function, err := ResolveTyped(t.Context(), id, []model.TypeLayout{argument}, typedQuoteLayout(25, model.TypeString), options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireTypedQuote(t *testing.T, function Callable, input model.Value, budget int64, want string) model.Value {
	t.Helper()
	got, err := function.Call(t.Context(), []model.Value{input}, CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
	if err != nil || got.IsNull || got.Type != model.TypeString || got.StrVal != want {
		t.Fatalf("quote = %v, %v; want %q", got, err, want)
	}
	return got
}

func TestTypedQuoteUsesSQLTextCast(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.TypeID
		input model.Value
		want  string
	}{
		{"boolean true", 16, model.BoolVal(true), "'true'"},
		{"boolean false", 16, model.BoolVal(false), "'false'"},
		{"bpchar trims only ASCII spaces", 1042, model.StringVal("a\u00a0  "), "'a\u00a0'"},
		{"varchar preserves spaces", 1043, model.StringVal("a  "), "'a  '"},
		{"name selected text cast", 19, model.StringVal("O'Reilly"), "'O''Reilly'"},
		{"signed integer output", 20, model.Int64Val(-9223372036854775808), "'-9223372036854775808'"},
		{"numeric precision and scale", 1700, model.Value{Type: model.TypeNumeric, StrVal: "9007199254740993.00"}, "'9007199254740993.00'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, id := range []model.FunctionID{1285, 1290} {
				function := typedQuoteCallable(t, id, typedQuoteLayout(tc.id, tc.input.Type), ResolveOptions{})
				requireTypedQuote(t, function, tc.input, 1<<20, tc.want)
			}
		})
	}
}

func TestTypedQuoteUsesFullArrayOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		element model.TypeID
		storage model.DataType
		input   model.Value
		want    string
	}{
		{"boolean element output", 16, model.TypeBoolean, model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.BoolVal(true), model.BoolVal(false), model.NullVal(model.TypeBoolean)}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 3}}}, "'{t,f,NULL}'"},
		{"bpchar padding retained by element output", 1042, model.TypeString, model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.StringVal("a  "), model.NullVal(model.TypeString)}, Dimensions: []model.ArrayDimension{{LowerBound: 1, Length: 2}}}, `'{"a  ",NULL}'`},
		{"nondefault bounds", 23, model.TypeInt64, model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(7), model.NullVal(model.TypeInt64)}, Dimensions: []model.ArrayDimension{{LowerBound: -1, Length: 2}}}, "'[-1:0]={7,NULL}'"},
		{"empty array", 23, model.TypeInt64, model.Value{Type: model.TypeArray}, "'{}'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			layout := replacementLayout(t, tc.element, tc.storage)
			function := typedQuoteCallable(t, 1285, layout, ResolveOptions{})
			layout.Children[0] = typedQuoteLayout(25, model.TypeString)
			requireTypedQuote(t, function, tc.input, 1<<20, tc.want)
		})
	}
}

func TestTypedQuoteNullSkipsCoercion(t *testing.T) {
	layout := replacementLayout(t, 23, model.TypeInt64)
	input := model.NullVal(model.TypeArray)
	input.ArrayVal = []model.Value{model.StringVal("invalid ignored shape")}
	strict := typedQuoteCallable(t, 1285, layout, ResolveOptions{})
	got, err := strict.Call(t.Context(), []model.Value{input}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !got.IsNull || got.Type != model.TypeString {
		t.Fatalf("strict NULL quote = %v, %v", got, err)
	}
	nullable := typedQuoteCallable(t, 1290, layout, ResolveOptions{})
	first := requireTypedQuote(t, nullable, input, 4, "NULL")
	second := requireTypedQuote(t, nullable, input, 4, "NULL")
	if unsafe.StringData(first.StrVal) == unsafe.StringData(second.StrVal) {
		t.Fatal("nullable NULL results share storage")
	}
	_, err = nullable.Call(t.Context(), []model.Value{input}, CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "54000")
}

func TestTypedQuoteAccountsForConversionOverlap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     model.TypeID
		input  model.Value
		budget int64
		want   string
	}{
		{"explicit bool cast", 16, model.BoolVal(true), 10, "'true'"},
		{"output and text input", 23, model.Int64Val(123), 8, "'123'"},
		{"escaped cast result", 1042, model.StringVal("a'b\\  "), 13, `E'a''b\\'`},
		{"trimmed cast result", 1042, model.StringVal("x" + strings.Repeat(" ", 8192)), 4, "'x'"},
		{"binary cast borrows input", 1043, model.StringVal("abc"), 5, "'abc'"},
		{"text identity borrows input", 25, model.StringVal("abc"), 5, "'abc'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := typedQuoteCallable(t, 1285, typedQuoteLayout(tc.id, tc.input.Type), ResolveOptions{})
			requireTypedQuote(t, function, tc.input, tc.budget, tc.want)
			_, err := function.Call(t.Context(), []model.Value{tc.input}, CallBudget{OutputBytes: tc.budget - 1, MemoryBytes: 64 << 20})
			requireReplacementSQLState(t, err, "54000")
		})
	}
}

func TestTypedQuoteOwnsBorrowedBinaryInput(t *testing.T) {
	function := typedQuoteCallable(t, 1285, typedQuoteLayout(1043, model.TypeString), ResolveOptions{})
	buffer := []byte("a'b")
	input := model.StringVal(unsafe.String(unsafe.SliceData(buffer), len(buffer)))
	first := requireTypedQuote(t, function, input, 6, "'a''b'")
	buffer[0] = 'x'
	requireTypedQuote(t, function, input, 6, "'x''b'")
	if first.StrVal != "'a''b'" {
		t.Fatalf("prior result changed with borrowed input: %q", first.StrVal)
	}
}

func TestTypedQuoteRetainsBoundIdentityAndMetadataBudget(t *testing.T) {
	layout := replacementLayout(t, 16, model.TypeBoolean)
	result := typedQuoteLayout(25, model.TypeString)
	function := typedQuoteCallable(t, 1290, layout, ResolveOptions{})
	if err := function.ValidateTypes([]model.Type{layout.Type}, result.Type); err != nil {
		t.Fatal(err)
	}
	spoof := layout.Type
	spoof.ID = 2283
	requireReplacementSQLState(t, function.ValidateTypes([]model.Type{spoof}, result.Type), "42804")
	_, err := function.Call(t.Context(), []model.Value{{Type: model.TypeAny}}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "42804")
	_, err = ResolveTyped(t.Context(), 1290, []model.TypeLayout{layout}, result, ResolveOptions{}, function.OwnedBytes()-1)
	requireReplacementSQLState(t, err, "54000")
	exact, err := ResolveTyped(t.Context(), 1290, []model.TypeLayout{layout}, result, ResolveOptions{}, function.OwnedBytes())
	if err != nil {
		t.Fatal(err)
	}
	requireTypedQuote(t, exact, model.Value{Type: model.TypeArray}, 6, "'{}'")
}

func TestTypedQuoteRejectsMalformedLayouts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		input  model.TypeLayout
		result model.TypeLayout
	}{
		{"raw anyelement", typedQuoteLayout(2283, model.TypeString), typedQuoteLayout(25, model.TypeString)},
		{"unknown identity", typedQuoteLayout(999999, model.TypeString), typedQuoteLayout(25, model.TypeString)},
		{"boolean string spoof", typedQuoteLayout(16, model.TypeString), typedQuoteLayout(25, model.TypeString)},
		{"array string spoof", typedQuoteLayout(1007, model.TypeString), typedQuoteLayout(25, model.TypeString)},
		{"wrong array child", replacementLayout(t, 23, model.TypeString), typedQuoteLayout(25, model.TypeString)},
		{"absent array child", typedQuoteLayout(1007, model.TypeArray), typedQuoteLayout(25, model.TypeString)},
		{"text result spoof", typedQuoteLayout(16, model.TypeBoolean), typedQuoteLayout(25, model.TypeInt64)},
		{"cstring result", typedQuoteLayout(16, model.TypeBoolean), typedQuoteLayout(2275, model.TypeString)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveTyped(t.Context(), 1285, []model.TypeLayout{tc.input}, tc.result, ResolveOptions{}, 1<<20)
			requireReplacementSQLState(t, err, "42804")
		})
	}
}

func typedQuoteConversion(storage model.DataType, kernel scalarKernel) Callable {
	return Callable{
		signature: Signature{ArgumentCount: 1}, argumentStorage: [8]model.DataType{storage},
		resultStorage: model.TypeString, kernel: kernel,
	}
}

func TestTypedQuoteInvokesRequiredConversionsOnce(t *testing.T) {
	for _, useInput := range []bool{false, true} {
		calls, inputCalls := 0, 0
		wantInputCalls := 0
		codec := quoteTypedCodec{conversion: typedQuoteConversion(model.TypeInt64, func(ctx context.Context, _ []model.Value, callBudget CallBudget) (model.Value, error) {
			budget := callBudget.OutputBytes
			calls++
			return ownedText(ctx, budget, "a'b")
		})}
		if useInput {
			wantInputCalls = 1
			codec.input = typedQuoteConversion(model.TypeString, func(ctx context.Context, args []model.Value, callBudget CallBudget) (model.Value, error) {
				budget := callBudget.OutputBytes
				inputCalls++
				return ownedText(ctx, budget, args[0].StrVal)
			})
		}
		got, err := codec.call(t.Context(), []model.Value{model.Int64Val(7)}, CallBudget{OutputBytes: 9, MemoryBytes: 64 << 20})
		if err != nil || got.StrVal != "'a''b'" || calls != 1 || inputCalls != wantInputCalls {
			t.Fatalf("quote via input=%v: %v, %v, conversion calls=%d input calls=%d", useInput, got, err, calls, inputCalls)
		}
		codec.nullable = true
		got, err = codec.call(t.Context(), []model.Value{model.NullVal(model.TypeInt64)}, CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
		if err != nil || got.StrVal != "NULL" || calls != 1 || inputCalls != wantInputCalls {
			t.Fatalf("NULL invoked conversion: %v, %v, calls=%d input calls=%d", got, err, calls, inputCalls)
		}
	}
}

func TestTypedQuotePreservesSelectedConversionFailures(t *testing.T) {
	failure := &model.Error{Code: "22003", Message: "selected conversion failed"}
	for _, tc := range []struct {
		failInput      bool
		wantInputCalls int
	}{{false, 0}, {true, 1}} {
		conversionCalls, inputCalls := 0, 0
		codec := quoteTypedCodec{
			conversion: typedQuoteConversion(model.TypeInt64, func(ctx context.Context, _ []model.Value, callBudget CallBudget) (model.Value, error) {
				budget := callBudget.OutputBytes
				conversionCalls++
				if !tc.failInput {
					return model.Value{}, failure
				}
				return ownedText(ctx, budget, "abc")
			}),
			input: typedQuoteConversion(model.TypeString, func(context.Context, []model.Value, CallBudget) (model.Value, error) {
				inputCalls++
				return model.Value{}, failure
			}),
		}
		_, err := codec.call(t.Context(), []model.Value{model.Int64Val(7)}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
		if !errors.Is(err, failure) || conversionCalls != 1 || inputCalls != tc.wantInputCalls {
			t.Fatalf("failure via input=%v: %v, conversion calls=%d input calls=%d", tc.failInput, err, conversionCalls, inputCalls)
		}
	}
}

func TestTypedQuoteCancellationStopsBeforeTextInput(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	codec := quoteTypedCodec{
		conversion: typedQuoteConversion(model.TypeInt64, func(context.Context, []model.Value, CallBudget) (model.Value, error) {
			cancel()
			return model.StringVal("abc"), nil
		}),
		input: typedQuoteConversion(model.TypeString, func(context.Context, []model.Value, CallBudget) (model.Value, error) {
			t.Fatal("text input ran after cancellation")
			return model.Value{}, nil
		}),
	}
	_, err := codec.call(ctx, []model.Value{model.Int64Val(7)}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled conversion = %v", err)
	}
}

func TestTypedQuoteRejectsMalformedConversionResults(t *testing.T) {
	for _, value := range []model.Value{model.Int64Val(7), model.NullVal(model.TypeString)} {
		codec := quoteTypedCodec{
			conversion: typedQuoteConversion(model.TypeInt64, func(context.Context, []model.Value, CallBudget) (model.Value, error) {
				return value, nil
			}),
			input: typedQuoteConversion(model.TypeString, func(context.Context, []model.Value, CallBudget) (model.Value, error) {
				t.Fatal("text input ran after malformed output")
				return model.Value{}, nil
			}),
		}
		_, err := codec.call(t.Context(), []model.Value{model.Int64Val(7)}, CallBudget{OutputBytes: 1024, MemoryBytes: 64 << 20})
		requireReplacementSQLState(t, err, "XX000")
	}
}

func TestTypedQuoteOutputOptionsApplyOnlyToSelectedConversion(t *testing.T) {
	options := ResolveOptions{ExtraFloatDigits: 4}
	boolean := typedQuoteCallable(t, 1285, typedQuoteLayout(16, model.TypeBoolean), options)
	requireTypedQuote(t, boolean, model.BoolVal(true), 10, "'true'")
	_, err := ResolveTyped(t.Context(), 1285, []model.TypeLayout{typedQuoteLayout(701, model.TypeFloat64)}, typedQuoteLayout(25, model.TypeString), options, 1<<20)
	requireReplacementSQLState(t, err, "22023")
}
