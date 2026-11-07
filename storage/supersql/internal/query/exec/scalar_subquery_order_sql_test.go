package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLScalarProjectionErrorPrecedesLaterSubquery(t *testing.T) {
	graph := sqlGraph(t, `SELECT 1/(i-1),(SELECT j FROM generate_series(1,2) AS t(j)) FROM generate_series(1,1) AS s(i)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "22012" {
		t.Fatalf("projection error order: %v, want division by zero before scalar cardinality", err)
	}
	if batch.Rows() != 0 {
		t.Fatal("failed projection exposed rows")
	}
	aggregateProjectionReleased(t, env.Query)
}
