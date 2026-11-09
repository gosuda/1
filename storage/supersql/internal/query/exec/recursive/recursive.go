// Package recursive executes delta-based recursive UNION relations.
package recursive

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/distinctset"
	"github.com/gosuda/supersql-alpha/internal/query/exec/rowstore"
)

// Worktable exposes the sealed previous delta. Lineage-dependent SEARCH/CYCLE
// steps receive the current parent row so generated paths follow that lineage.
type Worktable interface {
	Open(context.Context) (storage.BatchCursor, error)
}

// StepFactory borrows the worktable and captures until its cursor closes.
// Open must honor cancellation. Cursor ownership transfers even on error.
type StepFactory interface {
	Open(context.Context, Worktable) (storage.BatchCursor, error)
}

type SearchMode uint8

const (
	NoSearch SearchMode = iota
	BreadthFirst
	DepthFirst
)

type Options struct {
	Layouts                   []model.TypeLayout
	BaseColumns               int
	Search                    SearchMode
	SearchColumn              int
	SearchKeys                []int
	Cycle                     bool
	CycleMarkColumn           int
	CyclePathColumn           int
	CycleKeys                 []int
	CycleValue, CycleDefault  model.Value
	CycleContinue             bool
	MaxKeyBytes               uint64
	All                       bool
	Step                      StepFactory
	Query                     *runtime.Query
	Encoder                   storage.KeyEncoder
	BatchRows                 int
	BatchBytes, MaxIterations uint64
	Spool                     rowstore.Limits
	Seen                      distinctset.Options
}

var errStepCursor = errors.New("recursive step returned no cursor")

// New always takes ownership of anchor; Query, Encoder and Step are borrowed.
// Layouts, SEARCH/CYCLE indexes and mark values are deeply copied after
// admission. Spool.MetadataBytes bounds driver metadata; Spool.DecodeBytes
// bounds the owned row and generated paths. CYCLE also reserves two bounded keys.
// Seen supplies numeric limits only; its collaborators come from Options.
// MaxIterations bounds recursion generations, excluding the anchor.
// Next honors the requested batch bound; Close never drains a recursive step.
// Next, Close and worktable operations require caller serialization.
func New(ctx context.Context, anchor storage.BatchCursor, options Options) (storage.BatchCursor, error) {
	c := &cursor{source: anchor, options: options}
	if err := c.configure(ctx); err != nil {
		return nil, errors.Join(err, c.Close())
	}
	return c, nil
}

type cursor struct {
	options              Options
	ctx                  context.Context
	cancel               context.CancelFunc
	stopQuery            func() bool
	cancelStep           context.CancelFunc
	claim                *runtime.Reservation
	builder              *columnar.Builder
	source               storage.BatchCursor
	batch                columnar.Batch
	index                int
	row, parent           []model.Value
	delta, generation    *rowstore.Spool
	parents               *rowstore.Reader
	work                  *worktable
	seen                  *distinctset.Set
	cycleColumns          []storage.KeyColumn
	iterations           uint64
	expand                bool
	closed, eof, pending  bool
	closeErr              error
}

