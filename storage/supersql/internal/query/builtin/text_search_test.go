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

func TestTextPositionCountsCharacters(t *testing.T) {
	for _, id := range []model.FunctionID{849, 868} {
		t.Run(fmt.Sprint(id), func(t *testing.T) {
			function, err := builtin.Resolve(id, builtin.ResolveOptions{Collation: 950})
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name, input, needle string
				want                int64
			}{
				{"Unicode prefix", "aé🙂e\u0301界z", "界", 6},
				{"combining character", "e\u0301x", "\u0301", 2},
				{"first occurrence", "éxéx", "éx", 1},
				{"empty needle", "é", "", 1},
				{"both empty", "", "", 1},
				{"empty input", "", "x", 0},
				{"needle longer", "é", "éx", 0},
				{"absent", "é🙂", "e", 0},
				{"no normalization", "e\u0301", "é", 0},
				{"match across scan chunk", strings.Repeat("a", 4095) + "界🙂z", "a界🙂", 4095},
				{"long needle at end", strings.Repeat("ab", 5000) + "x", strings.Repeat("ab", 3000) + "x", 4001},
			} {
				t.Run(tc.name, func(t *testing.T) {
					value, err := function.Call(t.Context(), []model.Value{model.StringVal(tc.input), model.StringVal(tc.needle)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					if err != nil || value.Type != model.TypeInt64 || value.IsNull || value.IntVal != tc.want {
						t.Fatalf("position = %v, %v; want %d", value, err, tc.want)
					}
				})
			}
		})
	}
}

func TestByteaPositionCountsArbitraryBytes(t *testing.T) {
	function, err := builtin.Resolve(2014, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		input, needle []byte
		want          int64
	}{
		{"NUL", []byte{0xff, 0, 0x80, 0}, []byte{0, 0x80}, 2},
		{"UTF8 continuation byte", []byte("é🙂"), []byte{0x9f, 0x99}, 4},
		{"last byte", []byte{0, 0xff}, []byte{0xff}, 2},
		{"empty needle", []byte{0xff}, nil, 1},
		{"both empty", nil, nil, 1},
		{"empty input", nil, []byte{0}, 0},
		{"needle longer", []byte{0}, []byte{0, 0}, 0},
		{"absent", []byte{0, 0xff}, []byte{0x80}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := function.Call(t.Context(), []model.Value{model.BytesVal(tc.input), model.BytesVal(tc.needle)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.Type != model.TypeInt64 || value.IsNull || value.IntVal != tc.want {
				t.Fatalf("position = %v, %v; want %d", value, err, tc.want)
			}
		})
	}
	for i := range 2 {
		args := []model.Value{model.BytesVal([]byte{0xff}), model.BytesVal([]byte{0xff})}
		args[i] = model.NullVal(model.TypeBytes)
		value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull || value.Type != model.TypeInt64 {
			t.Fatalf("NULL argument %d = %v, %v", i, value, err)
		}
	}
}

func TestReplaceTextNonoverlappingMatches(t *testing.T) {
	function, err := builtin.Resolve(2087, builtin.ResolveOptions{Collation: 950})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, input, from, to, want string
	}{
		{"Unicode expansion", "aé🙂éz", "é", "界界", "a界界🙂界界z"},
		{"overlapping candidates", "aaaaa", "aa", "x", "xxa"},
		{"replacement not searched", "aa", "a", "aa", "aaaa"},
		{"deletion", "éxé", "é", "", "x"},
		{"delete all", "aaaa", "aa", "", ""},
		{"empty needle", "é🙂", "", "x", "é🙂"},
		{"empty input", "", "a", "x", ""},
		{"absent", "é🙂", "x", "large", "é🙂"},
		{"literal replacement", "aa", "a", `$1\x`, `$1\x$1\x`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []model.Value{model.StringVal(tc.input), model.StringVal(tc.from), model.StringVal(tc.to)}
			requireSearchTextResult(t, function, args, tc.want)
		})
	}
}

func TestTranslateTextCharacterMapping(t *testing.T) {
	function, err := builtin.Resolve(878, builtin.ResolveOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, input, from, to, want string
	}{
		{"Unicode mapping and deletion", "é🙂界x", "🙂é界", "ab", "bax"},
		{"Unicode expansion", "aba", "ab", "🙂界", "🙂界🙂"},
		{"first duplicate mapping", "é🙂é", "éé🙂", "甲乙丙", "甲丙甲"},
		{"unmapped suffix deleted", "abcabc", "abc", "x", "xx"},
		{"empty replacement deletes", "é🙂é", "é🙂", "", ""},
		{"extra replacements ignored", "aba", "a", "xy", "xbx"},
		{"empty source mapping", "é🙂", "", "x", "é🙂"},
		{"absent mapping", "é🙂", "ab", "cd", "é🙂"},
		{"empty input", "", "a", "🙂", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []model.Value{model.StringVal(tc.input), model.StringVal(tc.from), model.StringVal(tc.to)}
			requireSearchTextResult(t, function, args, tc.want)
		})
	}
}

