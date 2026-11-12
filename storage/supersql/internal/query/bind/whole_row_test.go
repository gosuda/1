package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestWholeRowColumnPrecedence(t *testing.T) {
	for _, sql := range []string{
		`SELECT v FROM (VALUES(7))v(v)`,
		`SELECT v FROM (VALUES(7))a(v),(SELECT) v`,
		`SELECT (SELECT v FROM (SELECT) v) AS v FROM (VALUES(7))a(v)`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireCompositeTargetColumns(t, statement.Columns(), []string{"v"}, []model.TypeID{23})
		})
	}
	_, err := bindSQL(t, `SELECT v FROM (VALUES(1))a(v),(VALUES(2))b(v),(SELECT)v`, nil, nil)
	requireCode(t, err, "42702")
}

func TestWholeRowDerivedFieldsSurviveProjection(t *testing.T) {
	for _, sql := range []string{
		`SELECT (v).a,(v).b FROM (VALUES(1,NULL::text))v(a,b)`,
		`SELECT (s.r).* FROM (SELECT v AS r FROM (VALUES(1,NULL::text))v(a,b))s`,
		`WITH s AS (SELECT v AS r FROM (VALUES(1,NULL::text))v(a,b)) SELECT (s.r).* FROM s`,
		`SELECT (j).* FROM ((VALUES(1))l(a) CROSS JOIN (VALUES(NULL::text))r(b))j`,
		`SELECT (q.r).* FROM (VALUES(1,NULL::text))v(a,b), LATERAL (SELECT v AS r)q`,
		`SELECT (q.r).* FROM (VALUES(1,NULL::text))v(a,b), LATERAL (SELECT (SELECT v) AS r)q`,
		`SELECT (q.r).* FROM (VALUES(1,NULL::text))v(a,b), LATERAL (VALUES(v))q(r)`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireCompositeTargetColumns(t, statement.Columns(), []string{"a", "b"}, []model.TypeID{23, 25})
		})
	}
}

func TestWholeRowFunctionAliasFields(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (f).* FROM generate_series(1,2)f(a)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireCompositeTargetColumns(t, statement.Columns(), []string{"a"}, []model.TypeID{23})
}

func TestWholeRowZeroColumnNamespaces(t *testing.T) {
	for _, sql := range []string{
		`SELECT e FROM (SELECT)e`,
		`SELECT j FROM ((SELECT)l CROSS JOIN (SELECT)r)j`,
		`SELECT (SELECT e) FROM (SELECT)e`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			columns := statement.Columns()
			if len(columns) != 1 || columns[0].Layout.Type.ID != 2249 || len(columns[0].Layout.Children) != 0 {
				t.Fatalf("zero-column relation value = %+v", columns)
			}
		})
	}
	for _, sql := range []string{`SELECT e.* FROM (SELECT)e`, `SELECT (e).* FROM (SELECT)e`} {
		statement, err := bindSQL(t, sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(statement.Columns()) != 0 {
			t.Fatalf("zero-column star = %+v", statement.Columns())
		}
	}
	_, err := bindSQL(t, `SELECT 1 FROM (SELECT)e,(SELECT)e`, nil, nil)
	requireCode(t, err, "42712")
}

