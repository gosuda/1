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
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type onceResolver func(expr.Function) (expr.ScalarCall, error)

func (r onceResolver) Resolve(f expr.Function) (expr.ScalarCall, error) { return r(f) }

func onceCallAST() *zpgparse.Node {
	return zpgparse.MakeFuncCallNode([]*zpgparse.Node{zpgparse.MakeStrNode("public"), zpgparse.MakeStrNode("changing")}, nil, -1)
}

func onceCaseAST() *zpgparse.Node {
	n := zpgparse.MakeCaseExprNode(onceCallAST(), []*zpgparse.Node{
		zpgparse.MakeCaseWhenNode(zpgparse.MakeAConstIntNode(0, -1), zpgparse.MakeAConstIntNode(10, -1), -1),
		zpgparse.MakeCaseWhenNode(zpgparse.MakeAConstIntNode(2, -1), zpgparse.MakeAConstIntNode(20, -1), -1),
	}, -1)
	n.GetCaseExpr().Defresult = zpgparse.MakeAConstIntNode(30, -1)
	return n
}

func onceInAST(operator string) *zpgparse.Node {
	return zpgparse.MakeAExprNode(zpgparse.A_Expr_Kind_AEXPR_IN, []*zpgparse.Node{zpgparse.MakeStrNode(operator)}, onceCallAST(), zpgparse.MakeListNode([]*zpgparse.Node{
		zpgparse.MakeAConstIntNode(0, -1), zpgparse.MakeAConstIntNode(2, -1),
		{Node: &zpgparse.Node_AConst{AConst: &zpgparse.A_Const{Isnull: true, Location: -1}}},
	}), -1)
}

func bindOnceAST(t *testing.T, node *zpgparse.Node) (*ir.Graph, ir.ScalarID) {
	t.Helper()
	snapshot := tableSnapshot(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	snapshot.descriptors = append(snapshot.descriptors, testDescriptor(t, model.DescriptorSpec{
		ID: 12, OID: 90012, Version: 2, State: model.StatePublic,
		Name:    model.NamespaceName{Parent: 10, Kind: model.ObjectRoutine, Name: "changing"},
		Routine: new(compiledRoutine(t, snapshot, "SELECT 2", model.RoutineMetadata{Owner: 10, LanguageOID: 14, Parallel: "unsafe", Cost: 100, Result: integer, Language: "sql", Volatility: "volatile"})),
	}))
	raw := &zpgparse.RawStmt{Stmt: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: &zpgparse.SelectStmt{
		Op: zpgparse.SetOperation_SETOP_NONE, TargetList: []*zpgparse.Node{zpgparse.MakeResTargetNodeWithVal(node, -1)},
	}}}}
	options := bindingOptions("")
	options.Limits.SourceBytes = 1024
	statement, err := bind.Bind(t.Context(), raw, snapshot, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if statement.Effects().Volatility != ir.Volatile {
		t.Fatal("single evaluation hid volatile effects")
	}
	graph := statement.Graph()
	binding, ok := graph.Binding(graph.Root(), 0)
	if !ok {
		t.Fatal("missing result binding")
	}
	return graph, binding.Expr
}

func TestBoundSimpleCaseEvaluatesLeftOnce(t *testing.T) {
	testBoundOnceOutput(t, onceCaseAST(), []model.Value{model.Int64Val(20), model.Int64Val(30), model.Int64Val(30)})
}

func TestBoundInEvaluatesLeftOnce(t *testing.T) {
	testBoundOnceOutput(t, onceInAST("="), []model.Value{model.BoolVal(true), model.NullVal(model.TypeBoolean), model.NullVal(model.TypeBoolean)})
}

func TestBoundNotInEvaluatesLeftOnce(t *testing.T) {
	testBoundOnceOutput(t, onceInAST("<>"), []model.Value{model.BoolVal(false), model.NullVal(model.TypeBoolean), model.NullVal(model.TypeBoolean)})
}

func testBoundOnceOutput(t *testing.T, node *zpgparse.Node, expected []model.Value) {
	t.Helper()
	for _, rows := range []int{1, 17, 1024} {
		for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
			t.Run(fmt.Sprintf("rows%d/mode%d", rows, mode), func(t *testing.T) {
				assertBoundOnceOutput(t, node, expected, rows, mode)
			})
		}
	}
}

func assertBoundOnceOutput(t *testing.T, node *zpgparse.Node, expected []model.Value, rows int, mode expr.Mode) {
	t.Helper()
	graph, root := bindOnceAST(t, node)
	calls := 0
	inputs := []model.Value{model.Int64Val(2), model.Int64Val(3), model.NullVal(model.TypeInt64)}
	options := expr.Options{Limits: expr.DefaultLimits(), Functions: onceResolver(func(expr.Function) (expr.ScalarCall, error) {
		return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
			value := inputs[calls%len(inputs)]
			calls++
			return value, nil
		}, nil
	})}
	program, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root}, options)
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
	input, out := columnar.Batch{Length: rows}, columnar.Batch{}
	for invocation := range 2 {
		if err := state.Eval(t.Context(), &input, nil, &out, mode); err != nil {
			t.Fatal(err)
		}
		if calls != (invocation+1)*rows {
			t.Fatalf("calls=%d want %d", calls, (invocation+1)*rows)
		}
		for lane := range rows {
			got, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], lane, 1024)
			if err != nil {
				t.Fatal(err)
			}
			want := expected[(invocation*rows+lane)%len(expected)]
			if got.IsNull != want.IsNull || !want.IsNull && got.IntVal != want.IntVal {
				t.Fatalf("lane %d: got %+v want %+v", lane, got, want)
			}
		}
	}
}

var errInactiveOnceCall = errors.New("inactive call")

func TestBoundSimpleCaseInactiveOuterArmSuppressesErrors(t *testing.T) {
	for _, mode := range []expr.Mode{expr.Direct, expr.ScalarCalls} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			falseNode := &zpgparse.Node{Node: &zpgparse.Node_AConst{AConst: &zpgparse.A_Const{Val: &zpgparse.A_Const_Boolval{Boolval: &zpgparse.Boolean{Boolval: false}}, Location: -1}}}
			outer := zpgparse.MakeCaseExprNode(nil, []*zpgparse.Node{zpgparse.MakeCaseWhenNode(falseNode, onceCaseAST(), -1)}, -1)
			outer.GetCaseExpr().Defresult = zpgparse.MakeAConstIntNode(7, -1)
			graph, root := bindOnceAST(t, outer)
			calls := 0
			options := expr.Options{Limits: expr.DefaultLimits(), Functions: onceResolver(func(expr.Function) (expr.ScalarCall, error) {
				return func(context.Context, []model.Value, builtin.CallBudget) (model.Value, error) {
					calls++
					return model.Value{}, errInactiveOnceCall
				}, nil
			})}
			program, err := expr.Compile(t.Context(), graph, []ir.ScalarID{root}, options)
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
			input, out := columnar.Batch{Length: 17}, columnar.Batch{}
			if err := state.Eval(t.Context(), &input, nil, &out, mode); err != nil {
				t.Fatal(err)
			}
			if calls != 0 {
				t.Fatalf("inactive calls=%d", calls)
			}
			for lane := range input.Rows() {
				got, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], lane, 1024)
				if err != nil {
					t.Fatal(err)
				}
				if got.IsNull || got.IntVal != 7 {
					t.Fatalf("lane %d: %+v", lane, got)
				}
			}
		})
	}
}
