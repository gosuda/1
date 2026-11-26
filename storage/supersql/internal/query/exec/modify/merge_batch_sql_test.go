package modify_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sort"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

func mergeInvocation(t *testing.T, f *sessionExecutionFixture) session.PreparedExecution {
	t.Helper()
	invocation := writableInvocation(t, f)
	invocation.Environment.Apply = apply.Options{MaxRowBytes: 4096, WorkBytes: 1 << 20}
	return invocation
}
func mergeIntRows(t *testing.T, f *sessionExecutionFixture, sql string) ([][]int64, session.ExecutionResult, error) {
	t.Helper()
	prepared := f.prepare(t, sql, nil)
	var rows [][]int64
	result, err := f.session.RunPrepared(t.Context(), prepared, mergeInvocation(t, f), func(ctx context.Context, input *session.Rows) error {
		var err error
		rows, err = collectWritableRows(ctx, input)
		return err
	})
	sort.Slice(rows, func(i, j int) bool {
		for k := range rows[i] {
			if rows[i][k] != rows[j][k] {
				return rows[i][k] < rows[j][k]
			}
		}
		return false
	})
	return rows, result, err
}
func TestMergeExecutesAllMatchClasses(t *testing.T) {
	f := newSessionExecutionFixture(t)
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10),(2,20)`); err != nil {
		t.Fatal(err)
	}
	got, result, err := mergeIntRows(t, f, `MERGE INTO items AS t USING (VALUES(1,5),(3,30)) AS s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=t.value+s.v WHEN NOT MATCHED BY TARGET THEN INSERT(id,value) VALUES(s.id,s.v) WHEN NOT MATCHED BY SOURCE THEN DELETE RETURNING t.id,t.value`)
	if err != nil || result.RowsAffected != 3 || !reflect.DeepEqual(got, [][]int64{{1, 15}, {2, 20}, {3, 30}}) {
		t.Fatalf("merge rows=%v result=%+v error=%v", got, result, err)
	}
	if value, err := f.committedValue(t, 1); err != nil || value != 15 {
		t.Fatalf("updated row=%d,%v", value, err)
	}
	if _, err := f.committedValue(t, 2); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("deleted row=%v", err)
	}
	if value, err := f.committedValue(t, 3); err != nil || value != 30 {
		t.Fatalf("inserted row=%d,%v", value, err)
	}
}

func TestMergeOrderedConditionsAndPresenceAreIndependentOfNullData(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      [][]int64
		affected  uint64
	}{
		{"no-op duplicate", `MERGE INTO items t USING (VALUES(1,0),(1,5)) s(id,v) ON t.id=s.id WHEN MATCHED AND s.v>0 THEN UPDATE SET value=t.value+s.v WHEN MATCHED THEN DO NOTHING RETURNING t.id,t.value`, [][]int64{{1, 15}}, 1},
		{"null then later condition", `MERGE INTO items t USING (VALUES(1,NULL::int)) s(id,v) ON t.id=s.id WHEN MATCHED AND s.v>0 THEN DELETE WHEN MATCHED THEN UPDATE SET value=7 RETURNING t.id,t.value`, [][]int64{{1, 7}}, 1},
		{"nullable source marker", `MERGE INTO items t USING (VALUES(NULL::int,20)) s(id,v) ON t.id=s.id WHEN NOT MATCHED BY TARGET THEN INSERT(id,value) VALUES(99,s.v) WHEN NOT MATCHED BY SOURCE THEN DO NOTHING RETURNING t.id,t.value`, [][]int64{{99, 20}}, 1},
		{"simultaneous assignment", `MERGE INTO items t USING (VALUES(1)) s(id) ON t.id=s.id WHEN MATCHED THEN UPDATE SET id=t.value,value=t.id RETURNING t.id,t.value`, [][]int64{{10, 1}}, 1},
		{"scalar assignment", `MERGE INTO items t USING (VALUES(1,5)) s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=(SELECT t.value+s.v) RETURNING t.id,(SELECT new.value)`, [][]int64{{1, 15}}, 1},
		{"all no-op", `MERGE INTO items t USING (VALUES(1)) s(id) ON t.id=s.id WHEN MATCHED THEN DO NOTHING RETURNING t.id,t.value`, nil, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newSessionExecutionFixture(t)
			if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10)`); err != nil {
				t.Fatal(err)
			}
			got, result, err := mergeIntRows(t, f, scenario.sql)
			if err != nil || result.RowsAffected != scenario.affected || !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("rows=%v want=%v result=%+v error=%v", got, scenario.want, result, err)
			}
		})
	}
}
func TestMergeCardinalityAndUniqueErrorsRollBackEveryAction(t *testing.T) {
	for _, scenario := range []struct{ name, sql, code string }{
		{"repeated update", `MERGE INTO items t USING (VALUES(1,1),(1,2)) s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=t.value+s.v`, "21000"},
		{"moved key repeated", `MERGE INTO items t USING (VALUES(1),(1)) s(id) ON t.id=s.id WHEN MATCHED THEN UPDATE SET id=t.id+10`, "21000"},
		{"on predicate is not filtering", `MERGE INTO items t USING (VALUES(1,20)) s(id,v) ON t.id=s.id AND t.value<0 WHEN NOT MATCHED THEN INSERT(id,value) VALUES(s.id,s.v)`, "23505"},
		{"late action error", `MERGE INTO items t USING (VALUES(1,2),(2,0)) s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=10/s.v`, "22012"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newSessionExecutionFixture(t)
			if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10),(2,20)`); err != nil {
				t.Fatal(err)
			}
			_, result, err := mergeIntRows(t, f, scenario.sql)
			requireUniqueSQLState(t, err, scenario.code)
			if result.RowsAffected != 0 {
				t.Fatalf("failed MERGE count=%d", result.RowsAffected)
			}
			for id, want := range map[int64]int64{1: 10, 2: 20} {
				if value, err := f.committedValue(t, id); err != nil || value != want {
					t.Fatalf("rollback row %d=%d,%v", id, value, err)
				}
			}
		})
	}
}
func TestMergeReturningReportsActionAndNullableImages(t *testing.T) {
	f := newSessionExecutionFixture(t)
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10),(2,20)`); err != nil {
		t.Fatal(err)
	}
	sql := `MERGE INTO items t USING (VALUES(1,5),(3,30)) s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=t.value+s.v WHEN NOT MATCHED BY TARGET THEN INSERT(id,value) VALUES(s.id,s.v) WHEN NOT MATCHED BY SOURCE THEN DELETE RETURNING merge_action(),old.value,new.value`
	prepared := f.prepare(t, sql, nil)
	got := map[string][2]model.Value{}
	result, err := f.session.RunPrepared(t.Context(), prepared, mergeInvocation(t, f), func(ctx context.Context, rows *session.Rows) error {
		var batch columnar.Batch
		for {
			err := rows.Next(ctx, &batch)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			for row := range batch.Rows() {
				action, _, err := columnar.CopyValue(ctx, &batch.Columns[0], batch.Lane(row), 4096)
				if err != nil {
					return err
				}
				var images [2]model.Value
				for i := range images {
					images[i], _, err = columnar.CopyValue(ctx, &batch.Columns[i+1], batch.Lane(row), 4096)
					if err != nil {
						return err
					}
				}
				got[action.StrVal] = images
			}
		}
	})
	want := map[string][2]model.Value{"INSERT": {model.NullVal(model.TypeInt64), model.Int64Val(30)}, "UPDATE": {model.Int64Val(10), model.Int64Val(15)}, "DELETE": {model.Int64Val(20), model.NullVal(model.TypeInt64)}}
	if err != nil || result.RowsAffected != 3 || !reflect.DeepEqual(got, want) {
		t.Fatalf("action images=%v result=%+v error=%v", got, result, err)
	}
}
func TestMergeExecutesAsMandatoryWritableCTE(t *testing.T) {
	f := newSessionExecutionFixture(t)
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10)`); err != nil {
		t.Fatal(err)
	}
	prepared := f.prepare(t, `WITH m AS (MERGE INTO items t USING (VALUES(1,5)) s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=t.value+s.v RETURNING t.value) SELECT value FROM m LIMIT 0`, nil)
	_, err := f.session.RunPrepared(t.Context(), prepared, mergeInvocation(t, f), func(context.Context, *session.Rows) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if value, err := f.committedValue(t, 1); err != nil || value != 15 {
		t.Fatalf("mandatory MERGE=%d,%v", value, err)
	}
}

func TestMergeSelfSourceUsesExplicitTargetIdentity(t *testing.T) {
	f := newSessionExecutionFixture(t)
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,2),(2,1)`); err != nil {
		t.Fatal(err)
	}
	got, result, err := mergeIntRows(t, f, `MERGE INTO items t USING items s ON t.id=s.value WHEN MATCHED THEN UPDATE SET value=s.id*10 RETURNING t.id,t.value`)
	if err != nil || result.RowsAffected != 2 || !reflect.DeepEqual(got, [][]int64{{1, 20}, {2, 10}}) {
		t.Fatalf("self MERGE rows=%v result=%+v error=%v", got, result, err)
	}
}

func TestMergeUniqueIndexFailureRestoresBaseAndIndex(t *testing.T) {
	f, index := uniqueSessionFixture(t)
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10),(2,20)`); err != nil {
		t.Fatal(err)
	}
	_, result, err := mergeIntRows(t, f, `MERGE INTO items t USING (VALUES(1,20)) s(id,v) ON t.id=s.id WHEN MATCHED THEN UPDATE SET value=s.v RETURNING t.value`)
	requireUniqueSQLState(t, err, "23505")
	if result.RowsAffected != 0 {
		t.Fatalf("failed unique MERGE count=%d", result.RowsAffected)
	}
	requireUniquePair(t, f, index, 1, 10)
	requireUniquePair(t, f, index, 2, 20)
}
