package modify_test

import (
	"reflect"
	"testing"
)

func TestCheckUsesCompletedRowBeforeConflictRouting(t *testing.T) {
	f, descriptor := checkedColumnExpressionFixture(t, false, "doubled > 0", true)
	got, err := runColumnSQL(t, f, descriptor, `INSERT INTO computed(id) VALUES(1) RETURNING id,value,doubled`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{1, 7, 14}}) {
		t.Fatalf("completed row = %v, %v", got, err)
	}
	for _, sql := range []string{
		`INSERT INTO computed(id,value) VALUES(1,-1) ON CONFLICT(id) DO NOTHING`,
		`INSERT INTO computed(id,value) VALUES(2,2),(3,-3)`,
		`UPDATE computed SET value=-1 WHERE id=1`,
		`INSERT INTO computed(id,value) VALUES(1,1) ON CONFLICT(id) DO UPDATE SET value=-excluded.value`,
		`MERGE INTO computed t USING (VALUES(1,-1)) s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=s.v`,
	} {
		_, err = runColumnSQL(t, f, descriptor, sql)
		requireUniqueSQLState(t, err, "23514")
		got, err = runColumnSQL(t, f, descriptor, `SELECT id,value,doubled FROM computed`)
		if err != nil || !reflect.DeepEqual(got, [][]int64{{1, 7, 14}}) {
			t.Fatalf("after %s: rows=%v, error=%v", sql, got, err)
		}
	}
}

func TestCheckUnknownPasses(t *testing.T) {
	f, descriptor := checkedColumnExpressionFixture(t, false, "NULL::boolean", true)
	got, err := runColumnSQL(t, f, descriptor, `INSERT INTO computed(id) VALUES(1) RETURNING doubled`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{14}}) {
		t.Fatalf("unknown check = %v, %v", got, err)
	}
}

func TestUnenforcedCheckDoesNotEvaluate(t *testing.T) {
	f, descriptor := checkedColumnExpressionFixture(t, false, "1 / (value-value) > 0", false)
	got, err := runColumnSQL(t, f, descriptor, `INSERT INTO computed(id) VALUES(1) RETURNING doubled`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{14}}) {
		t.Fatalf("unenforced check = %v, %v", got, err)
	}
}
