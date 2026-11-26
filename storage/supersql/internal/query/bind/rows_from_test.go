package bind_test

import (
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestBindMultiUnnestPreservesHeterogeneousLayouts(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 21, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 12, Storage: model.TypeString, Collation: 951}}
	vector := model.TypeLayout{Type: model.Type{ID: 22, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	arrays := []model.TypeLayout{
		{Type: model.Type{ID: 1015, Modifier: 12, Storage: model.TypeArray, Collation: 951}, Children: []model.TypeLayout{text}},
		{Type: model.Type{ID: 1006, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{vector}},
	}
	statement, err := bindSQL(t, `SELECT * FROM unnest($1,$2) WITH ORDINALITY AS u(label,numbers,sequence)`, nil, arrays)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 3 {
		t.Fatalf("columns = %+v, want label, numbers, sequence", columns)
	}
	for i, want := range []struct {
		name   string
		layout model.TypeLayout
	}{{"label", text}, {"numbers", vector}, {"sequence", model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}}} {
		if columns[i].Name != want.name || columns[i].Nullable != (i < 2) {
			t.Errorf("column %d = %+v, want %s nullable=%t", i, columns[i], want.name, i < 2)
		}
		requireArrayAggregateLayout(t, columns[i].Layout, want.layout)
	}
	graph := statement.Graph()
	id, source := boundRowsFrom(t, graph)
	if source.InputCount() != 2 || !source.RowsFromOrdinality || source.ExpressionCount() != 0 || source.Predicate != 0 {
		t.Fatalf("zipper contract = %+v", source)
	}
	for i, array := range arrays {
		childID, _ := graph.Input(id, uint32(i))
		child, _ := graph.Rel(childID)
		argumentID, _ := graph.Expression(childID, 0)
		argument, _ := graph.Scalar(argumentID)
		if child.Kind != ir.Unnest || child.UnnestOrdinality || child.OutputCount() != 1 || argument.Kind != ir.Param || argument.Parameter != uint32(i+1) {
			t.Fatalf("input %d = %+v, argument = %+v", i, child, argument)
		}
		requireSubscriptsLayout(t, graph, argument.Layout, array)
		requireArrayAggregateLayout(t, statement.Parameters()[i], array)
		childSlot, _ := graph.Output(childID, 0)
		outputSlot, _ := graph.Output(id, uint32(i))
		childInfo, _ := graph.Slot(childSlot)
		outputInfo, _ := graph.Slot(outputSlot)
		if childSlot == outputSlot || !outputInfo.Nullable {
			t.Errorf("output %d = %+v on %d, child = %+v on %d; want distinct nullable slot with identical layout", i, outputInfo, outputSlot, childInfo, childSlot)
		}
		requireSubscriptsLayout(t, graph, childInfo.Layout, array.Children[0])
		requireSubscriptsLayout(t, graph, outputInfo.Layout, array.Children[0])
	}
}

func TestBindRowsFromPadsNonnullableGenerators(t *testing.T) {
	statement, err := bindSQL(t, `SELECT * FROM ROWS FROM(generate_series(2::bigint,4::bigint),generate_subscripts(ARRAY[8,9],1),generate_series(6,7)) WITH ORDINALITY`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	id, source := boundRowsFrom(t, graph)
	if source.InputCount() != 3 || source.OutputCount() != 4 || !source.RowsFromOrdinality || source.Effects != (ir.Effects{MayError: true}) {
		t.Fatalf("zipper contract = %+v", source)
	}
	for i, kind := range []ir.RelKind{ir.GenerateSeries, ir.GenerateSubscripts, ir.GenerateSeries} {
		childID, _ := graph.Input(id, uint32(i))
		child, _ := graph.Rel(childID)
		childSlot, _ := graph.Output(childID, 0)
		childInfo, _ := graph.Slot(childSlot)
		outputSlot, _ := graph.Output(id, uint32(i))
		outputInfo, _ := graph.Slot(outputSlot)
		wrongChild := child.Kind != kind || child.OutputCount() != 1 || childInfo.Nullable
		wrongPadding := childSlot == outputSlot || !outputInfo.Nullable || outputInfo.Layout != childInfo.Layout
		if wrongChild || wrongPadding {
			t.Errorf("input %d kind=%v, child=%+v, output=%+v; want %v with nullable padding output", i, child.Kind, childInfo, outputInfo, kind)
		}
	}
	columns := statement.Columns()
	for i, typ := range []model.TypeID{20, 23, 23, 20} {
		if columns[i].Layout.Type.ID != typ || columns[i].Nullable != (i < 3) {
			t.Errorf("column %d = %+v, want type %d nullable=%t", i, columns[i], typ, i < 3)
		}
	}
}

func TestBindRowsFromUsesOneNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		columns   []string
	}{
		{"first function namespace", `SELECT generate_series.generate_series,generate_series.unnest FROM ROWS FROM(generate_series(1,2),unnest(ARRAY['x']))`, []string{"generate_series", "unnest"}},
		{"table alias preserves names", `SELECT r.generate_series,r.unnest FROM ROWS FROM(generate_series(1,2),unnest(ARRAY['x'])) r`, []string{"generate_series", "unnest"}},
		{"partial aliases span functions", `SELECT r.x,r.unnest,r.ordinality FROM ROWS FROM(generate_series(1,2),unnest(ARRAY['x'])) WITH ORDINALITY r(x)`, []string{"x", "unnest", "ordinality"}},
		{"expanded duplicate names", `SELECT u.* FROM unnest(ARRAY[1],ARRAY['x']) u`, []string{"unnest", "unnest"}},
		{"single rows from uses alias", `SELECT g.g FROM ROWS FROM(generate_series(1,2)) g`, []string{"g"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			columns := statement.Columns()
			if len(columns) != len(tc.columns) {
				t.Fatalf("columns = %+v, want %v", columns, tc.columns)
			}
			for i, name := range tc.columns {
				if columns[i].Name != name {
					t.Errorf("column %d name = %q, want %q", i, columns[i].Name, name)
				}
			}
		})
	}
}

