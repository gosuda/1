package exec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) topK(input storage.BatchCursor, node physical.NodeSpec, slots []physical.Slot, layouts []model.TypeLayout, offset, count uint64) (storage.BatchCursor, error) {
	keys, keyColumns, err := c.orderColumns(node.Delivered.Ordering, slots, layouts)
	if err != nil {
		return nil, err
	}
	if c.preparing {
		return nil, nil
	}
	env := &c.portal.env
	// If the child already delivers the required ordering, stream directly without sorting.
	if len(node.Children) == 1 && c.plan != nil {
		if child, ok := c.plan.Node(node.Children[0]); ok && partitionSortOrder(child.Delivered(), node.Delivered.Ordering) {
			cursor := &limitCursor{
				input:     input,
				env:       env,
				layouts:   layouts,
				offset:    offset,
				remaining: count,
				withTies:  node.Limit.WithTies,
				source:    columnar.Batch{Columns: make([]columnar.Vector, 0, len(slots))},
			}
			if node.Limit.WithTies {
				cursor.keys = keys
				cursor.keyColumns = keyColumns
			}
			return cursor, nil
		}
	}
	if count == 0 {
		return &limitCursor{
			input:     input,
			env:       env,
			layouts:   layouts,
			offset:    offset,
			remaining: 0,
			withTies:  node.Limit.WithTies,
			source:    columnar.Batch{Columns: make([]columnar.Vector, 0, len(slots))},
		}, nil
	}
	sortOptions := env.Sort
	sortOptions.Query = env.Query
	sortOptions.Encoder = env.Keys
	sortOptions.Layouts = layouts
	sortOptions.BatchRows = env.Limits.BatchRows
	sortOptions.BatchBytes = env.Limits.BatchBytes
	sortOptions.Keys = make([]exsort.Key, len(keys))
	for i, key := range keys {
		sortOptions.Keys[i] = exsort.Key{Column: keyColumns[i], Descending: key.Descending, NullsFirst: key.NullsFirst}
	}

	heapBudget := topKHeapBudget(sortOptions)
	minEntryBytes := uint64(unsafe.Sizeof(topKEntry{})) + uint64(len(layouts))*uint64(unsafe.Sizeof(model.Value{})) + 16
	if count == math.MaxUint64 || offset > math.MaxUint64-count || offset+count > uint64(math.MaxInt)/2 || heapBudget < minEntryBytes || offset+count > heapBudget/minEntryBytes {
		sorted, err := exsort.New(c.portal.ctx, input, sortOptions)
		if err != nil {
			return nil, err
		}
		cursor := &limitCursor{
			input:     sorted,
			env:       env,
			layouts:   layouts,
			offset:    offset,
			remaining: count,
			withTies:  node.Limit.WithTies,
			source:    columnar.Batch{Columns: make([]columnar.Vector, 0, len(slots))},
		}
		if node.Limit.WithTies {
			cursor.keys = keys
			cursor.keyColumns = keyColumns
		}
		return cursor, nil
	}
	return newTopKCursor(c.portal.ctx, input, env, layouts, keys, keyColumns, offset, count, node.Limit.WithTies, sortOptions, heapBudget)
}

func topKHeapBudget(o exsort.Options) uint64 {
	return min(o.RunBytes, o.WorkBytes/2)
}

type topKEntry struct {
	key     []byte
	row     []model.Value
	ordinal int64
	bytes   uint64
}

func topKWorse(a, b topKEntry) bool {
	if n := bytes.Compare(a.key, b.key); n != 0 {
		return n > 0
	}
	return a.ordinal > b.ordinal
}

func topKSiftUp(h []topKEntry, i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !topKWorse(h[i], h[parent]) {
			break
		}
		h[i], h[parent] = h[parent], h[i]
		i = parent
	}
}

func topKSiftDown(h []topKEntry, i int) {
	n := len(h)
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		worst := left
		if right := left + 1; right < n && topKWorse(h[right], h[left]) {
			worst = right
		}
		if !topKWorse(h[worst], h[i]) {
			break
		}
		h[i], h[worst] = h[worst], h[i]
		i = worst
	}
}

// topKCursor maintains a bounded max-heap of size K = offset + count (plus
// boundary peers when WITH TIES is active), avoiding a full sort of all N rows.
// If buffered rows or ties exceed the memory budget mid-stream, it seamlessly
// transitions to external sort via topKSpliceCursor.
type topKCursor struct {
	input       storage.BatchCursor
	env         *Environment
	layouts     []model.TypeLayout
	keys        []storage.KeyColumn
	keyColumns  []int
	keyValues   []model.Value
	offset      uint64
	count       uint64
	k           int
	withTies    bool
	sortOptions exsort.Options
	heapBudget  uint64
	maxKeyBytes uint64

	claim     columnar.Allocation
	usedBytes uint64
	tiesBytes uint64
	heap      []topKEntry
	ties      []topKEntry
	results   []topKEntry
	pos       int

	source   columnar.Batch
	output   *columnar.Builder
	fallback storage.BatchCursor

	initialized bool
	fellBack    bool
	ended       bool
	closed      bool
	closeErr    error
}

