package bind_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func bindingOptions(sql string) bind.Options {
	return bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: ir.Limits{MaxNodes: 10000, MaxOperands: 20000, MaxSlots: 10000, MaxDepth: 100, MaxBytes: 16 << 20, SourceBytes: uint32(len(sql))}}
}
func bindSQL(t *testing.T, sql string, snapshot bind.CatalogSnapshot, params []model.TypeLayout) (*bind.BoundStatement, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Stmts) != 1 {
		t.Fatal("test needs one statement")
	}
	options := bindingOptions(sql)
	// A production caller resolves declared types from the pinned catalog and
	// hands the resolver to Bind (session/execute_statement.go sets
	// builtins.Types from NewDeclaredTypeRuntime). Mirror that here so a
	// snapshot carrying user domains, composites or ranges reaches the same
	// builtin resolvers the executor will use.
	if metadata, ok := snapshot.(metadataSnapshot); ok {
		definitions := make([]builtin.TypeDefinition, 0, len(metadata.descriptors))
		for _, descriptor := range metadata.descriptors {
			if descriptor.Kind() == model.ObjectType {
				definitions = append(definitions, builtin.TypeDefinition{Descriptor: descriptor, Published: true})
			}
		}
		if len(definitions) != 0 {
			// The compiler is consulted only for a domain constraint or a range
			// canonical/difference function. These fixtures declare neither, so
			// resolve the declared types without one.
			resolver, err := builtin.NewDeclaredTypeResolver(t.Context(), definitions, nil, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 1024, MaxDepth: 64})
			if err != nil {
				t.Fatal(err)
			}
			options.Builtin.Types = resolver
		}
	}
	return bind.Bind(t.Context(), parsed.Stmts[0], snapshot, options, params)
}
func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != code {
		t.Fatalf("error = %v, want SQLSTATE %s", err, code)
	}
}
func TestBindParameterInferenceDoesNotReadQuotedText(t *testing.T) {
	statement, err := bindSQL(t, `SELECT '$1' AS "Text", $1 + 2 AS n`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	params := statement.Parameters()
	if len(columns) != 2 || columns[0].Name != "Text" || columns[0].Layout.Type.ID != 25 || columns[1].Layout.Type.ID != 23 {
		t.Fatalf("result columns = %#v", columns)
	}
	if len(params) != 1 || params[0].Type.ID != 23 {
		t.Fatalf("parameters = %#v", params)
	}
	columns[0].Name = "changed"
	params[0].Type.ID = 16
	if statement.Columns()[0].Name != "Text" || statement.Parameters()[0].Type.ID != 23 {
		t.Fatal("caller mutation changed bound statement")
	}
}
func TestBindPreservesTypedEmptyOutput(t *testing.T) {
	statement, err := bindSQL(t, `SELECT 1 AS n, NULL::text AS t WHERE false`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 2 || columns[0].Layout.Type.ID != 23 || columns[0].Nullable || columns[1].Layout.Type.ID != 25 || !columns[1].Nullable {
		t.Fatalf("empty result schema = %#v", columns)
	}
}
func TestBindScopeAndTypeErrors(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"missing relation", `SELECT * FROM absent`, "42P01"},
		{"missing column", `SELECT x`, "42703"},
		{"ambiguous column", `SELECT x FROM (VALUES(1)) a(x), (VALUES(2)) b(x)`, "42702"},
		{"duplicate output names ambiguous", `SELECT a.x FROM (SELECT 1 AS x, 2 AS x) a`, "42702"},
		{"unknown function", `SELECT no_such_function(1)`, "42883"},
		{"unknown type", `SELECT 1::missing_type`, "42704"},
		{"inconsistent parameter", `SELECT pg_catalog.int48eq($1, $1)`, "42P08"},
		{"ungrouped column", `SELECT x, count(*) FROM (VALUES(1)) a(x)`, "42803"},
		{"nonlateral reference", `SELECT * FROM (VALUES(1)) a(x), (SELECT a.x) b`, "42P01"},
		{"star without relation", `SELECT *`, "42601"},
		{"qualified relation shadows outer", `SELECT (SELECT a.x FROM (VALUES(2)) a(y)) FROM (VALUES(1)) a(x)`, "42703"},
		{"RANGE offset without order", `SELECT sum(x) OVER (RANGE 1 PRECEDING) FROM (VALUES(1)) t(x)`, "42P20"},
		{"RANGE offset with multiple keys", `SELECT sum(x) OVER (ORDER BY x,y RANGE 1 PRECEDING) FROM (VALUES(1,2)) t(x,y)`, "42P20"},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := bindSQL(t, tc.sql, nil, nil); requireCode(t, err, tc.code) })
	}
}
func TestBindOuterJoinNullability(t *testing.T) {
	statement, err := bindSQL(t, `SELECT a.x, b.y FROM (VALUES(1)) a(x) LEFT JOIN (VALUES(2)) b(y) ON a.x=b.y`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 2 || columns[0].Nullable || !columns[1].Nullable {
		t.Fatalf("outer join columns = %#v", columns)
	}
}
func TestBindLateralCapturesAndCTEScopes(t *testing.T) {
	for _, sql := range []string{
		`SELECT b.y FROM (VALUES(1)) a(x), LATERAL (SELECT a.x+1 AS y) b`,
		`WITH a AS (SELECT 1 AS x), b AS (SELECT x+1 AS y FROM a) SELECT y FROM b`,
		`WITH a AS (SELECT 1 AS x) SELECT q.x FROM (WITH a AS (SELECT 2 AS x) SELECT x FROM a) q`,
		`SELECT (SELECT a.x) AS x FROM (VALUES(1)) a(x)`,
		`SELECT EXISTS(SELECT 1 WHERE a.x=1) AS found FROM (VALUES(1)) a(x)`,
		`SELECT a.x IN (SELECT 1) AS found FROM (VALUES(1)) a(x)`,
		`WITH RECURSIVE a(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM a WHERE x<3) SELECT x FROM a`,
		`SELECT b.y FROM (VALUES(1)) a(x) LEFT JOIN LATERAL (SELECT a.x AS y) b ON b.y>1`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(statement.Columns()) != 1 {
				t.Fatalf("columns = %#v", statement.Columns())
			}
		})
	}
}
func TestBindAggregatesAndWindowsExposeResultTypes(t *testing.T) {
	for _, sql := range []string{
		`SELECT count(*) AS n FROM (VALUES(1),(2)) a(x)`,
		`SELECT sum(x) AS n FROM (VALUES(1),(2)) a(x)`,
		`SELECT count(x) AS n FROM (VALUES(1),(NULL)) a(x)`,
		`SELECT row_number() OVER (ORDER BY x) AS n FROM (VALUES(1),(2)) a(x)`,
		`SELECT row_number() OVER w AS n FROM (VALUES(1),(2)) a(x) WINDOW w AS (ORDER BY x ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			columns := statement.Columns()
			if len(columns) != 1 || columns[0].Layout.Type.ID != 20 {
				t.Fatalf("columns = %#v", columns)
			}
		})
	}
}
func TestBindChecksUnusedExcludedSubtrees(t *testing.T) {
	_, err := bindSQL(t, `WITH unused AS (SELECT pg_catalog.pg_read_file('secret')) SELECT 1`, nil, nil)
	requireCode(t, err, "0A000")
}
func TestBindCancellationAndResourceLimit(t *testing.T) {
	sql := `SELECT 1+2+3`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = bind.Bind(ctx, parsed.Stmts[0], nil, bindingOptions(sql), nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want canceled", err)
	}
	options := bindingOptions(sql)
	options.Limits.MaxNodes = 2
	_, err = bind.Bind(t.Context(), parsed.Stmts[0], nil, options, nil)
	requireCode(t, err, "54000")
}

type metadataSnapshot struct{ descriptors []*model.Descriptor }

func (s metadataSnapshot) Timestamp() model.Timestamp { return 77 }
func (s metadataSnapshot) Descriptor(ctx context.Context, id model.ObjectID) (catalog.PublishedDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PublishedDescriptor{}, err
	}
	for _, d := range s.descriptors {
		if d.ID() == id {
			return catalog.PublishedDescriptor{Descriptor: d}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (s metadataSnapshot) Resolve(ctx context.Context, name model.NamespaceName) (catalog.PublishedDescriptor, error) {
	if err := ctx.Err(); err != nil {
		return catalog.PublishedDescriptor{}, err
	}
	for _, d := range s.descriptors {
		if d.Name() == name {
			return catalog.PublishedDescriptor{Descriptor: d}, nil
		}
	}
	return catalog.PublishedDescriptor{}, catalog.ErrUnknownObject
}
func (s metadataSnapshot) ScanNamespace(ctx context.Context, parent model.ObjectID, kind model.ObjectKind, visit func(catalog.PublishedDescriptor) error) error {
	for _, d := range s.descriptors {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Name().Parent == parent && d.Kind() == kind {
			if err := visit(catalog.PublishedDescriptor{Descriptor: d}); err != nil {
				return err
			}
		}
	}
	return nil
}
func testDescriptor(t *testing.T, spec model.DescriptorSpec) *model.Descriptor {
	t.Helper()
	d, err := model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// compiledRoutine turns a SQL routine fixture into a durable, executable
// definition. A hand-rolled ExpressionDefinition carries no encoding, and
// procedural.Decode rejects any body without one, so binding such a stub cannot
// reproduce what CREATE FUNCTION stores.
//
// metadata is authoritative for parameters and result, exactly as it is when
// the binder compiles a real CREATE FUNCTION; body supplies the SQL text.
func compiledRoutine(t *testing.T, snapshot bind.CatalogSnapshot, body string, metadata model.RoutineMetadata) model.RoutineMetadata {
	t.Helper()
	parsed, err := zpgparse.Parse("CREATE FUNCTION public.fixture() RETURNS void LANGUAGE sql AS " + quoteLiteral(body))
	if err != nil {
		t.Fatal(err)
	}
	create := parsed.Stmts[0].Stmt.GetCreateFunctionStmt()
	if create == nil {
		t.Fatal("routine fixture needs a CREATE FUNCTION statement")
	}
	metadata.Atomic = true
	compiled, err := procedural.CompileRoutine(t.Context(), create, metadata, snapshot, procedural.CompileOptions{
		Bind: bindingOptions(body), Limits: procedural.DefaultLimits(), CheckBodies: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}

func quoteLiteral(text string) string { return "'" + strings.ReplaceAll(text, "'", "''") + "'" }
func tableSnapshot(t *testing.T) metadataSnapshot {
	t.Helper()
	typ := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	schema := testDescriptor(t, model.DescriptorSpec{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}})
	table := testDescriptor(t, model.DescriptorSpec{ID: 11, OID: 10011, Version: 3, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectTable, Name: "Mixed"}, Physical: model.PhysicalIdentity{Generation: 4, EncodingVersion: 1}, Table: &model.TableMetadata{Owner: 10, ColumnIDHighWatermark: 1, Columns: []model.ColumnDescriptor{{ID: 1, Name: "X", State: model.StatePublic, Layout: typ}}}})
	return metadataSnapshot{[]*model.Descriptor{schema, table}}
}
func TestBindQuotedCatalogNamesAndVersionedDependencies(t *testing.T) {
	statement, err := bindSQL(t, `SELECT "X" FROM "Mixed"`, tableSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 1 || columns[0].Name != "X" || columns[0].Layout.Type.ID != 23 {
		t.Fatalf("columns = %#v", columns)
	}
	found := false
	for _, dep := range statement.Dependencies() {
		if dep.Object == 11 && dep.Version == 3 && dep.Column == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("dependencies = %#v", statement.Dependencies())
	}
	_, err = bindSQL(t, `SELECT "X" FROM mixed`, tableSnapshot(t), nil)
	requireCode(t, err, "42P01")
}

func TestBindUserRoutineDoesNotInheritBuiltinExclusion(t *testing.T) {
	snapshot := tableSnapshot(t)
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}
	routine := testDescriptor(t, model.DescriptorSpec{ID: 12, OID: 90012, Version: 2, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "pg_read_file", Signature: "text"}, Routine: new(compiledRoutine(t, snapshot, "SELECT 'safe'::text", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: []model.RoutineParameter{{Name: "path", Layout: text, Mode: "in"}}, Result: text, Language: "sql", Volatility: "immutable", Strict: true}))})
	snapshot.descriptors = append(snapshot.descriptors, routine)
	statement, err := bindSQL(t, `SELECT public.pg_read_file('not a file')`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if statement.Columns()[0].Layout.Type.ID != 25 {
		t.Fatalf("result type = %v", statement.Columns()[0].Layout.Type)
	}
	found := false
	for _, dependency := range statement.Dependencies() {
		if dependency.Object == 12 && dependency.Version == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("dependencies = %#v", statement.Dependencies())
	}
}

func TestBindModificationHasWriteEffects(t *testing.T) {
	for _, sql := range []string{`INSERT INTO "Mixed" ("X") VALUES (7)`, `UPDATE "Mixed" SET "X"="X"+1 WHERE "X"=2`, `DELETE FROM "Mixed" WHERE "X"=2`} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, tableSnapshot(t), nil)
			if err != nil {
				t.Fatal(err)
			}
			if statement.Effects().Volatility != ir.Volatile || !statement.Effects().MayError {
				t.Fatalf("write effects = %#v", statement.Effects())
			}
			if len(statement.Columns()) != 0 {
				t.Fatalf("non-RETURNING schema = %#v", statement.Columns())
			}
		})
	}
}

func TestBindSearchPathFindsDifferentUserOverload(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 950}}
	routine := testDescriptor(t, model.DescriptorSpec{ID: 13, OID: 90013, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "pg_read_file", Signature: "int4"}, Routine: new(compiledRoutine(t, snapshot, "SELECT 'safe'::text", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: []model.RoutineParameter{{Name: "number", Layout: integer, Mode: "in"}}, Result: text, Language: "sql", Volatility: "immutable"}))})
	snapshot.descriptors = append(snapshot.descriptors, routine)
	statement, err := bindSQL(t, `SELECT pg_read_file(7)`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if statement.Columns()[0].Layout.Type.ID != 25 {
		t.Fatalf("result type = %v", statement.Columns()[0].Layout.Type)
	}
}

func TestBindReturningUsesTypedPostWriteFields(t *testing.T) {
	statement, err := bindSQL(t, `UPDATE "Mixed" SET "X"=7 RETURNING old."X" AS before, new."X" AS after, tableoid AS target_oid, old.tableoid AS old_oid`, tableSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 4 || columns[0].Name != "before" || columns[1].Name != "after" || columns[0].Layout.Type.ID != 23 || columns[1].Layout.Type.ID != 23 ||
		columns[2].Name != "target_oid" || columns[3].Name != "old_oid" || columns[2].Layout.Type.ID != 26 || columns[3].Layout.Type.ID != 26 {
		t.Fatalf("RETURNING columns = %#v", columns)
	}
}

func TestBindSettingCommandOwnsLiteralValues(t *testing.T) {
	statement, err := bindSQL(t, `SET LOCAL application_name = 'owned'`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	command, ok := statement.Command().(bind.SettingCommand)
	if !ok || command.Action != bind.SetSetting || command.Name != "application_name" || !command.Local {
		t.Fatalf("command = %#v", statement.Command())
	}
	if len(command.Values) != 1 || command.Values[0].StrVal != "owned" {
		t.Fatalf("command = %#v", statement.Command())
	}
	command.Values[0].StrVal = "changed"
	if statement.Command().(bind.SettingCommand).Values[0].StrVal != "owned" {
		t.Fatal("command accessor retained a mutable value slice")
	}
	if statement.Graph() != nil {
		t.Fatal("utility command unexpectedly has a relational graph")
	}
}

func TestDistinctOnRejectsNonprefixOrdering(t *testing.T) {
	for _, sql := range []string{
		`SELECT DISTINCT ON (x) x, y FROM (VALUES (1,2)) q(x,y) ORDER BY y, x`,
		`SELECT DISTINCT ON (x,y) x, y, z FROM (VALUES (1,2,3)) q(x,y,z) ORDER BY x, z`,
	} {
		_, err := bindSQL(t, sql, nil, nil)
		requireCode(t, err, "42P10")
	}
}

func TestOrderByRejectsDifferentExpressionsWithSameAlias(t *testing.T) {
	_, err := bindSQL(t, `SELECT x AS k,x+1 AS k FROM (VALUES(1)) t(x) ORDER BY k`, nil, nil)
	requireCode(t, err, "42702")
}

func TestGroupingRejectsInvalidTargetReferences(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"missing ordinal", `SELECT x FROM (VALUES(1)) t(x) GROUP BY 2`, "42P10"},
		{"ambiguous alias", `SELECT x AS k,y AS k FROM (VALUES(1,2)) t(x,y) GROUP BY k`, "42702"},
		{"aggregate alias", `SELECT sum(x) AS total FROM (VALUES(1)) t(x) GROUP BY total`, "42803"},
		{"window ordinal", `SELECT row_number() OVER() FROM (VALUES(1)) t(x) GROUP BY 1`, "42P20"},
		{"noninteger constant", `SELECT 1 GROUP BY 'x'`, "42601"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, nil, nil)
			requireCode(t, err, tc.code)
		})
	}
}