func (c *cursor) configure(ctx context.Context) error {
	o := &c.options
	if o.BaseColumns == 0 && o.Search == NoSearch && !o.Cycle {
		o.BaseColumns = len(o.Layouts)
	}
	missingInput := c.source == nil || o.Step == nil
	missingServices := o.Query == nil || o.Encoder == nil
	invalidLimits := o.BatchRows <= 0 || o.BatchBytes == 0 || o.MaxIterations == 0
	if missingInput || missingServices || invalidLimits || !validRecursiveSchema(o) {
		return runtime.ErrInvalidConfig
	}
	reservationBytes := o.Spool.MetadataBytes
	if reservationBytes > uint64(math.MaxInt) || o.Spool.DecodeBytes > uint64(math.MaxInt)-reservationBytes {
		return runtime.ErrInvalidConfig
	}
	reservationBytes += o.Spool.DecodeBytes
	if o.Cycle {
		if o.MaxKeyBytes > (uint64(math.MaxInt)-reservationBytes)/2 {
			return runtime.ErrInvalidConfig
		}
		reservationBytes += 2 * o.MaxKeyBytes
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(o.Query.Context())); err != nil {
		return err
	}
	base := uint64(unsafe.Sizeof(cursor{})) + uint64(unsafe.Sizeof(worktable{})) + 256
	unit := uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(columnar.Vector{}))
	indexBytes := uint64(len(o.SearchKeys)+len(o.CycleKeys))*uint64(unsafe.Sizeof(int(0))) + uint64(len(o.CycleKeys))*uint64(unsafe.Sizeof(storage.KeyColumn{}))
	layoutBytes := uint64(len(o.Layouts)) * unit
	if base > o.Spool.MetadataBytes || layoutBytes > o.Spool.MetadataBytes-base || indexBytes > o.Spool.MetadataBytes-base-layoutBytes {
		return limitError("admit recursive metadata")
	}
	claim, err := o.Query.TryReserve(runtime.WorkMemory, int64(reservationBytes))
	if err != nil {
		return err
	}
	c.claim = claim
	remaining := o.Spool.MetadataBytes - base - layoutBytes - indexBytes
	layouts := make([]model.TypeLayout, len(o.Layouts))
	for i, layout := range o.Layouts {
		owned, used, err := model.CopyTypeLayout(ctx, layout, remaining)
		if err != nil {
			return err
		}
		layouts[i], remaining = owned, remaining-used
	}
	o.Layouts = layouts
	o.SearchKeys = append([]int(nil), o.SearchKeys...)
	o.CycleKeys = append([]int(nil), o.CycleKeys...)
	if o.Cycle {
		var used uint64
		o.CycleValue, used, err = model.CopyValue(ctx, o.CycleValue, remaining)
		if err != nil {
			return err
		}
		remaining -= used
		o.CycleDefault, used, err = model.CopyValue(ctx, o.CycleDefault, remaining)
		if err != nil {
			return err
		}
		remaining -= used
		c.cycleColumns = make([]storage.KeyColumn, len(o.CycleKeys))
		for i, index := range o.CycleKeys {
			layout := layouts[index]
			c.cycleColumns[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: layout.Type, Children: layout.Children}
		}
	}
	c.row = make([]model.Value, len(layouts))
	c.batch.Columns = make([]columnar.Vector, 0, o.BaseColumns)
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.stopQuery = context.AfterFunc(o.Query.Context(), c.cancel)
	c.builder, err = columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: o.BatchRows, MaxBytes: o.BatchBytes, Allocator: o.Query.Allocator(runtime.OutputMemory)})
	if err != nil {
		return err
	}
	c.delta, err = rowstore.New(ctx, o.Query, layouts, o.Spool)
	if err != nil {
		return err
	}
	if !o.All {
		seen := o.Seen
		seen.Query, seen.Layouts, seen.Encoder = o.Query, layouts, o.Encoder
		c.seen, err = distinctset.New(ctx, seen)
	}
	o.Seen = distinctset.Options{}
	return err
}

