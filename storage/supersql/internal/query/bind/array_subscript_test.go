package bind_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestArrayFetchBindsDimensionChains(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		sql        string
		kind       ir.ScalarKind
		subscripts ir.ArraySubscripts
		bounds     []int64
	}{
		{"multidimensional element", `SELECT (ARRAY[[1,2],[3,4]])[2][1]`, ir.ArrayIndex, ir.ArraySubscripts{}, []int64{2, 1}},
		{"slice", `SELECT (ARRAY[[1,2],[3,4]])[1:2][2:3]`, ir.ArraySlice, ir.ArraySubscripts{Dimensions: 2, Upper: 3, Lower: 3}, []int64{2, 3, 1, 2}},
		{"mixed index before slice", `SELECT (ARRAY[[1,2],[3,4]])[2][1:3]`, ir.ArraySlice, ir.ArraySubscripts{Dimensions: 2, Upper: 3, Lower: 3}, []int64{2, 3, 1, 1}},
		{"mixed index after slice", `SELECT (ARRAY[[1,2],[3,4]])[:][2]`, ir.ArraySlice, ir.ArraySubscripts{Dimensions: 2, Upper: 2, Lower: 2}, []int64{2, 1}},
		{"omitted upper", `SELECT (ARRAY[[1,2],[3,4]])[:][2:]`, ir.ArraySlice, ir.ArraySubscripts{Dimensions: 2, Lower: 2}, []int64{2}},
		{"omitted lower", `SELECT (ARRAY[[1,2],[3,4]])[:2][:]`, ir.ArraySlice, ir.ArraySubscripts{Dimensions: 2, Upper: 1}, []int64{2}},
		{"all omitted", `SELECT (ARRAY[[1,2],[3,4]])[:][:]`, ir.ArraySlice, ir.ArraySubscripts{Dimensions: 2}, nil},
		{"six dimensions", `SELECT (ARRAY[1])[1][2][3][4][5][6]`, ir.ArrayIndex, ir.ArraySubscripts{}, []int64{1, 2, 3, 4, 5, 6}},
		{"six sliced dimensions", `SELECT (ARRAY[1])[1:2][3:4][5:6][7:8][9:10][11:12]`, ir.ArraySlice, ir.ArraySubscripts{Dimensions: 6, Upper: 63, Lower: 63}, []int64{2, 4, 6, 8, 10, 12, 1, 3, 5, 7, 9, 11}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			graph := statement.Graph()
			id, fetch := boundArrayFetch(t, graph)
			if fetch.Kind != scenario.kind || fetch.Subscripts != scenario.subscripts || fetch.OperandCount() != uint32(len(scenario.bounds)+1) {
				t.Fatalf("fetch = %+v, want kind %d, subscripts %+v, %d bounds", fetch, scenario.kind, scenario.subscripts, len(scenario.bounds))
			}
			for i, want := range scenario.bounds {
				operand, _ := graph.Operand(id, uint32(i+1))
				value, err := graph.CloneConstant(t.Context(), operand, 1024)
				if err != nil || value.IsNull || value.IntVal != want {
					t.Fatalf("bound %d = %+v, %v; want %d", i, value, err, want)
				}
			}
			wantType := model.TypeID(23)
			if scenario.kind == ir.ArraySlice {
				wantType = 1007
			}
			columns := statement.Columns()
			if len(columns) != 1 || columns[0].Layout.Type.ID != wantType || !columns[0].Nullable {
				t.Fatalf("fetch output = %+v, want nullable type %d", columns, wantType)
			}
			if want := strings.Index(scenario.sql, "ARRAY"); fetch.Span.Start != uint32(want) {
				t.Errorf("fetch source offset = %d, want %d", fetch.Span.Start, want)
			}
		})
	}
}

func TestArrayFetchBindingDoesNotInspectRuntimeShape(t *testing.T) {
	for _, sql := range []string{
		`SELECT (ARRAY[]::int4[])[1][1]`,
		`SELECT (ARRAY[[1],[2,3]])[1][1]`,
		`SELECT (ARRAY[1])[999]`,
		`SELECT (ARRAY[1])[2:1]`,
		`SELECT (ARRAY[1])[NULL:2]`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, fetch := boundArrayFetch(t, statement.Graph())
			if !fetch.Nullable {
				t.Fatalf("shape-independent fetch cannot exclude NULL: %+v", fetch)
			}
		})
	}
}

