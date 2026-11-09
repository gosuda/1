package ir

import (
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type arrayCoerceFixture struct {
	fixture
	int4, int4Array, int8Array LayoutID
}

func newArrayCoerceFixture(t *testing.T) arrayCoerceFixture {
	t.Helper()
	f := arrayCoerceFixture{fixture: newFixture(t)}
	int4 := model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}}
	f.int4 = f.layout(int4)
	f.int4Array = f.layout(model.TypeLayout{Type: model.Type{ID: 1007, Storage: model.TypeArray}, Children: []model.TypeLayout{int4}})
	f.int8Array = f.layout(model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: model.Type{ID: 20, Storage: model.TypeInt64}}}})
	return f
}

func (f arrayCoerceFixture) layout(value model.TypeLayout) LayoutID {
	f.t.Helper()
	id, err := f.b.AddLayout(f.t.Context(), value)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f arrayCoerceFixture) invocation(spec ScalarSpec, parameters ...LayoutID) ScalarID {
	f.t.Helper()
	var err error
	spec.Signature, err = f.b.AddSignature(f.t.Context(), SignatureSpec{Signature: Signature{Function: spec.Function, Result: spec.Layout, Nullable: spec.Nullable, Strict: spec.Strict, Effects: spec.Effects}, Parameters: parameters})
	if err != nil {
		f.t.Fatal(err)
	}
	return f.scalar(spec)
}

func TestArrayCoerceAcceptsElementConversionPipelines(t *testing.T) {
	for _, pipeline := range []string{"identity", "implicit widening", "assignment typmod", "explicit IO", "eight operand call"} {
		t.Run(pipeline, func(t *testing.T) {
			f := newArrayCoerceFixture(t)
			effects := Effects{MayError: true}
			value := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -3}, {Length: 1, LowerBound: 4}}, ArrayVal: []model.Value{model.Int64Val(7), model.NullVal(model.TypeInt64)}}
			castContext := ImplicitCast
			result := f.int8Array
			if pipeline == "assignment typmod" {
				castContext = AssignmentCast
				value = model.NullVal(model.TypeArray)
			}
			if pipeline == "explicit IO" {
				castContext = ExplicitCast
				value = model.Value{Type: model.TypeArray}
			}
			source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int4Array, Nullable: value.IsNull}, Value: value})
			element := f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArrayElement, Scope: f.scope, Layout: f.int4, Nullable: true}})
			var template ScalarID
			switch pipeline {
			case "identity":
				template, result = element, f.int4Array
			case "implicit widening":
				template = f.invocation(ScalarSpec{Scalar: Scalar{Kind: Cast, Scope: f.scope, Layout: f.integer, Function: 481, Strict: true, Nullable: true, Effects: effects, CastContext: castContext}, Operands: []ScalarID{element}}, f.int4)
			case "assignment typmod":
				numeric := model.TypeLayout{Type: model.Type{ID: 1700, Modifier: -1, Storage: model.TypeNumeric}}
				unconstrained := f.layout(numeric)
				numeric.Type.Modifier = 655366
				constrained := f.layout(numeric)
				result = f.layout(model.TypeLayout{Type: model.Type{ID: 1231, Modifier: 655366, Storage: model.TypeArray}, Children: []model.TypeLayout{numeric}})
				converted := f.invocation(ScalarSpec{Scalar: Scalar{Kind: Cast, Scope: f.scope, Layout: unconstrained, Function: 1740, Strict: true, Nullable: true, Effects: effects, CastContext: castContext}, Operands: []ScalarID{element}}, f.int4)
				modifier := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int4}, Value: model.Int64Val(655366)})
				template = f.invocation(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: constrained, Function: 1703, Strict: true, Nullable: true, Effects: effects}, Operands: []ScalarID{converted, modifier}}, unconstrained, f.int4)
			case "explicit IO":
				text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}}
				textLayout := f.layout(text)
				output := f.invocation(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: textLayout, Function: 43, Strict: true, Nullable: true, Effects: effects}, Operands: []ScalarID{element}}, f.int4)
				text.Type.ID = 1043
				varchar := f.layout(text)
				typeID := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int4}, Value: model.Int64Val(1043)})
				modifier := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int4}, Value: model.Int64Val(-1)})
				input := f.invocation(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: varchar, Function: 1046, Strict: true, Nullable: true, Effects: effects}, Operands: []ScalarID{output, typeID, modifier}}, textLayout, f.int4, f.int4)
				relabel := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Relabel, Scope: f.scope, Layout: textLayout, Nullable: true, Effects: effects, CastContext: castContext}, Operands: []ScalarID{input}})
				text.Type.ID = 25
				text.Type.Collation = 951
				collated := f.layout(text)
				template = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Collate, Scope: f.scope, Layout: collated, Nullable: true, Effects: effects}, Operands: []ScalarID{relabel}})
				result = f.layout(model.TypeLayout{Type: model.Type{ID: 1009, Modifier: -1, Collation: 951, Storage: model.TypeArray}, Children: []model.TypeLayout{text}})
			case "eight operand call":
				operands := []ScalarID{element}
				parameters := []LayoutID{f.int4}
				for range 7 {
					operands = append(operands, f.constant(1))
					parameters = append(parameters, f.integer)
				}
				template = f.invocation(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 900001, Strict: true, Nullable: true, Effects: effects}, Operands: operands}, parameters...)
			}
			coercion := f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArrayCoerce, Scope: f.scope, Layout: result, Nullable: value.IsNull, Effects: effects, CastContext: castContext}, Operands: []ScalarID{source, template}})
			output := f.slot(result, value.IsNull, 0, 0)
			root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1, Effects: effects}, Outputs: []model.SlotID{output}, Expressions: []ScalarID{coercion}})
			f.freeze(root)
		})
	}
}

