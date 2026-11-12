package builtin

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestCStringAndUnknownCodecsPreserveCStringBoundary(t *testing.T) {
	for _, id := range []model.FunctionID{109, 110, 2292, 2293} {
		function, err := Resolve(id, ResolveOptions{})
		if err != nil {
			t.Fatal(err)
		}
		value, err := function.Call(t.Context(), []model.Value{model.StringVal("x\x00ignored")}, CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
		if err != nil || value.Type != model.TypeString || value.IsNull || value.StrVal != "x" {
			t.Fatalf("cstring %d=%v,%v", id, value, err)
		}
		_, err = function.Call(t.Context(), []model.Value{model.StringVal("xy")}, CallBudget{OutputBytes: 1, MemoryBytes: 64 << 20})
		requireReplacementSQLState(t, err, "54000")
		value, err = function.Call(t.Context(), []model.Value{model.NullVal(model.TypeString)}, CallBudget{OutputBytes: 0, MemoryBytes: 64 << 20})
		if err != nil || !value.IsNull {
			t.Fatalf("NULL cstring %d=%v,%v", id, value, err)
		}
	}
}
