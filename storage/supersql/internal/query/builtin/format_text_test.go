package builtin

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func formatTextCallable(t *testing.T, arguments ...model.TypeLayout) Callable {
	t.Helper()
	text := typedQuoteLayout(25, model.TypeString)
	inputs := append([]model.TypeLayout{text}, arguments...)
	function, err := ResolveTyped(t.Context(), 3539, inputs, text, ResolveOptions{Collation: 950, DefaultCollation: 950}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func TestFormatPositionsWidthsAndPercentLiterals(t *testing.T) {
	function := formatTextCallable(t, typedQuoteLayout(23, model.TypeInt64), typedQuoteLayout(25, model.TypeString), typedQuoteLayout(25, model.TypeString))
	for _, tc := range []struct{ format, want string }{
		{"%2$s|%1$s|%3$s", "é|5|z"},
		{"[%2$*1$s][%s]", "[    é][z]"},
		{"[%*s]", "[    é]"},
		{"[%--*s]", "[é    ]"},
		{"%3$s %2$s", "z é"},
		{"%2$s %% %s", "é % z"},
		{"[%05s]", "[    5]"},
	} {
		t.Run(tc.format, func(t *testing.T) {
			got, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.format), model.Int64Val(5), model.StringVal("é"), model.StringVal("z")}, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
			if err != nil || got.IsNull || got.StrVal != tc.want {
				t.Fatalf("format=%v,%v want %q", got, err, tc.want)
			}
		})
	}
}

func TestFormatWidthTextUsesIntegerInputAndRawTypeOutput(t *testing.T) {
	function := formatTextCallable(t, typedQuoteLayout(25, model.TypeString), typedQuoteLayout(16, model.TypeBoolean))
	for _, tc := range []struct{ width, want string }{{"-3", "[t  ]"}, {"0x3", "[  t]"}, {" 3 ", "[  t]"}} {
		got, err := function.Call(t.Context(), []model.Value{model.StringVal("[%*s]"), model.StringVal(tc.width), model.BoolVal(true)}, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
		if err != nil || got.StrVal != tc.want {
			t.Fatalf("width %q=%v,%v want %q", tc.width, got, err, tc.want)
		}
	}
}

func TestFormatQuotesAfterRawOutputConversion(t *testing.T) {
	function := formatTextCallable(t, typedQuoteLayout(25, model.TypeString), typedQuoteLayout(1042, model.TypeString))
	got, err := function.Call(t.Context(), []model.Value{model.StringVal("%1$I|%2$L"), model.StringVal(`a"b`), model.StringVal("x  ")}, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	if err != nil || got.StrVal != `"a""b"|'x  '` {
		t.Fatalf("quoted format=%v,%v", got, err)
	}
}

func TestFormatNullIdentifierPrecedesMinimumWidthOverflow(t *testing.T) {
	function := formatTextCallable(t, typedQuoteLayout(23, model.TypeInt64), typedQuoteLayout(25, model.TypeString))
	values := []model.Value{model.StringVal("%*I"), model.Int64Val(math.MinInt32), model.NullVal(model.TypeString)}
	_, err := function.Call(t.Context(), values, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "22004")
	values[0], values[2] = model.StringVal("%*s"), model.StringVal("x")
	_, err = function.Call(t.Context(), values, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "22003")
}

func TestFormatChecksSpecifierBeforeWidthAndWidthBeforeValue(t *testing.T) {
	function := formatTextCallable(t, typedQuoteLayout(25, model.TypeString))
	values := []model.Value{model.StringVal("%*Q"), model.StringVal("bad")}
	_, err := function.Call(t.Context(), values, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "22023")
	values[0] = model.StringVal("%*s")
	_, err = function.Call(t.Context(), values, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "22P02")
}

func TestFormatRejectsBrokenAndOverflowingDirectives(t *testing.T) {
	function, err := Resolve(3540, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ format, state string }{
		{"%", "22023"}, {"%0$s", "22023"}, {"%*0$s", "22023"}, {"%*12s", "22023"}, {"%1$%", "22023"}, {"%2147483648s", "22003"}, {"%2147483648", "22003"}, {"%3$s", "22023"},
	} {
		_, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.format)}, CallBudget{OutputBytes: 4096, MemoryBytes: 64 << 20})
		requireReplacementSQLState(t, err, tc.state)
	}
}

func TestFormatLiteralAndNullNeedOnlyTheirActualPayload(t *testing.T) {
	function, err := Resolve(3540, ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := function.Call(t.Context(), []model.Value{model.StringVal("literal")}, CallBudget{OutputBytes: 7, MemoryBytes: 64 << 20})
	if err != nil || got.StrVal != "literal" {
		t.Fatalf("literal=%v,%v", got, err)
	}
	null, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeString)}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !null.IsNull {
		t.Fatalf("NULL format=%v,%v", null, err)
	}
}

func TestFormatAccountsForConvertedOutputOverlap(t *testing.T) {
	function := formatTextCallable(t, typedQuoteLayout(23, model.TypeInt64))
	arguments := []model.Value{model.StringVal("%s"), model.Int64Val(123)}
	got, err := function.Call(t.Context(), arguments, CallBudget{OutputBytes: 6, MemoryBytes: 64 << 20})
	if err != nil || got.StrVal != "123" {
		t.Fatalf("exact output overlap=%v,%v", got, err)
	}
	_, err = function.Call(t.Context(), arguments, CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
	requireReplacementSQLState(t, err, "54000")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = function.Call(ctx, arguments, CallBudget{OutputBytes: 6, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled format=%v", err)
	}
}

func TestFormatRejectsInvalidConcreteArgumentStorageBeforeDeferral(t *testing.T) {
	text := typedQuoteLayout(25, model.TypeString)
	_, err := ResolveTyped(t.Context(), 3539, []model.TypeLayout{text, typedQuoteLayout(23, model.TypeFloat64)}, text, ResolveOptions{}, 1<<20)
	requireReplacementSQLState(t, err, "42804")
}
