package exec_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLBetweenInclusiveAndReversedBounds(t *testing.T) {
	graph := sqlGraph(t, `SELECT k,
		x BETWEEN lo AND hi, x NOT BETWEEN lo AND hi,
		x BETWEEN SYMMETRIC lo AND hi, x NOT BETWEEN SYMMETRIC lo AND hi
		FROM (VALUES(1,1,1,3),(2,3,1,3),(3,0,1,3),(4,4,1,3),
			(5,2,3,1),(6,0,3,1),(7,2,2,2)) t(k,x,lo,hi) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, yes, no := model.Int64Val, model.BoolVal(true), model.BoolVal(false)
	assertAnyValueRows(t, portal, []model.TypeID{23, 16, 16, 16, 16}, [][]model.Value{
		{i(1), yes, no, yes, no},
		{i(2), yes, no, yes, no},
		{i(3), no, yes, no, yes},
		{i(4), no, yes, no, yes},
		{i(5), no, yes, yes, no},
		{i(6), no, yes, no, yes},
		{i(7), yes, no, yes, no},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLBetweenThreeValuedLogic(t *testing.T) {
	graph := sqlGraph(t, `SELECT k,
		x BETWEEN lo AND hi, x NOT BETWEEN lo AND hi,
		x BETWEEN SYMMETRIC lo AND hi, x NOT BETWEEN SYMMETRIC lo AND hi
		FROM (VALUES(1,NULL,1,3),(2,0,1,NULL),(3,4,NULL,3),
			(4,2,1,NULL),(5,2,NULL,3),(6,2,NULL,NULL)) t(k,x,lo,hi) ORDER BY k`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, yes, no := model.Int64Val, model.BoolVal(true), model.BoolVal(false)
	null := model.NullVal(model.TypeBoolean)
	assertAnyValueRows(t, portal, []model.TypeID{23, 16, 16, 16, 16}, [][]model.Value{
		{i(1), null, null, null, null},
		{i(2), no, yes, null, null},
		{i(3), no, yes, null, null},
		{i(4), null, null, null, null},
		{i(5), null, null, null, null},
		{i(6), null, null, null, null},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLBetweenResolvesEachComparison(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      []model.Value
	}{
		{
			"mixed integer and numeric bounds",
			`SELECT 2::int2 BETWEEN 1::int8 AND 2.5::numeric,
				2.5::numeric BETWEEN SYMMETRIC 3::int4 AND 2::int8,
				9007199254740993::int8 BETWEEN 9007199254740992::numeric AND 9007199254740992::numeric`,
			[]model.Value{model.BoolVal(true), model.BoolVal(true), model.BoolVal(false)},
		},
		{
			"unknown operand resolves independently",
			`SELECT '10' BETWEEN 2 AND '9', '10' NOT BETWEEN 2 AND '9'`,
			[]model.Value{model.BoolVal(true), model.BoolVal(false)},
		},
		{
			"independent explicit bound collations",
			`SELECT 'm' BETWEEN ('a' COLLATE "C") AND ('z' COLLATE "POSIX"),
				'm' BETWEEN SYMMETRIC ('z' COLLATE "POSIX") AND ('a' COLLATE "C")`,
			[]model.Value{model.BoolVal(true), model.BoolVal(true)},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			oids := make([]model.TypeID, len(scenario.want))
			for i := range oids {
				oids[i] = 16
			}
			assertAnyValueRows(t, portal, oids, [][]model.Value{scenario.want})
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLBetweenMasksUnneededBounds(t *testing.T) {
	graph := sqlGraph(t, `SELECT x,
		x BETWEEN 1 AND 10/x, x NOT BETWEEN 1 AND 10/x,
		CASE WHEN x=0 THEN NULL::boolean ELSE x BETWEEN SYMMETRIC 10/x AND 1 END,
		CASE WHEN x=0 THEN NULL::boolean ELSE x NOT BETWEEN SYMMETRIC 10/x AND 1 END
		FROM (VALUES(0),(2)) t(x) ORDER BY x`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i, yes, no := model.Int64Val, model.BoolVal(true), model.BoolVal(false)
	null := model.NullVal(model.TypeBoolean)
	assertAnyValueRows(t, portal, []model.TypeID{23, 16, 16, 16, 16}, [][]model.Value{
		{i(0), no, yes, null, null},
		{i(2), yes, no, yes, no},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLBetweenEvaluatesRequiredBounds(t *testing.T) {
	for _, scenario := range []struct {
		name, expression string
	}{
		{"between upper bound", `x BETWEEN -1 AND 1/x`},
		{"not between upper bound", `x NOT BETWEEN -1 AND 1/x`},
		{"symmetric reversed branch", `x BETWEEN SYMMETRIC 1 AND 1/x`},
		{"not symmetric reversed branch", `x NOT BETWEEN SYMMETRIC 1 AND 1/x`},
		{"unknown first comparison", `x BETWEEN NULL::integer AND 1/x`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT `+scenario.expression+` FROM (VALUES(0)) t(x)`, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
				t.Fatalf("required bound error=%v, want SQLSTATE 22012", err)
			}
			if batch.Rows() != 0 {
				t.Fatalf("failed bound published %d rows", batch.Rows())
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
