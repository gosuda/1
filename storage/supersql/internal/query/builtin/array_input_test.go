package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func arrayInputCallable(t testing.TB, id model.TypeID, storage model.DataType, options builtin.ResolveOptions) builtin.Callable {
	t.Helper()
	info, ok := builtin.LookupType(id)
	if !ok {
		t.Fatalf("missing element type %d", id)
	}
	layout := model.TypeLayout{
		Type:     model.Type{ID: info.Array, Storage: model.TypeArray, Modifier: -1},
		Children: []model.TypeLayout{{Type: model.Type{ID: id, Storage: storage, Modifier: -1}}},
	}
	arguments := []model.TypeLayout{
		{Type: model.Type{ID: 2275, Storage: model.TypeString, Modifier: -1}},
		{Type: model.Type{ID: 26, Storage: model.TypeInt64, Modifier: -1}},
		{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}},
	}
	function, err := builtin.ResolveTyped(t.Context(), 750, arguments, layout, options, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func arrayInputArguments(input string, id model.TypeID, typmod int64) []model.Value {
	return []model.Value{model.StringVal(input), model.Int64Val(int64(id)), model.Int64Val(typmod)}
}

func TestArrayInputLexing(t *testing.T) {
	function := arrayInputCallable(t, 25, model.TypeString, builtin.ResolveOptions{})
	for _, tc := range []struct {
		name, input string
		want        []string
	}{
		{"quoted empty and punctuation", `{"","a,b","{x}","a\"b","c\\d"}`, []string{"", "a,b", "{x}", `a"b`, `c\d`}},
		{"unquoted escapes", `{a\,b,\{x\},c\\d,\"q\"}`, []string{"a,b", "{x}", `c\d`, `"q"`}},
		{"ASCII whitespace", " \t{ \ta \r\n, b\t c \v,\" d \" \f} \n", []string{"a", "b\t c", " d "}},
		{"escaped edge whitespace", `{\ a\ ,a\  ,\ ,x\	y}`, []string{" a ", "a ", " ", "x\ty"}},
		{"Unicode whitespace is data", "{\u00a0a\u00a0,\u2003}", []string{"\u00a0a\u00a0", "\u2003"}},
		{"quoted NULL is text", `{"NULL",N\ULL,"null",NULLx}`, []string{"NULL", "NULL", "null", "NULLx"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), arrayInputArguments(tc.input, 25, -1), builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if len(value.ArrayVal) != len(tc.want) {
				t.Fatalf("element count = %d, want %d", len(value.ArrayVal), len(tc.want))
			}
			for i, want := range tc.want {
				got := value.ArrayVal[i]
				if got.Type != model.TypeString || got.IsNull || got.StrVal != want {
					t.Errorf("element %d = %v, want text %q", i, got, want)
				}
			}
		})
	}
}

func TestArrayInputNullSettingIsCaptured(t *testing.T) {
	options := builtin.ResolveOptions{}
	withNulls := arrayInputCallable(t, 25, model.TypeString, options)
	options.ArrayNullsDisabled = true
	withoutNulls := arrayInputCallable(t, 25, model.TypeString, options)
	options.ArrayNullsDisabled = false
	for _, tc := range []struct {
		name     string
		function builtin.Callable
		nulls    bool
	}{
		{"enabled", withNulls, true}, {"disabled", withoutNulls, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := tc.function.Call(t.Context(), arrayInputArguments(`{NULL,nUlL,"NULL",N\ULL}`, 25, -1), builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if len(value.ArrayVal) != 4 {
				t.Fatalf("element count = %d, want 4", len(value.ArrayVal))
			}
			for i, text := range []string{"NULL", "nUlL", "NULL", "NULL"} {
				got := value.ArrayVal[i]
				wantNull := tc.nulls && i < 2
				wrongType := got.Type != model.TypeString || got.IsNull != wantNull
				wrongText := !wantNull && got.StrVal != text
				if wrongType || wrongText {
					t.Errorf("element %d = %v, want null=%v text=%q", i, got, wantNull, text)
				}
			}
		})
	}
}

