package builtin_test

import (
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

func stringArrayCallable(t *testing.T, id model.FunctionID, options builtin.ResolveOptions) builtin.Callable {
	t.Helper()
	function, err := builtin.Resolve(id, options)
	if err != nil {
		t.Fatal(err)
	}
	return function
}

func requireStringArray(t *testing.T, value model.Value, want []model.Value) {
	t.Helper()
	if value.Type != model.TypeArray || value.IsNull {
		t.Fatalf("string_to_array result = %v; want nonnull array", value)
	}
	if len(value.ArrayVal) != len(want) {
		t.Fatalf("elements = %v; want %v", value.ArrayVal, want)
	}
	if len(want) == 0 {
		if len(value.Dimensions) != 0 {
			t.Fatalf("empty array dimensions = %v; want zero dimensions", value.Dimensions)
		}
		return
	}
	if len(value.Dimensions) != 1 || value.Dimensions[0].Length != int32(len(want)) || value.Dimensions[0].LowerBound != 1 {
		t.Fatalf("dimensions = %v; want [%d] with lower bound 1", value.Dimensions, len(want))
	}
	for i, expected := range want {
		actual := value.ArrayVal[i]
		if actual.Type != model.TypeString || actual.IsNull != expected.IsNull || actual.StrVal != expected.StrVal {
			t.Errorf("element %d = %v; want %v", i, actual, expected)
		}
	}
}

func TestStringToArrayDelimiterModes(t *testing.T) {
	function := stringArrayCallable(t, 394, builtin.ResolveOptions{Collation: 950})
	for _, tc := range []struct {
		name      string
		input     string
		delimiter model.Value
		want      []model.Value
	}{
		{"UTF8 codepoints not graphemes", "é🙂e\u0301界", model.NullVal(model.TypeString), []model.Value{model.StringVal("é"), model.StringVal("🙂"), model.StringVal("e"), model.StringVal("\u0301"), model.StringVal("界")}},
		{"empty delimiter keeps source", "é🙂e\u0301界", model.StringVal(""), []model.Value{model.StringVal("é🙂e\u0301界")}},
		{"multicodepoint delimiter", "é🙂界e\u0301🙂界z", model.StringVal("🙂界"), []model.Value{model.StringVal("é"), model.StringVal("e\u0301"), model.StringVal("z")}},
		{"leading adjacent trailing delimiters", ":é::界:", model.StringVal(":"), []model.Value{model.StringVal(""), model.StringVal("é"), model.StringVal(""), model.StringVal("界"), model.StringVal("")}},
		{"nonoverlapping matches", "aaaaa", model.StringVal("aa"), []model.Value{model.StringVal(""), model.StringVal(""), model.StringVal("a")}},
		{"delimiter equals source", "🙂", model.StringVal("🙂"), []model.Value{model.StringVal(""), model.StringVal("")}},
		{"delimiter longer than source", "é", model.StringVal("é🙂"), []model.Value{model.StringVal("é")}},
		{"byte exact matching", "e\u0301:É", model.StringVal("é"), []model.Value{model.StringVal("e\u0301:É")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input), tc.delimiter}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireStringArray(t, value, tc.want)
		})
	}
}

func TestStringToArrayNullSourceAndEmptyShape(t *testing.T) {
	for _, id := range []model.FunctionID{376, 394} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function := stringArrayCallable(t, id, builtin.ResolveOptions{})
			for _, delimiter := range []model.Value{model.NullVal(model.TypeString), model.StringVal(""), model.StringVal(":")} {
				args := []model.Value{model.NullVal(model.TypeString), delimiter}
				if id == 376 {
					args = append(args, model.StringVal(""))
				}
				value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull || value.Type != model.TypeArray {
					t.Fatalf("NULL source with delimiter %v = %v, %v", delimiter, value, err)
				}
				args[0] = model.StringVal("")
				value, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil {
					t.Fatal(err)
				}
				requireStringArray(t, value, nil)
			}
		})
	}
}