func newTopKCursor(
	ctx context.Context,
	input storage.BatchCursor,
	env *Environment,
	layouts []model.TypeLayout,
	keys []storage.KeyColumn,
	keyColumns []int,
	offset, count uint64,
	withTies bool,
	sortOptions exsort.Options,
	heapBudget uint64,
) (*topKCursor, error) {
	maxKeyBytes := min(sortOptions.MaxRecordBytes, sortOptions.RunBytes)
	if _, err := env.Keys.EncodePrefix(ctx, keys, nil, maxKeyBytes); err != nil {
		return nil, errors.Join(err, input.Close())
	}
	return &topKCursor{
		input:       input,
		env:         env,
		layouts:     layouts,
		keys:        keys,
		keyColumns:  keyColumns,
		keyValues:   make([]model.Value, len(keys)),
		offset:      offset,
		count:       count,
		k:           int(offset + count),
		withTies:    withTies,
		sortOptions: sortOptions,
		heapBudget:  heapBudget,
		maxKeyBytes: maxKeyBytes,
		source:      columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))},
	}, nil
}

func (t *topKCursor) initialize(ctx context.Context) error {
	var err error
	t.claim, err = t.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return err
	}
	var ordinal int64
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		t.source.MaxRows = t.env.Limits.BatchRows
		err = t.input.Next(ctx, &t.source)
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err = t.source.Validate(max(t.source.Length, t.env.Limits.BatchRows)); err != nil {
			return err
		}
		if t.source.Rows() > t.env.Limits.BatchRows {
			return columnar.ErrInvalidBatch
		}
		if t.source.Rows() == 0 {
			t.source.Reset()
			continue
		}
		if len(t.source.Columns) != len(t.layouts) {
			return columnar.ErrValueLayout
		}
		for i := range t.source.Columns {
			if !t.source.Columns[i].MatchesLayout(t.layouts[i]) {
				return columnar.ErrValueLayout
			}
		}
		for rowIdx := range t.source.Rows() {
			if err = ctx.Err(); err != nil {
				return err
			}
			if ordinal == math.MaxInt64 {
				return &model.Error{Code: "53200", Message: "top-K input ordinal exhausted", Cause: runtime.ErrBudget}
			}
			lane := t.source.Lane(rowIdx)
			keyRemaining := t.sortOptions.MaxRecordBytes * 8
			for i, col := range t.keyColumns {
				v, used, copyErr := columnar.CopyValue(ctx, &t.source.Columns[col], lane, keyRemaining)
				if copyErr != nil {
					clear(t.keyValues)
					return copyErr
				}
				keyRemaining -= used
				t.keyValues[i] = v
			}
			encoded, encErr := t.env.Keys.EncodePrefix(ctx, t.keys, t.keyValues, t.maxKeyBytes)
			clear(t.keyValues)
			if encErr != nil {
				if errors.Is(encErr, columnar.ErrBatchBudget) {
					return &model.Error{Code: "53200", Message: "encode top-K key", Cause: errors.Join(runtime.ErrBudget, encErr)}
				}
				return encErr
			}

			cmp := -1
			if len(t.heap) == t.k {
				cmp = bytes.Compare(encoded, t.heap[0].key)
				if cmp > 0 || (cmp == 0 && !t.withTies) {
					ordinal++
					continue
				}
			}

			row := make([]model.Value, len(t.layouts))
			var copied uint64
			remaining := t.sortOptions.MaxRecordBytes * 8
			for col := range t.layouts {
				v, used, copyErr := columnar.CopyValue(ctx, &t.source.Columns[col], lane, remaining)
				if copyErr != nil {
					return copyErr
				}
				remaining -= used
				copied += used
				row[col] = v
			}
			entryBytes := uint64(unsafe.Sizeof(topKEntry{})) + uint64(cap(encoded)) + uint64(len(row))*uint64(unsafe.Sizeof(model.Value{})) + copied

			peak := t.usedBytes + entryBytes
			needFallback := peak > t.heapBudget
			if !needFallback {
				if resizeErr := t.claim.Resize(peak); errors.Is(resizeErr, runtime.ErrBudget) {
					needFallback = true
				} else if resizeErr != nil {
					return resizeErr
				}
			}
			if needFallback {
				return t.fallbackToSort(ctx, rowIdx)
			}

			item := topKEntry{key: encoded, row: row, ordinal: ordinal, bytes: entryBytes}
			if len(t.heap) < t.k {
				t.heap = append(t.heap, item)
				topKSiftUp(t.heap, len(t.heap)-1)
				t.usedBytes += entryBytes
			} else if cmp == 0 {
				t.ties = append(t.ties, item)
				t.tiesBytes += entryBytes
				t.usedBytes += entryBytes
			} else {
				evicted := t.heap[0]
				t.heap[0] = item
				topKSiftDown(t.heap, 0)
				t.usedBytes += entryBytes
				if t.withTies && bytes.Equal(t.heap[0].key, evicted.key) {
					t.ties = append(t.ties, evicted)
					t.tiesBytes += evicted.bytes
				} else {
					t.usedBytes -= evicted.bytes
					if len(t.ties) > 0 {
						clear(t.ties)
						t.ties = t.ties[:0]
						t.usedBytes -= t.tiesBytes
						t.tiesBytes = 0
					}
				}
				if resizeErr := t.claim.Resize(t.usedBytes); resizeErr != nil {
					return resizeErr
				}
			}
			ordinal++
		}
		t.source.Reset()
	}

	input := t.input
	t.input = nil
	if err = input.Close(); err != nil {
		return err
	}
	t.source.Reset()

	results := append(t.heap, t.ties...)
	t.heap = nil
	t.ties = nil
	t.tiesBytes = 0

	slices.SortFunc(results, func(a, b topKEntry) int {
		if n := bytes.Compare(a.key, b.key); n != 0 {
			return n
		}
		if a.ordinal < b.ordinal {
			return -1
		}
		if a.ordinal > b.ordinal {
			return 1
		}
		return 0
	})

	skip := int(min(t.offset, uint64(len(results))))
	clear(results[:skip])
	results = results[skip:]
	var retainedBytes uint64
	for i := range results {
		keyCap := uint64(cap(results[i].key))
		results[i].key = nil
		if results[i].bytes >= keyCap {
			results[i].bytes -= keyCap
		} else {
			results[i].bytes = 0
		}
		retainedBytes += results[i].bytes
	}
	t.usedBytes = retainedBytes
	if err = t.claim.Resize(retainedBytes); err != nil {
		return err
	}
	t.results = results
	t.pos = 0
	t.initialized = true
	return nil
}

