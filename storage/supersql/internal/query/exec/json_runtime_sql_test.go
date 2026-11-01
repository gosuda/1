package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLJSONBComparisonPreservesByteDifferenceAndRootArrayOrder(t *testing.T) {
	graph := sqlGraph(t, `SELECT jsonb_cmp('"a"','"z"'),jsonb_cmp('"é"','"a"'),jsonb_cmp('false','true'),jsonb_cmp('[]','null'),jsonb_cmp('{"a":1}','{"z":1}')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23, 23, 23}, [][]model.Value{{model.Int64Val(-25), model.Int64Val(98), model.Int64Val(-1), model.Int64Val(-1), model.Int64Val(-25)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLJSONObjectUniqueErrorsRespectFinalizationTiming(t *testing.T) {
	for _, scenario := range []struct {
		function string
		code     string
	}{{"json_object_agg_unique", "22030"}, {"jsonb_object_agg_unique", "22012"}} {
		t.Run(scenario.function, func(t *testing.T) {
			graph := sqlGraph(t, "SELECT "+scenario.function+"('a',1/(id-3)) FROM generate_series(1,4)g(id)", nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlError *model.Error
			if !errors.As(err, &sqlError) || sqlError.Code != scenario.code {
				t.Fatalf("aggregate error = %v, want %s", err, scenario.code)
			}
			if batch.Rows() != 0 {
				t.Fatal("failed aggregate exposed a result")
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
