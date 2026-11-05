package builtin_test

import (
	"bytes"
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

func requireOwnedTextTransform(t *testing.T, function builtin.Callable, args []model.Value, want string) {
	t.Helper()
	value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || value.Type != model.TypeString || value.IsNull || value.StrVal != want {
		t.Fatalf("transform = %v, %v; want %q", value, err, want)
	}
	if want == "" {
		return
	}
	if unsafe.StringData(value.StrVal) == unsafe.StringData(args[0].StrVal) {
		t.Fatal("result retains borrowed input storage")
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestTextCaseUsesASCIIWordBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          model.FunctionID
		input, want string
	}{
		{"lower ASCII only", 870, "AZaz ÉéİıΣσKß", "azaz ÉéİıΣσKß"},
		{"upper ASCII only", 871, "AZaz ÉéİıΣσKß", "AZAZ ÉéİıΣσKß"},
		{"unchanged lower owns storage", 870, "éabc", "éabc"},
		{"unchanged upper owns storage", 871, "éABC", "éABC"},
		{"punctuation starts words", 872, "o'NEILL_foo-BAR\tBAZ.quX", "O'Neill_Foo-Bar\tBaz.Qux"},
		{"ASCII digits continue words", 872, "123ABC a1BC 0Z x9_YZ", "123abc A1bc 0z X9_Yz"},
		{"nonASCII starts words", 872, "éCOLE ÅNGSTRÖM İSTANBUL KELVIN ßETA", "éCole ÅNgstrÖM İStanbul KElvin ßEta"},
		{"combining marks start words", 872, "a\u0301BC AéBC １２ABC", "A\u0301Bc AéBc １２Abc"},
		{"empty lower", 870, "", ""},
		{"empty upper", 871, "", ""},
		{"empty initcap", 872, "", ""},
	} {
		for _, collation := range []model.CollationID{950, 951} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, collation), func(t *testing.T) {
				function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{Collation: collation})
				if err != nil {
					t.Fatal(err)
				}
				requireOwnedTextTransform(t, function, []model.Value{model.StringVal(tc.input)}, tc.want)
			})
		}
	}
}

func TestTextCaseRequiresConcreteCollation(t *testing.T) {
	for _, tc := range []struct {
		id   model.FunctionID
		want string
	}{
		{870, "abc"}, {871, "ABC"}, {872, "Abc"},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			for _, options := range []builtin.ResolveOptions{
				{}, {Collation: 100}, {Collation: 100, DefaultCollation: 100}, {Collation: 123456},
			} {
				function, err := builtin.Resolve(tc.id, options)
				if err != nil {
					t.Fatal(err)
				}
				value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeString)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull || value.Type != model.TypeString {
					t.Fatalf("strict NULL = %v, %v", value, err)
				}
				_, err = function.Call(t.Context(), []model.Value{model.StringVal("")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "42P22")
			}
			for _, concrete := range []model.CollationID{950, 951} {
				function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{Collation: 100, DefaultCollation: concrete})
				if err != nil {
					t.Fatal(err)
				}
				requireOwnedTextTransform(t, function, []model.Value{model.StringVal("aBc")}, tc.want)
			}
		})
	}
}

