package neosepia

import (
	"context"
	"errors"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type paxScanSpec struct {
	Target                   storage.ObjectRef
	Layout                   []storage.ColumnLayout
	Projection               []model.ColumnID
	Spans                    []encodedSpan
	Predicate                *predicateProgram
	Limit                    storage.ScanLimit
	MaxRows                  int
	MaxBytes, PredicateBytes uint64
	Overlay                  storage.OverlayView
	OverlayLimits            mergeLimits
	OverlayBlockBytes        uint64
}

type defaultColumn struct {
	id      model.ColumnID
	builder *columnar.Builder
	vector  columnar.Vector
}

type paxScanCursor struct {
	source           *readCursor
	spec             paxScanSpec
	lease            *blockLease
	pending          readRow
	hasPending       bool
	finished, closed bool
	span             int
	emitted          uint64
	lanes            []uint32
	ownUnits         []uint64
	blockUnit        uint64
	filter           columnar.Batch
	scratch          predicateScratch
	defaults         []defaultColumn
	empty            *columnar.Builder
	used             uint64
	own              *columnar.Builder
	ownBatch         columnar.Batch
	ownIDs           []model.ColumnID
	ownSlots         []int
	ownValues        []model.Value
	reverse          bool
}

// Source ownership transfers on every return. Spec metadata is borrowed from the
// bound plan through Close. MaxBytes excludes source blocks, predicate scratch,
// and the separately bounded overlay merge and overlay vectors.
func newPAXScanCursor(ctx context.Context, source *mvccCursor, spec paxScanSpec) (_ *paxScanCursor, err error) {
	if source == nil {
		return nil, storage.ErrInvalidScanPlan
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, source.Close())
		}
	}()
	if err := (storage.ReadRequest{Target: spec.Target, Layout: spec.Layout, ColumnIDs: spec.Projection}).Validate(ctx); err != nil {
		return nil, err
	}
	if spec.MaxRows <= 0 || uint64(spec.MaxRows) > math.MaxUint32 {
		return nil, columnar.ErrBatchBudget
	}
	if !spec.Limit.Enabled && spec.Limit.Count != 0 {
		return nil, storage.ErrInvalidScanPlan
	}
	filterColumns := 0
	if spec.Predicate != nil {
		filterColumns = len(spec.Predicate.Columns())
	}
	fixed := uint64(unsafe.Sizeof(paxScanCursor{})) + uint64(unsafe.Sizeof(readCursor{})) + uint64(spec.MaxRows)*12 + uint64(filterColumns+len(spec.Projection))*uint64(unsafe.Sizeof(columnar.Vector{}))
	if fixed > spec.MaxBytes {
		return nil, columnar.ErrBatchBudget
	}
	reader, err := newReadCursor(ctx, source, spec.Overlay, spec.Target, spec.OverlayLimits)
	if err != nil {
		return nil, err
	}
	cursor := &paxScanCursor{source: reader, spec: spec, used: fixed, lanes: make([]uint32, 0, spec.MaxRows), ownUnits: make([]uint64, 0, spec.MaxRows), filter: columnar.Batch{Columns: make([]columnar.Vector, filterColumns)}}
	cursor.reverse = source.reverse
	if cursor.reverse {
		cursor.span = len(spec.Spans) - 1
	}
	return cursor, nil
}

