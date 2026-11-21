package exec

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	exmodify "github.com/gosuda/supersql-alpha/internal/query/exec/modify"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

// MeasuredCount distinguishes a measured zero from an unavailable metric.
type MeasuredCount struct {
	Value uint64
	Known bool
}

// StorageObservation reports physical source work attributed to one plan node.
type StorageObservation struct {
	ReadBytes    MeasuredCount
	DecodedBytes MeasuredCount
	BlocksRead   MeasuredCount
	BlocksPruned MeasuredCount
	RangesPruned MeasuredCount
}

// NodeObservation is an immutable snapshot of one physical node invocation set.
type NodeObservation struct {
	Node        physical.NodeID
	Rows        uint64
	Batches     uint64
	Loops       uint64
	Nanoseconds MeasuredCount
	Storage     StorageObservation
}

// Observation is an immutable execution-wide measurement snapshot.
type Observation struct {
	Nodes           []NodeObservation
	ExecutionNanos  uint64
	LockWaitNanos   MeasuredCount
	Resources       runtime.QueryUsage
	ResourcesKnown  bool
}

type measuredCounter struct {
	value atomic.Uint64
	known atomic.Bool
}

func (c *measuredCounter) add(value uint64, known bool) {
	if !known {
		return
	}
	c.known.Store(true)
	c.value.Add(value)
}

func (c *measuredCounter) snapshot() MeasuredCount {
	return MeasuredCount{Value: c.value.Load(), Known: c.known.Load()}
}

type nodeCounters struct {
	rows, batches, loops                          atomic.Uint64
	nanos                                         measuredCounter
	readBytes, decodedBytes                       measuredCounter
	blocksRead, blocksPruned, rangesPruned        measuredCounter
}

func (n *nodeCounters) ObserveScan(observation storage.ScanObservation) {
	n.readBytes.add(observation.ReadBytes.Value, observation.ReadBytes.Known)
	n.decodedBytes.add(observation.DecodedBytes.Value, observation.DecodedBytes.Known)
	n.blocksRead.add(observation.BlocksRead.Value, observation.BlocksRead.Known)
	n.blocksPruned.add(observation.BlocksPruned.Value, observation.BlocksPruned.Known)
	n.rangesPruned.add(observation.RangesPruned.Value, observation.RangesPruned.Known)
}

// Instrumentation is owned by one execution invocation. It can be attached as
// both an executor observer and a lock-wait observer.
type Instrumentation struct {
	mu             sync.Mutex
	nodes          []nodeCounters
	started        time.Time
	executionNanos uint64
	finished       bool
	usage          runtime.QueryUsage
	usageKnown     bool
	lockWaitNanos  atomic.Uint64
	timing         bool
}

// NewInstrumentation enables per-invocation counters. When timing is false,
// node calls avoid clock reads and node durations remain unknown.
func NewInstrumentation(timing bool) *Instrumentation { return &Instrumentation{timing: timing} }

