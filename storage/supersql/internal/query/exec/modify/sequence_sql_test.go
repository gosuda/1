package modify_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/session"
)

func sequenceSQLFixture(t *testing.T) (*sessionExecutionFixture, model.SequenceRuntime) {
	t.Helper()
	f := newSessionExecutionFixture(t)
	store, err := schema.NewSequenceStore(t.Context(), schema.SequenceStoreOptions{Target: storage.ObjectRef{ObjectID: 90, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Encoder: f.store.options.Encoder, MetadataBytes: 1 << 20, RecordBytes: 4096, KeyBytes: 1024, MaxIncarnations: 8, MaxCache: 8})
	if err != nil {
		t.Fatal(err)
	}
	options := f.transactionOptions
	options.Resolver = store
	bootstrap, err := f.store.manager.Begin(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	err = bootstrap.Statement(t.Context(), func(ctx context.Context) error { return store.InitializeAllocator(ctx, bootstrap) })
	if err == nil {
		_, err = bootstrap.Commit(t.Context())
	}
	if err = errors.Join(err, bootstrap.Close(t.Context())); err != nil {
		t.Fatal(err)
	}
	service, err := schema.NewSequenceService(t.Context(), schema.SequenceServiceOptions{Manager: f.store.manager, Store: store, Transactions: options, MaxOperations: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	state, err := service.Initialize(t.Context(), schema.SequenceConfig{Start: 1, Increment: 1, Min: 1, Max: 100, Cache: 3})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{ID: 8, OID: 10008, Name: model.NamespaceName{Parent: 2, Kind: model.ObjectSequence, Name: "numbers"}, Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Sequence: &model.SequenceMetadata{Identity: uint64(state.Incarnation), Incarnation: uint64(state.Incarnation), Type: integer.Type, Start: 1, Increment: 1, Min: 1, Max: 100, Cache: 3}}, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 64, MaxDepth: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	dependencies, err := f.catalog.CreateDependencies(t.Context(), descriptor)
	if err != nil {
		t.Fatal(err)
	}
	err = f.session.Statement(t.Context(), func(ctx context.Context, tx *txn.Transaction) error {
		view, err := tx.ReadView(ctx, dependencies)
		if err != nil {
			return err
		}
		snapshot, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		change, err := f.catalog.Create(ctx, snapshot, descriptor)
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			return err
		}
		return tx.ApplyMutations(ctx, change.Mutations)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	f.dependencies = append(f.dependencies, dependencies...)
	sequence, err := session.NewSequenceSession(session.SequenceSessionOptions{Allocations: service, MaxSequences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sequence.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	view, err := f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
			t.Error(err)
		}
	})
	runtime, err := session.NewSequenceRuntime(snapshot, sequence)
	if err != nil {
		t.Fatal(err)
	}
	relations, err := session.NewRelationRuntime(snapshot, nil, []string{"public"}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return f, struct {
		model.SequenceRuntime
		model.RelationRuntime
	}{runtime, relations}
}

func runSequenceSQL(t *testing.T, f *sessionExecutionFixture, runtime model.SequenceRuntime, sql string) ([][]int64, error) {
	t.Helper()
	prepared := prepareWritableCatalogSQL(t, f, sql)
	invocation := mergeInvocation(t, f)
	invocation.Dependencies = f.dependencies
	invocation.Environment.Sequences = runtime
	invocation.Environment.Relations = runtime.(model.RelationRuntime)
	var got [][]int64
	_, err := f.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	return got, err
}

func TestSQLSequenceEffectsRespectMasksAndSurviveRollback(t *testing.T) {
	f, runtime := sequenceSQLFixture(t)
	got, err := runSequenceSQL(t, f, runtime, `SELECT CASE WHEN false THEN nextval(10008::regclass) ELSE 7 END`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{7}}) {
		t.Fatalf("masked sequence=%v,%v", got, err)
	}
	_, err = runSequenceSQL(t, f, runtime, `SELECT currval(10008::regclass)`)
	requireUniqueSQLState(t, err, "55000")
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	got, err = runSequenceSQL(t, f, runtime, `SELECT nextval(10008::regclass) FROM generate_series(1,3)`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{1}, {2}, {3}}) {
		t.Fatalf("sequence reservation=%v,%v", got, err)
	}
	if err := f.session.Rollback(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		sql  string
		want [][]int64
	}{
		{`SELECT currval(10008::regclass),lastval()`, [][]int64{{3, 3}}},
		{`SELECT setval(10008::regclass,10,false),currval(10008::regclass),lastval()`, [][]int64{{10, 3, 3}}},
		{`SELECT nextval(10008::regclass),currval(10008::regclass),lastval()`, [][]int64{{10, 10, 10}}},
		{`SELECT nextval('numbers'::text),currval('numbers')`, [][]int64{{11, 11}}},
		{`SELECT nextval('public.numbers'),currval('"numbers"'::regclass)`, [][]int64{{12, 12}}},
		{`SELECT ('numbers'::regclass)::text='numbers', ('-'::regclass)::text='-', ('4294967295'::regclass)::text='4294967295'`, [][]int64{{1, 1, 1}}},
		{`SELECT to_regclass('missing') IS NULL, to_regclass('4294967296') IS NULL, to_regclass('public.numbers')::text='numbers'`, [][]int64{{1, 1, 1}}},
	} {
		got, err = runSequenceSQL(t, f, runtime, step.sql)
		if err != nil || !reflect.DeepEqual(got, step.want) {
			t.Fatalf("%s: %v,%v want %v", step.sql, got, err, step.want)
		}
	}
}

func TestReadOnlySequenceMutationFailsBeforeAllocation(t *testing.T) {
	f, runtime := sequenceSQLFixture(t)
	readonly := true
	if err := f.session.Begin(t.Context(), session.TransactionStart{Characteristics: session.BeginOptions{ReadOnly: &readonly}}); err != nil {
		t.Fatal(err)
	}
	_, err := runSequenceSQL(t, f, runtime, `SELECT nextval(10008::regclass)`)
	requireUniqueSQLState(t, err, "25006")
	if err := f.session.Rollback(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	got, err := runSequenceSQL(t, f, runtime, `SELECT nextval(10008::regclass)`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{1}}) {
		t.Fatalf("readonly consumed sequence: %v,%v", got, err)
	}
}
