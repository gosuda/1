package exec_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLIOCastMasksAndCatalogPrecedence(t *testing.T) {
	graph := sqlGraph(t, `SELECT x::text,x::text::integer,x::text::bytea,
		CASE WHEN x=0 THEN NULL::integer ELSE y::integer END,
		true::text,'\x41'::bytea::text
		FROM (VALUES(0,'bad'::text),(2,'3')) v(x,y) ORDER BY v.x`, nil)
	env, dir := groupingSetsEnvironment(t, false)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	want := [][]model.Value{
		{model.StringVal("0"), model.Int64Val(0), model.BytesVal([]byte("0")), model.NullVal(model.TypeInt64), model.StringVal("true"), model.StringVal("\\x41")},
		{model.StringVal("2"), model.Int64Val(2), model.BytesVal([]byte("2")), model.Int64Val(3), model.StringVal("true"), model.StringVal("\\x41")},
	}
	if got := rows(t, portal); !reflect.DeepEqual(got, want) {
		t.Fatalf("I/O casts=%v, want %v", got, want)
	}
	groupingSetsReleased(t, env, dir)
}

func TestSQLIOCastReportsActiveInputErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT x::integer FROM (VALUES('bad'::text)) v(x)`, nil)
	env, dir := groupingSetsEnvironment(t, false)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22P02" {
		t.Fatalf("I/O cast error=%v, want invalid integer input", err)
	}
	if batch.Rows() != 0 {
		t.Fatalf("invalid input returned %d rows", batch.Rows())
	}
	groupingSetsReleased(t, env, dir)
}
