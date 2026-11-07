package builtin_test

import (
	"fmt"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestCharacterCodeUnicodeBoundaries(t *testing.T) {
	ascii := callable(t, "ascii", 25)
	chr := callable(t, "chr", 23)
	for _, tc := range []struct {
		text string
		code int64
	}{
		{"A", 65}, {"é", 233}, {"界", 30028}, {"🙂", 128578},
		{"\ud7ff", 0xd7ff}, {"\ue000", 0xe000}, {"\U0010ffff", 0x10ffff},
	} {
		t.Run(fmt.Sprintf("U+%X", tc.code), func(t *testing.T) {
			value, err := ascii.Call(t.Context(), []model.Value{model.StringVal(tc.text + "suffix")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
			if err != nil || value.IntVal != tc.code {
				t.Fatalf("ascii = %v, %v; want %d", value, err, tc.code)
			}
			value, err = chr.Call(t.Context(), []model.Value{model.Int64Val(tc.code)}, builtin.CallBudget{OutputBytes: int64(len(tc.text)), MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != tc.text {
				t.Fatalf("chr = %v, %v; want %q", value, err, tc.text)
			}
			_, err = chr.Call(t.Context(), []model.Value{model.Int64Val(tc.code)}, builtin.CallBudget{OutputBytes: int64(len(tc.text) - 1), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
	value, err := ascii.Call(t.Context(), []model.Value{model.StringVal("")}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
	if err != nil || value.IntVal != 0 || value.IsNull {
		t.Fatalf("ascii empty = %v, %v; want zero", value, err)
	}
	for _, invalid := range []string{"a\x00", "a\xff"} {
		_, err := ascii.Call(t.Context(), []model.Value{model.StringVal(invalid)}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		requireSQLState(t, err, "22021")
	}
}

func TestCharacterCodeRejectsInvalidScalarValues(t *testing.T) {
	chr := callable(t, "chr", 23)
	for _, tc := range []struct {
		input int64
		state string
	}{
		{-1, "22023"}, {0, "54000"}, {0xd800, "54000"}, {0xdfff, "54000"},
		{0x110000, "54000"}, {math.MaxInt32, "54000"},
	} {
		t.Run(fmt.Sprint(tc.input), func(t *testing.T) {
			_, err := chr.Call(t.Context(), []model.Value{model.Int64Val(tc.input)}, builtin.CallBudget{OutputBytes: 4, MemoryBytes: 64 << 20})
			requireSQLState(t, err, tc.state)
		})
	}
}

func TestBaseConversionPreservesSignedInputWidth(t *testing.T) {
	for _, tc := range []struct {
		id    model.FunctionID
		input int64
		want  string
	}{
		{2089, -1, "ffffffff"}, {2090, -1, "ffffffffffffffff"},
		{2089, math.MinInt32, "80000000"}, {2090, math.MinInt64, "8000000000000000"},
		{2089, 0, "0"}, {2090, 256, "100"},
		{6330, -1, "11111111111111111111111111111111"},
		{6331, math.MinInt64, "1000000000000000000000000000000000000000000000000000000000000000"},
		{6332, -1, "37777777777"}, {6333, -1, "1777777777777777777777"},
		{6330, 0, "0"}, {6332, 512, "1000"},
	} {
		t.Run(fmt.Sprintf("%d/%d", tc.id, tc.input), func(t *testing.T) {
			function, err := builtin.Resolve(tc.id, builtin.ResolveOptions{})
			if err != nil {
				t.Fatal(err)
			}
			args := []model.Value{model.Int64Val(tc.input)}
			value, err := function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.StrVal != tc.want {
				t.Fatalf("base conversion = %v, %v; want %q", value, err, tc.want)
			}
			_, err = function.Call(t.Context(), args, builtin.CallBudget{OutputBytes: int64(len(tc.want) - 1), MemoryBytes: 64 << 20})
			requireSQLState(t, err, "54000")
		})
	}
}
