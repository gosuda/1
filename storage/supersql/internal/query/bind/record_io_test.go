package bind_test

import (
	"context"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func recordIOSnapshot(t *testing.T) (metadataSnapshot, model.TypeLayout, model.TypeLayout) {
	t.Helper()
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	label := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 7, Storage: model.TypeString, Collation: 950}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	// Attribute IDs stay sparse so the codec dependency assertions below still
	// prove dropped-attribute IDs are skipped without a tombstoned descriptor.
	fields := []model.ColumnDescriptor{
		{ID: 1, Name: "label", State: model.StatePublic, Layout: label},
		{ID: 3, Name: "numbers", State: model.StatePublic, Layout: integers},
	}
	snapshot, array := unnestNamedSnapshot(t, fields, []model.TypeLayout{label, integers})
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 31, OID: 90001, Version: 4, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "record_array"}, Type: &model.TypeMetadata{Kind: model.TypeKindArray, Layout: array, Base: 30, BaseLayout: array.Children[0]},
	}))
	return snapshot, array.Children[0], array
}

func TestRecordInputBindingPreservesNominalLayout(t *testing.T) {
	snapshot, record, _ := recordIOSnapshot(t)
	statement, err := bindSQL(t, `SELECT '(abc,"{1,2}")'::outside_search_path.record_type`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireArrayAggregateLayout(t, statement.Columns()[0].Layout, record)
	for _, dependency := range []model.Dependency{{Object: 10, Version: 1}, {Object: 30, Version: 3}, {Object: 30, Version: 3, Column: 1}, {Object: 30, Version: 3, Column: 3}} {
		if !slices.Contains(statement.Dependencies(), dependency) {
			t.Errorf("missing codec dependency %+v", dependency)
		}
	}
	if slices.Contains(statement.Dependencies(), model.Dependency{Object: 30, Version: 3, Column: 2}) {
		t.Fatal("dropped field became a codec dependency")
	}
}

func TestRecordTextIOUsesDeclaredFieldInputs(t *testing.T) {
	snapshot, _, _ := recordIOSnapshot(t)
	for _, scenario := range []struct{ name, sql, output string }{
		{"anonymous output", `SELECT record_out(ROW(7,''::text,NULL::int))`, `(7,"",)`},
		{"unknown field output", `SELECT record_out(ROW('a',NULL))`, `(a,)`},
		{"named output", `SELECT record_out('(abc,"{1,2}")'::outside_search_path.record_type)`, `(abc,"{1,2}")`},
		{"text cast", `SELECT ('(abc,)'::text::outside_search_path.record_type)::text`, `(abc,)`},
		{"row field casts", `SELECT (ROW('abcdef', ARRAY[1::smallint,2::smallint])::outside_search_path.record_type)::text`, `(abc,"{1,2}")`},
		{"array input", `SELECT record_out(('{"(abc,)","(,)",NULL}'::outside_search_path.record_array)[1])`, `(abc,)`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			values, err := evaluateRecordExpression(t, statement, nil)
			if err != nil || values[0].IsNull || values[0].StrVal != scenario.output {
				t.Fatalf("output = %+v, %v; want %q", values, err, scenario.output)
			}
		})
	}
	for _, scenario := range []struct{ name, sql, code string }{
		{"field typmod", `SELECT '(abcd,)'::outside_search_path.record_type`, "22001"},
		{"array field input", `SELECT '(abc,"{bad}")'::outside_search_path.record_type`, "22P02"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := bindSQL(t, scenario.sql, snapshot, nil)
			requireCode(t, err, scenario.code)
		})
	}
}