func validRecursiveSchema(o *Options) bool {
	lineage := o.Search == DepthFirst || o.Cycle
	if o.BaseColumns < 0 || o.BaseColumns > len(o.Layouts) || o.Search > DepthFirst || lineage && o.Spool.MaxReaders < 2 {
		return false
	}
	generated := 0
	if o.Search != NoSearch {
		generated++
		if o.SearchColumn < o.BaseColumns || o.SearchColumn >= len(o.Layouts) || !validKeyIndexes(o.SearchKeys, o.BaseColumns) {
			return false
		}
		layout := o.Layouts[o.SearchColumn]
		if o.Search == BreadthFirst {
			if layout.Type.Storage != model.TypeComposite || len(layout.Children) != len(o.SearchKeys)+1 || layout.Children[0].Type.Storage != model.TypeInt64 {
				return false
			}
			for i, index := range o.SearchKeys {
				if !sameTypeLayout(layout.Children[i+1], o.Layouts[index]) {
					return false
				}
			}
		} else if !validPathLayout(layout, o.SearchKeys, o.Layouts) {
			return false
		}
	} else if len(o.SearchKeys) != 0 {
		return false
	}
	if o.Cycle {
		generated += 2
		if o.MaxKeyBytes == 0 || o.CycleMarkColumn < o.BaseColumns || o.CycleMarkColumn >= len(o.Layouts) || o.CyclePathColumn < o.BaseColumns || o.CyclePathColumn >= len(o.Layouts) || o.CycleMarkColumn == o.CyclePathColumn || !validKeyIndexes(o.CycleKeys, o.BaseColumns) {
			return false
		}
		if o.Search != NoSearch && (o.SearchColumn == o.CycleMarkColumn || o.SearchColumn == o.CyclePathColumn) {
			return false
		}
		if !recursiveValueLayout(o.CycleValue, o.Layouts[o.CycleMarkColumn], 0) || !recursiveValueLayout(o.CycleDefault, o.Layouts[o.CycleMarkColumn], 0) || !validPathLayout(o.Layouts[o.CyclePathColumn], o.CycleKeys, o.Layouts) {
			return false
		}
	} else if len(o.CycleKeys) != 0 {
		return false
	}
	return o.BaseColumns+generated == len(o.Layouts)
}

func validKeyIndexes(keys []int, base int) bool {
	if len(keys) == 0 {
		return false
	}
	for i, index := range keys {
		if index < 0 || index >= base {
			return false
		}
		for _, previous := range keys[:i] {
			if previous == index {
				return false
			}
		}
	}
	return true
}

func validPathLayout(path model.TypeLayout, keys []int, layouts []model.TypeLayout) bool {
	if path.Type.Storage != model.TypeArray || len(path.Children) != 1 {
		return false
	}
	record := path.Children[0]
	if record.Type.Storage != model.TypeComposite || len(record.Children) != len(keys) {
		return false
	}
	for i, index := range keys {
		if !sameTypeLayout(record.Children[i], layouts[index]) {
			return false
		}
	}
	return true
}

func recursiveValueLayout(value model.Value, layout model.TypeLayout, depth int) bool {
	if depth > 64 || value.Type != layout.Type.Storage {
		return false
	}
	if value.IsNull {
		return true
	}
	switch layout.Type.Storage {
	case model.TypeComposite:
		if len(value.ArrayVal) != len(layout.Children) {
			return false
		}
		for i := range value.ArrayVal {
			if !recursiveValueLayout(value.ArrayVal[i], layout.Children[i], depth+1) {
				return false
			}
		}
	case model.TypeArray:
		if len(layout.Children) != 1 || model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))) != nil {
			return false
		}
		for _, element := range value.ArrayVal {
			if !recursiveValueLayout(element, layout.Children[0], depth+1) {
				return false
			}
		}
	}
	return true
}

func sameTypeLayout(left, right model.TypeLayout) bool {
	return sameTypeLayoutAt(left, right, 0)
}

func sameTypeLayoutAt(left, right model.TypeLayout, depth int) bool {
	if depth > 64 || left.Type != right.Type || len(left.Children) != len(right.Children) {
		return false
	}
	for i := range left.Children {
		if !sameTypeLayoutAt(left.Children[i], right.Children[i], depth+1) {
			return false
		}
	}
	return true
}

func limitError(message string) error {
	return &model.Error{Code: "53200", Message: message, Cause: runtime.ErrBudget}
}

