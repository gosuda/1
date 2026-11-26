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

func overlayCallable(t *testing.T, id model.FunctionID) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func TestOverlayTextCharacterBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, source, replacement, want string
		id                              model.FunctionID
		start, length                   int64
	}{
		{"default replacement characters", "aé🙂界z", "Ωλ", "aΩλ界z", 1405, 2, 0},
		{"explicit character length", "aé🙂界z", "Ω", "aΩz", 1404, 2, 3},
		{"combining mark counts separately", "aé🙂界z", "e\u0301", "ae\u0301界z", 1405, 2, 0},
		{"zero inserts", "é🙂界", "λ", "éλ🙂界", 1404, 2, 0},
		{"negative overlaps", "é🙂界", "λ", "é🙂λ🙂界", 1404, 3, -1},
		{"negative clamps suffix", "é🙂界", "λ", "éλé🙂界", 1404, 2, -10},
		{"minimum length", "é🙂界", "λ", "λé🙂界", 1404, 1, math.MinInt32},
		{"beyond end appends", "é🙂", "λ", "é🙂λ", 1405, 99, 0},
		{"beyond end with overlap", "abc", "X", "abcXbc", 1404, 6, -4},
		{"maximum start empty replacement", "é🙂", "", "é🙂", 1405, math.MaxInt32, 0},
		{"maximum valid sum", "é🙂", "λ", "é🙂λ", 1404, math.MaxInt32 - 1, 1},
		{"empty source", "", "λ🙂", "λ🙂", 1405, 1, 0},
		{"empty replacement inserts nothing", "é🙂界", "", "é🙂界", 1405, 2, 0},
		{"full deletion", "é🙂界", "", "", 1404, 1, 3},
		{"empty result", "", "", "", 1405, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := overlayCallable(t, tc.id)
			args := []model.Value{model.StringVal(tc.source), model.StringVal(tc.replacement), model.Int64Val(tc.start)}
			if tc.id == 1404 {
				args = append(args, model.Int64Val(tc.length))
			}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeString || value.StrVal != tc.want {
				t.Fatalf("overlay = %v, %v; want %q", value, err, tc.want)
			}
			if tc.want != "" {
				_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
				requireSQLState(t, err, "54000")
			}
		})
	}
}

func TestOverlayByteaByteBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, source, replacement, want string
		id                              model.FunctionID
		start, length                   int64
	}{
		{"default replacement bytes", "aé🙂z", "λ", "aλ🙂z", 752, 2, 0},
		{"explicit splits character", "aé🙂z", "X", "a\xc3X🙂z", 749, 3, 1},
		{"binary replacement", "\x00\xff\x7f\x01", "\x80\x00", "\x00\x80\x00\x01", 752, 2, 0},
		{"zero inserts", "\x00\xff", "\x80", "\x00\x80\xff", 749, 2, 0},
		{"negative overlaps", "abc", "X", "abXbc", 749, 3, -1},
		{"negative clamps suffix", "abc", "X", "aXabc", 749, 2, -10},
		{"minimum length", "abc", "X", "Xabc", 749, 1, math.MinInt32},
		{"beyond end appends", "ab", "X", "abX", 752, 99, 0},
		{"beyond end with overlap", "abc", "X", "abcXbc", 749, 6, -4},
		{"maximum start empty replacement", "ab", "", "ab", 752, math.MaxInt32, 0},
		{"maximum valid sum", "ab", "X", "abX", 749, math.MaxInt32 - 1, 1},
		{"empty source", "", "\xff", "\xff", 752, 1, 0},
		{"empty replacement inserts nothing", "abc", "", "abc", 752, 2, 0},
		{"full deletion", "abc", "", "", 749, 1, 3},
		{"empty result", "", "", "", 752, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := overlayCallable(t, tc.id)
			source, replacement := []byte(tc.source), []byte(tc.replacement)
			args := []model.Value{model.BytesVal(source), model.BytesVal(replacement), model.Int64Val(tc.start)}
			if tc.id == 749 {
				args = append(args, model.Int64Val(tc.length))
			}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.Type != model.TypeBytes || string(value.BytesVal) != tc.want {
				t.Fatalf("overlay = %v, %v; want %x", value, err, tc.want)
			}
			if string(source) != tc.source || string(replacement) != tc.replacement {
				t.Fatal("overlay mutated a borrowed operand")
			}
			if tc.want != "" {
				_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
				requireSQLState(t, err, "54000")
			}
			for _, input := range [][]byte{source, replacement} {
				for i := range input {
					input[i] ^= 0xff
				}
			}
			if string(value.BytesVal) != tc.want {
				t.Fatalf("operand mutation changed overlay result to %x; want %x", value.BytesVal, tc.want)
			}
		})
	}
}

func TestOverlayTextOwnsUnchangedOperands(t *testing.T) {
	for _, tc := range []struct {
		name, source, replacement, want string
	}{
		{"source only", strings.Clone("é🙂"), "", "é🙂"},
		{"replacement only", "", strings.Clone("λ界"), "λ界"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := overlayCallable(t, 1405)
			args := []model.Value{model.StringVal(tc.source), model.StringVal(tc.replacement), model.Int64Val(1)}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != tc.want {
				t.Fatalf("overlay = %v, %v; want %q", value, err, tc.want)
			}
			result := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
			for _, input := range []string{tc.source, tc.replacement} {
				start := uintptr(unsafe.Pointer(unsafe.StringData(input)))
				if result >= start && result < start+uintptr(len(input)) {
					t.Fatal("overlay result retains borrowed text storage")
				}
			}
		})
	}
}

