package exec_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLUsingJoinAliasVisibleColumns(t *testing.T) {
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	for _, tc := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{
			name: "whole alias star excludes original merged columns",
			sql: `SELECT *, j.* FROM (
				(VALUES(11,1)) l(lv,k)
				JOIN (VALUES(1,21)) r(k,rv) USING(k) AS u
			) AS j`,
			want: [][]model.Value{{i(1), i(11), i(21), i(1), i(11), i(21)}},
		},
		{
			name: "column aliases follow visible output order",
			sql: `SELECT j.right_value, j.merged_key, j.left_value FROM (
				(VALUES(11,1)) l(lv,k)
				JOIN (VALUES(1,21)) r(k,rv) USING(k)
			) AS j(merged_key,left_value,right_value)`,
			want: [][]model.Value{{i(21), i(1), i(11)}},
		},
		{
			name: "partial column aliases preserve remaining names",
			sql: `SELECT j.merged_key, j.lv, j.rv FROM (
				(VALUES(11,1)) l(lv,k)
				JOIN (VALUES(1,21)) r(k,rv) USING(k)
			) AS j(merged_key)`,
			want: [][]model.Value{{i(1), i(11), i(21)}},
		},
		{
			name: "USING alias exposes only ordered keys and preserves original stars",
			sql: `SELECT u.k, u.*, l.*, r.*
				FROM (VALUES(11,9,1),(12,8,2)) l(lv,z,k)
				LEFT JOIN (VALUES(1,21,9)) r(k,rv,z) USING(k,z) AS u
				ORDER BY u.k`,
			want: [][]model.Value{
				{i(1), i(1), i(9), i(11), i(9), i(1), i(1), i(21), i(9)},
				{i(2), i(2), i(8), i(12), i(8), i(2), null, null, null},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := statefulEnvironment(t)
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

func TestSQLUsingMergedColumnCommonType(t *testing.T) {
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	for _, tc := range []struct {
		name string
		sql  string
		oids []model.TypeID
		want [][]model.Value
	}{
		{
			name: "inner merge uses wider right type",
			sql: `SELECT k, l.k, r.k
				FROM (VALUES(1::int4),(2)) l(k)
				JOIN (VALUES(2::int8),(3)) r(k) USING(k)`,
			oids: []model.TypeID{20, 23, 20},
			want: [][]model.Value{{i(2), i(2), i(2)}},
		},
		{
			name: "left merge coerces preserved narrow key",
			sql: `SELECT k, l.k, r.k
				FROM (VALUES(1::int4),(2)) l(k)
				LEFT JOIN (VALUES(2::int8)) r(k) USING(k)
				ORDER BY 1`,
			oids: []model.TypeID{20, 23, 20},
			want: [][]model.Value{{i(1), i(1), null}, {i(2), i(2), i(2)}},
		},
		{
			name: "right merge coerces preserved narrow key",
			sql: `SELECT k, l.k, r.k
				FROM (VALUES(2::int8)) l(k)
				RIGHT JOIN (VALUES(2::int4),(3)) r(k) USING(k)
				ORDER BY 1`,
			oids: []model.TypeID{20, 20, 23},
			want: [][]model.Value{{i(2), i(2), i(2)}, {i(3), null, i(3)}},
		},
		{
			name: "full merge coalesces keys without narrowing or matching nulls",
			sql: `SELECT u.k, l.k, r.k
				FROM (VALUES(1::int4),(2),(NULL)) l(k)
				FULL JOIN (VALUES(2::int8),(2147483648),(NULL)) r(k) USING(k) AS u
				ORDER BY u.k NULLS LAST`,
			oids: []model.TypeID{20, 23, 20},
			want: [][]model.Value{
				{i(1), i(1), null},
				{i(2), i(2), i(2)},
				{i(2147483648), null, i(2147483648)},
				{null, null, null},
				{null, null, null},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			env := statefulEnvironment(t)
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
