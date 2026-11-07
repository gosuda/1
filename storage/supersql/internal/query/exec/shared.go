package exec

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

// sharedProducer evaluates each source batch once. Consumers pin independent
// prefixes; portal cleanup closes them before closing the producer's spool.
type sharedProducer struct {
	mu           sync.Mutex
	input        storage.BatchCursor
	env          *Environment
	layouts      []model.TypeLayout
	spoolLayouts []model.TypeLayout
	partitions   uint32
	ordered      bool
	source       columnar.Batch
	spool        *rowstore.Spool
	done, closed bool
	err          error
	closeErr     error
}

// complete drains mandatory producers without borrowing a consumer or growing
// a row slice. Zero-column writers have no referenceable result to retain.
func (s *sharedProducer) complete(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.err != nil || s.done {
		return s.err
	}
	if len(s.layouts) != 0 && s.spool == nil {
		layouts := s.layouts
		if s.spoolLayouts != nil {
			layouts = s.spoolLayouts
		}
		s.spool, s.err = rowstore.New(ctx, s.env.Query, layouts, s.env.Spool)
		if s.err != nil {
			return s.err
		}
	}
	for !s.done {
		if s.err = s.fill(ctx, s.env.Limits.BatchRows); s.err != nil {
			return s.err
		}
	}
	return nil
}

func (s *sharedProducer) reader(ctx context.Context, position rowstore.Position, maximum int) (*rowstore.Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	if s.err != nil {
		return nil, s.err
	}
	if s.spool == nil {
		layouts := s.layouts
		if s.spoolLayouts != nil {
			layouts = s.spoolLayouts
		}
		s.spool, s.err = rowstore.New(ctx, s.env.Query, layouts, s.env.Spool)
		if s.err != nil {
			return nil, s.err
		}
	}
	for uint64(position) == s.spool.Bytes() && !s.done {
		s.err = s.fill(ctx, maximum)
		if s.err != nil {
			return nil, s.err
		}
	}
	if s.done && uint64(position) == s.spool.Bytes() {
		return nil, io.EOF
	}
	reader, err := s.spool.PrefixReader(ctx)
	if err != nil {
		return nil, err
	}
	if err = reader.Seek(ctx, position); err != nil {
		return nil, errors.Join(err, reader.Close())
	}
	return reader, nil
}
func (s *sharedProducer) fill(ctx context.Context, maximum int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.source.MaxRows = min(maximum, s.env.Limits.BatchRows)
	defer s.source.Reset()
	err := s.input.Next(ctx, &s.source)
	if err == io.EOF {
		input := s.input
		s.input = nil
		if s.closeErr = errors.Join(s.closeErr, input.Close()); s.closeErr != nil {
			return s.closeErr
		}
		if s.spool != nil {
			if err = s.spool.Seal(ctx); err != nil {
				return err
			}
		}
		s.done = true
		return nil
	}
	if err != nil {
		return err
	}
	if s.spool != nil {
		_, err = s.spool.AppendBatch(ctx, &s.source)
	}
	return err
}
func (s *sharedProducer) closeInput() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeInputLocked()
}

func (s *sharedProducer) closeInputLocked() error {
	s.source.Reset()
	if s.input != nil {
		s.closeErr = errors.Join(s.closeErr, s.input.Close())
		s.input = nil
	}
	return s.closeErr
}

func (s *sharedProducer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.closeInputLocked()
	s.layouts = nil
	s.spoolLayouts = nil
	if s.spool != nil {
		s.closeErr = errors.Join(s.closeErr, s.spool.Close())
		s.spool = nil
	}
	s.env = nil
	return s.closeErr
}

type sharedCursor struct {
	producer      *sharedProducer
	env           *Environment
	layouts       []model.TypeLayout
	reader        *rowstore.Reader
	position      rowstore.Position
	output        *columnar.Builder
	ended, closed bool
	closeErr      error
}

func (s *sharedCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if s.closed || s.ended {
		return io.EOF
	}
	if s.output != nil {
		if err := s.output.Close(); err != nil {
			return err
		}
		s.output = nil
	}
	var err error
	s.output, err = builder(s.env, s.layouts)
	if err != nil {
		return err
	}
	maximum := batchRows(out, s.env)
	for s.output.Rows() < maximum {
		row, err := s.nextRow(ctx, maximum-s.output.Rows())
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err = s.output.Append(ctx, row); err != nil {
			return err
		}
	}
	if s.output.Rows() == 0 && s.ended {
		return io.EOF
	}
	return s.output.Publish(out)
}

func (s *sharedCursor) nextRow(ctx context.Context, maximum int) ([]model.Value, error) {
	for !s.ended {
		var err error
		if s.reader == nil {
			s.reader, err = s.producer.reader(ctx, s.position, maximum)
			if err == io.EOF {
				s.ended = true
				return nil, io.EOF
			}
			if err != nil {
				return nil, err
			}
		}
		position, row, err := s.reader.NextRow(ctx)
		if err != io.EOF {
			return row, err
		}
		s.position = position
		s.closeErr = errors.Join(s.closeErr, s.reader.Close())
		s.reader = nil
		if s.closeErr != nil {
			return nil, s.closeErr
		}
	}
	return nil, io.EOF
}

func (s *sharedCursor) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.reader != nil {
		s.closeErr = errors.Join(s.closeErr, s.reader.Close())
		s.reader = nil
	}
	if s.output != nil {
		s.closeErr = errors.Join(s.closeErr, s.output.Close())
		s.output = nil
	}
	s.layouts = nil
	s.env = nil
	s.producer = nil
	return s.closeErr
}

func validSpoolLimits(l rowstore.Limits) bool {
	validBytes := l.MaxBytes > 0 && l.MaxRecordBytes > 0 && l.MetadataBytes > 0 && l.DecodeBytes > 0
	return validBytes && l.MaxRows > 0 && l.MaxReaders > 0
}

// sharedProducer owns input on every outcome.
func (c *invocation) sharedProducer(input storage.BatchCursor, layouts []model.TypeLayout) (*sharedProducer, error) {
	var producer *sharedProducer
	if tagged, ok := input.(partitionedCursor); ok {
		var err error
		producer, err = c.sharedPartitionProducer(tagged, layouts)
		if err != nil {
			return nil, errors.Join(err, input.Close())
		}
	} else {
		producer = &sharedProducer{input: input, env: &c.portal.env, layouts: layouts, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))}}
	}
	c.portal.producers = append(c.portal.producers, producer)
	return producer, nil
}

func (c *invocation) sharedReader(producer *sharedProducer, layouts []model.TypeLayout) (storage.BatchCursor, error) {
	if producer.partitions != 0 {
		return c.sharedPartitionReader(producer, layouts)
	}
	if err := c.charge(2048); err != nil {
		return nil, err
	}
	reader := &sharedCursor{producer: producer, env: &c.portal.env, layouts: layouts}
	c.portal.owned = append(c.portal.owned, reader)
	return reader, nil
}
