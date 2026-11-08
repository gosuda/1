package modify_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func writableInvocation(t *testing.T, f *sessionExecutionFixture) session.PreparedExecution {
	t.Helper()
	invocation := f.invocation(t)
	invocation.Dependencies = nil
	invocation.Environment.Spool = rowstore.Limits{MemoryBytes: 64 << 10, MaxBytes: 16 << 20, MaxRows: 10000, MaxRecordBytes: 4096, MetadataBytes: 4096, DecodeBytes: 4096, MaxReaders: 4}
	invocation.Environment.Join = exjoin.Options{MaxRecordBytes: 4096, MemoryBytes: 64 << 10, HashBytes: 128 << 10, WorkBytes: 16 << 20, PartitionBytes: 16 << 20, MaxPartitions: 2, MaxDepth: 2}
	return invocation
}
func collectWritableRows(ctx context.Context, rows *session.Rows) ([][]int64, error) {
	var result [][]int64
	var batch columnar.Batch
	for {
		err := rows.Next(ctx, &batch)
		if err == io.EOF {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		for row := range batch.Rows() {
			values := make([]int64, len(batch.Columns))
			for i := range values {
				v, _, err := columnar.CopyValue(ctx, &batch.Columns[i], batch.Lane(row), 4096)
				if err != nil {
					return nil, err
				}
				values[i] = v.IntVal
			}
			result = append(result, values)
		}
	}
}
func TestWritableCTEEffectsAreIndependentOfConsumerDemand(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		pull      bool
		want      [][]int64
	}{
		{"unused without returning", `WITH w AS (INSERT INTO items VALUES(1,10)) SELECT 7`, true, [][]int64{{7}}},
		{"shared returning", `WITH w AS (INSERT INTO items VALUES(1,10) RETURNING value) SELECT a.value,b.value FROM w a CROSS JOIN w b`, true, [][]int64{{10, 10}}},
		{"limit zero", `WITH w AS (INSERT INTO items VALUES(1,10) RETURNING value) SELECT value FROM w LIMIT 0`, true, nil},
		{"zero pulls", `WITH w AS (INSERT INTO items VALUES(1,10) RETURNING value) SELECT value FROM w`, false, nil},
		{"zero pulls skip result", `WITH w AS (INSERT INTO items VALUES(1,10)) SELECT 1/n FROM generate_series(0,0) AS t(n)`, false, nil},
		{"recursive keyword without recursion", `WITH RECURSIVE w AS (INSERT INTO items VALUES(1,10) RETURNING value) SELECT value FROM w`, true, [][]int64{{10}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newSessionExecutionFixture(t)
			prepared := f.prepare(t, scenario.sql, nil)
			invocation := writableInvocation(t, f)
			var got [][]int64
			_, err := f.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
				if !scenario.pull {
					return nil
				}
				var err error
				got, err = collectWritableRows(ctx, rows)
				return err
			})
			if err != nil || !reflect.DeepEqual(got, scenario.want) {
				t.Fatalf("rows=%v want=%v error=%v", got, scenario.want, err)
			}
			if value, err := f.committedValue(t, 1); err != nil || value != 10 {
				t.Fatalf("mandatory insert=%d,%v", value, err)
			}
			usage := invocation.Environment.Query.Usage()
			if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Readers != 0 || usage.Files != 0 {
				t.Fatalf("retained CTE resources: %+v", usage)
			}
		})
	}
}
func TestWritableCTECommunicatesOnlyThroughReturning(t *testing.T) {
	f := newSessionExecutionFixture(t)
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10)`); err != nil {
		t.Fatal(err)
	}
	prepared := f.prepare(t, `WITH w AS (UPDATE items SET value=20 WHERE id=1 RETURNING value) SELECT items.value,w.value FROM items CROSS JOIN w`, nil)
	var got [][]int64
	_, err := f.session.RunPrepared(t.Context(), prepared, writableInvocation(t, f), func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	if err != nil || !reflect.DeepEqual(got, [][]int64{{10, 20}}) {
		t.Fatalf("frozen source and returning=%v,%v", got, err)
	}
	if value, err := f.committedValue(t, 1); err != nil || value != 20 {
		t.Fatalf("committed update=%d,%v", value, err)
	}
}
func TestWritableCTEFailureRollsBackEveryWriter(t *testing.T) {
	f := newSessionExecutionFixture(t)
	prepared := f.prepare(t, `WITH a AS (INSERT INTO items VALUES(1,10) RETURNING id), b AS (INSERT INTO items SELECT id,20 FROM a RETURNING id) SELECT id FROM b`, nil)
	_, err := f.session.RunPrepared(t.Context(), prepared, writableInvocation(t, f), func(ctx context.Context, rows *session.Rows) error {
		_, err := collectWritableRows(ctx, rows)
		return err
	})
	requireUniqueSQLState(t, err, "23505")
	if _, err := f.committedValue(t, 1); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("earlier CTE survived statement failure: %v", err)
	}
}
func TestWritableCTEConsumerFailureRollsBackEffects(t *testing.T) {
	f := newSessionExecutionFixture(t)
	prepared := f.prepare(t, `WITH w AS (INSERT INTO items VALUES(1,10) RETURNING value) SELECT value FROM w`, nil)
	_, err := f.session.RunPrepared(t.Context(), prepared, writableInvocation(t, f), func(ctx context.Context, rows *session.Rows) error {
		var batch columnar.Batch
		if err := rows.Next(ctx, &batch); err != nil {
			return err
		}
		return errSessionConsumer
	})
	if !errors.Is(err, errSessionConsumer) {
		t.Fatalf("consumer failure=%v", err)
	}
	if _, err := f.committedValue(t, 1); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("CTE survived consumer failure: %v", err)
	}
}
func TestWritableCTECountsOnlyRootCommand(t *testing.T) {
	f := newSessionExecutionFixture(t)
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10)`); err != nil {
		t.Fatal(err)
	}
	prepared := f.prepare(t, `WITH w AS (INSERT INTO items VALUES(2,20),(3,30) RETURNING id) UPDATE items SET value=value+1 RETURNING value`, nil)
	var got [][]int64
	result, err := f.session.RunPrepared(t.Context(), prepared, writableInvocation(t, f), func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	if err != nil || result.RowsAffected != 1 || !reflect.DeepEqual(got, [][]int64{{11}}) {
		t.Fatalf("root mutation=%+v rows=%v error=%v", result, got, err)
	}
	for id, want := range map[int64]int64{1: 11, 2: 20, 3: 30} {
		if value, err := f.committedValue(t, id); err != nil || value != want {
			t.Fatalf("row %d=%d,%v want=%d", id, value, err, want)
		}
	}
}

