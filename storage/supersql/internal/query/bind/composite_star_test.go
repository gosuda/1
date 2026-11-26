package bind_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestCompositeStarNestedRowsKeepFieldOrder(t *testing.T) {
	const sql = `SELECT (ROW(9,(ROW(1,'x'::text)).*)).* AS ignored`
	statement, err := bindSQL(t, sql, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 3 {
		t.Fatalf("columns = %+v, want three flattened fields", columns)
	}
	for i, name := range []string{"f1", "f2", "f3"} {
		if columns[i].Name != name {
			t.Errorf("column %d name = %q, want %q", i, columns[i].Name, name)
		}
	}
	values := evaluateCompositeStar(t, statement, sql, expr.Options{Limits: expr.DefaultLimits()})
	if values[0].IntVal != 9 || values[1].IntVal != 1 || values[2].StrVal != "x" {
		t.Fatalf("expanded values = %+v, want 9, 1, x", values)
	}
}

func TestCompositeStarFieldArrayPrefixPreservesNamedLayout(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: 12, Storage: model.TypeString, Collation: 951}}
	integers := model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}}
	// Attribute IDs stay sparse so the dependency assertion below still pins the
	// descriptors the expansion actually reads.
	fields := []model.ColumnDescriptor{
		{ID: 1, Name: "label", State: model.StatePublic, Layout: text},
		{ID: 3, Name: "numbers", State: model.StatePublic, Layout: integers},
	}
	snapshot, array := unnestNamedSnapshot(t, fields, []model.TypeLayout{text, integers})
	statement, err := bindSQL(t, `SELECT (ROW($1)).f1[1].* AS ignored`, snapshot, []model.TypeLayout{array})
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 2 || columns[0].Name != "label" || columns[1].Name != "numbers" || !columns[0].Nullable || !columns[1].Nullable {
		t.Fatalf("expanded columns = %+v, want nullable label and numbers", columns)
	}
	requireArrayAggregateLayout(t, columns[0].Layout, text)
	requireArrayAggregateLayout(t, columns[1].Layout, integers)
	for _, field := range []model.ColumnID{1, 3} {
		found := false
		for _, dependency := range statement.Dependencies() {
			found = found || dependency == (model.Dependency{Object: 30, Version: 3, Column: field})
		}
		if !found {
			t.Errorf("missing named composite dependency for field %d", field)
		}
	}
}

func TestRowRelationStarRespectsQualifiedJoinFields(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (ROW(l.*,r.*)).* FROM (VALUES(1,'x'::text))l(k,a) JOIN (VALUES(1,true))r(k,b) USING(k)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 4 {
		t.Fatalf("qualified joined fields = %+v, want both copies of k", columns)
	}
	for i, typ := range []model.TypeID{23, 25, 23, 16} {
		if columns[i].Name != fmt.Sprintf("f%d", i+1) || columns[i].Layout.Type.ID != typ {
			t.Errorf("field %d = %+v, want type %d", i, columns[i], typ)
		}
	}
}

