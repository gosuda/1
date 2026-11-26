package groupingset

import (
	"context"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type replayCursor struct {
	ctx              context.Context
	reader           *rowstore.Reader
	builder          *columnar.Builder
	pending          []model.Value
	partitioned      *partitionedState
	encoder          storage.KeyEncoder
	maxRecordBytes   uint64
	pendingPartition uint32
	published        uint32
	rows             int
	haveRow          bool
	eof              bool
	closed           bool
	closeErr         error
}

func (r *replayCursor) reconstructPartition(ctx context.Context) error {
	s := r.partitioned
	defer clear(s.values)
	for i, column := range s.columns {
		s.values[i] = r.pending[column]
	}
	partition, err := s.encodePartition(ctx, r.encoder, r.maxRecordBytes)
	if err != nil {
		return err
	}
	r.pendingPartition = partition
	return nil
}

func (r *replayCursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if r.closed {
		if r.eof {
			return io.EOF
		}
		return runtime.ErrClosed
	}
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, r.Close())
	}
	out.Reset()
	defer func() {
		if err != nil {
			if cleanup := r.Close(); cleanup != nil {
				err = errors.Join(err, cleanup)
			}
		}
	}()
	if err := errors.Join(context.Cause(ctx), context.Cause(r.ctx)); err != nil {
		return err
	}
	if r.eof {
		return io.EOF
	}
	limit := r.rows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	r.builder.Reset()
	var partition uint32
	for r.builder.Rows() < limit {
		if !r.haveRow {
			_, r.pending, err = r.reader.NextRow(ctx)
			if err == io.EOF {
				r.eof = true
				if r.builder.Rows() == 0 {
					return io.EOF
				}
				break
			}
			if err != nil {
				return err
			}
			r.haveRow = true
			if r.partitioned != nil {
				if err := r.reconstructPartition(ctx); err != nil {
					return err
				}
			}
		}
		if r.partitioned != nil {
			if r.builder.Rows() > 0 && partition != r.pendingPartition {
				break
			}
			partition = r.pendingPartition
		}
		if err := r.builder.Append(ctx, r.pending); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && r.builder.Rows() > 0 {
				break
			}
			return err
		}
		r.pending, r.haveRow = nil, false
	}
	if err := r.builder.Publish(out); err != nil {
		return err
	}
	r.published = partition
	return nil
}

func (r *replayCursor) Close() error {
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	r.pending = nil
	r.partitioned, r.encoder = nil, nil
	if r.reader != nil {
		r.closeErr = r.reader.Close()
		r.reader = nil
	}
	if r.builder != nil {
		r.closeErr = errors.Join(r.closeErr, r.builder.Close())
		r.builder = nil
	}
	r.ctx = nil
	return r.closeErr
}
