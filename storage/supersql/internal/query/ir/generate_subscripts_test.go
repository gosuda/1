package ir

import (
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestGenerateSubscriptsAcceptsNullableArguments(t *testing.T) {
	for _, arguments := range []int{2, 3} {
		for _, outputs := range []int{1, 2} {
			t.Run(fmt.Sprintf("arguments%d/outputs%d", arguments, outputs), func(t *testing.T) {
				f, spec := subscriptsTestRelation(t)
				spec.Expressions = spec.Expressions[:arguments]
				spec.Outputs = spec.Outputs[:outputs]
				root := f.rel(spec)
				graph := f.freeze(root)
				for i, want := range spec.Expressions {
					id, ok := graph.Expression(root, uint32(i))
					expr, _ := graph.Scalar(id)
					if !ok || id != want || !expr.Nullable {
						t.Errorf("argument %d = %+v, want original nullable expression %d", i, expr, want)
					}
				}
				for i := range outputs {
					id, _ := graph.Output(root, uint32(i))
					slot, _ := graph.Slot(id)
					typ, _ := graph.Type(slot.Layout)
					want := model.TypeID(23)
					if i == 1 {
						want = 20
					}
					if slot.Nullable || typ != (model.Type{ID: want, Modifier: -1, Storage: model.TypeInt64}) {
						t.Errorf("output %d = %+v, type %+v; want non-null type %d", i, slot, typ, want)
					}
				}
			})
		}
	}
}

func TestGenerateSubscriptsRejectsMalformedContracts(t *testing.T) {
	for _, defect := range []string{
		"input relation", "no arguments", "missing dimension", "extra argument",
		"no outputs", "extra output", "scalar source", "wrong dimension", "wrong reverse",
		"dimension modifier", "reverse modifier", "reverse collation",
		"nullable subscript", "subscript identity", "subscript modifier", "subscript collation", "subscript storage", "subscript children",
		"nullable ordinality", "ordinality identity", "ordinality modifier", "ordinality collation", "ordinality storage", "ordinality children",
		"missing error effect", "invalid volatility", "foreign payload",
	} {
		t.Run(defect, func(t *testing.T) {
			f, spec := subscriptsTestRelation(t)
			output := f.b.graph.slots[spec.Outputs[0]-1]
			ordinal := f.b.graph.slots[spec.Outputs[1]-1]
			switch defect {
			case "input relation":
				spec.Inputs = []RelID{f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})}
			case "no arguments":
				spec.Expressions = nil
			case "missing dimension":
				spec.Expressions = spec.Expressions[:1]
			case "extra argument":
				spec.Expressions = append(spec.Expressions, spec.Expressions[2])
			case "no outputs":
				spec.Outputs = nil
			case "extra output":
				spec.Outputs = append(spec.Outputs, f.slot(output.Layout, false, 0, 0))
			case "scalar source":
				spec.Expressions[0] = spec.Expressions[1]
			case "wrong dimension":
				spec.Expressions[1] = f.constant(1)
			case "wrong reverse":
				spec.Expressions[2] = spec.Expressions[1]
			case "dimension modifier":
				layout := subscriptsTestLayout(t, f.b, model.TypeLayout{Type: model.Type{ID: 23, Storage: model.TypeInt64}})
				spec.Expressions[1] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: layout, Parameter: 4, Nullable: true}})
			case "reverse modifier":
				spec.Expressions[2] = f.truth()
			case "reverse collation":
				layout := subscriptsTestLayout(t, f.b, model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean, Collation: 100}})
				spec.Expressions[2] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: layout, Parameter: 4, Nullable: true}})
			case "nullable subscript":
				spec.Outputs[0] = f.slot(output.Layout, true, 0, 0)
			case "nullable ordinality":
				spec.Outputs[1] = f.slot(ordinal.Layout, true, 0, 0)
			case "subscript identity", "subscript modifier", "subscript collation", "subscript storage", "subscript children",
				"ordinality identity", "ordinality modifier", "ordinality collation", "ordinality storage", "ordinality children":
				index := 0
				switch defect {
				case "ordinality identity", "ordinality modifier", "ordinality collation", "ordinality storage", "ordinality children":
					index = 1
				}
				slot := f.b.graph.slots[spec.Outputs[index]-1]
				layout := model.TypeLayout{Type: f.b.graph.layouts[slot.Layout-1].typ}
				switch defect {
				case "subscript identity", "ordinality identity":
					layout.Type.ID++
				case "subscript modifier", "ordinality modifier":
					layout.Type.Modifier = 0
				case "subscript collation", "ordinality collation":
					layout.Type.Collation = 100
				case "subscript storage", "ordinality storage":
					layout.Type.Storage = model.TypeFloat64
				case "subscript children", "ordinality children":
					layout.Children = []model.TypeLayout{{Type: layout.Type}}
					layout.Type.Storage = model.TypeComposite
				}
				spec.Outputs[index] = f.slot(subscriptsTestLayout(t, f.b, layout), false, 0, 0)
			case "missing error effect":
				spec.Effects.MayError = false
			case "invalid volatility":
				spec.Effects.Volatility = Volatile + 1
			case "foreign payload":
				spec.Rows = 1
			}
			_, err := f.b.AddRel(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestGenerateSubscriptsRequiresEveryArgumentEffect(t *testing.T) {
	for index := range 3 {
		t.Run(fmt.Sprintf("argument%d", index), func(t *testing.T) {
			f, spec := subscriptsTestRelation(t)
			layout := f.b.graph.scalars[spec.Expressions[index]-1].Layout
			effects := Effects{Volatility: Volatile}
			signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 900000, Result: layout, Nullable: true, Effects: effects}})
			if err != nil {
				t.Fatal(err)
			}
			spec.Expressions[index] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: layout, Function: 900000, Signature: signature, Nullable: true, Effects: effects}})
			_, err = f.b.AddRel(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func subscriptsTestRelation(t *testing.T) (fixture, RelSpec) {
	t.Helper()
	f := newFixture(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	int4 := subscriptsTestLayout(t, f.b, integer)
	int8 := subscriptsTestLayout(t, f.b, model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}})
	array := subscriptsTestLayout(t, f.b, model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}})
	reverse := subscriptsTestLayout(t, f.b, boolean)
	arguments := make([]ScalarID, 3)
	for i, layout := range []LayoutID{array, int4, reverse} {
		arguments[i] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: layout, Parameter: uint32(i + 1), Nullable: true}})
	}
	return f, RelSpec{Rel: Rel{Kind: GenerateSubscripts, Scope: f.scope, Effects: Effects{MayError: true}}, Outputs: []model.SlotID{f.slot(int4, false, 0, 0), f.slot(int8, false, 0, 0)}, Expressions: arguments}
}

func subscriptsTestLayout(t *testing.T, builder *Builder, layout model.TypeLayout) LayoutID {
	t.Helper()
	id, err := builder.AddLayout(t.Context(), layout)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
