package exec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

type limitCursor struct {
	input             storage.BatchCursor
	env               *Environment
	layouts           []model.TypeLayout
	offset, remaining uint64
	withTies          bool
	keys              []storage.KeyColumn
	keyColumns        []int
	boundary          []byte
	boundaryClaim     columnar.Allocation
	source            columnar.Batch
	output            *columnar.Builder
	ended             bool
	closed            bool
	closeErr          error
}

func (l *limitCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if l.closed || l.ended {
		return io.EOF
	}
	if l.output != nil {
		if err := l.output.Close(); err != nil {
			return err
		}
		l.output = nil
	}
	if l.remaining == 0 && l.boundary == nil {
		l.ended = true
		return eofAfter(l.input.Close())
	}
	maxRows := batchRows(out, l.env)
	l.source.MaxRows = maxRows
	if !l.withTies && l.offset <= math.MaxUint64-l.remaining && l.offset+l.remaining < uint64(maxRows) {
		l.source.MaxRows = int(l.offset + l.remaining)
	}
	err := l.input.Next(ctx, &l.source)
	if err != nil {
		if err == io.EOF {
			l.ended = true
			return eofAfter(l.input.Close())
		}
		return err
	}
	l.output, err = builder(l.env, l.layouts)
	if err != nil {
		return err
	}
	for row := range l.source.Rows() {
		if err = ctx.Err(); err != nil {
			return err
		}
		if l.offset > 0 {
			l.offset--
			continue
		}
		if l.remaining == 0 {
			if !l.withTies {
				l.ended = true
				break
			}
			key, claim, keyErr := l.key(ctx, row)
			if keyErr != nil {
				return keyErr
			}
			equal := bytes.Equal(key, l.boundary)
			key = nil
			claim.Release()
			if !equal {
				l.ended = true
				break
			}
		} else {
			l.remaining--
			if l.remaining == 0 && l.withTies {
				l.boundary, l.boundaryClaim, err = l.key(ctx, row)
				if err != nil {
					return err
				}
			}
		}
		if err = appendLane(ctx, l.env, l.output, &l.source, row); err != nil {
			return err
		}
	}
	if l.remaining == 0 && !l.withTies {
		l.ended = true
	}
	// A terminal prefix is owned before closing the source, so storage pins can be
	// released now rather than being retained until the consumer's next call.
	if l.ended {
		if err = l.input.Close(); err != nil {
			return err
		}
		l.source.Reset()
	}
	return l.output.Publish(out)
}
func eofAfter(err error) error {
	if err != nil {
		return err
	}
	return io.EOF
}
func (l *limitCursor) key(ctx context.Context, row int) ([]byte, columnar.Allocation, error) {
	if l.env.Limits.BatchBytes > math.MaxUint64/2 {
		return nil, nil, columnar.ErrBatchBudget
	}
	claim, err := l.env.Query.Allocator(runtime.WorkMemory).Reserve(l.env.Limits.BatchBytes * 2)
	if err != nil {
		return nil, nil, err
	}
	values := l.env.row[:len(l.keys)]
	remaining := l.env.Limits.BatchBytes
	defer clear(values)
	for i, col := range l.keyColumns {
		value, size, copyErr := columnar.CopyValue(ctx, &l.source.Columns[col], l.source.Lane(row), remaining)
		if copyErr != nil {
			claim.Release()
			return nil, nil, copyErr
		}
		remaining -= size
		values[i] = value
	}
	key, err := l.env.Keys.EncodePrefix(ctx, l.keys, values, l.env.Limits.BatchBytes)
	if err != nil {
		claim.Release()
		return nil, nil, err
	}
	clear(values)
	if err = claim.Resize(uint64(cap(key))); err != nil {
		claim.Release()
		return nil, nil, err
	}
	return key, claim, nil
}
func (l *limitCursor) Close() error {
	if l.closed {
		return l.closeErr
	}
	l.closed = true
	l.closeErr = l.input.Close()
	l.source.Reset()
	if l.output != nil {
		l.closeErr = errors.Join(l.closeErr, l.output.Close())
		l.output = nil
	}
	l.boundary = nil
	if l.boundaryClaim != nil {
		l.boundaryClaim.Release()
		l.boundaryClaim = nil
	}
	return l.closeErr
}

type materializeCursor struct {
	input    storage.BatchCursor
	env      *Environment
	layouts  []model.TypeLayout
	spool    *rowstore.Spool
	reader   *rowstore.Reader
	source   columnar.Batch
	output   *columnar.Builder
	ready    bool
	ended    bool
	closed   bool
	closeErr error
}

func (m *materializeCursor) fill(ctx context.Context) error {
	var err error
	m.spool, err = rowstore.New(ctx, m.env.Query, m.layouts, m.env.Spool)
	if err != nil {
		return err
	}
	m.source.MaxRows = m.env.Limits.BatchRows
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = m.input.Next(ctx, &m.source)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if _, err = m.spool.AppendBatch(ctx, &m.source); err != nil {
			return err
		}
	}
	if err = m.input.Close(); err != nil {
		return err
	}
	m.source.Reset()
	if err = m.spool.Seal(ctx); err != nil {
		return err
	}
	m.reader, err = m.spool.Reader(ctx)
	if err != nil {
		return err
	}
	m.ready = true
	return nil
}
func (m *materializeCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if m.closed || m.ended {
		return io.EOF
	}
	if !m.ready {
		if err := m.fill(ctx); err != nil {
			return err
		}
	}
	if m.output != nil {
		if err := m.output.Close(); err != nil {
			return err
		}
		m.output = nil
	}
	var err error
	m.output, err = builder(m.env, m.layouts)
	if err != nil {
		return err
	}
	maxRows := batchRows(out, m.env)
	for m.output.Rows() < maxRows {
		_, row, err := m.reader.NextRow(ctx)
		if err == io.EOF {
			m.ended = true
			break
		}
		if err != nil {
			return err
		}
		if err = m.output.Append(ctx, row); err != nil {
			return err
		}
	}
	if m.output.Rows() == 0 && m.ended {
		return io.EOF
	}
	return m.output.Publish(out)
}
func (m *materializeCursor) Close() error {
	if m.closed {
		return m.closeErr
	}
	m.closed = true
	m.closeErr = m.input.Close()
	m.source.Reset()
	if m.output != nil {
		m.closeErr = errors.Join(m.closeErr, m.output.Close())
		m.output = nil
	}
	if m.reader != nil {
		m.closeErr = errors.Join(m.closeErr, m.reader.Close())
		m.reader = nil
	}
	if m.spool != nil {
		m.closeErr = errors.Join(m.closeErr, m.spool.Close())
		m.spool = nil
	}
	return m.closeErr
}
