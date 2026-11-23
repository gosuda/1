package modify_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func coveringSessionFixture(t *testing.T) *sessionExecutionFixture {
	t.Helper()
	index, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: 3, OID: 10003, Name: model.NamespaceName{Parent: 2, Kind: model.ObjectIndex, Name: "items_value_cover"},
		Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Index: &model.IndexMetadata{Table: 1, Keys: []model.KeyElement{{Column: 2, Type: integer.Type}}, Include: []model.ColumnID{1}},
	}, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	return newSessionExecutionFixtureWithIndexes(t, index)
}

type coveringOnlyAccess struct{ *session.CatalogAccess }

func (access coveringOnlyAccess) Access(ctx context.Context, request opt.AccessRequest) (opt.Access, error) {
	paths, err := access.CatalogAccess.Access(ctx, request)
	if err != nil {
		return opt.Access{}, err
	}
	paths.Paths = slices.DeleteFunc(paths.Paths, func(path opt.AccessPath) bool { return path.Algorithm != physical.SecondaryScan })
	if len(paths.Paths) == 0 {
		return opt.Access{}, fmt.Errorf("%w: catalog advertised no covering index", opt.ErrInvalid)
	}
	return paths, nil
}

func prepareCoveringSQL(t *testing.T, fixture *sessionExecutionFixture, sql string) *session.Prepared {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	view, err := fixture.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := fixture.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	options := fixture.planning(sql)
	access, err := session.NewCatalogAccess(snapshot, view, fixture.store.options.Encoder, session.CatalogAccessOptions{
		MaxPaths: 8, MaxBytes: 4 << 20, CapabilityEpoch: 1,
		IndexLimits:      schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 16, MaxColumns: 16},
		IndexExpressions: session.IndexExpressionOptions{Binding: options.Bind.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	options.Optimize.Access = coveringOnlyAccess{access}
	prepared, err := session.Prepare(t.Context(), parsed.Stmts[0], snapshot, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func readCoveringSQL(t *testing.T, fixture *sessionExecutionFixture, prepared *session.Prepared) [][2]int64 {
	t.Helper()
	invocation := fixture.invocation(t)
	invocation.Dependencies = nil
	var result [][2]int64
	invocation.Environment.Sort = exsort.Options{MaxRecordBytes: 4096, RunBytes: 64 << 10, WorkBytes: 8 << 20, MaxRuns: 4, MergeFanIn: 2}
	_, err := fixture.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
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
				var pair [2]int64
				for column := range pair {
					value, _, err := columnar.CopyValue(ctx, &batch.Columns[column], batch.Lane(row), 4096)
					if err != nil {
						return err
					}
					pair[column] = value.IntVal
				}
				result = append(result, pair)
			}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	usage := invocation.Environment.Query.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("covering scan retained resources: %+v", usage)
	}
	return result
}

func TestCatalogCoveringIndexPreservesDuplicateKeysAndLogicalColumns(t *testing.T) {
	fixture := coveringSessionFixture(t)
	if err := runUniqueSQL(t, fixture, fixture.session, `INSERT INTO items VALUES(101,10),(202,10),(303,20),(404,30)`); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		sql  string
		want [][2]int64
	}{
		{`SELECT id,value FROM items WHERE value=10 ORDER BY id`, [][2]int64{{101, 10}, {202, 10}}},
		{`SELECT id,value FROM items WHERE value=10 AND id>150 ORDER BY id`, [][2]int64{{202, 10}}},
	} {
		prepared := prepareCoveringSQL(t, fixture, scenario.sql)
		if got := readCoveringSQL(t, fixture, prepared); !slices.Equal(got, scenario.want) {
			t.Fatalf("%s: got %v, want %v", scenario.sql, got, scenario.want)
		}
	}
}

func TestCoveringIndexEmptyRangeProtectsPhysicalIndexKeys(t *testing.T) {
	for _, move := range []bool{false, true} {
		name := "insert"
		if move {
			name = "key_move"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := coveringSessionFixture(t)
				if move {
					if err := runUniqueSQL(t, fixture, fixture.session, `INSERT INTO items VALUES(999,20)`); err != nil {
						t.Fatal(err)
					}
				}
				prepared := prepareCoveringSQL(t, fixture, `SELECT id,value FROM items WHERE value=15`)
				if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
					t.Fatal(err)
				}
				if got := readCoveringSQL(t, fixture, prepared); len(got) != 0 {
					t.Fatalf("empty secondary range returned %v", got)
				}
				writer := fixture.peer(t)
				sql := `INSERT INTO items VALUES(999,15)`
				if move {
					sql = `UPDATE items SET value=15 WHERE id=999`
				}
				writePlan := fixture.prepare(t, sql, nil, func(options *session.PrepareOptions) {
					options.Optimize.Access = keyedMutationAccess{options.Optimize.Access.(mutationAccess)}
				})
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
					t.Fatalf("writer crossed empty secondary range: %v", err)
				default:
				}
				outside := fixture.peer(t)
				if err := runUniqueSQL(t, fixture, outside, `INSERT INTO items VALUES(888,16)`); err != nil {
					t.Fatal(err)
				}
				if err := fixture.session.Rollback(t.Context(), false); err != nil {
					t.Fatal(err)
				}
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if got := readCoveringSQL(t, fixture, prepared); !slices.Equal(got, [][2]int64{{999, 15}}) {
					t.Fatalf("secondary range after release: %v", got)
				}
			})
		})
	}
}

