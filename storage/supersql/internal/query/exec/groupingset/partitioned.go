package groupingset

import (
	"context"
	"errors"
	"math"
	"unsafe"

	"github.com/zeebo/xxh3"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

type partitionedCursor struct{ cursor }

func (c *partitionedCursor) Partition() uint32 { return c.partitioned.published }

type partitionedReplayCursor struct{ replayCursor }

func (r *partitionedReplayCursor) Partition() uint32 { return r.published }

type partitionedState struct {
	count, pending, published uint32
	indices                   []int
	columns                   []int
	passIndices               []int
	layout                    []storage.KeyColumn
	values                    []model.Value
}

var errInputPartition = &model.Error{Code: "XX000", Message: "grouping set input partition does not match canonical distribution key"}

func inspectPartitioned(ctx context.Context, partitioning aggregate.Partitioning, options Options) (admission, error) {
	plan, err := inspect(ctx, options)
	if err != nil {
		return admission{}, err
	}
	a := options.Aggregate
	if partitioning.Count < 2 || len(partitioning.KeyIndices) == 0 || len(partitioning.KeyIndices) > len(a.GroupColumns) || uint64(len(a.GroupColumns)) > math.MaxUint32 {
		return admission{}, runtime.ErrInvalidConfig
	}
	for i, index := range partitioning.KeyIndices {
		if err := context.Cause(ctx); err != nil {
			return admission{}, err
		}
		if index < 0 || index >= len(a.GroupColumns) {
			return admission{}, runtime.ErrInvalidConfig
		}
		for _, previous := range partitioning.KeyIndices[:i] {
			if previous == index {
				return admission{}, runtime.ErrInvalidConfig
			}
		}
		for _, set := range options.Sets {
			if err := context.Cause(ctx); err != nil {
				return admission{}, err
			}
			present := false
			for _, ordinal := range set {
				if int(ordinal) == index {
					present = true
					break
				}
			}
			if !present {
				return admission{}, &model.Error{Code: "0A000", Message: "partitioned grouping set omits a distribution key"}
			}
		}
	}
	remaining := plan.child
	if err := charge(&remaining, 1, uint64(unsafe.Sizeof(partitionedState{}))); err != nil {
		return admission{}, err
	}
	unit := 3*uint64(unsafe.Sizeof(int(0))) + uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(model.Value{}))
	if err := charge(&remaining, uint64(len(partitioning.KeyIndices)), unit); err != nil {
		return admission{}, err
	}
	// Authentication copies one distribution tuple; replay borrows the decoded
	// row. Both share the same bounded encoding and numeric workspace.
	if err := charge(&remaining, 10, a.MaxRecordBytes); err != nil {
		return admission{}, err
	}
	for _, index := range partitioning.KeyIndices {
		if a.InputLayouts[a.GroupColumns[index]].Type.Storage == model.TypeNumeric {
			if err := charge(&remaining, 1, 1<<20); err != nil {
				return admission{}, err
			}
		}
	}
	if remaining == 0 {
		return admission{}, resource("admit partitioned grouping set aggregate pass", nil)
	}
	plan.owned += plan.child - remaining
	plan.child = remaining
	return plan, nil
}

