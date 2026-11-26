package bind_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestSQLJSONBehaviorsAreLazyAndInputsEvaluateOnce(t *testing.T) {
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 40, OID: 90040, Version: 1, State: model.StatePublic,
		Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "changing"},
		Routine: new(compiledRoutine(t, snapshot, "SELECT 9", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: integer, Language: "sql", Volatility: "volatile"})),
	}))
	for _, test := range []struct {
		name  string
		sql   string
		want  int64
		calls int
	}{
		{"unselected default", `SELECT JSON_VALUE('{"a":7}', '$.a' RETURNING int DEFAULT public.changing() ON ERROR)`, 7, 0},
		{"passing value", `SELECT JSON_VALUE('null', '$x' PASSING public.changing() AS x RETURNING int ERROR ON ERROR)`, 9, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			statement, err := bindSQL(t, test.sql, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			values, err := evaluateRecordExpression(t, statement, onceResolver(func(expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls++
					return model.Int64Val(9), nil
				}, nil
			}))
			if err != nil || len(values) != 1 || values[0].IsNull || values[0].IntVal != test.want || calls != test.calls {
				t.Fatalf("value=%v calls=%d error=%v; want %d calls=%d", values, calls, err, test.want, test.calls)
			}
		})
	}
}

func TestSQLJSONTimezoneExecutionAndMutabilityAreBound(t *testing.T) {
	for _, test := range []struct {
		sql        string
		volatility ir.Volatility
	}{
		{`SELECT JSON_EXISTS('null', '$ == null')`, ir.Immutable},
		{`SELECT JSON_EXISTS('null', '$d == $z' PASSING date '2000-01-01' AS d, timestamptz '2000-01-01 UTC' AS z)`, ir.Stable},
	} {
		statement, err := bindSQL(t, test.sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		var found bool
		graph := statement.Graph()
		for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
			scalar, _ := graph.Scalar(id)
			if scalar.Kind != ir.SQLJSON {
				continue
			}
			found = true
			if scalar.JSONFlags&ir.SQLJSONUseTZ == 0 || scalar.Effects.Volatility != test.volatility {
				t.Fatalf("SQL/JSON flags=%v effects=%+v; want timezone and %v", scalar.JSONFlags, scalar.Effects, test.volatility)
			}
		}
		if !found {
			t.Fatal("SQL/JSON scalar was not bound")
		}
	}
}

func TestSQLJSONDocumentContextsRejectNonDocumentInputs(t *testing.T) {
	for _, sql := range []string{
		`SELECT JSON(1)`,
		`SELECT 1 IS JSON`,
		`SELECT JSON_SERIALIZE(1)`,
		`SELECT JSON_EXISTS(1, '$')`,
	} {
		_, err := bindSQL(t, sql, nil, nil)
		var sqlError *model.Error
		if !errors.As(err, &sqlError) || sqlError.Code != "42804" {
			t.Fatalf("%s error=%v; want SQLSTATE 42804", sql, err)
		}
	}
}