func TestBindRowsFromKeepsPerFunctionRecordDefinitions(t *testing.T) {
	statement, err := bindSQL(t, `SELECT * FROM ROWS FROM(unnest(ARRAY[ROW(1)]) AS(a bigint,b varchar(3) COLLATE "C"),generate_series(1,2),unnest(ARRAY[ROW(true)]) AS(c boolean)) WITH ORDINALITY AS r(x)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 5 {
		t.Fatalf("declared columns = %+v, want five", columns)
	}
	for i, want := range []struct {
		name string
		id   model.TypeID
	}{{"x", 20}, {"b", 1043}, {"generate_series", 23}, {"c", 16}, {"ordinality", 20}} {
		if columns[i].Name != want.name || columns[i].Layout.Type.ID != want.id || columns[i].Nullable != (i < 4) {
			t.Errorf("column %d = %+v, want %s type %d nullable=%t", i, columns[i], want.name, want.id, i < 4)
		}
	}
	if columns[1].Layout.Type.Modifier != 7 || columns[1].Layout.Type.Collation != 950 {
		t.Errorf("record field layout = %+v, want varchar(3) COLLATE C", columns[1].Layout)
	}
	graph := statement.Graph()
	id, _ := boundRowsFrom(t, graph)
	first, _ := graph.Input(id, 0)
	argument, _ := graph.Expression(first, 0)
	array, _ := graph.Scalar(argument)
	record, _ := graph.LayoutChild(array.Layout, 0)
	field, _ := graph.LayoutChild(record, 0)
	typ, _ := graph.Type(field)
	if graph.LayoutChildCount(record) != 1 || typ.ID != 23 {
		t.Fatalf("source record changed to match declaration: fields=%d type=%+v", graph.LayoutChildCount(record), typ)
	}
}

func TestBindRowsFromRetainsZeroColumnInputs(t *testing.T) {
	snapshot, array := unnestNamedSnapshot(t, nil, nil)
	for _, tc := range []struct {
		sql     string
		outputs uint32
	}{
		{`SELECT * FROM ROWS FROM(unnest($1),generate_series(1,2))`, 1},
		{`SELECT * FROM unnest($1,$1)`, 0},
		{`SELECT * FROM unnest($1,$1) WITH ORDINALITY`, 1},
	} {
		t.Run(tc.sql, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, snapshot, []model.TypeLayout{array})
			if err != nil {
				t.Fatal(err)
			}
			graph := statement.Graph()
			id, source := boundRowsFrom(t, graph)
			first, _ := graph.Input(id, 0)
			child, _ := graph.Rel(first)
			wrongChild := child.Kind != ir.Unnest || child.OutputCount() != 0 || child.ExpressionCount() != 1
			wrongOutput := source.OutputCount() != tc.outputs || uint32(len(statement.Columns())) != tc.outputs
			if source.InputCount() != 2 || wrongChild || wrongOutput {
				t.Fatalf("zero-field source = %+v, zipper = %+v, columns = %+v", child, source, statement.Columns())
			}
		})
	}
}

func TestBindRowsFromCapturesOuterButNotSiblingColumns(t *testing.T) {
	for _, lateral := range []string{"", "LATERAL "} {
		statement, err := bindSQL(t, `SELECT generate_series.generate_series,generate_series.unnest FROM (VALUES(2,ARRAY[8])) a(n,v), `+lateral+`ROWS FROM(generate_series(1,a.n),unnest(a.v))`, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		graph := statement.Graph()
		id, source := boundRowsFrom(t, graph)
		if graph.CaptureCount(source.Scope) != 2 {
			t.Fatalf("capture count = %d, want both outer columns", graph.CaptureCount(source.Scope))
		}
		for i, index := range []uint32{1, 0} {
			child, _ := graph.Input(id, uint32(i))
			argumentID, _ := graph.Expression(child, index)
			argument, _ := graph.Scalar(argumentID)
			capture, _ := graph.Capture(source.Scope, uint32(i))
			if argument.Kind != ir.Slot || argument.Slot != capture {
				t.Errorf("input %d argument = %+v, want lexical capture %d", i, argument, capture)
			}
		}
	}
	statement, err := bindSQL(t, `SELECT r.x,r.y FROM (VALUES(2)) a(generate_series), ROWS FROM(generate_series(1,1),generate_series(1,generate_series)) AS r(x,y)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, source := boundRowsFrom(t, statement.Graph())
	if statement.Graph().CaptureCount(source.Scope) != 1 {
		t.Fatal("second function did not resolve the outer column sharing its sibling's name")
	}
}

func TestBindMultiUnnestDoesNotDuplicateEffectsOrMutateAST(t *testing.T) {
	const sql = `SELECT * FROM unnest(ARRAY[random()],ARRAY[random()])`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	original := proto.Clone(parsed.Stmts[0])
	statement, err := bind.Bind(t.Context(), parsed.Stmts[0], nil, bindingOptions(sql), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(original, parsed.Stmts[0]) {
		t.Fatal("binding rewrote the caller's parsed statement")
	}
	graph := statement.Graph()
	_, source := boundRowsFrom(t, graph)
	if source.Effects != (ir.Effects{MayError: true, Volatility: ir.Volatile}) {
		t.Fatalf("zipper effects = %+v, want volatile and may-error", source.Effects)
	}
	calls := 0
	for i := range graph.ScalarCount() {
		scalar, _ := graph.Scalar(ir.ScalarID(i + 1))
		if scalar.Kind == ir.Call {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("effectful calls = %d, want the two original random calls", calls)
	}
}

func TestBindMultiUnnestRewriteForcesBuiltinButPreservesQualifiedResolution(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	array := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 12, OID: 90012, Version: 1, State: model.StatePublic,
		Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "unnest", Signature: "int4[],int4[]"},
		Routine: new(compiledRoutine(t, snapshot, "SELECT 1", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: []model.RoutineParameter{{Name: "a", Layout: array, Mode: "in"}, {Name: "b", Layout: array, Mode: "in"}}, Result: integer, Language: "sql", Volatility: "immutable"})),
	}))
	const sql = `SELECT * FROM unnest($1,$2)`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bindingOptions(sql)
	options.SearchPath = []string{"public", "pg_catalog"}
	statement, err := bind.Bind(t.Context(), parsed.Stmts[0], snapshot, options, []model.TypeLayout{array, array})
	if err != nil {
		t.Fatal(err)
	}
	_, source := boundRowsFrom(t, statement.Graph())
	if source.InputCount() != 2 {
		t.Fatalf("builtin expansion inputs = %d, want two", source.InputCount())
	}
	for _, dep := range statement.Dependencies() {
		if dep.Object == 12 {
			t.Fatal("FROM-only builtin expansion resolved the user overload")
		}
	}
	statement, err = bindSQL(t, `SELECT public.unnest($1,$2)`, snapshot, []model.TypeLayout{array, array})
	if err != nil {
		t.Fatal(err)
	}
	if columns := statement.Columns(); len(columns) != 1 || columns[0].Layout.Type.ID != 23 {
		t.Fatalf("qualified user result = %+v, want int4", columns)
	}
	statement, err = bindSQL(t, `SELECT * FROM public.unnest($1,$2)`, snapshot, []model.TypeLayout{array, array})
	if err != nil {
		t.Fatal(err)
	}
	if columns := statement.Columns(); len(columns) != 1 || columns[0].Layout.Type.ID != 23 || columns[0].Name != "unnest" {
		t.Fatalf("qualified user table function result = %+v, want int4 column unnest", columns)
	}
	found := false
	for _, dep := range statement.Dependencies() {
		if dep.Object == 12 {
			found = true
		}
	}
	if !found {
		t.Fatal("qualified user table function did not resolve the user overload")
	}
}

