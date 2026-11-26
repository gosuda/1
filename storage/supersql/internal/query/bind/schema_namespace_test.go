package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func schemaNamespaceSnapshot(t *testing.T) metadataSnapshot {
	t.Helper()
	snapshot := tableSnapshot(t)
	spec, err := snapshot.descriptors[1].SpecWithin(t.Context(), model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
	if err != nil {
		t.Fatal(err)
	}
	spec.Name.Name = "t"
	spec.Table.Columns[0].Name = "x"
	snapshot.descriptors[1] = testDescriptor(t, spec)

	other := testDescriptor(t, model.DescriptorSpec{
		ID: 12, OID: 10012, Version: 1, State: model.StatePublic,
		Name:   model.NamespaceName{Kind: model.ObjectSchema, Name: "other"},
		Schema: &model.SchemaMetadata{Owner: 10},
	})
	spec.ID = 13
	spec.OID = 10013
	spec.Name.Parent = other.ID()
	spec.Table.ColumnIDHighWatermark = 2
	spec.Table.Columns = []model.ColumnDescriptor{
		{ID: 1, Name: "x", State: model.StatePublic, Layout: model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}},
		{ID: 2, Name: "y", State: model.StatePublic, Layout: model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}},
	}
	snapshot.descriptors = append(snapshot.descriptors, other, testDescriptor(t, spec))
	return snapshot
}

func TestBindSchemaQualifiedColumnsSelectExactRelation(t *testing.T) {
	statement, err := bindSQL(t, `SELECT other.t.x, public.t.x FROM public.t, other.t`, schemaNamespaceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 2 || columns[0].Name != "x" || columns[0].Layout.Type.ID != 25 || columns[1].Name != "x" || columns[1].Layout.Type.ID != 23 {
		t.Fatalf("qualified columns = %+v, want x text, x integer", columns)
	}
}

func TestBindSchemaQualifiedStarsRetainSearchPathIdentity(t *testing.T) {
	statement, err := bindSQL(t, `SELECT other.t.*, public.t.* FROM t, other.t`, schemaNamespaceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 3 {
		t.Fatalf("qualified stars = %+v, want three columns", columns)
	}
	for i, want := range []struct {
		name string
		typ  model.TypeID
	}{{"x", 25}, {"y", 16}, {"x", 23}} {
		if columns[i].Name != want.name || columns[i].Layout.Type.ID != want.typ {
			t.Errorf("column %d = %+v, want %s with type %d", i, columns[i], want.name, want.typ)
		}
	}
}

func TestBindSchemaNamespaceAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		code string
	}{
		{"table qualifier", `SELECT t.x FROM public.t, other.t`, "42P09"},
		{"qualifier before column lookup", `SELECT t.y FROM public.t, other.t`, "42P09"},
		{"table star", `SELECT t.* FROM public.t, other.t`, "42P09"},
		{"unqualified column", `SELECT x FROM public.t, other.t`, "42702"},
		{"qualified missing column", `SELECT public.t.y FROM public.t, other.t`, "42703"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			requireCode(t, err, tc.code)
		})
	}
}

func TestBindSchemaNamespaceCollisions(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"same relation", `SELECT * FROM public.t, public.t`},
		{"same relation through search path", `SELECT * FROM t, public.t`},
		{"alias follows unaliased relation", `SELECT * FROM public.t, other.t AS t`},
		{"unaliased relation follows alias", `SELECT * FROM public.t AS t, other.t`},
		{"duplicate aliases", `SELECT * FROM public.t AS a, other.t AS a`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			requireCode(t, err, "42712")
		})
	}
}

func TestBindSchemaAliasHidesCatalogName(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"renamed column", `SELECT public.t.x FROM public.t AS a`},
		{"renamed star", `SELECT public.t.* FROM public.t AS a`},
		{"same name column", `SELECT public.t.x FROM public.t AS t`},
		{"same name star", `SELECT public.t.* FROM public.t AS t`},
		{"dotted alias is not schema qualification", `SELECT public.t.x FROM public.t AS "public.t"`},
		{"schema qualification is not dotted alias", `SELECT "public.t".x FROM public.t`},
		{"whole join alias hides schema", `SELECT public.t.* FROM (public.t CROSS JOIN other.t) AS j`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			requireCode(t, err, "42P01")
		})
	}
}

