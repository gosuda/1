package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestCollateRejectsChangedMetadata(t *testing.T) {
	for _, defect := range []string{"missing", "multiple", "forward", "kind", "identity", "storage", "modifier", "nested collation", "nullable", "effects", "payload"} {
		t.Run(defect, func(t *testing.T) {
			f := newFixture(t)
			layout := model.TypeLayout{Type: model.Type{ID: 1009, Modifier: -1, Collation: 100, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}}}}
			inputLayout, err := f.b.AddLayout(t.Context(), layout)
			if err != nil {
				t.Fatal(err)
			}
			input := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: inputLayout, Nullable: true}, Value: model.NullVal(model.TypeArray)})
			layout.Type.Collation = 951
			switch defect {
			case "identity":
				layout.Type.ID = 1015
			case "storage":
				layout.Type.Storage = model.TypeComposite
			case "modifier":
				layout.Type.Modifier = 8
			case "nested collation":
				layout.Children[0].Type.Collation = 951
			}
			resultLayout, err := f.b.AddLayout(t.Context(), layout)
			if err != nil {
				t.Fatal(err)
			}
			spec := ScalarSpec{Scalar: Scalar{Kind: Collate, Scope: f.scope, Layout: resultLayout, Nullable: true}, Operands: []ScalarID{input}}
			switch defect {
			case "missing":
				spec.Operands = nil
			case "multiple":
				spec.Operands = append(spec.Operands, input)
			case "forward":
				spec.Operands[0] = input + 1
			case "kind":
				spec.Kind = ScalarKind(255)
			case "nullable":
				spec.Nullable = false
			case "effects":
				spec.Effects.MayError = true
			case "payload":
				spec.Signature = 1
			}
			_, err = f.b.AddScalar(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestCollateCloneOwnsOperandsAndNestedLayouts(t *testing.T) {
	f := newFixture(t)
	layout := model.TypeLayout{Type: model.Type{ID: 1009, Modifier: -1, Collation: 100, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}}}}
	inputLayout, err := f.b.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	input := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: inputLayout, Nullable: true}, Value: model.NullVal(model.TypeArray)})
	layout.Type.Collation = 951
	resultLayout, err := f.b.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	args := []ScalarID{input}
	collate := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Collate, Scope: f.scope, Layout: resultLayout, Nullable: true}, Operands: args})
	args[0] = 0
	layout.Children[0].Type.Collation = 950
	output := f.slot(resultLayout, true, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{output}, Expressions: []ScalarID{collate}})
	graph := f.freeze(root)
	copySpec, err := graph.CloneScalarSpec(t.Context(), collate, 1024)
	if err != nil {
		t.Fatal(err)
	}
	copySpec.Operands[0] = 0
	builder, err := Extend(t.Context(), graph, limitsForTest())
	if err != nil {
		t.Fatal(err)
	}
	extended, err := builder.Build(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []*Graph{graph, extended} {
		operand, _ := g.Operand(collate, 0)
		if operand != input {
			t.Fatalf("owned operand=%d, want %d", operand, input)
		}
		node, _ := g.Scalar(collate)
		typ, _ := g.Type(node.Layout)
		child, _ := g.LayoutChild(node.Layout, 0)
		element, _ := g.Type(child)
		if typ.Collation != 951 || element.Collation != 100 || !node.Nullable {
			t.Fatalf("relabel result=%+v element=%+v nullable=%v", typ, element, node.Nullable)
		}
		value, err := g.CloneConstant(t.Context(), operand, 1024)
		if err != nil || !value.IsNull || value.Type != model.TypeArray {
			t.Fatalf("relabel operand=%+v, %v", value, err)
		}
	}
}

func TestCollateCannotOmitOperandEffects(t *testing.T) {
	f := newFixture(t)
	text, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}})
	if err != nil {
		t.Fatal(err)
	}
	effects := Effects{MayError: true}
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 1258, Result: text, Effects: effects, InputCollation: 100}, Parameters: []LayoutID{text, text}})
	if err != nil {
		t.Fatal(err)
	}
	value := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: text}, Value: model.StringVal("x")})
	input := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: text, Signature: signature, Function: 1258, Effects: effects}, Operands: []ScalarID{value, value}})
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Collate, Scope: f.scope, Layout: text}, Operands: []ScalarID{input}})
	requireInvalid(t, err)
}

func TestBinaryRelabelRejectsValueConversions(t *testing.T) {
	for _, defect := range []string{"storage", "modifier", "collation", "nullable", "context"} {
		t.Run(defect, func(t *testing.T) {
			f := newFixture(t)
			inputLayout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}})
			if err != nil {
				t.Fatal(err)
			}
			input := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: inputLayout}, Value: model.StringVal("value")})
			result := model.TypeLayout{Type: model.Type{ID: 1043, Modifier: -1, Collation: 100, Storage: model.TypeString}}
			switch defect {
			case "storage":
				result.Type.ID, result.Type.Storage, result.Type.Collation = 1700, model.TypeNumeric, 0
			case "modifier":
				result.Type.Modifier = 8
			case "collation":
				result.Type.Collation = 951
			}
			layout, err := f.b.AddLayout(t.Context(), result)
			if err != nil {
				t.Fatal(err)
			}
			spec := ScalarSpec{Scalar: Scalar{Kind: Relabel, Scope: f.scope, Layout: layout, CastContext: ExplicitCast}, Operands: []ScalarID{input}}
			if defect == "nullable" {
				spec.Nullable = true
			}
			if defect == "context" {
				spec.CastContext = 0
			}
			_, err = f.b.AddScalar(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}
