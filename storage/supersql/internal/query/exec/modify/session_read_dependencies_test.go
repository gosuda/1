package modify_test

import (
	"context"
	"io"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

func TestSessionPreparedDerivesDescriptorAndScanReads(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	insert := fixture.prepare(t, `INSERT INTO items VALUES(1,10),(2,20)`, nil)
	invocation := fixture.invocation(t)
	invocation.Dependencies = nil
	result, err := fixture.session.RunPrepared(t.Context(), insert, invocation, func(context.Context, *session.Rows) error { return nil })
	if err != nil || !result.Commit.Committed || result.RowsAffected != 2 {
		t.Fatalf("derived insert dependencies=%+v,%v", result, err)
	}
	prepared := fixture.prepare(t, `SELECT value FROM items WHERE id=1`, nil)
	invocation = fixture.invocation(t)
	invocation.Dependencies = nil
	seen := 0
	_, err = fixture.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		for {
			var batch columnar.Batch
			err := rows.Next(ctx, &batch)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], 0, 4096)
			if err != nil {
				return err
			}
			if value.IntVal != 10 {
				t.Fatalf("derived scan read=%v", value)
			}
			seen += batch.Rows()
		}
	})
	if err != nil || seen != 1 {
		t.Fatalf("derived scan rows=%d error=%v", seen, err)
	}
	usage := invocation.Environment.Query.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("dependency discovery leaked query resources: %+v", usage)
	}
}

func TestDerivedScanSpansDoNotBlockUnrelatedKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newSessionExecutionFixture(t)
		seed := fixture.prepare(t, `INSERT INTO items VALUES(1,10)`, nil)
		invocation := fixture.invocation(t)
		invocation.Dependencies = nil
		if _, err := fixture.session.RunPrepared(t.Context(), seed, invocation, func(context.Context, *session.Rows) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
			t.Fatal(err)
		}
		read := fixture.prepare(t, `SELECT value FROM items WHERE id=1`, nil, func(options *session.PrepareOptions) {
			options.Optimize.Access = keyedMutationAccess{options.Optimize.Access.(mutationAccess)}
		})
		invocation = fixture.invocation(t)
		invocation.Dependencies = nil
		if _, err := fixture.session.RunPrepared(t.Context(), read, invocation, func(ctx context.Context, rows *session.Rows) error {
			for {
				var batch columnar.Batch
				err := rows.Next(ctx, &batch)
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
			}
		}); err != nil {
			t.Fatal(err)
		}
		writer := fixture.peer(t)
		insert := fixture.prepare(t, `INSERT INTO items VALUES(2,20)`, nil)
		write := fixture.invocation(t)
		write.Dependencies = nil
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := writer.RunPrepared(ctx, insert, write, func(context.Context, *session.Rows) error { return nil })
			done <- err
		}()
		synctest.Wait()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			cancel()
			if err := fixture.session.Rollback(t.Context(), false); err != nil {
				t.Fatal(err)
			}
			<-done
			t.Fatal("point-read dependency blocked a disjoint key")
		}
		if err := fixture.session.Rollback(t.Context(), false); err != nil {
			t.Fatal(err)
		}
		if value, err := fixture.committedValue(t, 2); err != nil || value != 20 {
			t.Fatalf("disjoint committed row=%d,%v", value, err)
		}
	})
}

type keyedMutationAccess struct{ mutationAccess }

func (access keyedMutationAccess) Access(ctx context.Context, request opt.AccessRequest) (opt.Access, error) {
	paths, err := access.mutationAccess.Access(ctx, request)
	if err == nil {
		paths.Paths[0].Algorithm = physical.PrimaryKeyScan
	}
	return paths, err
}
