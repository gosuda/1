package bind_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestIndexTimezoneContextAndDataIdentitySurviveRebinding(t *testing.T) {
	const sql = `CREATE INDEX timezone_context ON public."Mixed" ("X") WHERE (make_timestamp(2000,1,1,0,0,0) AT TIME ZONE 'LMT') = to_timestamp(946702562)`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := indexClassSnapshot(t)
	options := bindingOptions(sql)
	options.Builtin.TimeZone = builtin.TimeZoneContext{Name: "America/New_York"}
	compiler, err := session.NewIndexExpressionCompiler(snapshot, session.IndexExpressionOptions{Binding: options.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}
	bound, err := bind.BindCreateIndex(t.Context(), parsed.Stmts[0].Stmt.GetIndexStmt(), snapshot, bind.CreateIndexOptions{Bind: options, Descriptors: limits, ObjectID: 12, OID: 10012, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Expressions: compiler})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := bound.Index.SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	result := model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}}
	program, err := compiler.CompileIndexExpression(t.Context(), descriptor.Index.Predicate, nil, result)
	if err != nil {
		t.Fatal(err)
	}
	value, err := program.Evaluate(t.Context(), nil, model.ExpressionRuntime{})
	if err != nil || value.IsNull || value.IntVal != 1 {
		t.Fatalf("rebound timezone predicate: %v, %v", value, err)
	}
	changed := *descriptor.Index.Predicate
	changed.Encoding = bytes.Replace(changed.Encoding, []byte(builtin.TimeZoneDataSHA256), []byte(strings.Repeat("0", len(builtin.TimeZoneDataSHA256))), 1)
	_, err = compiler.CompileIndexExpression(t.Context(), &changed, nil, result)
	requireCode(t, err, "0A000")
}

func TestCheckFreezesPendingRowAndEffectfulRoutine(t *testing.T) {
	for _, volatility := range []string{"stable", "volatile"} {
		t.Run(volatility, func(t *testing.T) {
			snapshot := tableSnapshot(t)
			table := snapshot.descriptors[1]
			integer := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}}
			boolean := model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}}
			routine := defaultRoutineDescriptor(t, 40, 90040, 10, "check_value", []model.RoutineParameter{{Name: "x", Layout: integer, Mode: "in"}}, boolean, volatility)
			snapshot.descriptors = []*model.Descriptor{snapshot.descriptors[0], routine}
			const sql = `SELECT check_value("X")`
			parsed, err := zpgparse.Parse(sql)
			if err != nil {
				t.Fatal(err)
			}
			options := bindingOptions(sql)
			ast := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
			definition, err := bind.DefineCheck(t.Context(), ast, table, 50, snapshot, options)
			if err != nil {
				t.Fatal(err)
			}
			snapshot.descriptors = append(snapshot.descriptors, table)
			calls := 0
			compiler, err := session.NewColumnExpressionCompiler(snapshot, session.ColumnExpressionOptions{
				Binding: options.Limits, Evaluation: expr.DefaultLimits(), InputBytes: 1 << 20, ResultBytes: 1 << 20,
				Builtin: builtin.ResolveOptions{Collation: options.Collation, DefaultCollation: options.DefaultCollation},
				Functions: onceResolver(func(function expr.Function) (expr.ScalarCall, error) {
					if function.ID != 90040 {
						return nil, &builtin.UnknownFunctionError{ID: function.ID}
					}
					return func(ctx context.Context, args []model.Value, _ builtin.CallBudget) (model.Value, error) {
						if err := ctx.Err(); err != nil {
							return model.Value{}, err
						}
						calls++
						return model.BoolVal(args[0].IntVal > 0), nil
					}, nil
				}),
			})
			if err != nil {
				t.Fatal(err)
			}
			columns := []storage.ColumnLayout{{ID: 1, Type: integer.Type}}
			program, err := compiler.CompileCheck(t.Context(), definition, 50, columns)
			if err != nil {
				t.Fatal(err)
			}
			for _, input := range []int64{3, -2} {
				value, err := program.Evaluate(t.Context(), []model.Value{model.Int64Val(input)}, model.ExpressionRuntime{})
				if err != nil || value.IsNull || (value.IntVal == 1) != (input > 0) {
					t.Fatalf("CHECK(%d) = %v, %v", input, value, err)
				}
			}
			if calls != 2 {
				t.Fatalf("CHECK routine calls = %d, want 2", calls)
			}
			_, err = compiler.CompileCheck(t.Context(), definition, 51, columns)
			requireCode(t, err, "42804")
			_, err = bind.BindIndexExpression(t.Context(), definition, columns, boolean, snapshot, options.Limits)
			if err == nil {
				t.Fatal("CHECK definition accepted as an index expression")
			}
			snapshot.descriptors[1] = defaultRoutineDescriptor(t, 40, 90041, 10, "check_value", []model.RoutineParameter{{Name: "x", Layout: integer, Mode: "in"}}, boolean, volatility)
			_, err = compiler.CompileCheck(t.Context(), definition, 50, columns)
			requireCode(t, err, "0A000")
			value, err := program.Evaluate(t.Context(), []model.Value{model.Int64Val(7)}, model.ExpressionRuntime{})
			if err != nil || value.IsNull || value.IntVal != 1 || calls != 3 {
				t.Fatalf("detached CHECK after catalog change = %v, %v; calls = %d", value, err, calls)
			}
		})
	}
}

func TestCheckRejectsNonRowScalarDefinitions(t *testing.T) {
	snapshot := tableSnapshot(t)
	for _, expression := range []string{`42`, `$1 > 0`, `(SELECT true)`, `sum("X") > 0`, `row_number() OVER () > 0`, `generate_series(1, 2) > 0`, `other."X" > 0`} {
		t.Run(expression, func(t *testing.T) {
			sql := "SELECT " + expression
			parsed, err := zpgparse.Parse(sql)
			if err != nil {
				t.Fatal(err)
			}
			ast := parsed.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val
			_, err = bind.DefineCheck(t.Context(), ast, snapshot.descriptors[1], 50, snapshot, bindingOptions(sql))
			if err == nil {
				t.Fatalf("accepted CHECK expression %s", expression)
			}
		})
	}
}
