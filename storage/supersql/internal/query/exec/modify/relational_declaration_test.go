package modify_test

import (
	"context"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func relationalSmokePointDependencies(t *testing.T, fixture *sessionExecutionFixture, names []model.NamespaceName, descriptors ...*model.Descriptor) []txn.ReadDependency {
	t.Helper()
	dependencies := slices.Clone(fixture.dependencies)
	seenNames := make(map[model.NamespaceName]struct{}, len(names)+len(descriptors)+1)
	appendName := func(name model.NamespaceName) {
		if _, exists := seenNames[name]; exists {
			return
		}
		dependency, err := fixture.catalog.NamespaceDependency(t.Context(), name)
		if err != nil {
			t.Fatal(err)
		}
		seenNames[name] = struct{}{}
		dependencies = append(dependencies, dependency)
	}
	appendName(fixture.descriptors[0].Name())
	for _, name := range names {
		appendName(name)
	}
	seenDescriptors := make(map[model.ObjectID]struct{}, len(descriptors))
	appendDescriptor := func(id model.ObjectID) {
		if _, exists := seenDescriptors[id]; exists {
			return
		}
		seenDescriptors[id] = struct{}{}
		dependencies = append(dependencies, fixture.catalog.DescriptorDependency(id))
	}
	for _, descriptor := range descriptors {
		if descriptor == nil {
			t.Fatal("nil relational smoke descriptor")
		}
		appendName(descriptor.Name())
		appendDescriptor(descriptor.ID())
		if table, ok := descriptor.Table(); ok {
			for _, index := range table.Indexes {
				appendDescriptor(index)
			}
		}
	}
	return dependencies
}

func relationalSmokeName(fixture *sessionExecutionFixture, kind model.ObjectKind, name string) model.NamespaceName {
	return model.NamespaceName{Parent: fixture.descriptors[0].ID(), Kind: kind, Name: name}
}

func TestRelationalDeclarationProtectedForeignKeyDiscovery(t *testing.T) {
	fixture, _, _, sequences, _ := sequenceDDLFixture(t)
	parentName := relationalSmokeName(fixture, model.ObjectTable, "relational_parent")
	parentCreate := relationalSmokePointDependencies(t, fixture, []model.NamespaceName{
		parentName,
		relationalSmokeName(fixture, model.ObjectIndex, "relational_parent_pkey"),
	})
	parent, err := createIdentityTable(t, fixture, parentCreate, `CREATE TABLE relational_parent(id bigint PRIMARY KEY, value bigint)`)
	if err != nil {
		t.Fatal(err)
	}

	childName := relationalSmokeName(fixture, model.ObjectTable, "relational_child")
	childCreate := relationalSmokePointDependencies(t, fixture, []model.NamespaceName{
		childName,
		relationalSmokeName(fixture, model.ObjectIndex, "relational_child_pkey"),
	}, parent.Descriptor)
	child, err := createIdentityTable(t, fixture, childCreate, `CREATE TABLE relational_child(id bigint PRIMARY KEY, parent_id bigint REFERENCES relational_parent(id), value bigint)`)
	if err != nil {
		t.Fatal(err)
	}

	points := relationalSmokePointDependencies(t, fixture, nil, parent.Descriptor, child.Descriptor)
	requireIdentityRows(t, fixture, sequences, points, parent, `INSERT INTO relational_parent VALUES(1,10) RETURNING id,value`, [][]int64{{1, 10}})
	requireIdentityRows(t, fixture, sequences, points, child, `INSERT INTO relational_child VALUES(1,1,20) RETURNING id,parent_id,value`, [][]int64{{1, 1, 20}})
	_, err = runDeclarationQuery(t, fixture, sequences, points, `INSERT INTO relational_child VALUES(2,2,30)`, child.Mutations)
	requireUniqueSQLState(t, err, "23503")
	requireIdentityRows(t, fixture, sequences, points, child, `SELECT id,parent_id,value FROM relational_child ORDER BY id`, [][]int64{{1, 1, 20}})
	requireIdentityRows(t, fixture, sequences, points, parent, `SELECT id,value FROM relational_parent ORDER BY id`, [][]int64{{1, 10}})
}

func TestRelationalDeclarationLocalDeferredUniqueRollback(t *testing.T) {
	fixture, _, _, sequences, _ := sequenceDDLFixture(t)
	tableName := relationalSmokeName(fixture, model.ObjectTable, "relational_deferred")
	create := relationalSmokePointDependencies(t, fixture, []model.NamespaceName{
		tableName,
		relationalSmokeName(fixture, model.ObjectIndex, "relational_deferred_pkey"),
		relationalSmokeName(fixture, model.ObjectIndex, "relational_deferred_value_key"),
	})
	table, err := createIdentityTable(t, fixture, create, `CREATE TABLE relational_deferred(id bigint PRIMARY KEY, value bigint UNIQUE DEFERRABLE INITIALLY DEFERRED)`)
	if err != nil {
		t.Fatal(err)
	}
	points := relationalSmokePointDependencies(t, fixture, nil, table.Descriptor)
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeclarationQuery(t, fixture, sequences, points, `INSERT INTO relational_deferred VALUES(1,7),(2,7)`, table.Mutations); err != nil {
		t.Fatal(err)
	}
	_, err = relationalSmokeCommand(t, fixture, points, `SET CONSTRAINTS ALL IMMEDIATE`)
	requireUniqueSQLState(t, err, "23505")
	if err := fixture.session.Rollback(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	requireIdentityRows(t, fixture, sequences, points, table, `SELECT id,value FROM relational_deferred ORDER BY id`, nil)
}

// relationalSmokeCommand routes a utility command through the product session
// path. Utility commands bind to a command, not a physical plan, so they cannot
// be invoked through runDeclarationQuery's prepared-plan pipeline.
func relationalSmokeCommand(t *testing.T, fixture *sessionExecutionFixture, dependencies []txn.ReadDependency, sql string) (session.ExecutionResult, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := session.StatementExecution{
		Execution: session.PreparedExecution{Catalog: fixture.catalog, Dependencies: dependencies},
		Planning: func(context.Context, *catalog.CatalogSnapshot, storage.SnapshotReader) (session.PrepareOptions, error) {
			return fixture.planning(sql), nil
		},
	}
	return fixture.session.RunStatement(t.Context(), parsed.Stmts[0], options, nil)
}