func (cursor *paxScanCursor) Next(ctx context.Context, out *columnar.Batch) (err error) {
	if out != nil {
		out.Reset()
	}
	if cursor.closed {
		return io.EOF
	}
	defer func() {
		if err != nil {
			if out != nil {
				out.Reset()
			}
			err = iterationError(err, cursor.Close())
		}
	}()
	if out == nil || out.MaxRows < 0 {
		return columnar.ErrInvalidBatch
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	clear(cursor.filter.Columns)
	cursor.filter.Selection = nil
	cursor.filter.Length = 0
	cursor.ownBatch.Reset()
	cursor.ownUnits = cursor.ownUnits[:0]
	cursor.blockUnit = 0
	if cursor.own != nil {
		cursor.own.Reset()
	}
	if cursor.lease != nil {
		err = cursor.lease.release()
		cursor.lease = nil
		if err != nil {
			return err
		}
	}
	if cursor.finished || len(cursor.spec.Spans) == 0 {
		return io.EOF
	}
	if cursor.spec.Limit.Enabled && cursor.emitted == cursor.spec.Limit.Count {
		return io.EOF
	}
	maximum := cursor.spec.MaxRows
	if out.MaxRows > 0 {
		maximum = min(maximum, out.MaxRows)
	}
	if err := cursor.collect(ctx, maximum); err != nil {
		return err
	}
	if cursor.lease != nil {
		key, _, _, _, _, keyErr := cursor.lease.block.Version(0)
		if keyErr != nil {
			return keyErr
		}
		cursor.blockUnit = samplingUnit(cursor.spec.Target, key)
	}
	if cursor.lease == nil && cursor.ownBatch.Length == 0 {
		return io.EOF
	}
	cursor.filter.Length = cursor.rows()
	cursor.filter.Selection = cursor.lanes
	if cursor.spec.Predicate != nil {
		// Visibility has already consumed these lanes: rejecting their payload
		// cannot expose an older matching version from another run.
		if cursor.lease != nil {
			mayMatch, err := cursor.spec.Predicate.zoneMayMatch(ctx, cursor.lease.block, cursor.spec.Predicate.root)
			if err != nil {
				return err
			}
			if !mayMatch {
				cursor.lease.block.observeZonePruned()
				return cursor.publishEmpty(out)
			}
		}
		for i, id := range cursor.spec.Predicate.Columns() {
			vector, err := cursor.column(ctx, id)
			if err != nil {
				return err
			}
			cursor.filter.Columns[i] = vector
		}
		if err := cursor.spec.Predicate.Filter(ctx, &cursor.filter, &cursor.scratch, cursor.spec.PredicateBytes); err != nil {
			return err
		}
	}
	selected := cursor.filter.Selection
	if cursor.spec.Limit.Enabled {
		selected = selected[:min(uint64(len(selected)), cursor.spec.Limit.Count-cursor.emitted)]
	}
	if len(selected) == 0 {
		return cursor.publishEmpty(out)
	}
	if cap(out.Columns) < len(cursor.spec.Projection) {
		out.Columns = make([]columnar.Vector, len(cursor.spec.Projection))
	} else {
		out.Columns = out.Columns[:len(cursor.spec.Projection)]
	}
	for i, id := range cursor.spec.Projection {
		vector, err := cursor.column(ctx, id)
		if err != nil {
			return err
		}
		out.Columns[i] = vector
	}
	// Length is the physical extent of the leased block or owned builder that every
	// projected vector carries; Selection names the logical lanes, so a predicate
	// or LIMIT may publish fewer rows than Length.
	out.Length = cursor.rows()
	out.Selection = selected
	if cursor.spec.Limit.Enabled {
		cursor.emitted += uint64(len(selected))
	}
	if cursor.spec.Limit.Enabled && cursor.emitted == cursor.spec.Limit.Count {
		if err := cursor.source.Close(); err != nil {
			return err
		}
		cursor.pending = readRow{}
		cursor.hasPending = false
		cursor.finished = true
	}
	return nil
}

func (cursor *paxScanCursor) collect(ctx context.Context, maximum int) error {
	cursor.lanes = cursor.lanes[:0]
	for len(cursor.lanes) < maximum {
		var row readRow
		if cursor.hasPending {
			row = cursor.pending
			cursor.pending = readRow{}
			cursor.hasPending = false
		} else {
			var err error
			row, err = cursor.source.Next(ctx)
			if err == io.EOF {
				cursor.finished = true
				break
			}
			if err != nil {
				return err
			}
		}
		if !cursor.inSpan(row.key) {
			if cursor.span < 0 || cursor.span == len(cursor.spec.Spans) {
				cursor.finished = true
				return cursor.source.Close()
			}
			continue
		}
		if row.own != nil {
			if cursor.lease != nil {
				cursor.pending = row
				cursor.hasPending = true
				break
			}
			if err := cursor.loadOwn(ctx, row.own); err != nil {
				if errors.Is(err, columnar.ErrBatchBudget) && cursor.ownBatch.Length != 0 {
					cursor.pending = row
					cursor.hasPending = true
					break
				}
				return err
			}
			cursor.lanes = append(cursor.lanes, uint32(cursor.ownBatch.Length-1))
			cursor.ownUnits = append(cursor.ownUnits, samplingUnit(cursor.spec.Target, row.own.Key))
			continue
		}
		if err := validateScanTarget(cursor.spec.Target, row.lease.block.Target()); err != nil {
			return err
		}
		if cursor.ownBatch.Length != 0 || (cursor.lease != nil && cursor.lease != row.lease) {
			cursor.pending = row
			cursor.hasPending = true
			break
		}
		if cursor.lease == nil {
			cursor.lease = row.lease.retain()
		}
		cursor.lanes = append(cursor.lanes, uint32(row.row))
	}
	return nil
}

func (cursor *paxScanCursor) inSpan(key []byte) bool {
	if cursor.reverse {
		for cursor.span >= 0 && compareCuts(keyCut{prefix: key}, cursor.spec.Spans[cursor.span].lower) < 0 {
			cursor.span--
		}
		return cursor.span >= 0 && cursor.spec.Spans[cursor.span].contains(key)
	}
	for cursor.span < len(cursor.spec.Spans) && compareCuts(keyCut{prefix: key}, cursor.spec.Spans[cursor.span].upper) >= 0 {
		cursor.span++
	}
	return cursor.span < len(cursor.spec.Spans) && cursor.spec.Spans[cursor.span].contains(key)
}

func (cursor *paxScanCursor) rows() int {
	if cursor.lease != nil {
		return cursor.lease.block.Rows()
	}
	return cursor.ownBatch.Length
}

func (cursor *paxScanCursor) column(ctx context.Context, id model.ColumnID) (columnar.Vector, error) {
	layout, ok := scanColumnLayout(cursor.spec.Layout, id)
	if !ok {
		return columnar.Vector{}, storage.ErrInvalidProjection
	}
	var vector columnar.Vector
	var err error
	if cursor.lease == nil {
		for i, ownID := range cursor.ownIDs {
			if ownID == id {
				vector = cursor.ownBatch.Columns[i]
				break
			}
		}
	} else {
		vector, err = cursor.lease.block.Column(ctx, id)
		if errors.Is(err, errColumnMissing) {
			if cursor.lease.block.Target().DescriptorVersion == cursor.spec.Target.DescriptorVersion {
				return columnar.Vector{}, storage.ErrCorruptStorage
			}
			return cursor.defaultColumn(ctx, layout)
		}
	}
	if err != nil {
		return columnar.Vector{}, err
	}
	if !scanTypeMatches(vector, model.TypeLayout{Type: layout.Type, Children: layout.Children}) {
		return columnar.Vector{}, storage.ErrInvalidLayout
	}
	if !layout.Nullable && len(vector.Nulls) != 0 {
		for _, lane := range cursor.filter.Selection {
			if vector.IsNull(int(lane)) {
				return columnar.Vector{}, storage.ErrCorruptStorage
			}
		}
	}
	return vector, nil
}

func scanColumnLayout(layout []storage.ColumnLayout, id model.ColumnID) (*storage.ColumnLayout, bool) {
	low, high := 0, len(layout)
	for low < high {
		middle := low + (high-low)/2
		if layout[middle].ID < id {
			low = middle + 1
		} else {
			high = middle
		}
	}
	if low == len(layout) || layout[low].ID != id {
		return nil, false
	}
	return &layout[low], true
}

func scanTypeMatches(vector columnar.Vector, layout model.TypeLayout) bool {
	if vector.Type != layout.Type || len(vector.Children) != len(layout.Children) {
		return false
	}
	for i, child := range layout.Children {
		if !scanTypeMatches(vector.Children[i], child) {
			return false
		}
	}
	return true
}

func (cursor *paxScanCursor) defaultColumn(ctx context.Context, layout *storage.ColumnLayout) (columnar.Vector, error) {
	for i := range cursor.defaults {
		column := &cursor.defaults[i]
		if column.id == layout.ID {
			vector := column.vector
			vector.Length = cursor.lease.block.Rows()
			return vector, nil
		}
	}
	value := layout.MissingValue
	if !layout.HasMissingValue {
		if !layout.Nullable {
			return columnar.Vector{}, storage.ErrCorruptStorage
		}
		value = model.NullVal(layout.Type.Storage)
	}
	if len(cursor.defaults) == cap(cursor.defaults) {
		capacity := max(4, len(cursor.defaults)+1, min(math.MaxInt/2, cap(cursor.defaults))*2)
		size := uint64(unsafe.Sizeof(defaultColumn{}))
		if uint64(capacity) > (cursor.spec.MaxBytes-cursor.used)/size {
			return columnar.Vector{}, columnar.ErrBatchBudget
		}
		next := make([]defaultColumn, len(cursor.defaults), capacity)
		copy(next, cursor.defaults)
		cursor.used += uint64(capacity-cap(cursor.defaults)) * size
		cursor.defaults = next
	}
	overhead := uint64(unsafe.Sizeof(columnar.Builder{})) + uint64(unsafe.Sizeof(columnar.Vector{}))
	if overhead > cursor.spec.MaxBytes-cursor.used {
		return columnar.Vector{}, columnar.ErrBatchBudget
	}
	builder, err := columnar.NewBuilder([]model.TypeLayout{{Type: layout.Type, Children: layout.Children}}, columnar.BuilderOptions{MaxRows: 1, MaxBytes: cursor.spec.MaxBytes - cursor.used - overhead})
	if err != nil {
		return columnar.Vector{}, err
	}
	if err := builder.Append(ctx, []model.Value{value}); err != nil {
		return columnar.Vector{}, errors.Join(err, builder.Close())
	}
	var batch columnar.Batch
	if err := builder.Publish(&batch); err != nil {
		return columnar.Vector{}, errors.Join(err, builder.Close())
	}
	vector := batch.Columns[0]
	vector.Encoding = columnar.Constant
	cursor.defaults = append(cursor.defaults, defaultColumn{id: layout.ID, builder: builder, vector: vector})
	cursor.used += uint64(unsafe.Sizeof(columnar.Builder{})) + builder.AllocatedBytes()
	vector.Length = cursor.lease.block.Rows()
	return vector, nil
}

func (cursor *paxScanCursor) publishEmpty(out *columnar.Batch) error {
	if cursor.empty == nil {
		overhead := uint64(unsafe.Sizeof(columnar.Builder{})) + uint64(len(cursor.spec.Projection))*uint64(unsafe.Sizeof(model.TypeLayout{}))
		if overhead > cursor.spec.MaxBytes-cursor.used {
			return columnar.ErrBatchBudget
		}
		layouts := make([]model.TypeLayout, len(cursor.spec.Projection))
		for i, id := range cursor.spec.Projection {
			layout, _ := scanColumnLayout(cursor.spec.Layout, id)
			layouts[i] = model.TypeLayout{Type: layout.Type, Children: layout.Children}
		}
		builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 1, MaxBytes: cursor.spec.MaxBytes - cursor.used - overhead})
		if err != nil {
			return err
		}
		cursor.empty = builder
		cursor.used += uint64(unsafe.Sizeof(columnar.Builder{})) + builder.AllocatedBytes()
	}
	if err := cursor.empty.Publish(out); err != nil {
		return err
	}
	out.Selection = cursor.lanes[:0]
	return nil
}

