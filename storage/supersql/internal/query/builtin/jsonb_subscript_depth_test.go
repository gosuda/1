package builtin

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func nestedJSONBArray(depth int, leaf string) string {
	return strings.Repeat("[", depth) + leaf + strings.Repeat("]", depth)
}

func assertSQLState54001(t *testing.T, err error) {
	t.Helper()
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "54001" {
		t.Fatalf("error = %v, want SQLSTATE 54001", err)
	}
}

func TestJSONBSubscriptDepthExceedsMaximumReturns54001WithoutPanic(t *testing.T) {
	budget := CallBudget{OutputBytes: 64 << 20, MemoryBytes: 64 << 20}

	t.Run("j[a][b] = depth-6399 non-empty jsonb", func(t *testing.T) {
		deep := nestedJSONBArray(jsonTextMaxDepth-1, "1")
		_, err := JSONBSubscriptStore(t.Context(), []model.Value{
			{Type: model.TypeJSONB, StrVal: "{}"},
			model.StringVal("a"),
			model.StringVal("b"),
			{Type: model.TypeJSONB, StrVal: deep},
		}, budget)
		assertSQLState54001(t, err)
	})

	t.Run("j[a][b] = depth-6399 empty-leaf jsonb", func(t *testing.T) {
		deep := nestedJSONBArray(jsonTextMaxDepth-1, "")
		_, err := JSONBSubscriptStore(t.Context(), []model.Value{
			{Type: model.TypeJSONB, StrVal: "{}"},
			model.StringVal("a"),
			model.StringVal("b"),
			{Type: model.TypeJSONB, StrVal: deep},
		}, budget)
		assertSQLState54001(t, err)
	})

	t.Run("j[a] = depth-6400 jsonb", func(t *testing.T) {
		deep := nestedJSONBArray(jsonTextMaxDepth, "1")
		_, err := JSONBSubscriptStore(t.Context(), []model.Value{
			{Type: model.TypeJSONB, StrVal: "{}"},
			model.StringVal("a"),
			{Type: model.TypeJSONB, StrVal: deep},
		}, budget)
		assertSQLState54001(t, err)
	})

	t.Run("path of 6401 keys on scalar replacement", func(t *testing.T) {
		args := make([]model.Value, 0, jsonTextMaxDepth+3)
		args = append(args, model.Value{Type: model.TypeJSONB, StrVal: "{}"})
		for i := range jsonTextMaxDepth + 1 {
			args = append(args, model.StringVal("k"+strconv.Itoa(i)))
		}
		args = append(args, model.Value{Type: model.TypeJSONB, StrVal: "1"})
		_, err := JSONBSubscriptStore(t.Context(), args, budget)
		assertSQLState54001(t, err)
	})
}

func TestJSONBSubscriptDepthAtMaximumSucceeds(t *testing.T) {
	budget := CallBudget{OutputBytes: 64 << 20, MemoryBytes: 64 << 20}

	t.Run("j[a] = depth-6399 reaches exact max depth 6400", func(t *testing.T) {
		deep := nestedJSONBArray(jsonTextMaxDepth-1, "1")
		got, err := JSONBSubscriptStore(t.Context(), []model.Value{
			{Type: model.TypeJSONB, StrVal: "{}"},
			model.StringVal("a"),
			{Type: model.TypeJSONB, StrVal: deep},
		}, budget)
		if err != nil {
			t.Fatalf("JSONBSubscriptStore depth 6400 failed: %v", err)
		}
		want := `{"a": ` + deep + `}`
		if got.Type != model.TypeJSONB || got.StrVal != want {
			t.Fatalf("unexpected output length=%d, want %d", len(got.StrVal), len(want))
		}
	})

	t.Run("j[a][b] = depth-6398 reaches exact max depth 6400", func(t *testing.T) {
		deep := nestedJSONBArray(jsonTextMaxDepth-2, "1")
		got, err := JSONBSubscriptStore(t.Context(), []model.Value{
			{Type: model.TypeJSONB, StrVal: "{}"},
			model.StringVal("a"),
			model.StringVal("b"),
			{Type: model.TypeJSONB, StrVal: deep},
		}, budget)
		if err != nil {
			t.Fatalf("JSONBSubscriptStore depth 6400 failed: %v", err)
		}
		want := `{"a": {"b": ` + deep + `}}`
		if got.Type != model.TypeJSONB || got.StrVal != want {
			t.Fatalf("unexpected output length=%d, want %d", len(got.StrVal), len(want))
		}
	})

	t.Run("path of 6400 keys on scalar replacement reaches exact max depth 6400", func(t *testing.T) {
		args := make([]model.Value, 0, jsonTextMaxDepth+2)
		args = append(args, model.Value{Type: model.TypeJSONB, StrVal: "{}"})
		for i := range jsonTextMaxDepth {
			args = append(args, model.StringVal("k"+strconv.Itoa(i)))
		}
		args = append(args, model.Value{Type: model.TypeJSONB, StrVal: "1"})
		got, err := JSONBSubscriptStore(t.Context(), args, budget)
		if err != nil {
			t.Fatalf("JSONBSubscriptStore 6400-key path failed: %v", err)
		}
		if got.Type != model.TypeJSONB || !strings.HasPrefix(got.StrVal, `{"k0": `) {
			t.Fatalf("unexpected output prefix: %q", got.StrVal[:min(len(got.StrVal), 32)])
		}
	})
}

func TestAppendJSONBNodesDefensiveDepthAndFrameBounds(t *testing.T) {
	t.Run("undersized frames slice returns 54001 instead of panicking", func(t *testing.T) {
		nodes := []jsonbNode{
			{kind: jsonTextArray, child: 1, next: -1},
			{kind: jsonTextArray, child: 2, next: -1},
			{kind: jsonTextNumber, child: -1, next: -1, text: "1"},
		}
		var out strings.Builder
		err := appendJSONBNodes(t.Context(), &out, nodes, make([]jsonbFrame, 1))
		assertSQLState54001(t, err)
	})

	t.Run("depth exceeding jsonTextMaxDepth returns 54001 even with extra frames", func(t *testing.T) {
		count := jsonTextMaxDepth + 1
		nodes := make([]jsonbNode, count+1)
		for i := range count {
			nodes[i] = jsonbNode{kind: jsonTextArray, child: i + 1, next: -1}
		}
		nodes[count] = jsonbNode{kind: jsonTextNumber, child: -1, next: -1, text: "1"}
		var out strings.Builder
		err := appendJSONBNodes(t.Context(), &out, nodes, make([]jsonbFrame, count+2))
		assertSQLState54001(t, err)
	})
}