func TestArrayCoerceRejectsInvalidMetadata(t *testing.T) {
	for _, defect := range []string{"missing operands", "missing template", "extra operand", "missing context", "unknown context", "scalar source", "scalar result", "missing nullability", "extra nullability", "source element identity", "result element identity", "result element modifier", "result element collation", "foreign payload"} {
		t.Run(defect, func(t *testing.T) {
			f := newArrayCoerceFixture(t)
			source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int8Array, Nullable: true}, Value: model.NullVal(model.TypeArray)})
			element := f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArrayElement, Scope: f.scope, Layout: f.integer, Nullable: true}})
			spec := ScalarSpec{Scalar: Scalar{Kind: ArrayCoerce, Scope: f.scope, Layout: f.int8Array, Nullable: true, CastContext: ImplicitCast}, Operands: []ScalarID{source, element}}
			switch defect {
			case "missing operands":
				spec.Operands = nil
			case "missing template":
				spec.Operands = spec.Operands[:1]
			case "extra operand":
				spec.Operands = append(spec.Operands, element)
			case "missing context":
				spec.CastContext = 0
			case "unknown context":
				spec.CastContext = CastContext(255)
			case "scalar source":
				spec.Operands[0] = f.constant(1)
			case "scalar result":
				spec.Layout = f.integer
			case "missing nullability":
				spec.Nullable = false
			case "extra nullability":
				spec.Operands[0] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Array, Scope: f.scope, Layout: f.int8Array}})
			case "source element identity":
				spec.Operands[0] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int4Array, Nullable: true}, Value: model.NullVal(model.TypeArray)})
			case "result element identity":
				spec.Layout = f.int4Array
			case "result element modifier", "result element collation":
				child := model.Type{ID: 20, Storage: model.TypeInt64}
				if defect == "result element modifier" {
					child.Modifier = 8
				} else {
					child.Collation = 951
				}
				spec.Layout = f.layout(model.TypeLayout{Type: model.Type{ID: 1016, Storage: model.TypeArray}, Children: []model.TypeLayout{{Type: child}}})
			case "foreign payload":
				spec.Function = 481
			}
			_, err := f.b.AddScalar(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestArrayCoerceRejectsRowDependentTemplates(t *testing.T) {
	for _, defect := range []string{"slot chain", "parameter chain", "branching chain", "once chain", "subquery chain", "slot auxiliary", "parameter auxiliary", "call auxiliary", "element auxiliary"} {
		t.Run(defect, func(t *testing.T) {
			f := newArrayCoerceFixture(t)
			source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int8Array, Nullable: true}, Value: model.NullVal(model.TypeArray)})
			element := f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArrayElement, Scope: f.scope, Layout: f.integer, Nullable: true}})
			input := element
			var auxiliary ScalarID
			switch defect {
			case "slot chain", "slot auxiliary":
				slot := f.slot(f.integer, true, 0, 0)
				input = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Nullable: true, Slot: slot}})
			case "parameter chain", "parameter auxiliary":
				input = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: f.integer, Nullable: true, Parameter: 1}})
			case "branching chain":
				input = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Coalesce, Scope: f.scope, Layout: f.integer, Nullable: true}, Operands: []ScalarID{element, f.constant(1)}})
			case "once chain":
				input = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: f.scope, Layout: f.integer, Nullable: true}, Operands: []ScalarID{element}})
			case "subquery chain":
				scope, err := f.b.AddScope(t.Context(), f.scope, nil)
				if err != nil {
					t.Fatal(err)
				}
				slot, err := f.b.AddSlot(t.Context(), SlotInfo{Scope: scope, Layout: f.integer})
				if err != nil {
					t.Fatal(err)
				}
				value := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: scope, Layout: f.integer}, Value: model.Int64Val(1)})
				root := f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: scope, Rows: 1}, Outputs: []model.SlotID{slot}, Expressions: []ScalarID{value}})
				input = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Subquery, Scope: f.scope, Layout: f.integer, Nullable: true, Effects: Effects{MayError: true}, SubqueryKind: ScalarSubquery, Relation: root}})
			case "call auxiliary":
				input = f.invocation(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 900002, Nullable: true}})
			case "element auxiliary":
				input = f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArrayElement, Scope: f.scope, Layout: f.integer, Nullable: true}})
			}
			switch defect {
			case "slot auxiliary", "parameter auxiliary", "call auxiliary", "element auxiliary":
				auxiliary, input = input, element
			}
			operands, parameters := []ScalarID{input}, []LayoutID{f.integer}
			if auxiliary != 0 {
				operands = append(operands, auxiliary)
				parameters = append(parameters, f.integer)
			}
			effects := Effects{MayError: true}
			template := f.invocation(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 900001, Strict: true, Nullable: true, Effects: effects}, Operands: operands}, parameters...)
			_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: ArrayCoerce, Scope: f.scope, Layout: f.int8Array, Nullable: true, Effects: effects, CastContext: ExplicitCast}, Operands: []ScalarID{source, template}})
			requireInvalid(t, err)
		})
	}
}

