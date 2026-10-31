package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// PostgreSQL 18.4 reference: src/backend/parser/parse_func.c,
// check_srf_call_placement.
func TestSetReturningFunctionPlacement(t *testing.T) {
	for _, sql := range []string{
		`SELECT 1 WHERE generate_series(1,2) = 1`,
		`SELECT CASE WHEN true THEN generate_series(1,2) ELSE 0 END`,
		`SELECT coalesce(generate_series(1,2),0)`,
		`SELECT sum(generate_series(1,2))`,
		`SELECT lag(generate_series(1,2)) OVER ()`,
		`SELECT 1 LIMIT generate_series(1,2)`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, nil, nil)
			requireCode(t, err, "0A000")
		})
	}
}

func TestTargetSetReturningFunctionLowersToProjectSet(t *testing.T) {
	statement, err := bindSQL(t, `SELECT generate_series(1,2), generate_series(1,generate_series(2,3))`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	root, _ := graph.Rel(graph.Root())
	if root.Kind != ir.Project {
		t.Fatalf("root kind = %v, want final Project", root.Kind)
	}
	input, _ := graph.Input(graph.Root(), 0)
	outer, _ := graph.Rel(input)
	if outer.Kind != ir.ProjectSet {
		t.Fatalf("outer expansion kind = %v, want ProjectSet", outer.Kind)
	}
	input, _ = graph.Input(input, 0)
	inner, _ := graph.Rel(input)
	if inner.Kind != ir.ProjectSet {
		t.Fatalf("inner expansion kind = %v, want ProjectSet", inner.Kind)
	}
}
