package bind_test

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
)

func TestRowSyntaxUsesSQLFieldComparisonSemantics(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		want      bool
		null      bool
	}{
		{"row null", `SELECT ROW(1,NULL::int) = ROW(1,NULL::int)`, false, true},
		{"record null", `SELECT record_eq(ROW(1,NULL::int),ROW(1,NULL::int))`, true, false},
		{"unequal row before null", `SELECT ROW(1,NULL::int) <> ROW(2,NULL::int)`, true, false},
		{"row cross type", `SELECT ROW(1::smallint) = ROW(1::bigint)`, true, false},
		{"row IN null", `SELECT ROW(1,NULL::int) IN (ROW(1,NULL::int),ROW(2,3))`, false, true},
		{"row IN unknown", `SELECT ROW('a') IN (ROW('a'),ROW('b'))`, true, false},
		{"anonymous record order", `SELECT record_lt(ROW(1,NULL::int),ROW(1,2))`, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			values, err := evaluateRecordExpression(t, statement, nil)
			if err != nil {
				t.Fatal(err)
			}
			wrongBoolean := !scenario.null && (values[0].IntVal != 0) != scenario.want
			if values[0].IsNull != scenario.null || wrongBoolean {
				t.Fatalf("result = %+v, %v; want bool=%v null=%v", values, err, scenario.want, scenario.null)
			}
		})
	}
}

func TestRecordFunctionsDoNotCoerceMismatchedFieldDeclarations(t *testing.T) {
	for _, scenario := range []struct{ sql, code string }{
		{`SELECT record_eq(ROW(1::smallint),ROW(1::bigint))`, "42804"},
		{`SELECT record_eq(ROW('a'),ROW('a'::text))`, "42804"},
		{`SELECT record_eq(ROW(NULL),ROW(NULL))`, "42883"},
	} {
		statement, err := bindSQL(t, scenario.sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		_, err = evaluateRecordExpression(t, statement, nil)
		requireCode(t, err, scenario.code)
	}
}

func TestRowComparisonRejectsCountAndOperatorMismatches(t *testing.T) {
	for _, scenario := range []struct{ sql, code string }{
		{`SELECT ROW(1) = ROW(1,2)`, "42601"},
		{`SELECT ROW(1) = ROW(true)`, "42883"},
		{`SELECT ROW() = ROW()`, "0A000"},
	} {
		_, err := bindSQL(t, scenario.sql, nil, nil)
		requireCode(t, err, scenario.code)
	}
}

func TestRowComparisonSkipsLaterVolatileFields(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{ID: 40, OID: 90040, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "changing"}, Routine: new(compiledRoutine(t, snapshot, "SELECT 1", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: integer, Language: "sql", Volatility: "volatile"}))}))
	for _, scenario := range []struct {
		sql   string
		calls int
	}{
		{`SELECT ROW(1,public.changing()) = ROW(2,public.changing())`, 0},
		{`SELECT record_eq(ROW(1,public.changing()),ROW(2,public.changing()))`, 2},
	} {
		statement, err := bindSQL(t, scenario.sql, snapshot, nil)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		values, err := evaluateRecordExpression(t, statement, onceResolver(func(expr.Function) (expr.ScalarCall, error) {
			return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
				calls++
				return model.Int64Val(1), nil
			}, nil
		}))
		if err != nil || values[0].IsNull || values[0].IntVal != 0 || calls != scenario.calls || statement.Effects().Volatility != ir.Volatile {
			t.Fatalf("comparison=%s values=%+v calls=%d effects=%+v error=%v", scenario.sql, values, calls, statement.Effects(), err)
		}
	}
}

func TestRowComparisonRetainsConstantFoldErrors(t *testing.T) {
	const sql = `SELECT ROW(1,1/0) = ROW(1,3)`
	statement, err := bindSQL(t, sql, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = normalize.Normalize(t.Context(), statement.Graph(), normalize.Options{Limits: bindingOptions(sql).Limits, MaxWork: 1000000, MaxBytes: 16 << 20})
	requireCode(t, err, "22012")
}
