package bind_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type declarationIdentities struct{ next model.ObjectID }

func (a *declarationIdentities) AllocateDeclarationIdentity(ctx context.Context) (model.ObjectID, model.OID, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	a.next++
	return a.next, model.OID(100000 + a.next), nil
}

func bindTableDeclaration(t *testing.T, sql string, snapshot metadataSnapshot) (*bind.BoundDeclaration, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	return bind.BindCreateTable(t.Context(), parsed.Stmts[0].Stmt.GetCreateStmt(), snapshot, bind.CreateDeclarationOptions{Bind: bindingOptions(sql), Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}, Identities: &declarationIdentities{next: 100}, Owner: (model.Principal{}).OID(), Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}})
}

func TestTableDeclarationFreezesLaterColumnReferences(t *testing.T) {
	snapshot := tableSnapshot(t)
	bound, err := bindTableDeclaration(t, `CREATE TABLE declared (computed integer GENERATED ALWAYS AS (input + 1) STORED, input integer DEFAULT 4, CONSTRAINT positive CHECK (computed > input), PRIMARY KEY (computed), UNIQUE (input))`, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	descriptors := bound.Descriptors()
	spec, err := descriptors[0].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if spec.Table.Columns[0].Generated == nil || !spec.Table.Columns[0].GeneratedStored || spec.Table.Columns[1].Default == nil {
		t.Fatal("column policies were not frozen")
	}
	found := false
	for _, dep := range spec.Table.Columns[0].Generated.Dependencies {
		if dep.Object == spec.ID && dep.Column == spec.Table.Columns[1].ID {
			found = true
		}
	}
	if !found {
		t.Fatal("generated expression did not retain allocated table and later column identity")
	}
	indexDescriptors := 0
	for _, descriptor := range descriptors {
		if descriptor.Kind() != model.ObjectIndex {
			continue
		}
		indexDescriptors++
		indexSpec, err := descriptor.Spec(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if indexSpec.Index.Table != spec.ID || !indexSpec.Index.Unique {
			t.Fatal("constraint index does not belong to declared table")
		}
		linked := false
		for _, c := range spec.Table.Constraints {
			if c.Index == descriptor.ID() {
				linked = true
			}
		}
		if !linked {
			t.Fatal("unique index has no constraint link")
		}
	}
	if indexDescriptors != 2 || len(spec.Table.Indexes) != 2 {
		t.Fatalf("constraint index descriptors=%d links=%v", indexDescriptors, spec.Table.Indexes)
	}
	for _, dep := range bound.Dependencies() {
		if dep.Object == spec.ID {
			t.Fatal("new table leaked into external read dependency inventory")
		}
	}
	// Mutation of a returned descriptor copy cannot alter the declaration.
	spec.Table.Columns[0].Name = "changed"
	again, err := descriptors[0].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if again.Table.Columns[0].Name != "computed" {
		t.Fatal("declaration metadata is mutable")
	}
}

func TestTableDeclarationRejectsLaterGeneratedReference(t *testing.T) {
	_, err := bindTableDeclaration(t, `CREATE TABLE declared (a integer GENERATED ALWAYS AS (b + 1) STORED, b integer GENERATED ALWAYS AS (c + 1) STORED, c integer)`, tableSnapshot(t))
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "42P17" {
		t.Fatalf("generated dependency error=%v", err)
	}
}

func TestTableDeclarationUsesAssignmentDefaultCoercion(t *testing.T) {
	bound, err := bindTableDeclaration(t, `CREATE TABLE declared (a integer DEFAULT 1.5)`, tableSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := bound.Descriptors()[0].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bind.BindColumnDefault(t.Context(), spec.Table.Columns[0].Default, spec.Table.Columns[0].Layout, tableSnapshot(t), bindingOptions("SELECT 1")); err != nil {
		t.Fatal(err)
	}
}

func TestTableDeclarationPreservesNoSQLPrimaryKey(t *testing.T) {
	bound, err := bindTableDeclaration(t, `CREATE TABLE declared (__supersql_row_identity text)`, tableSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := bound.Descriptors()[0].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Table.Columns) != 2 || spec.Table.Columns[0].SystemIdentity || !spec.Table.Columns[1].SystemIdentity || spec.Table.Columns[1].Name == spec.Table.Columns[0].Name {
		t.Fatal("physical identity shadowed the user column")
	}
	for _, c := range spec.Table.Constraints {
		if c.Kind == model.ConstraintPrimaryKey {
			t.Fatal("physical identity invented a SQL primary key")
		}
	}
}

func TestTableDeclarationRejectsDuplicateConstraintNames(t *testing.T) {
	_, err := bindTableDeclaration(t, `CREATE TABLE declared (a integer CONSTRAINT repeated UNIQUE, b integer CONSTRAINT repeated CHECK (b > 0))`, tableSnapshot(t))
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "42710" {
		t.Fatalf("duplicate constraint error=%v", err)
	}
}

func TestTableDeclarationBindsPartialEqualityExclusion(t *testing.T) {
	sql := `CREATE TABLE declared (a integer, EXCLUDE USING btree (a WITH =) WHERE (a > 0))`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bind.BindCreateTable(t.Context(), parsed.Stmts[0].Stmt.GetCreateStmt(), tableSnapshot(t), bind.CreateDeclarationOptions{Bind: bindingOptions(sql), Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}, Identities: &declarationIdentities{next: 100}, Owner: (model.Principal{}).OID(), Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}})
	if err != nil {
		t.Fatal(err)
	}
	descriptors := bound.Descriptors()
	var table, index *model.Descriptor
	for _, descriptor := range descriptors {
		switch descriptor.Kind() {
		case model.ObjectTable:
			table = descriptor
		case model.ObjectIndex:
			index = descriptor
		}
	}
	if table == nil || index == nil {
		t.Fatalf("descriptors = %d", len(descriptors))
	}
	tableSpec, err := table.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	indexSpec, err := index.Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(tableSpec.Table.Constraints) != 1 || tableSpec.Table.Constraints[0].Kind != model.ConstraintExclude ||
		len(tableSpec.Table.Constraints[0].ExcludeOperators) != 1 || indexSpec.Index.Predicate == nil {
		t.Fatal("partial exclusion metadata was not frozen")
	}
}

func TestSchemaDeclarationExistingDoesNotAllocate(t *testing.T) {
	sql := `CREATE SCHEMA IF NOT EXISTS public`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	identities := &declarationIdentities{next: 100}
	bound, err := bind.BindCreateSchema(t.Context(), parsed.Stmts[0].Stmt.GetCreateSchemaStmt(), tableSnapshot(t), bind.CreateDeclarationOptions{Bind: bindingOptions(sql), Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}, Identities: identities, Owner: (model.Principal{}).OID()})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Existing() == nil || len(bound.Descriptors()) != 0 || identities.next != 100 {
		t.Fatal("IF NOT EXISTS allocated or replaced existing schema")
	}
}

