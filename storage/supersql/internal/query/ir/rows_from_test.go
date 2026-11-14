package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestRowsFromRejectsMalformedContracts(t *testing.T) {
	for _, defect := range []string{
		"one input", "missing output", "extra output", "nonnullable data", "reused input",
		"different nested identity", "different nested modifier", "different nested collation",
		"nullable ordinal", "ordinal identity", "ordinal modifier", "ordinal collation",
		"foreign ordinal flag", "foreign expression", "foreign predicate", "foreign payload",
		"missing error effect", "missing third input effect",
	} {
		t.Run(defect, func(t *testing.T) {
			f, spec := rowsFromTestRelation(t)
			data := f.b.graph.slots[spec.Outputs[0]-1]
			ordinal := f.b.graph.slots[spec.Outputs[3]-1]
			switch defect {
			case "one input":
				spec.Inputs = spec.Inputs[:1]
			case "missing output":
				spec.Outputs = spec.Outputs[1:]
			case "extra output":
				spec.Outputs = append([]model.SlotID{f.slot(data.Layout, true, 0, 0)}, spec.Outputs...)
			case "nonnullable data":
				spec.Outputs[0] = f.slot(data.Layout, false, 0, 0)
			case "reused input":
				spec.Outputs[0], _ = f.b.graph.Output(spec.Inputs[0], 0)
			case "different nested identity", "different nested modifier", "different nested collation":
				layout := rowsFromRecordLayout()
				switch defect {
				case "different nested identity":
					layout.Children[0].Type.ID = 25
				case "different nested modifier":
					layout.Children[0].Type.Modifier = 12
				case "different nested collation":
					layout.Children[0].Type.Collation = 950
				}
				spec.Outputs[0] = f.slot(subscriptsTestLayout(t, f.b, layout), true, 0, 0)
			case "nullable ordinal":
				spec.Outputs[3] = f.slot(ordinal.Layout, true, 0, 0)
			case "ordinal identity", "ordinal modifier", "ordinal collation":
				layout := model.TypeLayout{Type: f.b.graph.layouts[ordinal.Layout-1].typ}
				switch defect {
				case "ordinal identity":
					layout.Type.ID = 23
				case "ordinal modifier":
					layout.Type.Modifier = 0
				case "ordinal collation":
					layout.Type.Collation = 100
				}
				spec.Outputs[3] = f.slot(subscriptsTestLayout(t, f.b, layout), false, 0, 0)
			case "foreign ordinal flag":
				spec.Kind, spec.Rows = Values, 0
				spec.Inputs, spec.Outputs = nil, nil
			case "foreign expression":
				spec.Expressions = []ScalarID{f.constant(1)}
			case "foreign predicate":
				spec.Predicate = f.truth()
			case "foreign payload":
				spec.UnnestOrdinality = true
			case "missing error effect":
				spec.Effects.MayError = false
			case "missing third input effect":
				input := f.b.graph.rels[spec.Inputs[2]-1]
				output, _ := f.b.graph.Output(spec.Inputs[2], 0)
				inputSlot := f.b.graph.slots[output-1]
				value := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: inputSlot.Layout, Parameter: 1, Nullable: true}})
				input.Effects = Effects{Volatility: Volatile}
				spec.Inputs[2] = f.rel(RelSpec{Rel: input.Metadata(), Outputs: []model.SlotID{output}, Expressions: []ScalarID{value}})
			}
			_, err := f.b.AddRel(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestRowsFromPreservesIndependentFullLayouts(t *testing.T) {
	f, spec := rowsFromTestRelation(t)
	root := f.rel(spec)
	graph := f.freeze(root)
	for i := uint32(0); i < 3; i++ {
		output, _ := graph.Output(root, i)
		info, _ := graph.Slot(output)
		child, _ := graph.LayoutChild(info.Layout, 0)
		typ, _ := graph.Type(child)
		if !info.Nullable || typ != rowsFromRecordLayout().Children[0].Type {
			t.Fatalf("output %d lost nullable nested SQL layout: %+v, %+v", i, info, typ)
		}
	}
}

func rowsFromRecordLayout() model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{{Type: model.Type{ID: 1043, Modifier: 11, Collation: 100, Storage: model.TypeString}}}}
}

func rowsFromTestRelation(t *testing.T) (fixture, RelSpec) {
	t.Helper()
	f := newFixture(t)
	spec := RelSpec{Rel: Rel{Kind: RowsFrom, Scope: f.scope, RowsFromOrdinality: true, Effects: Effects{MayError: true}}}
	for i := range 3 {
		inputLayout := subscriptsTestLayout(t, f.b, rowsFromRecordLayout())
		outputLayout := subscriptsTestLayout(t, f.b, rowsFromRecordLayout())
		input := f.slot(inputLayout, true, 0, 0)
		value := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: inputLayout, Parameter: uint32(i + 1), Nullable: true}})
		spec.Inputs = append(spec.Inputs, f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}, Outputs: []model.SlotID{input}, Expressions: []ScalarID{value}}))
		spec.Outputs = append(spec.Outputs, f.slot(outputLayout, true, 0, 0))
	}
	ordinal := subscriptsTestLayout(t, f.b, model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}})
	spec.Outputs = append(spec.Outputs, f.slot(ordinal, false, 0, 0))
	return f, spec
}
