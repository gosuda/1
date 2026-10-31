package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestArrayConstructorReusesMatchingGroupedExpressions(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		result    model.TypeID
	}{
		{"typed outer", `SELECT ARRAY[ARRAY[x]]::integer[] FROM (VALUES(1))t(x) GROUP BY ARRAY[x]`, 1007},
		{"untyped outer", `SELECT ARRAY[ARRAY[x]] FROM (VALUES(1))t(x) GROUP BY ARRAY[x]`, 1007},
		{"matching cast group", `SELECT ARRAY[ARRAY[x]]::bigint[] FROM (VALUES(1))t(x) GROUP BY ARRAY[x]::bigint[]`, 1016},
		{"identity cast", `SELECT ARRAY[x]::integer[] FROM (VALUES(1))t(x) GROUP BY ARRAY[x]`, 1007},
		{"preserved collation", `SELECT ARRAY[ARRAY[x]]::text[] FROM (VALUES('a'::text COLLATE "C"))t(x) GROUP BY ARRAY[x]`, 1009},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bound, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			columns := bound.Columns()
			if len(columns) != 1 || columns[0].Layout.Type.ID != tc.result {
				t.Fatalf("grouped array output: %+v", columns)
			}
		})
	}
}

func TestArrayConstructorDoesNotReuseDifferentlyCastGroup(t *testing.T) {
	for _, sql := range []string{
		`SELECT ARRAY[ARRAY[x]]::bigint[] FROM (VALUES(1))t(x) GROUP BY ARRAY[x]`,
		`SELECT ARRAY[x]::bigint[] FROM (VALUES(1))t(x) GROUP BY ARRAY[x]`,
	} {
		_, err := bindSQL(t, sql, nil, nil)
		var sqlErr *model.Error
		if !errors.As(err, &sqlErr) || sqlErr.Code != "42803" {
			t.Fatalf("different grouped constructor: %v; want 42803", err)
		}
	}
}
