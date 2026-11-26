package builtin_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestLikeEscapeTextNormalization(t *testing.T) {
	function, err := builtin.Resolve(1637, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, pattern, escape, want string
	}{
		{"empty pattern", "", "!", ""},
		{"disabled escape", `a\b\\%_`, "", `a\\b\\\\%_`},
		{"default escape preserves pattern", `é\_%\`, `\`, `é\_%\`},
		{"custom escaped wildcards", `a!_b!%`, "!", `a\_b\%`},
		{"doubled custom escape", `!!`, "!", `\!`},
		{"literal backslash", `a\b`, "!", `a\\b`},
		{"escaped backslash", `!\`, "!", `\\`},
		{"trailing custom escape", `a!`, "!", `a\`},
		{"multibyte escape", `🙂_🙂🙂🙂界🙂`, "🙂", `\_\🙂\界\`},
		{"escaped multibyte character", `é界é_`, "é", `\界\_`},
		{"replacement character is valid", "�_", "�", `\_`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []model.Value{model.StringVal(tc.pattern), model.StringVal(tc.escape)}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.Type != model.TypeString || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("normalization = %v, %v; want %q", value, err, tc.want)
			}
			if tc.want != "" && unsafe.StringData(value.StrVal) == unsafe.StringData(tc.pattern) {
				t.Fatal("result retains borrowed pattern storage")
			}
			if tc.want != "" {
				_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
				requireSQLState(t, err, "54000")
			}
		})
	}
}

func TestLikeEscapeByteaNormalization(t *testing.T) {
	function, err := builtin.Resolve(2009, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                  string
		pattern, escape, want []byte
	}{
		{"empty pattern", nil, nil, nil},
		{"disabled escape", []byte{0, '\\', 0xff, '%', '_'}, nil, []byte{0, '\\', '\\', 0xff, '%', '_'}},
		{"default escape preserves pattern", []byte{0, 0xff, '\\'}, []byte{'\\'}, []byte{0, 0xff, '\\'}},
		{"custom escaped wildcards", []byte(`a!_b!%`), []byte{'!'}, []byte(`a\_b\%`)},
		{"doubled custom escape", []byte(`!!`), []byte{'!'}, []byte(`\!`)},
		{"literal backslash", []byte(`a\b`), []byte{'!'}, []byte(`a\\b`)},
		{"escaped backslash", []byte(`!\`), []byte{'!'}, []byte(`\\`)},
		{"trailing custom escape", []byte(`a!`), []byte{'!'}, []byte(`a\`)},
		{"NUL escape", []byte{0, '_', 0, 0, 0, '\\', 0}, []byte{0}, []byte{'\\', '_', '\\', 0, '\\', '\\', '\\'}},
		{"non UTF8 escape", []byte{0xff, '%', 0xff, 0xff}, []byte{0xff}, []byte{'\\', '%', '\\', 0xff}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := bytes.Clone(tc.pattern)
			args := []model.Value{model.BytesVal(tc.pattern), model.BytesVal(tc.escape)}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.Type != model.TypeBytes || value.IsNull || !bytes.Equal(value.BytesVal, tc.want) {
				t.Fatalf("normalization = %v, %v; want %x", value, err, tc.want)
			}
			if !bytes.Equal(tc.pattern, original) {
				t.Fatal("call changed borrowed pattern")
			}
			if len(tc.want) != 0 {
				_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
				requireSQLState(t, err, "54000")
			}
			for i := range tc.pattern {
				tc.pattern[i] ^= 0xff
			}
			for i := range tc.escape {
				tc.escape[i] ^= 0xff
			}
			if !bytes.Equal(value.BytesVal, tc.want) {
				t.Fatalf("borrowed operand mutation changed result to %x; want %x", value.BytesVal, tc.want)
			}
		})
	}
}

func TestLikeEscapeInvalidOperands(t *testing.T) {
	for _, tc := range []struct {
		name    string
		id      model.FunctionID
		pattern model.Value
		escape  model.Value
		state   string
	}{
		{"text escape width", 1637, model.StringVal(""), model.StringVal("ab"), "22025"},
		{"combining escape width", 1637, model.StringVal(""), model.StringVal("e\u0301"), "22025"},
		{"invalid pattern UTF8", 1637, model.StringVal("\xff"), model.StringVal(`\`), "22021"},
		{"invalid escape UTF8", 1637, model.StringVal(""), model.StringVal("\xff"), "22021"},
		{"pattern NUL", 1637, model.StringVal("\x00"), model.StringVal(""), "22021"},
		{"escape NUL", 1637, model.StringVal(""), model.StringVal("\x00"), "22021"},
		{"bytea escape width", 2009, model.BytesVal(nil), model.BytesVal([]byte{0, 1}), "22025"},
		{"bytea multibyte escape", 2009, model.BytesVal(nil), model.BytesVal([]byte("é")), "22025"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = function.Call(t.Context(), []model.Value{tc.pattern, tc.escape}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.state)
		})
	}
}

func TestLikeEscapeStrictNullSkipsNormalization(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
	}{
		{"text", 1637, []model.Value{model.StringVal("\xff"), model.StringVal("ab")}},
		{"bytea", 2009, []model.Value{model.BytesVal([]byte{'\\'}), model.BytesVal([]byte{0, 1})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			for i := range tc.args {
				saved := tc.args[i]
				tc.args[i] = model.NullVal(saved.Type)
				value, err := function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull || value.Type != saved.Type {
					t.Fatalf("NULL argument %d = %v, %v", i, value, err)
				}
				tc.args[i] = saved
			}
		})
	}
}

func TestLikeEscapeCancellation(t *testing.T) {
	pattern := strings.Repeat(`!_\`, 10000)
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
	}{
		{"text", 1637, []model.Value{model.StringVal(pattern), model.StringVal("!")}},
		{"bytea", 2009, []model.Value{model.BytesVal([]byte(pattern)), model.BytesVal([]byte{'!'})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err = function.Call(ctx, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled normalization = %v", err)
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 3}
			_, err = function.Call(interrupted, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted normalization = %v", err)
			}
		})
	}
}