func TestArrayFetchDoesNotIntroduceExecutionErrors(t *testing.T) {
	for _, sql := range []string{`SELECT (ARRAY[1])[9]`, `SELECT (ARRAY[1])[2:1]`} {
		statement, err := bindSQL(t, sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if effects := statement.Effects(); effects.MayError || effects.Volatility != ir.Immutable {
			t.Fatalf("%s: fetch effects = %+v, want immutable and non-erroring", sql, effects)
		}
	}
}

func TestNullArraySliceStillInfersBothBoundParameters(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (NULL::int4[])[$1:$2]`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	parameters := statement.Parameters()
	if len(parameters) != 2 || parameters[0].Type.ID != 23 || parameters[1].Type.ID != 23 {
		t.Fatalf("bound parameters = %+v, want two int4 parameters", parameters)
	}
	graph := statement.Graph()
	id, _ := boundArrayFetch(t, graph)
	for i, want := range []uint32{2, 1} {
		operand, _ := graph.Operand(id, uint32(i+1))
		parameter, _ := graph.Scalar(operand)
		if parameter.Kind != ir.Param || parameter.Parameter != want {
			t.Fatalf("bound %d = %+v, want parameter $%d", i, parameter, want)
		}
	}
}

func TestArrayFetchPreservesDeclaredLayouts(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	text := model.TypeLayout{Type: model.Type{ID: 1043, Storage: model.TypeString, Modifier: 12, Collation: 950}}
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: 655366}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Storage: model.TypeComposite, Modifier: 7}, Children: []model.TypeLayout{text, integers, numeric}}
	for _, array := range []model.TypeLayout{
		{Type: model.Type{ID: 1015, Storage: model.TypeArray, Modifier: 12, Collation: 950}, Children: []model.TypeLayout{text}},
		{Type: model.Type{ID: 1231, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{numeric}},
		{Type: model.Type{ID: 2287, Storage: model.TypeArray, Modifier: 7}, Children: []model.TypeLayout{record}},
	} {
		for _, scenario := range []struct {
			sql    string
			result model.TypeLayout
		}{
			{`SELECT $1[$2]`, array.Children[0]},
			{`SELECT $1[$2:]`, array},
		} {
			statement, err := bindSQL(t, scenario.sql, nil, []model.TypeLayout{array})
			if err != nil {
				t.Fatal(err)
			}
			requireArrayAggregateLayout(t, statement.Columns()[0].Layout, scenario.result)
			parameters := statement.Parameters()
			if len(parameters) != 2 {
				t.Fatalf("parameters = %+v, want declared source and inferred bound", parameters)
			}
			requireArrayAggregateLayout(t, parameters[0], array)
			requireArrayAggregateLayout(t, parameters[1], integer)
		}
	}
}

func TestArrayFetchUsesAssignmentCastsForBounds(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		input     model.TypeID
		declared  []model.TypeLayout
	}{
		{"numeric literal", `SELECT (ARRAY[1])[1.5]`, 1700, nil},
		{"bigint expression", `SELECT (ARRAY[1])[1::bigint]`, 20, nil},
		{"declared numeric", `SELECT (ARRAY[1])[$1]`, 1700, []model.TypeLayout{{Type: model.Type{ID: 1700, Storage: model.TypeNumeric, Modifier: -1}}}},
		{"declared bigint lower", `SELECT (ARRAY[1])[$1:]`, 20, []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64, Modifier: -1}}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			statement, err := bindSQL(t, scenario.sql, nil, scenario.declared)
			if err != nil {
				t.Fatal(err)
			}
			graph := statement.Graph()
			id, _ := boundArrayFetch(t, graph)
			operand, _ := graph.Operand(id, 1)
			cast, _ := graph.Scalar(operand)
			result, _ := graph.Type(cast.Layout)
			if cast.Kind != ir.Cast || cast.CastContext != ir.AssignmentCast || result.ID != 23 {
				t.Fatalf("bound = %+v, type %+v; want assignment cast to int4", cast, result)
			}
			input, _ := graph.Operand(operand, 0)
			scalar, _ := graph.Scalar(input)
			typ, _ := graph.Type(scalar.Layout)
			if typ.ID != scenario.input {
				t.Fatalf("bound input = %+v, want type %d", typ, scenario.input)
			}
			if len(scenario.declared) != 0 {
				requireArrayAggregateLayout(t, statement.Parameters()[0], scenario.declared[0])
			}
		})
	}
}

func TestArrayFetchPreservesExplicitCollation(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Modifier: -1, Collation: 950}}
	texts := model.TypeLayout{Type: model.Type{ID: 1009, Storage: model.TypeArray, Modifier: -1, Collation: 950}, Children: []model.TypeLayout{text}}
	for _, sql := range []string{
		`SELECT ($1 COLLATE "POSIX")[1]`,
		`SELECT ($1 COLLATE "POSIX")[:]`,
		`SELECT (ARRAY['x'] COLLATE "POSIX")[1]`,
	} {
		statement, err := bindSQL(t, sql, nil, []model.TypeLayout{texts})
		if err != nil {
			t.Fatal(err)
		}
		if got := statement.Columns()[0].Layout.Type.Collation; got != 951 {
			t.Fatalf("%s: output collation = %d, want POSIX", sql, got)
		}
		graph := statement.Graph()
		id, fetch := boundArrayFetch(t, graph)
		if fetch.Kind == ir.ArrayIndex {
			sourceID, _ := graph.Operand(id, 0)
			source, _ := graph.Scalar(sourceID)
			child, _ := graph.LayoutChild(source.Layout, 0)
			got, _ := graph.Type(fetch.Layout)
			want, _ := graph.Type(child)
			if got != want {
				t.Fatalf("core fetch type = %+v, want exact source child %+v", got, want)
			}
		}
	}
}

func TestArrayFetchRespectsParenthesizedSliceSource(t *testing.T) {
	statement, err := bindSQL(t, `SELECT ((ARRAY[1,2,3])[2:3])[1]`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	var kinds []ir.ScalarKind
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		scalar, _ := graph.Scalar(id)
		if scalar.Kind == ir.ArrayIndex || scalar.Kind == ir.ArraySlice {
			kinds = append(kinds, scalar.Kind)
		}
	}
	if !slices.Equal(kinds, []ir.ScalarKind{ir.ArraySlice, ir.ArrayIndex}) || statement.Columns()[0].Layout.Type.ID != 23 {
		t.Fatalf("parenthesized fetch kinds = %v, output = %+v", kinds, statement.Columns())
	}
}

func TestArrayFetchReusesGroupedExpressions(t *testing.T) {
	for _, sql := range []string{
		`SELECT a[1] FROM (VALUES(ARRAY[1]))t(a) GROUP BY t.a[1]`,
		`SELECT a[1:2] FROM (VALUES(ARRAY[1]))t(a) GROUP BY t.a[1:2]`,
		`SELECT (ARRAY[x])[1] FROM (VALUES(1))t(x) GROUP BY ARRAY[t.x]`,
	} {
		statement, err := bindSQL(t, sql, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !statement.Columns()[0].Nullable {
			t.Fatalf("grouped fetch output = %+v, want nullable", statement.Columns())
		}
	}
	for _, sql := range []string{
		`SELECT a[2] FROM (VALUES(ARRAY[1]))t(a) GROUP BY a[1]`,
		`SELECT a[x] FROM (VALUES(ARRAY[1],1))t(a,x) GROUP BY a`,
		`SELECT (NULL::int4[])[x] FROM (VALUES(1))t(x) GROUP BY ()`,
	} {
		_, err := bindSQL(t, sql, nil, nil)
		requireCode(t, err, "42803")
	}
}

func TestArrayFetchRetainsOuterBoundCaptures(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (SELECT (NULL::int4[])[t."X"]) FROM "Mixed" t`, tableSnapshot(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	dependency := model.Dependency{Object: 11, Version: 3, Column: 1}
	if !slices.Contains(statement.Dependencies(), dependency) {
		t.Fatalf("dependencies = %+v, missing %+v", statement.Dependencies(), dependency)
	}
	_, fetch := boundArrayFetch(t, statement.Graph())
	if got := statement.Graph().CaptureCount(fetch.Scope); got != 1 {
		t.Fatalf("fetch scope captures = %d, want outer bound", got)
	}
}

func TestArrayFetchValidatesBoundsEvenForNullSources(t *testing.T) {
	for _, scenario := range []struct{ sql, code, location string }{
		{`SELECT (NULL::int4[])[missing_bound]`, "42703", "missing_bound"},
		{`SELECT (NULL::int4[])[true]`, "42804", "true"},
		{`SELECT (NULL::int4[])[1:missing_bound]`, "42703", "missing_bound"},
		{`SELECT (ARRAY[1])[1][1][1][1][1][1][1]`, "54000", "ARRAY"},
		{`SELECT (ARRAY[1])[:][:][:][:][:][:][:]`, "54000", "ARRAY"},
	} {
		_, err := bindSQL(t, scenario.sql, nil, nil)
		requireCode(t, err, scenario.code)
		var sqlErr *model.Error
		if !errors.As(err, &sqlErr) || sqlErr.Position != strings.Index(scenario.sql, scenario.location)+1 {
			t.Fatalf("%s: error position = %v", scenario.sql, err)
		}
	}
}

func TestArrayFetchLeavesUnsupportedIndirectionExplicit(t *testing.T) {
	_, err := bindSQL(t, `SELECT (1)[1]`, nil, nil)
	var missing *bind.MissingBindingError
	if !errors.As(err, &missing) {
		t.Fatalf("non-array subscript error=%v, want required-binding failure", err)
	}
}

func TestBoundArraySliceEvaluatesSourceOnceAndBoundsInOrder(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray, Modifier: -1}, Children: []model.TypeLayout{integer}}
	snapshot := tableSnapshot(t)
	for i, name := range []string{"source_array", "upper_one", "upper_two", "lower_one", "lower_two"} {
		result, body := integer, "SELECT 1"
		if i == 0 {
			result, body = integers, "SELECT ARRAY[[7]]"
		}
		snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
			ID: model.ObjectID(20 + i), OID: model.OID(90020 + i), Version: 1, State: model.StatePublic,
			Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: name},
			Routine: new(compiledRoutine(t, snapshot, body, model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: result, Language: "sql", Volatility: "volatile"})),
		}))
	}
	statement, err := bindSQL(t, `SELECT (public.source_array())[public.lower_one():public.upper_one()][public.lower_two():public.upper_two()]`, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	if effects := statement.Effects(); effects.Volatility != ir.Volatile || !effects.MayError {
		t.Fatalf("guarded effects = %+v, want volatile and may-error", effects)
	}
	graph := statement.Graph()
	root, ok := graph.Binding(graph.Root(), 0)
	if !ok {
		t.Fatal("missing output binding")
	}
	for _, scenario := range []struct {
		name       string
		nullSource bool
		nullUpper  bool
	}{
		{"nonnull source", false, false},
		{"null source", true, false},
		{"null first upper", false, true},
	} {
		for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
			t.Run(scenario.name, func(t *testing.T) {
				var calls []model.FunctionID
				options := expr.Options{Limits: expr.DefaultLimits(), Functions: onceResolver(func(function expr.Function) (expr.ScalarCall, error) {
					return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
						calls = append(calls, function.ID)
						if function.ID == 90020 {
							if scenario.nullSource {
								return model.NullVal(model.TypeArray), nil
							}
							return model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(7)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}}}, nil
						}
						if function.ID == 90021 && scenario.nullUpper {
							return model.NullVal(model.TypeInt64), nil
						}
						return model.Int64Val(1), nil
					}, nil
				})}
				program, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root.Expr}, options)
				if err != nil {
					t.Fatal(err)
				}
				state, err := expr.NewState(program, expr.StateOptions{})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := state.Close(); err != nil {
						t.Error(err)
					}
				})
				input, out := columnar.Batch{Length: 1}, columnar.Batch{}
				if err := state.Eval(t.Context(), &input, nil, &out, mode); err != nil {
					t.Fatal(err)
				}
				wantCalls := []model.FunctionID{90020, 90021, 90022, 90023, 90024}
				if scenario.nullSource {
					wantCalls = wantCalls[:1]
				}
				if !slices.Equal(calls, wantCalls) {
					t.Fatalf("mode %d calls = %v, want %v", mode, calls, wantCalls)
				}
				value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], 0, 1024)
				if err != nil {
					t.Fatal(err)
				}
				if wantNull := scenario.nullSource || scenario.nullUpper; value.IsNull != wantNull {
					t.Fatalf("slice = %+v, want NULL %t", value, wantNull)
				}
				if !value.IsNull && (len(value.ArrayVal) != 1 || value.ArrayVal[0].IntVal != 7) {
					t.Fatalf("slice = %+v, want one element 7", value)
				}
			})
		}
	}
}

func boundArrayFetch(t *testing.T, graph *ir.Graph) (ir.ScalarID, ir.Scalar) {
	t.Helper()
	for id := ir.ScalarID(1); uint32(id) <= graph.ScalarCount(); id++ {
		scalar, _ := graph.Scalar(id)
		if scalar.Kind == ir.ArrayIndex || scalar.Kind == ir.ArraySlice {
			return id, scalar
		}
	}
	t.Fatal("missing typed array fetch")
	return 0, ir.Scalar{}
}
