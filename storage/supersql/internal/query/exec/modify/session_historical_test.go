package modify_test

import (
	"context"
	"fmt"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestSessionPreparedHistoricalSnapshotDoesNotReadCurrentClock(t *testing.T) {
	for _, mode := range []string{"implicit", "explicit read-only"} {
		t.Run(mode, func(t *testing.T) { runSessionHistorical(t, mode) })
	}
}

func runSessionHistorical(t *testing.T, mode string) {
	t.Helper()
	fixture := newSessionExecutionFixture(t)
	first, err := fixture.session.RunPrepared(t.Context(), fixture.prepare(t, `INSERT INTO items VALUES(1,10)`, nil), fixture.invocation(t), func(context.Context, *session.Rows) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.session.RunPrepared(t.Context(), fixture.prepare(t, `INSERT INTO items VALUES(2,20)`, nil), fixture.invocation(t), func(context.Context, *session.Rows) error { return nil }); err != nil {
		t.Fatal(err)
	}
	sql := fmt.Sprintf("SELECT value FROM items AS OF SYSTEM TIME %d", first.Commit.Result.CommitTS)
	parsed, err := zpgparse.ParseExtended(sql, zpgparse.ParseOptions{AllowSystemTime: true})
	if err != nil {
		t.Fatal(err)
	}
	planning := fixture.planning(sql)
	request := fixture.store.request(t, 1)
	fixture.store.clock.mu.Lock()
	fixture.store.clock.epoch.Config[0]++
	fixture.store.clock.unavailable = true
	fixture.store.clock.mu.Unlock()
	historical, err := session.PrepareHistorical(t.Context(), parsed, 0, nil, session.HistoricalOptions{Snapshots: fixture.store.manager, Catalog: fixture.catalog, SystemTime: session.SystemTimeOptions{Bind: planning.Bind, Expressions: expr.Options{Limits: expr.DefaultLimits(), Builtin: fixture.settings.BuiltinOptions(950, 950)}, Converter: fixture.store.clock, MaxClauses: 8, MaxBytes: 16 << 20}, Planning: func(_ context.Context, _ *catalog.CatalogSnapshot, reader storage.SnapshotReader) (session.PrepareOptions, error) {
		options := planning
		options.Optimize.Access = mutationAccess{reader: reader, target: request.Target, layout: request.Layout, keys: fixture.store.options.KeyLayout}
		return options, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	prepared := historical.Prepared()
	if err := historical.Close(); err != nil {
		t.Fatal(err)
	}
	if mode == "explicit read-only" {
		readOnly := true
		if err := fixture.session.Begin(t.Context(), session.TransactionStart{Characteristics: session.BeginOptions{ReadOnly: &readOnly}}); err != nil {
			t.Fatal(err)
		}
	}
	invocation := fixture.invocation(t)
	seen := 0
	result, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		for {
			batch := columnar.Batch{MaxRows: 1}
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
				t.Fatalf("historical value = %d; want 10", value.IntVal)
			}
			seen += batch.Rows()
		}
	})
	expectedCommit := mode == "implicit"
	if err != nil || result.Commit.Committed != expectedCommit || seen != 1 {
		t.Fatalf("historical session = %+v rows=%d error=%v", result, seen, err)
	}
	usage := invocation.Environment.Query.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("historical resources retained: %+v", usage)
	}
	if !expectedCommit {
		seen = 0
		_, err := fixture.session.RunPrepared(t.Context(), prepared, fixture.invocation(t), func(ctx context.Context, rows *session.Rows) error {
			for {
				var batch columnar.Batch
				err := rows.Next(ctx, &batch)
				if err == io.EOF {
					return nil
				}
				if err != nil {
					return err
				}
				seen += batch.Rows()
			}
		})
		if err != nil || seen != 1 {
			t.Fatalf("reused historical snapshot rows=%d error=%v", seen, err)
		}
		if _, err := fixture.session.Commit(t.Context(), false); err != nil {
			t.Fatal(err)
		}
	}
}
