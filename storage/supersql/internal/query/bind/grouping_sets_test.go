package bind_test

import (
	"fmt"
	"strings"
	"testing"
)

func TestGroupingCubeExpansionIsBounded(t *testing.T) {
	var columns, keys []string
	for i := range 18 {
		columns = append(columns, fmt.Sprintf("1 AS k%d", i))
		keys = append(keys, fmt.Sprintf("k%d", i))
	}
	sql := "SELECT count(*) FROM (SELECT " + strings.Join(columns, ",") + ") v GROUP BY CUBE(" + strings.Join(keys, ",") + ")"
	statement, err := bindSQL(t, sql, nil, nil)
	if statement != nil {
		t.Fatal("oversized grouping cube returned a partial statement")
	}
	requireCode(t, err, "54000")
}

func TestGroupingFunctionRejectsInvalidArguments(t *testing.T) {
	for _, scenario := range []struct{ name, sql, code string }{
		{"not grouped", `SELECT GROUPING(a) FROM (VALUES(1)) t(a)`, "42803"},
		{"different expression", `SELECT GROUPING(a+1) FROM (VALUES(1)) t(a) GROUP BY a`, "42803"},
		{"aggregate argument", `SELECT sum(GROUPING(a)) FROM (VALUES(1)) t(a) GROUP BY a`, "42803"},
		{"WHERE scope", `SELECT a FROM (VALUES(1)) t(a) WHERE GROUPING(a)=0 GROUP BY a`, "42803"},
		{"no GROUPING_ID alias", `SELECT GROUPING_ID(a) FROM (VALUES(1)) t(a) GROUP BY a`, "42883"},
		{"too many arguments", `SELECT GROUPING(` + strings.TrimSuffix(strings.Repeat("a,", 32), ",") + `) FROM (VALUES(1)) t(a) GROUP BY a`, "54000"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, nil, nil)
			if statement != nil {
				t.Fatal("invalid GROUPING returned a bound statement")
			}
			requireCode(t, err, scenario.code)
		})
	}
}
