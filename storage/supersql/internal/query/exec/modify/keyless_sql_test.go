package modify_test

import (
	"reflect"
	"testing"
)

func TestPhysicalIdentityPreservesDuplicateSQLRows(t *testing.T) {
	f, d := physicalColumnExpressionFixture(t, false, "", false, true)
	for _, step := range []struct {
		sql  string
		rows [][]int64
	}{
		{`INSERT INTO computed VALUES(1,2,DEFAULT),(1,2,DEFAULT) RETURNING *`, [][]int64{{1, 2, 4}, {1, 2, 4}}},
		{`SELECT * FROM computed AS t(a,b,c)`, [][]int64{{1, 2, 4}, {1, 2, 4}}},
		{`UPDATE computed SET value=3 RETURNING *`, [][]int64{{1, 3, 6}, {1, 3, 6}}},
		{`MERGE INTO computed t USING (VALUES(1,4)) s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=s.v RETURNING t.*`, [][]int64{{1, 4, 8}, {1, 4, 8}}},
		{`DELETE FROM computed RETURNING *`, [][]int64{{1, 4, 8}, {1, 4, 8}}},
	} {
		got, err := runColumnSQL(t, f, d, step.sql)
		if err != nil || !reflect.DeepEqual(got, step.rows) {
			t.Fatalf("%s: got %v, %v; want %v", step.sql, got, err, step.rows)
		}
	}
}
