package builtin_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func digestCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func TestMD5VectorsAtExactBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.FunctionID
		input model.Value
		want  string
	}{
		{"empty text", 2311, model.StringVal(""), "d41d8cd98f00b204e9800998ecf8427e"},
		{"UTF8 text", 2311, model.StringVal("é界🙂"), "704fd18f50a23c709a6296f650d3d9f8"},
		{"binary NUL", 2321, model.BytesVal([]byte{'a', 0, 'b', 0xff}), "0dd4df5b5be53b9566c53387ec9a7bac"},
		{"UTF8 crossing chunks", 2311, model.StringVal(strings.Repeat("é界🙂", 1500)), "812832a59ac6144e6614a00f0d05ee8f"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := digestCallable(t, tc.id)
			args := []model.Value{tc.input}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeString || value.StrVal != tc.want {
				t.Fatalf("md5 = %v, %v; want %q", value, err, tc.want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 31, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestSHAVectorsAtExactBudget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		id    model.FunctionID
		input []byte
		want  string
	}{
		{"sha224", 3419, []byte("abc"), "23097d223405d8228642a477bda255b32aadbce4bda0b3f7e36c9da7"},
		{"sha256 binary NUL", 3420, []byte{'a', 0, 'b', 0xff}, "a37cc3026aae4d519e0b19c298fa913b4dccfdf0658cbccbb7deaa0226d5acdb"},
		{"sha384", 3421, []byte("abc"), "cb00753f45a35e8bb5a03d699ac65007272c32ab0eded1631a8b605a43ff5bed8086072ba1e7cc2358baeca134c825a7"},
		{"sha512 crossing chunks", 3422, bytes.Repeat([]byte{'a', 0, 'b', 0xff}, 3000), "4887d16129ce03f9f8d3c6b8b5ef322c21ce74a3f99f63664395e23215a18806f719b52f0563b6a4b0e9f3dd99aa464420a1450564039b36a103956ac82cbc01"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := digestCallable(t, tc.id)
			args := []model.Value{model.BytesVal(tc.input)}
			size := int64(len(tc.want) / 2)
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: size, MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeBytes || hex.EncodeToString(value.BytesVal) != tc.want {
				t.Fatalf("%s = %v, %v; want bytea %s", tc.name, value, err, tc.want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: size - 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestSHAResultsOwnStorage(t *testing.T) {
	function := digestCallable(t, 3420)
	input := []byte{'a', 0, 'b', 0xff}
	first, err := function.Call(t.Context(), []model.Value{model.BytesVal(input)}, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	input[0] = 0xff
	second, err := function.Call(t.Context(), []model.Value{model.BytesVal(input)}, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(first.BytesVal); got != "a37cc3026aae4d519e0b19c298fa913b4dccfdf0658cbccbb7deaa0226d5acdb" {
		t.Fatalf("earlier result changed after input mutation or later call: %s", got)
	}
	first.BytesVal[0] ^= 0xff
	if got := hex.EncodeToString(second.BytesVal); got != "b8842d16ae80b39d5ca499035a61d3534e5967cae9c1550f7d1afc04a213b337" {
		t.Fatalf("later result aliases earlier output: %s", got)
	}
}

func TestMD5RejectsInvalidText(t *testing.T) {
	function := digestCallable(t, 2311)
	for _, tc := range []struct {
		name  string
		input string
	}{
		{"invalid UTF8", "\xff"},
		{"embedded NUL", "a\x00b"},
		{"truncated UTF8 after chunks", strings.Repeat("é", 5000) + "\xe2\x82"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input)}, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22021")
		})
	}
}

func TestDigestStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{2311, 2321, 3419, 3420, 3421, 3422} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := digestCallable(t, id)
			input := model.BytesVal([]byte{0xff, 0})
			if id == 2311 {
				input = model.StringVal("\xff\x00")
			}
			input.IsNull = true
			wantType := model.TypeBytes
			if id == 2311 || id == 2321 {
				wantType = model.TypeString
			}
			value, err := function.Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != wantType {
				t.Fatalf("NULL digest = %v, %v; want NULL type %v", value, err, wantType)
			}
		})
	}
}

func TestDigestCancellation(t *testing.T) {
	text := model.StringVal(strings.Repeat("é", 1<<17))
	binary := model.BytesVal(bytes.Repeat([]byte{0xff, 0}, 1<<17))
	for _, tc := range []struct {
		name        string
		id          model.FunctionID
		input       model.Value
		checkpoints int
	}{
		{"text validation", 2311, text, 3},
		{"text hashing", 2311, text, 80},
		{"MD5 bytea hashing", 2321, binary, 4},
		{"SHA bytea hashing", 3422, binary, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := digestCallable(t, tc.id)
			args := []model.Value{tc.input}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := function.Call(ctx, args, builtin.CallBudget{OutputBytes: 64, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled digest = %v", err)
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: tc.checkpoints}
			_, err = function.Call(interrupted, args, builtin.CallBudget{OutputBytes: 64, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted digest = %v", err)
			}
		})
	}
}
