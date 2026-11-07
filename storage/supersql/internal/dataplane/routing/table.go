// Package routing intersects typed SQL key spans with immutable local partitions.
package routing

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrInvalidSpec = errors.New("invalid local routing specification")
	ErrCoverage    = errors.New("local partitions do not form gap-free ordered coverage")
	ErrLimit       = errors.New("local routing resource limit exceeded")
)

// Limits are per operation. Every field must be positive. MaxBytes charges
// cumulative owned and scratch allocations; MaxNodes counts visited value nodes.
// MaxSpans bounds each input/output span list; MaxWork charges comparisons,
// visited values and bytes, including the declared encoder inputs.
type Limits struct {
	MaxNodes      uint64
	MaxSpans      uint64
	MaxPartitions uint64
	MaxWork       uint64
	MaxBytes      uint64
	MaxMorsels    uint64
}

func DefaultLimits() Limits {
	return Limits{MaxNodes: 1 << 20, MaxSpans: 1024, MaxPartitions: 4096, MaxWork: 1 << 26, MaxBytes: 64 << 20, MaxMorsels: 8192}
}

// Partition identities are scoped to Spec.Target and Spec.Epoch. Placement is
// local; the descriptor carries no transport or execution resource.
type Partition struct {
	ID   uint64
	Span storage.KeySpan
}

type Spec struct {
	Target     storage.ObjectRef
	Epoch      uint64
	KeyLayout  []storage.KeyColumn
	Partitions []Partition
}

type Morsel struct {
	Target      storage.ObjectRef
	Epoch       uint64
	PartitionID uint64
	Span        storage.KeySpan
}

// Table owns its metadata and borrows the immutable, concurrency-safe encoder.
// Each Route result owns its bounds independently of the table and other results.
type Table struct {
	target     storage.ObjectRef
	epoch      uint64
	layout     []storage.KeyColumn
	partitions []partition
	encoder    storage.KeyEncoder
}

type partition struct {
	id   uint64
	span interval
}

func NewTable(ctx context.Context, spec Spec, encoder storage.KeyEncoder, limits Limits) (*Table, error) {
	budget, err := newBudget(ctx, limits)
	if err != nil {
		return nil, err
	}
	if err := spec.Target.Validate(); err != nil {
		return nil, fmt.Errorf("routing target: %w", err)
	}
	if spec.Epoch == 0 || encoder == nil || len(spec.KeyLayout) == 0 || len(spec.Partitions) == 0 {
		return nil, ErrInvalidSpec
	}
	n := uint64(len(spec.Partitions))
	if n > limits.MaxPartitions || n > limits.MaxSpans || uint64(len(spec.KeyLayout)) > limits.MaxNodes {
		return nil, ErrLimit
	}
	if err := budget.allocate(1, uint64(unsafe.Sizeof(Table{}))); err != nil {
		return nil, err
	}
	for _, key := range spec.KeyLayout {
		if err := budget.visitLayout(ctx, model.TypeLayout{Type: key.Type, Children: key.Children}, 0); err != nil {
			return nil, err
		}
	}
	layout, layoutBytes, err := storage.CopyKeyLayout(ctx, spec.KeyLayout, limits.MaxBytes-budget.bytes)
	if err != nil {
		if errors.Is(err, model.ErrValueBudget) {
			return nil, ErrLimit
		}
		return nil, err
	}
	budget.bytes += layoutBytes
	for _, key := range layout {
		if key.ColumnID == 0 || key.Type.ID == 0 || !key.Type.Storage.Valid() {
			return nil, storage.ErrInvalidLayout
		}
	}
	// An empty prefix still asks the SQL service to validate every key type and
	// collation, including full-range tables without any finite boundaries.
	if _, err := budget.encode(ctx, encoder, layout, nil); err != nil {
		return nil, err
	}
	if err := budget.allocate(n, uint64(unsafe.Sizeof(partition{}))+8); err != nil {
		return nil, err
	}
	ids := make([]uint64, len(spec.Partitions))
	for i, descriptor := range spec.Partitions {
		if descriptor.ID == 0 {
			return nil, ErrInvalidSpec
		}
		ids[i] = descriptor.ID
	}
	if err := budget.work(ctx, n*uint64(bits.Len64(n))*2); err != nil {
		return nil, err
	}
	slices.Sort(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i-1] == ids[i] {
			return nil, ErrInvalidSpec
		}
	}
	table := &Table{target: spec.Target, epoch: spec.Epoch, layout: layout, encoder: encoder, partitions: make([]partition, len(spec.Partitions))}
	for i, descriptor := range spec.Partitions {
		span, err := compileSpan(ctx, descriptor.Span, layout, encoder, budget)
		if err != nil {
			return nil, fmt.Errorf("partition %d: %w", descriptor.ID, err)
		}
		if i == 0 && span.lower.infinity != -1 {
			return nil, ErrCoverage
		}
		if i > 0 {
			order, err := budget.compare(ctx, table.partitions[i-1].span.upper, span.lower)
			if err != nil {
				return nil, err
			}
			if order != 0 {
				return nil, ErrCoverage
			}
		}
		owned, err := budget.copySpan(ctx, descriptor.Span)
		if err != nil {
			return nil, err
		}
		span.lower.bound, span.upper.bound = owned.Lower, owned.Upper
		table.partitions[i] = partition{id: descriptor.ID, span: span}
	}
	if table.partitions[len(table.partitions)-1].span.upper.infinity != 1 {
		return nil, ErrCoverage
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return table, nil
}