func TestRecordNullLiteralRetainsDeclaredSchema(t *testing.T) {
	snapshot, record, _ := recordIOSnapshot(t)
	statement, err := bindSQL(t, `SELECT NULL::outside_search_path.record_type`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireArrayAggregateLayout(t, statement.Columns()[0].Layout, record)
	values, err := evaluateRecordExpression(t, statement, nil)
	if err != nil || !values[0].IsNull || values[0].Type != model.TypeComposite {
		t.Fatalf("named NULL = %+v, %v", values, err)
	}
}

func TestRecordFunctionsKeepDistinctNominalArguments(t *testing.T) {
	snapshot, record, _ := recordIOSnapshot(t)
	other := record
	other.Type.ID = 90002
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 32, OID: 90002, Version: 1, State: model.StatePublic,
		Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "other_record"},
		Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: other, Array: 33, Fields: []model.ColumnDescriptor{{ID: 1, Name: "label", State: model.StatePublic, Layout: other.Children[0]}, {ID: 2, Name: "numbers", State: model.StatePublic, Layout: other.Children[1]}}},
	}))
	statement, err := bindSQL(t, `SELECT record_eq($1,$2)`, snapshot, []model.TypeLayout{record, other})
	if err != nil {
		t.Fatal(err)
	}
	_, call, _ := variadicRoot(t, statement)
	for i, want := range []model.TypeLayout{record, other} {
		layout, _ := statement.Graph().SignatureParameter(call.Signature, uint32(i))
		typ, _ := statement.Graph().Type(layout)
		if typ != want.Type {
			t.Fatalf("argument %d declaration = %+v, want %+v", i, typ, want.Type)
		}
		child, _ := statement.Graph().LayoutChild(layout, 0)
		field, _ := statement.Graph().Type(child)
		if field != want.Children[0].Type {
			t.Fatalf("argument %d field = %+v, want typmod and collation %+v", i, field, want.Children[0].Type)
		}
	}
	statement, err = bindSQL(t, `SELECT record_eq('(abc,)'::outside_search_path.record_type,'(abc,)'::outside_search_path.other_record)`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	values, err := evaluateRecordExpression(t, statement, nil)
	if err != nil || values[0].IsNull || values[0].IntVal != 1 {
		t.Fatalf("distinct nominal record comparison = %+v, %v", values, err)
	}
}

func TestRecordCodecRejectsInconsistentPublicDescriptors(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 950, Storage: model.TypeString}}
	// The declaration and the caller-supplied value layout are independent
	// inputs, so the codec still has to reject an element whose attributes
	// disagree with the declaration it resolves by identity.
	snapshot, array := unnestNamedSnapshot(t, []model.ColumnDescriptor{{ID: 1, Name: "wrong", State: model.StatePublic, Layout: integer}}, []model.TypeLayout{integer})
	array.Children[0].Children = []model.TypeLayout{text}
	for _, sql := range []string{`SELECT record_out($1)`, `SELECT record_eq($1,$1)`, `SELECT '(1)'::outside_search_path.record_type`, `SELECT NULL::outside_search_path.record_type`} {
		_, err := bindSQL(t, sql, snapshot, []model.TypeLayout{array.Children[0]})
		requireCode(t, err, "XX000")
	}
}

func TestRowToNamedCompositeRejectsUncastableFields(t *testing.T) {
	snapshot, _, _ := recordIOSnapshot(t)
	for _, sql := range []string{
		`SELECT ROW('abc')::outside_search_path.record_type`,
		`SELECT ROW('abc', ARRAY[1], 3)::outside_search_path.record_type`,
		`SELECT ROW('abc', 1)::outside_search_path.record_type`,
		`SELECT $1::outside_search_path.record_type`,
	} {
		_, err := bindSQL(t, sql, snapshot, []model.TypeLayout{{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}}})
		requireCode(t, err, "42846")
	}
}

func evaluateRecordExpression(t *testing.T, statement *bind.BoundStatement, functions expr.Resolver) ([]model.Value, error) {
	t.Helper()
	graph := statement.Graph()
	roots := make([]ir.ScalarID, len(statement.Columns()))
	for i := range roots {
		binding, ok := graph.Binding(graph.Root(), uint32(i))
		if !ok {
			t.Fatalf("missing output %d", i)
		}
		roots[i] = binding.Expr
	}
	program, err := expr.Compile(t.Context(), graph, roots, expr.Options{Limits: expr.DefaultLimits(), Functions: functions, Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, ExtraFloatDigits: 1}})
	if err != nil {
		return nil, err
	}
	state, err := expr.NewState(program, expr.StateOptions{})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	input, output := columnar.Batch{Length: 1}, columnar.Batch{}
	if err := state.Eval(t.Context(), &input, nil, &output, expr.Direct); err != nil {
		return nil, err
	}
	values := make([]model.Value, len(roots))
	for i := range values {
		values[i], _, err = columnar.CopyValue(t.Context(), &output.Columns[i], 0, 1<<20)
		if err != nil {
			return nil, err
		}
	}
	return values, nil
}

