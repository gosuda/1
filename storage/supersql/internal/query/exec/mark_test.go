package exec_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func compiledMark(t *testing.T, kind ir.SubqueryKind, operator string, negate bool, values []model.Value) *fragment.FragmentGraph {
	t.Helper()
	base := sqlGraph(t, `SELECT x FROM (VALUES(1),(2),(NULL)) t(x)`, nil)
	graph, _ := base.Plan().Graph(1)
	left, _ := graph.Rel(graph.Root())
	leftSlot, _ := graph.Output(graph.Root(), 0)
	integer, _ := graph.Slot(leftSlot)
	b, err := ir.Extend(t.Context(), graph, irLimits())
	if err != nil {
		t.Fatal(err)
	}
	boolean, err := b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}})
	if err != nil {
		t.Fatal(err)
	}
	rightSlot, err := b.AddSlot(t.Context(), integer)
	if err != nil {
		t.Fatal(err)
	}
	cells := make([]ir.ScalarID, len(values))
	for i, value := range values {
		cells[i], err = b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Const, Scope: left.Scope, Layout: integer.Layout, Nullable: value.IsNull}, Value: value})
		if err != nil {
			t.Fatal(err)
		}
	}
	right, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Values, Scope: left.Scope, Rows: uint64(len(cells))}, Outputs: []model.SlotID{rightSlot}, Expressions: cells})
	if err != nil {
		t.Fatal(err)
	}
	var op builtin.Operator
	for candidate := range builtin.OperatorsNamed(operator) {
		if candidate.Left == 23 && candidate.Right == 23 && candidate.Result == 16 {
			op = candidate
			break
		}
	}
	if op.ID == 0 {
		t.Fatalf("missing int4 operator %q", operator)
	}
	effects := ir.Effects{MayError: true}
	signature, err := b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Operator: op.ID, Result: boolean, Strict: true, Nullable: true, Effects: effects}, Parameters: []ir.LayoutID{integer.Layout, integer.Layout}})
	if err != nil {
		t.Fatal(err)
	}
	arguments := make([]ir.ScalarID, 2)
	for i, slot := range []model.SlotID{leftSlot, rightSlot} {
		arguments[i], err = b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: left.Scope, Layout: integer.Layout, Nullable: true, Slot: slot}})
		if err != nil {
			t.Fatal(err)
		}
	}
	predicate, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: left.Scope, Layout: boolean, Nullable: true, Operator: op.ID, Signature: signature, Strict: true, Effects: effects}, Operands: arguments})
	if err != nil {
		t.Fatal(err)
	}
	mark, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: left.Scope, Layout: boolean, Nullable: true})
	if err != nil {
		t.Fatal(err)
	}
	root, err := b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Join, Scope: left.Scope, JoinKind: ir.MarkJoin, MarkKind: kind, MarkSlot: mark, Predicate: predicate, Effects: effects}, Inputs: []ir.RelID{graph.Root(), right}, Outputs: []model.SlotID{leftSlot, mark}})
	if err != nil {
		t.Fatal(err)
	}
	if negate {
		value, err := b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Slot, Scope: left.Scope, Layout: boolean, Nullable: true, Slot: mark}})
		if err != nil {
			t.Fatal(err)
		}
		value, err = b.AddScalar(t.Context(), ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, BoolOp: ir.Not, Scope: left.Scope, Layout: boolean, Nullable: true}, Operands: []ir.ScalarID{value}})
		if err != nil {
			t.Fatal(err)
		}
		negated, err := b.AddSlot(t.Context(), ir.SlotInfo{Scope: left.Scope, Layout: boolean, Nullable: true})
		if err != nil {
			t.Fatal(err)
		}
		root, err = b.AddRel(t.Context(), ir.RelSpec{Rel: ir.Rel{Kind: ir.Project, Scope: left.Scope, Effects: effects}, Inputs: []ir.RelID{root}, Outputs: []model.SlotID{leftSlot, negated}, Bindings: []ir.Binding{{Slot: leftSlot, Expr: arguments[0]}, {Slot: negated, Expr: value}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	graph, err = b.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := opt.Optimize(t.Context(), graph, opt.Options{IR: irLimits(), Plan: physical.Limits{MaxNodes: 1024, MaxOperands: 32768, MaxProperties: 32768, MaxDepth: 128, MaxBytes: 16 << 20}, MaxBytes: 256 << 20, MaxWork: 100000000, MaxOutputColumns: 256, MaxAccessPaths: 8, WorkingMemory: 1 << 20, SearchTime: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := fragment.Build(t.Context(), planned.Plan, fragment.Limits{MaxNodes: 1024, MaxEdges: 4096, MaxDepth: 128, MaxWork: 100000, MaxBytes: 4 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return fragments
}

func TestCompiledMarkComparisonAndNegationPreserveUnknown(t *testing.T) {
	unknown := model.NullVal(model.TypeBoolean)
	for _, tc := range []struct {
		name     string
		kind     ir.SubqueryKind
		operator string
		negate   bool
		right    []model.Value
		marks    []model.Value
	}{
		{"less than ANY", ir.AnySubquery, "<", false, []model.Value{model.Int64Val(2), model.NullVal(model.TypeInt64)}, []model.Value{model.BoolVal(true), unknown, unknown}},
		{"less than ALL", ir.AllSubquery, "<", false, []model.Value{model.Int64Val(2), model.NullVal(model.TypeInt64)}, []model.Value{unknown, model.BoolVal(false), unknown}},
		{"NOT IN", ir.AnySubquery, "=", true, []model.Value{model.Int64Val(2), model.NullVal(model.TypeInt64)}, []model.Value{unknown, model.BoolVal(false), unknown}},
		{"ALL empty", ir.AllSubquery, "<", false, nil, []model.Value{model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := compiledMark(t, tc.kind, tc.operator, tc.negate, tc.right)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			want := [][]model.Value{{model.Int64Val(1), tc.marks[0]}, {model.Int64Val(2), tc.marks[1]}, {model.NullVal(model.TypeInt64), tc.marks[2]}}
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows=%v want=%v", got, want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
