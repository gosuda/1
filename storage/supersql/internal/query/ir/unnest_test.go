package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestUnnestRejectsMalformedContracts(t *testing.T) {
	for _, defect := range []string{
		"input relation", "no argument", "extra argument", "scalar source",
		"missing data", "extra data", "nonnullable data", "different scalar layout",
		"missing ordinal", "nullable ordinal", "ordinal identity", "ordinal modifier", "ordinal collation",
		"ordinal storage", "ordinal children", "foreign ordinal flag", "foreign payload", "missing error effect",
	} {
		t.Run(defect, func(t *testing.T) {
			f, spec := unnestTestRelation(t)
			data := f.b.graph.slots[spec.Outputs[0]-1]
			ordinal := f.b.graph.slots[spec.Outputs[1]-1]
			switch defect {
			case "input relation":
				spec.Inputs = []RelID{f.rel(RelSpec{Rel: Rel{Kind: Values, Scope: f.scope, Rows: 1}})}
			case "no argument":
				spec.Expressions = nil
			case "extra argument":
				spec.Expressions = append(spec.Expressions, spec.Expressions[0])
			case "scalar source":
				spec.Expressions[0] = f.constant(1)
			case "missing data":
				spec.Outputs = spec.Outputs[1:]
			case "extra data":
				spec.Outputs = append([]model.SlotID{f.slot(data.Layout, true, 0, 0)}, spec.Outputs...)
			case "nonnullable data":
				spec.Outputs[0] = f.slot(data.Layout, false, 0, 0)
			case "different scalar layout":
				spec.Outputs[0] = f.slot(ordinal.Layout, true, 0, 0)
			case "missing ordinal":
				spec.Outputs = nil
			case "nullable ordinal":
				spec.Outputs[1] = f.slot(ordinal.Layout, true, 0, 0)
			case "ordinal identity", "ordinal modifier", "ordinal collation", "ordinal storage", "ordinal children":
				layout := model.TypeLayout{Type: f.b.graph.layouts[ordinal.Layout-1].typ}
				switch defect {
				case "ordinal identity":
					layout.Type.ID = 23
				case "ordinal modifier":
					layout.Type.Modifier = 0
				case "ordinal collation":
					layout.Type.Collation = 950
				case "ordinal storage":
					layout.Type.Storage = model.TypeFloat64
				case "ordinal children":
					layout.Children = []model.TypeLayout{{Type: layout.Type}}
					layout.Type.Storage = model.TypeComposite
				}
				spec.Outputs[1] = f.slot(subscriptsTestLayout(t, f.b, layout), false, 0, 0)
			case "foreign ordinal flag":
				spec.Kind = Values
				spec.Expressions, spec.Outputs = nil, nil
			case "foreign payload":
				spec.Rows = 1
			case "missing error effect":
				spec.Effects.MayError = false
			}
			_, err := f.b.AddRel(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestUnnestRecordSchemaMismatchRemainsRuntimeChecked(t *testing.T) {
	for _, fields := range []int{0, 1, 2} {
		for _, ordinality := range []bool{false, true} {
			f, spec := unnestTestRelation(t)
			integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
			record := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{integer}}
			array := model.TypeLayout{Type: model.Type{ID: 2287, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{record}}
			layout := subscriptsTestLayout(t, f.b, array)
			spec.Expressions[0] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: layout, Parameter: 2, Nullable: true}})
			ordinal := spec.Outputs[1]
			bigint := f.b.graph.slots[ordinal-1].Layout
			spec.Outputs = nil
			for range fields {
				spec.Outputs = append(spec.Outputs, f.slot(bigint, true, 0, 0))
			}
			spec.UnnestOrdinality = ordinality
			if ordinality {
				spec.Outputs = append(spec.Outputs, ordinal)
			}
			root := f.rel(spec)
			graph := f.freeze(root)
			node, _ := graph.Rel(root)
			if node.OutputCount() != uint32(len(spec.Outputs)) || node.UnnestOrdinality != ordinality {
				t.Fatalf("record fields=%d ordinality=%t: relation = %+v", fields, ordinality, node)
			}
		}
	}
}

func TestUnnestRequiresArrayExpressionEffects(t *testing.T) {
	f, spec := unnestTestRelation(t)
	layout := f.b.graph.scalars[spec.Expressions[0]-1].Layout
	effects := Effects{Volatility: Volatile}
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 900000, Result: layout, Nullable: true, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	spec.Expressions[0] = f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: layout, Function: 900000, Signature: signature, Nullable: true, Effects: effects}})
	_, err = f.b.AddRel(t.Context(), spec)
	requireInvalid(t, err)
}

func unnestTestRelation(t *testing.T) (fixture, RelSpec) {
	t.Helper()
	f := newFixture(t)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	int4 := subscriptsTestLayout(t, f.b, integer)
	int8 := subscriptsTestLayout(t, f.b, model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}})
	array := subscriptsTestLayout(t, f.b, model.TypeLayout{Type: model.Type{ID: 1007, Modifier: -1, Storage: model.TypeArray}, Children: []model.TypeLayout{integer}})
	argument := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Param, Scope: f.scope, Layout: array, Parameter: 1, Nullable: true}})
	return f, RelSpec{Rel: Rel{Kind: Unnest, Scope: f.scope, UnnestOrdinality: true, Effects: Effects{MayError: true}}, Outputs: []model.SlotID{f.slot(int4, true, 0, 0), f.slot(int8, false, 0, 0)}, Expressions: []ScalarID{argument}}
}
