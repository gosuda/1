package modify_test

import (
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSequenceDefaultsCompleteOnceBeforeConflict(t *testing.T) {
	f, runtime := sequenceSQLFixture(t)
	f, descriptor := populateColumnExpressionFixture(t, f, false, "doubled > 0", true, false, `nextval('numbers')`)
	expressions := model.ExpressionRuntime{Sequences: runtime, Relations: runtime.(model.RelationRuntime)}
	_, err := runSequenceSQL(t, f, runtime, `SELECT currval('numbers')`)
	requireUniqueSQLState(t, err, "55000")
	for _, step := range []struct {
		sql     string
		rows    [][]int64
		current int64
	}{
		{`INSERT INTO computed(id) VALUES(1) RETURNING *`, [][]int64{{1, 1, 2}}, 1},
		{`INSERT INTO computed(id) VALUES(1) ON CONFLICT(id) DO NOTHING RETURNING value`, nil, 2},
		{`INSERT INTO computed(id) VALUES(1) ON CONFLICT(id) DO UPDATE SET value=DEFAULT RETURNING *`, [][]int64{{1, 4, 8}}, 4},
		{`UPDATE computed SET value=DEFAULT WHERE id=1 RETURNING *`, [][]int64{{1, 5, 10}}, 5},
		{`MERGE INTO computed t USING (VALUES(1)) s(id) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=DEFAULT RETURNING t.*`, [][]int64{{1, 6, 12}}, 6},
		{`MERGE INTO computed t USING (VALUES(2)) s(id) ON t.id=s.id WHEN NOT MATCHED THEN INSERT(id) VALUES(s.id) RETURNING t.*`, [][]int64{{2, 7, 14}}, 7},
	} {
		got, err := runColumnRuntime(t, f, descriptor, step.sql, expressions)
		if err != nil || !reflect.DeepEqual(got, step.rows) {
			t.Fatalf("%s: %v,%v; want %v", step.sql, got, err, step.rows)
		}
		got, err = runSequenceSQL(t, f, runtime, `SELECT currval('numbers')`)
		if err != nil || !reflect.DeepEqual(got, [][]int64{{step.current}}) {
			t.Fatalf("%s advanced sequence incorrectly: %v,%v", step.sql, got, err)
		}
	}
}