func TestRowRelationStarCapturesOuterScope(t *testing.T) {
	statement, err := bindSQL(t, `SELECT (SELECT ROW(v.*)) FROM (VALUES(1,'x'::text))v(a,b)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 1 || len(columns[0].Layout.Children) != 2 || columns[0].Layout.Children[0].Type.ID != 23 || columns[0].Layout.Children[1].Type.ID != 25 {
		t.Fatalf("outer row layout = %+v, want record(int4,text)", columns)
	}
}

func TestCompositeStarRequiresRegisteredDescriptor(t *testing.T) {
	for _, sql := range []string{
		`SELECT (1).*`,
		`SELECT (NULL::record).*`,
		`SELECT (ARRAY[ROW(1)])[1].*`,
		`SELECT (ROW(ROW(1))).f1.*`,
		`SELECT (v.r).* FROM (VALUES(ROW(1)))v(r)`,
		`SELECT ROW((1).*)`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, nil, nil)
			requireCode(t, err, "42809")
		})
	}
}

func TestCompositeStarRejectsScalarContexts(t *testing.T) {
	for _, sql := range []string{
		`SELECT ((ROW(1)).*) IS NULL`,
		`SELECT ((1).*) IS NULL`,
		`SELECT coalesce((ROW(1)).*, ROW(2))`,
		`SELECT abs((ROW(1)).*)`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, nil, nil)
			requireCode(t, err, "0A000")
		})
	}
}

func TestCompositeStarEmptyNamedDescriptorAddsNoFields(t *testing.T) {
	snapshot, array := unnestNamedSnapshot(t, nil, nil)
	statement, err := bindSQL(t, `SELECT ROW(($1)[1].*)`, snapshot, []model.TypeLayout{array})
	if err != nil {
		t.Fatal(err)
	}
	columns := statement.Columns()
	if len(columns) != 1 || columns[0].Layout.Type.ID != 2249 || len(columns[0].Layout.Children) != 0 {
		t.Fatalf("empty expanded row = %+v, want record()", columns)
	}
	statement, err = bindSQL(t, `SELECT ($1)[1].*`, snapshot, []model.TypeLayout{array})
	if err != nil {
		t.Fatal(err)
	}
	if columns := statement.Columns(); len(columns) != 0 {
		t.Fatalf("empty composite produced columns: %+v", columns)
	}
}

func TestCompositeStarRepeatsVolatileParentPerField(t *testing.T) {
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	fields := []model.ColumnDescriptor{{ID: 1, Name: "first", State: model.StatePublic, Layout: integer}, {ID: 2, Name: "second", State: model.StatePublic, Layout: integer}}
	snapshot, array := unnestNamedSnapshot(t, fields, []model.TypeLayout{integer, integer})
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 31, OID: 90012, Version: 1, State: model.StatePublic,
		Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "changing"},
		Routine: new(compiledRoutine(t, snapshot, "SELECT ROW(0,0)", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: array.Children[0], Language: "sql", Volatility: "volatile"})),
	}))
	for _, test := range []struct {
		sql string
		row bool
	}{
		{`SELECT (outside_search_path.changing()).*`, false},
		{`SELECT ROW((outside_search_path.changing()).*)`, true},
		{`SELECT (ROW((outside_search_path.changing()).*)).*`, false},
	} {
		t.Run(test.sql, func(t *testing.T) {
			statement, err := bindSQL(t, test.sql, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			if statement.Effects().Volatility != ir.Volatile || !statement.Effects().MayError {
				t.Fatalf("expanded effects = %+v, want volatile and may-error", statement.Effects())
			}
			calls := int64(0)
			values := evaluateCompositeStar(t, statement, test.sql, expr.Options{Limits: expr.DefaultLimits(), Functions: onceResolver(func(expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls++
					return model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.Int64Val(calls), model.Int64Val(calls * 10)}}, nil
				}, nil
			})})
			if test.row {
				values = values[0].ArrayVal
			}
			if calls != 2 || len(values) != 2 || values[0].IntVal != 1 || values[1].IntVal != 20 {
				t.Fatalf("calls=%d values=%+v, want two calls and 1,20", calls, values)
			}
		})
	}
}

func TestCompositeStarExpansionLimits(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit func(*ir.Limits)
	}{
		{"nodes", func(l *ir.Limits) { l.MaxNodes = 120 }},
		{"operands", func(l *ir.Limits) { l.MaxOperands = 80 }},
		{"slots", func(l *ir.Limits) { l.MaxSlots = 5 }},
		{"bytes", func(l *ir.Limits) { l.MaxBytes = 4096 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			sql := `SELECT (ROW(1,2,3,4,5,6,7,8,9,10)).*`
			parsed, err := zpgparse.Parse(sql)
			if err != nil {
				t.Fatal(err)
			}
			options := bindingOptions(sql)
			test.limit(&options.Limits)
			statement, err := bind.Bind(t.Context(), parsed.Stmts[0], nil, options, nil)
			if statement != nil {
				t.Fatal("oversized expansion returned a partial statement")
			}
			requireCode(t, err, "54000")
		})
	}
}

func TestCompositeStarCatalogCancellation(t *testing.T) {
	snapshot, array := unnestNamedSnapshot(t, nil, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sql := `SELECT ($1)[1].*`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	_, err = bind.Bind(ctx, parsed.Stmts[0], unnestCancelSnapshot{snapshot, cancel}, bindingOptions(sql), []model.TypeLayout{array})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("binding error = %v, want cancellation during descriptor discovery", err)
	}
}

func evaluateCompositeStar(t *testing.T, statement *bind.BoundStatement, sql string, options expr.Options) []model.Value {
	t.Helper()
	graph, err := normalize.Normalize(t.Context(), statement.Graph(), normalize.Options{
		Limits: bindingOptions(sql).Limits, MaxWork: 1000000, MaxBytes: 16 << 20,
		Builtin: builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, ExtraFloatDigits: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	roots := make([]ir.ScalarID, len(statement.Columns()))
	for i := range roots {
		binding, ok := graph.Binding(graph.Root(), uint32(i))
		if !ok {
			t.Fatalf("missing output binding %d", i)
		}
		roots[i] = binding.Expr
	}
	program, err := expr.Compile(t.Context(), graph, roots, options)
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
	input, output := columnar.Batch{Length: 1}, columnar.Batch{}
	if err := state.Eval(t.Context(), &input, nil, &output, expr.Direct); err != nil {
		t.Fatal(err)
	}
	values := make([]model.Value, len(roots))
	for i := range values {
		values[i], _, err = columnar.CopyValue(t.Context(), &output.Columns[i], 0, 4096)
		if err != nil {
			t.Fatal(err)
		}
	}
	return values
}
