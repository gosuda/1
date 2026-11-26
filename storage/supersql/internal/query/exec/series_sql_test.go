package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLGenerateSeriesOrdinalityAndBounds(t *testing.T) {
	i := model.Int64Val
	for _, tc := range []struct {
		name, sql string
		oids      []model.TypeID
		want      [][]model.Value
	}{
		{"descending ordinality", `SELECT * FROM generate_series(5,1,-2) WITH ORDINALITY AS g(v,n)`, []model.TypeID{23, 20}, [][]model.Value{{i(5), i(1)}, {i(3), i(2)}, {i(1), i(3)}}},
		{"bigint endpoint", `SELECT * FROM generate_series(9223372036854775806::bigint,9223372036854775807::bigint)`, []model.TypeID{20}, [][]model.Value{{i(9223372036854775806)}, {i(9223372036854775807)}}},
		{"huge source stops at consumer limit", `SELECT n FROM generate_series(1::bigint,9223372036854775807::bigint) WITH ORDINALITY AS g(v,n) LIMIT 3`, []model.TypeID{20}, [][]model.Value{{i(1)}, {i(2)}, {i(3)}}},
		{"lateral replay stops at consumer limit", `SELECT g.v FROM (VALUES(1),(2)) l(x), generate_series(1::bigint,9223372036854775807::bigint) AS g(v) LIMIT 3`, []model.TypeID{20}, [][]model.Value{{i(1)}, {i(2)}, {i(3)}}},
		{"rows from preserves scalar alias", `SELECT g FROM ROWS FROM(generate_series(1,2)) AS g`, []model.TypeID{23}, [][]model.Value{{i(1)}, {i(2)}}},
		{"strict null suppresses zero step", `SELECT * FROM generate_series(NULL::integer,3,0)`, []model.TypeID{23}, nil},
		{"zero limit suppresses zero step", `SELECT * FROM generate_series(1,3,0) LIMIT 0`, []model.TypeID{23}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, tc.oids, tc.want)
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLGenerateSeriesLateralScopes(t *testing.T) {
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	for _, tc := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"implicit lateral", `SELECT l.n,g.v FROM (VALUES(1),(3)) l(n), generate_series(1,l.n) AS g(v) ORDER BY l.n,g.v`, [][]model.Value{{i(1), i(1)}, {i(3), i(1)}, {i(3), i(2)}, {i(3), i(3)}}},
		{"left empty and null arguments", `SELECT l.n,g.v FROM (VALUES(1),(3),(NULL)) l(n) LEFT JOIN generate_series(2,l.n) AS g(v) ON true ORDER BY l.n,g.v`, [][]model.Value{{i(1), null}, {i(3), i(2)}, {i(3), i(3)}, {null, null}}},
		{"using predicate outside independent body", `SELECT x,g.x FROM (VALUES(1),(2),(3)) l(x) LEFT JOIN generate_series(2,3) AS g(x) USING(x) ORDER BY 1`, [][]model.Value{{i(1), null}, {i(2), i(2)}, {i(3), i(3)}}},
		{"full independent function", `SELECT x,g.x FROM (VALUES(1),(2)) l(x) FULL JOIN generate_series(2,3) AS g(x) USING(x) ORDER BY 1`, [][]model.Value{{i(1), null}, {i(2), i(2)}, {i(3), i(3)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, []model.TypeID{23, 23}, tc.want)
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLGenerateSeriesZeroStepFailsOnDemand(t *testing.T) {
	graph := sqlGraph(t, `SELECT * FROM generate_series(1,3,0)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	defer batch.Reset()
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22023" {
		t.Fatalf("zero step error=%v", err)
	}
	aggregateProjectionReleased(t, env.Query)
}