func TestBindSchemaNamespaceLexicalErrorsDoNotFallThrough(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
		code string
	}{
		{
			name: "local alias hides outer column",
			sql:  `SELECT (SELECT t.y FROM public.t AS t) FROM other.t`,
			code: "42703",
		},
		{
			name: "local qualifier ambiguity hides outer relation",
			sql:  `SELECT (SELECT t.x FROM public.t, other.t) FROM public.t`,
			code: "42P09",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, schemaNamespaceSnapshot(t), nil)
			requireCode(t, err, tc.code)
		})
	}
}

func TestBindSchemaQualificationIsDistinctFromDottedAlias(t *testing.T) {
	statement, err := bindSQL(t, `SELECT public.t.x, "public.t".x, public.t.*, "public.t".*
		FROM public.t, (VALUES('alias'::text)) AS "public.t"(x)`, schemaNamespaceSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 4 {
		t.Fatalf("qualified columns = %+v, want four columns", columns)
	}
	for i, want := range []model.TypeID{23, 25, 23, 25} {
		if columns[i].Name != "x" || columns[i].Layout.Type.ID != want {
			t.Errorf("column %d = %+v, want x with type %d", i, columns[i], want)
		}
	}
}

func TestBindQuotedSchemaAndTableDotsRemainDistinct(t *testing.T) {
	snapshot := schemaNamespaceSnapshot(t)
	for i, name := range []string{"a.b", "c", "a", "b.c"} {
		spec, err := snapshot.descriptors[i].SpecWithin(t.Context(), model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
		if err != nil {
			t.Fatal(err)
		}
		spec.Name.Name = name
		snapshot.descriptors[i] = testDescriptor(t, spec)
	}
	statement, err := bindSQL(t, `SELECT "a.b".c.x, a."b.c".x, "a.b".c.*, a."b.c".*
		FROM "a.b".c, a."b.c"`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 5 {
		t.Fatalf("qualified columns = %+v, want five columns", columns)
	}
	for i, want := range []struct {
		name string
		typ  model.TypeID
	}{{"x", 23}, {"x", 25}, {"x", 23}, {"x", 25}, {"y", 16}} {
		if columns[i].Name != want.name || columns[i].Layout.Type.ID != want.typ {
			t.Errorf("column %d = %+v, want %s with type %d", i, columns[i], want.name, want.typ)
		}
	}
}

func TestBindSchemaOuterLateralRejectsImmediateLeftReference(t *testing.T) {
	for _, sql := range []string{
		`SELECT r.x FROM public.t RIGHT JOIN LATERAL (SELECT public.t.x) AS r ON true`,
		`SELECT r.x FROM public.t FULL JOIN LATERAL (SELECT public.t.*) AS r ON true`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, schemaNamespaceSnapshot(t), nil)
			requireCode(t, err, "42P10")
		})
	}
}

func TestBindSchemaQualifiedViewUsesViewNamespace(t *testing.T) {
	snapshot := schemaNamespaceSnapshot(t)
	parsed, err := zpgparse.Parse(`CREATE VIEW public.v AS SELECT 7 AS x`)
	if err != nil {
		t.Fatal(err)
	}
	const definition = `CREATE VIEW public.v AS SELECT 7 AS x`
	command, ok, err := bind.BindObjectCommand(t.Context(), parsed.Stmts[0].Stmt, snapshot, bind.CreateDeclarationOptions{
		Bind: bindingOptions(definition), Descriptors: model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20},
		Identities: &declarationIdentities{next: 100}, Owner: (model.Principal{}).OID(),
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
	})
	if err != nil || !ok {
		t.Fatalf("view declaration = %v, %v", ok, err)
	}
	var view model.DescriptorSpec
	for _, created := range command.Creates() {
		if created.Kind() == model.ObjectView {
			view, err = created.SpecWithin(t.Context(), model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if view.View == nil {
		t.Fatal("view declaration created no view descriptor")
	}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, view))
	statement, err := bindSQL(t, `SELECT public.v.x, public.v.* FROM public.v`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 2 {
		t.Fatalf("columns = %v, want two view columns", columns)
	}
	for _, c := range columns {
		if c.Name != "x" || c.Layout.Type.ID != 23 {
			t.Errorf("column = %+v, want x integer", c)
		}
	}
}