type writableTargets []execution.MutationTarget

func (targets writableTargets) ResolveMutationTarget(ctx context.Context, ref storage.ObjectRef) (execution.MutationTarget, error) {
	for _, target := range targets {
		if target.Descriptor.ID() == ref.ObjectID {
			return target.ResolveMutationTarget(ctx, ref)
		}
	}
	return execution.MutationTarget{}, storage.ErrInvalidTarget
}

func prepareWritableCatalogSQL(t *testing.T, f *sessionExecutionFixture, sql string) *session.Prepared {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := f.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := snapshot.Close(); err != nil {
			t.Error(err)
		}
	}()
	options := f.planning(sql)
	access, err := session.NewCatalogAccess(snapshot, view, f.store.options.Encoder, session.CatalogAccessOptions{MaxPaths: 8, MaxBytes: 4 << 20, CapabilityEpoch: 1, IndexLimits: schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 16, MaxColumns: 16}, IndexExpressions: session.IndexExpressionOptions{Binding: options.Bind.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	options.Optimize.Access = access
	prepared, err := session.Prepare(t.Context(), parsed.Stmts[0], snapshot, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestWritableCTEMovesRowsAcrossTargetsAtomically(t *testing.T) {
	f := newSessionExecutionFixture(t)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
	spec, err := f.store.options.Descriptor.SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	spec.ID = 5
	spec.OID = 10005
	spec.Name.Name = "archive"
	archive, err := model.NewDescriptor(t.Context(), spec, limits)
	if err != nil {
		t.Fatal(err)
	}
	target := storage.ObjectRef{ObjectID: 5, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	policy, err := txn.NewRowNormalizer(t.Context(), target, []txn.ColumnRule{{Layout: f.store.options.Layout[0], KeyPosition: 1}, {Layout: f.store.options.Layout[1]}}, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	f.transactionOptions.Resolver.(sessionPolicies)[target] = policy
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	err = f.session.Statement(t.Context(), func(ctx context.Context, transaction *txn.Transaction) error {
		dependencies, err := f.catalog.CreateDependencies(ctx, archive)
		if err != nil {
			return err
		}
		view, err := transaction.ReadView(ctx, dependencies)
		if err != nil {
			return err
		}
		snapshot, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		change, err := f.catalog.Create(ctx, snapshot, archive)
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			return err
		}
		return transaction.ApplyMutations(ctx, change.Mutations)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if err := runUniqueSQL(t, f, f.session, `INSERT INTO items VALUES(1,10)`); err != nil {
		t.Fatal(err)
	}
	prepared := prepareWritableCatalogSQL(t, f, `WITH moved AS (DELETE FROM items WHERE id=1 RETURNING id,value), added AS (INSERT INTO archive SELECT id,value FROM moved RETURNING value) SELECT value FROM added`)
	invocation := writableInvocation(t, f)
	invocation.Environment.MutationTargets = writableTargets{{Descriptor: f.store.options.Descriptor}, {Descriptor: archive}}
	var got [][]int64
	_, err = f.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	if err != nil || !reflect.DeepEqual(got, [][]int64{{10}}) {
		t.Fatalf("move returning=%v,%v", got, err)
	}
	if _, err := f.committedValue(t, 1); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("move source remains: %v", err)
	}
	view, err := f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	}()
	request := f.store.request(t, 1)
	request.Target = target
	row, err := view.Get(t.Context(), request)
	if err != nil || len(row.Values) != 2 || row.Values[1].IntVal != 10 {
		t.Fatalf("move destination=%v,%v", row, err)
	}
}

func TestWritableCTEEarlyConsumerDoesNotDrainSelect(t *testing.T) {
	f := newSessionExecutionFixture(t)
	prepared := f.prepare(t, `WITH w AS (INSERT INTO items VALUES(1,10)) SELECT n FROM generate_series(1,2147483647) AS t(n)`, nil)
	_, err := f.session.RunPrepared(t.Context(), prepared, writableInvocation(t, f), func(ctx context.Context, rows *session.Rows) error {
		var batch columnar.Batch
		return rows.Next(ctx, &batch)
	})
	if err != nil {
		t.Fatal(err)
	}
	if value, err := f.committedValue(t, 1); err != nil || value != 10 {
		t.Fatalf("mandatory write after early SELECT=%d,%v", value, err)
	}
}
