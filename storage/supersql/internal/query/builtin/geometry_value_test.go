package builtin_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

func TestClosingPathOwnsResultEvenWhenAlreadyClosed(t *testing.T) {
	budget := builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 1 << 20}
	for _, text := range []string{"[(0,0),(1,1)]", "((0,0),(1,1))"} {
		input, err := jsonCallable(t, 121).Call(t.Context(), []model.Value{model.StringVal(text)}, budget)
		if err != nil {
			t.Fatal(err)
		}
		closed, err := jsonCallable(t, 1433).Call(t.Context(), []model.Value{input}, budget)
		if err != nil {
			t.Fatal(err)
		}
		output, err := jsonCallable(t, 122).Call(t.Context(), []model.Value{closed}, budget)
		if err != nil || output.StrVal != "((0,0),(1,1))" {
			t.Fatalf("closed path = %q, %v", output.StrVal, err)
		}
		clear(closed.BytesVal)
		output, err = jsonCallable(t, 122).Call(t.Context(), []model.Value{input}, budget)
		if err != nil || output.StrVal != text {
			t.Fatalf("closing path changed caller storage: %q, %v", output.StrVal, err)
		}
		_, err = jsonCallable(t, 1433).Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: 1 << 20})
		requireSQLState(t, err, "54000")
		_, err = jsonCallable(t, 1433).Call(t.Context(), []model.Value{input}, builtin.CallBudget{OutputBytes: 1 << 20, MemoryBytes: 0})
		requireSQLState(t, err, "54000")
	}
}