func (c *cursor) readRow(ctx context.Context) error {
	for c.index >= c.batch.Rows() {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		c.batch.Reset()
		c.batch.MaxRows = c.options.BatchRows
		if err := c.source.Next(ctx, &c.batch); err != nil {
			return err
		}
		if err := c.batch.Validate(max(c.batch.Length, c.options.BatchRows)); err != nil {
			return err
		}
		c.index = 0
		if c.batch.Rows() == 0 {
			continue
		}
		if c.batch.Rows() > c.options.BatchRows || len(c.batch.Columns) != c.options.BaseColumns {
			return columnar.ErrInvalidBatch
		}
		for i := range c.options.BaseColumns {
			if !c.batch.Columns[i].MatchesLayout(c.options.Layouts[i]) {
				return columnar.ErrValueLayout
			}
		}
	}
	lane := c.batch.Lane(c.index)
	c.index++
	clear(c.row)
	remaining := c.options.Spool.DecodeBytes
	for i := range c.options.BaseColumns {
		value, used, err := columnar.CopyValue(ctx, &c.batch.Columns[i], lane, remaining)
		if err != nil {
			return err
		}
		c.row[i], remaining = value, remaining-used
	}
	c.expand = true
	if err := c.decorateRow(ctx); err != nil {
		return err
	}
	remaining = c.options.Spool.DecodeBytes
	for _, value := range c.row {
		used, err := model.ValueBytes(ctx, value, remaining)
		if err != nil {
			if errors.Is(err, model.ErrValueBudget) || errors.Is(err, model.ErrValueDepth) {
				return limitError("recursive working row exceeds decode limit")
			}
			return err
		}
		remaining -= used
	}
	return nil
}

func (c *cursor) decorateRow(ctx context.Context) error {
	o := &c.options
	if o.Search != NoSearch {
		record := c.keyRecord(o.SearchKeys)
		switch o.Search {
		case BreadthFirst:
			depth := int64(0)
			if c.parent == nil {
				if c.iterations > math.MaxInt64 {
					return &model.Error{Code: "22003", Message: "recursive search depth is out of range"}
				}
				depth = int64(c.iterations)
			} else {
				parent := c.parent[o.SearchColumn]
				if parent.Type != model.TypeComposite || parent.IsNull || len(parent.ArrayVal) != len(o.SearchKeys)+1 || parent.ArrayVal[0].Type != model.TypeInt64 || parent.ArrayVal[0].IsNull {
					return columnar.ErrValueLayout
				}
				if parent.ArrayVal[0].IntVal == math.MaxInt64 {
					return &model.Error{Code: "22003", Message: "recursive search depth is out of range"}
				}
				depth = parent.ArrayVal[0].IntVal + 1
			}
			fields := make([]model.Value, len(record.ArrayVal)+1)
			fields[0] = model.Int64Val(depth)
			copy(fields[1:], record.ArrayVal)
			c.row[o.SearchColumn] = model.Value{Type: model.TypeComposite, ArrayVal: fields}
		case DepthFirst:
			path, err := appendPath(c.parentPath(o.SearchColumn), record)
			if err != nil {
				return err
			}
			c.row[o.SearchColumn] = path
		}
	}
	if !o.Cycle {
		return nil
	}
	record := c.keyRecord(o.CycleKeys)
	cycled := false
	if c.parent != nil {
		var err error
		cycled, err = c.pathContains(ctx, c.parentPath(o.CyclePathColumn), record)
		if err != nil {
			return err
		}
	}
	if cycled {
		c.row[o.CycleMarkColumn] = o.CycleValue
		c.expand = false
	} else {
		c.row[o.CycleMarkColumn] = o.CycleDefault
		c.expand = o.CycleContinue
	}
	path, err := appendPath(c.parentPath(o.CyclePathColumn), record)
	if err != nil {
		return err
	}
	c.row[o.CyclePathColumn] = path
	return nil
}

func (c *cursor) keyRecord(keys []int) model.Value {
	fields := make([]model.Value, len(keys))
	for i, index := range keys {
		fields[i] = c.row[index]
	}
	return model.Value{Type: model.TypeComposite, ArrayVal: fields}
}

func (c *cursor) parentPath(column int) model.Value {
	if c.parent == nil {
		return model.Value{}
	}
	return c.parent[column]
}

