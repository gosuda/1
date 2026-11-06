package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestArrayAggregateSelectsInputOverload(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		sql       string
		function  model.FunctionID
		array     model.TypeID
		element   model.TypeID
		collation model.CollationID
	}{
		{"integer", `SELECT array_agg(1)`, 2335, 1007, 23, 0},
		{"numeric literal", `SELECT array_agg(1.25)`, 2335, 1231, 1700, 0},
		{"collated text", `SELECT array_agg('value'::text COLLATE "C")`, 2335, 1009, 25, 950},
		{"typed null", `SELECT array_agg(NULL::int4)`, 2335, 1007, 23, 0},
		{"integer array", `SELECT array_agg(ARRAY[1,2])`, 4053, 1007, 23, 0},
		{"null array", `SELECT array_agg(NULL::int4[])`, 4053, 1007, 23, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			call := boundArrayAggregate(t, statement.Graph())
			if call.Function != scenario.function {
				t.Fatalf("aggregate function = %d, want %d", call.Function, scenario.function)
			}
			columns := statement.Columns()
			if len(columns) != 1 {
				t.Fatalf("result column count = %d, want 1", len(columns))
			}
			layout := columns[0].Layout
			if layout.Type.ID != scenario.array || layout.Type.Storage != model.TypeArray || layout.Type.Modifier != -1 || layout.Type.Collation != scenario.collation {
				t.Fatalf("array result = %+v, want type %d, typmod -1, collation %d", layout.Type, scenario.array, scenario.collation)
			}
			if len(layout.Children) != 1 || layout.Children[0].Type.ID != scenario.element || layout.Children[0].Type.Collation != scenario.collation {
				t.Fatalf("array element = %+v, want type %d, collation %d", layout.Children, scenario.element, scenario.collation)
			}
			if !columns[0].Nullable {
				t.Fatal("array aggregate result must allow NULL for empty input")
			}
		})
	}
}

func TestArrayAggregatePreservesParameterElementLayouts(t *testing.T) {
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: 655366}}
	text := model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 12, Collation: 950}}
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: 7}, Children: []model.TypeLayout{text, integers, numeric}}
	records := model.TypeLayout{Type: model.Type{ID: 2287, Storage: model.TypeArray, Modifier: 7}, Children: []model.TypeLayout{record}}
	texts := model.TypeLayout{Type: model.Type{ID: 1015, Storage: model.TypeArray, Modifier: 12, Collation: 950}, Children: []model.TypeLayout{text}}
	for _, scenario := range []struct {
		name     string
		input    model.TypeLayout
		function model.FunctionID
		array    model.TypeID
		element  model.TypeLayout
	}{
		{"numeric scalar", numeric, 2335, 1231, numeric},
		{"record scalar", record, 2335, 2287, record},
		{"record array", records, 4053, 2287, record},
		{"collated array", texts, 4053, 1015, text},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, `SELECT array_agg($1) FILTER (WHERE $1 IS NULL)`, nil, []model.TypeLayout{scenario.input})
			if err != nil {
				t.Fatal(err)
			}
			call := boundArrayAggregate(t, statement.Graph())
			if call.Function != scenario.function {
				t.Fatalf("aggregate function = %d, want %d", call.Function, scenario.function)
			}
			parameters := statement.Parameters()
			if len(parameters) != 1 {
				t.Fatalf("parameter count = %d, want 1", len(parameters))
			}
			requireArrayAggregateLayout(t, parameters[0], scenario.input)
			columns := statement.Columns()
			if len(columns) != 1 {
				t.Fatalf("result column count = %d, want 1", len(columns))
			}
			want := model.TypeLayout{
				Type:     model.Type{ID: scenario.array, Storage: model.TypeArray, Modifier: -1, Collation: scenario.input.Type.Collation},
				Children: []model.TypeLayout{scenario.element},
			}
			requireArrayAggregateLayout(t, columns[0].Layout, want)
		})
	}
}

func TestArrayAggregateRejectsAmbiguousUnknownInput(t *testing.T) {
	for _, sql := range []string{
		`SELECT array_agg(NULL)`,
		`SELECT array_agg('value')`,
		`SELECT array_agg($1)`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			requireCode(t, err, "42725")
			if statement != nil {
				t.Fatal("ambiguous polymorphic input returned a bound statement")
			}
		})
	}
}

func boundArrayAggregate(t *testing.T, graph *ir.Graph) ir.AggregateCall {
	t.Helper()
	for id := ir.RelID(1); uint32(id) <= graph.RelCount(); id++ {
		if call, ok := graph.Aggregate(id, 0); ok {
			return call
		}
	}
	t.Fatal("missing aggregate call")
	return ir.AggregateCall{}
}

func requireArrayAggregateLayout(t *testing.T, got, want model.TypeLayout) {
	t.Helper()
	if got.Type != want.Type {
		t.Errorf("layout type = %+v, want %+v", got.Type, want.Type)
	}
	if len(got.Children) != len(want.Children) {
		t.Fatalf("type %d child count = %d, want %d", want.Type.ID, len(got.Children), len(want.Children))
	}
	for i := range want.Children {
		requireArrayAggregateLayout(t, got.Children[i], want.Children[i])
	}
}
