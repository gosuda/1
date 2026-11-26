package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestBindIntegerSeriesScopeAndOverloadErrors(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"ambiguous unknown bounds", `SELECT * FROM generate_series(NULL,NULL)`, "42725"},
		{"wrong arity", `SELECT * FROM generate_series(1)`, "42883"},
		{"alias hides function name", `SELECT generate_series.generate_series FROM generate_series(1,3) AS g`, "42P01"},
		{"right lateral capture", `SELECT * FROM (VALUES(3)) l(n) RIGHT JOIN generate_series(1,l.n) AS g(v) ON true`, "42P10"},
		{"full lateral capture", `SELECT * FROM (VALUES(3)) l(n) FULL JOIN LATERAL generate_series(1,l.n) AS g(v) ON true`, "42P10"},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := bindSQL(t, tc.sql, nil, nil); requireCode(t, err, tc.code) })
	}
}

func TestBindSubscriptsAliasesAndOrdinality(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		columns   []string
	}{
		{"default name", `SELECT generate_subscripts.generate_subscripts FROM generate_subscripts(ARRAY[4,5],1)`, []string{"generate_subscripts"}},
		{"table alias", `SELECT g.g FROM generate_subscripts(ARRAY[4,5],1) AS g`, []string{"g"}},
		{"column aliases", `SELECT s.position,s.sequence FROM generate_subscripts(ARRAY[4,5],1,true) WITH ORDINALITY AS s(position,sequence)`, []string{"position", "sequence"}},
		{"partial aliases", `SELECT s.position,s.ordinality FROM generate_subscripts(ARRAY[4,5],1) WITH ORDINALITY AS s(position)`, []string{"position", "ordinality"}},
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
				want := model.Type{ID: id, Modifier: -1, Storage: model.TypeInt64}
				if columns[i].Name != name || columns[i].Nullable || columns[i].Layout.Type != want || len(columns[i].Layout.Children) != 0 {
					t.Errorf("column %d = %+v, want non-null %s with type %+v", i, columns[i], name, want)
				}
			}
			_, source := boundSubscripts(t, statement.Graph())
			if source.OutputCount() != uint32(len(tc.columns)) || source.InputCount() != 0 {
				t.Fatalf("subscripts source inputs=%d outputs=%d", source.InputCount(), source.OutputCount())
			}
		})
	}
}

func TestBindSubscriptsPreservesArrayLayoutsAndArgumentOrder(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 12, Collation: 950}}
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: 655366}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: 7}, Children: []model.TypeLayout{text, numeric}}
	for _, array := range []model.TypeLayout{
		{Type: model.Type{ID: 1015, Storage: model.TypeArray, Modifier: 12, Collation: 950}, Children: []model.TypeLayout{text}},
		{Type: model.Type{ID: 2287, Storage: model.TypeArray, Modifier: 7}, Children: []model.TypeLayout{record}},
	} {
		statement, err := bindSQL(t, `SELECT * FROM generate_subscripts($1,$3,$2) WITH ORDINALITY`, nil, []model.TypeLayout{array})
		if err != nil {
			t.Fatal(err)
		}
		parameters := statement.Parameters()
		if len(parameters) != 3 {
			t.Fatalf("parameters = %+v, want array, boolean, int4", parameters)
		}
		want := []model.TypeLayout{array, {Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}, {Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}}
		for i := range want {
			requireArrayAggregateLayout(t, parameters[i], want[i])
		}
		graph := statement.Graph()
		id, source := boundSubscripts(t, graph)
		if source.ExpressionCount() != 3 {
			t.Fatalf("subscripts arguments = %d, want three original arguments", source.ExpressionCount())
		}
		for i, parameter := range []uint32{1, 3, 2} {
			argument, _ := graph.Expression(id, uint32(i))
			expr, _ := graph.Scalar(argument)
			if expr.Kind != ir.Param || expr.Parameter != parameter || !expr.Nullable {
				t.Fatalf("argument %d = %+v, want nullable parameter $%d", i, expr, parameter)
			}
			requireSubscriptsLayout(t, graph, expr.Layout, want[parameter-1])
		}
	}
}

