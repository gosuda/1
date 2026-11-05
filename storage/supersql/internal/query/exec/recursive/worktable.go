package recursive

import (
	"context"
	"errors"
	"io"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type worktable struct {
	ctx        context.Context
	spool      *rowstore.Spool
	options    *Options
	position   rowstore.Position
	readers    *workReader
	single     bool
	closeSpool bool
	closed     bool
}

func (w *worktable) Open(ctx context.Context) (storage.BatchCursor, error) {
	if w.closed {
		return nil, runtime.ErrClosed
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(w.ctx)); err != nil {
		return nil, err
	}
	claim, err := w.options.Query.TryReserve(runtime.WorkMemory, int64(unsafe.Sizeof(workReader{}))+128)
	if err != nil {
		return nil, err
	}
	r := &workReader{owner: w, claim: claim}
	r.reader, err = w.spool.Reader(ctx)
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	r.builder, err = columnar.NewBuilder(w.options.Layouts, columnar.BuilderOptions{MaxRows: w.options.BatchRows, MaxBytes: w.options.BatchBytes, Allocator: w.options.Query.Allocator(runtime.OutputMemory)})
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	r.next = w.readers
	if r.next != nil {
		r.next.previous = r
	}
	w.readers = r
	return r, nil
}

func (w *worktable) close() error {
	w.closed = true
	var err error
	// Fence even readers omitted by a failed step constructor or Close. Spool
	// Close waits for readers, so they must be released before the extent.
	for w.readers != nil {
		err = errors.Join(err, w.readers.Close())
	}
	if w.closeSpool {
		err = errors.Join(err, w.spool.Close())
	}
	w.spool, w.options = nil, nil
	return err
}

type workReader struct {
	owner          *worktable
	previous, next *workReader
	reader         *rowstore.Reader
	builder        *columnar.Builder
	claim          *runtime.Reservation
	pending        []model.Value
	havePending, singleRead, closed, eof bool
	closeErr       error
}

func (r *workReader) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, r.Close())
	}
	out.Reset()
	if r.closed {
		if r.eof {
			return io.EOF
		}
		return runtime.ErrClosed
	}
	defer func() {
		if err != nil {
			r.eof = err == io.EOF
			if cleanup := r.Close(); cleanup != nil {
				err = errors.Join(err, cleanup)
			}
		}
	}()
	if r.eof {
		return io.EOF
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(r.owner.ctx)); err != nil {
		return err
	}
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.owner.ctx, cancel)
	defer func() { stop(); cancel() }()
	limit := r.owner.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	r.builder.Reset()
	count := 0
	for count < limit {
		row := r.pending
		if !r.havePending {
			if r.owner.single {
				if r.singleRead {
					if count == 0 {
						return io.EOF
					}
					r.eof = true
					break
				}
				row, err = r.reader.Read(call, r.owner.position)
			} else {
				_, row, err = r.reader.NextRow(call)
			}
			if err != nil {
				if err != io.EOF || count == 0 {
					return err
				}
				r.eof = true
				break
			}
		}
		if err := r.builder.Append(call, row); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && count > 0 {
				r.pending, r.havePending = row, true
				break
			}
			return err
		}
		r.pending, r.havePending = nil, false
		if r.owner.single {
			r.singleRead = true
		}
		count++
	}
	return r.builder.Publish(out)
}

func (r *workReader) Close() error {
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	if r.previous != nil {
		r.previous.next = r.next
	} else if r.owner.readers == r {
		r.owner.readers = r.next
	}
	if r.next != nil {
		r.next.previous = r.previous
	}
	r.previous, r.next = nil, nil
	r.pending = nil
	if r.reader != nil {
		r.closeErr = r.reader.Close()
		r.reader = nil
	}
	if r.builder != nil {
		r.closeErr = errors.Join(r.closeErr, r.builder.Close())
		r.builder = nil
	}
	r.claim.Release()
	return r.closeErr
}
