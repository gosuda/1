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

func textPaddingCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireOwnedPaddingText(t *testing.T, function builtin.Callable, args []model.Value, want string) {
	t.Helper()
	value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || value.Type != model.TypeString || value.IsNull || value.StrVal != want {
		t.Fatalf("function %d result = %v, %v; want %q", function.Signature().ID, value, err, want)
	}
	if want == "" {
		return
	}
	result := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
	for _, argument := range args {
		start := uintptr(unsafe.Pointer(unsafe.StringData(argument.StrVal)))
		if result >= start && result < start+uintptr(len(argument.StrVal)) {
			t.Fatal("result retains borrowed string storage")
		}
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestTextPaddingCharacterLengths(t *testing.T) {
	for _, tc := range []struct {
		name, source, fill, want string
		id                       model.FunctionID
		length                   int64
	}{
		{"left Unicode fill", "界🙂", "éλ🙂", "éλ🙂é界🙂", 873, 6},
		{"right Unicode fill", "界🙂", "éλ🙂", "界🙂éλ🙂é", 874, 6},
		{"left default fill", "界🙂", "", "  界🙂", 879, 4},
		{"right default fill", "界🙂", "", "界🙂  ", 880, 4},
		{"left truncation", "é🙂界", "λ", "é🙂", 873, 2},
		{"right truncation", "é🙂界", "λ", "é🙂", 874, 2},
		{"default truncation", "é🙂界", "", "é🙂", 880, 2},
		{"negative length", "é🙂", "界", "", 873, -1},
		{"minimum length", "é🙂", "", "", 880, math.MinInt32},
		{"zero length", "é🙂", "界", "", 874, 0},
		{"empty source", "", "é🙂", "é🙂é", 873, 3},
		{"empty fill cannot grow", "é🙂", "", "é🙂", 874, math.MaxInt32},
		{"empty fill still truncates", "é🙂界", "", "é🙂", 873, 2},
		{"both strings empty", "", "", "", 874, math.MaxInt32},
		{"unchanged input", "é🙂", "界", "é🙂", 873, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := textPaddingCallable(t, tc.id)
			args := []model.Value{model.StringVal(tc.source), model.Int64Val(tc.length)}
			if function.Signature().ArgumentCount == 3 {
				args = append(args, model.StringVal(tc.fill))
			}
			requireOwnedPaddingText(t, function, args, tc.want)
		})
	}
}

func TestTextPaddingRepeatedFillCrossesCopyBoundaries(t *testing.T) {
	longFill := strings.Repeat("é", 2500) + "x"
	for _, tc := range []struct {
		name, fill, padding string
		length              int64
	}{
		{"short fill", "é界", strings.Repeat("é界", 3000) + "é", 6002},
		{"long fill", longFill, strings.Repeat(longFill, 3) + "é", 7505},
	} {
		for _, id := range []model.FunctionID{873, 874} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, id), func(t *testing.T) {
				want := tc.padding + "!"
				if id == 874 {
					want = "!" + tc.padding
				}
				args := []model.Value{model.StringVal("!"), model.Int64Val(tc.length), model.StringVal(tc.fill)}
				requireOwnedPaddingText(t, textPaddingCallable(t, id), args, want)
			})
		}
	}
}

func TestTextTrimmingCharacterSets(t *testing.T) {
	for _, tc := range []struct {
		name, source, set, want string
		id                      model.FunctionID
	}{
		{"leading set", "é🙂界é🙂", "🙂é", "界é🙂", 875},
		{"trailing set", "é🙂界é🙂", "🙂é", "é🙂界", 876},
		{"both ends set", "é🙂界é🙂", "🙂é", "界", 884},
		{"leading default", "  \t界 \n  ", "", "\t界 \n  ", 881},
		{"trailing default", "  \t界 \n  ", "", "  \t界 \n", 882},
		{"both ends default", "  \t界 \n  ", "", "\t界 \n", 885},
		{"default preserves Unicode spaces", " \u00a0界\u00a0 ", "", "\u00a0界\u00a0", 885},
		{"set is not a substring", "🙂é🙂界é", "é🙂", "界", 884},
		{"shared UTF8 prefix is not a match", "ê界ê", "é", "ê界ê", 884},
		{"no Unicode normalization", "é界é", "e\u0301", "é界é", 884},
		{"replacement character is valid", "�界�", "�", "界", 884},
		{"empty set", "é🙂界", "", "é🙂界", 875},
		{"empty source", "", "é🙂", "", 876},
		{"all characters trimmed", "é🙂é", "🙂é🙂", "", 884},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := textPaddingCallable(t, tc.id)
			args := []model.Value{model.StringVal(tc.source)}
			if function.Signature().ArgumentCount == 2 {
				args = append(args, model.StringVal(tc.set))
			}
			requireOwnedPaddingText(t, function, args, tc.want)
		})
	}
}

