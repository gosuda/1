package opt

import (
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (o *optimizer) newExpression(node physical.NodeSpec, children []*expression, producer *expression) *expression {
	if !o.charge(1, uint64(unsafe.Sizeof(expression{}))) {
		return nil
	}
	e := &expression{node: node, children: children, producer: producer}
	e.scalars = allocate[*expression](o, uint64(len(node.ScalarSubplans)))
	if o.err != nil {
		return nil
	}
	for i, subplan := range node.ScalarSubplans {
		scalar, _ := o.graph.Scalar(subplan.Scalar.Scalar)
		body := o.groups[scalar.Relation].seed
		e.scalars[i] = o.winner(scalar.Relation, body.node.Delivered)
		if o.err != nil {
			return nil
		}
	}
	e.node.Required = allocate[physical.Properties](o, uint64(len(children)))
	if o.err != nil {
		return nil
	}
	for i, child := range children {
		if child == nil {
			o.err = ErrInvalid
			return nil
		}
		e.node.Required[i] = child.node.Delivered
		if node.Algorithm == physical.RowsFrom {
			e.node.Required[i].Ordering = nil
			e.node.Required[i].Rewindability = physical.OnePass
		}
	}
	r, _ := o.graph.Rel(node.Logical.Rel)
	if len(children) == 1 && len(e.node.Delivered.Ordering) == 0 {
		switch node.Algorithm {
		case physical.Filter, physical.Project, physical.ProjectSet, physical.Limit, physical.Window, physical.CTEMaterialize, physical.Materialize, physical.Sample, physical.LockRows, physical.StreamDistinct:
			e.node.Delivered.Ordering = outputOrder(children[0].node.Delivered.Ordering, node.Output)
		}
	}
	e.node.Break = physical.ExpectedBreak(node.Algorithm)
	if node.Algorithm < physical.TableScan || node.Algorithm > physical.SecondaryScan {
		e.node.Estimate = o.estimate(r, e)
	}
	o.cost(e)
	return e
}
func (o *optimizer) estimate(r ir.Rel, e *expression) physical.Estimate {
	width := float64(len(e.node.Output)) * 16
	estimate := physical.Estimate{RowBytes: width, Confidence: 1}
	if e.producer != nil {
		estimate = e.producer.node.Estimate
		estimate.RowBytes = width
		return estimate
	}
	if len(e.children) == 0 {
		if e.node.Algorithm == physical.Values {
			estimate.Rows = float64(r.Rows)
			estimate.LowerRows, estimate.UpperRows = estimate.Rows, estimate.Rows
			return estimate
		}
		if e.node.Algorithm == physical.GenerateSeries {
			return physical.Estimate{Rows: 1000, UpperRows: 1 << 64, RowBytes: width, Confidence: 0.1}
		}
		if e.node.Algorithm == physical.GenerateSubscripts {
			return physical.Estimate{Rows: 1000, UpperRows: 1<<31 - 1, RowBytes: width, Confidence: 0.1}
		}
		if e.node.Algorithm == physical.Unnest {
			return physical.Estimate{Rows: 1000, UpperRows: 1 << 63, RowBytes: width, Confidence: 0.1}
		}
		return physical.Estimate{Rows: 1000, UpperRows: 1000000, RowBytes: width, Confidence: 0.1}
	}
	if e.node.Algorithm == physical.RowsFrom {
		for _, child := range e.children {
			input := child.node.Estimate
			estimate.Rows = max(estimate.Rows, input.Rows)
			estimate.LowerRows = max(estimate.LowerRows, input.LowerRows)
			estimate.UpperRows = max(estimate.UpperRows, input.UpperRows)
			estimate.Confidence = min(estimate.Confidence, input.Confidence)
			estimate.StatisticsEpoch = max(estimate.StatisticsEpoch, input.StatisticsEpoch)
		}
		return estimate
	}
	left := e.children[0].node.Estimate
	estimate = left
	estimate.RowBytes = width
	if e.node.Algorithm == physical.ProjectSet {
		estimate.Rows = cappedMultiply(left.Rows, 1000)
		estimate.LowerRows = 0
		estimate.UpperRows = cappedMultiply(left.UpperRows, 1<<63)
		estimate.Confidence = min(left.Confidence, 0.1)
		return estimate
	}
	if len(e.children) == 2 {
		right := e.children[1].node.Estimate
		estimate.Confidence = min(left.Confidence, right.Confidence)
		estimate.StatisticsEpoch = max(left.StatisticsEpoch, right.StatisticsEpoch)
		if e.node.Algorithm == physical.UnionAll || e.node.Algorithm == physical.RecursiveUnion {
			estimate.Rows = cappedAdd(left.Rows, right.Rows)
			estimate.UpperRows = cappedAdd(left.UpperRows, right.UpperRows)
			estimate.LowerRows = cappedAdd(left.LowerRows, right.LowerRows)
			if e.node.Algorithm == physical.RecursiveUnion {
				estimate.UpperRows = 1e100
				estimate.Confidence = 0.1
			}
			return estimate
		}
		if e.node.Algorithm == physical.HashSetOp {
			estimate.LowerRows = 0
			switch r.SetKind {
			case ir.Union:
				estimate.Rows = cappedAdd(left.Rows, right.Rows)
				estimate.UpperRows = cappedAdd(left.UpperRows, right.UpperRows)
			case ir.Intersect:
				estimate.Rows = min(left.Rows, right.Rows)
				estimate.UpperRows = min(left.UpperRows, right.UpperRows)
			case ir.Except:
				estimate.Rows = left.Rows * 0.5
				estimate.UpperRows = left.UpperRows
			}
			if !r.All {
				estimate.Rows = min(estimate.Rows, max(1, math.Sqrt(estimate.Rows)))
			}
			return estimate
		}
		estimate.Rows = cappedMultiply(left.Rows, right.Rows)
		estimate.UpperRows = cappedMultiply(left.UpperRows, right.UpperRows)
		estimate.LowerRows = 0
		if r.Predicate != 0 {
			estimate.Rows *= o.selectivity(r.Predicate, 0)
			estimate.Confidence = min(estimate.Confidence, 0.25)
		}
		switch r.JoinKind {
		case ir.LeftJoin:
			estimate.Rows = max(left.Rows, estimate.Rows)
			estimate.LowerRows = left.LowerRows
			estimate.UpperRows = max(left.UpperRows, estimate.UpperRows)
		case ir.RightJoin:
			estimate.Rows = max(right.Rows, estimate.Rows)
			estimate.LowerRows = right.LowerRows
			estimate.UpperRows = max(right.UpperRows, estimate.UpperRows)
		case ir.FullJoin:
			estimate.Rows = max(left.Rows, right.Rows, estimate.Rows)
			estimate.LowerRows = max(left.LowerRows, right.LowerRows)
			estimate.UpperRows = cappedAdd(estimate.UpperRows, cappedAdd(left.UpperRows, right.UpperRows))
		case ir.SemiJoin, ir.AntiJoin:
			estimate.Rows = left.Rows * 0.5
			estimate.UpperRows = left.UpperRows
		case ir.MarkJoin:
			estimate = left
			estimate.RowBytes = width
		}
		if r.Kind == ir.Apply && (r.ApplyKind == ir.ExistsApply || r.ApplyKind == ir.ScalarApply) {
			estimate = left
			estimate.RowBytes = width
		}
		return estimate
	}
	switch e.node.Algorithm {
	case physical.Filter:
		estimate.Rows *= o.selectivity(r.Predicate, 0)
		estimate.LowerRows = 0
		estimate.Confidence = min(estimate.Confidence, 0.25)
	case physical.HashAggregate, physical.StreamAggregate:
		estimate.Rows, estimate.LowerRows, estimate.UpperRows = 0, 0, 0
		for i := uint32(0); i < r.GroupingSetCount(); i++ {
			width, _ := o.graph.GroupingSetWidth(e.node.Logical.Rel, i)
			if width == 0 {
				estimate.Rows = cappedAdd(estimate.Rows, 1)
				estimate.LowerRows = cappedAdd(estimate.LowerRows, 1)
				estimate.UpperRows = cappedAdd(estimate.UpperRows, 1)
				continue
			}
			estimate.Rows = cappedAdd(estimate.Rows, min(left.Rows, math.Sqrt(left.Rows)))
			estimate.UpperRows = cappedAdd(estimate.UpperRows, left.UpperRows)
			estimate.Confidence = min(estimate.Confidence, 0.25)
		}
	case physical.HashDistinct, physical.StreamDistinct:
		estimate.Rows = math.Sqrt(left.Rows)
		estimate.LowerRows = 0
		estimate.Confidence = min(estimate.Confidence, 0.25)
	case physical.Limit, physical.TopK:
		if !e.node.Limit.RuntimeBounds && !e.node.Limit.WithTies {
			count, offset := float64(e.node.Limit.Count), float64(e.node.Limit.Offset)
			estimate.Rows = min(count, max(0, left.Rows-offset))
			estimate.LowerRows = min(count, max(0, left.LowerRows-offset))
			estimate.UpperRows = min(count, max(0, left.UpperRows-offset))
		}
	}
	return estimate
}
func cappedAdd(a, b float64) float64      { return min(1e100, a+b) }
func cappedMultiply(a, b float64) float64 { return min(1e100, a*b) }
func byteEstimate(value float64) uint64 {
	if value >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return uint64(math.Ceil(max(0, value)))
}
func (o *optimizer) cost(e *expression) {
	n := &e.node
	rows := n.Estimate.Rows
	c := physical.Cost{CPURows: rows, CPUBatches: math.Ceil(rows / 1024)}
	inputRows, inputBytes := rows, rows*n.Estimate.RowBytes
	childTotal := 0.0
	for _, child := range e.children {
		childTotal = cappedAdd(childTotal, child.node.Cost.Total)
	}
	if e.producer != nil {
		childTotal = cappedAdd(childTotal, e.producer.node.Cost.Total)
	}
	if len(e.children) > 0 {
		inputRows = e.children[0].node.Estimate.Rows
		inputBytes = inputRows * e.children[0].node.Estimate.RowBytes
		c.CPURows = inputRows
	}
	stateBytes := 0.0
	switch n.Algorithm {
	case physical.TableScan, physical.PrimaryKeyScan, physical.SecondaryScan:
		access := o.access[n.Logical.Rel]
		scanRows, rowBytes := rows, n.Estimate.RowBytes
		blocks, versionWork := access.blocks, access.versionWork
		statistic, tracked := o.pathStatistic(n.Logical.Rel, n.Scan.Request.Target)
		if o.err != nil {
			return
		}
		if tracked && statistic.table != nil {
			if statistic.table.LiveRows.Known {
				scanRows = float64(statistic.table.LiveRows.Value) * statistic.selectivity
			}
			if statistic.table.AverageRowBytes > 0 {
				rowBytes = statistic.table.AverageRowBytes
			}
			if statistic.table.Blocks.Known {
				blocks = float64(statistic.table.Blocks.Value)
			}
			if statistic.table.VersionChainWorkKnown {
				versionWork = statistic.table.VersionChainWork
			}
		}
		c.CPURows = scanRows
		c.CPUBatches = max(math.Ceil(scanRows/1024), blocks)
		c.DecodeBytes = max(n.Scan.Estimate.DecodeBytes, scanRows*rowBytes)
		c.VersionMerge = max(n.Scan.Estimate.VisibilityChecks, scanRows*versionWork)
		if n.Algorithm != physical.TableScan {
			c.IndexProbes = max(n.Scan.Estimate.IndexProbes, 1)
			if scanRows == 0 {
				c.IndexProbes = 0
			}
		}
		if scanRows > 0 {
			c.Startup = blocks
		}
	case physical.GenerateSeries, physical.GenerateSubscripts, physical.Unnest:
		c.Startup = float64(len(n.Expressions))
	case physical.RowsFrom:
		c.CPURows = cappedMultiply(rows, float64(len(n.Output)))
		stateBytes = float64(len(e.children))*64 + n.Estimate.RowBytes
		for _, child := range e.children {
			c.Startup = cappedAdd(c.Startup, child.node.Cost.Startup)
		}
	case physical.Project, physical.ProjectSet:
		logical, _ := o.graph.Rel(n.Logical.Rel)
		c.CPURows = 0
		for i := range logical.BindingCount() {
			binding, _ := o.graph.Binding(n.Logical.Rel, i)
			scalar, _ := o.graph.Scalar(binding.Expr)
			if scalar.Kind != ir.Slot {
				c.CPURows = cappedAdd(c.CPURows, inputRows)
			}
		}
		if n.Algorithm == physical.ProjectSet {
			c.CPURows = cappedAdd(c.CPURows, rows)
			stateBytes = float64(len(n.Output))*32 + n.Estimate.RowBytes
		}
	case physical.NestedLoopJoin, physical.Apply:
		right := e.children[1].node.Estimate.Rows
		c.CPURows = cappedAdd(inputRows, cappedMultiply(inputRows, right))
		if n.Algorithm == physical.Apply {
			childTotal = cappedAdd(e.children[0].node.Cost.Total, cappedMultiply(inputRows, e.children[1].node.Cost.Total))
		}
	case physical.HashJoin:
		right := e.children[1].node.Estimate
		c.CPURows = cappedAdd(inputRows, right.Rows)
		stateBytes = right.Rows * (right.RowBytes + 32)
		c.Startup = e.children[1].node.Cost.Total + right.Rows
	case physical.MergeJoin:
		right := e.children[1].node.Estimate
		c.CPURows = cappedAdd(inputRows, right.Rows)
		// With no equality-group distribution estimate, allow a single skewed group.
		stateBytes = cappedMultiply(right.Rows, right.RowBytes)
	case physical.LookupJoin:
		c.IndexProbes = inputRows
		c.VersionMerge = max(inputRows, rows)
		c.DecodeBytes = rows * e.children[1].node.Estimate.RowBytes
		childTotal = e.children[0].node.Cost.Total
	case physical.Sort:
		c.CPURows = inputRows * math.Log2(max(2, inputRows))
		stateBytes = inputBytes
	case physical.TopK:
		retained := inputRows
		if !n.Limit.RuntimeBounds && !n.Limit.WithTies {
			retained = min(inputRows, float64(n.Limit.Offset+n.Limit.Count))
		}
		c.CPURows = inputRows * math.Log2(max(2, retained))
		stateBytes = retained * n.Estimate.RowBytes
	case physical.HashAggregate, physical.HashDistinct:
		stateBytes = rows * (n.Estimate.RowBytes + 32)
		if n.Algorithm == physical.HashAggregate {
			logical, _ := o.graph.Rel(n.Logical.Rel)
			if logical.GroupingSetCount() > 1 {
				c.CPURows = cappedMultiply(inputRows, float64(logical.GroupingSetCount())+1)
				c.CPUBatches = math.Ceil(c.CPURows / 1024)
			}
			ordered, distinct := false, false
			for i := range logical.AggregateCount() {
				call, _ := o.graph.Aggregate(n.Logical.Rel, i)
				ordered = ordered || call.OrderCount() != 0
				distinct = distinct || call.Distinct
			}
			if ordered {
				expanded := cappedMultiply(inputRows, float64(logical.AggregateCount()))
				sortRows := cappedMultiply(expanded, math.Log2(max(2, expanded)))
				passes := float64(max(1, logical.GroupingSetCount()))
				c.CPURows = cappedAdd(c.CPURows, cappedMultiply(sortRows, passes))
				c.CPUBatches = cappedAdd(c.CPUBatches, math.Ceil(cappedMultiply(expanded, passes)/1024))
				stateBytes = max(stateBytes, cappedMultiply(inputBytes, float64(logical.AggregateCount())))
				if distinct {
					ordinalRows := cappedMultiply(inputRows, passes)
					c.CPURows = cappedAdd(c.CPURows, ordinalRows)
					c.CPUBatches = cappedAdd(c.CPUBatches, math.Ceil(ordinalRows/1024))
				}
			}
		}
	case physical.StreamAggregate, physical.StreamDistinct:
		stateBytes = n.Estimate.RowBytes + 32
	case physical.Materialize, physical.CTEMaterialize, physical.Window, physical.Modify:
		stateBytes = inputBytes
	case physical.RecursiveUnion, physical.HashSetOp:
		stateBytes = inputBytes + e.children[1].node.Estimate.Rows*e.children[1].node.Estimate.RowBytes
	case physical.LocalGather, physical.LocalOrderedMerge, physical.LocalHashRepartition:
		stateBytes = float64(n.Exchange.QueueBytes)
		c.ExchangeBytes = inputBytes
	}
	if n.Algorithm == physical.HashAggregate || n.Algorithm == physical.StreamAggregate {
		logical, _ := o.graph.Rel(n.Logical.Rel)
		retained := 0.0
		for i := range logical.AggregateCount() {
			call, _ := o.graph.Aggregate(n.Logical.Rel, i)
			switch call.Function {
			case 2335, 4053, 3538, 3545:
				retained = cappedAdd(retained, inputBytes)
			}
		}
		stateBytes = max(stateBytes, retained)
	}
	if n.Break != physical.Streaming || stateBytes > 0 {
		state := max(uint64(1), byteEstimate(stateBytes))
		spillable := n.Break != physical.ExchangeBoundary && n.Algorithm != physical.RowsFrom && n.Algorithm != physical.ProjectSet
		n.Memory = physical.Memory{WorkingBytes: min(state, o.options.WorkingMemory), PeakBytes: min(state, o.options.WorkingMemory), Spillable: spillable}
		if state > o.options.WorkingMemory {
			if !spillable {
				o.err = ErrLimit
				return
			}
			n.Memory.SpillBytes = state - o.options.WorkingMemory
			c.SpillReadBytes, c.SpillWriteBytes = float64(n.Memory.SpillBytes), float64(n.Memory.SpillBytes)
		}
		if n.Break == physical.MaterializeInput || n.Break == physical.MaterializeBoth {
			c.Startup = max(c.Startup, childTotal+inputRows)
		}
	}
	for _, body := range e.scalars {
		executions := 1.0
		if len(body.node.Delivered.Parameters) != 0 {
			executions = inputRows
		}
		childTotal = cappedAdd(childTotal, cappedMultiply(executions, body.node.Cost.Total))
	}
	local := c.CPURows + c.CPUBatches*8 + c.DecodeBytes/64 + c.IndexProbes*40 + c.VersionMerge*0.25 + (c.SpillReadBytes+c.SpillWriteBytes)/16 + c.ExchangeBytes/32
	c.Total = max(c.Startup, cappedAdd(childTotal, local+c.Startup))
	n.Cost = c
}

func sameDistribution(a, b physical.Distribution) bool {
	return a.Kind == b.Kind && a.Partitions == b.Partitions && a.Function == b.Function && a.Epoch == b.Epoch && slices.Equal(a.Keys, b.Keys)
}
func ordered(delivered, required []physical.OrderKey) bool {
	return len(delivered) >= len(required) && slices.Equal(delivered[:len(required)], required)
}
func delivers(a, b physical.Properties) bool {
	return sameDistribution(a.Distribution, b.Distribution) && ordered(a.Ordering, b.Ordering) && (b.Rewindability == physical.OnePass || a.Rewindability == physical.Rewindable) && slices.Equal(a.Parameters, b.Parameters)
}
func (o *optimizer) enforce(e *expression, required physical.Properties) *expression {
	if e == nil || o.err != nil {
		return nil
	}
	if !slices.Equal(e.node.Delivered.Parameters, required.Parameters) {
		o.unsupported(e.node.Logical.Rel, "property requirement changes correlation inputs")
		return nil
	}
	if !sameDistribution(e.node.Delivered.Distribution, required.Distribution) {
		n := o.enforcerNode(e)
		switch required.Distribution.Kind {
		case physical.SingleNode:
			n.Algorithm = physical.LocalGather
			if len(required.Ordering) != 0 && ordered(e.node.Delivered.Ordering, required.Ordering) {
				n.Algorithm = physical.LocalOrderedMerge
				n.Delivered.Ordering = required.Ordering
			}
		case physical.HashPartitioned:
			n.Algorithm = physical.LocalHashRepartition
		default:
			o.unsupported(e.node.Logical.Rel, "distribution has no local exchange implementation")
			return nil
		}
		n.Delivered.Distribution = required.Distribution
		n.Exchange = physical.ExchangeSpec{Inputs: e.node.Delivered.Distribution.Partitions, QueueBytes: min(o.options.WorkingMemory, uint64(64<<10))}
		e = o.newExpression(n, []*expression{e}, nil)
	}
	if o.err != nil {
		return nil
	}
	if !ordered(e.node.Delivered.Ordering, required.Ordering) {
		n := o.enforcerNode(e)
		n.Algorithm = physical.Sort
		n.Delivered.Ordering = required.Ordering
		e = o.newExpression(n, []*expression{e}, nil)
	}
	if o.err != nil {
		return nil
	}
	if required.Rewindability == physical.Rewindable && e.node.Delivered.Rewindability != physical.Rewindable {
		n := o.enforcerNode(e)
		n.Algorithm = physical.Materialize
		n.Delivered.Rewindability = physical.Rewindable
		n.Delivered.Ordering = e.node.Delivered.Ordering
		e = o.newExpression(n, []*expression{e}, nil)
	}
	return e
}
func (o *optimizer) enforcerNode(e *expression) physical.NodeSpec {
	return physical.NodeSpec{Logical: e.node.Logical, Output: e.node.Output, Delivered: physical.Properties{Distribution: e.node.Delivered.Distribution, Rewindability: physical.OnePass, Parameters: e.node.Delivered.Parameters}}
}
