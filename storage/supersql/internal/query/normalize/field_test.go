package normalize_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
)

func TestFieldNormalizesUnusedConstantErrors(t *testing.T) {
	for _, sql := range []string{
		`SELECT (ROW(1/0,2)).f2`,
		`SELECT (ROW(2,1/0)).f1`,
		`SELECT (s.r).f2 FROM (SELECT ROW(1/0,2) AS r)s`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := normalize.Normalize(t.Context(), bound(t, sql), options())
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
				t.Fatalf("normalization error=%v, want planning 22012", err)
			}
		})
	}
}

func TestFieldRuntimeEvaluation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		projected  bool
		limited    bool
		volatility ir.Volatility
		selected   uint32
		callError  error
		wantError  error
		wantCalls  int
		want       int64
	}{
		{name: "direct skips unused runtime error", selected: 1, callError: errDeadArm, want: 2},
		{name: "projected skips unused runtime error", projected: true, selected: 1, callError: errDeadArm, want: 2},
		{name: "direct preserves selected runtime error", callError: errDeadArm, wantError: errDeadArm, wantCalls: 1},
		{name: "projected preserves selected runtime error", projected: true, callError: errDeadArm, wantError: errDeadArm, wantCalls: 1},
		{name: "direct skips unused volatile call", volatility: ir.Volatile, selected: 1, want: 2},
		{name: "direct preserves selected volatile call", volatility: ir.Volatile, wantCalls: 1, want: 7},
		{name: "projected volatile row is a barrier", projected: true, volatility: ir.Volatile, selected: 1, wantCalls: 1, want: 2},
		{name: "limit preserves unused runtime error", projected: true, limited: true, selected: 1, callError: errDeadArm, wantError: errDeadArm, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
			rowLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{integer, integer}})
			if err != nil {
				t.Fatal(err)
			}
			effects := ir.Effects{MayError: tc.callError != nil, Volatility: tc.volatility}
			signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: 990007, Result: f.integer, Effects: effects}})
			if err != nil {
				t.Fatal(err)
			}
			call := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.integer, Function: 990007, Signature: signature, Effects: effects}})
			two := f.constant(f.integer, model.Int64Val(2))
			row := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Row, Scope: f.scope, Layout: rowLayout, Effects: effects}, Operands: []ir.ScalarID{call, two}})
			input := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: f.scope, Rows: 1}})
			fieldEffects := effects
			if tc.projected {
				slot := f.slot(rowLayout, false)
				input = f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope, Effects: effects}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{slot}, Bindings: []ir.Binding{{Slot: slot, Expr: row}}})
				row = f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: f.scope, Layout: rowLayout, Slot: slot}})
				fieldEffects = ir.Effects{}
				if tc.limited {
					one := f.constant(f.integer, model.Int64Val(1))
					input = f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Limit, Scope: f.scope, Count: one, Effects: effects}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{slot}})
				}
			}
			field := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Field, Scope: f.scope, Layout: f.integer, FieldIndex: tc.selected, Nullable: tc.projected, Effects: fieldEffects}, Operands: []ir.ScalarID{row}})
			output := f.slot(f.integer, tc.projected)
			root := f.rel(ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: f.scope, Effects: effects}, Inputs: []ir.RelID{input}, Outputs: []model.SlotID{output}, Bindings: []ir.Binding{{Slot: output, Expr: field}}})
			graph := normalized(t, f.freeze(root))
			calls := 0
			functions := resolver(func(expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls++
					return model.Int64Val(7), tc.callError
				}, nil
			})
			batch, err := evaluateFieldProjects(t, graph, graph.Root(), functions)
			if !errors.Is(err, tc.wantError) {
				t.Fatalf("execution error=%v, want %v", err, tc.wantError)
			}
			if tc.wantError == nil {
				value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], 0, 1<<20)
				if err != nil || value.IsNull || value.IntVal != tc.want {
					t.Fatalf("field result=%+v, %v, want %d", value, err, tc.want)
				}
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func evaluateFieldProjects(t *testing.T, graph *ir.Graph, id ir.RelID, functions expr.Resolver) (columnar.Batch, error) {
	t.Helper()
	relation, _ := graph.Rel(id)
	if relation.Kind == ir.Values {
		if relation.Rows != 1 || relation.OutputCount() != 0 {
			t.Fatal("field fixture requires one zero-column input row")
		}
		return columnar.Batch{Length: 1}, nil
	}
	inputID, _ := graph.Input(id, 0)
	input, err := evaluateFieldProjects(t, graph, inputID, functions)
	if err != nil {
		return columnar.Batch{}, err
	}
	if relation.Kind == ir.Limit {
		count, err := graph.CloneConstant(t.Context(), relation.Count, 1024)
		if err != nil || count.IntVal != 1 || input.Length != 1 {
			t.Fatalf("field fixture requires LIMIT 1 over one row: count=%+v input=%d error=%v", count, input.Length, err)
		}
		return input, nil
	}
	if relation.Kind != ir.Project {
		t.Fatalf("unsupported field fixture relation %v", relation.Kind)
	}
	child, _ := graph.Rel(inputID)
	inputs := make([]expr.Input, child.OutputCount())
	for i := range inputs {
		slot, _ := graph.Output(inputID, uint32(i))
		inputs[i] = expr.Input{Slot: slot, Column: i}
	}
	roots := make([]ir.ScalarID, relation.BindingCount())
	for i := range roots {
		binding, _ := graph.Binding(id, uint32(i))
		roots[i] = binding.Expr
	}
	program, err := expr.Compile(t.Context(), graph, roots, expr.Options{Limits: expr.DefaultLimits(), Inputs: inputs, Functions: functions, Builtin: options().Builtin})
	if err != nil {
		return columnar.Batch{}, err
	}
	state, err := expr.NewState(program, expr.StateOptions{})
	if err != nil {
		return columnar.Batch{}, err
	}
	t.Cleanup(func() {
		if err := state.Close(); err != nil {
			t.Error(err)
		}
	})
	var output columnar.Batch
	err = state.Eval(t.Context(), &input, nil, &output, expr.Direct)
	return output, err
}

func TestFieldPreservesSelectedTypeAndNullability(t *testing.T) {
	graph := normalized(t, bound(t, `SELECT (ROW(1, NULL::varchar(7) COLLATE "C")).f2`))
	root, _ := graph.Scalar(expression(t, graph))
	typ, _ := graph.Type(root.Layout)
	if typ.ID != 1043 || typ.Modifier != 11 || typ.Collation != 950 || !root.Nullable {
		t.Fatalf("selected field type=%+v nullable=%v", typ, root.Nullable)
	}
	values, err := evaluate(t, graph, []ir.ScalarID{expression(t, graph)}, nil, 1)
	if err != nil || !values[0][0].IsNull {
		t.Fatalf("selected nullable field=%+v, %v", values, err)
	}
}
