package bind_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestArrayConstructorBindsSubarrays(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	for _, scenario := range []struct {
		name string
		sql  string
		kind ir.ScalarKind
	}{
		{"bracket subarrays", `SELECT ARRAY[[1,2],[3,4]]`, ir.ArrayStack},
		{"constructor subarrays", `SELECT ARRAY[ARRAY[1,2],ARRAY[3,4]]`, ir.ArrayStack},
		{"null subarrays", `SELECT ARRAY[NULL::int4[],NULL::int4[]]`, ir.ArrayStack},
		{"mixed null and nonempty", `SELECT ARRAY[NULL,ARRAY[1,2]]`, ir.ArrayStack},
		{"unequal shapes bind before execution", `SELECT ARRAY[[1],[2,3]]`, ir.ArrayStack},
		{"scalar elements", `SELECT ARRAY[NULL,1]`, ir.Array},
		{"typed scalar empty", `SELECT ARRAY[]::int4[]`, ir.Array},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireArrayConstructor(t, statement, scenario.kind, integers)
		})
	}
}

func TestArrayConstructorPropagatesExplicitTypeIntoNestedLiterals(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		sql     string
		array   model.TypeID
		element model.Type
	}{
		{"bracket empties", `SELECT ARRAY[[],[]]::int4[]`, 1007, model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}},
		{"constructor empties", `SELECT ARRAY[ARRAY[],ARRAY[]]::int4[]`, 1007, model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}},
		{"deep empties", `SELECT ARRAY[[[]],[[]]]::int8[]`, 1016, model.Type{ID: 20, Storage: model.TypeInt64, Modifier: -1}},
		{"explicit integer narrowing", `SELECT ARRAY[[1],[2]]::int2[]`, 1005, model.Type{ID: 21, Storage: model.TypeInt64, Modifier: -1}},
		{"explicit boolean conversion", `SELECT ARRAY[[0],[1]]::boolean[]`, 1000, model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}},
		{"numeric modifier", `SELECT ARRAY[[1.25],[2.5]]::numeric(6,2)[]`, 1231, model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: 393222}},
		{"modified empties", `SELECT ARRAY[[],[]]::numeric(6,2)[]`, 1231, model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: 393222}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			want := model.TypeLayout{Type: model.Type{ID: scenario.array, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{{Type: scenario.element}}}
			requireArrayConstructor(t, statement, ir.ArrayStack, want)
		})
	}
}

func TestArrayConstructorPreservesParameterElementLayouts(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	text := model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 12, Collation: 950}}
	texts := model.TypeLayout{Type: model.Type{ID: 1015, Storage: model.TypeArray, Modifier: 12, Collation: 950}, Children: []model.TypeLayout{text}}
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: 655366}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: 7}, Children: []model.TypeLayout{text, integers, numeric}}
	records := model.TypeLayout{Type: model.Type{ID: 2287, Storage: model.TypeArray, Modifier: 7}, Children: []model.TypeLayout{record}}
	for _, scenario := range []struct {
		name  string
		input model.TypeLayout
	}{
		{"integers", integers},
		{"collated modified text", texts},
		{"records with nested fields", records},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, `SELECT ARRAY[$1,$2]`, nil, []model.TypeLayout{scenario.input, scenario.input})
			if err != nil {
				t.Fatal(err)
			}
			requireArrayConstructor(t, statement, ir.ArrayStack, scenario.input)
			parameters := statement.Parameters()
			if len(parameters) != 2 {
				t.Fatalf("parameter count = %d, want 2", len(parameters))
			}
			for _, parameter := range parameters {
				requireArrayAggregateLayout(t, parameter, scenario.input)
			}
		})
	}
}

func TestArrayConstructorInfersParametersFromOperandContext(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	for _, scenario := range []struct {
		name      string
		sql       string
		kind      ir.ScalarKind
		parameter model.TypeLayout
	}{
		{"subarray context", `SELECT ARRAY[$1,ARRAY[1,2]]`, ir.ArrayStack, integers},
		{"explicit nested context", `SELECT ARRAY[$1,ARRAY[]]::int4[]`, ir.ArrayStack, integers},
		{"explicit scalar context", `SELECT ARRAY[$1]::int4[]`, ir.Array, integer},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			requireArrayConstructor(t, statement, scenario.kind, integers)
			parameters := statement.Parameters()
			if len(parameters) != 1 {
				t.Fatalf("parameter count = %d, want 1", len(parameters))
			}
			requireArrayAggregateLayout(t, parameters[0], scenario.parameter)
		})
	}
}