func TestBindSubscriptsCapturesLateralArguments(t *testing.T) {
	for _, lateral := range []string{"", "LATERAL "} {
		statement, err := bindSQL(t, `SELECT s.n FROM (VALUES(ARRAY[4,5],1,true)) a(v,d,r), `+lateral+`generate_subscripts(a.v,a.d,a.r) AS s(n)`, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		graph := statement.Graph()
		id, source := boundSubscripts(t, graph)
		if graph.CaptureCount(source.Scope) != 3 || source.ExpressionCount() != 3 {
			t.Fatalf("subscripts captures=%d arguments=%d, want three", graph.CaptureCount(source.Scope), source.ExpressionCount())
		}
		for i := range uint32(3) {
			argument, _ := graph.Expression(id, i)
			expr, _ := graph.Scalar(argument)
			capture, _ := graph.Capture(source.Scope, i)
			if expr.Kind != ir.Slot || expr.Slot != capture {
				t.Errorf("argument %d = %+v, want captured slot %d", i, expr, capture)
			}
		}
	}
}

func TestBindSubscriptsRetainsEagerArgumentsAfterNull(t *testing.T) {
	statement, err := bindSQL(t, `SELECT * FROM generate_subscripts(NULL::int4[],1/0,random()>0)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	id, source := boundSubscripts(t, graph)
	if source.ExpressionCount() != 3 || source.Effects != (ir.Effects{MayError: true, Volatility: ir.Volatile}) {
		t.Fatalf("subscripts arguments=%d effects=%+v", source.ExpressionCount(), source.Effects)
	}
	for i, kind := range []ir.ScalarKind{ir.Const, ir.Call, ir.Call} {
		argument, _ := graph.Expression(id, uint32(i))
		expr, _ := graph.Scalar(argument)
		if expr.Kind != kind {
			t.Errorf("argument %d kind=%v, want original %v", i, expr.Kind, kind)
		}
	}
}

func TestBindSubscriptsScopeAndOverloadErrors(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"unknown array", `SELECT * FROM generate_subscripts(NULL,1)`, "42804"},
		{"scalar source", `SELECT * FROM generate_subscripts(1,1)`, "42883"},
		{"wrong arity", `SELECT * FROM generate_subscripts(ARRAY[1])`, "42883"},
		{"nonimplicit dimension", `SELECT * FROM generate_subscripts(ARRAY[1],1::int8)`, "42883"},
		{"nonboolean reverse", `SELECT * FROM generate_subscripts(ARRAY[1],1,1)`, "42883"},
		{"alias hides function name", `SELECT generate_subscripts.generate_subscripts FROM generate_subscripts(ARRAY[1],1) AS g`, "42P01"},
		{"right lateral capture", `SELECT * FROM (VALUES(ARRAY[1])) l(a) RIGHT JOIN generate_subscripts(l.a,1) AS g(v) ON true`, "42P10"},
		{"full lateral capture", `SELECT * FROM (VALUES(ARRAY[1])) l(a) FULL JOIN LATERAL generate_subscripts(l.a,1) AS g(v) ON true`, "42P10"},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := bindSQL(t, tc.sql, nil, nil); requireCode(t, err, tc.code) })
	}
}

func boundSubscripts(t *testing.T, graph *ir.Graph) (ir.RelID, ir.Rel) {
	t.Helper()
	for i := range graph.RelCount() {
		id := ir.RelID(i + 1)
		source, _ := graph.Rel(id)
		if source.Kind == ir.GenerateSubscripts {
			return id, source
		}
	}
	t.Fatal("missing GenerateSubscripts relation")
	return 0, ir.Rel{}
}

func requireSubscriptsLayout(t *testing.T, graph *ir.Graph, id ir.LayoutID, want model.TypeLayout) {
	t.Helper()
	got, ok := graph.Type(id)
	if !ok || got != want.Type || graph.LayoutChildCount(id) != uint32(len(want.Children)) {
		t.Fatalf("layout %d = %+v with %d children, want %+v", id, got, graph.LayoutChildCount(id), want)
	}
	for i, child := range want.Children {
		childID, _ := graph.LayoutChild(id, uint32(i))
		requireSubscriptsLayout(t, graph, childID, child)
	}
}

func TestBindSubscriptsCoercesDimensionWithoutChangingParameterType(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 21, Storage: model.TypeInt64, Modifier: -1}}
	statement, err := bindSQL(t, `SELECT * FROM generate_subscripts(ARRAY[1],$1)`, nil, []model.TypeLayout{integer})
	if err != nil {
		t.Fatal(err)
	}
	parameters := statement.Parameters()
	if len(parameters) != 1 {
		t.Fatalf("parameters = %+v, want declared int2", parameters)
	}
	requireArrayAggregateLayout(t, parameters[0], integer)
	graph := statement.Graph()
	id, _ := boundSubscripts(t, graph)
	argument, _ := graph.Expression(id, 1)
	dimension, _ := graph.Scalar(argument)
	typ, _ := graph.Type(dimension.Layout)
	if typ != (model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}) || !dimension.Nullable {
		t.Fatalf("dimension = %+v, type %+v; want nullable int4", dimension, typ)
	}
}
