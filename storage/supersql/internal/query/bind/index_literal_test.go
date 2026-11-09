package bind_test

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type literalIndexCompiler struct{}

func (literalIndexCompiler) CheckIndexExpression(ctx context.Context, binding *bind.BoundIndexExpression) error {
	_, err := expr.Compile(ctx, binding.Graph, []ir.ScalarID{binding.Root}, expr.Options{Limits: expr.DefaultLimits(), Builtin: binding.Builtin})
	return err
}

func TestIndexLiteralSettingsSurviveDescriptorRebinding(t *testing.T) {
	const sql = `CREATE INDEX literal_style ON public."Mixed" ("X") WHERE interval '-1 2:00' < interval '-1 day'`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := indexClassSnapshot(t)
	options := bindingOptions(sql)
	options.Builtin.IntervalStyle = builtin.IntervalSQLStandard
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 20}
	bound, err := bind.BindCreateIndex(t.Context(), parsed.Stmts[0].Stmt.GetIndexStmt(), snapshot, bind.CreateIndexOptions{
		Bind: options, Descriptors: limits, ObjectID: 12, OID: 10012,
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Expressions: literalIndexCompiler{},
	})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := bound.Index.SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	result := model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}}
	binding, err := bind.BindIndexExpression(t.Context(), descriptor.Index.Predicate, nil, result, snapshot, options.Limits)
	if err != nil {
		t.Fatal(err)
	}
	program, err := expr.Compile(t.Context(), binding.Graph, []ir.ScalarID{binding.Root}, expr.Options{Limits: expr.DefaultLimits(), Builtin: binding.Builtin})
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
	value, _, err := columnar.CopyValue(t.Context(), &output.Columns[0], 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if value.IsNull || value.IntVal != 1 {
		t.Fatalf("rebound SQL-standard predicate=%v, want true", value)
	}
}