func TestStringToArrayNullMarkers(t *testing.T) {
	function := stringArrayCallable(t, 376, builtin.ResolveOptions{Collation: 950})
	for _, tc := range []struct {
		name      string
		input     string
		delimiter model.Value
		marker    model.Value
		want      []model.Value
	}{
		{"NULL marker disables replacement", "NULL::x", model.StringVal(":"), model.NullVal(model.TypeString), []model.Value{model.StringVal("NULL"), model.StringVal(""), model.StringVal("x")}},
		{"case sensitive marker", "NULL:null:NULL", model.StringVal(":"), model.StringVal("NULL"), []model.Value{model.NullVal(model.TypeString), model.StringVal("null"), model.NullVal(model.TypeString)}},
		{"empty marker replaces empty fields", ":é::", model.StringVal(":"), model.StringVal(""), []model.Value{model.NullVal(model.TypeString), model.StringVal("é"), model.NullVal(model.TypeString), model.NullVal(model.TypeString)}},
		{"whole source marker", "é🙂", model.StringVal(""), model.StringVal("é🙂"), []model.Value{model.NullVal(model.TypeString)}},
		{"codepoint marker", "🙂é🙂", model.NullVal(model.TypeString), model.StringVal("🙂"), []model.Value{model.NullVal(model.TypeString), model.StringVal("é"), model.NullVal(model.TypeString)}},
		{"marker cannot span codepoint fields", "é🙂", model.NullVal(model.TypeString), model.StringVal("é🙂"), []model.Value{model.StringVal("é"), model.StringVal("🙂")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input), tc.delimiter, tc.marker}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireStringArray(t, value, tc.want)
		})
	}
}

