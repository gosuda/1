package builtin_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestCharacterTypmodBoundaries(t *testing.T) {
	for _, typ := range []struct {
		name string
		id   model.TypeID
	}{
		{"bpchar", 1042},
		{"varchar", 1043},
	} {
		t.Run(typ.name, func(t *testing.T) {
			function := callable(t, typ.name, typ.id, 23, 16)
			for _, tc := range []struct {
				name     string
				input    string
				length   int64
				explicit bool
				want     string
				code     string
			}{
				{"multibyte exact", "é日", 2, false, "é日", ""},
				{"explicit truncation", "é日z", 2, true, "é日", ""},
				{"implicit overflow", "é日z", 2, false, "", "22001"},
				{"excess spaces", "é日  ", 2, false, "é日", ""},
				{"excess tab", "é日\t", 2, false, "", "22001"},
				{"excess nonbreaking space", "é日\u00a0", 2, false, "", "22001"},
				{"explicit nonbreaking space", "é日\u00a0", 2, true, "é日", ""},
				{"combining character counts separately", "e\u0301x", 1, true, "e", ""},
				{"invalid UTF8 after cut", "a\xff", 1, true, "", "22021"},
				{"NUL after cut", "a\x00", 1, true, "", "22021"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					value, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input), model.Int64Val(tc.length + 4), model.BoolVal(tc.explicit)}, builtin.CallBudget{OutputBytes: 32, MemoryBytes: 64 << 20})
					if tc.code != "" {
						requireSQLState(t, err, tc.code)
						return
					}
					if err != nil || value.StrVal != tc.want {
						t.Fatalf("coercion = %q, %v; want %q", value.StrVal, err, tc.want)
					}
				})
			}
		})
	}
}

func TestCharacterInputUsesAssignmentSemantics(t *testing.T) {
	for _, name := range []string{"bpcharin", "varcharin"} {
		t.Run(name, func(t *testing.T) {
			function := callable(t, name, 2275, 26, 23)
			for _, tc := range []struct {
				name, input, want, code string
			}{
				{"multibyte exact", "é日", "é日", ""},
				{"excess ASCII spaces", "é日  ", "é日", ""},
				{"excess nonspace", "é日x", "", "22001"},
				{"excess Unicode space", "é日\u2003", "", "22001"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					value, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input), model.Int64Val(0), model.Int64Val(6)}, builtin.CallBudget{OutputBytes: 5, MemoryBytes: 64 << 20})
					if tc.code != "" {
						requireSQLState(t, err, tc.code)
						return
					}
					if err != nil || value.StrVal != tc.want {
						t.Fatalf("input = %q, %v; want %q", value.StrVal, err, tc.want)
					}
				})
			}
		})
	}
}

func TestCharacterPaddingAndUnconstrainedInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   model.TypeID
	}{
		{"bpchar", 1042}, {"varchar", 1043},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, input := range []string{"", "é", "é ", "é   "} {
				want := input
				if tc.id == 1042 {
					switch input {
					case "":
						want = "    "
					case "é", "é ":
						want = "é   "
					}
				}
				for _, function := range []struct {
					call builtin.Callable
					args []model.Value
				}{
					{callable(t, tc.name, tc.id, 23, 16), []model.Value{model.StringVal(input), model.Int64Val(8), model.BoolVal(false)}},
					{callable(t, tc.name+"in", 2275, 26, 23), []model.Value{model.StringVal(input), model.Int64Val(0), model.Int64Val(8)}},
				} {
					value, err := function.call.Call(t.Context(), function.args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
					if err != nil || value.StrVal != want {
						t.Fatalf("%s(%q) = %q, %v; want %q", function.call.Signature().Name, input, value.StrVal, err, want)
					}
				}
			}
			input := "é日z  "
			function := callable(t, tc.name+"in", 2275, 26, 23)
			value, err := function.Call(t.Context(), []model.Value{model.StringVal(input), model.Int64Val(0), model.Int64Val(-1)}, builtin.CallBudget{OutputBytes: int64(len(input)), MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != input {
				t.Fatalf("unconstrained input = %q, %v; want %q", value.StrVal, err, input)
			}
		})
	}
}

