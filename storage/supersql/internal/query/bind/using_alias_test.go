package bind_test

import "testing"

func TestBindUsingJoinAliasScopeErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		code string
	}{
		{
			name: "whole alias hides left qualifier",
			sql: `SELECT l.k FROM (
				(VALUES(1)) l(k) JOIN (VALUES(1)) r(k) USING(k)
			) AS j`,
			code: "42P01",
		},
		{
			name: "whole alias hides right star",
			sql: `SELECT r.* FROM (
				(VALUES(1)) l(k) JOIN (VALUES(1)) r(k) USING(k)
			) AS j`,
			code: "42P01",
		},
		{
			name: "whole alias hides USING qualifier",
			sql: `SELECT u.k FROM (
				(VALUES(1)) l(k) JOIN (VALUES(1)) r(k) USING(k) AS u
			) AS j`,
			code: "42P01",
		},
		{
			name: "whole alias hides USING star",
			sql: `SELECT u.* FROM (
				(VALUES(1)) l(k) JOIN (VALUES(1)) r(k) USING(k) AS u
			) AS j`,
			code: "42P01",
		},
		{
			name: "USING alias excludes nonmerged members",
			sql: `SELECT u.lv
				FROM (VALUES(1,11)) l(k,lv)
				JOIN (VALUES(1,21)) r(k,rv) USING(k) AS u`,
			code: "42703",
		},
		{
			name: "column alias replaces merged column name",
			sql: `SELECT j.k FROM (
				(VALUES(11,1)) l(lv,k) JOIN (VALUES(1,21)) r(k,rv) USING(k)
			) AS j(merged_key,left_value,right_value)`,
			code: "42703",
		},
		{
			name: "USING alias conflicts with join input",
			sql: `SELECT * FROM (VALUES(1)) l(k)
				JOIN (VALUES(1)) r(k) USING(k) AS l`,
			code: "42712",
		},
		{
			name: "USING alias conflicts with preceding relation",
			sql: `SELECT * FROM (VALUES(9)) u(x),
				(VALUES(1)) l(k) JOIN (VALUES(1)) r(k) USING(k) AS u`,
			code: "42712",
		},
		{
			name: "following relation conflicts with USING alias",
			sql: `SELECT * FROM
				(VALUES(1)) l(k) JOIN (VALUES(1)) r(k) USING(k) AS u,
				(VALUES(9)) u(x)`,
			code: "42712",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, nil, nil)
			requireCode(t, err, tc.code)
		})
	}
}
