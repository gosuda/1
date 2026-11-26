package builtin_test

import (
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestJSONStripPreservesDuplicatesAndNumericLexemes(t *testing.T) {
	for _, tc := range []struct {
		name, input, want string
		arrays            bool
	}{
		{"object fields", ` { "a":1e+100000,"a":null,"b":{"x":null},"c":[null,{"x":null}] } `, `{"a":1e+100000,"b":{},"c":[null,{}]}`, false},
		{"array elements", `[null,{"a":null},[null,1],null]`, `[{},[1]]`, true},
		{"root null", `null`, `null`, true},
		{"decoded strings", `{"\u0061":"\u00e9\/\u0001"}`, "{\"a\":\"é/\\u0001\"}", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := jsonCallable(t, 3261).Call(t.Context(), []model.Value{jsonDatum(tc.input), model.BoolVal(tc.arrays)}, builtin.CallBudget{OutputBytes: int64(len(tc.want)), MemoryBytes: 64 << 20})
			if err != nil || value.IsNull || value.StrVal != tc.want {
				t.Fatalf("strip = %v, %v; want %q", value, err, tc.want)
			}
		})
	}
}

func TestJSONStripDeletedFieldsNeedNoDecodedArena(t *testing.T) {
	input := `{"` + strings.Repeat("x", 1<<16) + `":null}`
	value, err := jsonCallable(t, 3261).Call(t.Context(), []model.Value{jsonDatum(input), model.BoolVal(false)}, builtin.CallBudget{OutputBytes: 2, MemoryBytes: 64 << 20})
	if err != nil || value.StrVal != "{}" {
		t.Fatalf("removed large key = %v, %v", value, err)
	}
	_, err = jsonCallable(t, 3261).Call(t.Context(), []model.Value{jsonDatum(input), model.BoolVal(false)}, builtin.CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "54000")
}

func TestJSONStripValidatesDeletedKeyEscapes(t *testing.T) {
	_, err := jsonCallable(t, 3261).Call(t.Context(), []model.Value{jsonDatum(`{"\u0000":null}`), model.BoolVal(true)}, builtin.CallBudget{OutputBytes: 2, MemoryBytes: 64 << 20})
	requireSQLState(t, err, "22P05")
}
