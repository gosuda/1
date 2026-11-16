package bind_test

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestCollationNameResolution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		collation model.CollationID
		code      string
	}{
		{`"C"`, 950, ""},
		{`pg_catalog."POSIX"`, 951, ""},
		{`"default"`, 100, ""},
		{`c`, 0, "42704"},
		{`"posix"`, 0, "42704"},
		{`"DEFAULT"`, 0, "42704"},
		{`public."C"`, 0, "42704"},
		{`"PG_CATALOG"."C"`, 0, "42704"},
		{`public.ucs_basic`, 0, "42704"},
		{`missing_collation`, 0, "42704"},
		{`pg_catalog.ucs_basic`, 0, "0A000"},
		{`pg_catalog.unicode`, 0, "0A000"},
		{`pg_catalog.pg_c_utf8`, 0, "0A000"},
		{`pg_catalog.pg_unicode_fast`, 0, "0A000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, `SELECT 'x' COLLATE `+tc.name, nil, nil)
			if tc.code != "" {
				requireCode(t, err, tc.code)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := statement.Columns()[0].Layout.Type.Collation; got != tc.collation {
				t.Fatalf("collation = %d, want %d", got, tc.collation)
			}
		})
	}
}

func TestCollationDerivationErrors(t *testing.T) {
	for _, tc := range []struct{ name, sql, code string }{
		{"integer operand", `SELECT 1 COLLATE "C"`, "42804"},
		{"noncollatable string storage", `SELECT 'x'::cstring COLLATE "C"`, "42804"},
		{"integer array", `SELECT ARRAY[1] COLLATE "C"`, "42804"},
		{"comparison conflict", `SELECT ('a' COLLATE "C") = ('a' COLLATE "POSIX")`, "42P21"},
		{"coalesce conflict", `SELECT coalesce('a' COLLATE "C", 'b' COLLATE "POSIX")`, "42P21"},
		{"case conflict", `SELECT CASE WHEN false THEN 'a' COLLATE "C" ELSE 'b' COLLATE "POSIX" END`, "42P21"},
		{"array conflict", `SELECT ARRAY['a' COLLATE "C", 'b' COLLATE "POSIX"]`, "42P21"},
		{"cast strength", `SELECT coalesce(('a' COLLATE "C")::text, 'b' COLLATE "POSIX")`, "42P21"},
		{"aggregate strength", `SELECT coalesce(min('a' COLLATE "C"), 'b' COLLATE "POSIX")`, "42P21"},
		{"window strength", `SELECT coalesce(first_value('a' COLLATE "C") OVER (), 'b' COLLATE "POSIX")`, "42P21"},
		{"set conflict", `SELECT 'a' COLLATE "C" UNION ALL SELECT 'b' COLLATE "POSIX"`, "42P21"},
		{"sort conflict", `SELECT coalesce(a,b) FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) t(a,b) ORDER BY 1`, "42P21"},
		{"group conflict", `SELECT coalesce(a,b) FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) t(a,b) GROUP BY coalesce(a,b)`, "42P21"},
		{"distinct conflict", `SELECT DISTINCT coalesce(a,b) FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) t(a,b)`, "42P21"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := bindSQL(t, tc.sql, nil, nil)
			requireCode(t, err, tc.code)
		})
	}
}

func TestCollationDerivationResults(t *testing.T) {
	for _, tc := range []struct {
		name, sql string
		collation model.CollationID
	}{
		{"default identity", `SELECT 'a'::text`, 100},
		{"nondefault implicit wins", `SELECT coalesce('a',x) FROM (VALUES('b' COLLATE "POSIX")) t(x)`, 951},
		{"explicit default wins", `SELECT coalesce('a' COLLATE "default",x) FROM (VALUES('b' COLLATE "POSIX")) t(x)`, 100},
		{"implicit conflict", `SELECT coalesce(a,b) FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) t(a,b)`, 0},
		{"stronger sibling resolves conflict", `SELECT coalesce(coalesce(a,b),'z' COLLATE "default") FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) t(a,b)`, 100},
		{"outer explicit resolves conflict", `SELECT coalesce(a,b) COLLATE "C" FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) t(a,b)`, 950},
		{"stacked explicit override", `SELECT ('a' COLLATE "POSIX") COLLATE "C"`, 950},
		{"case conditions independent", `SELECT CASE WHEN ('a' COLLATE "C") = 'a' THEN 'b' COLLATE "POSIX" ELSE 'c' END`, 951},
		{"same type cast", `SELECT ('a' COLLATE "POSIX")::text`, 951},
		{"IO cast drops noncollatable state", `SELECT (('1' COLLATE "POSIX")::int4)::text`, 100},
		{"aggregate result", `SELECT min('a' COLLATE "POSIX")`, 951},
		{"window result", `SELECT first_value('a' COLLATE "POSIX") OVER ()`, 951},
		{"aggregate order independent", `SELECT min('a' COLLATE "C" ORDER BY 'b' COLLATE "POSIX")`, 950},
		{"row fields independent", `SELECT ROW('a' COLLATE "C",'b' COLLATE "POSIX")`, 0},
		{"set default precedence", `SELECT 'a' UNION ALL SELECT 'b' COLLATE "POSIX"`, 951},
		{"implicit set conflict allowed", `SELECT a FROM (VALUES('a' COLLATE "C")) t(a) UNION ALL SELECT b FROM (VALUES('b' COLLATE "POSIX")) t(b)`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := statement.Columns()[0].Layout.Type.Collation; got != tc.collation {
				t.Fatalf("result collation = %d, want %d", got, tc.collation)
			}
		})
	}
}

