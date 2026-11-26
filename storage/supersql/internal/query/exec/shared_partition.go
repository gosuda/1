package exec

import (
	"context"
	"errors"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) sharedPartitionProducer(input partitionedCursor, layouts []model.TypeLayout) (*sharedProducer, error) {
	if c.prepared == nil || c.current == 0 || uint64(c.current) >= uint64(len(c.prepared.nodes)) {
		return nil, ErrInvalid
	}
	distribution := c.prepared.nodes[c.current].node.Delivered.Distribution
	if distribution.Kind != physical.HashPartitioned || distribution.Partitions < 2 {
		return nil, ErrInvalid
	}
	env := &c.portal.env
	if env.Limits.BatchRows <= 0 || uint64(env.Limits.BatchRows) > math.MaxUint32 || env.Limits.BatchBytes == 0 {
		return nil, ErrInvalid
	}
	width := uint64(len(layouts))
	for _, allocation := range [...]struct{ count, bytes uint64 }{
		{1, uint64(unsafe.Sizeof(sharedProducer{})) + uint64(unsafe.Sizeof(partitionTagInput{}))},
		{width + 1, uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(unsafe.Sizeof(columnar.Vector{}))},
		{width, uint64(unsafe.Sizeof(columnar.Vector{}))},
	} {
		if allocation.count > math.MaxUint64/allocation.bytes {
			return nil, columnar.ErrBatchBudget
		}
		if err := c.charge(allocation.count * allocation.bytes); err != nil {
			return nil, err
		}
	}
	spoolLayouts := make([]model.TypeLayout, len(layouts)+1)
	spoolLayouts[0] = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
	copy(spoolLayouts[1:], layouts)
	transport := &partitionTagInput{input: input, partitions: distribution.Partitions, batchRows: env.Limits.BatchRows, columns: len(layouts)}
	transport.output = make([]columnar.Vector, len(layouts)+1)
	transport.batch.Columns = make([]columnar.Vector, 0, len(layouts))
	ordered, ok := input.(globallyOrderedPartitions)
	return &sharedProducer{
		input: transport, env: env, layouts: layouts, spoolLayouts: spoolLayouts,
		partitions: distribution.Partitions, ordered: ok && ordered.globallyOrdered(),
	}, nil
}

type sharedPartitionCursor struct {
	sharedCursor
	// The reader owns this row until the next read, including across a spill
	// migration caused by another consumer. Never advance it before Append.
	pending    []model.Value
	columns    []columnar.Vector
	partitions uint32
	tag        uint32
}

func (s *sharedPartitionCursor) partition() uint32 { return s.tag }

type orderedSharedPartitionCursor struct{ *sharedPartitionCursor }

func (s *orderedSharedPartitionCursor) globallyOrdered() bool { return true }

func (c *invocation) sharedPartitionReader(producer *sharedProducer, layouts []model.TypeLayout) (storage.BatchCursor, error) {
	if len(layouts) != len(producer.layouts) {
		return nil, columnar.ErrValueLayout
	}
	fixed := uint64(unsafe.Sizeof(sharedPartitionCursor{})) + uint64(unsafe.Sizeof(orderedSharedPartitionCursor{})) + 2048
	unit := uint64(unsafe.Sizeof(columnar.Vector{}))
	if uint64(len(layouts)) > (math.MaxUint64-fixed)/unit {
		return nil, columnar.ErrBatchBudget
	}
	if err := c.charge(fixed + uint64(len(layouts))*unit); err != nil {
		return nil, err
	}
	reader := &sharedPartitionCursor{
		sharedCursor: sharedCursor{producer: producer, env: &c.portal.env, layouts: layouts},
		columns:      make([]columnar.Vector, len(layouts)), partitions: producer.partitions,
	}
	var cursor storage.BatchCursor = reader
	if producer.ordered {
		cursor = &orderedSharedPartitionCursor{reader}
	}
	c.portal.owned = append(c.portal.owned, cursor)
	return cursor, nil
}

func (s *sharedPartitionCursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, s.Close())
	}
	out.Reset()
	if s.closed {
		return io.EOF
	}
	defer func() {
		if err != nil {
			if closeErr := s.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
		}
	}()
	if s.ended {
		return io.EOF
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if s.output != nil {
		if err := s.output.Close(); err != nil {
			return err
		}
		s.output = nil
	}
	s.output, err = builder(s.env, s.layouts)
	if err != nil {
		return err
	}
	maximum := batchRows(out, s.env)
	for s.output.Rows() < maximum {
		if s.pending == nil {
			s.pending, err = s.nextRow(ctx, maximum-s.output.Rows())
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
		}
		if len(s.pending) != len(s.layouts)+1 {
			return columnar.ErrValueLayout
		}
		tag := s.pending[0]
		if tag.IsNull || tag.Type != model.TypeInt64 || tag.IntVal < 0 || uint64(tag.IntVal) >= uint64(s.partitions) {
			return ErrInvalid
		}
		partition := uint32(tag.IntVal)
		if s.output.Rows() > 0 && partition != s.tag {
			break
		}
		if err := s.output.Append(ctx, s.pending[1:]); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && s.output.Rows() > 0 {
				break
			}
			return err
		}
		s.tag = partition
		s.pending = nil
	}
	if s.output.Rows() == 0 {
		return io.EOF
	}
	out.Columns = s.columns
	return s.output.Publish(out)
}

func (s *sharedPartitionCursor) Close() error {
	s.pending, s.columns = nil, nil
	return s.sharedCursor.Close()
}