func TestArrayInputPreservesShapeAndBounds(t *testing.T) {
	function := arrayInputCallable(t, 23, model.TypeInt64, builtin.ResolveOptions{})
	for _, tc := range []struct {
		name, input string
		dimensions  []model.ArrayDimension
		values      []int64
	}{
		{"inferred matrix", `{{1,2},{3,4}}`, []model.ArrayDimension{{LowerBound: 1, Length: 2}, {LowerBound: 1, Length: 2}}, []int64{1, 2, 3, 4}},
		{"explicit signed bounds", ` [-2:-1] [+4:5] = {{1,2},{3,4}} `, []model.ArrayDimension{{LowerBound: -2, Length: 2}, {LowerBound: 4, Length: 2}}, []int64{1, 2, 3, 4}},
		{"upper-only dimension", `[2]={1,2}`, []model.ArrayDimension{{LowerBound: 1, Length: 2}}, []int64{1, 2}},
		{"minimum bound", `[-2147483648:-2147483648]={1}`, []model.ArrayDimension{{LowerBound: math.MinInt32, Length: 1}}, []int64{1}},
		{"maximum valid bound", `[2147483646:2147483646]={1}`, []model.ArrayDimension{{LowerBound: math.MaxInt32 - 1, Length: 1}}, []int64{1}},
		{"six dimensions", `{{{{{{1}}}}}}`, []model.ArrayDimension{{LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}, {LowerBound: 1, Length: 1}}, []int64{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), arrayInputArguments(tc.input, 23, -1), builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(value.Dimensions, tc.dimensions) {
				t.Errorf("dimensions = %v, want %v", value.Dimensions, tc.dimensions)
			}
			if len(value.ArrayVal) != len(tc.values) {
				t.Fatalf("element count = %d, want %d", len(value.ArrayVal), len(tc.values))
			}
			for i, want := range tc.values {
				if got := value.ArrayVal[i]; got.IsNull || got.Type != model.TypeInt64 || got.IntVal != want {
					t.Errorf("element %d = %v, want %d", i, got, want)
				}
			}
		})
	}
}

func TestArrayInputNormalizesEmptySubarrays(t *testing.T) {
	function := arrayInputCallable(t, 23, model.TypeInt64, builtin.ResolveOptions{})
	for _, input := range []string{`{}`, `{ }`, `{{}}`, `{{},{}}`, `{{{{{{}}}}}}`} {
		value, err := function.Call(t.Context(), arrayInputArguments(input, 23, -1), builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || value.Type != model.TypeArray || value.IsNull || len(value.ArrayVal) != 0 || len(value.Dimensions) != 0 {
			t.Errorf("input %q = %v, %v; want nonnull dimensionless empty array", input, value, err)
		}
	}
	value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeString), model.Int64Val(23), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.Type != model.TypeArray || !value.IsNull {
		t.Fatalf("NULL input = %v, %v; want NULL array", value, err)
	}
}