func newPartitioned(ctx context.Context, input aggregate.PartitionedInput, partitioning aggregate.Partitioning, options Options) (*partitionedCursor, error) {
	plan, err := inspectPartitioned(ctx, partitioning, options)
	if err != nil {
		if input != nil {
			err = errors.Join(err, input.Close())
		}
		return nil, err
	}
	claim, err := options.Aggregate.Query.Allocator(runtime.WorkMemory).Reserve(plan.owned)
	if err != nil {
		if input != nil {
			err = errors.Join(err, input.Close())
		}
		return nil, err
	}
	c := &partitionedCursor{cursor: cursor{input: input, options: options, claim: claim}}
	if err := c.configure(ctx, plan); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	if err := c.configurePartitioning(ctx, partitioning); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

// CheckPartitionedSupport validates every remapped aggregate pass without
// consuming input or opening the replay spool. Temporary claims are released.
func CheckPartitionedSupport(ctx context.Context, partitioning aggregate.Partitioning, options Options) error {
	c, err := newPartitioned(ctx, nil, partitioning, options)
	if err != nil {
		return err
	}
	return c.Close()
}

// NewPartitioned owns input on every outcome and freezes it once. Every set
// must retain every distribution key; repeated sets remain separate passes.
// Query and Encoder are borrowed. Next and Close must be serialized, and output
// batches and Partition remain valid until the next Next or Close.
func NewPartitioned(ctx context.Context, input aggregate.PartitionedInput, partitioning aggregate.Partitioning, options Options) (aggregate.PartitionedCursor, error) {
	if input == nil {
		return nil, runtime.ErrInvalidConfig
	}
	c, err := newPartitioned(ctx, input, partitioning, options)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *cursor) configurePartitioning(ctx context.Context, partitioning aggregate.Partitioning) error {
	s := &partitionedState{count: partitioning.Count}
	c.partitioned = s
	s.indices = make([]int, len(partitioning.KeyIndices))
	copy(s.indices, partitioning.KeyIndices)
	s.columns = make([]int, len(s.indices))
	s.passIndices = make([]int, len(s.indices))
	s.layout = make([]storage.KeyColumn, len(s.indices))
	s.values = make([]model.Value, len(s.indices))
	a := c.options.Aggregate
	for i, index := range s.indices {
		s.columns[i] = a.GroupColumns[index]
		s.layout[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: a.InputLayouts[s.columns[i]].Type, Children: a.InputLayouts[s.columns[i]].Children, NullsFirst: true}
	}
	if _, err := a.Encoder.EncodePrefix(ctx, s.layout, nil, a.MaxRecordBytes); err != nil {
		return err
	}
	for _, set := range c.options.Sets {
		if err := c.check(ctx); err != nil {
			return err
		}
		pass := c.passOptions(set)
		if err := aggregate.CheckPartitionedSupport(ctx, aggregate.Partitioning{Count: s.count, KeyIndices: s.passIndices}, pass); err != nil {
			return err
		}
	}
	return nil
}

func (s *partitionedState) encodePartition(ctx context.Context, encoder storage.KeyEncoder, maxBytes uint64) (uint32, error) {
	key, err := encoder.EncodePrefix(ctx, s.layout, s.values, maxBytes)
	if err != nil {
		return 0, err
	}
	if uint64(cap(key)) > maxBytes {
		return 0, resource("grouping set distribution key exceeds resource limit", nil)
	}
	return uint32(xxh3.Hash(key) % uint64(s.count)), nil
}

func (c *cursor) authenticatePartition(ctx context.Context) error {
	a, s := c.options.Aggregate, c.partitioned
	if len(c.batch.Columns) != len(a.InputLayouts) {
		return columnar.ErrInvalidBatch
	}
	for i, layout := range a.InputLayouts {
		if !c.batch.Columns[i].MatchesLayout(layout) {
			return columnar.ErrValueLayout
		}
	}
	if c.batch.Rows() == 0 {
		return nil
	}
	partition := c.input.(aggregate.PartitionedInput).Partition()
	if partition >= s.count {
		return errInputPartition
	}
	defer clear(s.values)
	for row := range c.batch.Rows() {
		if err := c.check(ctx); err != nil {
			return err
		}
		clear(s.values)
		remaining := a.MaxRecordBytes * 8
		for i, column := range s.columns {
			value, used, err := columnar.CopyValue(ctx, &c.batch.Columns[column], c.batch.Lane(row), remaining)
			if err != nil {
				return err
			}
			s.values[i], remaining = value, remaining-used
		}
		expected, err := s.encodePartition(ctx, a.Encoder, a.MaxRecordBytes)
		if err != nil {
			return err
		}
		if partition != expected {
			return errInputPartition
		}
	}
	return nil
}
