package neosepia

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// The caller retains the source lease through encoding and serializes Close.
// Workspace excludes the source's independently bounded decode cache and output.
func rewriteBlock(ctx context.Context, source *paxBlock, order []uint32, limits blockLimits) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil || source.closed {
		return nil, storage.ErrClosed
	}
	if len(order) == 0 || len(order) > limits.MaxRows {
		return nil, columnar.ErrInvalidBatch
	}
	for i, lane := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if uint64(lane) >= uint64(source.rows) {
			return nil, columnar.ErrInvalidBatch
		}
		if i != 0 && lane <= order[i-1] {
			return nil, columnar.ErrInvalidBatch
		}
	}
	budget := encBudget{limit: limits.MaxDecodedBytes}
	headerBytes := uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(unsafe.Sizeof(model.ColumnID(0)))
	if err := budget.chargeMul(uint64(source.columns), headerBytes); err != nil {
		return nil, err
	}
	columns := make([]model.ColumnID, source.columns)
	batch := columnar.Batch{Length: source.rows, Columns: make([]columnar.Vector, source.columns)}
	for i := range columns {
		columns[i] = model.ColumnID(binary.LittleEndian.Uint32(source.descriptors[i*paxColumnSize:]))
		vector, err := source.Column(ctx, columns[i])
		if err != nil {
			return nil, err
		}
		batch.Columns[i] = vector
	}
	selected, used, err := gatherDecodedBlock(ctx, &batch, order, budget.limit-budget.used)
	if err != nil {
		return nil, err
	}
	if err := budget.charge(used); err != nil {
		return nil, err
	}
	versions, err := gatherVersions(ctx, source.vis, order, &budget)
	if err != nil {
		return nil, err
	}
	limits.MaxDecodedBytes -= budget.used
	if limits.MaxDecodedBytes == 0 {
		return nil, columnar.ErrBatchBudget
	}
	return encodeBlock(ctx, source.target, versions, &selected, columns, limits)
}

func gatherVersions(ctx context.Context, visibility []byte, order []uint32, budget *encBudget) (blockVersions, error) {
	rowBytes := uint64(unsafe.Sizeof(columnar.Range{})) + uint64(unsafe.Sizeof(model.Timestamp(0))) + 8 + uint64(unsafe.Sizeof(storage.MutationOp(0)))
	if err := budget.chargeMul(uint64(len(order)), rowBytes); err != nil {
		return blockVersions{}, err
	}
	keyType := model.Type{ID: 17, Storage: model.TypeBytes}
	versions := blockVersions{
		Keys:     columnar.Vector{Type: keyType, Length: len(order), Data: visibility, Ranges: make([]columnar.Range, len(order))},
		CommitTS: make([]model.Timestamp, len(order)), Ordinals: make([]uint64, len(order)), Kinds: make([]storage.MutationOp, len(order)),
	}
	for i, lane := range order {
		if err := ctx.Err(); err != nil {
			return blockVersions{}, err
		}
		record := visibility[int(lane)*paxVersionSize:]
		versions.CommitTS[i] = model.Timestamp(int64(binary.LittleEndian.Uint64(record)))
		versions.Ordinals[i] = binary.LittleEndian.Uint64(record[8:])
		versions.Kinds[i] = storage.MutationOp(record[16])
		versions.Keys.Ranges[i] = columnar.Range{Offset: binary.LittleEndian.Uint32(record[24:]), Length: binary.LittleEndian.Uint32(record[28:])}
		if versions.Kinds[i] == storage.MutationDeleteRange {
			if versions.EndKeys.Ranges == nil {
				if err := budget.chargeMul(uint64(len(order)), uint64(unsafe.Sizeof(columnar.Range{}))); err != nil {
					return blockVersions{}, err
				}
				versions.EndKeys = columnar.Vector{Type: keyType, Length: len(order), Data: visibility, Ranges: make([]columnar.Range, len(order))}
			}
			versions.EndKeys.Ranges[i] = columnar.Range{Offset: binary.LittleEndian.Uint32(record[32:]), Length: binary.LittleEndian.Uint32(record[36:])}
		}
	}
	return versions, nil
}

type coalescedBlockEmit func(target storage.ObjectRef, data []byte, blobs []toastBlob, rows uint64, historyUntil model.Timestamp) error

