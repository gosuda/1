package modify_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

func conflictRows(t *testing.T, f *sessionExecutionFixture, sql string) ([][]int64, session.ExecutionResult, error) {
	t.Helper()
	prepared := f.prepare(t, sql, nil)
	var got [][]int64
	result, err := f.session.RunPrepared(t.Context(), prepared, mergeInvocation(t, f), func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	return got, result, err
}
func TestConflictActionsUseSelectedPrimaryOrSecondaryOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	for _, scenario := range []struct {
		name, sql string
		want      [][]int64
		affected  uint64
	}{
		{"primary update", `INSERT INTO items VALUES(1,5) ON CONFLICT(id) DO UPDATE SET value=items.value+excluded.value RETURNING old.id,old.value,new.id,new.value`, [][]int64{{1, 10, 1, 15}}, 1},
		{"secondary owner", `INSERT INTO items VALUES(9,10) ON CONFLICT(value) DO UPDATE SET value=items.value+excluded.id RETURNING old.id,old.value,new.id,new.value`, [][]int64{{1, 10, 1, 19}}, 1},
		{"targetless nothing", `INSERT INTO items VALUES(9,10) ON CONFLICT DO NOTHING RETURNING id`, nil, 0},
		{"selected nothing", `INSERT INTO items VALUES(9,10) ON CONFLICT(value) DO NOTHING RETURNING id`, nil, 0},
		{"false then true", `INSERT INTO items VALUES(1,0),(1,5) ON CONFLICT(id) DO UPDATE SET value=excluded.value WHERE excluded.value>0 RETURNING value`, [][]int64{{5}}, 1},
		{"alias target", `INSERT INTO items AS t VALUES(1,5) ON CONFLICT(id) DO UPDATE SET value=t.value+excluded.value RETURNING value`, [][]int64{{15}}, 1},
		{"scalar action", `INSERT INTO items VALUES(1,5) ON CONFLICT(id) DO UPDATE SET value=(SELECT items.value+excluded.value) RETURNING value`, [][]int64{{15}}, 1},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, _ := uniqueSessionFixture(t)
			if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10),(2,20)`); err != nil {
				t.Fatal(err)
			}
			got, result, err := conflictRows(t, f, scenario.sql)
			if err != nil || result.RowsAffected != scenario.affected || !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("rows=%v want=%v result=%+v error=%v", got, scenario.want, result, err)
			}
		})
	}
}
func TestConflictErrorsNeverSuppressOtherConstraintsOrRepeatTargets(t *testing.T) {
	for _, scenario := range []struct{ name, sql, code string }{
		{"unselected primary", `INSERT INTO items VALUES(1,30) ON CONFLICT(value) DO NOTHING`, "23505"},
		{"nonarbiter update violation", `INSERT INTO items VALUES(1,20) ON CONFLICT(id) DO UPDATE SET value=excluded.value`, "23505"},
		{"not null before skip", `INSERT INTO items VALUES(1,NULL) ON CONFLICT DO NOTHING`, "23502"},
		{"duplicate update", `INSERT INTO items VALUES(1,11),(1,12) ON CONFLICT(id) DO UPDATE SET value=excluded.value`, "21000"},
		{"moved owner duplicate", `INSERT INTO items VALUES(9,10),(8,10) ON CONFLICT(value) DO UPDATE SET id=excluded.id`, "21000"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, index := uniqueSessionFixture(t)
			if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10),(2,20)`); err != nil {
				t.Fatal(err)
			}
			_, result, err := conflictRows(t, f, scenario.sql)
			requireUniqueSQLState(t, err, scenario.code)
			if result.RowsAffected != 0 {
				t.Fatalf("failed conflict count=%d", result.RowsAffected)
			}
			requireUniquePair(t, f, index, 1, 10)
			requireUniquePair(t, f, index, 2, 20)
		})
	}
}
func TestConflictUpdateRejectsRowsInsertedEarlierBySameCommand(t *testing.T) {
	f := newSessionExecutionFixture(t)
	_, result, err := conflictRows(t, f, `INSERT INTO items VALUES(1,10),(1,20) ON CONFLICT(id) DO UPDATE SET value=excluded.value`)
	requireUniqueSQLState(t, err, "21000")
	if result.RowsAffected != 0 {
		t.Fatalf("failed insertion count=%d", result.RowsAffected)
	}
	if _, err := f.committedValue(t, 1); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("earlier proposed row survived: %v", err)
	}
}
func TestConflictNothingDeduplicatesProposalsWithoutUpdating(t *testing.T) {
	f := newSessionExecutionFixture(t)
	got, result, err := conflictRows(t, f, `INSERT INTO items VALUES(1,10),(1,20) ON CONFLICT(id) DO NOTHING RETURNING value`)
	if err != nil || result.RowsAffected != 1 || !reflect.DeepEqual(got, [][]int64{{10}}) {
		t.Fatalf("nothing rows=%v result=%+v error=%v", got, result, err)
	}
}
func TestConflictFalseWhereRetainsOwnerUpdateLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _ := uniqueSessionFixture(t)
		if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10)`); err != nil {
			t.Fatal(err)
		}
		if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
			t.Fatal(err)
		}
		_, result, err := conflictRows(t, f, `INSERT INTO items VALUES(9,10) ON CONFLICT(value) DO UPDATE SET value=30 WHERE FALSE RETURNING value`)
		if err != nil || result.RowsAffected != 0 {
			t.Fatalf("false action=%+v,%v", result, err)
		}
		peer := f.peer(t)
		writePlan := f.prepare(t, `UPDATE items SET value=40 WHERE id=1`, nil)
		invocation := writableInvocation(t, f)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			defer close(done)
			_, err := peer.RunPrepared(ctx, writePlan, invocation, func(context.Context, *session.Rows) error { return nil })
			done <- err
		}()
		defer func() {
			cancel()
			for range done {
			}
		}()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("false WHERE released conflict owner: %v", err)
		default:
		}
		if err := f.session.Rollback(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if value, err := f.committedValue(t, 1); err != nil || value != 40 {
			t.Fatalf("owner update after release=%d,%v", value, err)
		}
	})
}
func TestConflictConsumerFailureRestoresWholeCommand(t *testing.T) {
	f, index := uniqueSessionFixture(t)
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10)`); err != nil {
		t.Fatal(err)
	}
	prepared := f.prepare(t, `INSERT INTO items VALUES(9,10),(2,20) ON CONFLICT(value) DO UPDATE SET value=11 RETURNING id`, nil)
	_, err := f.session.RunPrepared(t.Context(), prepared, mergeInvocation(t, f), func(ctx context.Context, rows *session.Rows) error {
		var batch columnar.Batch
		if err := rows.Next(ctx, &batch); err != nil {
			return err
		}
		return errSessionConsumer
	})
	if !errors.Is(err, errSessionConsumer) {
		t.Fatalf("consumer conflict error=%v", err)
	}
	requireUniquePair(t, f, index, 1, 10)
	if _, err := f.committedValue(t, 2); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("later insert survived consumer error: %v", err)
	}
}
