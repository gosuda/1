package modify_test

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	exapply "github.com/gosuda/supersql-alpha/internal/query/exec/apply"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

func TestNestedScalarReadProtectsAbsentPrimaryKey(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newSessionExecutionFixture(t)
		prepared := fixture.prepare(t, `SELECT COALESCE((SELECT value FROM items WHERE id=5),99)`, nil, func(options *session.PrepareOptions) {
			options.Optimize.Access = keyedMutationAccess{options.Optimize.Access.(mutationAccess)}
		})
		if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
			t.Fatal(err)
		}
		invocation := fixture.invocation(t)
		invocation.Dependencies = nil
		invocation.Environment.Apply = exapply.Options{MaxRowBytes: 4096, WorkBytes: 1 << 20}
		invocation.Environment.Spool = rowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 10000, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 4}
		_, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
			var batch columnar.Batch
			if err := rows.Next(ctx, &batch); err != nil {
				return err
			}
			if batch.Rows() != 1 {
				t.Fatalf("scalar empty lookup returned %d rows", batch.Rows())
			}
			value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], batch.Lane(0), 4096)
			if err != nil {
				return err
			}
			if value.IntVal != 99 {
				t.Fatalf("empty scalar lookup = %v, want 99", value)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		writer := fixture.peer(t)
		writePlan := fixture.prepare(t, `INSERT INTO items VALUES(5,50)`, nil)
		write := fixture.invocation(t)
		write.Dependencies = nil
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			defer close(done)
			_, err := writer.RunPrepared(ctx, writePlan, write, func(context.Context, *session.Rows) error { return nil })
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
			t.Fatalf("nested-only scan failed to protect absent key: %v", err)
		default:
		}
		outside := fixture.peer(t)
		if err := runUniqueSQL(t, fixture, outside, `INSERT INTO items VALUES(6,60)`); err != nil {
			t.Fatal(err)
		}
		if err := fixture.session.Rollback(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if value, err := fixture.committedValue(t, 5); err != nil || value != 50 {
			t.Fatalf("writer after scalar lock release = %d,%v", value, err)
		}
		usage := invocation.Environment.Query.Usage()
		if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Readers != 0 || usage.Files != 0 {
			t.Fatalf("scalar read retained query resources: %+v", usage)
		}
	})
}

func TestScalarSubqueriesUseMutationAssignmentAndReturningImages(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	if err := runUniqueSQL(t, fixture, fixture.session, `INSERT INTO items VALUES(1,10)`); err != nil {
		t.Fatal(err)
	}
	prepared := fixture.prepare(t, `UPDATE items SET value=1+(SELECT items.value) WHERE id=1 RETURNING (SELECT new.value),(SELECT old.value)`, nil)
	invocation := fixture.invocation(t)
	invocation.Dependencies = nil
	invocation.Environment.Apply = exapply.Options{MaxRowBytes: 4096, WorkBytes: 1 << 20}
	seen := 0
	result, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		var batch columnar.Batch
		if err := rows.Next(ctx, &batch); err != nil {
			return err
		}
		for row := range batch.Rows() {
			for column, want := range []int64{11, 10} {
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[column], batch.Lane(row), 4096)
				if err != nil {
					return err
				}
				if value.IntVal != want {
					t.Fatalf("returning scalar column %d = %v, want %d", column, value, want)
				}
			}
			seen++
		}
		return nil
	})
	if err != nil || seen != 1 || result.RowsAffected != 1 || !result.Commit.Committed {
		t.Fatalf("scalar mutation result=%+v rows=%d error=%v", result, seen, err)
	}
	if value, err := fixture.committedValue(t, 1); err != nil || value != 11 {
		t.Fatalf("committed scalar assignment = %d,%v", value, err)
	}
}
