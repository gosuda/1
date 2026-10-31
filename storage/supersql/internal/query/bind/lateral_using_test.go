package bind_test

import (
	"fmt"
	"testing"
)

func TestBindLateralOnRejectsAmbiguousUnqualifiedColumns(t *testing.T) {
	for _, join := range []string{"INNER JOIN", "LEFT JOIN"} {
		t.Run(join, func(t *testing.T) {
			sql := fmt.Sprintf(`SELECT l.key, r.key FROM (VALUES (1)) l(key)
				%s LATERAL (SELECT l.key AS key) r ON key = r.key`, join)
			_, err := bindSQL(t, sql, nil, nil)
			requireCode(t, err, "42702")
		})
	}
}

func TestBindCorrelatedLateralRejectsRightAndFullJoins(t *testing.T) {
	for _, join := range []string{"RIGHT JOIN", "FULL JOIN"} {
		for _, tc := range []struct{ name, prefix, condition string }{
			{"on", "", "ON TRUE"},
			{"using", "", "USING (key)"},
			{"natural", "NATURAL ", ""},
		} {
			t.Run(join+"/"+tc.name, func(t *testing.T) {
				sql := fmt.Sprintf(`SELECT * FROM (VALUES (1)) l(key)
					%s%s LATERAL (SELECT l.key AS key) r %s`, tc.prefix, join, tc.condition)
				_, err := bindSQL(t, sql, nil, nil)
				requireCode(t, err, "42P10")
			})
		}
	}
}