func TestPreparedCoveringIndexRejectsChangedIndexDescriptor(t *testing.T) {
	fixture := coveringSessionFixture(t)
	if err := runUniqueSQL(t, fixture, fixture.session, `INSERT INTO items VALUES(101,10)`); err != nil {
		t.Fatal(err)
	}
	prepared := prepareCoveringSQL(t, fixture, `SELECT id,value FROM items WHERE value=10`)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
	spec, err := fixture.descriptors[2].SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	spec.Version++
	next, err := model.NewDescriptor(t.Context(), spec, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	err = fixture.session.Statement(t.Context(), func(ctx context.Context, transaction *txn.Transaction) error {
		view, err := transaction.ReadView(ctx, fixture.dependencies)
		if err != nil {
			return err
		}
		snapshot, err := fixture.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		change, err := fixture.catalog.Update(ctx, snapshot, 1, next)
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			return err
		}
		return transaction.ApplyMutations(ctx, change.Mutations)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	invocation := fixture.invocation(t)
	invocation.Dependencies = nil
	called := false
	_, err = fixture.session.RunPrepared(t.Context(), prepared, invocation, func(context.Context, *session.Rows) error {
		called = true
		return nil
	})
	requireUniqueSQLState(t, err, "0A000")
	if called {
		t.Fatal("consumer received a plan with a changed index descriptor")
	}
}

type readerWithoutTimestamp struct{ storage.SnapshotReader }

func TestCatalogAccessRejectsDifferentReaderSnapshot(t *testing.T) {
	fixture := coveringSessionFixture(t)
	oldView, err := fixture.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := oldView.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := fixture.catalog.Snapshot(oldView, oldView.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	if err := runUniqueSQL(t, fixture, fixture.session, `INSERT INTO items VALUES(101,10)`); err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := current.Close(); err != nil {
			t.Error(err)
		}
	}()
	// This test binds no statement text, so it supplies the source bound
	// explicitly like the other out-of-band index expression sites in this
	// package; ir.Limits never treats zero as unlimited.
	binding := fixture.planning("").Bind.Limits
	binding.SourceBytes = 1 << 20
	access, err := session.NewCatalogAccess(snapshot, readerWithoutTimestamp{current}, fixture.store.options.Encoder, session.CatalogAccessOptions{
		MaxPaths: 8, MaxBytes: 4 << 20, CapabilityEpoch: 1,
		IndexLimits:      schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 16, MaxColumns: 16},
		IndexExpressions: session.IndexExpressionOptions{Binding: binding, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	row := fixture.store.request(t, 101)
	_, err = access.Negotiate(t.Context(), storage.PushdownRequest{
		Target: row.Target, Layout: row.Layout, ColumnIDs: row.ColumnIDs, KeyLayout: fixture.store.options.KeyLayout,
		Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
	}, 1<<20)
	if !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("mixed catalog/data snapshot negotiation = %v", err)
	}
}