// Matches checks logical identity without coupling it to partition placement.
func (t *Table) Matches(target storage.ObjectRef, layout []storage.KeyColumn) bool {
	return t != nil && t.target == target && storage.KeyLayoutsEqual(t.layout, layout)
}

// Route returns key-ordered, disjoint morsels, keeping partition boundaries.
// Empty input means no work; full coverage requires an explicit unbounded span.
// Any error, including cancellation, returns no output.
func (t *Table) Route(ctx context.Context, requested []storage.KeySpan, limits Limits) ([]Morsel, error) {
	budget, err := newBudget(ctx, limits)
	if err != nil {
		return nil, err
	}
	if t == nil || t.encoder == nil {
		return nil, ErrInvalidSpec
	}
	if uint64(len(requested)) > limits.MaxSpans || uint64(len(t.partitions)) > limits.MaxPartitions {
		return nil, ErrLimit
	}
	if len(requested) == 0 {
		return nil, nil
	}
	if err := budget.allocate(uint64(len(requested)), uint64(unsafe.Sizeof(interval{}))); err != nil {
		return nil, err
	}
	spans := make([]interval, len(requested))
	for i, span := range requested {
		spans[i], err = compileSpan(ctx, span, t.layout, t.encoder, budget)
		if err != nil {
			return nil, err
		}
	}
	spans, err = normalize(ctx, spans, budget)
	if err != nil {
		return nil, err
	}
	var count uint64
	if err := t.intersections(ctx, spans, budget, func(_ uint64, _ interval) error {
		if count == limits.MaxMorsels || count == limits.MaxSpans {
			return ErrLimit
		}
		count++
		return nil
	}); err != nil {
		return nil, err
	}
	if err := budget.allocate(count, uint64(unsafe.Sizeof(Morsel{}))); err != nil {
		return nil, err
	}
	result := make([]Morsel, int(count))
	index := 0
	if err := t.intersections(ctx, spans, budget, func(id uint64, span interval) error {
		owned, err := budget.copySpan(ctx, storage.KeySpan{Lower: span.lower.bound, Upper: span.upper.bound})
		if err != nil {
			return err
		}
		result[index] = Morsel{Target: t.target, Epoch: t.epoch, PartitionID: id, Span: owned}
		index++
		return nil
	}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func (t *Table) intersections(ctx context.Context, spans []interval, budget *budget, emit func(uint64, interval) error) error {
	for p, s := 0, 0; p < len(t.partitions) && s < len(spans); {
		partition, requested := t.partitions[p], spans[s]
		lower, upper := partition.span.lower, partition.span.upper
		order, err := budget.compare(ctx, lower, requested.lower)
		if err != nil {
			return err
		}
		if order < 0 {
			lower = requested.lower
		}
		endOrder, err := budget.compare(ctx, upper, requested.upper)
		if err != nil {
			return err
		}
		if endOrder > 0 {
			upper = requested.upper
		}
		order, err = budget.compare(ctx, lower, upper)
		if err != nil {
			return err
		}
		if order < 0 {
			if err := emit(partition.id, interval{lower: lower, upper: upper}); err != nil {
				return err
			}
		}
		if endOrder <= 0 {
			p++
		}
		if endOrder >= 0 {
			s++
		}
	}
	return nil
}