func TestArrayCoerceRejectsConstantTemplate(t *testing.T) {
	f := newArrayCoerceFixture(t)
	source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int8Array, Nullable: true}, Value: model.NullVal(model.TypeArray)})
	_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: ArrayCoerce, Scope: f.scope, Layout: f.int8Array, Nullable: true, CastContext: ExplicitCast}, Operands: []ScalarID{source, f.constant(1)}})
	requireInvalid(t, err)
}

func TestArrayCoerceRejectsInvalidStepArity(t *testing.T) {
	for _, count := range []int{0, 9} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			f := newArrayCoerceFixture(t)
			source := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Const, Scope: f.scope, Layout: f.int8Array, Nullable: true}, Value: model.NullVal(model.TypeArray)})
			operands, parameters := make([]ScalarID, count), make([]LayoutID, count)
			for i := range count {
				operands[i], parameters[i] = f.constant(1), f.integer
			}
			if count != 0 {
				operands[0] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArrayElement, Scope: f.scope, Layout: f.integer, Nullable: true}})
			}
			effects := Effects{MayError: true}
			template := f.invocation(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Function: 900001, Strict: true, Nullable: true, Effects: effects}, Operands: operands}, parameters...)
			_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: ArrayCoerce, Scope: f.scope, Layout: f.int8Array, Nullable: true, Effects: effects, CastContext: ExplicitCast}, Operands: []ScalarID{source, template}})
			requireInvalid(t, err)
		})
	}
}

func TestArrayCoerceCannotOmitOperandEffects(t *testing.T) {
	for _, omitted := range []string{"source", "template"} {
		t.Run(omitted, func(t *testing.T) {
			f := newArrayCoerceFixture(t)
			sourceEffects := Effects{Volatility: Volatile}
			templateEffects := Effects{MayError: true}
			source := f.invocation(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.int4Array, Nullable: true, Function: 900002, Effects: sourceEffects}})
			element := f.scalar(ScalarSpec{Scalar: Scalar{Kind: ArrayElement, Scope: f.scope, Layout: f.int4, Nullable: true}})
			template := f.invocation(ScalarSpec{Scalar: Scalar{Kind: Cast, Scope: f.scope, Layout: f.integer, Nullable: true, Function: 481, Strict: true, Effects: templateEffects, CastContext: ImplicitCast}, Operands: []ScalarID{element}}, f.int4)
			effects := sourceEffects
			if omitted == "source" {
				effects = templateEffects
			}
			_, err := f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: ArrayCoerce, Scope: f.scope, Layout: f.int8Array, Nullable: true, Effects: effects, CastContext: ImplicitCast}, Operands: []ScalarID{source, template}})
			requireInvalid(t, err)
		})
	}
}

func TestArrayElementRejectsInvalidMetadata(t *testing.T) {
	for _, defect := range []string{"operand", "nonnull", "effects", "parameter", "slot", "cast context"} {
		t.Run(defect, func(t *testing.T) {
			f := newFixture(t)
			spec := ScalarSpec{Scalar: Scalar{Kind: ArrayElement, Scope: f.scope, Layout: f.integer, Nullable: true}}
			switch defect {
			case "operand":
				spec.Operands = []ScalarID{f.constant(1)}
			case "nonnull":
				spec.Nullable = false
			case "effects":
				spec.Effects.MayError = true
			case "parameter":
				spec.Parameter = 1
			case "slot":
				spec.Slot = f.slot(f.integer, true, 0, 0)
			case "cast context":
				spec.CastContext = ExplicitCast
			}
			_, err := f.b.AddScalar(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}
