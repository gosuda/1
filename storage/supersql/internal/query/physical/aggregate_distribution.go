package physical

import (
	"slices"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func validateAggregateDistribution(node, input NodeSpec, graph *ir.Graph, logical ir.Rel) error {
	before, after := input.Delivered.Distribution, node.Delivered.Distribution
	sameShape := before.Kind == after.Kind && before.Partitions == after.Partitions && before.Function == after.Function && before.Epoch == after.Epoch
	if !sameShape || len(before.Keys) != len(after.Keys) {
		return invalid("aggregation changes partition mapping")
	}
	if before.Kind == Replicated {
		return invalid("aggregate would duplicate replicated input")
	}
	for i, key := range before.Keys {
		ordinal := slices.Index(node.Aggregate.GroupKeys, key)
		if ordinal < 0 || ordinal >= len(node.Output) {
			return invalid("grouping cannot retain its input partition key")
		}
		expected := key
		expected.Slot = node.Output[ordinal]
		if after.Keys[i] != expected {
			return invalid("aggregate partition key is not its grouped output")
		}
		for set := uint32(0); set < logical.GroupingSetCount(); set++ {
			width, ok := graph.GroupingSetWidth(node.Logical.Rel, set)
			if !ok {
				return invalid("grouping set metadata missing")
			}
			present := false
			for column := uint32(0); column < width; column++ {
				group, ok := graph.GroupingKey(node.Logical.Rel, set, column)
				if !ok {
					return invalid("grouping set key missing")
				}
				present = present || group == uint32(ordinal)
			}
			if !present {
				return invalid("grouping set crosses input partitions")
			}
		}
	}
	return nil
}

func validateAggregateOrdering(node, input NodeSpec) error {
	if node.Algorithm == HashAggregate {
		if len(node.Delivered.Ordering) != 0 {
			return invalid("hash aggregate invents ordering")
		}
		return nil
	}
	for i, order := range node.Delivered.Ordering {
		if i >= len(node.Aggregate.GroupKeys) || i >= len(input.Delivered.Ordering) || i >= len(node.Output) {
			return invalid("aggregate ordering exceeds grouped output")
		}
		expected := input.Delivered.Ordering[i]
		expected.Key.Slot = node.Output[i]
		if order != expected {
			return invalid("aggregate ordering is not its grouped output order")
		}
	}
	return nil
}