func (cursor *paxScanCursor) SampleUnit(lane uint32) (uint64, bool) {
	if cursor.closed {
		return 0, false
	}
	if cursor.lease != nil {
		if uint64(lane) >= uint64(cursor.lease.block.Rows()) {
			return 0, false
		}
		return cursor.blockUnit, cursor.blockUnit != 0
	}
	if uint64(lane) >= uint64(len(cursor.ownUnits)) {
		return 0, false
	}
	return cursor.ownUnits[lane], true
}

func samplingUnit(target storage.ObjectRef, key []byte) uint64 {
	value := uint64(target.ObjectID) ^ target.DescriptorVersion*0x9e3779b97f4a7c15 ^ target.Generation*0xbf58476d1ce4e5b9 ^ uint64(target.EncodingVersion)
	for _, octet := range key {
		value ^= uint64(octet)
		value *= 0x100000001b3
	}
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	value ^= value >> 31
	if value == 0 {
		return 1
	}
	return value
}

func (cursor *paxScanCursor) Close() error {
	if cursor.closed {
		return nil
	}
	cursor.closed = true
	err := cursor.source.Close()
	if cursor.lease != nil {
		err = errors.Join(err, cursor.lease.release())
		cursor.lease = nil
	}
	for _, column := range cursor.defaults {
		err = errors.Join(err, column.builder.Close())
	}
	if cursor.empty != nil {
		err = errors.Join(err, cursor.empty.Close())
		cursor.empty = nil
	}
	if cursor.own != nil {
		err = errors.Join(err, cursor.own.Close())
		cursor.own = nil
	}
	cursor.ownBatch = columnar.Batch{}
	cursor.ownIDs = nil
	cursor.ownSlots = nil
	cursor.ownValues = nil
	err = errors.Join(err, cursor.scratch.Close())
	cursor.source = nil
	cursor.pending = readRow{}
	cursor.filter = columnar.Batch{}
	cursor.spec = paxScanSpec{}
	cursor.lanes = nil
	cursor.ownUnits = nil
	cursor.defaults = nil
	return err
}