func TestRepeatTextCountsAndChunkBoundaries(t *testing.T) {
	function, err := builtin.Resolve(1622, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		input string
		count int64
		want  string
	}{
		{"negative minimum", "abc", math.MinInt32, ""},
		{"negative", "abc", -1, ""},
		{"zero", "abc", 0, ""},
		{"empty huge count", "", math.MaxInt32, ""},
		{"one owns input", "é界", 1, "é界"},
		{"multiple Unicode", "é界", 3, "é界é界é界"},
		{"unaligned copy chunks", "ab界🙂", 2000, strings.Repeat("ab界🙂", 2000)},
		{"source exceeds copy chunk", strings.Repeat("abc", 2000), 3, strings.Repeat("abc", 6000)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []model.Value{model.StringVal(tc.input), model.Int64Val(tc.count)}
			requireOwnedTextTransform(t, function, args, tc.want)
		})
	}
	_, err = function.Call(t.Context(), []model.Value{model.StringVal("abc"), model.Int64Val(math.MaxInt32)}, builtin.CallBudget{OutputBytes: math.MaxInt64, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	_, err = function.Call(t.Context(), []model.Value{model.StringVal(""), model.Int64Val(int64(math.MaxInt32) + 1)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22003")
}

func TestReverseTextUsesCodePoints(t *testing.T) {
	function, err := builtin.Resolve(3062, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, input, want string
	}{
		{"empty", "", ""},
		{"single character owns storage", "界", "界"},
		{"UTF8 widths", "aé界🙂", "🙂界éa"},
		{"combining mark reverses separately", "a\u0301🙂界", "界🙂\u0301a"},
		{"replacement character is valid", "a�b", "b�a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requireOwnedTextTransform(t, function, []model.Value{model.StringVal(tc.input)}, tc.want)
		})
	}
}

func TestReverseByteaUsesBytesAndOwnsResult(t *testing.T) {
	function, err := builtin.Resolve(6382, builtin.ResolveOptions{Collation: 123456})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		input, want []byte
	}{
		{"empty", nil, nil},
		{"single NUL", []byte{0}, []byte{0}},
		{"arbitrary bytes", []byte{0xff, 0, 0xc3, 0xa9}, []byte{0xa9, 0xc3, 0, 0xff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := bytes.Clone(tc.input)
			args := []model.Value{model.BytesVal(tc.input)}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.Type != model.TypeBytes || value.IsNull || !bytes.Equal(value.BytesVal, tc.want) {
				t.Fatalf("reverse = %v, %v; want %x", value, err, tc.want)
			}
			if !bytes.Equal(tc.input, original) {
				t.Fatal("reverse mutated borrowed input")
			}
			if len(tc.want) != 0 {
				_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
				requireSQLState(t, err, "54000")
			}
			for i := range tc.input {
				tc.input[i] ^= 0xff
			}
			if !bytes.Equal(value.BytesVal, tc.want) {
				t.Fatalf("borrowed input mutation changed result to %x; want %x", value.BytesVal, tc.want)
			}
		})
	}
}

func TestTextTransformsRejectInvalidUTF8(t *testing.T) {
	for _, id := range []model.FunctionID{870, 871, 872, 1622, 3062} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function, err := builtin.Resolve(id, builtin.ResolveOptions{Collation: 950})
			if err != nil {
				t.Fatal(err)
			}
			for _, invalid := range []string{"\xff", "\xc3", "a\x00b"} {
				args := []model.Value{model.StringVal(invalid)}
				if id == 1622 {
					args = append(args, model.Int64Val(0))
				}
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22021")
			}
		})
	}
}

func TestTextTransformsStrictNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
		kind model.DataType
	}{
		{"repeat text NULL", 1622, []model.Value{model.NullVal(model.TypeString), model.Int64Val(math.MaxInt32)}, model.TypeString},
		{"repeat count NULL skips invalid UTF8", 1622, []model.Value{model.StringVal("\xff"), model.NullVal(model.TypeInt64)}, model.TypeString},
		{"reverse text", 3062, []model.Value{model.NullVal(model.TypeString)}, model.TypeString},
		{"reverse bytea", 6382, []model.Value{model.NullVal(model.TypeBytes)}, model.TypeBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			value, err := function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != tc.kind {
				t.Fatalf("strict NULL = %v, %v", value, err)
			}
		})
	}
}

func TestTextTransformsCancellationDuringConstruction(t *testing.T) {
	longText := strings.Repeat("AéB", 16384)
	for _, tc := range []struct {
		name      string
		id        model.FunctionID
		args      []model.Value
		remaining int
	}{
		{"lower", 870, []model.Value{model.StringVal(longText)}, 24},
		{"upper", 871, []model.Value{model.StringVal(longText)}, 24},
		{"initcap", 872, []model.Value{model.StringVal(longText)}, 24},
		{"reverse text", 3062, []model.Value{model.StringVal(longText)}, 24},
		{"reverse bytea", 6382, []model.Value{model.BytesVal([]byte(longText))}, 6},
		{"repeat short source", 1622, []model.Value{model.StringVal("é"), model.Int64Val(200000)}, 6},
		{"repeat long source", 1622, []model.Value{model.StringVal(longText), model.Int64Val(2)}, 24},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{Collation: 950})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = function.Call(ctx, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-canceled transform = %v", err)
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			// Let UTF8 validation finish before interrupting output construction.
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: tc.remaining}
			_, err = function.Call(interrupted, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted transform = %v", err)
			}
		})
	}
}