func TestOverlayRejectsInvalidPositions(t *testing.T) {
	for _, id := range []model.FunctionID{1404, 1405, 749, 752} {
		for _, tc := range []struct {
			name          string
			start, length int64
			state         string
		}{
			{"zero start", 0, math.MaxInt32, "22011"},
			{"minimum start", math.MinInt32, math.MinInt32, "22011"},
			{"sum overflow", math.MaxInt32, 1, "22003"},
		} {
			t.Run(fmt.Sprintf("%d/%s", id, tc.name), func(t *testing.T) {
				function := overlayCallable(t, id)
				args := []model.Value{model.StringVal("abc"), model.StringVal("X"), model.Int64Val(tc.start)}
				if id == 749 || id == 752 {
					args[0], args[1] = model.BytesVal([]byte("abc")), model.BytesVal([]byte("X"))
				}
				if function.Signature().ArgumentCount == 4 {
					args = append(args, model.Int64Val(tc.length))
				}
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, tc.state)
			})
		}
	}
}

func TestOverlayTextRejectsInvalidOperands(t *testing.T) {
	for _, id := range []model.FunctionID{1404, 1405} {
		for _, invalid := range []string{"valid\xff", "valid\x00"} {
			for index := range 2 {
				function := overlayCallable(t, id)
				args := []model.Value{model.StringVal("abc"), model.StringVal(""), model.Int64Val(1)}
				if id == 1404 {
					args = append(args, model.Int64Val(math.MaxInt32-1))
				}
				args[index] = model.StringVal(invalid)
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22021")
			}
		}
	}
}

func TestOverlayStrictNullPrecedesKernelErrors(t *testing.T) {
	for _, id := range []model.FunctionID{1404, 1405, 749, 752} {
		function := overlayCallable(t, id)
		for nullIndex := range int(function.Signature().ArgumentCount) {
			args := []model.Value{model.StringVal("\xff"), model.StringVal("\x00"), model.Int64Val(0)}
			if id == 749 || id == 752 {
				args[0], args[1] = model.BytesVal([]byte("abc")), model.BytesVal([]byte("X"))
			}
			if function.Signature().ArgumentCount == 4 {
				args = append(args, model.Int64Val(math.MaxInt32))
			}
			wantType := args[0].Type
			args[nullIndex] = model.NullVal(args[nullIndex].Type)
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || !value.IsNull || value.Type != wantType {
				t.Fatalf("function %d NULL operand %d = %v, %v; want typed NULL", id, nullIndex, value, err)
			}
		}
	}
}

func TestOverlayRejectsOutOfRangeIntegerArguments(t *testing.T) {
	for _, id := range []model.FunctionID{1404, 1405, 749, 752} {
		function := overlayCallable(t, id)
		for index := 2; index < int(function.Signature().ArgumentCount); index++ {
			for _, invalid := range []int64{math.MinInt32 - 1, math.MaxInt32 + 1} {
				args := []model.Value{model.StringVal("abc"), model.StringVal("X"), model.Int64Val(1)}
				if id == 749 || id == 752 {
					args[0], args[1] = model.BytesVal([]byte("abc")), model.BytesVal([]byte("X"))
				}
				if function.Signature().ArgumentCount == 4 {
					args = append(args, model.Int64Val(1))
				}
				args[index] = model.Int64Val(invalid)
				_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 16, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "22003")
			}
		}
	}
}

func TestOverlayCancellation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		id          model.FunctionID
		args        []model.Value
		checkpoints int
	}{
		{"source validation", 1404, []model.Value{model.StringVal(strings.Repeat("é", 32768)), model.StringVal("X"), model.Int64Val(1), model.Int64Val(32768)}, 4},
		{"replacement validation", 1405, []model.Value{model.StringVal(""), model.StringVal(strings.Repeat("界", 32768)), model.Int64Val(1)}, 4},
		{"overlapping text positions", 1404, []model.Value{model.StringVal(strings.Repeat("x", 32768)), model.StringVal("X"), model.Int64Val(25000), model.Int64Val(-20000)}, 14},
		{"text output", 1404, []model.Value{model.StringVal(""), model.StringVal(strings.Repeat("x", 32768)), model.Int64Val(1), model.Int64Val(0)}, 12},
		{"byte source output", 749, []model.Value{model.BytesVal(bytes.Repeat([]byte{0xff}, 32768)), model.BytesVal(nil), model.Int64Val(1), model.Int64Val(0)}, 4},
		{"byte replacement output", 752, []model.Value{model.BytesVal(nil), model.BytesVal(bytes.Repeat([]byte{0xff}, 32768)), model.Int64Val(1)}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := overlayCallable(t, tc.id)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, err := function.Call(ctx, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled overlay = %v", err)
			}
			ctx, cancel = context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: tc.checkpoints}
			_, err = function.Call(interrupted, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted overlay = %v", err)
			}
		})
	}
}