func (i *Instrumentation) bind(plan *physical.Plan) error {
	if i == nil || plan == nil {
		return ErrInvalid
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	count := plan.NodeCount()
	if i.nodes != nil {
		if len(i.nodes) != int(count)+1 {
			return ErrInvalid
		}
		return nil
	}
	i.nodes = make([]nodeCounters, int(count)+1)
	i.started = time.Now()
	return nil
}

func (i *Instrumentation) ObserveLockWait(wait time.Duration) {
	if i == nil || wait <= 0 {
		return
	}
	i.lockWaitNanos.Add(uint64(wait))
}

func (i *Instrumentation) observeUsage(usage runtime.QueryUsage) {
	if i == nil {
		return
	}
	i.mu.Lock()
	if !i.usageKnown {
		i.usage = usage
		i.usageKnown = true
	}
	i.mu.Unlock()
}

func (i *Instrumentation) finish() {
	if i == nil {
		return
	}
	i.mu.Lock()
	if !i.finished {
		i.finished = true
		if !i.started.IsZero() {
			i.executionNanos = uint64(time.Since(i.started))
		}
	}
	i.mu.Unlock()
}

func (i *Instrumentation) Snapshot() Observation {
	if i == nil {
		return Observation{}
	}
	i.mu.Lock()
	result := Observation{
		ExecutionNanos: i.executionNanos,
		LockWaitNanos:  MeasuredCount{Value: i.lockWaitNanos.Load(), Known: true},
		Resources:      i.usage,
		ResourcesKnown: i.usageKnown,
	}
	nodes := i.nodes
	result.Nodes = make([]NodeObservation, max(0, len(nodes)-1))

	i.mu.Unlock()
	for index := 1; index < len(nodes); index++ {
		counters := &nodes[index]
		result.Nodes[index-1] = NodeObservation{
			Node:        physical.NodeID(index),
			Rows:        counters.rows.Load(),
			Batches:     counters.batches.Load(),
			Loops:       counters.loops.Load(),
			Nanoseconds: counters.nanos.snapshot(),
			Storage: StorageObservation{
				ReadBytes:    counters.readBytes.snapshot(),
				DecodedBytes: counters.decodedBytes.snapshot(),
				BlocksRead:   counters.blocksRead.snapshot(),
				BlocksPruned: counters.blocksPruned.snapshot(),
				RangesPruned: counters.rangesPruned.snapshot(),
			},
		}
	}
	return result
}
func (i *Instrumentation) withNode(ctx context.Context, node physical.NodeID) context.Context {
	if i == nil || node == 0 || int(node) >= len(i.nodes) {
		return ctx
	}
	return storage.WithScanObserver(ctx, &i.nodes[node])
}

type cursorObservation struct {
	source storage.BatchCursor
	node   *nodeCounters
	scan   bool
	begun  bool
	timing bool
	rows   uint64
}

func (c *cursorObservation) begin() {
	if !c.begun {
		c.begun = true
		c.node.loops.Add(1)
	}
}

func (c *cursorObservation) Next(ctx context.Context, out *columnar.Batch) error {
	c.begin()
	if c.scan {
		ctx = storage.WithScanObserver(ctx, c)
	}
	var started time.Time
	if c.timing {
		started = time.Now()
	}
	err := c.source.Next(ctx, out)
	if c.timing {
		c.node.nanos.add(uint64(time.Since(started)), true)
	}
	if err == nil {
		rows := uint64(out.Rows())
		c.node.rows.Add(rows)
		if rows != 0 {
			c.node.batches.Add(1)
		}
		c.rows += rows
	}
	return err
}

func (c *cursorObservation) Close() error { return c.source.Close() }
func (c *cursorObservation) SampleUnit(lane uint32) (uint64, bool) {
	source, ok := c.source.(storage.SamplingCursor)
	if !ok {
		return 0, false
	}
	return source.SampleUnit(lane)
}


func (c *cursorObservation) ObserveScan(observation storage.ScanObservation) {
	c.node.ObserveScan(observation)
}

type partitionedObservation struct {
	*cursorObservation
	partitioned partitionedCursor
}

func (c *partitionedObservation) partition() uint32 { return c.partitioned.partition() }

type orderedPartitionObservation struct {
	*partitionedObservation
	ordered globallyOrderedPartitions
}

func (c *orderedPartitionObservation) globallyOrdered() bool { return c.ordered.globallyOrdered() }

type modifyObservation struct {
	*cursorObservation
	modify exmodify.Cursor
}

func (c *modifyObservation) Complete(ctx context.Context) error {
	c.begin()
	var started time.Time
	if c.timing {
		started = time.Now()
	}
	err := c.modify.Complete(ctx)
	if c.timing {
		c.node.nanos.add(uint64(time.Since(started)), true)
	}
	if err == nil {
		affected := c.modify.RowsAffected()
		if affected > c.rows {
			c.node.rows.Add(affected - c.rows)
		}
	}
	return err
}

func (c *modifyObservation) RowsAffected() uint64 { return c.modify.RowsAffected() }

func (i *Instrumentation) wrap(id physical.NodeID, algorithm physical.Algorithm, source storage.BatchCursor) storage.BatchCursor {
	if i == nil || source == nil || id == 0 || int(id) >= len(i.nodes) {
		return source
	}
	scan := algorithm == physical.TableScan || algorithm == physical.PrimaryKeyScan || algorithm == physical.SecondaryScan || algorithm == physical.LookupJoin
	base := &cursorObservation{source: source, node: &i.nodes[id], scan: scan, timing: i.timing}
	if modify, ok := source.(exmodify.Cursor); ok {
		return &modifyObservation{cursorObservation: base, modify: modify}
	}
	if ordered, ok := source.(globallyOrderedPartitions); ok {
		partitioned := &partitionedObservation{cursorObservation: base, partitioned: ordered}
		return &orderedPartitionObservation{partitionedObservation: partitioned, ordered: ordered}
	}
	if partitioned, ok := source.(partitionedCursor); ok {
		return &partitionedObservation{cursorObservation: base, partitioned: partitioned}
	}
	return base
}