func (t *topKCursor) fallbackToSort(ctx context.Context, currentRowIdx int) error {
	retained := append(t.heap, t.ties...)
	t.heap = nil
	t.ties = nil
	t.tiesBytes = 0
	for i := range retained {
		retained[i].key = nil
	}
	slices.SortFunc(retained, func(a, b topKEntry) int {
		if a.ordinal < b.ordinal {
			return -1
		}
		if a.ordinal > b.ordinal {
			return 1
		}
		return 0
	})
	if t.claim != nil {
		t.claim.Release()
		t.claim = nil
	}
	t.usedBytes = 0

	splice := &topKSpliceCursor{
		retained:  retained,
		tail:      t.source,
		tailIndex: currentRowIdx,
		input:     t.input,
		env:       t.env,
		layouts:   t.layouts,
	}
	t.source = columnar.Batch{}
	t.input = nil

	sorted, err := exsort.New(ctx, splice, t.sortOptions)
	if err != nil {
		return err
	}
	limit := &limitCursor{
		input:     sorted,
		env:       t.env,
		layouts:   t.layouts,
		offset:    t.offset,
		remaining: t.count,
		withTies:  t.withTies,
		source:    columnar.Batch{Columns: make([]columnar.Vector, 0, len(t.layouts))},
	}
	if t.withTies {
		limit.keys = t.keys
		limit.keyColumns = t.keyColumns
	}
	t.fallback = limit
	t.fellBack = true
	t.initialized = true
	return nil
}

