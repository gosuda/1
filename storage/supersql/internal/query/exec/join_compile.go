package exec

import (
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) join(node physical.NodeSpec, layouts []model.TypeLayout, children []storage.BatchCursor) (storage.BatchCursor, error) {
	options := c.portal.env.Join
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	options.Kind = relation.JoinKind
	options.MarkKind = relation.MarkKind
	options.ExactLookup = false
	options.Query = c.portal.env.Query
	options.Encoder = c.portal.env.Keys
	options.BatchRows = c.portal.env.Limits.BatchRows
	options.BatchBytes = c.portal.env.Limits.BatchBytes
	options.OutputLayouts = layouts
	options.Parameters = c.portal.env.Parameters
	options.Captures = c.captures
	options.Subqueries = c
	options.Sequences = c.portal.env.Sequences
	options.Relations = c.portal.env.Relations
	options.Temporal = c.portal.env.Temporal
	options.Settings = c.portal.env.Settings
	options.TextSearch = c.portal.env.TextSearch
	options.Advisory = c.portal.env.Advisory
	options.Native = c.portal.env.Native
	var slots [2][]physical.Slot
	for side := range slots {
		child, _ := c.plan.Node(node.Children[side])
		if err := c.charge(uint64(child.OutputCount()) * uint64(unsafe.Sizeof(physical.Slot{}))); err != nil {
			return nil, err
		}
		slots[side] = make([]physical.Slot, child.OutputCount())
		for i := range slots[side] {
			slots[side][i], _ = child.Output(uint32(i))
		}
	}
	var err error
	options.LeftLayouts, err = c.layouts(slots[0])
	if err != nil {
		return nil, err
	}
	options.RightLayouts, err = c.layouts(slots[1])
	if err != nil {
		return nil, err
	}
	if err := c.charge(uint64(len(layouts)) * uint64(unsafe.Sizeof(exjoin.Column{}))); err != nil {
		return nil, err
	}
	options.Output = make([]exjoin.Column, len(layouts))
	for i := range options.Output {
		slot := node.Output[i]
		if relation.JoinKind == ir.MarkJoin && slot.Graph == node.Logical.Graph && slot.ID == relation.MarkSlot {
			options.Output[i] = exjoin.Column{Side: exjoin.Mark, Index: 0}
		} else if i < len(slots[0]) {
			options.Output[i] = exjoin.Column{Side: exjoin.Left, Index: i}
		} else {
			options.Output[i] = exjoin.Column{Side: exjoin.Right, Index: i - len(slots[0])}
		}
	}
	options.Keys = nil
	// Mark predicates must see unequal and NULL witnesses as well as equal keys.
	keyed := node.Algorithm == physical.HashJoin || node.Algorithm == physical.MergeJoin || node.Algorithm == physical.LookupJoin
	if keyed && relation.JoinKind != ir.MarkJoin {
		if err := c.charge(uint64(len(node.Join.Keys)) * uint64(unsafe.Sizeof(exjoin.Key{}))); err != nil {
			return nil, err
		}
		options.Keys = make([]exjoin.Key, len(node.Join.Keys))
		for i, key := range node.Join.Keys {
			left, right := joinColumn(slots[0], key.Left.Slot), joinColumn(slots[1], key.Right.Slot)
			if left < 0 || right < 0 {
				return nil, ErrInvalid
			}
			options.Keys[i] = exjoin.Key{Left: left, Right: right, NullEqual: key.Nulls == physical.NullsEqual}
		}
	}
	options.Residual = nil
	options.ResidualInputs = nil
	if relation.Predicate != 0 {
		count := len(slots[0]) + len(slots[1])
		if err := c.charge(uint64(count) * (uint64(unsafe.Sizeof(physical.Slot{})) + uint64(unsafe.Sizeof(exjoin.Column{})))); err != nil {
			return nil, err
		}
		inputs := make([]physical.Slot, 0, count)
		inputs = append(inputs, slots[0]...)
		inputs = append(inputs, slots[1]...)
		options.ResidualInputs = make([]exjoin.Column, count)
		for i := range inputs {
			if i < len(slots[0]) {
				options.ResidualInputs[i] = exjoin.Column{Side: exjoin.Left, Index: i}
			} else {
				options.ResidualInputs[i] = exjoin.Column{Side: exjoin.Right, Index: i - len(slots[0])}
			}
		}
		options.Residual, err = c.program(node.Logical.Graph, []ir.ScalarID{relation.Predicate}, inputs)
		if err != nil {
			return nil, err
		}
	}
	leftDistribution := c.prepared.nodes[node.Children[0]].node.Delivered.Distribution
	rightDistribution := c.prepared.nodes[node.Children[1]].node.Delivered.Distribution
	partitioned := leftDistribution.Kind == physical.HashPartitioned && rightDistribution.Kind == physical.HashPartitioned
	if partitioned && (node.Algorithm == physical.HashJoin || node.Algorithm == physical.MergeJoin) {
		return c.partitionedJoin(node, children, options)
	}
	if node.Algorithm == physical.LookupJoin {
		return c.lookup(node, children, options)
	}
	if node.Algorithm == physical.MergeJoin {
		ordering, err := c.mergeOrdering(node)
		if err != nil {
			return nil, err
		}
		if c.preparing {
			return nil, exjoin.CheckMergeSupport(c.portal.ctx, options, ordering)
		}
		return exjoin.NewMerge(c.portal.ctx, children[0], children[1], options, ordering)
	}
	if c.preparing {
		return nil, nil
	}
	cursor, err := exjoin.New(c.portal.ctx, children[0], children[1], options)
	return cursor, err
}

func joinColumn(slots []physical.Slot, key physical.Slot) int {
	for i, slot := range slots {
		if slot.Graph == key.Graph && slot.ID == key.ID {
			return i
		}
	}
	return -1
}

func (c *invocation) mergeOrdering(node physical.NodeSpec) ([]exjoin.MergeOrder, error) {
	left, _ := c.plan.Node(node.Children[0])
	right, _ := c.plan.Node(node.Children[1])
	if err := c.charge(uint64(len(node.Join.Keys)) * uint64(unsafe.Sizeof(exjoin.MergeOrder{}))); err != nil {
		return nil, err
	}
	ordering := make([]exjoin.MergeOrder, len(node.Join.Keys))
	for i, key := range node.Join.Keys {
		l, leftFound := left.Delivered().Ordering(uint32(i))
		r, rightFound := right.Delivered().Ordering(uint32(i))
		missing := !leftFound || !rightFound
		wrongKeys := l.Key != key.Left || r.Key != key.Right
		if missing || wrongKeys {
			return nil, ErrInvalid
		}
		if l.Direction != r.Direction || l.Nulls != r.Nulls || l.Operator != r.Operator {
			return nil, ErrInvalid
		}
		if !canonicalOrderingOperator(c.portal.env.Keys, l.Operator, l.Key.Type, l.Direction) {
			return nil, ErrCapability
		}
		ordering[i] = exjoin.MergeOrder{Descending: l.Direction == ir.Descending, NullsFirst: l.Nulls == ir.NullsFirst}
	}
	return ordering, nil
}
