// Package physical owns validated, immutable local execution plans. Algorithm
// selection is not evidence that an executor implements the selected algorithm.
package physical

import (
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

// IDs are one-based and local to a Plan. Rewritten expressions must belong to an
// explicitly registered replacement graph; graph-local IDs cannot be transplanted.
type NodeID uint32
type GraphID uint32
type RelRef struct {
	Graph GraphID
	Rel   ir.RelID
}
type ScalarRef struct {
	Graph  GraphID
	Scalar ir.ScalarID
}

// ScalarSubplan is a lazy expression dependency in this plan.
type ScalarSubplan struct {
	Scalar ScalarRef
	Root   NodeID
}
type Slot struct {
	Graph    GraphID
	ID       model.SlotID
	Layout   ir.LayoutID
	Nullable bool
}

type Algorithm uint8

const (
	TableScan Algorithm = iota + 1
	PrimaryKeyScan
	SecondaryScan
	Values
	Filter
	Project
	LookupJoin
	NestedLoopJoin
	Apply
	HashJoin
	MergeJoin
	HashAggregate
	StreamAggregate
	Sort
	TopK
	Limit
	HashDistinct
	StreamDistinct
	Window
	CTEMaterialize
	CTEScan
	RecursiveUnion
	WorktableScan
	LockRows
	Modify
	UnionAll
	Materialize
	LocalGather
	LocalOrderedMerge
	LocalHashRepartition
	// HashSetOp counts both inputs using SQL not-distinct equality. SetKind and
	// All on the logical relation determine output multiplicity.
	HashSetOp
	GenerateSeries
	GenerateSubscripts
	Unnest
	RowsFrom
	ProjectSet
	CatalogScan
	Sample
)

type DistributionKind uint8

const (
	SingleNode DistributionKind = iota + 1
	HashPartitioned
	RangePartitioned
	Replicated
)

// Key carries resolved SQL identity, not an encoder or a compiled expression.
type Key struct {
	Slot      Slot
	Type      model.Type
	Collation model.CollationID
}
type OrderKey struct {
	Key       Key
	Direction ir.Direction
	Nulls     ir.NullOrder
	Operator  model.OperatorID
}
type Distribution struct {
	Kind       DistributionKind
	Keys       []Key
	Partitions uint32
	Function   uint64
	Epoch      uint64
}
type Rewindability uint8

const (
	OnePass Rewindability = iota + 1
	Rewindable
)

type Properties struct {
	Ordering      []OrderKey
	Distribution  Distribution
	Rewindability Rewindability
	Parameters    []Slot
}

type PipelineBreak uint8

const (
	Streaming PipelineBreak = iota + 1
	BuildRight
	MaterializeInput
	MaterializeBoth
	ExchangeBoundary
)

type Cost struct {
	Startup, Total                                              float64
	CPURows, CPUBatches, DecodeBytes, IndexProbes, VersionMerge float64
	SpillReadBytes, SpillWriteBytes, ExchangeBytes              float64
}
type Estimate struct {
	Rows, LowerRows, UpperRows, RowBytes float64
	Confidence                           float64
	StatisticsEpoch                      uint64
}
type Memory struct {
	WorkingBytes, PeakBytes, SpillBytes uint64
	Spillable                           bool
}

type ScanColumn struct {
	Source, Target model.ColumnID
}

type LookupScanSpec struct {
	Enabled        bool
	OwnerColumn    model.ColumnID
	BaseColumnMap  []ScanColumn
	BaseRequest    storage.PushdownRequest
	BaseAccepted   storage.Predicate
	BaseResidual   storage.Predicate
	BaseProjection []model.ColumnID
}

type ScanSpec struct {
	Source    storage.ObjectRef
	ColumnMap []ScanColumn
	// CapabilityEpoch identifies the capability decision, not a read fence.
	CapabilityEpoch    uint64
	LimitOwner         NodeID
	Request            storage.PushdownRequest
	Accepted, Residual storage.Predicate
	Projection         []model.ColumnID
	Ordering           []storage.ScanOrdering
	Estimate           storage.ScanEstimate
	AppliedLimit       storage.ScanLimit
	Lookup             LookupScanSpec
}

type NullEquality uint8

const (
	NullsUnequal NullEquality = iota + 1
	NullsEqual
)

type JoinKey struct {
	Left, Right Key
	Equality    model.OperatorID
	Nulls       NullEquality
	Predicate   ScalarRef
}
type JoinSpec struct {
	Keys []JoinKey
	// ANY/ALL marks retain SQL UNKNOWN; nullable NOT IN is not a plain anti join.
	NullAware   bool
	MarkKind    ir.SubqueryKind
	Lookup      storage.ObjectRef
	LookupIndex model.IndexID
}

type AggregatePhase uint8

const (
	CompleteAggregate AggregatePhase = iota + 1
	PartialAggregate
	FinalAggregate
)

// Combine is an explicit resolved aggregate-state contract. Zero functions are
// never interpreted as permission to merge partial states.
type Combine struct {
	Aggregate                            model.FunctionID
	Combine                              model.FunctionID
	StateGraph                           GraphID
	StateLayout                          ir.LayoutID
}
type AggregateSpec struct {
	Phase     AggregatePhase
	GroupKeys []Key
	Combine   []Combine
}

type LimitScope uint8

const (
	GlobalLimit LimitScope = iota + 1
	PartitionLimit
)

type LimitSpec struct {
	Scope         LimitScope
	Offset, Count uint64
	// RuntimeBounds evaluates the logical relation's Count/Offset per invocation.
	// Static Count/Offset must then be zero and cannot justify local pruning.
	RuntimeBounds bool
	// A partition cap is an optimization only when its ancestor consumes all
	// partitions with the same global top-K and ordering.
	Global   NodeID
	WithTies bool
}
type ExchangeSpec struct {
	Inputs     uint32
	QueueBytes uint64
}

// Producer is an ordinary dependency for CTEScan and a recursive feedback
// identity for WorktableScan; feedback is not a child edge in the pipeline DAG.
type WorktableSpec struct {
	Graph    GraphID
	CTE      ir.CTEID
	Producer NodeID
}

type CatalogScanSpec struct {
	Relation model.CatalogRelationID
	Columns  []model.ColumnID
}

type NodeSpec struct {
	Logical               RelRef
	Algorithm             Algorithm
	Children              []NodeID
	Output                []Slot
	Expressions           []ScalarRef
	ScalarSubplans        []ScalarSubplan
	StatementDependencies []NodeID
	Required              []Properties // one requirement for each child
	Delivered             Properties
	Cost                  Cost
	Estimate              Estimate
	Memory                Memory
	Break                 PipelineBreak
	Scan                  ScanSpec
	Join                  JoinSpec
	Aggregate             AggregateSpec
	Limit                 LimitSpec
	Exchange              ExchangeSpec
	Worktable             WorktableSpec
	Catalog               CatalogScanSpec
}
type Spec struct {
	Graphs []*ir.Graph
	Nodes  []NodeSpec
	Root   NodeID
}
type Limits struct {
	MaxNodes, MaxOperands, MaxProperties, MaxDepth uint32
	MaxBytes                                       uint64
}

type Plan struct {
	graphs []*ir.Graph
	nodes  []NodeSpec
	root   NodeID
	bytes  uint64
}

// Node is a read-only view. Slice-bearing data is accessed by checked index or
// copied explicitly through Plan.CloneNode.
type Node struct {
	plan *Plan
	id   NodeID
}
type Metadata struct {
	Logical   RelRef
	Algorithm Algorithm
	Cost      Cost
	Estimate  Estimate
	Memory    Memory
	Break     PipelineBreak
	Limit     LimitSpec
	Exchange  ExchangeSpec
	Worktable WorktableSpec
}

type PropertyView struct{ value *Properties }

func (p *Plan) Root() NodeID       { return p.root }
func (p *Plan) NodeCount() uint32  { return uint32(len(p.nodes)) }
func (p *Plan) GraphCount() uint32 { return uint32(len(p.graphs)) }
func (p *Plan) OwnedBytes() uint64 { return p.bytes }
func (p *Plan) Graph(id GraphID) (*ir.Graph, bool) {
	if id == 0 || uint64(id) > uint64(len(p.graphs)) {
		return nil, false
	}
	return p.graphs[id-1], true
}
func (p *Plan) Node(id NodeID) (Node, bool) {
	if id == 0 || uint64(id) > uint64(len(p.nodes)) {
		return Node{}, false
	}
	return Node{p, id}, true
}
func (n Node) Metadata() Metadata {
	s := n.plan.nodes[n.id-1]
	return Metadata{s.Logical, s.Algorithm, s.Cost, s.Estimate, s.Memory, s.Break, s.Limit, s.Exchange, s.Worktable}
}
func (n Node) ChildCount() uint32            { return uint32(len(n.plan.nodes[n.id-1].Children)) }
func (n Node) OutputCount() uint32           { return uint32(len(n.plan.nodes[n.id-1].Output)) }
func (n Node) ExpressionCount() uint32       { return uint32(len(n.plan.nodes[n.id-1].Expressions)) }
func (n Node) Child(i uint32) (NodeID, bool) { return item(n.plan.nodes[n.id-1].Children, i) }
func (n Node) Output(i uint32) (Slot, bool)  { return item(n.plan.nodes[n.id-1].Output, i) }
func (n Node) Expression(i uint32) (ScalarRef, bool) {
	return item(n.plan.nodes[n.id-1].Expressions, i)
}
func (n Node) ScalarSubplanCount() uint32 { return uint32(len(n.plan.nodes[n.id-1].ScalarSubplans)) }
func (n Node) ScalarSubplan(i uint32) (ScalarSubplan, bool) {
	return item(n.plan.nodes[n.id-1].ScalarSubplans, i)
}
func (n Node) StatementDependencyCount() uint32 {
	return uint32(len(n.plan.nodes[n.id-1].StatementDependencies))
}
func (n Node) StatementDependency(i uint32) (NodeID, bool) {
	return item(n.plan.nodes[n.id-1].StatementDependencies, i)
}
func (n Node) Delivered() PropertyView { return PropertyView{&n.plan.nodes[n.id-1].Delivered} }
func (n Node) Required(i uint32) (PropertyView, bool) {
	s := &n.plan.nodes[n.id-1]
	if uint64(i) >= uint64(len(s.Required)) {
		return PropertyView{}, false
	}
	return PropertyView{&s.Required[i]}, true
}
func (v PropertyView) OrderingCount() uint32              { return uint32(len(v.value.Ordering)) }
func (v PropertyView) Ordering(i uint32) (OrderKey, bool) { return item(v.value.Ordering, i) }
func (v PropertyView) Distribution() (DistributionKind, uint32, uint64, uint64) {
	d := &v.value.Distribution
	return d.Kind, d.Partitions, d.Function, d.Epoch
}
func (v PropertyView) PartitionKeyCount() uint32         { return uint32(len(v.value.Distribution.Keys)) }
func (v PropertyView) PartitionKey(i uint32) (Key, bool) { return item(v.value.Distribution.Keys, i) }
func (v PropertyView) Rewindability() Rewindability      { return v.value.Rewindability }
func (v PropertyView) ParameterCount() uint32            { return uint32(len(v.value.Parameters)) }
func (v PropertyView) Parameter(i uint32) (Slot, bool)   { return item(v.value.Parameters, i) }
func item[T any](values []T, i uint32) (T, bool) {
	if uint64(i) >= uint64(len(values)) {
		var zero T
		return zero, false
	}
	return values[i], true
}
