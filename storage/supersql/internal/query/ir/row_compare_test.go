package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func rowComparisonFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	var err error
	f.integer, err = f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64, Modifier: -1}})
	if err != nil {
		t.Fatal(err)
	}
	f.boolean, err = f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 16, Storage: model.TypeBoolean, Modifier: -1}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestRowCompareRejectsMalformedOperandsAndMetadata(t *testing.T) {
	for _, defect := range []string{"empty", "single field", "equality strategy", "unknown strategy", "nonboolean result", "noncall comparator", "unary comparator", "bigint comparator", "operator comparator", "missing nullability", "missing effects", "foreign payload"} {
		t.Run(defect, func(t *testing.T) {
			f := rowComparisonFixture(t)
			left := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.integer, Nullable: true}, Value: model.NullVal(model.TypeInt64)})
			right := f.constant(2)
			effects := Effects{MayError: true}
			selected := SignatureSpec{Signature: Signature{Function: 351, Result: f.integer, Strict: true, Nullable: true, Effects: effects}, Parameters: []LayoutID{f.integer, f.integer}}
			call := ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 351, Strict: true, Nullable: true, Effects: effects}, Operands: []ScalarID{left, right}}
			switch defect {
			case "unary comparator":
				selected.Parameters, call.Operands = selected.Parameters[:1], call.Operands[:1]
			case "bigint comparator":
				layout, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64, Modifier: -1}})
				if err != nil {
					t.Fatal(err)
				}
				selected.Result, call.Layout = layout, layout
			case "operator comparator":
				selected.Function, selected.Operator = 0, 97
				call.Function, call.Operator = 0, 97
			}
			var err error
			call.Signature, err = f.b.AddSignature(t.Context(), selected)
			if err != nil {
				t.Fatal(err)
			}
			comparator := f.scalar(call)
			root := ScalarSpec{Scalar: Scalar{Kind: RowCompare, Scope: f.scope, Layout: f.boolean, RowComparison: RowCompareLess, Nullable: true, Effects: effects}, Operands: []ScalarID{comparator, comparator}}
			switch defect {
			case "empty":
				root.Operands = nil
			case "single field":
				root.Operands = root.Operands[:1]
			case "equality strategy":
				root.RowComparison = 3
			case "unknown strategy":
				root.RowComparison = 255
			case "nonboolean result":
				root.Layout = f.integer
			case "noncall comparator":
				root.Operands[1] = right
			case "missing nullability":
				root.Nullable = false
			case "missing effects":
				root.Effects = Effects{}
			case "foreign payload":
				root.Function = 351
			}
			_, err = f.b.AddScalar(t.Context(), root)
			requireInvalid(t, err)
		})
	}
}

func TestRowComparatorRejectsIncompleteSelectedSignature(t *testing.T) {
	for _, defect := range []string{"missing signature", "function identity", "strictness", "parameter type", "signature effects", "signature nullability"} {
		t.Run(defect, func(t *testing.T) {
			f := rowComparisonFixture(t)
			selected := SignatureSpec{Signature: Signature{Function: 351, Result: f.integer, Strict: true, Effects: Effects{MayError: true}}, Parameters: []LayoutID{f.integer, f.integer}}
			call := ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 351, Strict: true, Effects: Effects{MayError: true}}, Operands: []ScalarID{f.constant(1), f.constant(2)}}
			switch defect {
			case "function identity":
				call.Function = 350
			case "strictness":
				call.Strict = false
			case "parameter type":
				selected.Parameters[1] = f.boolean
			case "signature effects":
				selected.Effects.Volatility = Volatile
			case "signature nullability":
				selected.Nullable = true
			}
			var err error
			if defect != "missing signature" {
				call.Signature, err = f.b.AddSignature(t.Context(), selected)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err = f.b.AddScalar(t.Context(), call)
			requireInvalid(t, err)
		})
	}
}

func TestRowComparisonStrategyRejectedOnOtherScalarKinds(t *testing.T) {
	f := rowComparisonFixture(t)
	_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.boolean, RowComparison: RowCompareLess}, Value: model.BoolVal(true)})
	requireInvalid(t, err)
}