func TestStringToArrayCollationFollowsComparison(t *testing.T) {
	for _, options := range []builtin.ResolveOptions{{}, {Collation: 100}, {Collation: 100, DefaultCollation: 100}, {Collation: 12345}} {
		function := stringArrayCallable(t, 376, options)
		for _, tc := range []struct {
			name      string
			delimiter model.Value
			marker    model.Value
			want      []model.Value
		}{
			{"codepoints need no collation", model.NullVal(model.TypeString), model.NullVal(model.TypeString), []model.Value{model.StringVal("é"), model.StringVal("🙂")}},
			{"whole source needs no collation", model.StringVal(""), model.NullVal(model.TypeString), []model.Value{model.StringVal("é🙂")}},
		} {
			t.Run(fmt.Sprintf("%v/%s", options, tc.name), func(t *testing.T) {
				value, err := function.Call(t.Context(), []model.Value{model.StringVal("é🙂"), tc.delimiter, tc.marker}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
				if err != nil {
					t.Fatal(err)
				}
				requireStringArray(t, value, tc.want)
			})
		}
		for _, args := range [][]model.Value{
			{model.StringVal("é🙂"), model.StringVal("absent"), model.NullVal(model.TypeString)},
			{model.StringVal("é🙂"), model.StringVal(""), model.StringVal("different length")},
			{model.StringVal("é🙂"), model.NullVal(model.TypeString), model.StringVal("")},
		} {
			_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "42P22")
		}
	}
	for _, options := range []builtin.ResolveOptions{{Collation: 950}, {Collation: 951}, {Collation: 100, DefaultCollation: 950}, {Collation: 100, DefaultCollation: 951}} {
		function := stringArrayCallable(t, 376, options)
		value, err := function.Call(t.Context(), []model.Value{model.StringVal("é:É"), model.StringVal(":"), model.StringVal("é")}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
		if err != nil {
			t.Fatal(err)
		}
		requireStringArray(t, value, []model.Value{model.NullVal(model.TypeString), model.StringVal("É")})
	}
}

func TestStringToArrayExactOutputBudget(t *testing.T) {
	largeMarker := strings.Repeat("é", 10000)
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
		want []model.Value
	}{
		{"Unicode bytes", 394, []model.Value{model.StringVal("é🙂"), model.NullVal(model.TypeString)}, []model.Value{model.StringVal("é"), model.StringVal("🙂")}},
		{"empty fields still own headers", 394, []model.Value{model.StringVal("::"), model.StringVal(":")}, []model.Value{model.StringVal(""), model.StringVal(""), model.StringVal("")}},
		{"nulls discard source payload", 376, []model.Value{model.StringVal(largeMarker + ":" + largeMarker), model.StringVal(":"), model.StringVal(largeMarker)}, []model.Value{model.NullVal(model.TypeString), model.NullVal(model.TypeString)}},
		{"mixed payload and nulls", 376, []model.Value{model.StringVal("é::🙂"), model.StringVal(":"), model.StringVal("")}, []model.Value{model.StringVal("é"), model.NullVal(model.TypeString), model.StringVal("🙂")}},
		{"large absent delimiter is not retained", 394, []model.Value{model.StringVal("x"), model.StringVal(largeMarker)}, []model.Value{model.StringVal("x")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := stringArrayCallable(t, tc.id, builtin.ResolveOptions{Collation: 950})
			budget := int64(len(tc.want))*int64(unsafe.Sizeof(model.Value{})) + int64(unsafe.Sizeof(model.ArrayDimension{}))
			for _, field := range tc.want {
				budget += int64(len(field.StrVal))
			}
			value, err := function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: budget, MemoryBytes: 64 << 20})
			if err != nil {
				t.Fatal(err)
			}
			requireStringArray(t, value, tc.want)
			measured, err := model.ValueBytes(t.Context(), value, uint64(budget))
			if err != nil || measured != uint64(budget) {
				t.Fatalf("owned bytes = %d, %v; want %d", measured, err, budget)
			}
			if cap(value.ArrayVal) != len(value.ArrayVal) || cap(value.Dimensions) != len(value.Dimensions) {
				t.Fatal("array retains uncharged slice capacity")
			}
			_, err = function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: budget - 1, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}

func TestStringToArrayOwnsResultStorage(t *testing.T) {
	function := stringArrayCallable(t, 376, builtin.ResolveOptions{Collation: 950})
	input := strings.Clone("é::🙂:NULL")
	args := []model.Value{model.StringVal(input), model.StringVal(":"), model.StringVal("NULL")}
	first, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	second, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	want := []model.Value{model.StringVal("é"), model.StringVal(""), model.StringVal("🙂"), model.NullVal(model.TypeString)}
	requireStringArray(t, first, want)
	for i, field := range first.ArrayVal {
		if field.StrVal == "" {
			continue
		}
		start := uintptr(unsafe.Pointer(unsafe.StringData(field.StrVal)))
		for _, argument := range args {
			borrowed := uintptr(unsafe.Pointer(unsafe.StringData(argument.StrVal)))
			if start >= borrowed && start-borrowed < uintptr(len(argument.StrVal)) {
				t.Errorf("element %d retains borrowed argument storage", i)
			}
		}
	}
	first.ArrayVal[0] = model.StringVal("changed")
	first.Dimensions[0].LowerBound = -10
	requireStringArray(t, second, want)
	if args[0].StrVal != input {
		t.Fatal("result mutation changed input")
	}
}

func TestStringToArrayRejectsInvalidUTF8(t *testing.T) {
	function := stringArrayCallable(t, 376, builtin.ResolveOptions{Collation: 950})
	for _, invalid := range []string{"a\x00b", "a\xffb", "a\xc3"} {
		for index := range 3 {
			args := []model.Value{model.StringVal("a:b"), model.StringVal(":"), model.StringVal("x")}
			args[index] = model.StringVal(invalid)
			_, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			requireSQLState(t, err, "22021")
		}
	}
}

func TestStringToArrayCancellation(t *testing.T) {
	marker := strings.Repeat("é", 16000)
	for _, tc := range []struct {
		name string
		args []model.Value
	}{
		{"codepoint fields", []model.Value{model.StringVal(strings.Repeat("é🙂", 4000)), model.NullVal(model.TypeString), model.NullVal(model.TypeString)}},
		{"delimiter fields", []model.Value{model.StringVal(strings.Repeat("é:", 6000)), model.StringVal(":"), model.NullVal(model.TypeString)}},
		{"whole field copy", []model.Value{model.StringVal(strings.Repeat("x", 32768)), model.StringVal(""), model.NullVal(model.TypeString)}},
		{"null marker comparison", []model.Value{model.StringVal(marker), model.StringVal(""), model.StringVal(marker)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function := stringArrayCallable(t, 376, builtin.ResolveOptions{Collation: 950})
			baseline, baselineCancel := context.WithCancel(t.Context())
			defer baselineCancel()
			counter := &characterCancelContext{Context: baseline, cancel: baselineCancel, remaining: math.MaxInt}
			if _, err := function.Call(counter, tc.args, builtin.CallBudget{OutputBytes: 32 << 20, MemoryBytes: 64 << 20}); err != nil {
				t.Fatal(err)
			}
			checks := math.MaxInt - counter.remaining
			for _, checkpoint := range []int{1, checks / 4, checks / 2, 3 * checks / 4, checks} {
				ctx, cancel := context.WithCancel(t.Context())
				interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: checkpoint}
				value, err := function.Call(interrupted, tc.args, builtin.CallBudget{OutputBytes: 32 << 20, MemoryBytes: 64 << 20})
				cancel()
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("interrupted split at %d/%d = %v", checkpoint, checks, err)
				}
				if len(value.ArrayVal) != 0 || len(value.Dimensions) != 0 {
					t.Fatal("interrupted split returned a partial array")
				}
			}
		})
	}
}