func appendPath(parent, record model.Value) (model.Value, error) {
	if parent.Type == model.TypeAny {
		return model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}}, ArrayVal: []model.Value{record}}, nil
	}
	if parent.Type != model.TypeArray || parent.IsNull || len(parent.Dimensions) != 1 || parent.Dimensions[0].Length != int32(len(parent.ArrayVal)) {
		return model.Value{}, columnar.ErrValueLayout
	}
	if len(parent.ArrayVal) >= math.MaxInt32 {
		return model.Value{}, &model.Error{Code: "22003", Message: "recursive path length is out of range"}
	}
	values := make([]model.Value, len(parent.ArrayVal)+1)
	copy(values, parent.ArrayVal)
	values[len(parent.ArrayVal)] = record
	return model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: int32(len(values)), LowerBound: 1}}, ArrayVal: values}, nil
}

func (c *cursor) pathContains(ctx context.Context, path, record model.Value) (bool, error) {
	if path.Type != model.TypeArray || path.IsNull || len(path.Dimensions) != 1 || path.Dimensions[0].Length != int32(len(path.ArrayVal)) {
		return false, columnar.ErrValueLayout
	}
	current, err := c.options.Encoder.EncodePrefix(ctx, c.cycleColumns, record.ArrayVal, c.options.MaxKeyBytes)
	if err != nil {
		return false, err
	}
	for _, candidate := range path.ArrayVal {
		if candidate.Type != model.TypeComposite || candidate.IsNull || len(candidate.ArrayVal) != len(c.cycleColumns) {
			return false, columnar.ErrValueLayout
		}
		encoded, err := c.options.Encoder.EncodePrefix(ctx, c.cycleColumns, candidate.ArrayVal, c.options.MaxKeyBytes)
		if err != nil {
			return false, err
		}
		if bytes.Equal(current, encoded) {
			return true, nil
		}
	}
	return false, nil
}

func (c *cursor) finishStep() error {
	if c.cancelStep != nil {
		c.cancelStep()
		c.cancelStep = nil
	}
	var err error
	if c.source != nil {
		err = c.source.Close()
		c.source = nil
	}
	c.batch.Reset()
	c.index = 0
	clear(c.row)
	if c.work != nil {
		err = errors.Join(err, c.work.close())
		c.work = nil
	}
	return err
}

func (c *cursor) needsLineage() bool {
	return c.options.Search == DepthFirst || c.options.Cycle
}

func (c *cursor) advance(ctx context.Context) error {
	if c.needsLineage() {
		return c.advanceDecorated(ctx)
	}
	if err := c.finishStep(); err != nil {
		return err
	}
	if c.delta.Rows() == 0 {
		return io.EOF
	}
	if c.iterations == c.options.MaxIterations {
		return limitError("recursive iteration limit exceeded")
	}
	if err := c.delta.Seal(ctx); err != nil {
		return err
	}
	stepContext, cancel := context.WithCancel(c.ctx)
	c.cancelStep = cancel
	c.work = &worktable{ctx: stepContext, spool: c.delta, options: &c.options, closeSpool: true}
	c.delta = nil
	var err error
	c.delta, err = rowstore.New(ctx, c.options.Query, c.options.Layouts, c.options.Spool)
	if err != nil {
		return err
	}
	c.iterations++
	return c.openStep(ctx, stepContext, cancel)
}

