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
	execution "github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func columnExpressionAST(t *testing.T, sql string) *zpgparse.Node {
	t.Helper()
	parsed, err := zpgparse.Parse("SELECT " + sql)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
}
func columnExpressionFixture(t *testing.T, generatedKey bool) (*sessionExecutionFixture, *model.Descriptor) {
	return checkedColumnExpressionFixture(t, generatedKey, "", false)
}

func checkedColumnExpressionFixture(t *testing.T, generatedKey bool, checkSQL string, enforced bool) (*sessionExecutionFixture, *model.Descriptor) {
	return physicalColumnExpressionFixture(t, generatedKey, checkSQL, enforced, false)
}

func physicalColumnExpressionFixture(t *testing.T, generatedKey bool, checkSQL string, enforced, keyless bool) (*sessionExecutionFixture, *model.Descriptor) {
	t.Helper()
	f := newSessionExecutionFixture(t)
	return populateColumnExpressionFixture(t, f, generatedKey, checkSQL, enforced, keyless, "3+4")
}

func populateColumnExpressionFixture(t *testing.T, f *sessionExecutionFixture, generatedKey bool, checkSQL string, enforced, keyless bool, defaultSQL string) (*sessionExecutionFixture, *model.Descriptor) {
	t.Helper()
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
	view, err := f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	options := f.planning("SELECT 7").Bind
	options.Builtin = f.settings.BuiltinOptions(950, 950)
	frozen, err := bind.BindRoutineDefault(t.Context(), columnExpressionAST(t, defaultSQL), integer, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	spec := model.DescriptorSpec{ID: 5, OID: 10005, Name: model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: "computed"}, Version: 1, State: model.StatePublic, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Table: &model.TableMetadata{ColumnIDHighWatermark: 3, Columns: []model.ColumnDescriptor{{ID: 1, Name: "id", State: model.StatePublic, Layout: integer}, {ID: 2, Name: "value", State: model.StatePublic, Layout: integer, Default: &frozen.Definition}, {ID: 3, Name: "doubled", State: model.StatePublic, Layout: integer}}, PrimaryKey: []model.KeyElement{{Column: 1, Type: integer.Type}}}}
	spec.Table.Columns[1].DefaultOID = 10006
	generatedColumn := model.ColumnID(3)
	generatedSQL := "value*2"
	if generatedKey {
		spec.Table.Columns = spec.Table.Columns[:2]
		spec.Table.ColumnIDHighWatermark = 2
		generatedColumn = 1
		generatedSQL = "value+100"
	}
	if keyless {
		layout := model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
		spec.Table.Columns = append(spec.Table.Columns, model.ColumnDescriptor{ID: 4, Name: "__physical_row", Layout: layout, State: model.StatePublic, SystemIdentity: true})
		spec.Table.ColumnIDHighWatermark = 4
		spec.Table.PrimaryKey = []model.KeyElement{{Column: 4, Type: layout.Type}}
	}
	declaration, err := model.NewDescriptor(t.Context(), spec, limits)
	if err != nil {
		t.Fatal(err)
	}
	definition, err := bind.DefineStoredGenerated(t.Context(), columnExpressionAST(t, generatedSQL), declaration, generatedColumn, integer, snapshot, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := range spec.Table.Columns {
		if spec.Table.Columns[i].ID == generatedColumn {
			spec.Table.Columns[i].Generated = definition
			spec.Table.Columns[i].GeneratedStored = true
			spec.Table.Columns[i].DefaultOID = 10007
		}
	}
	descriptor, err := model.NewDescriptor(t.Context(), spec, limits)
	if err != nil {
		t.Fatal(err)
	}
	if checkSQL != "" {
		check, err := bind.DefineCheck(t.Context(), columnExpressionAST(t, checkSQL), descriptor, 6, snapshot, options)
		if err != nil {
			t.Fatal(err)
		}
		spec.Table.Constraints = []model.ConstraintDescriptor{{ID: 6, Name: "value_check", Kind: model.ConstraintCheck, Expression: check, Enforced: enforced}}
		descriptor, err = model.NewDescriptor(t.Context(), spec, limits)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
		t.Fatal(err)
	}
	if err := f.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	err = f.session.Statement(t.Context(), func(ctx context.Context, transaction *txn.Transaction) error {
		dependencies, err := f.catalog.CreateDependencies(ctx, descriptor)
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
		change, err := f.catalog.Create(ctx, snapshot, descriptor)
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
	view, err = f.store.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = f.catalog.Snapshot(view, view.Timestamp(), catalog.BorrowReader)
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := session.NewColumnExpressionCompiler(snapshot, session.ColumnExpressionOptions{Binding: options.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20, Builtin: options.Builtin})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := schema.NewTablePolicy(t.Context(), schema.TablePolicyOptions{Descriptor: descriptor, Encoder: f.store.options.Encoder, Expressions: compiler, MetadataBytes: 1 << 20, RowBytes: 1 << 20, MaxColumns: 64})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
		t.Fatal(err)
	}
	target := storage.ObjectRef{ObjectID: 5, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	f.transactionOptions.Resolver.(sessionPolicies)[target] = policy
	return f, descriptor
}
func runColumnSQL(t *testing.T, f *sessionExecutionFixture, descriptor *model.Descriptor, sql string) ([][]int64, error) {
	return runColumnRuntime(t, f, descriptor, sql, model.ExpressionRuntime{})
}

func runColumnRuntime(t *testing.T, f *sessionExecutionFixture, descriptor *model.Descriptor, sql string, expressions model.ExpressionRuntime) ([][]int64, error) {
	t.Helper()
	prepared := prepareWritableCatalogSQL(t, f, sql)
	invocation := mergeInvocation(t, f)
	invocation.Environment.MutationTargets = execution.MutationTarget{Descriptor: descriptor}
	invocation.Environment.Sequences = expressions.Sequences
	invocation.Environment.Relations = expressions.Relations
	invocation.Environment.Temporal = expressions.Temporal
	var got [][]int64
	_, err := f.session.RunPrepared(t.Context(), prepared, invocation, func(ctx context.Context, rows *session.Rows) error {
		var err error
		got, err = collectWritableRows(ctx, rows)
		return err
	})
	return got, err
}
func TestStoredColumnExpressionsCompleteDefaultsAndReturning(t *testing.T) {
	f, descriptor := columnExpressionFixture(t, false)
	for _, scenario := range []struct {
		sql  string
		want [][]int64
	}{
		{`INSERT INTO computed(id) VALUES(1) RETURNING id,value,doubled`, [][]int64{{1, 7, 14}}},
		{`UPDATE computed SET value=8 WHERE id=1 RETURNING id,value,doubled`, [][]int64{{1, 8, 16}}},
		{`UPDATE computed SET doubled=DEFAULT WHERE id=1 RETURNING id,value,doubled`, [][]int64{{1, 8, 16}}},
		{`UPDATE computed SET value=DEFAULT WHERE id=1 RETURNING id,value,doubled`, [][]int64{{1, 7, 14}}},
		{`INSERT INTO computed(id) VALUES(1) ON CONFLICT(id) DO UPDATE SET value=excluded.doubled RETURNING id,value,doubled`, [][]int64{{1, 14, 28}}},
		{`UPDATE computed SET id=2 WHERE id=1 RETURNING id,value,doubled`, [][]int64{{2, 14, 28}}},
	} {
		got, err := runColumnSQL(t, f, descriptor, scenario.sql)
		if err != nil || !reflect.DeepEqual(got, scenario.want) {
			t.Fatalf("%s: got=%v want=%v error=%v", scenario.sql, got, scenario.want, err)
		}
	}
}
func TestStoredGeneratedKeyIsEncodedAfterRowCompletion(t *testing.T) {
	f, descriptor := columnExpressionFixture(t, true)
	got, err := runColumnSQL(t, f, descriptor, `INSERT INTO computed(value) VALUES(9) RETURNING id,value`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{109, 9}}) {
		t.Fatalf("generated key=%v,%v", got, err)
	}
	got, err = runColumnSQL(t, f, descriptor, `MERGE INTO computed t USING (VALUES(8)) s(v) ON t.value=s.v WHEN NOT MATCHED THEN INSERT(id,value) VALUES(DEFAULT,s.v) RETURNING id,value`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{108, 8}}) {
		t.Fatalf("MERGE generated key=%v,%v", got, err)
	}
	got, err = runColumnSQL(t, f, descriptor, `UPDATE computed SET value=10 WHERE id=109 RETURNING id,value`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{110, 10}}) {
		t.Fatalf("updated generated key=%v,%v", got, err)
	}
	got, err = runColumnSQL(t, f, descriptor, `INSERT INTO computed(value) VALUES(10) ON CONFLICT(id) DO UPDATE SET value=excluded.value+1 RETURNING id,value`)
	if err != nil || !reflect.DeepEqual(got, [][]int64{{111, 11}}) {
		t.Fatalf("conflict generated key=%v,%v", got, err)
	}
}
