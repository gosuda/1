package builtin_test

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func callable(t *testing.T, name string, arguments ...model.TypeID) builtin.Callable {
	t.Helper()
	for signature := range builtin.SignaturesNamed(name) {
		if int(signature.ArgumentCount) != len(arguments) {
			continue
		}
		match := true
		for i, argument := range arguments {
			if signature.Arguments[i] != argument {
				match = false
			}
		}
		if !match {
			continue
		}
		function, err := builtin.ResolveSignature(signature, builtin.ResolveOptions{Collation: 100, DefaultCollation: 950, ExtraFloatDigits: 1})
		if err != nil {
			t.Fatal(err)
		}
		return function
	}
	t.Fatalf("missing metadata for %s(%v)", name, arguments)
	return builtin.Callable{}
}

func requireSQLState(t *testing.T, err error, code string) {
	t.Helper()
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.SQLState() != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}

func TestIntegerArithmeticBoundaries(t *testing.T) {
	for _, test := range []struct {
		name             string
		id               model.TypeID
		minimum, maximum int64
	}{
		{"int2", 21, math.MinInt16, math.MaxInt16},
		{"int4", 23, math.MinInt32, math.MaxInt32},
		{"int8", 20, math.MinInt64, math.MaxInt64},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, operation := range []struct {
				suffix      string
				left, right int64
			}{
				{"pl", test.maximum, 1}, {"mi", test.minimum, 1},
				{"mul", test.minimum, -1}, {"div", test.minimum, -1},
			} {
				_, err := callable(t, test.name+operation.suffix, test.id, test.id).Call(t.Context(), []model.Value{model.Int64Val(operation.left), model.Int64Val(operation.right)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22003")
			}
			_, err := callable(t, "abs", test.id).Call(t.Context(), []model.Value{model.Int64Val(test.minimum)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22003")
			value, err := callable(t, test.name+"mod", test.id, test.id).Call(t.Context(), []model.Value{model.Int64Val(test.minimum), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.IntVal != 0 {
				t.Fatalf("MIN %% -1 = %v, %v", value, err)
			}
			_, err = callable(t, test.name+"div", test.id, test.id).Call(t.Context(), []model.Value{model.Int64Val(1), model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22012")
		})
	}
}

func TestMixedIntegerResultWidth(t *testing.T) {
	value, err := callable(t, "int24pl", 21, 23).Call(t.Context(), []model.Value{model.Int64Val(math.MaxInt16), model.Int64Val(1)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IntVal != 32768 {
		t.Fatalf("int2 + int4 = %v, %v", value, err)
	}
	_, err = callable(t, "int2", 23).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22003")
}

func TestFloatIntegerCastRoundsBeforeRangeCheck(t *testing.T) {
	function := callable(t, "int2", 701)
	for _, test := range []struct {
		input float64
		want  int64
	}{{2.5, 2}, {3.5, 4}, {-2.5, -2}, {-32768.5, -32768}} {
		value, err := function.Call(t.Context(), []model.Value{model.Float64Val(test.input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || value.IntVal != test.want {
			t.Fatalf("int2(%v) = %v, %v; want %d", test.input, value, err, test.want)
		}
	}
	for _, input := range []float64{32767.5, math.NaN(), math.Inf(1)} {
		_, err := function.Call(t.Context(), []model.Value{model.Float64Val(input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22003")
	}
	_, err := callable(t, "int8", 701).Call(t.Context(), []model.Value{model.Float64Val(0x1p63)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22003")
}

func TestFloatNaNAndRangeSemantics(t *testing.T) {
	for _, test := range []struct {
		name string
		a, b float64
		want bool
	}{
		{"float8eq", math.NaN(), math.NaN(), true},
		{"float8gt", math.NaN(), math.Inf(1), true},
		{"float8lt", math.Inf(1), math.NaN(), true},
	} {
		value, err := callable(t, test.name, 701, 701).Call(t.Context(), []model.Value{model.Float64Val(test.a), model.Float64Val(test.b)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || (value.IntVal != 0) != test.want {
			t.Fatalf("%s = %v, %v", test.name, value, err)
		}
	}
	divide := callable(t, "float8div", 701, 701)
	value, err := divide.Call(t.Context(), []model.Value{model.Float64Val(math.NaN()), model.Float64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !math.IsNaN(value.FloatVal) {
		t.Fatalf("NaN / 0 = %v, %v", value, err)
	}
	value, err = divide.Call(t.Context(), []model.Value{model.Float64Val(42), model.Float64Val(math.Inf(1))}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.FloatVal != 0 {
		t.Fatalf("42 / Infinity = %v, %v", value, err)
	}
	for _, test := range []struct {
		name string
		id   model.TypeID
		a, b float64
		code string
	}{
		{"float8div", 701, 1, 0, "22012"},
		{"float8mul", 701, math.MaxFloat64, 2, "22003"},
		{"float8mul", 701, math.SmallestNonzeroFloat64, 0.5, "22003"},
		{"float4pl", 700, math.MaxFloat32, math.MaxFloat32, "22003"},
	} {
		_, err := callable(t, test.name, test.id, test.id).Call(t.Context(), []model.Value{model.Float64Val(test.a), model.Float64Val(test.b)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, test.code)
	}
	_, err = callable(t, "float4", 701).Call(t.Context(), []model.Value{model.Float64Val(1e-100)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22003")
}

func TestTypedNullAndPhysicalContract(t *testing.T) {
	function := callable(t, "int4div", 23, 23)
	value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeInt64), model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull || value.Type != model.TypeInt64 {
		t.Fatalf("NULL / 0 = %v, %v", value, err)
	}
	_, err = function.Call(t.Context(), []model.Value{model.NullVal(model.TypeString), model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42804")
	_, err = function.Call(t.Context(), []model.Value{model.NullVal(model.TypeInt64)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42883")
	_, err = function.Call(t.Context(), []model.Value{model.Int64Val(math.MaxInt64), model.Int64Val(1)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22003")
	_, err = callable(t, "float4up", 700).Call(t.Context(), []model.Value{model.Float64Val(0.1)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42804")
}

func TestTextAndBytesHaveDifferentPositions(t *testing.T) {
	text := model.StringVal("aé日z")
	value, err := callable(t, "substr", 25, 23, 23).Call(t.Context(), []model.Value{text, model.Int64Val(2), model.Int64Val(2)}, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "é日" {
		t.Fatalf("text substring = %v, %v", value, err)
	}
	input := []byte("aé日z")
	value, err = callable(t, "substr", 17, 23, 23).Call(t.Context(), []model.Value{model.BytesVal(input), model.Int64Val(2), model.Int64Val(1)}, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
	if err != nil || len(value.BytesVal) != 1 || value.BytesVal[0] != 0xc3 {
		t.Fatalf("bytea substring = %v, %v", value, err)
	}
	input[1] = 0
	if value.BytesVal[0] != 0xc3 || cap(value.BytesVal) != 1 {
		t.Fatal("substring retained borrowed input storage")
	}
	for _, test := range []struct {
		name  string
		count int64
		want  string
	}{{"left", -1, "aé日"}, {"right", -1, "é日z"}, {"right", math.MinInt32, ""}} {
		value, err = callable(t, test.name, 25, 23).Call(t.Context(), []model.Value{text, model.Int64Val(test.count)}, builtin.CallBudget{OutputBytes: 16, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != test.want {
			t.Fatalf("%s = %v, %v", test.name, value, err)
		}
	}
	value, err = callable(t, "substring", 25, 23, 23).Call(t.Context(), []model.Value{text, model.Int64Val(0), model.Int64Val(2)}, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "a" {
		t.Fatalf("substring start zero = %v, %v", value, err)
	}
	_, err = callable(t, "substr", 25, 23, 23).Call(t.Context(), []model.Value{text, model.Int64Val(1), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: 16, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22011")
	value, err = callable(t, "length", 25).Call(t.Context(), []model.Value{text}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IntVal != 4 {
		t.Fatalf("length = %v, %v", value, err)
	}
	value, err = callable(t, "octet_length", 25).Call(t.Context(), []model.Value{text}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IntVal != 7 {
		t.Fatalf("octet_length = %v, %v", value, err)
	}
}

func TestOutputBudgetAndCancellation(t *testing.T) {
	function := callable(t, "textcat", 25, 25)
	arguments := []model.Value{model.StringVal("é"), model.StringVal("日")}
	_, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	value, err := function.Call(t.Context(), arguments, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "é日" {
		t.Fatalf("exact-budget concat = %v, %v", value, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = function.Call(ctx, arguments, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled call = %v", err)
	}
	_, err = callable(t, "textin", 2275).Call(t.Context(), []model.Value{model.StringVal(string([]byte{0xff}))}, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22021")
}

func TestKnownMissingAndUnknownFunctions(t *testing.T) {
	_, err := builtin.Resolve(model.FunctionID(math.MaxUint32), builtin.ResolveOptions{})
	var unknown *builtin.UnknownFunctionError
	if !errors.As(err, &unknown) {
		t.Fatalf("unknown function error = %v", err)
	}
	for _, name := range []string{"pg_read_file"} {
		for signature := range builtin.SignaturesNamed(name) {
			_, err = builtin.Resolve(signature.ID, builtin.ResolveOptions{})
			var missing *builtin.MissingImplementationError
			if !errors.As(err, &missing) {
				t.Fatalf("%s unexpectedly executable: %v", name, err)
			}
		}
	}
	for signature := range builtin.SignaturesNamed("int4pl") {
		signature.Result = 20
		_, err = builtin.ResolveSignature(signature, builtin.ResolveOptions{})
		requireSQLState(t, err, "42804")
	}
}

func TestCollationMustResolveWithoutAmbientState(t *testing.T) {
	for signature := range builtin.SignaturesNamed("text_lt") {
		unresolved, err := builtin.Resolve(signature.ID, builtin.ResolveOptions{Collation: 100})
		if err != nil {
			t.Fatal(err)
		}
		value, err := unresolved.Call(t.Context(), []model.Value{model.NullVal(model.TypeString), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull {
			t.Fatalf("strict NULL comparison = %v, %v", value, err)
		}
		_, err = unresolved.Call(t.Context(), []model.Value{model.StringVal("Z"), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "42P22")
		function, err := builtin.Resolve(signature.ID, builtin.ResolveOptions{Collation: 100, DefaultCollation: 951})
		if err != nil {
			t.Fatal(err)
		}
		value, err = function.Call(t.Context(), []model.Value{model.StringVal("Z"), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || value.IntVal != 1 {
			t.Fatalf("POSIX ordering = %v, %v", value, err)
		}
	}
}

func TestPrimitiveInputOutput(t *testing.T) {
	for _, test := range []struct {
		text string
		want int64
	}{{"  -0x8000 ", -32768}, {"010", 10}, {"0b_10_0101", 37}, {"1_000", 1000}} {
		value, err := callable(t, "int2in", 2275).Call(t.Context(), []model.Value{model.StringVal(test.text)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || value.IntVal != test.want {
			t.Fatalf("int2in(%q) = %v, %v", test.text, value, err)
		}
	}
	for _, input := range []string{"10__0", "1_", "_1"} {
		_, err := callable(t, "int2in", 2275).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22P02")
	}
	_, err := callable(t, "boolin", 2275).Call(t.Context(), []model.Value{model.StringVal("o")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22P02")
	value, err := callable(t, "byteain", 2275).Call(t.Context(), []model.Value{model.StringVal(`\x De 00 ff `)}, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
	if err != nil || string(value.BytesVal) != string([]byte{0xde, 0, 0xff}) {
		t.Fatalf("bytea hex input = %v, %v", value, err)
	}
	value, err = callable(t, "byteaout", 17).Call(t.Context(), []model.Value{value}, builtin.CallBudget{OutputBytes: 8, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != `\xde00ff` {
		t.Fatalf("bytea hex output = %v, %v", value, err)
	}
	_, err = callable(t, "byteain", 2275).Call(t.Context(), []model.Value{model.StringVal(`\x d e`)}, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22P02")
	value, err = callable(t, "float4in", 2275).Call(t.Context(), []model.Value{model.StringVal("7.038531e-26")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || math.Float32bits(float32(value.FloatVal)) != 0x15ae43fd {
		t.Fatalf("single-rounding input = %v, %v", value, err)
	}
	for _, input := range []string{"1e-999", "1_0"} {
		_, err := callable(t, "float8in", 2275).Call(t.Context(), []model.Value{model.StringVal(input)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		code := "22003"
		if strings.ContainsRune(input, '_') {
			code = "22P02"
		}
		requireSQLState(t, err, code)
	}
}

func TestFloatOutputUsesPostgresExponentAndMidpointRules(t *testing.T) {
	for _, test := range []struct {
		name  string
		id    model.TypeID
		input float64
		want  string
	}{
		{"float8out", 701, 1e6, "1000000"},
		{"float4out", 700, 1e6, "1e+06"},
		{"float8out", 701, math.Inf(1), "Infinity"},
		{"float8out", 701, math.Copysign(0, -1), "-0"},
		{"float4out", 700, 100000000, "1e+08"},
		{"float4out", 700, 100000016, "1.00000016e+08"},
	} {
		value, err := callable(t, test.name, test.id).Call(t.Context(), []model.Value{model.Float64Val(test.input)}, builtin.CallBudget{OutputBytes: 64, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != test.want {
			t.Fatalf("%s(%v) = %v, %v; want %q", test.name, test.input, value, err, test.want)
		}
	}
}

func TestByteaEscapeOutputAndBooleanTextCast(t *testing.T) {
	for signature := range builtin.SignaturesNamed("byteaout") {
		function, err := builtin.Resolve(signature.ID, builtin.ResolveOptions{ByteaEscape: true})
		if err != nil {
			t.Fatal(err)
		}
		value, err := function.Call(t.Context(), []model.Value{model.BytesVal([]byte{'A', '\\', 0, 255})}, builtin.CallBudget{OutputBytes: 11, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != `A\\\000\377` {
			t.Fatalf("bytea escape = %v, %v", value, err)
		}
		_, err = function.Call(t.Context(), []model.Value{model.BytesVal([]byte{255})}, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54000")
	}
	value, err := callable(t, "text", 16).Call(t.Context(), []model.Value{model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "true" {
		t.Fatalf("boolean text cast = %v, %v", value, err)
	}
	value, err = callable(t, "boolout", 16).Call(t.Context(), []model.Value{model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "t" {
		t.Fatalf("boolean output = %v, %v", value, err)
	}
}
