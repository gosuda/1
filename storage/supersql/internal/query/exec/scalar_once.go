package exec

import (
	"context"
	"errors"
	"io"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exapply "github.com/gosuda/supersql-alpha/internal/query/exec/apply"
)

// Scalar cardinality needs at most two rows. The producer stores that bounded
// prefix on demand; readers never reopen the inner plan for later outer rows.
type scalarOnceFactory struct {
	producer *sharedProducer
	reader   sharedCursor
	env      *Environment
	layouts  []model.TypeLayout
}

func scalarOnceBytes(layouts []model.TypeLayout) uint64 {
	return uint64(unsafe.Sizeof(scalarOnceFactory{})) + uint64(unsafe.Sizeof(deferredScalarCursor{})) + uint64(unsafe.Sizeof(limitCursor{})) + uint64(unsafe.Sizeof(sharedProducer{})) + uint64(len(layouts))*2*uint64(unsafe.Sizeof(columnar.Vector{})) + 2048
}

func newScalarOnce(factory exapply.RightFactory, layouts []model.TypeLayout, env *Environment) *scalarOnceFactory {
	// The wrapping limit cursor appends body-width rows through the owner's
	// shared scratch row, which build sized for the owner's own plan width. A
	// row subquery body is one column per compared field, so widen the scratch
	// row here instead of slicing past its capacity.
	if cap(env.row) < len(layouts) {
		env.row = make([]model.Value, len(layouts))
	}
	input := &deferredScalarCursor{factory: factory}
	limited := &limitCursor{input: input, env: env, layouts: layouts, remaining: 2, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))}}
	producer := &sharedProducer{input: limited, env: env, layouts: layouts, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))}}
	return &scalarOnceFactory{producer: producer, env: env, layouts: layouts}
}

func (f *scalarOnceFactory) Open(ctx context.Context, _ []model.Value) (storage.BatchCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f.reader.producer != nil && !f.reader.closed {
		return nil, ErrInvalid
	}
	f.reader = sharedCursor{producer: f.producer, env: f.env, layouts: f.layouts}
	return &f.reader, nil
}

type deferredScalarCursor struct {
	factory exapply.RightFactory
	input   storage.BatchCursor
	closed  bool
	err     error
}

func (c *deferredScalarCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if c.closed {
		return io.EOF
	}
	if c.input == nil {
		input, err := c.factory.Open(ctx, nil)
		c.input = input
		if err != nil {
			return errors.Join(err, c.Close())
		}
		if input == nil {
			return errors.Join(ErrInvalid, c.Close())
		}
	}
	return c.input.Next(ctx, out)
}

func (c *deferredScalarCursor) Close() error {
	if c.closed {
		return c.err
	}
	c.closed = true
	if c.input != nil {
		c.err = c.input.Close()
		c.input = nil
	}
	c.factory = nil
	return c.err
}
