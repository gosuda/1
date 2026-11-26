package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestVariadicSignatureRejectsInvalidShape(t *testing.T) {
	for _, defect := range []string{"operator", "missing parameter", "scalar parameter", "array not final", "too many parameters"} {
		t.Run(defect, func(t *testing.T) {
			f := newFixture(t)
			array, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}})
			if err != nil {
				t.Fatal(err)
			}
			spec := SignatureSpec{Signature: Signature{Function: 3058, Result: f.integer, VariadicArray: true}, Parameters: []LayoutID{array}}
			switch defect {
			case "operator":
				spec.Function, spec.Operator = 0, 1
			case "missing parameter":
				spec.Parameters = nil
			case "scalar parameter":
				spec.Parameters[0] = f.integer
			case "array not final":
				spec.Parameters = append(spec.Parameters, f.integer)
			case "too many parameters":
				spec.Parameters = make([]LayoutID, 101)
				for i := range spec.Parameters {
					spec.Parameters[i] = array
				}
			}
			_, err = f.b.AddSignature(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestVariadicSignatureCannotBeUsedAsCast(t *testing.T) {
	f := newFixture(t)
	array, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}})
	if err != nil {
		t.Fatal(err)
	}
	input := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: array, Nullable: true}, Value: model.NullVal(model.TypeArray)})
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 3058, Result: f.integer, Nullable: true, VariadicArray: true}, Parameters: []LayoutID{array}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Cast, Scope: f.scope, Layout: f.integer, Function: 3058, Signature: signature, Nullable: true, CastContext: ExplicitCast}, Operands: []ScalarID{input}})
	requireInvalid(t, err)
}

func TestSignatureOwnsOneHundredOrderedParameters(t *testing.T) {
	f := newFixture(t)
	one, truth := f.constant(1), f.truth()
	parameters := make([]LayoutID, 100)
	args := make([]ScalarID, len(parameters))
	for i := range parameters {
		parameters[i], args[i] = f.integer, one
	}
	parameters[99], args[99] = f.boolean, truth
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 3058, Result: f.integer}, Parameters: parameters})
	if err != nil {
		t.Fatal(err)
	}
	parameters[99] = f.integer
	call := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 3058, Signature: signature}, Operands: args})
	out := f.slot(f.integer, false, 0, 0)
	root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{out}, Expressions: []ScalarID{call}})
	graph := f.freeze(root)
	bound, _ := graph.Signature(signature)
	last, _ := graph.SignatureParameter(signature, 99)
	operand, _ := graph.Operand(call, 99)
	if bound.ParameterCount() != 100 || last != f.boolean || operand != truth {
		t.Fatalf("count=%d final layout=%d operand=%d", bound.ParameterCount(), last, operand)
	}
}

func TestVariadicSignatureCannotBeUsedByStatefulCalls(t *testing.T) {
	for _, kind := range []RelKind{Aggregate, Window} {
		t.Run(map[RelKind]string{Aggregate: "aggregate", Window: "window"}[kind], func(t *testing.T) {
			f := newFixture(t)
			array, err := f.b.AddLayout(t.Context(), model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}})
			if err != nil {
				t.Fatal(err)
			}
			argument := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: array, Nullable: true}, Value: model.NullVal(model.TypeArray)})
			input := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})
			result := f.slot(f.integer, true, 0, 0)
			signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 3058, Result: f.integer, Nullable: true, VariadicArray: true}, Parameters: []LayoutID{array}})
			if err != nil {
				t.Fatal(err)
			}
			spec := RelSpec{Rel: Rel{Kind: kind, Scope: f.scope}, Inputs: []RelID{input}, Outputs: []model.SlotID{result}}
			if kind == Aggregate {
				spec.GroupingSets = [][]uint32{{}}
				spec.Aggregates = []AggregateSpec{{AggregateCall: AggregateCall{Function: 3058, Signature: signature, Result: result}, Args: []ScalarID{argument}}}
			} else {
				spec.Frame = Frame{Mode: RowsFrame, Start: FrameBound{Kind: UnboundedPreceding}, End: FrameBound{Kind: CurrentRow}}
				spec.Windows = []WindowSpec{{WindowCall: WindowCall{Function: 3058, Signature: signature, Result: result}, Args: []ScalarID{argument}}}
			}
			_, err = f.b.AddRel(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}