func TestArrayConstructorKeepsVectorsAsScalarElements(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		vector  model.TypeID
		array   model.TypeID
		element model.TypeID
	}{
		{"int2vector", 22, 1006, 21},
		{"oidvector", 30, 1013, 26},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			element := model.TypeLayout{Type: model.Type{ID: scenario.element, Storage: model.TypeInt64, Modifier: -1}}
			vector := model.TypeLayout{Type: model.Type{ID: scenario.vector, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{element}}
			statement, err := bindSQL(t, `SELECT ARRAY[$1]`, nil, []model.TypeLayout{vector})
			if err != nil {
				t.Fatal(err)
			}
			want := model.TypeLayout{Type: model.Type{ID: scenario.array, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{vector}}
			requireArrayConstructor(t, statement, ir.Array, want)
		})
	}
}

func TestArrayStackRetainsEagerDependenciesAndCaptures(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 12, OID: 90012, Version: 2, State: model.StatePublic,
		Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "changing"},
		Routine: new(compiledRoutine(t, snapshot, "SELECT 2", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: integer, Language: "sql", Volatility: "volatile"})),
	}))
	statement, err := bindSQL(t, `SELECT (SELECT ARRAY[NULL::int4[],ARRAY[t."X",public.changing()]]) FROM "Mixed" t`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, dependency := range []model.Dependency{{Object: 11, Version: 3, Column: 1}, {Object: 12, Version: 2}} {
		if !slices.Contains(statement.Dependencies(), dependency) {
			t.Errorf("dependencies = %+v, missing %+v", statement.Dependencies(), dependency)
		}
	}
	if effects := statement.Effects(); effects.Volatility != ir.Volatile || !effects.MayError {
		t.Errorf("statement effects = %+v, want volatile and may-error", effects)
	}
	graph := statement.Graph()
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		scalar, _ := graph.Scalar(id)
		if scalar.Kind != ir.ArrayStack {
			continue
		}
		if scalar.Effects.Volatility != ir.Volatile || !scalar.Effects.MayError || scalar.Nullable {
			t.Errorf("stack effects = %+v, nullable = %t; want volatile, may-error, nonnullable", scalar.Effects, scalar.Nullable)
		}
		if scalar.OperandCount() != 2 || graph.CaptureCount(scalar.Scope) != 1 {
			t.Fatalf("stack operands = %d, captures = %d; want 2 operands and 1 capture", scalar.OperandCount(), graph.CaptureCount(scalar.Scope))
		}
		capture, _ := graph.Capture(scalar.Scope, 0)
		inner, _ := graph.Operand(id, 1)
		reference, _ := graph.Operand(inner, 0)
		column, _ := graph.Scalar(reference)
		if column.Kind != ir.Slot || column.Slot != capture {
			t.Fatalf("captured operand = %+v, want slot %d", column, capture)
		}
		return
	}
	t.Fatal("missing array stack")
}

func TestNestedArrayErrorsRetainSourcePositions(t *testing.T) {
	for _, scenario := range []struct {
		name string
		sql  string
		code string
	}{
		{"untyped empty", `SELECT ARRAY[ARRAY[1], ARRAY[]]`, "42P18"},
		{"incompatible element types", `SELECT ARRAY[ARRAY[1], ARRAY[true]]`, "42804"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := bindSQL(t, scenario.sql, nil, nil)
			requireCode(t, err, scenario.code)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) {
				t.Fatal(err)
			}
			if want := strings.LastIndex(scenario.sql, "ARRAY") + 1; sqlErr.Position != want {
				t.Errorf("error position = %d, want %d", sqlErr.Position, want)
			}
		})
	}
}

func requireArrayConstructor(t *testing.T, statement *bind.BoundStatement, kind ir.ScalarKind, layout model.TypeLayout) {
	t.Helper()
	columns := statement.Columns()
	if len(columns) != 1 {
		t.Fatalf("result column count = %d, want 1", len(columns))
	}
	requireArrayAggregateLayout(t, columns[0].Layout, layout)
	if columns[0].Nullable {
		t.Error("array constructor result must be nonnullable")
	}
	graph := statement.Graph()
	binding, ok := graph.Binding(graph.Root(), 0)
	if !ok {
		t.Fatal("missing result binding")
	}
	root, ok := graph.Scalar(binding.Expr)
	if !ok || root.Kind != kind {
		t.Fatalf("constructor scalar = %+v, want kind %d", root, kind)
	}
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		scalar, _ := graph.Scalar(id)
		if scalar.Kind != ir.ArrayStack {
			continue
		}
		if scalar.Nullable || !scalar.Effects.MayError || scalar.OperandCount() == 0 {
			t.Fatalf("invalid array stack contract: %+v", scalar)
		}
	}
}
