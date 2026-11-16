package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestMinMaxRejectsInvalidComparisonContracts(t *testing.T) {
	for _, defect := range []string{"empty", "unary comparison", "nonboolean comparison", "operand layout", "comparison layout", "missing effects", "missing nullability", "identity"} {
		t.Run(defect, func(t *testing.T) {
			f := newFixture(t)
			input := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.integer, Nullable: true}, Value: model.NullVal(model.TypeInt64)})
			effects := Effects{MayError: true}
			sig := SignatureSpec{Signature: Signature{Function: 469, Result: f.boolean, Strict: true, Nullable: true, Effects: effects}, Parameters: []LayoutID{f.integer, f.integer}}
			spec := ScalarSpec{Scalar: Scalar{Kind: Least, Scope: f.scope, Layout: f.integer, Function: 469, Strict: true, Nullable: true, Effects: effects}, Operands: []ScalarID{input}}
			switch defect {
			case "empty":
				spec.Operands = nil
			case "unary comparison":
				sig.Parameters = sig.Parameters[:1]
			case "nonboolean comparison":
				sig.Result = f.integer
			case "operand layout":
				spec.Operands[0] = f.truth()
			case "comparison layout":
				sig.Parameters[1] = f.boolean
			case "missing effects":
				spec.Effects = Effects{}
			case "missing nullability":
				spec.Nullable = false
			case "identity":
				spec.Function = 470
			}
			var err error
			spec.Signature, err = f.b.AddSignature(t.Context(), sig)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.b.AddScalar(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}
