// Package opt selects bounded, validated physical plans without opening executors.
package opt

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/opt/span"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

var (
	ErrInvalid     = errors.New("invalid optimizer options or input")
	ErrLimit       = errors.New("optimizer hard resource limit exceeded")
	ErrUnsupported = errors.New("missing physical planning contract")
)

type UnsupportedError struct {
	Relation ir.RelID
	Reason   string
}

func (e *UnsupportedError) Error() string {
	return fmt.Sprintf("%v: relation %d: %s", ErrUnsupported, e.Relation, e.Reason)
}
func (e *UnsupportedError) Unwrap() error { return ErrUnsupported }

type Statistics interface {
	Lookup(context.Context, storage.ObjectRef) (*statistics.Table, bool, error)
}

type ExtendedStatisticDefinition struct {
	OID             model.OID
	Keys            []model.ColumnID
	ExpressionCount int
	Kinds           []byte
}

type ExtendedStatistics interface {
	Definitions(context.Context, storage.ObjectRef) ([]ExtendedStatisticDefinition, bool, error)
}

type StatisticVersion struct {
	Target storage.ObjectRef
	Epoch  uint64
}

// Zero soft limits select defaults; negative limits are invalid. Hard limits
// include original-order seed construction, not only memo exploration.
type Options struct {
	IR               ir.Limits
	Plan             physical.Limits
	MaxBytes         uint64
	MaxWork          uint64
	MaxOutputColumns uint32
	MaxAccessPaths   int
	WorkingMemory    uint64
	MemoExpressions  int
	MemoBytes        int64
	SearchTime       time.Duration
	// Parameters are borrowed for one Optimize call and specialize scan bounds.
	Parameters          []span.Parameter
	Access               AccessPlanner
	Statistics           Statistics
	ExtendedStatistics   ExtendedStatistics
	StatisticsLimits     statistics.Limits
	StatisticsKeyEncoder storage.KeyEncoder
	Required             physical.Properties
}

type Result struct {
	Plan             *physical.Plan
	Statistics       []StatisticVersion
	BudgetExhausted  bool
	PlanningDuration time.Duration
	MemoExpressions  int
	MemoBytes        uint64
}

type logicalProperties struct {
	output     []physical.Slot
	parameters []physical.Slot
	keys       [][]physical.Slot
	estimate   physical.Estimate
}
type expression struct {
	node     physical.NodeSpec
	children []*expression
	producer *expression
	scalars  []*expression
}
type group struct {
	logical      ir.RelID
	properties   logicalProperties
	alternatives []*expression
	seed         *expression
	active       bool
}
type optimizer struct {
	ctx         context.Context
	graph       *ir.Graph
	options     Options
	groups      []group
	work, bytes uint64
	memoBytes   uint64
	expressions int
	err         error
	exhausted   bool
	searchStart time.Time
	access      []scanAccess
	predicates  []ir.ScalarID
	predicateSlots [][]model.SlotID
	producers   []*expression
	lockedOccurrences []model.ScanOccurrence
}

func Optimize(ctx context.Context, graph *ir.Graph, options Options) (Result, error) {
	return optimize(ctx, graph, options, true)
}

func optimizeOriginal(ctx context.Context, graph *ir.Graph, options Options) (Result, error) {
	return optimize(ctx, graph, options, false)
}

