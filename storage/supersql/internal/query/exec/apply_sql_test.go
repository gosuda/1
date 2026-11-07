package exec_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/apply"
)

func applyEnvironment(t *testing.T) exec.Environment {
	t.Helper()
	env := setWindowEnvironment(t)
	env.Apply = apply.Options{MaxRowBytes: 4096, WorkBytes: 64 << 10}
	return env
}

func TestSQLLateralUsesCurrentOuterCaptures(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{"projection", `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT a.x*10 AS y) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(10)}, {model.Int64Val(2), model.Int64Val(20)}}},
		{"outer null extension", `SELECT a.x,b.y FROM (VALUES(1),(3)) a(x) LEFT JOIN LATERAL (SELECT y FROM (VALUES(10),(20)) r(y) WHERE y=a.x*10) b ON TRUE`, [][]model.Value{{model.Int64Val(1), model.Int64Val(10)}, {model.Int64Val(3), model.NullVal(model.TypeInt64)}}},
		{"ordered limit", `SELECT a.x,b.y FROM (VALUES(1),(3),(9)) a(x) CROSS JOIN LATERAL (SELECT y FROM (VALUES(4),(2)) r(y) WHERE y>=a.x ORDER BY y LIMIT 1) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(2)}, {model.Int64Val(3), model.Int64Val(4)}}},
		{"nested scopes", `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT c.y FROM (VALUES(10)) q(z) CROSS JOIN LATERAL (SELECT a.x+q.z AS y) c) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(11)}, {model.Int64Val(2), model.Int64Val(12)}}},
		{"aggregate argument", `SELECT a.x,b.total FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT sum(y+a.x) AS total FROM (VALUES(10),(20)) r(y)) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(32)}, {model.Int64Val(2), model.Int64Val(34)}}},
		{"window argument", `SELECT a.x,b.n FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT first_value(y+a.x) OVER(ORDER BY y) AS n FROM (VALUES(10),(20)) r(y)) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(11)}, {model.Int64Val(1), model.Int64Val(11)}, {model.Int64Val(2), model.Int64Val(12)}, {model.Int64Val(2), model.Int64Val(12)}}},
		{"join residual", `SELECT a.x,b.v FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT l.v FROM (VALUES(10),(20)) l(v) JOIN (VALUES(1)) r(k) ON l.v=a.x*10) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(10)}, {model.Int64Val(2), model.Int64Val(20)}}},
		{"captured limit", `SELECT b.y FROM (VALUES(0),(1),(2)) a(x) CROSS JOIN LATERAL (SELECT y FROM (VALUES(20),(10)) r(y) ORDER BY y LIMIT a.x) b`, [][]model.Value{{model.Int64Val(10)}, {model.Int64Val(10)}, {model.Int64Val(20)}}},
		{"external materialized CTE", `WITH c AS MATERIALIZED (SELECT y FROM (VALUES(10),(20)) r(y)) SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT c.y+a.x AS y FROM c) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(11)}, {model.Int64Val(1), model.Int64Val(21)}, {model.Int64Val(2), model.Int64Val(12)}, {model.Int64Val(2), model.Int64Val(22)}}},
		{"local materialized CTE", `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (WITH c AS MATERIALIZED (SELECT a.x+10 AS y) SELECT y FROM c) b`, [][]model.Value{{model.Int64Val(1), model.Int64Val(11)}, {model.Int64Val(2), model.Int64Val(12)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rows=%v want=%v", got, tc.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLLateralRepeatedRowsReleaseInnerInvocations(t *testing.T) {
	var sql strings.Builder
	sql.WriteString("SELECT b.y FROM (VALUES")
	for i := range 256 {
		if i != 0 {
			sql.WriteByte(',')
		}
		fmt.Fprintf(&sql, "(%d)", i)
	}
	sql.WriteString(") a(x) CROSS JOIN LATERAL (SELECT a.x+1 AS y) b")
	graph := sqlGraph(t, sql.String(), nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	got := rows(t, portal)
	if len(got) != 256 {
		t.Fatalf("lateral returned %d rows, want 256", len(got))
	}
	for i, row := range got {
		if len(row) != 1 || row[0].IsNull || row[0].IntVal != int64(i+1) {
			t.Fatalf("row %d=%v", i, row)
		}
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLLateralErrorReleasesInnerInvocation(t *testing.T) {
	graph := sqlGraph(t, `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT 10/(2-a.x) AS y) b`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
		t.Fatalf("lateral error=%v", err)
	}
	if batch.Rows() != 0 {
		t.Fatalf("error published %d partial rows", batch.Rows())
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLLateralEarlyCloseReleasesActiveInner(t *testing.T) {
	graph := sqlGraph(t, `SELECT a.x,b.y FROM (VALUES(1),(2)) a(x) CROSS JOIN LATERAL (SELECT y FROM (VALUES(10),(20)) r(y) WHERE y>a.x) b`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	batch := columnar.Batch{MaxRows: 1}
	if err := portal.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Rows() != 1 || batch.Columns[1].Int64[0] != 10 {
		t.Fatalf("first lateral batch=%+v", batch)
	}
	if err := portal.Close(); err != nil {
		t.Fatal(err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLCorrelatedScalarSubqueryReturnsNullForNoRow(t *testing.T) {
	graph := sqlGraph(t, `SELECT a.x,(SELECT y FROM (VALUES(10),(20)) r(y) WHERE y=a.x*10) AS y FROM (VALUES(1),(3)) a(x)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1), model.Int64Val(10)}, {model.Int64Val(3), model.NullVal(model.TypeInt64)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("scalar rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLCorrelatedScalarSubqueryRejectsMultipleRows(t *testing.T) {
	graph := sqlGraph(t, `SELECT (SELECT y FROM (VALUES(10),(20)) r(y) WHERE y>a.x) FROM (VALUES(1)) a(x)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "21000" {
		t.Fatalf("scalar cardinality error=%v", err)
	}
	if batch.Rows() != 0 {
		t.Fatalf("cardinality error published %d rows", batch.Rows())
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLCorrelatedScalarSubqueryRunsAfterLimit(t *testing.T) {
	graph := sqlGraph(t, `SELECT (SELECT 10/a.x) FROM (VALUES(0),(2)) a(x) ORDER BY a.x DESC LIMIT 1`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(5)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("limited scalar rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLScalarSubqueryReadsMaterializedCTE(t *testing.T) {
	graph := sqlGraph(t, `WITH a AS MATERIALIZED (SELECT 1 AS x) SELECT (SELECT a.x) FROM a`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{{model.Int64Val(1)}}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("CTE scalar rows=%v want=%v", got, want)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLCorrelatedExistsPreservesPredicateErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT a.x FROM (VALUES(1)) a(x) WHERE EXISTS(SELECT y FROM (VALUES(2)) r(y) WHERE 10/(a.x-1)>0)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
		t.Fatalf("EXISTS predicate error=%v", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLRowAndArraySubqueryForms(t *testing.T) {
	array := func(dimensions []model.ArrayDimension, values ...model.Value) model.Value {
		return model.Value{Type: model.TypeArray, Dimensions: dimensions, ArrayVal: values}
	}
	for _, tc := range []struct {
		name string
		sql  string
		want model.Value
	}{
		{"row IN", `SELECT ROW(1,2) IN (SELECT a,b FROM (VALUES(1,2),(3,4)) v(a,b))`, model.BoolVal(true)},
		{"row ALL", `SELECT ROW(1,2) < ALL (SELECT a,b FROM (VALUES(1,3),(2,0)) v(a,b))`, model.BoolVal(true)},
		{"row exact", `SELECT ROW(1,2) < (SELECT 1,3)`, model.BoolVal(true)},
		{"empty row exact", `SELECT ROW(1,2) = (SELECT 1,2 WHERE FALSE)`, model.NullVal(model.TypeBoolean)},
		{"scalar array", `SELECT ARRAY(SELECT x FROM (VALUES(1),(2)) v(x))`, array([]model.ArrayDimension{{Length: 2, LowerBound: 1}}, model.Int64Val(1), model.Int64Val(2))},
		{"nested array", `SELECT ARRAY(SELECT ARRAY[x,x+1] FROM (VALUES(1),(3)) v(x))`, array([]model.ArrayDimension{{Length: 2, LowerBound: 1}, {Length: 2, LowerBound: 1}}, model.Int64Val(1), model.Int64Val(2), model.Int64Val(3), model.Int64Val(4))},
		{"array ANY", `SELECT 2 = ANY(ARRAY[1,2])`, model.BoolVal(true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := applyEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			want := [][]model.Value{{tc.want}}
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows=%v want=%v", got, want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