func TestCollationSeparatesInputAndResult(t *testing.T) {
	for _, tc := range []struct {
		name, sql, function string
		input, result       model.CollationID
	}{
		{"comparison", `SELECT ('a' COLLATE "POSIX") = 'b'`, "texteq", 951, 0},
		{"outer relabel", `SELECT lower('a' COLLATE "POSIX") COLLATE "C"`, "lower", 951, 950},
		{"default overrides implicit", `SELECT (x COLLATE "default") = x FROM (VALUES('a' COLLATE "C")) t(x)`, "texteq", 100, 0},
		{"indeterminate comparison", `SELECT a=b FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) t(a,b)`, "texteq", 0, 0},
		{"aggregate", `SELECT min('a' COLLATE "POSIX")`, "min", 951, 951},
		{"window", `SELECT first_value('a' COLLATE "POSIX") OVER ()`, "first_value", 951, 951},
	} {
		t.Run(tc.name, func(t *testing.T) {
			statement, err := bindSQL(t, tc.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got := statement.Columns()[0].Layout.Type.Collation; got != tc.result {
				t.Fatalf("result collation = %d, want %d", got, tc.result)
			}
			requireFunctionInputCollation(t, statement.Graph(), tc.function, tc.input)
		})
	}
}

func TestCollationParameterAndArrayOwnership(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 951}}
	array := model.TypeLayout{Type: model.Type{ID: 1009, Modifier: -1, Storage: model.TypeArray, Collation: 951}, Children: []model.TypeLayout{text}}
	statement, err := bindSQL(t, `SELECT $1 COLLATE "C", $1, $2 COLLATE "C", $2`, nil, []model.TypeLayout{text, array})
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	for i, want := range []model.CollationID{950, 951, 950, 951} {
		if got := columns[i].Layout.Type.Collation; got != want {
			t.Errorf("column %d collation = %d, want %d", i, got, want)
		}
	}
	if got := columns[2].Layout.Children[0].Type.Collation; got != 951 {
		t.Errorf("array element collation = %d, want unchanged POSIX", got)
	}
	for i, parameter := range statement.Parameters() {
		if parameter.Type.Collation != 951 {
			t.Errorf("parameter %d lost its declared collation", i+1)
		}
	}
}

func TestCollationDoesNotRetypeInferredParameter(t *testing.T) {
	statement, err := bindSQL(t, `SELECT $1 COLLATE "POSIX", $1 COLLATE "C", $1`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []model.CollationID{951, 950, 100} {
		if got := statement.Columns()[i].Layout.Type.Collation; got != want {
			t.Errorf("column %d collation = %d, want %d", i, got, want)
		}
	}
	if got := statement.Parameters()[0].Type; got.ID != 25 || got.Collation != 100 {
		t.Fatalf("inferred parameter = %+v, want default-collated text", got)
	}
}

func TestCollationUserRoutineUsesCallInput(t *testing.T) {
	snapshot := tableSnapshot(t)
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Storage: model.TypeString, Collation: 100}}
	routine := testDescriptor(t, model.DescriptorSpec{ID: 12, OID: 90012, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "echo", Signature: "text"}, Routine: new(compiledRoutine(t, snapshot, "SELECT value", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Parameters: []model.RoutineParameter{{Name: "value", Layout: text, Mode: "in"}}, Result: text, Language: "sql", Volatility: "immutable", Strict: true}))})
	snapshot.descriptors = append(snapshot.descriptors, routine)
	statement, err := bindSQL(t, `SELECT public.echo('a' COLLATE "POSIX")`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := statement.Columns()[0].Layout.Type.Collation; got != 951 {
		t.Fatalf("routine result collation = %d, want POSIX", got)
	}
	graph := statement.Graph()
	for id := ir.SignatureID(1); uint32(id) <= graph.SignatureCount(); id++ {
		signature, _ := graph.Signature(id)
		if signature.Function == 90012 {
			if signature.InputCollation != 951 {
				t.Fatalf("routine input collation = %d, want POSIX", signature.InputCollation)
			}
			return
		}
	}
	t.Fatal("missing user routine signature")
}

func requireFunctionInputCollation(t *testing.T, graph *ir.Graph, name string, want model.CollationID) {
	t.Helper()
	found := false
	check := func(id ir.SignatureID, function model.FunctionID) {
		metadata, ok := builtin.LookupSignature(function)
		if !ok || metadata.Name != name {
			return
		}
		found = true
		signature, ok := graph.Signature(id)
		if !ok || signature.InputCollation != want {
			t.Fatalf("input collation = %d, want %d", signature.InputCollation, want)
		}
	}
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		scalar, _ := graph.Scalar(id)
		function := scalar.Function
		if scalar.Operator != 0 {
			if operator, ok := builtin.LookupOperator(scalar.Operator); ok {
				function = operator.Function
			}
		}
		check(scalar.Signature, function)
	}
	for id := ir.RelID(1); uint32(id) <= graph.RelCount(); id++ {
		if call, ok := graph.Aggregate(id, 0); ok {
			check(call.Signature, call.Function)
		}
		if call, ok := graph.Window(id, 0); ok {
			check(call.Signature, call.Function)
		}
	}
	if !found {
		t.Fatalf("missing bound %s", name)
	}
}