// PostgreSQL parses DEFERRABLE, NOT DEFERRABLE, INITIALLY DEFERRED and INITIALLY
// IMMEDIATE as separate CONSTR_ATTR_* items that follow the constraint they modify,
// so the binder must fold each into that constraint. Without the fold the trailing
// attribute fails as an unimplemented constraint kind.
func TestColumnConstraintDeferrabilityAttributesFoldIntoConstraint(t *testing.T) {
	snapshot := tableSnapshot(t)
	for _, test := range []struct {
		name              string
		declaration       string
		deferrable        bool
		initiallyDeferred bool
	}{
		{
			name:              "both attributes apply",
			declaration:       `CREATE TABLE deferred (id bigint PRIMARY KEY, value bigint UNIQUE DEFERRABLE INITIALLY DEFERRED)`,
			deferrable:        true,
			initiallyDeferred: true,
		},
		{
			// The grammar accepts the two attributes in either order.
			name:              "both attributes apply in the reverse order",
			declaration:       `CREATE TABLE deferred (id bigint PRIMARY KEY, value bigint UNIQUE INITIALLY DEFERRED DEFERRABLE)`,
			deferrable:        true,
			initiallyDeferred: true,
		},
		{
			name:        "deferrable without deferred timing",
			declaration: `CREATE TABLE deferred (id bigint PRIMARY KEY, value bigint UNIQUE DEFERRABLE)`,
			deferrable:  true,
		},
		{
			name:        "not deferrable resets to the default",
			declaration: `CREATE TABLE deferred (id bigint PRIMARY KEY, value bigint UNIQUE NOT DEFERRABLE)`,
		},
		{
			name:        "initially immediate resets the timing",
			declaration: `CREATE TABLE deferred (id bigint PRIMARY KEY, value bigint UNIQUE INITIALLY IMMEDIATE)`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			bound, err := bindTableDeclaration(t, test.declaration, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			descriptors := bound.Descriptors()
			spec, err := descriptors[0].Spec(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			var unique *model.ConstraintDescriptor
			for i := range spec.Table.Constraints {
				if spec.Table.Constraints[i].Kind == model.ConstraintUnique {
					unique = &spec.Table.Constraints[i]
				}
			}
			if unique == nil {
				t.Fatalf("unique constraint absent from %v", spec.Table.Constraints)
			}
			if unique.Deferrable != test.deferrable || unique.InitiallyDeferred != test.initiallyDeferred {
				t.Fatalf("unique deferrability = deferrable:%v deferred:%v, want deferrable:%v deferred:%v",
					unique.Deferrable, unique.InitiallyDeferred, test.deferrable, test.initiallyDeferred)
			}
		})
	}
}

// Repeating one deferrability axis is an error; the two axes are independent, so one
// of each is allowed (covered above) but two of the same is not. INITIALLY DEFERRED
// without DEFERRABLE is also invalid, and the fold must not silently imply it.
func TestColumnConstraintRepeatedDeferrabilityAttributeRejected(t *testing.T) {
	snapshot := tableSnapshot(t)
	for _, declaration := range []string{
		`CREATE TABLE repeated (id bigint PRIMARY KEY, value bigint UNIQUE DEFERRABLE NOT DEFERRABLE)`,
		`CREATE TABLE repeated (id bigint PRIMARY KEY, value bigint UNIQUE INITIALLY DEFERRED INITIALLY IMMEDIATE)`,
		`CREATE TABLE repeated (id bigint PRIMARY KEY, value bigint UNIQUE INITIALLY DEFERRED)`,
		`CREATE TABLE repeated (id bigint PRIMARY KEY, value bigint UNIQUE NOT DEFERRABLE INITIALLY DEFERRED)`,
	} {
		if _, err := bindTableDeclaration(t, declaration, snapshot); err == nil {
			t.Fatalf("%s: invalid deferrability attribute combination accepted", declaration)
		}
	}
}