func TestCharacterOutputOwnsStorage(t *testing.T) {
	input := strings.Repeat("é", 2) + "\u00a0  "
	for _, tc := range []struct {
		name string
		id   model.TypeID
		args []model.Value
		want string
	}{
		{"bpcharout", 1042, nil, input},
		{"varcharout", 1043, nil, input},
		{"text", 1042, nil, "éé\u00a0"},
		{"bpchar", 1042, []model.Value{model.Int64Val(-1), model.BoolVal(false)}, input},
		{"varchar", 1043, []model.Value{model.Int64Val(6), model.BoolVal(true)}, "éé"},
		{"bpchar", 1042, []model.Value{model.Int64Val(11), model.BoolVal(false)}, input + "  "},
	} {
		t.Run(tc.name+tc.want, func(t *testing.T) {
			ids := []model.TypeID{tc.id}
			if len(tc.args) != 0 {
				ids = append(ids, 23, 16)
			}
			function := callable(t, tc.name, ids...)
			args := append([]model.Value{model.StringVal(input)}, tc.args...)
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != tc.want {
				t.Fatalf("output = %q, %v; want %q", value.StrVal, err, tc.want)
			}
			if unsafe.StringData(value.StrVal) == unsafe.StringData(input) {
				t.Fatal("result retains borrowed string storage")
			}
			if args[0].StrVal != input {
				t.Fatal("call changed borrowed operand")
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestBpcharComparisonsIgnoreOnlyTrailingASCIISpaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
		want int64
	}{
		{"bpchareq", "é  ", "é", 1},
		{"bpcharne", "é\u00a0", "é ", 1},
		{"bpcharlt", "a  ", "b", 1},
		{"bpcharle", "é ", "é", 1},
		{"bpchargt", "é\u00a0 ", "é", 1},
		{"bpcharge", "é", "é ", 1},
		{"bpcharcmp", "  ", "", 0},
		{"bpcharcmp", "a ", "b", -1},
		{"bpcharcmp", "b", "a ", 1},
		{"bpcharcmp", "a b", "ab", -1},
	} {
		t.Run(tc.name+tc.a+tc.b, func(t *testing.T) {
			args := []model.Value{model.StringVal(tc.a), model.StringVal(tc.b)}
			value, err := callable(t, tc.name, 1042, 1042).Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.IntVal != tc.want {
				t.Fatalf("comparison = %v, %v; want %d", value, err, tc.want)
			}
			if args[0].StrVal != tc.a || args[1].StrVal != tc.b {
				t.Fatal("comparison changed borrowed operands")
			}
		})
	}
}

func TestCharacterStrictNullSkipsInvalidConversion(t *testing.T) {
	for _, id := range []model.FunctionID{1044, 1046, 668, 669} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		args := []model.Value{model.StringVal("\xff"), model.Int64Val(5), model.BoolVal(false)}
		if id == 1044 || id == 1046 {
			args[1], args[2] = model.Int64Val(0), model.Int64Val(5)
		}
		for i := range args {
			saved := args[i]
			args[i] = model.NullVal(saved.Type)
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != model.TypeString {
				t.Fatalf("function %d NULL argument %d = %v, %v", id, i, value, err)
			}
			args[i] = saved
		}
	}
}

func TestBpcharCollationErrorFollowsStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{1048, 1049, 1050, 1051, 1052, 1053, 1078} {
		function, err := builtin.Resolve(id, builtin.ResolveOptions{Collation: 100})
		if err != nil {
			t.Fatal(err)
		}
		value, err := function.Call(t.Context(), []model.Value{model.NullVal(model.TypeString), model.StringVal("\xff")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull {
			t.Fatalf("function %d strict NULL comparison = %v, %v", id, value, err)
		}
		_, err = function.Call(t.Context(), []model.Value{model.StringVal("a"), model.StringVal("a ")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "42P22")
	}
	for _, options := range []builtin.ResolveOptions{{Collation: 950}, {Collation: 951}, {Collation: 100, DefaultCollation: 951}} {
		function, err := builtin.Resolve(1049, options)
		if err != nil {
			t.Fatal(err)
		}
		value, err := function.Call(t.Context(), []model.Value{model.StringVal("Z "), model.StringVal("a")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || value.IntVal != 1 {
			t.Fatalf("resolved bpchar ordering = %v, %v", value, err)
		}
	}
}

func TestCharacterOutputAdmissionAndCancellation(t *testing.T) {
	function := callable(t, "bpchar", 1042, 23, 16)
	args := []model.Value{model.StringVal("é"), model.Int64Val(10485764), model.BoolVal(false)}
	_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = function.Call(ctx, args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled coercion = %v", err)
	}
	for _, tc := range []struct {
		name  string
		text  string
		limit int64
	}{
		{"long input", strings.Repeat("é", 10000), 10000},
		{"long padding", "", 10000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 3}
			_, err := function.Call(interrupted, []model.Value{model.StringVal(tc.text), model.Int64Val(tc.limit + 4), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted coercion = %v", err)
			}
		})
	}
}

type characterCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *characterCancelContext) Err() error {
	c.remaining--
	if c.remaining == 0 {
		c.cancel()
	}
	return c.Context.Err()
}
