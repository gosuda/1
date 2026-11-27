package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestArrayStackRejectsInvalidContracts(t *testing.T) {
	for _, defect := range []string{"empty", "scalar result", "scalar operand", "array identity", "array modifier", "element layout", "nullable result", "missing shape effect", "missing child effect", "extra effect", "foreign payload"} {
		t.Run(defect, func(t *testing.T) {
			f := newFixture(t)
			layout := model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}}
			array, err := f.b.AddLayout(t.Context(), layout)
			if err != nil {
				t.Fatal(err)
			}
			operandLayout := array
			switch defect {
			case "array identity":
				layout.Type.ID = 1007
			case "array modifier":
				layout.Type.Modifier = -1
			case "element layout":
				layout.Children[0].Type.ID = 23
			}
			if defect == "array identity" || defect == "array modifier" || defect == "element layout" {
				operandLayout, err = f.b.AddLayout(t.Context(), layout)
				if err != nil {
					t.Fatal(err)
				}
			}
			operand := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: operandLayout, Nullable: true}, Value: model.NullVal(model.TypeArray)})
			spec := ScalarSpec{Scalar: Scalar{Kind: ArrayStack, Scope: f.scope, Layout: array, Effects: Effects{MayError: true}}, Operands: []ScalarID{operand}}
			switch defect {
			case "empty":
				spec.Operands = nil
			case "scalar result":
				spec.Layout = f.integer
			case "scalar operand":
				spec.Operands[0] = f.constant(1)
			case "nullable result":
				spec.Nullable = true
			case "missing shape effect":
				spec.Effects = Effects{}
			case "missing child effect":
				effects := Effects{Volatility: Volatile}
				signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 900000, Result: array, Nullable: true, Effects: effects}})
				if err != nil {
					t.Fatal(err)
				}
				spec.Operands[0] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: array, Function: 900000, Signature: signature, Nullable: true, Effects: effects}})
			case "extra effect":
				spec.Effects.Volatility = Volatile
			case "foreign payload":
				spec.Function = 900000
			}
			_, err = f.b.AddScalar(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}
