// Package groupingset replays frozen projected input through bounded aggregates.
package groupingset

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

// Sets and Groupings index Aggregate.GroupColumns, not input columns. Empty sets
// are global aggregates; repeated sets retain bag multiplicity. Each Groupings
// entry has 1..31 ordered arguments; repeats retain their bit positions. OutputLayouts
// contains all grouping columns, aggregate results, then int4 GROUPING results.
// WorkBytes covers the driver, replay spool and one aggregate pass. Output batches
// use Query's output budget. Query and Encoder are borrowed; New copies metadata.
type Options struct {
	Aggregate aggregate.Options
	Sets      [][]uint32
	Groupings [][]uint32
	Spool     rowstore.Limits
}

type admission struct {
	owned uint64
	child uint64
}

func resource(message string, cause error) error {
	return &model.Error{Code: "53200", Message: message, Cause: errors.Join(runtime.ErrBudget, cause)}
}

func charge(remaining *uint64, count, unit uint64) error {
	if unit == 0 {
		return nil
	}
	if count > *remaining/unit {
		return resource("admit grouping set workspace", nil)
	}
	*remaining -= count * unit
	return nil
}

func layoutBytes(ctx context.Context, layouts []model.TypeLayout, remaining *uint64, depth int) error {
	if depth > 64 {
		return model.ErrInvalidTypeLayout
	}
	if err := charge(remaining, uint64(len(layouts)), uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
		return err
	}
	for _, layout := range layouts {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		if err := charge(remaining, uint64(len(layout.FieldNames)), uint64(unsafe.Sizeof(""))); err != nil {
			return err
		}
		for _, name := range layout.FieldNames {
			if err := context.Cause(ctx); err != nil {
				return err
			}
			if err := charge(remaining, uint64(len(name)), 1); err != nil {
				return err
			}
		}
		if len(layout.Children) != 0 {
			if err := layoutBytes(ctx, layout.Children, remaining, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func inspect(ctx context.Context, options Options) (admission, error) {
	a, s := options.Aggregate, options.Spool
	if err := context.Cause(ctx); err != nil {
		return admission{}, err
	}
	missing := a.Query == nil || a.Encoder == nil || len(options.Sets) == 0
	invalidBatch := a.BatchRows <= 0 || a.BatchBytes == 0 || a.MaxRecordBytes < 128
	invalidMemory := a.GroupBytes == 0 || a.PartitionMemoryBytes == 0 || a.WorkBytes == 0
	if missing || invalidBatch || invalidMemory || a.MaxPartitions < 2 {
		return admission{}, runtime.ErrInvalidConfig
	}
	if err := context.Cause(a.Query.Context()); err != nil {
		return admission{}, err
	}
	for _, size := range [...]uint64{a.BatchBytes, a.MaxRecordBytes, a.GroupBytes, a.PartitionMemoryBytes, a.WorkBytes, uint64(a.MaxPartitions)} {
		if size > uint64(math.MaxInt)/128 {
			return admission{}, runtime.ErrInvalidConfig
		}
	}
	invalidExtent := s.MaxRows == 0 || s.MaxBytes == 0 || s.MaxRecordBytes < 32
	invalidBuffers := s.MetadataBytes == 0 || s.DecodeBytes == 0 || s.MaxReaders <= 0
	if invalidExtent || invalidBuffers {
		return admission{}, runtime.ErrInvalidConfig
	}
	for _, size := range [...]uint64{s.MemoryBytes, s.MaxBytes, s.MaxRecordBytes, s.MetadataBytes, s.DecodeBytes} {
		if size > uint64(math.MaxInt) {
			return admission{}, runtime.ErrInvalidConfig
		}
	}
	if s.MaxRecordBytes > uint64(math.MaxInt)-8192 || s.DecodeBytes > uint64(math.MaxInt)-s.MaxRecordBytes-8192 {
		return admission{}, runtime.ErrInvalidConfig
	}
	if len(a.GroupColumns) > len(a.OutputLayouts) || len(a.Calls) > len(a.OutputLayouts)-len(a.GroupColumns) {
		return admission{}, runtime.ErrInvalidConfig
	}
	aggregateColumns := len(a.GroupColumns) + len(a.Calls)
	if len(options.Groupings) != len(a.OutputLayouts)-aggregateColumns {
		return admission{}, runtime.ErrInvalidConfig
	}
	for _, layout := range a.OutputLayouts[aggregateColumns:] {
		if layout.Type.ID != 23 || layout.Type.Storage != model.TypeInt64 || len(layout.Children) != 0 {
			return admission{}, columnar.ErrValueLayout
		}
	}
	remaining := a.WorkBytes
	// The append codec is released by Seal before the replay reader opens.
	for _, size := range [...]uint64{s.MetadataBytes, s.MemoryBytes, s.MaxRecordBytes, s.DecodeBytes, 8192} {
		if err := charge(&remaining, 1, size); err != nil {
			return admission{}, err
		}
	}
	spoolPeak := a.WorkBytes - remaining
	fixed := uint64(unsafe.Sizeof(cursor{})) + uint64(unsafe.Sizeof(replayCursor{})) + 2*uint64(unsafe.Sizeof(columnar.Builder{})) + 4096 + a.MaxRecordBytes*8
	if err := charge(&remaining, 1, fixed); err != nil {
		return admission{}, err
	}
	beforeLayouts := remaining
	if err := layoutBytes(ctx, a.InputLayouts, &remaining, 0); err != nil {
		return admission{}, err
	}
	inputLayoutBytes := beforeLayouts - remaining
	if err := layoutBytes(ctx, a.OutputLayouts, &remaining, 0); err != nil {
		return admission{}, err
	}
	for _, layouts := range [][]model.TypeLayout{a.InputLayouts, a.OutputLayouts} {
		for _, layout := range layouts {
			if err := layout.Validate(); err != nil {
				return admission{}, err
			}
		}
	}
	spoolMetadata := uint64(unsafe.Sizeof(rowstore.Spool{})) + 256 + uint64(unsafe.Sizeof(rowstore.Reader{})) + 128
	spoolRemaining := s.MetadataBytes
	for _, size := range [...]uint64{spoolMetadata, inputLayoutBytes, uint64(len(a.InputLayouts)) * uint64(unsafe.Sizeof(model.Value{}))} {
		if err := charge(&spoolRemaining, 1, size); err != nil {
			return admission{}, err
		}
	}
	// Root headers for the pass schema, retained batches and remapped row.
	for _, item := range [...]struct{ count, unit uint64 }{
		{uint64(len(a.OutputLayouts)), uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(unsafe.Sizeof(model.Value{}))},
		{uint64(aggregateColumns), uint64(unsafe.Sizeof(model.TypeLayout{}))},
		{uint64(len(a.InputLayouts)), uint64(unsafe.Sizeof(columnar.Vector{}))},
		{uint64(len(a.GroupColumns)), 2 * uint64(unsafe.Sizeof(int(0)))},
		{uint64(len(a.Calls)), uint64(unsafe.Sizeof(aggregate.Call{}))},
		{uint64(len(a.DirectGroupColumns)), uint64(unsafe.Sizeof(int(0)))},
		{uint64(len(options.Sets)), uint64(unsafe.Sizeof([]uint32{}))},
		{uint64(len(options.Groupings)), uint64(unsafe.Sizeof([]uint32{})) + uint64(unsafe.Sizeof(model.Value{}))},
	} {
		if err := charge(&remaining, item.count, item.unit); err != nil {
			return admission{}, err
		}
	}
	for _, call := range a.Calls {
		if err := charge(&remaining, uint64(len(call.Arguments)), uint64(unsafe.Sizeof(int(0)))); err != nil {
			return admission{}, err
		}
		if err := charge(&remaining, uint64(len(call.DirectLayouts)), uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
			return admission{}, err
		}
		if err := charge(&remaining, uint64(len(call.OrderBy)), uint64(unsafe.Sizeof(aggregate.OrderKey{}))); err != nil {
			return admission{}, err
		}
		if call.FilterColumn != nil {
			if err := charge(&remaining, 1, uint64(unsafe.Sizeof(int(0)))); err != nil {
				return admission{}, err
			}
		}
	}
	if len(options.Groupings) != 0 {
		if err := charge(&remaining, uint64(len(a.GroupColumns)), uint64(unsafe.Sizeof(bool(false)))); err != nil {
			return admission{}, err
		}
	}
	for _, args := range options.Groupings {
		if err := context.Cause(ctx); err != nil {
			return admission{}, err
		}
		if len(args) == 0 || len(args) > 31 {
			return admission{}, runtime.ErrInvalidConfig
		}
		if err := charge(&remaining, uint64(len(args)), uint64(unsafe.Sizeof(uint32(0)))); err != nil {
			return admission{}, err
		}
		for _, index := range args {
			if uint64(index) >= uint64(len(a.GroupColumns)) {
				return admission{}, runtime.ErrInvalidConfig
			}
		}
	}
	for _, set := range options.Sets {
		if err := context.Cause(ctx); err != nil {
			return admission{}, err
		}
		if err := charge(&remaining, uint64(len(set)), uint64(unsafe.Sizeof(uint32(0)))); err != nil {
			return admission{}, err
		}
		for i, index := range set {
			if uint64(index) >= uint64(len(a.GroupColumns)) {
				return admission{}, runtime.ErrInvalidConfig
			}
			for _, previous := range set[:i] {
				if index == previous {
					return admission{}, runtime.ErrInvalidConfig
				}
			}
		}
	}
	if remaining == 0 {
		return admission{}, resource("admit grouping set aggregate pass", nil)
	}
	a.OutputLayouts = a.OutputLayouts[:aggregateColumns]
	if err := aggregate.CheckSupport(ctx, a); err != nil {
		return admission{}, err
	}
	for i, column := range a.GroupColumns {
		if !model.TypeLayoutsEqual(a.InputLayouts[column], a.OutputLayouts[i]) {
			return admission{}, columnar.ErrValueLayout
		}
	}
	return admission{owned: a.WorkBytes - spoolPeak - remaining, child: remaining}, nil
}

// CheckSupport validates the executable shape and replay admission without
// pulling input, reserving query memory or opening a spool.
func CheckSupport(ctx context.Context, options Options) error {
	_, err := inspect(ctx, options)
	return err
}

// New takes input ownership on every outcome. When OwnDirectEvaluators is set
// on Aggregate, it likewise takes evaluator ownership on every outcome. Next
// and Close must be serialized; returned batches are borrowed until either
// method is called.
func New(ctx context.Context, input storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	plan, err := inspect(ctx, options)
	if input == nil {
		err = errors.Join(err, runtime.ErrInvalidConfig)
	}
	if err != nil {
		if input != nil {
			err = errors.Join(err, input.Close())
		}
		err = errors.Join(err, closeDirectEvaluators(options.Aggregate))
		return nil, err
	}
	claim, err := options.Aggregate.Query.Allocator(runtime.WorkMemory).Reserve(plan.owned)
	if err != nil {
		return nil, errors.Join(err, input.Close(), closeDirectEvaluators(options.Aggregate))
	}
	c := &cursor{input: input, options: options, claim: claim}
	if err := c.configure(ctx, plan); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

func (c *cursor) configure(ctx context.Context, plan admission) error {
	a := &c.options.Aggregate
	remaining := plan.owned
	copyLayouts := func(layouts []model.TypeLayout) ([]model.TypeLayout, error) {
		owned := make([]model.TypeLayout, len(layouts))
		for i, layout := range layouts {
			value, used, err := model.CopyTypeLayout(ctx, layout, remaining)
			if err != nil {
				return nil, err
			}
			owned[i], remaining = value, remaining-used
		}
		return owned, nil
	}
	var err error
	if a.InputLayouts, err = copyLayouts(a.InputLayouts); err != nil {
		return err
	}
	if a.OutputLayouts, err = copyLayouts(a.OutputLayouts); err != nil {
		return err
	}
	groups := make([]int, len(a.GroupColumns))
	copy(groups, a.GroupColumns)
	a.GroupColumns = groups
	calls := make([]aggregate.Call, len(a.Calls))
	for i, call := range a.Calls {
		calls[i] = call
		calls[i].Arguments = make([]int, len(call.Arguments))
		copy(calls[i].Arguments, call.Arguments)
		calls[i].DirectLayouts = make([]model.TypeLayout, len(call.DirectLayouts))
		copy(calls[i].DirectLayouts, call.DirectLayouts)
		calls[i].OrderBy = make([]aggregate.OrderKey, len(call.OrderBy))
		copy(calls[i].OrderBy, call.OrderBy)
		if call.FilterColumn != nil {
			column := *call.FilterColumn
			calls[i].FilterColumn = &column
		}
	}
	a.Calls = calls
	a.DirectGroupColumns = append([]int(nil), a.DirectGroupColumns...)
	sets := make([][]uint32, len(c.options.Sets))
	for i, set := range c.options.Sets {
		sets[i] = make([]uint32, len(set))
		copy(sets[i], set)
	}
	c.options.Sets = sets
	if len(c.options.Groupings) != 0 {
		groupings := make([][]uint32, len(c.options.Groupings))
		for i, args := range c.options.Groupings {
			if err := context.Cause(ctx); err != nil {
				return err
			}
			groupings[i] = make([]uint32, len(args))
			copy(groupings[i], args)
		}
		c.options.Groupings = groupings
		c.masks = make([]model.Value, len(groupings))
		c.present = make([]bool, len(groups))
	}
	c.options.Spool.MaxReaders = 1
	a.WorkBytes = plan.child
	c.passGroups = make([]int, len(groups))
	c.passLayouts = make([]model.TypeLayout, len(groups)+len(calls))
	if len(a.DirectGroupColumns) != 0 {
		c.passDirectColumns = make([]int, len(a.DirectGroupColumns))
	}
	c.row = make([]model.Value, len(a.OutputLayouts))
	c.batch.Columns = make([]columnar.Vector, 0, max(len(a.InputLayouts), len(a.OutputLayouts)))
	c.builder, err = columnar.NewBuilder(a.OutputLayouts, columnar.BuilderOptions{MaxRows: a.BatchRows, MaxBytes: a.BatchBytes, Allocator: a.Query.Allocator(runtime.OutputMemory)})
	if err != nil {
		return err
	}
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(a.Query.Context(), c.cancel)
	return nil
}
