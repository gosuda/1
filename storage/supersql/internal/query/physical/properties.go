package physical

import (
	"math"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (p *Plan) slot(s Slot) (model.Type, error) {
	g, ok := p.Graph(s.Graph)
	if !ok {
		return model.Type{}, invalid("slot graph")
	}
	info, ok := g.Slot(s.ID)
	if !ok || info.Layout != s.Layout || info.Nullable != s.Nullable {
		return model.Type{}, invalid("slot layout or nullability mismatch")
	}
	typ, ok := g.Type(s.Layout)
	if !ok {
		return model.Type{}, invalid("slot type")
	}
	return typ, nil
}
func containsSlot(schema []Slot, slot Slot) bool { return slices.Contains(schema, slot) }
func (p *Plan) key(k Key, schema []Slot) error {
	typ, err := p.slot(k.Slot)
	if err != nil {
		return err
	}
	if typ != k.Type || typ.Collation != k.Collation || !containsSlot(schema, k.Slot) {
		return invalid("key is absent or has incompatible SQL type/collation")
	}
	return nil
}
func (p *Plan) properties(value Properties, schema []Slot) error {
	if value.Rewindability != OnePass && value.Rewindability != Rewindable {
		return invalid("missing rewindability")
	}
	for i, slot := range value.Parameters {
		if _, err := p.slot(slot); err != nil {
			return err
		}
		if slices.Contains(value.Parameters[:i], slot) {
			return invalid("duplicate parameter")
		}
	}
	for _, order := range value.Ordering {
		if err := p.key(order.Key, schema); err != nil {
			return err
		}
		validDirection := order.Direction == ir.Ascending || order.Direction == ir.Descending
		validNullOrder := order.Nulls == ir.NullsFirst || order.Nulls == ir.NullsLast
		if order.Operator == 0 || !validDirection || !validNullOrder {
			return invalid("incomplete ordering key")
		}
	}
	d := value.Distribution
	switch d.Kind {
	case SingleNode:
		if d.Partitions != 1 || len(d.Keys) != 0 || d.Function != 0 || d.Epoch != 0 {
			return invalid("invalid SingleNode distribution")
		}
	case HashPartitioned, RangePartitioned:
		if d.Partitions < 2 || len(d.Keys) == 0 || d.Function == 0 || d.Epoch == 0 {
			return invalid("partition keys, function and epoch required")
		}
	case Replicated:
		if d.Partitions < 2 || len(d.Keys) != 0 || d.Function != 0 || d.Epoch == 0 {
			return invalid("invalid replicated distribution")
		}
	default:
		return invalid("missing distribution")
	}
	for i, key := range d.Keys {
		if err := p.key(key, schema); err != nil {
			return err
		}
		if slices.Contains(d.Keys[:i], key) {
			return invalid("duplicate partition key")
		}
	}
	return nil
}
func sameDistribution(a, b Distribution) bool {
	return a.Kind == b.Kind && a.Partitions == b.Partitions && a.Function == b.Function && a.Epoch == b.Epoch && slices.Equal(a.Keys, b.Keys)
}
func orderPrefix(delivered, required []OrderKey) bool {
	return len(delivered) >= len(required) && slices.Equal(delivered[:len(required)], required)
}
func satisfies(delivered, required Properties) bool {
	return sameDistribution(delivered.Distribution, required.Distribution) && orderPrefix(delivered.Ordering, required.Ordering) && (required.Rewindability == OnePass || delivered.Rewindability == Rewindable) && slices.Equal(delivered.Parameters, required.Parameters)
}
func finite(values ...float64) bool {
	for _, value := range values {
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return false
		}
	}
	return true
}
func metadata(n NodeSpec) error {
	c, e, m := n.Cost, n.Estimate, n.Memory
	if !finite(c.Startup, c.Total, c.CPURows, c.CPUBatches, c.DecodeBytes, c.IndexProbes, c.VersionMerge, c.SpillReadBytes, c.SpillWriteBytes, c.ExchangeBytes) || c.Total < c.Startup {
		return invalid("invalid cost")
	}
	if !finite(e.Rows, e.LowerRows, e.UpperRows, e.RowBytes, e.Confidence) || e.LowerRows > e.Rows || e.UpperRows < e.Rows || e.Confidence > 1 {
		return invalid("invalid cardinality interval")
	}
	hasSpill := m.SpillBytes != 0 || c.SpillReadBytes != 0 || c.SpillWriteBytes != 0
	unsupportedSpill := !m.Spillable && hasSpill
	if m.PeakBytes < m.WorkingBytes || unsupportedSpill {
		return invalid("invalid memory or spill metadata")
	}
	if n.Break < Streaming || n.Break > ExchangeBoundary {
		return invalid("missing pipeline boundary")
	}
	return nil
}

func ExpectedBreak(a Algorithm) PipelineBreak {
	switch a {
	case HashJoin:
		return BuildRight
	case HashAggregate, Sort, TopK, HashDistinct, Window, CTEMaterialize, Materialize, Modify:
		return MaterializeInput
	case RecursiveUnion, HashSetOp:
		return MaterializeBoth
	case LocalGather, LocalOrderedMerge, LocalHashRepartition:
		return ExchangeBoundary
	default:
		return Streaming
	}
}
func logicalKind(a Algorithm) ir.RelKind {
	switch a {
	case TableScan, PrimaryKeyScan, SecondaryScan:
		return ir.Scan
	case CatalogScan:
		return ir.CatalogScan
	case Sample:
		return ir.Sample
	case Values:
		return ir.Values
	case GenerateSeries:
		return ir.GenerateSeries
	case GenerateSubscripts:
		return ir.GenerateSubscripts
	case Unnest:
		return ir.Unnest
	case RowsFrom:
		return ir.RowsFrom
	case ProjectSet:
		return ir.ProjectSet
	case Filter:
		return ir.Filter
	case Project:
		return ir.Project
	case LookupJoin, NestedLoopJoin, HashJoin, MergeJoin:
		return ir.Join
	case Apply:
		return ir.Apply
	case HashAggregate, StreamAggregate:
		return ir.Aggregate
	case Sort:
		return ir.Sort
	case TopK, Limit:
		return ir.Limit
	case HashDistinct, StreamDistinct:
		return ir.Distinct
	case Window:
		return ir.Window
	case CTEMaterialize:
		return ir.CTEProducer
	case CTEScan, WorktableScan:
		return ir.CTEConsumer
	case RecursiveUnion:
		return ir.RecursiveUnion
	case LockRows:
		return ir.LockRows
	case Modify:
		return ir.Modify
	case UnionAll, HashSetOp:
		return ir.SetOp
	default:
		return 0
	}
}
func validChildCount(a Algorithm, count int) bool {
	switch a {
	case TableScan, PrimaryKeyScan, SecondaryScan, CatalogScan, Values, GenerateSeries, GenerateSubscripts, Unnest, CTEScan, WorktableScan:
		return count == 0
	case LookupJoin, NestedLoopJoin, Apply, HashJoin, MergeJoin, RecursiveUnion, UnionAll, HashSetOp:
		return count == 2
	case RowsFrom:
		return count >= 2
	default:
		return count == 1
	}
}
