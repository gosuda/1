package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestBindUnnestScalarAliasesAndOrdinality(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		columns   []string
	}{
		{"default", `SELECT unnest.unnest FROM unnest(ARRAY[1,NULL])`, []string{"unnest"}},
		{"table alias", `SELECT u.u FROM unnest(ARRAY[1]) AS u`, []string{"u"}},
		{"partial aliases", `SELECT u.v,u.ordinality FROM unnest(ARRAY[1]) WITH ORDINALITY AS u(v)`, []string{"v", "ordinality"}},
		{"rows from", `SELECT u.v,u.n FROM ROWS FROM(unnest(ARRAY[1])) WITH ORDINALITY AS u(v,n)`, []string{"v", "n"}},
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
				id := model.TypeID(23)
				if i == 1 {
					id = 20
				}
				if columns[i].Name != name || columns[i].Nullable != (i == 0) || columns[i].Layout.Type.ID != id {
					t.Errorf("column %d = %+v, want %s type %d", i, columns[i], name, id)
				}
			}
			_, source := boundUnnest(t, statement.Graph())
			if source.InputCount() != 0 || source.ExpressionCount() != 1 || source.UnnestOrdinality != (len(columns) == 2) {
				t.Fatalf("unnest contract = %+v", source)
			}
		})
	}
}

func TestBindUnnestRetainsConcreteParameterLayouts(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 12, Collation: 950}}
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	vector := model.TypeLayout{Type: model.Type{ID: 22, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	for _, element := range []model.TypeLayout{text, vector} {
		array := model.TypeLayout{Type: model.Type{ID: 90001, Storage: model.TypeArray, Modifier: -1, Collation: element.Type.Collation}, Children: []model.TypeLayout{element}}
		statement, err := bindSQL(t, `SELECT * FROM unnest($1)`, nil, []model.TypeLayout{array})
		if err != nil {
			t.Fatal(err)
		}
		requireArrayAggregateLayout(t, statement.Columns()[0].Layout, element)
		requireArrayAggregateLayout(t, statement.Parameters()[0], array)
		graph := statement.Graph()
		id, _ := boundUnnest(t, graph)
		argument, _ := graph.Expression(id, 0)
		scalar, _ := graph.Scalar(argument)
		if scalar.Kind != ir.Param || scalar.Parameter != 1 {
			t.Fatalf("source = %+v, want original typed parameter", scalar)
		}
		requireSubscriptsLayout(t, graph, scalar.Layout, array)
		array.Children[0].Type.Modifier = 100
		requireArrayAggregateLayout(t, statement.Columns()[0].Layout, element)
	}
}

func TestBindUnnestAnonymousRecordDefinitions(t *testing.T) {
	for _, sql := range []string{
		`SELECT u.a,u.b FROM unnest(ARRAY[ROW(1,'x'::text)]) AS u(a int,b text)`,
		`SELECT u.a,u.b FROM unnest(ARRAY[ROW(1,'x'::text),NULL::record]) AS u(a int,b text)`,
		`SELECT u.a,u.b FROM unnest(ARRAY[NULL::record,ROW(1,'x'::text)]) AS u(a int,b text)`,
		`SELECT u.a,u.b FROM unnest(ARRAY[NULL::record]) AS u(a int,b text)`,
		`SELECT u.a,u.b FROM unnest(ARRAY[]::record[]) AS u(a int,b text)`,
		`SELECT u.a,u.b FROM unnest(NULL::record[]) AS u(a int,b text)`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			columns := statement.Columns()
			if len(columns) != 2 {
				t.Fatalf("record schema = %+v", columns)
			}
			for i, want := range []struct {
				name string
				id   model.TypeID
			}{{"a", 23}, {"b", 25}} {
				if columns[i].Name != want.name || columns[i].Layout.Type.ID != want.id || !columns[i].Nullable {
					t.Errorf("record column %d = %+v, want %s type %d nullable", i, columns[i], want.name, want.id)
				}
			}
		})
	}
}