func optimize(ctx context.Context, graph *ir.Graph, options Options, reorder bool) (Result, error) {
	start := time.Now()
	if ctx == nil {
		return Result{}, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if graph == nil || graph.Root() == 0 {
		return Result{}, ErrInvalid
	}
	if err := validateOptions(&options); err != nil {
		return Result{}, err
	}
	if uint64(graph.RelCount())+uint64(graph.ScalarCount()) > uint64(options.IR.MaxNodes) || graph.SlotCount() > options.IR.MaxSlots || graph.OwnedBytes() > options.IR.MaxBytes {
		return Result{}, ErrLimit
	}
	var err error
	original := graph
	preparation := options
	var preparationWork, preparationBytes uint64
	if graph.CTECount() != 0 {
		preparationWork = uint64(graph.RelCount())*3 + uint64(options.IR.MaxOperands) + uint64(graph.CTECount())*3
		preparationBytes = uint64(graph.CTECount())*16 + uint64(graph.RelCount())*2 + 32
		if preparationWork >= options.MaxWork || preparationBytes >= options.MaxBytes {
			return Result{}, ErrLimit
		}
		preparation.MaxWork = preparationWork
	}
	graph, err = prepareGraph(ctx, graph, preparation)
	if err != nil {
		return Result{}, err
	}
	o := optimizer{ctx: ctx, graph: graph, options: options, work: preparationWork, bytes: preparationBytes}
	if graph != original && !o.charge(options.IR.MaxBytes, 1) {
		return Result{}, o.err
	}
	o.groups = allocate[group](&o, uint64(graph.RelCount())+1)
	o.access = allocate[scanAccess](&o, uint64(graph.RelCount())+1)
	o.predicates = allocate[ir.ScalarID](&o, uint64(graph.RelCount())+1)
	o.predicateSlots = allocate[[]model.SlotID](&o, uint64(graph.RelCount())+1)
	o.producers = allocate[*expression](&o, uint64(graph.CTECount())+1)
	o.collectLockedOccurrences()
	o.inventory()
	seed := o.seed(graph.Root(), 0)
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()) && o.step(1); id++ {
		r, _ := graph.Rel(id)
		if r.Kind != ir.CTEProducer {
			continue
		}
		cte, _ := graph.CTE(r.CTE)
		body, _ := graph.Rel(cte.Body)
		if body.Kind == ir.Modify {
			o.seed(id, 0)
		}
	}
	if o.err != nil {
		return Result{}, o.err
	}
	seed = o.enforce(seed, options.Required)
	if o.err != nil {
		return Result{}, o.err
	}
	best, err := o.plan(seed)
	if err != nil {
		return Result{}, err
	}
	bestCost := seed.node.Cost.Total
	o.searchStart = time.Now()
	for id := ir.RelID(1); id <= ir.RelID(graph.RelCount()) && o.search(); id++ {
		if o.groups[id].seed == nil {
			continue
		}
		o.explore(id)
	}
	if o.err != nil {
		return Result{}, o.err
	}
	if !o.exhausted || o.expressions != 0 {
		candidate := o.winner(graph.Root(), options.Required)
		if o.err != nil {
			return Result{}, o.err
		}
		if candidate != nil && candidate.node.Cost.Total < bestCost {
			best, err = o.plan(candidate)
			if err != nil {
				return Result{}, err
			}
		}
	}
	if reorder && o.search() {
		best, err = o.reorder(best)
		if err != nil {
			return Result{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{Plan: best, Statistics: o.statisticVersions(), BudgetExhausted: o.exhausted, PlanningDuration: time.Since(start), MemoExpressions: o.expressions, MemoBytes: o.memoBytes}, nil
}

func (o *optimizer) collectLockedOccurrences() {
	var count uint64
	for id := ir.RelID(1); id <= ir.RelID(o.graph.RelCount()); id++ {
		relation, ok := o.graph.Rel(id)
		if ok && relation.Kind == ir.LockRows {
			count += uint64(relation.LockCount())
		}
	}
	o.lockedOccurrences = allocate[model.ScanOccurrence](o, count)
	o.lockedOccurrences = o.lockedOccurrences[:0]
	for id := ir.RelID(1); id <= ir.RelID(o.graph.RelCount()); id++ {
		relation, ok := o.graph.Rel(id)
		if !ok || relation.Kind != ir.LockRows {
			continue
		}
		for index := range relation.LockCount() {
			lock, ok := o.graph.RowLock(id, index)
			if ok && lock.Occurrence != 0 && !slices.Contains(o.lockedOccurrences, lock.Occurrence) {
				o.lockedOccurrences = append(o.lockedOccurrences, lock.Occurrence)
			}
		}
	}
}

func (o *optimizer) statisticVersions() []StatisticVersion {
	result := make([]StatisticVersion, 0, len(o.access))
	appendVersion := func(target storage.ObjectRef, table *statistics.Table) {
		if slices.ContainsFunc(result, func(version StatisticVersion) bool { return version.Target == target }) {
			return
		}
		epoch := uint64(0)
		if table != nil {
			epoch = table.Epoch
		}
		result = append(result, StatisticVersion{Target: target, Epoch: epoch})
	}
	for _, access := range o.access {
		if access.statisticsTracked {
			appendVersion(access.statisticsTarget, access.statistics)
		}
		for _, statistic := range access.pathStatistics {
			appendVersion(statistic.target, statistic.table)
		}
	}
	slices.SortFunc(result, func(a, b StatisticVersion) int {
		if order := cmp.Compare(a.Target.ObjectID, b.Target.ObjectID); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Target.Generation, b.Target.Generation); order != 0 {
			return order
		}
		if order := cmp.Compare(a.Target.DescriptorVersion, b.Target.DescriptorVersion); order != 0 {
			return order
		}
		return cmp.Compare(a.Target.EncodingVersion, b.Target.EncodingVersion)
	})
	return result
}

func validateOptions(o *Options) error {
	invalidMemory := o.MaxBytes == 0 || o.MaxBytes > math.MaxInt64 || o.WorkingMemory == 0 || o.WorkingMemory > math.MaxInt64
	invalidWork := o.MaxWork == 0 || o.MaxOutputColumns == 0 || o.MaxAccessPaths <= 0
	invalidSearch := o.MemoExpressions < 0 || o.MemoBytes < 0 || o.SearchTime < 0
	if invalidMemory || invalidWork || invalidSearch {
		return ErrInvalid
	}
	if o.IR.SourceBytes == 0 || o.IR.MaxNodes == math.MaxUint32 || o.IR.MaxSlots == math.MaxUint32 || o.IR.MaxOperands == math.MaxUint32 {
		return ErrInvalid
	}
	invalidIRCounts := o.IR.MaxNodes == 0 || o.IR.MaxOperands == 0 || o.IR.MaxSlots == 0
	invalidIRStorage := o.IR.MaxDepth == 0 || o.IR.MaxDepth > 512 || o.IR.MaxBytes == 0
	invalidPlanCounts := o.Plan.MaxNodes == 0 || o.Plan.MaxNodes == math.MaxUint32 || o.Plan.MaxOperands == 0 || o.Plan.MaxProperties == 0
	invalidPlanStorage := o.Plan.MaxDepth == 0 || o.Plan.MaxDepth > 512 || o.Plan.MaxBytes == 0
	if invalidIRCounts || invalidIRStorage || invalidPlanCounts || invalidPlanStorage {
		return ErrInvalid
	}
	if o.MemoExpressions == 0 {
		o.MemoExpressions = 50000
	}
	if o.MemoBytes == 0 {
		o.MemoBytes = 32 << 20
	}
	if o.SearchTime == 0 {
		o.SearchTime = 100 * time.Millisecond
	}
	if o.Required.Distribution.Kind == 0 {
		if len(o.Required.Distribution.Keys) != 0 || o.Required.Distribution.Partitions != 0 || o.Required.Distribution.Function != 0 || o.Required.Distribution.Epoch != 0 {
			return ErrInvalid
		}
		o.Required.Distribution = physical.Distribution{Kind: physical.SingleNode, Partitions: 1}
	}
	if o.Required.Rewindability == 0 {
		o.Required.Rewindability = physical.OnePass
	}
	return nil
}
func (o *optimizer) step(n uint64) bool {
	if o.err != nil {
		return false
	}
	if o.err = o.ctx.Err(); o.err != nil {
		return false
	}
	if n > o.options.MaxWork-o.work {
		o.err = ErrLimit
		return false
	}
	o.work += n
	return true
}
func (o *optimizer) charge(n, size uint64) bool {
	if !o.step(1) {
		return false
	}
	if size != 0 && n > (o.options.MaxBytes-o.bytes)/size {
		o.err = ErrLimit
		return false
	}
	o.bytes += n * size
	return true
}
func allocate[T any](o *optimizer, n uint64) []T {
	var value T
	if n > uint64(^uint(0)>>1) || !o.step(n) || !o.charge(n, uint64(unsafe.Sizeof(value))) {
		return nil
	}
	return make([]T, int(n))
}
func (o *optimizer) unsupported(id ir.RelID, reason string) {
	if o.err == nil {
		o.err = &UnsupportedError{Relation: id, Reason: reason}
	}
}
func (o *optimizer) search() bool {
	if !o.step(1) {
		return false
	}
	if o.exhausted {
		return false
	}
	if o.expressions >= o.options.MemoExpressions || o.memoBytes >= uint64(o.options.MemoBytes) || time.Since(o.searchStart) >= o.options.SearchTime {
		o.exhausted = true
		return false
	}
	return true
}
func (o *optimizer) slot(id model.SlotID) physical.Slot {
	info, _ := o.graph.Slot(id)
	return physical.Slot{Graph: 1, ID: id, Layout: info.Layout, Nullable: info.Nullable}
}
func (o *optimizer) key(id model.SlotID) physical.Key {
	slot := o.slot(id)
	typ, _ := o.graph.Type(slot.Layout)
	return physical.Key{Slot: slot, Type: typ, Collation: typ.Collation}
}