func TestBindRowsFromRejectsInvalidDefinitionsAndNamespaces(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"duplicate output reference", `SELECT r.generate_series FROM ROWS FROM(generate_series(1,2),generate_series(3,4)) r`, "42702"},
		{"multi alias is not column name", `SELECT r.r FROM ROWS FROM(generate_series(1,2),unnest(ARRAY[1])) r`, "42703"},
		{"second function has no namespace", `SELECT unnest.unnest FROM ROWS FROM(generate_series(1,2),unnest(ARRAY[1]))`, "42P01"},
		{"alias hides first namespace", `SELECT generate_series.generate_series FROM ROWS FROM(generate_series(1,2),unnest(ARRAY[1])) r`, "42P01"},
		{"sibling output is invisible", `SELECT * FROM ROWS FROM(generate_series(1,2),generate_series(1,generate_series))`, "42703"},
		{"too many aliases", `SELECT * FROM unnest(ARRAY[1],ARRAY[2]) u(a,b,c)`, "42P10"},
		{"missing record definition", `SELECT * FROM ROWS FROM(unnest(ARRAY[ROW(1)]),generate_series(1,2))`, "42601"},
		{"top level record definitions", `SELECT * FROM ROWS FROM(unnest(ARRAY[ROW(1)]),generate_series(1,2)) AS r(a int,b int)`, "42601"},
		{"expanded top level definitions", `SELECT * FROM unnest(ARRAY[ROW(1)],ARRAY[ROW(2)]) AS r(a int,b int)`, "42601"},
		{"per function definitions forbid expansion", `SELECT * FROM ROWS FROM(unnest(ARRAY[1],ARRAY[2]) AS(a int,b int))`, "42883"},
		{"scalar record definitions", `SELECT * FROM ROWS FROM(generate_series(1,2) AS(a int),unnest(ARRAY[1]))`, "42601"},
		{"duplicate record definition", `SELECT * FROM ROWS FROM(unnest(ARRAY[ROW(1,2)]) AS(a int,a int),generate_series(1,2))`, "42701"},
		{"qualified unnest does not expand", `SELECT * FROM pg_catalog.unnest(ARRAY[1],ARRAY[2])`, "42883"},
		{"scalar unnest does not expand", `SELECT unnest(ARRAY[1],ARRAY[2])`, "42883"},
		{"aggregate decoration", `SELECT * FROM unnest(DISTINCT ARRAY[1],ARRAY[2])`, "42809"},
		{"order decoration", `SELECT * FROM unnest(ARRAY[1],ARRAY[2] ORDER BY 1)`, "42809"},
		{"right lateral capture", `SELECT * FROM (VALUES(2)) a(n) RIGHT JOIN ROWS FROM(generate_series(1,a.n),unnest(ARRAY[1])) r ON true`, "42P10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, nil, nil)
			requireCode(t, err, tc.code)
		})
	}
}

func TestBindRowsFromBoundsExpandedInputCount(t *testing.T) {
	const sql = `SELECT * FROM ROWS FROM(unnest(ARRAY[1],ARRAY[2]),unnest(ARRAY[3],ARRAY[4]))`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	options := bindingOptions(sql)
	options.Limits.MaxOperands = 3
	_, err = bind.Bind(t.Context(), parsed.Stmts[0], nil, options, nil)
	requireCode(t, err, "54000")
}

func boundRowsFrom(t *testing.T, graph *ir.Graph) (ir.RelID, ir.Rel) {
	t.Helper()
	for i := range graph.RelCount() {
		id := ir.RelID(i + 1)
		node, _ := graph.Rel(id)
		if node.Kind == ir.RowsFrom {
			return id, node
		}
	}
	t.Fatal("missing RowsFrom relation")
	return 0, ir.Rel{}
}