func (c *cursor) advanceDecorated(ctx context.Context) error {
	if err := c.finishStep(); err != nil {
		return err
	}
	for {
		if c.parents != nil {
			position, parent, err := c.parents.NextRow(ctx)
			if err == nil {
				c.parent = parent
				stepContext, cancel := context.WithCancel(c.ctx)
				c.cancelStep = cancel
				c.work = &worktable{ctx: stepContext, spool: c.generation, options: &c.options, position: position, single: true}
				return c.openStep(ctx, stepContext, cancel)
			}
			if err != io.EOF {
				return err
			}
			if err := c.closeGeneration(); err != nil {
				return err
			}
		}
		if c.delta.Rows() == 0 {
			return io.EOF
		}
		if c.iterations == c.options.MaxIterations {
			return limitError("recursive iteration limit exceeded")
		}
		if err := c.delta.Seal(ctx); err != nil {
			return err
		}
		c.generation, c.delta = c.delta, nil
		var err error
		c.delta, err = rowstore.New(ctx, c.options.Query, c.options.Layouts, c.options.Spool)
		if err != nil {
			return err
		}
		c.parents, err = c.generation.Reader(ctx)
		if err != nil {
			return err
		}
		c.iterations++
	}
}

func (c *cursor) openStep(ctx, stepContext context.Context, cancel context.CancelFunc) error {
	// Only cancellation during Open propagates from this Next call. A step
	// retained across Next calls must not inherit the completed call's context.
	stop := context.AfterFunc(ctx, cancel)
	source, err := c.options.Step.Open(stepContext, c.work)
	stop()
	c.source = source
	if err != nil {
		return err
	}
	if c.source == nil {
		return errStepCursor
	}
	return context.Cause(ctx)
}

func (c *cursor) closeGeneration() error {
	var err error
	if c.parents != nil {
		err = c.parents.Close()
		c.parents = nil
	}
	if c.generation != nil {
		err = errors.Join(err, c.generation.Close())
		c.generation = nil
	}
	c.parent = nil
	return err
}

func (c *cursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out == nil || out.MaxRows < 0 {
		return errors.Join(columnar.ErrInvalidBatch, c.Close())
	}
	out.Reset()
	if c.closed {
		if c.eof {
			return io.EOF
		}
		return runtime.ErrClosed
	}
	defer func() {
		if err != nil {
			c.eof = err == io.EOF
			if cleanup := c.Close(); cleanup != nil {
				err = errors.Join(err, cleanup)
			}
		}
	}()
	if c.eof {
		return io.EOF
	}
	call, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	limit := c.options.BatchRows
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	c.builder.Reset()
	count := 0
	for count < limit {
		if err := errors.Join(context.Cause(call), context.Cause(c.ctx)); err != nil {
			return err
		}
		if !c.pending {
			if err := c.nextAccepted(call); err != nil {
				if err != io.EOF || count == 0 {
					return err
				}
				c.eof = true
				break
			}
		}
		if err := c.builder.Append(call, c.row); err != nil {
			if errors.Is(err, columnar.ErrBatchBudget) && count > 0 {
				c.pending = true
				break
			}
			return err
		}
		c.pending = false
		clear(c.row)
		count++
	}
	return c.builder.Publish(out)
}

func (c *cursor) nextAccepted(ctx context.Context) error {
	for {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		err := c.readRow(ctx)
		if err == io.EOF {
			if err := c.advance(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if c.seen != nil {
			accepted, err := c.seen.Insert(ctx, c.row)
			if err != nil {
				return err
			}
			if !accepted {
				continue
			}
		}
		if !c.expand {
			return nil
		}
		_, err = c.delta.AppendRow(ctx, c.row)
		return err
	}
}

func (c *cursor) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.stopQuery != nil {
		c.stopQuery()
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.closeErr = c.finishStep()
	c.closeErr = errors.Join(c.closeErr, c.closeGeneration())
	if c.delta != nil {
		c.closeErr = errors.Join(c.closeErr, c.delta.Close())
		c.delta = nil
	}
	if c.seen != nil {
		c.closeErr = errors.Join(c.closeErr, c.seen.Close())
		c.seen = nil
	}
	if c.builder != nil {
		c.closeErr = errors.Join(c.closeErr, c.builder.Close())
		c.builder = nil
	}
	c.row, c.parent, c.batch.Columns, c.cycleColumns = nil, nil, nil, nil
	c.options = Options{}
	if c.claim != nil {
		c.claim.Release()
		c.claim = nil
	}
	return c.closeErr
}
