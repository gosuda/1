package modify_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestNestedPrimaryAndUniqueKeysPreserveArrayDimensions(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE nested_keys(key integer[] PRIMARY KEY, unique_key integer[] UNIQUE, label bigint)`)
	if err != nil {
		t.Fatal(err)
	}
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO nested_keys VALUES(ARRAY[1,2],ARRAY[9],1),('[0:1]={1,2}'::integer[],ARRAY[8],2),(ARRAY[]::integer[],ARRAY[7],3) RETURNING label`, [][]int64{{1}, {2}, {3}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT label FROM nested_keys ORDER BY key`, [][]int64{{3}, {2}, {1}})
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `INSERT INTO nested_keys VALUES(ARRAY[1,2],ARRAY[6],4)`, table.Mutations)
	requireUniqueSQLState(t, err, "23505")
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `INSERT INTO nested_keys VALUES(ARRAY[4],ARRAY[9],4)`, table.Mutations)
	requireUniqueSQLState(t, err, "23505")
	requireIdentityRows(t, fixture, sequences, dependencies, table, `UPDATE nested_keys SET key=ARRAY[5] WHERE label=1 RETURNING label`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT label FROM nested_keys WHERE key=ARRAY[5]`, [][]int64{{1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT count(*) FROM nested_keys`, [][]int64{{3}})
}

func TestOIDVectorPrimaryKeyPreservesEmptyShapeAndLengthOrdering(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE vector_keys(key oidvector PRIMARY KEY, label bigint)`)
	if err != nil {
		t.Fatal(err)
	}
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO vector_keys VALUES('1 2',1),('9',2),('',3) RETURNING label`, [][]int64{{1}, {2}, {3}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT label FROM vector_keys ORDER BY key`, [][]int64{{3}, {2}, {1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT array_ndims(key::oid[]),array_lower(key::oid[],1) FROM vector_keys WHERE label=3`, [][]int64{{1, 0}})
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `INSERT INTO vector_keys VALUES('7',4),('',5)`, table.Mutations)
	requireUniqueSQLState(t, err, "23505")
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT count(*) FROM vector_keys`, [][]int64{{3}})
}

func TestGeneratedArrayPrimaryKeyUsesDeclaredElementLayout(t *testing.T) {
	fixture, _, _, sequences, dependencies := sequenceDDLFixture(t)
	table, err := createIdentityTable(t, fixture, dependencies, `CREATE TABLE generated_keys(id bigint, key bigint[] GENERATED ALWAYS AS (ARRAY[id,id+1]) STORED PRIMARY KEY)`)
	if err != nil {
		t.Fatal(err)
	}
	requireIdentityRows(t, fixture, sequences, dependencies, table, `INSERT INTO generated_keys(id) VALUES(2),(1) RETURNING id`, [][]int64{{2}, {1}})
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT id FROM generated_keys ORDER BY key`, [][]int64{{1}, {2}})
	_, err = runDeclarationQuery(t, fixture, sequences, dependencies, `UPDATE generated_keys SET id=1 WHERE id=2`, table.Mutations)
	requireUniqueSQLState(t, err, "23505")
	requireIdentityRows(t, fixture, sequences, dependencies, table, `SELECT id FROM generated_keys ORDER BY key`, [][]int64{{1}, {2}})
}

func TestExpressionIndexOwnsPersistedNestedResultDeclaration(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
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
	const sql = `CREATE UNIQUE INDEX nested_expression ON items ((ARRAY[value,value+1]))`
	planning := fixture.planning(sql)
	compiler, err := session.NewIndexExpressionCompiler(snapshot, session.IndexExpressionOptions{Binding: planning.Bind.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64}
	bound, err := bind.BindCreateIndex(t.Context(), parsed.Stmts[0].Stmt.GetIndexStmt(), snapshot, bind.CreateIndexOptions{Bind: planning.Bind, Descriptors: limits, ObjectID: 3, OID: 10003, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Expressions: compiler})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := bound.Index.SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	spec.State = model.StatePublic
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var restored model.DescriptorSpec
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	index, err := model.NewDescriptor(t.Context(), restored, limits)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := schema.NewIndexPlan(t.Context(), bound.Table, index, fixture.store.options.Encoder, compiler, schema.IndexLimits{MetadataBytes: 4 << 20, EntryBytes: 1 << 20, MaxColumns: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(snapshot.Close(), view.Close()); err != nil {
		t.Fatal(err)
	}
	row := model.Row{Key: fixture.store.request(t, 7).Key, Values: []model.Value{model.Int64Val(7), model.Int64Val(11)}}
	entry, err := plan.Entry(t.Context(), &row)
	if err != nil {
		t.Fatal(err)
	}
	decoder, ok := fixture.store.options.Encoder.(storage.KeyDecoder)
	if !ok {
		t.Fatal("native encoder has no canonical decoder")
	}
	key, err := decoder.DecodeKey(t.Context(), plan.KeyLayout(), entry.Key, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 2 || len(key[0].ArrayVal) != 2 || key[0].ArrayVal[0].IntVal != 11 || key[0].ArrayVal[1].IntVal != 12 {
		t.Fatalf("expression index key = %+v", key)
	}
	row.Values[1] = model.Int64Val(12)
	next, err := plan.Entry(t.Context(), &row)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Compare(entry.Key, next.Key) >= 0 {
		t.Fatalf("nested expression order: %x >= %x", entry.Key, next.Key)
	}
}
