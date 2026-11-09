package sort

import (
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

// Each tape holds header, sorted rows, header, sorted rows, ... . Header rows
// have ordinal -1 and a 16-byte key containing little-endian row and byte counts.
// The byte count excludes the header. Fixed-width headers allow skipping runs
// without either scanning their rows or retaining a scan-sized position index.
type run struct {
	position rowstore.Position
	rows     uint64
	bytes    uint64
}

type mergeHead struct {
	reader    *rowstore.Reader
	row       []model.Value
	remaining uint64
	end       rowstore.Position
}

type headHeap []mergeHead

func (h headHeap) Len() int { return len(h) }
func (h headHeap) Less(i, j int) bool {
	if n := bytes.Compare(h[i].row[0].BytesVal, h[j].row[0].BytesVal); n != 0 {
		return n < 0
	}
	return h[i].row[1].IntVal < h[j].row[1].IntVal
}
func (h headHeap) Swap(i, j int)   { h[i], h[j] = h[j], h[i] }
func (h *headHeap) Push(value any) { *h = append(*h, value.(mergeHead)) }
func (h *headHeap) Pop() any {
	last := len(*h) - 1
	value := (*h)[last]
	(*h)[last] = mergeHead{}
	*h = (*h)[:last]
	return value
}

type merger struct {
	heads   headHeap
	readers []*rowstore.Reader
	advance bool
}

func (c *cursor) appendHeader(ctx context.Context, tape *rowstore.Spool, rows, size uint64) error {
	binary.LittleEndian.PutUint64(c.headerData[:8], rows)
	binary.LittleEndian.PutUint64(c.headerData[8:], size)
	c.header[0] = model.BytesVal(c.headerData[:])
	c.header[1] = model.Int64Val(-1)
	before := tape.Bytes()
	if _, err := tape.AppendRow(ctx, c.header); err != nil {
		return err
	}
	c.headerBytes = tape.Bytes() - before
	return nil
}

// loadRuns uses one temporary reader, closed before opening merge readers.
func (c *cursor) loadRuns(ctx context.Context, position rowstore.Position, count int) (_ rowstore.Position, err error) {
	if count > cap(c.runs) {
		return 0, resource("sort run descriptor limit exceeded", nil)
	}
	r, err := c.tape.Reader(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, r.Close()) }()
	c.runs = c.runs[:0]
	limit := c.tape.Bytes()
	for range count {
		row, err := r.Read(ctx, position)
		if err != nil {
			return 0, err
		}
		if row[1].IsNull || row[1].IntVal != -1 || row[0].IsNull || len(row[0].BytesVal) != 16 || c.headerBytes > limit-uint64(position) {
			return 0, rowstore.ErrCorrupt
		}
		next := uint64(position) + c.headerBytes
		run := run{position: rowstore.Position(next), rows: binary.LittleEndian.Uint64(row[0].BytesVal[:8]), bytes: binary.LittleEndian.Uint64(row[0].BytesVal[8:])}
		if run.rows == 0 || run.bytes == 0 || run.bytes > limit-next || run.rows > run.bytes {
			return 0, rowstore.ErrCorrupt
		}
		c.runs = append(c.runs, run)
		position = rowstore.Position(next + run.bytes)
	}
	return position, nil
}

func (c *cursor) openMerge(ctx context.Context, runs []run) (_ *merger, err error) {
	if len(runs) > c.options.MergeFanIn {
		return nil, resource("sort merge fan-in exceeded", nil)
	}
	m := &merger{heads: make(headHeap, 0, len(runs)), readers: make([]*rowstore.Reader, 0, len(runs))}
	defer func() {
		if err != nil {
			err = errors.Join(err, m.close())
		}
	}()
	for _, run := range runs {
		r, err := c.tape.Reader(ctx)
		if err != nil {
			return nil, err
		}
		m.readers = append(m.readers, r)
		if err := r.Seek(ctx, run.position); err != nil {
			return nil, err
		}
		h := mergeHead{reader: r, remaining: run.rows, end: run.position + rowstore.Position(run.bytes)}
		if err := h.next(ctx); err != nil {
			return nil, err
		}
		m.heads = append(m.heads, h)
	}
	heap.Init(&m.heads)
	return m, nil
}

func (h *mergeHead) next(ctx context.Context) error {
	position, row, err := h.reader.NextRow(ctx)
	if err != nil {
		return errors.Join(rowstore.ErrCorrupt, err)
	}
	if position >= h.end || row[1].IsNull || row[1].IntVal < 0 || row[0].IsNull {
		return rowstore.ErrCorrupt
	}
	h.row = row
	h.remaining--
	return nil
}

func (m *merger) next(ctx context.Context) ([]model.Value, error) {
	// The head stays borrowed until the consumer asks for its successor.
	if m.advance {
		if m.heads[0].remaining == 0 {
			heap.Pop(&m.heads)
		} else {
			if err := m.heads[0].next(ctx); err != nil {
				return nil, err
			}
			heap.Fix(&m.heads, 0)
		}
		m.advance = false
	}
	if len(m.heads) == 0 {
		return nil, io.EOF
	}
	m.advance = true
	return m.heads[0].row, nil
}

func (m *merger) close() error {
	var err error
	m.heads = nil
	for _, reader := range m.readers {
		err = errors.Join(err, reader.Close())
	}
	m.readers = nil
	return err
}

func (c *cursor) mergeRuns(ctx context.Context, out *rowstore.Spool, runs []run) (err error) {
	var rows, size uint64
	for _, run := range runs {
		rows += run.rows
		size += run.bytes
	}
	if err := c.appendHeader(ctx, out, rows, size); err != nil {
		return err
	}
	m, err := c.openMerge(ctx, runs)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, m.close()) }()
	for {
		if err := c.check(ctx); err != nil {
			return err
		}
		row, err := m.next(ctx)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := out.AppendRow(ctx, row); err != nil {
			return err
		}
	}
}

// compact completes a whole pass before replacing its source tape. Every row
// transfers once per pass; even a final singleton group advances to the next
// generation instead of folding a growing prefix into newly generated runs.
func (c *cursor) compact(ctx context.Context) (err error) {
	out, err := c.newSpool(ctx, false)
	if err != nil {
		return err
	}
	defer func() {
		if out != nil {
			err = errors.Join(err, out.Close())
		}
	}()
	var position rowstore.Position
	var outputRuns uint64
	// Full windows end on group boundaries, independently of MaxRuns divisibility.
	window := c.options.MaxRuns / c.options.MergeFanIn * c.options.MergeFanIn
	for remaining := c.runCount; remaining > 0; {
		count := int(min(uint64(window), remaining))
		position, err = c.loadRuns(ctx, position, count)
		if err != nil {
			return err
		}
		for start := 0; start < len(c.runs); start += c.options.MergeFanIn {
			end := min(start+c.options.MergeFanIn, len(c.runs))
			if err := c.mergeRuns(ctx, out, c.runs[start:end]); err != nil {
				return err
			}
			outputRuns++
		}
		remaining -= uint64(count)
	}
	if err := out.Seal(ctx); err != nil {
		return err
	}
	if err := c.tape.Close(); err != nil {
		return err
	}
	c.tape, out = out, nil
	c.runCount = outputRuns
	return nil
}