// paxBlockCoalescer accumulates surviving rows across multiple source PAX
// blocks into full target PAX blocks (up to BlockRows / BlockBytes) without
// pinning source block leases. Rows with differing target descriptor versions
// flush the pending block before reinitializing so historical schemas stay intact.
type paxBlockCoalescer struct {
	limits       blockLimits
	arenaBudget  uint64
	target       storage.ObjectRef
	columnIDs    []model.ColumnID
	builder      *columnar.Builder
	decoded      []columnar.Vector
	vectors      []*columnar.Vector
	lanes        []int
	keyData      []byte
	keyRanges    []columnar.Range
	endData      []byte
	endRanges    []columnar.Range
	commitTS     []model.Timestamp
	ordinals     []uint64
	kinds        []storage.MutationOp
	hasEndKey    bool
	historyUntil model.Timestamp
	dataBytes    uint64
	initialized  bool
	touch        uint64
}

func newPAXBlockCoalescer(limits blockLimits, arenaBudget uint64) *paxBlockCoalescer {
	maxArena := limits.MaxDecodedBytes
	if arenaBudget == 0 || arenaBudget > maxArena {
		arenaBudget = maxArena
	}
	return &paxBlockCoalescer{limits: limits, arenaBudget: arenaBudget}
}

func (c *paxBlockCoalescer) rows() int {
	return len(c.commitTS)
}

func (c *paxBlockCoalescer) arenaBytes() uint64 {
	var total uint64
	if c.builder != nil {
		total += c.builder.AllocatedBytes()
	}
	total += uint64(cap(c.keyData)) + uint64(cap(c.endData)) + uint64(cap(c.commitTS))*paxVersionSize
	return total
}

func (c *paxBlockCoalescer) initForBlock(ctx context.Context, source *paxBlock) error {
	if c.builder != nil {
		if err := c.builder.Close(); err != nil {
			return err
		}
		c.builder = nil
	}
	cols := source.columns
	columnIDs := make([]model.ColumnID, cols)
	decoded := make([]columnar.Vector, cols)
	layouts := make([]model.TypeLayout, cols)
	for i := range cols {
		id := model.ColumnID(binary.LittleEndian.Uint32(source.descriptors[i*paxColumnSize:]))
		vec, err := source.Column(ctx, id)
		if err != nil {
			return err
		}
		columnIDs[i] = id
		decoded[i] = vec
		layouts[i] = vectorTypeLayout(&vec)
	}
	if cols > 0 {
		builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{
			MaxRows:  c.limits.MaxRows,
			MaxBytes: c.arenaBudget,
		})
		if err != nil {
			return err
		}
		c.builder = builder
	}
	c.target = source.target
	c.columnIDs = columnIDs
	c.decoded = decoded
	c.vectors = make([]*columnar.Vector, cols)
	c.lanes = make([]int, cols)
	c.initialized = true
	return nil
}

func vectorTypeLayout(v *columnar.Vector) model.TypeLayout {
	layout := model.TypeLayout{Type: v.Type}
	if len(v.Children) != 0 {
		layout.Children = make([]model.TypeLayout, len(v.Children))
		for i := range v.Children {
			layout.Children[i] = vectorTypeLayout(&v.Children[i])
		}
	}
	return layout
}

func (c *paxBlockCoalescer) matchesBlockSchema(source *paxBlock) bool {
	if c.target != source.target {
		return false
	}
	if source.columns != len(c.columnIDs) {
		return false
	}
	for i, id := range c.columnIDs {
		if model.ColumnID(binary.LittleEndian.Uint32(source.descriptors[i*paxColumnSize:])) != id {
			return false
		}
	}
	return true
}