func TestWholeRowNullabilityTracksJoinPresence(t *testing.T) {
	for _, tc := range []struct {
		sql      string
		nullable []bool
	}{
		{`SELECT l,r FROM (VALUES(NULL::integer))l(a) LEFT JOIN (VALUES(NULL::integer))r(b) ON false`, []bool{false, true}},
		{`SELECT l,r FROM (VALUES(NULL::integer))l(a) RIGHT JOIN (VALUES(NULL::integer))r(b) ON false`, []bool{true, false}},
		{`SELECT l,r FROM (VALUES(NULL::integer))l(a) FULL JOIN (VALUES(NULL::integer))r(b) ON false`, []bool{true, true}},
		{`SELECT l,r,s FROM ((VALUES(NULL::integer))l(a) LEFT JOIN (VALUES(NULL::integer))r(b) ON false) FULL JOIN (VALUES(NULL::integer))s(c) ON false`, []bool{true, true, true}},
		{`SELECT l,r FROM (VALUES(NULL::integer))l(a) LEFT JOIN LATERAL (SELECT l.a AS b)r ON false`, []bool{false, true}},
		{`SELECT j FROM ((VALUES(NULL::integer))l(a) FULL JOIN (VALUES(NULL::integer))r(b) ON false)j`, []bool{false}},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			columns := statement.Columns()
			if len(columns) != len(tc.nullable) {
				t.Fatalf("columns = %+v", columns)
			}
			for i, nullable := range tc.nullable {
				if columns[i].Nullable != nullable {
					t.Errorf("column %d root nullable = %t, want %t", i, columns[i].Nullable, nullable)
				}
			}
		})
	}
}

func TestWholeRowNamesHonorScopeBoundaries(t *testing.T) {
	for _, tc := range []struct{ sql, code string }{
		{`SELECT l FROM ((SELECT)l CROSS JOIN (SELECT)r)j`, "42703"},
		{`SELECT q.r FROM (VALUES(1))v(a),(SELECT v AS r)q`, "42703"},
		{`SELECT q.r FROM (VALUES(1))v(a) RIGHT JOIN LATERAL (SELECT v AS r)q ON true`, "42P10"},
		{`SELECT q.r FROM (SELECT)v FULL JOIN LATERAL (SELECT v AS r)q ON true`, "42P10"},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, nil, nil)
			requireCode(t, err, tc.code)
		})
	}
}

func wholeRowTableSnapshot(t *testing.T) metadataSnapshot {
	t.Helper()
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	row := model.TypeLayout{Type: model.Type{ID: 20012, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{integer}}
	tableSpec, err := snapshot.descriptors[1].SpecWithin(t.Context(), model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
	if err != nil {
		t.Fatal(err)
	}
	tableSpec.Table.RowType = 12
	snapshot.descriptors[1] = testDescriptor(t, tableSpec)
	companion := testDescriptor(t, model.DescriptorSpec{ID: 12, OID: 20012, Version: 5, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "Mixed"}, Dependencies: []model.Dependency{{Object: 10, Version: 1}, {Object: 11, Version: 3}}, Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: row, Array: 13, Relation: 11, Fields: []model.ColumnDescriptor{{ID: 1, Name: "X", State: model.StatePublic, Layout: integer}}}})
	snapshot.descriptors = append(snapshot.descriptors, companion)
	return snapshot
}

