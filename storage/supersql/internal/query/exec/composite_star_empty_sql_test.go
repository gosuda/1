package exec_test

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLZeroFieldStarRetainsAggregateCardinalityWithoutCalls(t *testing.T) {
	empty := model.TypeLayout{Type: model.Type{ID: 90020, Storage: model.TypeComposite, Modifier: -1}}
	snapshot := make(lateralReplayCatalog, 3)
	for i, spec := range []model.DescriptorSpec{
		{ID: 10, OID: 10010, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Kind: model.ObjectSchema, Name: "public"}, Schema: &model.SchemaMetadata{Owner: 10}},
		{ID: 20, OID: 90020, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "empty_record"}, Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: empty, Array: 21}},
		{ID: 12, OID: 90012, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "empty_value"}, Routine: new(compiledRoutine(t, lateralReplayCatalog{}, "SELECT ROW()", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: empty, Language: "sql", Volatility: "volatile"}))},
	} {
		var err error
		snapshot[i], err = model.NewDescriptor(t.Context(), spec, model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, sql string
		want      int64
	}{
		{"scalar", "SELECT count(*) FROM (SELECT (public.empty_value()).* FROM generate_series(1,3))s", 3},
		{"aggregate", "SELECT count(*) FROM (SELECT (any_value(public.empty_value())).* FROM generate_series(1,3))s", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraphWithCatalog(t, tc.sql, snapshot, nil)
			env := statefulEnvironment(t)
			calls := 0
			env.Functions = aggregateProjectionResolver(func(expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls++
					return model.Value{Type: model.TypeComposite}, nil
				}, nil
			})
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertAnyValueRows(t, portal, []model.TypeID{20}, [][]model.Value{{model.Int64Val(tc.want)}})
			if calls != 0 {
				t.Fatalf("discarded zero-field parent executed %d times", calls)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