func TestSplitPartFieldBoundaries(t *testing.T) {
	function, err := builtin.Resolve(2088, builtin.ResolveOptions{Collation: 950})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, input, separator string
		field                  int64
		want                   string
	}{
		{"Unicode separator", "é🙂界🙂z", "🙂", 2, "界"},
		{"negative middle", "é🙂界🙂z", "🙂", -2, "界"},
		{"negative first", "a:b:c", ":", -3, "a"},
		{"last field", "a:b:c", ":", -1, "c"},
		{"leading empty", ":a:", ":", 1, ""},
		{"trailing empty", ":a:", ":", -1, ""},
		{"adjacent separators", "a::b", ":", 2, ""},
		{"overlapping separators", "aaaaa", "aa", -1, "a"},
		{"overlap middle", "aaaaa", "aa", -2, ""},
		{"positive past end", "a:b", ":", 3, ""},
		{"negative past end", "a:b", ":", -3, ""},
		{"minimum index", "a:b", ":", math.MinInt32, ""},
		{"maximum index", "a:b", ":", math.MaxInt32, ""},
		{"empty separator first", "é🙂", "", 1, "é🙂"},
		{"empty separator last", "é🙂", "", -1, "é🙂"},
		{"empty separator missing", "é🙂", "", -2, ""},
		{"separator absent", "é🙂", ":", -1, "é🙂"},
		{"empty input", "", ":", 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := []model.Value{model.StringVal(tc.input), model.StringVal(tc.separator), model.Int64Val(tc.field)}
			requireSearchTextResult(t, function, args, tc.want)
		})
	}
	for _, separator := range []string{"", ":"} {
		_, err := function.Call(t.Context(), []model.Value{model.StringVal(""), model.StringVal(separator), model.Int64Val(0)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22023")
	}
}

func requireSearchTextResult(t *testing.T, function builtin.Callable, args []model.Value, want string) {
	t.Helper()
	value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want)), MemoryBytes: 64 << 20})
	if err != nil || value.Type != model.TypeString || value.IsNull || value.StrVal != want {
		t.Fatalf("%s = %v, %v; want %q", function.Signature().Name, value, err, want)
	}
	if want == "" {
		return
	}
	result := uintptr(unsafe.Pointer(unsafe.StringData(value.StrVal)))
	for _, argument := range args {
		start := uintptr(unsafe.Pointer(unsafe.StringData(argument.StrVal)))
		if result >= start && result-start < uintptr(len(argument.StrVal)) {
			t.Fatal("result retains borrowed argument storage")
		}
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(want) - 1), MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestTextSearchInvalidOperandsAndStrictNull(t *testing.T) {
	for _, tc := range []struct {
		id   model.FunctionID
		args []model.Value
	}{
		{849, []model.Value{model.StringVal(""), model.StringVal("")}},
		{868, []model.Value{model.StringVal(""), model.StringVal("")}},
		{2087, []model.Value{model.StringVal(""), model.StringVal(""), model.StringVal("")}},
		{878, []model.Value{model.StringVal(""), model.StringVal(""), model.StringVal("")}},
		{2088, []model.Value{model.StringVal(""), model.StringVal(""), model.Int64Val(1)}},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{Collation: 950})
			if err != nil {
				t.Fatal(err)
			}
			for i, saved := range tc.args {
				if saved.Type != model.TypeString {
					continue
				}
				for _, invalid := range []string{"\xff", "\x00"} {
					tc.args[i] = model.StringVal(invalid)
					_, err = function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					requireSQLState(t, err, "22021")
					other := (i + 1) % len(tc.args)
					otherValue := tc.args[other]
					tc.args[other] = model.NullVal(otherValue.Type)
					value, err := function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
					if err != nil || !value.IsNull {
						t.Fatalf("NULL with invalid argument %d = %v, %v", i, value, err)
					}
					tc.args[other] = otherValue
				}
				tc.args[i] = saved
			}
		})
	}
}