func (c *paxBlockCoalescer) appendRow(
	ctx context.Context,
	source *paxBlock,
	rowIdx int,
	key, end []byte,
	ts model.Timestamp,
	ordinal uint64,
	kind storage.MutationOp,
	until model.Timestamp,
	emit coalescedBlockEmit,
) error {
	if source == nil || source.closed {
		return storage.ErrClosed
	}
	zeroColumnTombstone := c.initialized && c.target == source.target && source.columns == 0 && len(c.columnIDs) > 0 && kind != storage.MutationUpsert
	if c.initialized && !zeroColumnTombstone && !c.matchesBlockSchema(source) {
		if err := c.flush(ctx, emit); err != nil {
			return err
		}
		c.initialized = false
	}
	if !c.initialized {
		if err := c.initForBlock(ctx, source); err != nil {
			return err
		}
	}
	rowBytes := uint64(len(key)) + uint64(len(end)) + paxVersionSize
	if !zeroColumnTombstone && c.builder != nil {
		for i, id := range c.columnIDs {
			vec, err := source.Column(ctx, id)
			if err != nil {
				return err
			}
			c.decoded[i] = vec
			c.vectors[i] = &c.decoded[i]
			c.lanes[i] = rowIdx
			rowBytes += estimateVectorRowBytes(&c.decoded[i], rowIdx)
		}
	} else if c.builder != nil {
		for i := range c.columnIDs {
			c.vectors[i] = nil
			c.lanes[i] = 0
		}
	}
	blockOverhead := uint64(paxHeaderSize) + uint64(len(c.columnIDs))*paxColumnSize
	if c.rows() > 0 && (c.rows() >= c.limits.MaxRows ||
		blockOverhead+c.dataBytes+rowBytes > c.limits.MaxBytes ||
		uint64(len(c.keyData))+uint64(len(key)) > math.MaxUint32 ||
		uint64(len(c.endData))+uint64(len(end)) > math.MaxUint32) {
		if err := c.flush(ctx, emit); err != nil {
			return err
		}
	}
	if uint64(len(c.keyData))+uint64(len(key)) > math.MaxUint32 || uint64(len(c.endData))+uint64(len(end)) > math.MaxUint32 {
		return columnar.ErrBatchBudget
	}
	if c.builder != nil {
		err := c.builder.AppendVectors(ctx, c.vectors, c.lanes)
		if errors.Is(err, columnar.ErrBatchBudget) && c.rows() > 0 {
			if flushErr := c.flush(ctx, emit); flushErr != nil {
				return flushErr
			}
			err = c.builder.AppendVectors(ctx, c.vectors, c.lanes)
		}
		if err != nil {
			return err
		}
	}
	c.keyRanges = append(c.keyRanges, columnar.Range{Offset: uint32(len(c.keyData)), Length: uint32(len(key))})
	c.keyData = append(c.keyData, key...)
	c.endRanges = append(c.endRanges, columnar.Range{Offset: uint32(len(c.endData)), Length: uint32(len(end))})
	c.endData = append(c.endData, end...)
	if kind == storage.MutationDeleteRange {
		c.hasEndKey = true
	}
	c.commitTS = append(c.commitTS, ts)
	c.ordinals = append(c.ordinals, ordinal)
	c.kinds = append(c.kinds, kind)
	c.historyUntil = max(c.historyUntil, until)
	c.dataBytes += rowBytes
	return nil
}

func estimateVectorRowBytes(v *columnar.Vector, row int) uint64 {
	if v.IsNull(row) {
		return 1
	}
	switch v.Type.Storage {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean, model.TypeFloat64:
		return 8
	case model.TypeInterval:
		return 16
	case model.TypeBytes, model.TypeString, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		idx := row
		if v.Encoding == columnar.Dictionary {
			idx = int(v.Indices[row])
		}
		n := uint64(v.Ranges[idx].Length)
		if n > toastThreshold {
			return 13
		}
		return 4 + n
	case model.TypeVectorF32:
		return 8 + uint64(v.Ranges[row].Length)*4
	case model.TypeArray:
		r := v.Ranges[row]
		total := uint64(16) + uint64(v.ShapeRanges[row].Length)*8
		if len(v.Children) == 1 {
			for i := uint32(0); i < r.Length; i++ {
				total += estimateVectorRowBytes(&v.Children[0], int(r.Offset+i))
			}
		}
		return total
	case model.TypeComposite:
		var total uint64
		for i := range v.Children {
			total += estimateVectorRowBytes(&v.Children[i], row)
		}
		return total
	default:
		return 16
	}
}

