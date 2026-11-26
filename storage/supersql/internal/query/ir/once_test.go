package ir

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestOnceRejectsInvalidMetadataAndReferences(t *testing.T) {
	for _, defect := range []string{"missing", "multiple", "self", "forward", "layout", "nullable", "effects", "payload", "scope"} {
		t.Run(defect, func(t *testing.T) {
			f := newFixture(t)
			input := f.constant(1)
			spec := ScalarSpec{Scalar: Scalar{Kind: Once, Scope: f.scope, Layout: f.integer}, Operands: []ScalarID{input}}
			switch defect {
			case "missing":
				spec.Operands = nil
			case "multiple":
				spec.Operands = []ScalarID{input, input}
			case "self":
				spec.Operands[0] = input + 1
			case "forward":
				spec.Operands[0] = input + 2
			case "layout":
				spec.Layout = f.boolean
			case "nullable":
				spec.Nullable = true
			case "effects":
				spec.Effects = Effects{Volatility: Volatile}
			case "payload":
				spec.Parameter = 1
			case "scope":
				child, err := f.b.AddScope(t.Context(), f.scope, nil)
				if err != nil {
					t.Fatal(err)
				}
				spec.Scope = child
			}
			_, err := f.b.AddScalar(t.Context(), spec)
			requireInvalid(t, err)
		})
	}
}

func TestOnceCannotHideUnavailableSlot(t *testing.T) {
	f := newFixture(t)
	left, unavailable := f.slot(f.integer, false, 10, 1), f.slot(f.integer, false, 20, 1)
	scan := f.scan(10, left)
	input := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Slot, Scope: f.scope, Layout: f.integer, Slot: unavailable}})
	once := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: f.scope, Layout: f.integer}, Operands: []ScalarID{input}})
	out := f.slot(f.integer, false, 0, 0)
	_, err := f.b.AddRel(t.Context(), RelSpec{Rel: Rel{Kind: Project, Scope: f.scope}, Inputs: []RelID{scan}, Outputs: []model.SlotID{out}, Bindings: []Binding{{Slot: out, Expr: once}}})
	requireInvalid(t, err)
}

func TestOnceRequiresOperandEffects(t *testing.T) {
	f := newFixture(t)
	effects := Effects{Volatility: Volatile, MayError: true}
	signature, err := f.b.AddSignature(t.Context(), SignatureSpec{Signature: Signature{Function: 900000, Result: f.integer, Effects: effects}})
	if err != nil {
		t.Fatal(err)
	}
	input := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Call, Scope: f.scope, Layout: f.integer, Signature: signature, Function: 900000, Effects: effects}})
	_, err = f.b.AddScalar(t.Context(), ScalarSpec{Scalar: Scalar{Kind: Once, Scope: f.scope, Layout: f.integer}, Operands: []ScalarID{input}})
	requireInvalid(t, err)
}

func TestOnceMetadataRebuildRetainsOperandOwnership(t *testing.T) {
	f := newFixture(t)
	input := f.constant(5)
	args := []ScalarID{input}
	once := f.scalar(ScalarSpec{Scalar: Scalar{Kind: Once, Scope: f.scope, Layout: f.integer}, Operands: args})
	args[0] = 0
	slot := f.slot(f.integer, false, 10, 1)
	graph := f.freeze(f.scan(10, slot))
	node, _ := graph.Scalar(once)
	operand, _ := graph.Operand(once, 0)
	if operand != input {
		t.Fatalf("caller mutated owned operand: %d", operand)
	}
	copyFixture := newFixture(t)
	copyInput := copyFixture.constant(7)
	metadata := node.Metadata()
	metadata.Scope, metadata.Layout = copyFixture.scope, copyFixture.integer
	copyID := copyFixture.scalar(ScalarSpec{Scalar: metadata, Operands: []ScalarID{copyInput}})
	copySlot := copyFixture.slot(copyFixture.integer, false, 10, 1)
	copied := copyFixture.freeze(copyFixture.scan(10, copySlot))
	copyOperand, _ := copied.Operand(copyID, 0)
	value, err := copied.CloneConstant(t.Context(), copyOperand, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if value.IntVal != 7 {
		t.Fatalf("rebuilt operand=%+v", value)
	}
}