func TestSplitPartNullAndErrorPrecedence(t *testing.T) {
	function, err := builtin.Resolve(2088, builtin.ResolveOptions{Collation: 950})
	if err != nil {
		t.Fatal(err)
	}
	args := []model.Value{model.NullVal(model.TypeString), model.StringVal("\xff"), model.Int64Val(0)}
	value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || !value.IsNull || value.Type != model.TypeString {
		t.Fatalf("NULL with zero index = %v, %v", value, err)
	}
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: -1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22023")
	args[2] = model.Int64Val(int64(math.MaxInt32) + 1)
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22003")
	args[2] = model.StringVal("1")
	_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "42804")
}

func TestTextSearchLongScansRespectCancellation(t *testing.T) {
	long := strings.Repeat("é", 10000)
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
	}{
		{"text position", 849, []model.Value{model.StringVal(long), model.StringVal("x")}},
		{"strpos", 868, []model.Value{model.StringVal(long), model.StringVal("x")}},
		{"bytea position", 2014, []model.Value{model.BytesVal([]byte(long)), model.BytesVal([]byte{0})}},
		{"replacement", 2087, []model.Value{model.StringVal(long), model.StringVal("é"), model.StringVal("x")}},
		{"translation", 878, []model.Value{model.StringVal(long), model.StringVal("é"), model.StringVal("x")}},
		{"negative field", 2088, []model.Value{model.StringVal(long), model.StringVal("é"), model.Int64Val(-2)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{Collation: 950})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			interrupted := &characterCancelContext{Context: ctx, cancel: cancel, remaining: 3}
			_, err = function.Call(interrupted, tc.args, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted search = %v", err)
			}
			_, err = function.Call(ctx, tc.args, builtin.CallBudget{OutputBytes: -1, MemoryBytes: 64 << 20})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation before invalid budget = %v", err)
			}
		})
	}
}

func TestTextSearchCollationResolution(t *testing.T) {
	for _, tc := range []struct {
		id   model.FunctionID
		args []model.Value
	}{
		{849, []model.Value{model.StringVal("a"), model.StringVal("a")}},
		{868, []model.Value{model.StringVal("a"), model.StringVal("a")}},
		{2087, []model.Value{model.StringVal("a"), model.StringVal("a"), model.StringVal("x")}},
		{2088, []model.Value{model.StringVal("a:b"), model.StringVal(":"), model.Int64Val(2)}},
	} {
		t.Run(fmt.Sprint(tc.id), func(t *testing.T) {
			for _, options := range []builtin.ResolveOptions{
				{}, {Collation: 100}, {Collation: 100, DefaultCollation: 100}, {Collation: 12345},
			} {
				function, err := builtin.Resolve(tc.id, options)
				if err != nil {
					t.Fatal(err)
				}
				_, err = function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: 8, MemoryBytes: 64 << 20})
				requireSQLState(t, err, "42P22")
				saved := tc.args[0]
				tc.args[0] = model.NullVal(model.TypeString)
				value, err := function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
				if err != nil || !value.IsNull {
					t.Fatalf("NULL with unresolved collation = %v, %v", value, err)
				}
				tc.args[0] = saved
			}
			for _, options := range []builtin.ResolveOptions{
				{Collation: 951}, {Collation: 100, DefaultCollation: 950}, {Collation: 100, DefaultCollation: 951},
			} {
				function, err := builtin.Resolve(tc.id, options)
				if err != nil {
					t.Fatal(err)
				}
				value, err := function.Call(t.Context(), tc.args, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
				if tc.id == 849 || tc.id == 868 {
					if err != nil || value.IsNull || value.IntVal != 1 {
						t.Fatalf("resolved position = %v, %v; want 1", value, err)
					}
				} else {
					want := "x"
					if tc.id == 2088 {
						want = "b"
					}
					if err != nil || value.IsNull || value.StrVal != want {
						t.Fatalf("resolved search = %v, %v; want %q", value, err, want)
					}
				}
			}
		})
	}
}

func TestTextSearchBudgetsOnlyReturnedBytes(t *testing.T) {
	long := strings.Repeat("é:", 10000)
	for _, tc := range []struct {
		name string
		id   model.FunctionID
		args []model.Value
		want string
	}{
		{"replacement deletion", 2087, []model.Value{model.StringVal(long + "x"), model.StringVal("é:"), model.StringVal("")}, "x"},
		{"translation deletion", 878, []model.Value{model.StringVal(long + "x"), model.StringVal("é:"), model.StringVal("")}, "x"},
		{"last field", 2088, []model.Value{model.StringVal(long + "x"), model.StringVal(":"), model.Int64Val(-1)}, "x"},
		{"middle field", 2088, []model.Value{model.StringVal(long + "x"), model.StringVal(":"), model.Int64Val(-2)}, "é"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{Collation: 950})
			if err != nil {
				t.Fatal(err)
			}
			requireSearchTextResult(t, function, tc.args, tc.want)
		})
	}
}