func TestByteaTrimmingByteSets(t *testing.T) {
	for _, tc := range []struct {
		name              string
		id                model.FunctionID
		source, set, want []byte
	}{
		{"both ends", 2015, []byte{0, 0xff, 0xc3, 0xa9, 'x', 0xa9, 0xc3, 0xff, 0}, []byte{0xc3, 0, 0xff}, []byte{0xa9, 'x', 0xa9}},
		{"leading", 6195, []byte{0, 0xff, 0xc3, 0xa9, 'x', 0xff, 0}, []byte{0xc3, 0, 0xff}, []byte{0xa9, 'x', 0xff, 0}},
		{"trailing", 6196, []byte{0, 0xff, 'x', 0xa9, 0xc3, 0xff, 0}, []byte{0xc3, 0, 0xff}, []byte{0, 0xff, 'x', 0xa9}},
		{"empty input", 2015, nil, []byte{0xff}, nil},
		{"empty set", 6195, []byte{0, 0xff}, nil, []byte{0, 0xff}},
		{"all bytes trimmed", 6196, []byte{0xff, 0, 0xff}, []byte{0xff, 0, 0xff}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := textPaddingCallable(t, tc.id)
			original, originalSet := bytes.Clone(tc.source), bytes.Clone(tc.set)
			args := []model.Value{model.BytesVal(tc.source), model.BytesVal(tc.set)}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.Type != model.TypeBytes || value.IsNull || !bytes.Equal(value.BytesVal, tc.want) {
				t.Fatalf("trimmed value = %v, %v; want %x", value, err, tc.want)
			}
			if !bytes.Equal(tc.source, original) || !bytes.Equal(tc.set, originalSet) {
				t.Fatal("call changed borrowed operands")
			}
			if len(tc.want) > 0 {
				_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
				requireSQLState(t, err, "54000")
			}
			for i := range tc.source {
				tc.source[i] ^= 0xff
			}
			for i := range tc.set {
				tc.set[i] ^= 0xff
			}
			if !bytes.Equal(value.BytesVal, tc.want) {
				t.Fatalf("borrowed operand mutation changed result to %x; want %x", value.BytesVal, tc.want)
			}
		})
	}
}

func TestTextPaddingRejectsUnadmittedGrowth(t *testing.T) {
	for _, id := range []model.FunctionID{873, 874, 879, 880} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := textPaddingCallable(t, id)
			args := []model.Value{model.StringVal("é"), model.Int64Val(math.MaxInt32)}
			if function.Signature().ArgumentCount == 3 {
				args = append(args, model.StringVal("🙂"))
			}
			_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestTextPaddingAndTrimmingRejectInvalidText(t *testing.T) {
	for _, id := range []model.FunctionID{873, 874, 875, 876, 879, 880, 881, 882, 884, 885} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := textPaddingCallable(t, id)
			signature := function.Signature()
			for _, invalid := range []string{"\xff", "\x00"} {
				for index := range int(signature.ArgumentCount) {
					if signature.Arguments[index] != 25 {
						continue
					}
					args := make([]model.Value, signature.ArgumentCount)
					for i := range args {
						args[i] = model.StringVal("")
						if signature.Arguments[i] == 23 {
							args[i] = model.Int64Val(0)
						}
					}
					args[index] = model.StringVal(invalid)
					_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireSQLState(t, err, "22021")
				}
			}
		})
	}
}

func TestTextPaddingAndTrimmingStrictNull(t *testing.T) {
	for _, id := range []model.FunctionID{873, 874, 875, 876, 879, 880, 881, 882, 884, 885, 2015, 6195, 6196} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := textPaddingCallable(t, id)
			signature := function.Signature()
			for nullIndex := range int(signature.ArgumentCount) {
				args := make([]model.Value, signature.ArgumentCount)
				for i := range args {
					switch signature.Arguments[i] {
					case 17:
						args[i] = model.BytesVal([]byte{0, 0xff})
					case 23:
						args[i] = model.Int64Val(math.MaxInt32)
					default:
						args[i] = model.StringVal("\xff")
					}
				}
				args[nullIndex] = model.NullVal(args[nullIndex].Type)
				value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull || value.Type != args[0].Type {
					t.Fatalf("NULL argument %d returned %v, %v", nullIndex, value, err)
				}
			}
		})
	}
}

func TestTextPaddingAndTrimmingCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          model.FunctionID
		args        []model.Value
		checkpoints int
	}{
		{"padding input scan", 873, []model.Value{model.StringVal(strings.Repeat("é", 20000)), model.Int64Val(30000), model.StringVal(" ")}, 3},
		{"padding fill scan", 874, []model.Value{model.StringVal(""), model.Int64Val(100000), model.StringVal(strings.Repeat("é", 20000))}, 3},
		{"padding output copy", 873, []model.Value{model.StringVal("é"), model.Int64Val(100000), model.StringVal("x")}, 10},
		{"leading text scan", 875, []model.Value{model.StringVal(strings.Repeat("é", 12000) + "!"), model.StringVal("é")}, 10},
		{"trailing text scan", 882, []model.Value{model.StringVal("!" + strings.Repeat(" ", 20000))}, 10},
		{"text set membership", 884, []model.Value{model.StringVal("界"), model.StringVal(strings.Repeat("é", 10000) + "界")}, 10},
		{"bytea leading scan", 6195, []model.Value{model.BytesVal(bytes.Repeat([]byte{0xff}, 20000)), model.BytesVal([]byte{0xff})}, 4},
		{"bytea trailing scan", 6196, []model.Value{model.BytesVal(bytes.Repeat([]byte{0xff}, 20000)), model.BytesVal([]byte{0xff})}, 4},
		{"bytea set scan", 2015, []model.Value{model.BytesVal([]byte{'x'}), model.BytesVal(bytes.Repeat([]byte{0xff}, 20000))}, 4},
		{"bytea output copy", 2015, []model.Value{model.BytesVal(bytes.Repeat([]byte{'x'}, 20000)), model.BytesVal([]byte{'!'})}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := textPaddingCallable(t, tc.id)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := function.Call(ctx, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled call = %v", err)
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: tc.checkpoints}
			_, err = function.Call(interrupted, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted call = %v", err)
			}
		})
	}
}
