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
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type reservedDeclarationIDs struct{ next model.ObjectID }

func (r *reservedDeclarationIDs) AllocateDeclarationIdentity(ctx context.Context) (model.ObjectID, model.OID, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	r.next++
	return r.next, model.OID(10000 + r.next), nil
}

func TestDeclaredTableWritesFollowCreatingTransaction(t *testing.T) {
	testDeclaredTableLifecycle(t, `CREATE TABLE pending (value bigint DEFAULT 7, doubled bigint GENERATED ALWAYS AS(value*2) STORED, CHECK(doubled>0))`, `INSERT INTO pending DEFAULT VALUES RETURNING *`, [][]int64{{7, 14}})
}

func TestZeroColumnTableUsesInvisiblePhysicalIdentity(t *testing.T) {
	testDeclaredTableLifecycle(t, `CREATE TABLE pending ()`, `INSERT INTO pending DEFAULT VALUES RETURNING 1`, [][]int64{{1}})
}

func TestDeclaredIndexesSharePendingTableTransaction(t *testing.T) {
	testDeclaredTableLifecycle(t, `CREATE TABLE pending (id bigint PRIMARY KEY, value bigint UNIQUE)`, `INSERT INTO pending VALUES(1,2) RETURNING *`, [][]int64{{1, 2}})
}

func testDeclaredTableLifecycle(t *testing.T, create, sql string, want [][]int64) {
	t.Helper()
	f := newSessionExecutionFixture(t)
	var dependencies []txn.ReadDependency
	for _, layout := range f.catalog.Layouts() {
		dependencies = append(dependencies, txn.ReadDependency{Target: layout.Target, KeyLayout: []storage.KeyColumn{{ColumnID: layout.Columns[0].ID, Type: layout.Columns[0].Type}}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}})
	}
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	parsed, err := zpgparse.Parse(create)
	if err != nil {
		t.Fatal(err)
	}
	var declared *session.DeclarationResult
	err = f.session.Statement(t.Context(), func(ctx context.Context, transaction *txn.Transaction) error {
		view, err := transaction.ReadView(ctx, dependencies)
		if err != nil {
			return err
		}
		snapshot, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
		if err != nil {
			return errors.Join(err, view.Close())
		}
		options := f.planning("SELECT 7").Bind
		options.Builtin = f.settings.BuiltinOptions(950, 950)
		options.Limits.MaxBytes = 1 << 20
		bound, err := bind.BindCreateTable(ctx, parsed.Stmts[0].Stmt.GetCreateStmt(), snapshot, bind.CreateDeclarationOptions{Bind: options, Descriptors: model.DescriptorLimits{MaxBytes: 64 << 10, MaxItems: 64, MaxDepth: 64}, Identities: &reservedDeclarationIDs{next: 20}, Owner: (model.Principal{}).OID(), Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}})
		err = errors.Join(err, snapshot.Close(), view.Close())
		if err != nil {
			return err
		}
		evaluation := expr.DefaultLimits()
		evaluation.MaxBytes = 32 << 10
		declared, err = session.StageDeclaration(ctx, transaction, bound, session.DeclarationOptions{Catalog: f.catalog, Encoder: f.store.options.Encoder, Columns: session.ColumnExpressionOptions{Binding: options.Limits, Evaluation: evaluation, InputBytes: 64 << 10, ResultBytes: 64 << 10, Builtin: options.Builtin}, Indexes: session.IndexExpressionOptions{Binding: options.Limits, Evaluation: evaluation, InputBytes: 64 << 10, ResultBytes: 64 << 10}, Limits: schema.IndexLimits{MetadataBytes: 64 << 10, EntryBytes: 64 << 10, MaxColumns: 64}, PolicyBytes: 512 << 10})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	view, err := f.session.ReadView(t.Context(), dependencies)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	options := f.planning(sql)
	access, err := session.NewCatalogAccess(snapshot, view, f.store.options.Encoder, session.CatalogAccessOptions{MaxPaths: 8, MaxBytes: 4 << 20, CapabilityEpoch: 1, IndexLimits: schema.IndexLimits{MetadataBytes: 64 << 10, EntryBytes: 64 << 10, MaxColumns: 64}, IndexExpressions: session.IndexExpressionOptions{Binding: options.Bind.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	options.Optimize.Access = access
	prepared, err := session.Prepare(t.Context(), parsed.Stmts[0], snapshot, options, nil)
	err = errors.Join(err, snapshot.Close(), view.Close())
	if err != nil {
		t.Fatal(err)
	}
	invocation := mergeInvocation(t, f)
	invocation.Environment.MutationTargets = declared
	var got [][]int64
	_, err = f.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("new table insert = %v, %v", got, err)
	}
	if err := f.session.Rollback(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	current, err := f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	committed, err := f.catalog.Snapshot(current, current.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	_, lookupErr := committed.Resolve(t.Context(), model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: "pending"})
	if lookupErr == nil {
		t.Fatal("rolled-back table remained visible")
	}
	if err := errors.Join(committed.Close(), current.Close()); err != nil {
		t.Fatal(err)
	}
}
