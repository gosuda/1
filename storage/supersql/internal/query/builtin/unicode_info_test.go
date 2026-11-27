package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestUnicodeVersionMatchesPinnedData(t *testing.T) {
	function, err := builtin.Resolve(4549, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	value, err := function.Call(t.Context(), nil, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if value.StrVal != "16.0" {
		t.Fatalf("Unicode version=%q, want 16.0", value.StrVal)
	}
	_, err = function.Call(t.Context(), nil, builtin.CallBudget{OutputBytes: 3, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestUnicodeAssignedUsesUnicodeSixteenCategories(t *testing.T) {
	function, err := builtin.Resolve(6105, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, text string
		want       bool
	}{
		{"empty", "", true},
		{"ordinary", "ABC 한글", true},
		{"Unicode16 addition", "\U000105c0", true},
		{"private use", "\ue000", true},
		{"replacement", "\ufffd", true},
		{"unassigned", "\u0378", false},
		{"noncharacter", "\ufdd0", false},
		{"maximum noncharacter", "\U0010ffff", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.text)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if value.IsNull || (value.IntVal != 0) != tc.want {
				t.Fatalf("unicode_assigned=%v, want %t", value, tc.want)
			}
		})
	}
}

func TestUnicodeAssignedCancellationDuringScan(t *testing.T) {
	function, err := builtin.Resolve(6105, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &characterCancelContext{Context: parent, cancel: cancel, remaining: 3}
	_, err = function.Call(ctx, []model.Value{model.StringVal(strings.Repeat("a", 1<<20))}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Unicode assignment scan=%v", err)
	}
}
