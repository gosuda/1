package normalize_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/normalize"
)

func TestConstantCollatePreservesResultLayout(t *testing.T) {
	f := newFixture(t)
	typ := model.Type{ID: 1043, Modifier: 11, Collation: 100, Storage: model.TypeString}
	inputLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	typ.Collation = 951
	resultLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	input := f.constant(inputLayout, model.StringVal("abc"))
	collate := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Collate, Scope: f.scope, Layout: resultLayout}, Operands: []ir.ScalarID{input}})
	graph := normalized(t, f.project(collate, resultLayout, ir.Effects{}, false))
	root := expression(t, graph)
	node, _ := graph.Scalar(root)
	gotType, _ := graph.Type(node.Layout)
	if node.Kind != ir.Const || gotType != typ {
		t.Fatalf("folded collation kind=%v type=%+v, want %+v", node.Kind, gotType, typ)
	}
	values, err := evaluate(t, graph, []ir.ScalarID{root}, nil, 1)
	if err != nil || values[0][0].StrVal != "abc" {
		t.Fatalf("relabel result=%+v, %v", values, err)
	}
}

func TestFoldUsesBoundInputCollation(t *testing.T) {
	for _, tc := range []struct {
		name               string
		bound, left, right model.CollationID
		masked             bool
		wantError          bool
	}{
		{name: "bound overrides argument conflict", bound: 951, left: 950, right: 951},
		{name: "default yields to nondefault", left: 100, right: 951},
		{name: "implicit conflict stays executable", left: 950, right: 951, wantError: true},
		{name: "indeterminate ignores environment", wantError: true},
		{name: "masked indeterminate never executes", masked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var identity model.FunctionID
			for signature := range builtin.SignaturesNamed("text_lt") {
				identity = signature.ID
			}
			if identity == 0 {
				t.Fatal("missing text comparison signature")
			}
			layouts := make([]ir.LayoutID, 2)
			arguments := make([]ir.ScalarID, 2)
			for i, collation := range []model.CollationID{tc.left, tc.right} {
				layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: collation, Storage: model.TypeString}})
				if err != nil {
					t.Fatal(err)
				}
				layouts[i] = layout
				arguments[i] = f.constant(layout, model.StringVal([]string{"A", "a"}[i]))
			}
			effects := ir.Effects{MayError: true}
			signature, err := f.b.AddSignature(t.Context(), ir.SignatureSpec{Signature: ir.Signature{Function: identity, Result: f.boolean, Strict: true, Effects: effects, InputCollation: tc.bound}, Parameters: layouts})
			if err != nil {
				t.Fatal(err)
			}
			call := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Call, Scope: f.scope, Layout: f.boolean, Function: identity, Signature: signature, Strict: true, Effects: effects}, Operands: arguments})
			root := call
			if tc.masked {
				falsity := f.constant(f.boolean, model.BoolVal(false))
				root = f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Bool, Scope: f.scope, Layout: f.boolean, BoolOp: ir.And, Effects: effects}, Operands: []ir.ScalarID{falsity, call}})
			}
			original := f.project(root, f.boolean, effects, false)
			extension, err := ir.Extend(t.Context(), original, options().Limits)
			if err != nil {
				t.Fatal(err)
			}
			extended, err := extension.Build(t.Context(), original.Root())
			if err != nil {
				t.Fatal(err)
			}
			graph, err := normalize.Normalize(t.Context(), extended, options())
			if tc.wantError {
				var sql *model.Error
				if !errors.As(err, &sql) || sql.Code != "42P22" {
					t.Fatalf("indeterminate constant comparison error=%v, want 42P22", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			root = expression(t, graph)
			node, _ := graph.Scalar(root)
			values, err := evaluate(t, graph, []ir.ScalarID{root}, nil, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := int64(1)
			if tc.masked {
				want = 0
			}
			if node.Kind != ir.Const || values[0][0].IsNull || values[0][0].IntVal != want {
				t.Fatalf("comparison kind=%v value=%+v, want %d", node.Kind, values[0][0], want)
			}
		})
	}
}

func TestCollatePreservesOnceAfterNullableSimplification(t *testing.T) {
	f := newFixture(t)
	typ := model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}
	inputLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	typ.Collation = 950
	resultLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: typ})
	if err != nil {
		t.Fatal(err)
	}
	value := f.constant(inputLayout, model.StringVal("kept"))
	null := f.constant(inputLayout, model.NullVal(model.TypeString))
	truth := f.constant(f.boolean, model.BoolVal(true))
	choice := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Case, Scope: f.scope, Layout: inputLayout, Nullable: true}, Operands: []ir.ScalarID{truth, value, null}})
	once := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Once, Scope: f.scope, Layout: inputLayout, Nullable: true}, Operands: []ir.ScalarID{choice}})
	collate := f.scalar(ir.ScalarSpec{Scalar: ir.Scalar{Kind: ir.Collate, Scope: f.scope, Layout: resultLayout, Nullable: true}, Operands: []ir.ScalarID{once}})
	graph := normalized(t, f.project(collate, resultLayout, ir.Effects{}, true))
	root := expression(t, graph)
	node, _ := graph.Scalar(root)
	operand, _ := graph.Operand(root, 0)
	child, _ := graph.Scalar(operand)
	if node.Kind != ir.Collate || child.Kind != ir.Once || node.Nullable || child.Nullable {
		t.Fatalf("simplified relabel=%+v operand=%+v", node, child)
	}
	values, err := evaluate(t, graph, []ir.ScalarID{root}, nil, 1)
	if err != nil || values[0][0].StrVal != "kept" {
		t.Fatalf("Once relabel result=%+v, %v", values, err)
	}
}
