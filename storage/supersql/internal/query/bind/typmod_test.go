package bind_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestScalarModifierResultMetadata(t *testing.T) {
	for _, tc := range []struct {
		typeName string
		id       model.TypeID
		modifier int32
	}{
		{"numeric", 1700, -1},
		{"numeric(1)", 1700, 65540},
		{"decimal(2,4)", 1700, 131080},
		{"numeric(2,-3)", 1700, 133121},
		{"numeric(1000,1000)", 1700, 65537004},
		{"numeric(1000,-1000)", 1700, 65537052},
		{"varchar", 1043, -1},
		{"character varying(2)", 1043, 6},
		{"varchar(10485760)", 1043, 10485764},
		{"char", 1042, 5},
		{"character", 1042, 5},
		{"bpchar", 1042, -1},
		{"bpchar(3)", 1042, 7},
	} {
		t.Run(tc.typeName, func(t *testing.T) {
			statement, err := bindSQL(t, "SELECT NULL::"+tc.typeName, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			column := statement.Columns()[0]
			if column.Layout.Type.ID != tc.id || column.Layout.Type.Modifier != tc.modifier || !column.Nullable {
				t.Fatalf("result = %+v, want nullable type %d modifier %d", column, tc.id, tc.modifier)
			}
		})
	}
}

func TestScalarModifierErrorsHaveTypePosition(t *testing.T) {
	for _, tc := range []struct{ typeName, code string }{
		{"numeric(0)", "22023"},
		{"numeric(1001)", "22023"},
		{"numeric(2,-1001)", "22023"},
		{"numeric(2,1001)", "22023"},
		{"numeric(2,1,0)", "22023"},
		{"numeric(2.5)", "42601"},
		{"numeric(1+1)", "42601"},
		{"varchar(0)", "22023"},
		{"varchar(10485761)", "22023"},
		{"bpchar(-1)", "22023"},
		{"bpchar(10485761)", "22023"},
		{"pg_catalog.time(-1)", "22023"},
		{"pg_catalog.timestamp(-1)", "22023"},
	} {
		t.Run(tc.typeName, func(t *testing.T) {
			sql := "SELECT NULL::" + tc.typeName
			_, err := bindSQL(t, sql, nil, nil)
			requireCode(t, err, tc.code)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Position != strings.Index(sql, tc.typeName)+1 {
				t.Fatalf("error = %+v, want position at type name", sqlErr)
			}
		})
	}
}

func TestInferredParameterModifiersStayAtCastSites(t *testing.T) {
	statement, err := bindSQL(t, `SELECT $1::numeric(2,-3), $1::numeric(4,1), $1, $2::varchar(2), $2::varchar(5), $2`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	parameters := statement.Parameters()
	if len(parameters) != 2 || parameters[0].Type.ID != 1700 || parameters[1].Type.ID != 1043 || parameters[0].Type.Modifier != -1 || parameters[1].Type.Modifier != -1 {
		t.Fatalf("parameters = %+v, want unconstrained numeric and varchar", parameters)
	}
	for i, want := range []int32{133121, 262149, -1, 6, 9, -1} {
		if got := statement.Columns()[i].Layout.Type.Modifier; got != want {
			t.Errorf("column %d modifier = %d, want %d", i, got, want)
		}
	}
}

func TestDeclaredParameterModifiersRemainOwned(t *testing.T) {
	parameter := model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 12, Collation: 951}}
	statement, err := bindSQL(t, `SELECT $1::varchar(2), $1`, nil, []model.TypeLayout{parameter})
	if err != nil {
		t.Fatal(err)
	}
	if got := statement.Parameters()[0].Type; got != parameter.Type {
		t.Fatalf("declared parameter = %+v, want %+v", got, parameter.Type)
	}
	if got := statement.Columns()[0].Layout.Type; got.Modifier != 6 || got.Collation != 951 {
		t.Fatalf("cast result = %+v, want varchar(2) with POSIX collation", got)
	}
	if got := statement.Columns()[1].Layout.Type; got != parameter.Type {
		t.Fatalf("uncast result = %+v, want %+v", got, parameter.Type)
	}
}

