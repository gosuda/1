package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLRowsFromRecordSchemaErrorPrecedesLaterFunctionArguments(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM ROWS FROM(unnest(ARRAY[NULL::record,ROW(1)]) AS(a bigint),generate_series(1,2,0))`, nil)
	env := statefulEnvironment(t)
	env.Limits.BatchRows = 1
	env.Limits.Expressions.MaxRows = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "42804" {
		t.Fatalf("record schema precedence=%v, want 42804", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLUnnestSchemaMismatchCannotBeHiddenByLimit(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM unnest(ARRAY[NULL::record,ROW(1)]) AS u(a bigint) LIMIT 1`, nil)
	env := statefulEnvironment(t)
	env.Limits.BatchRows = 1
	env.Limits.Expressions.MaxRows = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sql *model.Error
	if !errors.As(err, &sql) || sql.Code != "42804" {
		t.Fatalf("record schema before LIMIT=%v, want 42804", err)
	}
	aggregateProjectionReleased(t, env.Query)
}
