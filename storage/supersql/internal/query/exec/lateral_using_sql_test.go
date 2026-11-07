package exec_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLLateralUsingAndNaturalMatchCapturedRows(t *testing.T) {
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	inner := [][]model.Value{
		{i(1), i(1), i(1), i(10), i(10)},
		{i(2), i(2), i(2), i(20), i(20)},
	}
	outer := [][]model.Value{
		{i(1), i(1), i(1), i(10), i(10)},
		{i(2), i(2), i(2), i(20), i(20)},
		{i(3), i(3), null, i(30), null},
		{i(4), i(4), null, i(40), null},
		{null, null, null, i(50), null},
	}
	for _, tc := range []struct {
		name, join, condition string
		want                  [][]model.Value
	}{
		{"inner using", "INNER JOIN", "USING (key)", inner},
		{"left using", "LEFT JOIN", "USING (key)", outer},
		{"inner natural", "NATURAL INNER JOIN", "", inner},
		{"left natural", "NATURAL LEFT JOIN", "", outer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql := fmt.Sprintf(`SELECT key, l.key, r.key, l.left_value, r.right_value
				FROM (VALUES (1,10), (2,20), (3,30), (4,40), (NULL,50)) l(key,left_value)
				%s LATERAL (
					SELECT v.key, v.bucket AS right_value
					FROM (VALUES (1,10), (9,10), (2,20), (9,30), (NULL,50)) v(key,bucket)
					WHERE v.bucket = l.left_value
				) r %s
				ORDER BY l.left_value`, tc.join, tc.condition)
			graph := sqlGraph(t, sql, nil)
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

func TestSQLLateralNaturalWithoutCommonColumns(t *testing.T) {
	i := model.Int64Val
	for _, tc := range []struct {
		name, join string
		want       [][]model.Value
	}{
		{
			"inner", "NATURAL INNER JOIN",
			[][]model.Value{{i(1), i(10)}, {i(1), i(11)}, {i(2), i(20)}},
		},
		{
			"left", "NATURAL LEFT JOIN",
			[][]model.Value{{i(1), i(10)}, {i(1), i(11)}, {i(2), i(20)}, {i(3), model.NullVal(model.TypeInt64)}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sql := fmt.Sprintf(`SELECT * FROM (VALUES (1), (2), (3)) l(key)
				%s LATERAL (
					SELECT v.value FROM (VALUES (1,10), (1,11), (2,20)) v(owner,value)
					WHERE v.owner = l.key
				) r ORDER BY key, value`, tc.join)
			graph := sqlGraph(t, sql, nil)
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

func TestSQLLateralUsingCoercesMergedKeyWithoutChangingQualifiedKeys(t *testing.T) {
	graph := sqlGraph(t, `SELECT key, l.key, r.key
		FROM (VALUES (1::integer), (2)) l(key)
		LEFT JOIN LATERAL (
			SELECT v.key::double precision AS key
			FROM (VALUES (1), (9)) v(key) WHERE v.key = l.key
		) r USING (key) ORDER BY l.key`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	floating := model.TypeLayout{Type: model.Type{ID: 701, Modifier: -1, Storage: model.TypeFloat64}}
	assertArrayAggregateRows(t, portal, []model.TypeLayout{floating, integer, floating}, [][]model.Value{
		{model.Float64Val(1), model.Int64Val(1), model.Float64Val(1)},
		{model.Float64Val(2), model.Int64Val(2), model.NullVal(model.TypeFloat64)},
	})
	aggregateProjectionReleased(t, env.Query)
}