func TestArrayInputRejectsMalformedShapeAndLexing(t *testing.T) {
	function := arrayInputCallable(t, 23, model.TypeInt64, builtin.ResolveOptions{})
	for _, tc := range []struct{ name, input, code string }{
		{"missing opening brace", `1,2}`, "22P02"},
		{"missing end brace", `{1`, "22P02"},
		{"leading delimiter", `{,1}`, "22P02"},
		{"trailing delimiter", `{1,}`, "22P02"},
		{"duplicate delimiter", `{1,,2}`, "22P02"},
		{"trailing junk", `{1}x`, "22P02"},
		{"unclosed quote", `{"1}`, "22P02"},
		{"embedded quote", `{1"2"}`, "22P02"},
		{"characters after quote", `{"1"2}`, "22P02"},
		{"unclosed escape", `{1\`, "22P02"},
		{"nonrectangular rows", `{{1},{2,3}}`, "22P02"},
		{"mixed depths", `{{1},2}`, "22P02"},
		{"empty and nonempty rows", `{{},{1}}`, "22P02"},
		{"missing row delimiter", `{{1}{2}}`, "22P02"},
		{"missing dimension assignment", `[1]{1}`, "22P02"},
		{"dimension contents mismatch", `[2]={1}`, "22P02"},
		{"dimension rank mismatch", `[1]={{1}}`, "22P02"},
		{"dimension missing number", `[]={1}`, "22P02"},
		{"dimension missing upper", `[1:]={1}`, "22P02"},
		{"dimension leading space", `[ 1]={1}`, "22P02"},
		{"dimension trailing space", `[1 ]={1}`, "22P02"},
		{"dimension space after colon", `[1: 1]={1}`, "22P02"},
		{"dimension sign without digits", `[+]={1}`, "22P02"},
		{"zero-length explicit bound", `[1:0]={}`, "2202E"},
		{"reversed bounds", `[2:1]={}`, "2202E"},
		{"bound above int32", `[2147483648]={1}`, "54000"},
		{"bound below int32", `[-2147483649:0]={1}`, "54000"},
		{"one-past bound overflow", `[2147483647:2147483647]={1}`, "54000"},
		{"dimension length overflow", `[-2147483648:0]={1}`, "54000"},
		{"seven nested dimensions", `{{{{{{{1}}}}}}}`, "54000"},
		{"seven explicit dimensions", `[1][1][1][1][1][1][1]={1}`, "54000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), arrayInputArguments(tc.input, 23, -1), builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestArrayInputPreservesPostgresErrorOrder(t *testing.T) {
	function := arrayInputCallable(t, 23, model.TypeInt64, builtin.ResolveOptions{})
	for _, tc := range []struct{ name, input, code string }{
		{"unterminated token before element conversion", `{2147483648`, "22P02"},
		{"delimited token before missing array end", `{2147483648,`, "22003"},
		{"unquoted brace before element conversion", `{2147483648{}`, "22P02"},
		{"quote suffix before element conversion", `{"2147483648"x}`, "22P02"},
		{"quoted token before unexpected brace", `{"2147483648"{}}`, "22003"},
		{"element conversion before shallow dimension mismatch", `{{1},2147483648}`, "22003"},
		{"element conversion before row length mismatch", `{{1},{2,2147483648}}`, "22003"},
		{"frozen rank before nested element conversion", `{1,{2147483648}}`, "22P02"},
		{"explicit dimension bound before element conversion", `[1:0]={2147483648}`, "2202E"},
		{"element conversion before explicit length mismatch", `[1]={1,2147483648}`, "22003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), arrayInputArguments(tc.input, 23, -1), builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.code)
		})
	}
}

func TestArrayInputExecutesPrimitiveCodecs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.TypeID
		input string
		want  model.Value
	}{
		{"boolean", 16, `{yes}`, model.BoolVal(true)},
		{"int2", 21, `{-32768}`, model.Int64Val(-32768)},
		{"int4", 23, `{0x7fff_ffff}`, model.Int64Val(2147483647)},
		{"int8", 20, `{-9223372036854775808}`, model.Int64Val(math.MinInt64)},
		{"float4", 700, `{0.1}`, model.Float64Val(float64(float32(0.1)))},
		{"float8", 701, `{1.25e2}`, model.Float64Val(125)},
		{"bytea", 17, `{"\\x0041ff"}`, model.BytesVal([]byte{0, 65, 255})},
		{"text", 25, `{é}`, model.StringVal("é")},
		{"bpchar", 1042, `{"a "}`, model.StringVal("a ")},
		{"varchar", 1043, `{"a "}`, model.StringVal("a ")},
		{"numeric", 1700, `{123.450}`, model.Value{Type: model.TypeNumeric, StrVal: "123.450"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayInputCallable(t, tc.id, tc.want.Type, builtin.ResolveOptions{})
			value, err := function.Call(t.Context(), arrayInputArguments(tc.input, tc.id, -1), builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if len(value.ArrayVal) != 1 {
				t.Fatalf("element count = %d, want 1", len(value.ArrayVal))
			}
			got := value.ArrayVal[0]
			wrongType := got.Type != tc.want.Type || got.IsNull
			wrongNumber := got.IntVal != tc.want.IntVal || got.FloatVal != tc.want.FloatVal
			wrongText := got.StrVal != tc.want.StrVal || !bytes.Equal(got.BytesVal, tc.want.BytesVal)
			if wrongType || wrongNumber || wrongText {
				t.Fatalf("element = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestArrayInputAppliesElementTypmod(t *testing.T) {
	for _, tc := range []struct {
		name       string
		id         model.TypeID
		storage    model.DataType
		input      string
		typmod     int64
		want, code string
	}{
		{"bpchar pads", 1042, model.TypeString, `{é}`, 8, "é   ", ""},
		{"varchar trims excess spaces", 1043, model.TypeString, `{"é日  "}`, 6, "é日", ""},
		{"varchar rejects excess characters", 1043, model.TypeString, `{é日x}`, 6, "", "22001"},
		{"numeric rounds", 1700, model.TypeNumeric, `{1.235}`, (5<<16 | 2) + 4, "1.24", ""},
		{"numeric rounding overflows precision", 1700, model.TypeNumeric, `{999.995}`, (5<<16 | 2) + 4, "", "22003"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayInputCallable(t, tc.id, tc.storage, builtin.ResolveOptions{})
			value, err := function.Call(t.Context(), arrayInputArguments(tc.input, tc.id, tc.typmod), builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if tc.code != "" {
				requireSQLState(t, err, tc.code)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(value.ArrayVal) != 1 || value.ArrayVal[0].IsNull || value.ArrayVal[0].StrVal != tc.want {
				t.Fatalf("array = %v, want single element %q", value.ArrayVal, tc.want)
			}
		})
	}
}

func TestArrayInputExactPayloadBudget(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		id                   model.TypeID
		storage              model.DataType
		input                string
		count, rank, payload int64
	}{
		{"fixed-width", 23, model.TypeInt64, `{1,2,3}`, 3, 1, 0},
		{"quoted escaped fixed-width", 23, model.TypeInt64, `{"\1","\2","\3"}`, 3, 1, 0},
		{"large escaped fixed-width token", 23, model.TypeInt64, `{"` + strings.Repeat(`\0`, 10000) + `"}`, 1, 1, 0},
		{"all null", 25, model.TypeString, `{NULL,NULL,NULL}`, 3, 1, 0},
		{"null and escaped text", 25, model.TypeString, `{"a\\b",NULL,é}`, 3, 1, 5},
		{"bytea decodes to fewer bytes", 17, model.TypeBytes, `{"\\x0041ff"}`, 1, 1, 3},
		{"matrix headers", 23, model.TypeInt64, `[0:1][4:5]={{1,2},{3,4}}`, 4, 2, 0},
		{"payload consumes spare headers", 25, model.TypeString, `{a,b,c,d,e,` + strings.Repeat("z", 1000) + `}`, 6, 1, 1005},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := arrayInputCallable(t, tc.id, tc.storage, builtin.ResolveOptions{})
			budget := tc.count*int64(unsafe.Sizeof(model.Value{})) + tc.rank*int64(unsafe.Sizeof(model.ArrayDimension{})) + tc.payload
			value, err := function.Call(t.Context(), arrayInputArguments(tc.input, tc.id, -1), builtin.CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			measured, err := model.ValueBytes(t.Context(), value, uint64(budget))
			if err != nil || measured != uint64(budget) {
				t.Fatalf("retained payload = %d, %v; want %d", measured, err, budget)
			}
			if cap(value.ArrayVal) != len(value.ArrayVal) || cap(value.Dimensions) != len(value.Dimensions) {
				t.Fatalf("result retains uncharged slice capacity: values %d/%d, dimensions %d/%d", len(value.ArrayVal), cap(value.ArrayVal), len(value.Dimensions), cap(value.Dimensions))
			}
			_, err = function.Call(t.Context(), arrayInputArguments(tc.input, tc.id, -1), builtin.CallBudget{OutputBytes: budget - 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestArrayInputOwnsResultStorage(t *testing.T) {
	input := strings.Clone(`[-1:1]={"borrowed",escaped\ value,final}`)
	function := arrayInputCallable(t, 25, model.TypeString, builtin.ResolveOptions{})
	args := arrayInputArguments(input, 25, -1)
	first, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	second, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	inputStart := uintptr(unsafe.Pointer(unsafe.StringData(input)))
	for i, value := range first.ArrayVal {
		start := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
		if start >= inputStart && start < inputStart+uintptr(len(input)) {
			t.Errorf("element %d retains input string storage", i)
		}
	}
	first.ArrayVal[0] = model.StringVal("changed")
	first.Dimensions[0].LowerBound = 10
	if second.ArrayVal[0].StrVal != "borrowed" || second.Dimensions[0].LowerBound != -1 || args[0].StrVal != input {
		t.Fatal("mutation of one result changed another result or borrowed input")
	}
}

func TestArrayInputCancellation(t *testing.T) {
	function := arrayInputCallable(t, 23, model.TypeInt64, builtin.ResolveOptions{})
	for _, tc := range []struct{ name, input string }{
		{"dimension whitespace", strings.Repeat(" ", 1<<16) + `{1}`},
		{"token whitespace", `{` + strings.Repeat(" ", 1<<16) + `1}`},
		{"long quoted token", `{"` + strings.Repeat("0", 1<<16) + `"}`},
		{"long escaped token", `{"` + strings.Repeat(`\0`, 1<<16) + `"}`},
		{"many elements", `{` + strings.Repeat("1,", 10000) + `1}`},
		{"trailing whitespace", `{1}` + strings.Repeat(" ", 1<<16)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 12}
			value, err := function.Call(interrupted, arrayInputArguments(tc.input, 23, -1), builtin.CallBudget{OutputBytes: 1 << 24, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) || len(value.ArrayVal) != 0 || len(value.Dimensions) != 0 {
				t.Fatalf("canceled parse = %v, %v; want cancellation without partial result", value, err)
			}
		})
	}
}
