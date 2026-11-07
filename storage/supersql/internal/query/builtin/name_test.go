package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestNameInputClipsOnUTF8ByteBoundary(t *testing.T) {
	input := callable(t, "namein", 2275)
	for _, tc := range []struct{ name, text, want string }{
		{"empty", "", ""},
		{"ascii boundary", strings.Repeat("x", 64), strings.Repeat("x", 63)},
		{"complete multibyte", strings.Repeat("x", 60) + "界z", strings.Repeat("x", 60) + "界"},
		{"partial multibyte", strings.Repeat("x", 61) + "界", strings.Repeat("x", 61)},
		{"four byte rune", strings.Repeat("x", 60) + "𐐀", strings.Repeat("x", 60)},
		{"cstring terminator", "Keep\x00discard", "Keep"},
		{"case and blanks", "Mixed Case ", "Mixed Case "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := input.Call(t.Context(), []model.Value{model.StringVal(tc.text)}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || got.Type != model.TypeString || got.IsNull || got.StrVal != tc.want {
				t.Fatalf("name input=%v,%v want %q", got, err, tc.want)
			}
		})
	}
}

func TestNameCharacterCastClipsBeforeTrimming(t *testing.T) {
	textToName := callable(t, "name", 25)
	characterToName := callable(t, "name", 1042)
	source := strings.Repeat("x", 62) + " z"
	preserved, err := textToName.Call(t.Context(), []model.Value{model.StringVal(source)}, builtin.CallBudget{OutputBytes: 63, MemoryBytes: 64 << 20})
	if err != nil || preserved.StrVal != strings.Repeat("x", 62)+" " {
		t.Fatalf("text name cast=%v,%v", preserved, err)
	}
	trimmed, err := characterToName.Call(t.Context(), []model.Value{model.StringVal(source)}, builtin.CallBudget{OutputBytes: 62, MemoryBytes: 64 << 20})
	if err != nil || trimmed.StrVal != strings.Repeat("x", 62) {
		t.Fatalf("bpchar name cast=%v,%v", trimmed, err)
	}
}

func TestNameOutputsRespectExactBudget(t *testing.T) {
	for _, id := range []model.FunctionID{35, 406, 408} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		value, err := function.Call(t.Context(), []model.Value{model.StringVal("Kept ")}, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
		if err != nil || value.StrVal != "Kept " {
			t.Fatalf("name output %d=%v,%v", id, value, err)
		}
		_, err = function.Call(t.Context(), []model.Value{model.StringVal("Kept ")}, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "54000")
	}
}

func TestNameCodecsPreserveStrictNullAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, id := range []model.FunctionID{34, 35, 406, 407, 408, 409} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		null, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeString)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !null.IsNull || null.Type != model.TypeString {
			t.Fatalf("NULL name %d=%v,%v", id, null, err)
		}
		_, err = function.Call(ctx, []model.Value{model.StringVal("name")}, builtin.CallBudget{OutputBytes: 63, MemoryBytes: 64 << 20})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled name %d=%v", id, err)
		}
	}
}
