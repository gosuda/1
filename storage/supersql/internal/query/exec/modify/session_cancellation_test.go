package modify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

func TestSessionPreparedCancellationRollsBackImplicitWrites(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	prepared := fixture.prepare(t, `INSERT INTO items VALUES(1,10),(2,20) RETURNING id`, nil)
	invocation := fixture.invocation(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := fixture.session.RunPrepared(ctx, prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		batch := columnar.Batch{MaxRows: 1}
		if err := rows.Next(ctx, &batch); err != nil {
			return err
		}
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled execution = %v", err)
	}
	if fixture.session.Active() {
		t.Fatal("implicit transaction survived cancellation")
	}
	for _, id := range []int64{1, 2} {
		if _, err := fixture.committedValue(t, id); !errors.Is(err, storage.ErrRowNotFound) {
			t.Fatalf("canceled row %d persisted: %v", id, err)
		}
	}
	usage := invocation.Environment.Query.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("cancellation retained resources: %+v", usage)
	}
}