func TestRowCastEvaluatesFieldsOnceInSourceOrder(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	snapshot, array := unnestNamedSnapshot(t, []model.ColumnDescriptor{{ID: 1, Name: "a", State: model.StatePublic, Layout: integer}, {ID: 2, Name: "b", State: model.StatePublic, Layout: integer}}, []model.TypeLayout{integer, integer})
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{ID: 40, OID: 90040, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "changing"}, Routine: new(compiledRoutine(t, snapshot, "SELECT 1", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: integer, Language: "sql", Volatility: "volatile"}))}))
	statement, err := bindSQL(t, `SELECT ROW(outside_search_path.changing(),outside_search_path.changing())::outside_search_path.record_type`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireArrayAggregateLayout(t, statement.Columns()[0].Layout, array.Children[0])
	calls := int64(0)
	values, err := evaluateRecordExpression(t, statement, onceResolver(func(expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			calls++
			return model.Int64Val(calls), nil
		}, nil
	}))
	if err != nil || calls != 2 || values[0].ArrayVal[0].IntVal != 1 || values[0].ArrayVal[1].IntVal != 2 || statement.Effects().Volatility != ir.Volatile {
		t.Fatalf("row cast values=%+v calls=%d effects=%+v error=%v", values, calls, statement.Effects(), err)
	}
}

func TestNamedCompositeArraySuffixUsesCatalogIdentity(t *testing.T) {
	snapshot, _, array := recordIOSnapshot(t)
	statement, err := bindSQL(t, `SELECT '{"(abc,)","(,)",NULL}'::outside_search_path.record_type[]`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	requireArrayAggregateLayout(t, statement.Columns()[0].Layout, array)
	if !slices.Contains(statement.Dependencies(), model.Dependency{Object: 31, Version: 4}) {
		t.Fatal("array type dependency missing")
	}
	values, err := evaluateRecordExpression(t, statement, nil)
	if err != nil {
		t.Fatal(err)
	}
	items := values[0].ArrayVal
	if len(items) != 3 || items[0].ArrayVal[0].StrVal != "abc" || !items[1].ArrayVal[0].IsNull || !items[2].IsNull {
		t.Fatalf("named array fields = %+v", items)
	}
}

func TestRowCastInfersParametersFromDeclaredFields(t *testing.T) {
	snapshot, record, _ := recordIOSnapshot(t)
	statement, err := bindSQL(t, `SELECT ROW($1,$2)::outside_search_path.record_type`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	parameters := statement.Parameters()
	if len(parameters) != 2 || parameters[0].Type.ID != 1043 || parameters[1].Type.ID != 1007 {
		t.Fatalf("row cast parameter types = %+v", parameters)
	}
	requireArrayAggregateLayout(t, statement.Columns()[0].Layout, record)
}

func TestRecordOutputKeepsNestedNominalDeclaration(t *testing.T) {
	snapshot, record, _ := recordIOSnapshot(t)
	outer := model.TypeLayout{Type: model.Type{ID: 90002, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{record}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{ID: 32, OID: 90002, Version: 1, State: model.StatePublic, Name: model.NamespaceName{Parent: 10, Kind: model.ObjectType, Name: "outer_record"}, Type: &model.TypeMetadata{Kind: model.TypeKindComposite, Layout: outer, Array: 33, Fields: []model.ColumnDescriptor{{ID: 1, Name: "nested", State: model.StatePublic, Layout: record}}}}))
	statement, err := bindSQL(t, `SELECT record_out($1)`, snapshot, []model.TypeLayout{outer})
	if err != nil {
		t.Fatal(err)
	}
	_, call, _ := variadicRoot(t, statement)
	layout, _ := statement.Graph().SignatureParameter(call.Signature, 0)
	for _, want := range []model.Type{outer.Type, record.Type, record.Children[0].Type} {
		got, _ := statement.Graph().Type(layout)
		if got != want {
			t.Fatalf("record output declaration = %+v, want %+v", got, want)
		}
		layout, _ = statement.Graph().LayoutChild(layout, 0)
	}
	if !statement.Columns()[0].Nullable {
		t.Fatal("nullable record argument became a nonnullable output")
	}
}

func TestUnknownRecordFieldsResolveInScalarContexts(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (ROW('a')).f1,(ROW('12')).f1::int,coalesce((ROW(NULL)).f1,7),concat((ROW('x')).f1)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	for i, want := range []model.TypeID{25, 23, 23, 25} {
		if columns[i].Layout.Type.ID != want {
			t.Errorf("field %d type = %d, want %d", i, columns[i].Layout.Type.ID, want)
		}
	}
	values, err := evaluateRecordExpression(t, statement, nil)
	if err != nil || values[0].StrVal != "a" || values[1].IntVal != 12 || values[2].IntVal != 7 || values[3].StrVal != "x" {
		t.Fatalf("contextual record fields = %+v, %v", values, err)
	}
}
