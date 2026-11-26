package physical

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) validateCTEDelivery(ctx context.Context, node NodeSpec) error {
	producer := node.Worktable.Producer
	if producer == 0 || uint64(producer) > uint64(len(p.nodes)) {
		return invalid("CTE delivery has no producer")
	}
	return p.validateOrdinalDelivery(ctx, node, p.nodes[producer-1])
}

func (p *Plan) validateOrdinalDelivery(ctx context.Context, node, input NodeSpec) error {
	if err := p.validateOrdinalLayouts(ctx, node.Output, input.Output); err != nil {
		return err
	}
	before, after := input.Delivered.Distribution, node.Delivered.Distribution
	sameShape := before.Kind == after.Kind && before.Partitions == after.Partitions && before.Function == after.Function && before.Epoch == after.Epoch
	if !sameShape || len(before.Keys) != len(after.Keys) {
		return invalid("materialized output changes distribution")
	}
	for i, key := range before.Keys {
		expected, ok := ordinalKey(key, input.Output, node.Output)
		if !ok || expected != after.Keys[i] {
			return invalid("materialized output partition key differs")
		}
	}
	if len(node.Delivered.Ordering) > len(input.Delivered.Ordering) {
		return invalid("materialized output invents ordering")
	}
	for i, order := range node.Delivered.Ordering {
		expected := input.Delivered.Ordering[i]
		key, ok := ordinalKey(expected.Key, input.Output, node.Output)
		if !ok {
			return invalid("materialized ordering key missing")
		}
		expected.Key = key
		if order != expected {
			return invalid("materialized output ordering differs")
		}
	}
	return nil
}

func (p *Plan) validateOrdinalLayouts(ctx context.Context, output, input []Slot) error {
	if len(output) != len(input) {
		return invalid("materialized output width differs")
	}
	for i, slot := range output {
		original := input[i]
		left, _ := p.Graph(slot.Graph)
		right, _ := p.Graph(original.Graph)
		equal, err := sameLayout(ctx, left, slot.Layout, right, original.Layout)
		if err != nil {
			return err
		}
		if !equal || original.Nullable && !slot.Nullable {
			return invalid("materialized output layout differs")
		}
	}
	return nil
}

// Graph construction bounds layout depth; comparison borrows the immutable trees.
func sameLayout(ctx context.Context, left *ir.Graph, leftID ir.LayoutID, right *ir.Graph, rightID ir.LayoutID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if left == right && leftID == rightID {
		return true, nil
	}
	leftType, _ := left.Type(leftID)
	rightType, _ := right.Type(rightID)
	children := left.LayoutChildCount(leftID)
	if leftType != rightType || children != right.LayoutChildCount(rightID) {
		return false, nil
	}
	for i := range children {
		leftChild, _ := left.LayoutChild(leftID, i)
		rightChild, _ := right.LayoutChild(rightID, i)
		equal, err := sameLayout(ctx, left, leftChild, right, rightChild)
		if err != nil || !equal {
			return equal, err
		}
	}
	return true, nil
}

func ordinalKey(key Key, input, output []Slot) (Key, bool) {
	for i, slot := range input {
		if slot.Graph == key.Slot.Graph && slot.ID == key.Slot.ID {
			key.Slot = output[i]
			return key, true
		}
	}
	return Key{}, false
}