func (c *paxBlockCoalescer) flush(ctx context.Context, emit coalescedBlockEmit) error {
	count := len(c.commitTS)
	if count == 0 {
		return nil
	}
	payload := columnar.Batch{Length: count}
	if c.builder != nil {
		if err := c.builder.Publish(&payload); err != nil {
			return err
		}
	}
	keyType := model.Type{ID: 17, Storage: model.TypeBytes}
	versions := blockVersions{
		Keys:     columnar.Vector{Type: keyType, Length: count, Data: c.keyData, Ranges: c.keyRanges},
		CommitTS: c.commitTS,
		Ordinals: c.ordinals,
		Kinds:    c.kinds,
	}
	if c.hasEndKey {
		versions.EndKeys = columnar.Vector{Type: keyType, Length: count, Data: c.endData, Ranges: c.endRanges}
	}
	if err := emitCoalescedBatch(ctx, c.target, versions, &payload, c.columnIDs, c.limits, c.historyUntil, emit); err != nil {
		return err
	}
	if c.builder != nil {
		c.builder.Reset()
	}
	c.keyData = c.keyData[:0]
	c.keyRanges = c.keyRanges[:0]
	c.endData = c.endData[:0]
	c.endRanges = c.endRanges[:0]
	c.commitTS = c.commitTS[:0]
	c.ordinals = c.ordinals[:0]
	c.kinds = c.kinds[:0]
	c.hasEndKey = false
	c.historyUntil = 0
	c.dataBytes = 0
	return nil
}

func (c *paxBlockCoalescer) Close() error {
	var err error
	if c.builder != nil {
		err = c.builder.Close()
		c.builder = nil
	}
	c.initialized = false
	c.columnIDs = nil
	c.decoded = nil
	c.vectors = nil
	c.lanes = nil
	c.keyData = nil
	c.keyRanges = nil
	c.endData = nil
	c.endRanges = nil
	c.commitTS = nil
	c.ordinals = nil
	c.kinds = nil
	c.hasEndKey = false
	c.historyUntil = 0
	return err
}

func emitCoalescedBatch(
	ctx context.Context,
	target storage.ObjectRef,
	versions blockVersions,
	payload *columnar.Batch,
	columnIDs []model.ColumnID,
	limits blockLimits,
	historyUntil model.Timestamp,
	emit coalescedBlockEmit,
) error {
	encoded, blobs, err := encodeBlockWithBlobs(ctx, target, versions, payload, columnIDs, limits)
	if errors.Is(err, columnar.ErrBatchBudget) && payload.Length > 1 {
		mid := payload.Length / 2
		left, err := sliceBatchForSplit(ctx, payload, 0, mid, limits.MaxDecodedBytes)
		if err != nil {
			return err
		}
		if err := emitCoalescedBatch(ctx, target, sliceBlockVersions(versions, 0, mid), &left, columnIDs, limits, historyUntil, emit); err != nil {
			return err
		}
		right, err := sliceBatchForSplit(ctx, payload, mid, payload.Length, limits.MaxDecodedBytes)
		if err != nil {
			return err
		}
		return emitCoalescedBatch(ctx, target, sliceBlockVersions(versions, mid, payload.Length), &right, columnIDs, limits, historyUntil, emit)
	}
	if err != nil {
		return err
	}
	return emit(target, encoded, blobs, uint64(payload.Length), historyUntil)
}

func sliceBatchForSplit(ctx context.Context, payload *columnar.Batch, start, end int, maxBytes uint64) (columnar.Batch, error) {
	if len(payload.Columns) == 0 {
		return columnar.Batch{Length: end - start}, nil
	}
	order := make([]uint32, end-start)
	for i := range order {
		order[i] = uint32(start + i)
	}
	sliced, _, err := gatherDecodedBlock(ctx, payload, order, maxBytes)
	return sliced, err
}

func sliceBlockVersions(versions blockVersions, start, end int) blockVersions {
	length := end - start
	sub := blockVersions{
		Keys:     columnar.Vector{Type: versions.Keys.Type, Length: length, Data: versions.Keys.Data, Ranges: versions.Keys.Ranges[start:end]},
		CommitTS: versions.CommitTS[start:end],
		Ordinals: versions.Ordinals[start:end],
		Kinds:    versions.Kinds[start:end],
	}
	if !encAbsentVector(&versions.EndKeys) {
		for _, kind := range sub.Kinds {
			if kind == storage.MutationDeleteRange {
				sub.EndKeys = columnar.Vector{Type: versions.EndKeys.Type, Length: length, Data: versions.EndKeys.Data, Ranges: versions.EndKeys.Ranges[start:end]}
				break
			}
		}
	}
	return sub
}
