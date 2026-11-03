package bind_test

import (
	"context"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type conflictIndexCompiler struct{}

func (conflictIndexCompiler) CheckIndexExpression(ctx context.Context, binding *bind.BoundIndexExpression) error {
	options := expr.Options{Limits: expr.DefaultLimits(), Builtin: binding.Builtin}
	for i, parameter := range binding.Parameters {
		options.Parameters = append(options.Parameters, expr.Parameter{Number: uint32(i + 1), Layout: parameter.Layout})
	}
	_, err := expr.Compile(ctx, binding.Graph, []ir.ScalarID{binding.Root}, options)
	return err
}
func conflictInferenceSnapshot(t *testing.T, indexSQL string) metadataSnapshot {
	t.Helper()
	snapshot := tableSnapshot(t)
	limits := model.DescriptorLimits{MaxBytes: 1 << 20, MaxItems: 100, MaxDepth: 32}
	spec, err := snapshot.descriptors[1].SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	spec.Table.ColumnIDHighWatermark = 2
	spec.Table.Columns = append(spec.Table.Columns, model.ColumnDescriptor{ID: 2, Name: "id", State: model.StatePublic, Layout: spec.Table.Columns[0].Layout})
	spec.Table.PrimaryKey = []model.KeyElement{{Column: 2, Type: spec.Table.Columns[0].Layout.Type}}
	snapshot.descriptors[1] = testDescriptor(t, spec)
	parsed, err := zpgparse.Parse(indexSQL)
	if err != nil {
		t.Fatal(err)
	}
	created, err := bind.BindCreateIndex(t.Context(), parsed.Stmts[0].Stmt.GetIndexStmt(), snapshot, bind.CreateIndexOptions{Bind: bindingOptions(indexSQL), Descriptors: limits, ObjectID: 12, OID: 10012, Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1}, Expressions: conflictIndexCompiler{}})
	if err != nil {
		t.Fatal(err)
	}
	indexSpec, err := created.Index.SpecWithin(t.Context(), limits)
	if err != nil {
		t.Fatal(err)
	}
	indexSpec.State = model.StatePublic
	spec.Table.Indexes = []model.ObjectID{12}
	snapshot.descriptors[1] = testDescriptor(t, spec)
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, indexSpec))
	return snapshot
}
func TestConflictInferencePreservesPredicateAndExpressionSemantics(t *testing.T) {
	for _, scenario := range []struct{ name, index, target, code string }{
		{"stronger range", `CREATE UNIQUE INDEX ux ON public."Mixed" ("X") WHERE "X">0`, `("X") WHERE "X">5`, ""},
		{"weaker range", `CREATE UNIQUE INDEX ux ON public."Mixed" ("X") WHERE "X">5`, `("X") WHERE "X">0`, "42P10"},
		{"commuted range", `CREATE UNIQUE INDEX ux ON public."Mixed" ("X") WHERE "X">0`, `("X") WHERE 5<"X"`, ""},
		{"inclusive boundary", `CREATE UNIQUE INDEX ux ON public."Mixed" ("X") WHERE "X">0`, `("X") WHERE "X">=0`, "42P10"},
		{"disjunction", `CREATE UNIQUE INDEX ux ON public."Mixed" ("X") WHERE "X">0`, `("X") WHERE "X"=1 OR "X"=2`, ""},
		{"unsafe disjunction", `CREATE UNIQUE INDEX ux ON public."Mixed" ("X") WHERE "X">0`, `("X") WHERE "X"=1 OR "X"=-1`, "42P10"},
		{"negated comparison", `CREATE UNIQUE INDEX ux ON public."Mixed" ("X") WHERE "X">0`, `("X") WHERE NOT ("X"<=5)`, ""},
		{"expression identity", `CREATE UNIQUE INDEX ux ON public."Mixed" (("X"+1))`, `(("X"+1))`, ""},
		{"different expression constant", `CREATE UNIQUE INDEX ux ON public."Mixed" (("X"+1))`, `(("X"+2))`, "42P10"},
		{"key order", `CREATE UNIQUE INDEX ux ON public."Mixed" ("X",id)`, `(id,"X")`, ""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			snapshot := conflictInferenceSnapshot(t, scenario.index)
			sql := `INSERT INTO public."Mixed" ("X",id) VALUES(1,1) ON CONFLICT ` + scenario.target + ` DO NOTHING`
			parsed, err := zpgparse.Parse(sql)
			if err != nil {
				t.Fatal(err)
			}
			_, err = bind.Bind(t.Context(), parsed.Stmts[0], snapshot, bindingOptions(sql), nil)
			if scenario.code != "" {
				requireCode(t, err, scenario.code)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