func (t *topKCursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	out.Reset()
	if t.closed || t.ended {
		if t.output != nil {
			closeErr := t.output.Close()
			t.output = nil
			if closeErr != nil {
				return closeErr
			}
		}
		return io.EOF
	}
	defer func() {
		if err != nil && err != io.EOF {
			err = errors.Join(err, t.Close())
		}
	}()
	if !t.initialized {
		if err = t.initialize(ctx); err != nil {
			return err
		}
	}
	if t.fallback != nil {
		err = t.fallback.Next(ctx, out)
		if err == io.EOF {
			t.ended = true
		}
		return err
	}
	if t.pos >= len(t.results) {
		t.ended = true
		if t.output != nil {
			if closeErr := t.output.Close(); closeErr != nil {
				t.output = nil
				return closeErr
			}
			t.output = nil
		}
		if t.claim != nil {
			t.claim.Release()
			t.claim = nil
		}
		t.results = nil
		t.usedBytes = 0
		return io.EOF
	}
	if t.output == nil {
		t.output, err = builder(t.env, t.layouts)
		if err != nil {
			return err
		}
	} else {
		t.output.Reset()
	}
	maxRows := batchRows(out, t.env)
	for t.pos < len(t.results) && t.output.Rows() < maxRows {
		if err = ctx.Err(); err != nil {
			return err
		}
		err = t.output.Append(ctx, t.results[t.pos].row)
		if errors.Is(err, columnar.ErrBatchBudget) && t.output.Rows() > 0 {
			err = nil
			break
		}
		if err != nil {
			return err
		}
		t.results[t.pos] = topKEntry{}
		t.pos++
	}
	return t.output.Publish(out)
}

func (t *topKCursor) Close() error {
	if t.closed {
		return t.closeErr
	}
	t.closed = true
	if t.input != nil {
		t.closeErr = errors.Join(t.closeErr, t.input.Close())
		t.input = nil
	}
	if t.fallback != nil {
		t.closeErr = errors.Join(t.closeErr, t.fallback.Close())
		t.fallback = nil
	}
	t.source.Reset()
	if t.output != nil {
		t.closeErr = errors.Join(t.closeErr, t.output.Close())
		t.output = nil
	}
	clear(t.heap)
	t.heap = nil
	clear(t.ties)
	t.ties = nil
	clear(t.results)
	t.results = nil
	clear(t.keyValues)
	t.keyValues = nil
	t.usedBytes = 0
	t.tiesBytes = 0
	if t.claim != nil {
		t.claim.Release()
		t.claim = nil
	}
	return t.closeErr
}

// topKSpliceCursor feeds retained top-K entries, the remainder of the current
// source batch, and the remaining input cursor into exsort on dynamic fallback.
type topKSpliceCursor struct {
	retained    []topKEntry
	retainedPos int
	tail        columnar.Batch
	tailIndex   int
	tailDone    bool
	input       storage.BatchCursor
	env         *Environment
	layouts     []model.TypeLayout
	output      *columnar.Builder
	closed      bool
	closeErr    error
}

func (s *topKSpliceCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if s.closed {
		return io.EOF
	}
	if s.retainedPos < len(s.retained) {
		if s.output == nil {
			var err error
			s.output, err = builder(s.env, s.layouts)
			if err != nil {
				return err
			}
		} else {
			s.output.Reset()
		}
		limit := batchRows(out, s.env)
		for s.retainedPos < len(s.retained) && s.output.Rows() < limit {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := s.output.Append(ctx, s.retained[s.retainedPos].row)
			if errors.Is(err, columnar.ErrBatchBudget) && s.output.Rows() > 0 {
				break
			}
			if err != nil {
				return err
			}
			s.retained[s.retainedPos] = topKEntry{}
			s.retainedPos++
		}
		if s.retainedPos == len(s.retained) {
			s.retained = nil
		}
		return s.output.Publish(out)
	}
	if !s.tailDone {
		if s.tailIndex < s.tail.Rows() {
			if s.output == nil {
				var err error
				s.output, err = builder(s.env, s.layouts)
				if err != nil {
					return err
				}
			} else {
				s.output.Reset()
			}
			limit := batchRows(out, s.env)
			for s.tailIndex < s.tail.Rows() && s.output.Rows() < limit {
				if err := ctx.Err(); err != nil {
					return err
				}
				err := appendLane(ctx, s.env, s.output, &s.tail, s.tailIndex)
				if errors.Is(err, columnar.ErrBatchBudget) && s.output.Rows() > 0 {
					break
				}
				if err != nil {
					return err
				}
				s.tailIndex++
			}
			if s.tailIndex >= s.tail.Rows() {
				s.tail.Reset()
				s.tailDone = true
			}
			return s.output.Publish(out)
		}
		s.tail.Reset()
		s.tailDone = true
	}
	if s.output != nil {
		err := s.output.Close()
		s.output = nil
		if err != nil {
			return err
		}
	}
	if s.input == nil {
		return io.EOF
	}
	return s.input.Next(ctx, out)
}

func (s *topKSpliceCursor) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	clear(s.retained)
	s.retained = nil
	s.tail.Reset()
	if s.output != nil {
		s.closeErr = errors.Join(s.closeErr, s.output.Close())
		s.output = nil
	}
	if s.input != nil {
		s.closeErr = errors.Join(s.closeErr, s.input.Close())
		s.input = nil
	}
	return s.closeErr
}