func TestBindUnnestRecordMismatchIsNotCoerced(t *testing.T) {
	statement, err := bindSQL(t, `SELECT * FROM ROWS FROM(unnest(ARRAY[ROW(1)]) AS(a bigint,b text)) WITH ORDINALITY AS u(x,y,n)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 3 {
		t.Fatalf("declared record schema = %+v", columns)
	}
	for i, want := range []struct {
		name     string
		id       model.TypeID
		nullable bool
	}{{"x", 20, true}, {"y", 25, true}, {"n", 20, false}} {
		if columns[i].Name != want.name || columns[i].Layout.Type.ID != want.id || columns[i].Nullable != want.nullable {
			t.Errorf("declared column %d = %+v, want %+v", i, columns[i], want)
		}
	}
	graph := statement.Graph()
	id, source := boundUnnest(t, graph)
	argument, _ := graph.Expression(id, 0)
	array, _ := graph.Scalar(argument)
	record, _ := graph.LayoutChild(array.Layout, 0)
	field, _ := graph.LayoutChild(record, 0)
	typ, _ := graph.Type(field)
	if array.Kind != ir.Array || graph.LayoutChildCount(record) != 1 || typ.ID != 23 || !source.UnnestOrdinality {
		t.Fatalf("actual record was coerced: array=%+v fields=%d field=%+v", array, graph.LayoutChildCount(record), typ)
	}
}

func TestBindUnnestLateralCaptureAndEffects(t *testing.T) {
	for _, lateral := range []string{"", "LATERAL "} {
		statement, err := bindSQL(t, `SELECT u.v FROM (VALUES(ARRAY[1])) a(v), `+lateral+`unnest(a.v) AS u(v)`, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		graph := statement.Graph()
		_, source := boundUnnest(t, graph)
		if graph.CaptureCount(source.Scope) != 1 || !statement.Columns()[0].Nullable {
			t.Fatalf("lateral source = %+v, columns = %+v", source, statement.Columns())
		}
	}
	statement, err := bindSQL(t, `SELECT * FROM unnest(ARRAY[random()])`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, source := boundUnnest(t, statement.Graph())
	if source.ExpressionCount() != 1 || source.Effects != (ir.Effects{MayError: true, Volatility: ir.Volatile}) {
		t.Fatalf("unnest evaluation contract = %+v", source)
	}
}

func TestBindUnnestRecordParameterRetainsSourceDescriptor(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 12, Collation: 950, Storage: model.TypeString}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: 7, Storage: model.TypeComposite}, Children: []model.TypeLayout{text}}
	array := model.TypeLayout{Type: model.Type{ID: 90001, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{record}}
	statement, err := bindSQL(t, `SELECT * FROM unnest($1) AS u(a varchar(3) COLLATE "C")`, nil, []model.TypeLayout{array})
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 1 || columns[0].Layout.Type.Modifier != 7 || columns[0].Layout.Type.Collation != 950 || !columns[0].Nullable {
		t.Fatalf("declared schema = %+v", columns)
	}
	requireArrayAggregateLayout(t, statement.Parameters()[0], array)
	graph := statement.Graph()
	id, _ := boundUnnest(t, graph)
	argument, _ := graph.Expression(id, 0)
	scalar, _ := graph.Scalar(argument)
	requireSubscriptsLayout(t, graph, scalar.Layout, array)
}

func TestBindUnnestDefinitionAndScopeErrors(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"unknown array", `SELECT * FROM unnest(NULL)`, "42725"},
		{"scalar source", `SELECT * FROM unnest(1)`, "42883"},
		{"missing definitions", `SELECT * FROM unnest(ARRAY[ROW(1)])`, "42601"},
		{"scalar definitions", `SELECT * FROM unnest(ARRAY[1]) AS u(a int)`, "42601"},
		{"top level ordinality definitions", `SELECT * FROM unnest(ARRAY[ROW(1)]) WITH ORDINALITY AS u(a int,n bigint)`, "42601"},
		{"duplicate definition", `SELECT * FROM unnest(ARRAY[ROW(1,2)]) AS u(a int,a int)`, "42701"},
		{"too many aliases", `SELECT * FROM unnest(ARRAY[1]) AS u(a,b)`, "42P10"},
		{"alias hides function", `SELECT unnest.unnest FROM unnest(ARRAY[1]) AS u`, "42P01"},
		{"right capture", `SELECT * FROM (VALUES(ARRAY[1])) a(v) RIGHT JOIN unnest(a.v) u(v) ON true`, "42P10"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, nil, nil)
			requireCode(t, err, tc.code)
		})
	}
}

func boundUnnest(t *testing.T, graph *ir.Graph) (ir.RelID, ir.Rel) {
	t.Helper()
	for i := range graph.RelCount() {
		id := ir.RelID(i + 1)
		node, _ := graph.Rel(id)
		if node.Kind == ir.Unnest {
			return id, node
		}
	}
	t.Fatal("missing Unnest relation")
	return 0, ir.Rel{}
}