func TestCommonResultModifiersDoNotNarrowBranches(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		modifier  int32
	}{
		{"matching case", `SELECT CASE WHEN true THEN 1::numeric(3,1) ELSE 2::numeric(3,1) END`, 196613},
		{"differing case", `SELECT CASE WHEN true THEN 1::numeric(3,1) ELSE 2::numeric(4,1) END`, -1},
		{"matching coalesce", `SELECT coalesce(NULL::varchar(3),'abc'::varchar(3))`, 7},
		{"differing coalesce", `SELECT coalesce(NULL::varchar(3),'abcde'::varchar(5))`, -1},
		{"unknown branch", `SELECT coalesce(NULL,'abc'::varchar(3))`, -1},
		{"matching values", `VALUES (1::numeric(3,1)), (2::numeric(3,1))`, 196613},
		{"differing values", `VALUES (1::numeric(3,1)), (2::numeric(4,1))`, -1},
		{"matching set", `SELECT 'a'::char(3) UNION ALL SELECT 'b'::char(3)`, 7},
		{"differing set", `SELECT 'a'::char(3) UNION ALL SELECT 'b'::char(4)`, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := statement.Columns()[0].Layout.Type.Modifier; got != tc.modifier {
				t.Fatalf("result modifier = %d, want %d", got, tc.modifier)
			}
		})
	}
}

func TestCharacterModifierPreservesCollationDerivation(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (('abcd' COLLATE "POSIX")::varchar(2)) COLLATE "C", ('a' COLLATE "default")::char(2)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if statement.Columns()[0].Layout.Type.Collation != 950 || statement.Columns()[1].Layout.Type.Collation != 100 {
		t.Fatalf("result collations = %+v", statement.Columns())
	}
	requireFunctionInputCollation(t, statement.Graph(), "varchar", 951)
	requireFunctionInputCollation(t, statement.Graph(), "bpchar", 100)
	_, err = bindSQL(t, `SELECT coalesce(('a' COLLATE "C")::varchar(2), ('b' COLLATE "POSIX")::varchar(2))`, nil, nil)
	requireCode(t, err, "42P21")
}

func TestArrayElementModifiersAreNotSilentlyErased(t *testing.T) {
	for _, tc := range []struct {
		sql      string
		modifier int32
	}{
		{`SELECT ARRAY[1::numeric(3,1),2::numeric(3,1)]`, 196613},
		{`SELECT ARRAY[1::numeric(3,1),2::numeric(4,1)]`, -1},
		{`SELECT ARRAY['a'::varchar(3)]::varchar(3)[]`, 7},
	} {
		statement, err := bindSQL(t, tc.sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		layout := statement.Columns()[0].Layout
		if len(layout.Children) != 1 || layout.Children[0].Type.Modifier != tc.modifier {
			t.Fatalf("%s: array layout = %+v, want element modifier %d", tc.sql, layout, tc.modifier)
		}
	}
}

func TestRoutineAcceptsConstrainedArgumentOfDeclaredType(t *testing.T) {
	snapshot := tableSnapshot(t)
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Modifier: -1, Storage: model.TypeNumeric}}
	routine := testDescriptor(t, model.DescriptorSpec{ID: 12, OID: 90012, Version: 1, State: model.StatePublic,
		Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "echo", Signature: "numeric"},
		Routine: new(compiledRoutine(t, snapshot, "SELECT value", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: []model.RoutineParameter{{Name: "value", Layout: numeric, Mode: "in"}}, Result: numeric, Language: "sql", Volatility: "immutable", Strict: true})),
	})
	snapshot.descriptors = append(snapshot.descriptors, routine)
	statement, err := bindSQL(t, `SELECT public.echo(1.25::numeric(3,2))`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	result := statement.Columns()[0].Layout.Type
	if result.ID != 1700 || result.Modifier != -1 {
		t.Fatalf("routine result type=%+v, want unconstrained numeric", result)
	}
}