func TestWholeRowNominalIdentityAndAliasFields(t *testing.T) {
	snapshot := wholeRowTableSnapshot(t)
	for _, sql := range []string{
		`SELECT "Mixed" FROM "Mixed"`,
		`SELECT public."Mixed" FROM "Mixed"`,
		`SELECT "a.b" FROM public."Mixed" AS "a.b"`,
	} {
		statement, err := bindSQL(t, sql, snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
		columns := statement.Columns()
		if len(columns) != 1 || columns[0].Layout.Type.ID != 20012 {
			t.Fatalf("nominal relation value = %+v", columns)
		}
		found := false
		for _, dep := range statement.Dependencies() {
			found = found || dep.Object == 12 && dep.Version == 5 && dep.Column == 1
		}
		if !found {
			t.Fatalf("missing rowtype field dependency: %+v", statement.Dependencies())
		}
	}
	for _, sql := range []string{
		`SELECT (w).renamed FROM "Mixed" w(renamed)`,
		`SELECT (s.r)."X" FROM (SELECT w AS r FROM "Mixed" w(renamed))s`,
	} {
		statement, err := bindSQL(t, sql, snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
		if columns := statement.Columns(); len(columns) != 1 || columns[0].Layout.Type.ID != 23 {
			t.Fatalf("nominal field = %+v", columns)
		}
	}
	for _, tc := range []struct{ sql, code string }{
		{`SELECT public."Mixed" FROM public."Mixed" w`, "42P01"},
		{`SELECT (s.r).renamed FROM (SELECT w AS r FROM "Mixed" w(renamed))s`, "42703"},
	} {
		_, err := bindSQL(t, tc.sql, snapshot, nil)
		requireCode(t, err, tc.code)
	}
	_, err := bindSQL(t, `SELECT "Mixed" FROM "Mixed"`, tableSnapshot(t), nil)
	requireCode(t, err, "42704")
}

func TestWholeRowReturningKeepsAbsentImagesNullable(t *testing.T) {
	for _, tc := range []struct {
		sql      string
		nullable []bool
	}{
		{`INSERT INTO "Mixed" VALUES(1) RETURNING old,new`, []bool{true, false}},
		{`DELETE FROM "Mixed" RETURNING old,new`, []bool{false, true}},
	} {
		statement, err := bindSQL(t, tc.sql, wholeRowTableSnapshot(t), nil)
		if err != nil {
			t.Fatal(err)
		}
		columns := statement.Columns()
		if len(columns) != 2 {
			t.Fatalf("returning images = %+v", columns)
		}
		for i, c := range columns {
			if c.Layout.Type.ID != 20012 || c.Nullable != tc.nullable[i] {
				t.Errorf("image %d = %+v", i, c)
			}
		}
	}
}

func TestUnusedWholeRowsNeedNoNominalMetadataOrCompositeAllocation(t *testing.T) {
	statement, err := bindSQL(t, `SELECT w."X" FROM "Mixed" w`, tableSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		scalar, _ := graph.Scalar(id)
		if scalar.Kind == ir.Row {
			t.Fatal("ordinary column access constructs a whole-row value")
		}
	}
}

func TestWholeRowDescriptorsRespectBindingLimits(t *testing.T) {
	const sql = `SELECT w FROM "Mixed" w`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bindingOptions(sql)
	options.Limits.MaxBytes = 256
	_, err = bind.Bind(t.Context(), parsed.Stmts[0], wholeRowTableSnapshot(t), options, nil)
	if !errors.Is(err, model.ErrDescriptorLimit) && !errors.Is(err, ir.ErrLimit) {
		requireCode(t, err, "54000")
	}
}

func TestWholeRowCapturesGroupedValue(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (SELECT v) AS r FROM (VALUES(1,NULL::text))v(a,b) GROUP BY v`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 1 || columns[0].Layout.Type.ID != 2249 || len(columns[0].Layout.Children) != 2 {
		t.Fatalf("grouped whole-row capture = %+v", columns)
	}
}

func TestWholeRowRequiresMatchingCompanionDescriptor(t *testing.T) {
	snapshot := wholeRowTableSnapshot(t)
	spec, err := snapshot.descriptors[2].Spec(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	spec.Type.Fields[0].Name = "different"
	snapshot.descriptors[2] = testDescriptor(t, spec)
	_, err = bindSQL(t, `SELECT "Mixed" FROM "Mixed"`, snapshot, nil)
	requireCode(t, err, "XX000")
}

func TestWholeRowUnqualifiedSchemasAreAmbiguous(t *testing.T) {
	_, err := bindSQL(t, `SELECT t FROM public.t,other.t`, schemaNamespaceSnapshot(t), nil)
	requireCode(t, err, "42P09")
}

func TestWholeRowReturningSourceHidesImplicitImageAlias(t *testing.T) {
	statement, err := bindSQL(t, `DELETE FROM "Mixed" USING (SELECT)old RETURNING old`, wholeRowTableSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 1 || columns[0].Layout.Type.ID != 2249 || len(columns[0].Layout.Children) != 0 {
		t.Fatalf("source whole row = %+v, want empty anonymous record rather than OLD image", columns)
	}
}
