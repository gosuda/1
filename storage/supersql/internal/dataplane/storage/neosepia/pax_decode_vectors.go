package neosepia

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// This file implements lazy typed decoding of every PAX v2 node body. All
// borrowed output aliases the block bytes and stays valid until block Close.
// Every freshly allocated buffer is charged against MaxDecodedBytes before it
// is allocated; malformed counts never allocate.

// decNode decodes one validated descriptor whose payload starts at absolute
// block offset abs. Nested payloads must stay inside their parent; the root
// call passes the whole block (parentEnd == len(data)).
func (b *paxBlock) decNode(ctx context.Context, desc *decDescriptor, abs uint64, parentEnd uint64, depth int) (columnar.Vector, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	if depth > 64 {
		return columnar.Vector{}, paxCorrupt("nested column depth exceeds 64")
	}
	rows := int(desc.rows)
	if uint64(rows) != uint64(desc.rows) {
		return columnar.Vector{}, paxCorrupt("row count overflows int")
	}
	if desc.payload != abs || desc.length > parentEnd || abs > parentEnd-desc.length {
		return columnar.Vector{}, paxCorrupt("column payload outside parent")
	}
	payload := b.data[abs : abs+desc.length]
	var nulls []byte
	rest := payload
	if desc.nulls > 0 {
		need := (rows + 7) / 8
		if len(rest) < need {
			return columnar.Vector{}, paxCorrupt("null bitmap overflows payload")
		}
		nulls = rest[:need]
		if err := verifyPAXNulls(nulls, rows, desc.nulls); err != nil {
			return columnar.Vector{}, err
		}
		rest = rest[need:]
	}
	base := columnar.Vector{Type: desc.typ, Encoding: columnar.Flat, Length: rows, Nulls: nulls}
	switch desc.storage {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		return b.decIntegers(ctx, desc, &base, rest)
	case model.TypeFloat64:
		return b.decFloat64s(ctx, desc, &base, rest)
	case model.TypeInterval:
		return b.decIntervals(ctx, desc, &base, rest)
	case model.TypeString, model.TypeBytes, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		return b.decByteStrings(ctx, desc, &base, rest)
	case model.TypeVectorF32:
		return b.decVectorF32(ctx, desc, &base, rest)
	case model.TypeArray:
		return b.decArray(ctx, desc, &base, payload, abs, depth)
	case model.TypeComposite:
		return b.decComposite(ctx, desc, &base, payload, abs, depth)
	default:
		return columnar.Vector{}, paxCorrupt("invalid storage type %d", uint8(desc.storage))
	}
}

func (b *paxBlock) decIntegers(ctx context.Context, desc *decDescriptor, base *columnar.Vector, body []byte) (columnar.Vector, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	rows := base.Length
	var vals []int64
	if desc.encoding == encodingPlain {
		if borrowed, ok := borrowPAXInt64(body); ok && len(borrowed) == rows {
			vals = borrowed
		}
	}
	if vals == nil {
		if err := b.chargeLocked(uint64(rows) * 8); err != nil {
			return columnar.Vector{}, err
		}
		vals = make([]int64, rows)
		if err := decodeIntegers(body, desc.encoding, vals); err != nil {
			return columnar.Vector{}, fmtWrapPAX(err)
		}
	}
	if desc.storage == model.TypeBoolean {
		for i := range rows {
			if i%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return columnar.Vector{}, err
				}
			}
			if paxIsNull(base.Nulls, i) {
				continue
			}
			if vals[i] != 0 && vals[i] != 1 {
				return columnar.Vector{}, paxCorrupt("boolean lane %d holds %d", i, vals[i])
			}
		}
	}
	base.Int64 = vals
	return *base, nil
}

func (b *paxBlock) decFloat64s(ctx context.Context, desc *decDescriptor, base *columnar.Vector, body []byte) (columnar.Vector, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	rows := base.Length
	if uint64(len(body)) != uint64(rows)*8 {
		return columnar.Vector{}, paxCorrupt("float64 body %d bytes, want %d", len(body), rows*8)
	}
	var vals []float64
	vals = borrowPAXFloat64(body, rows)
	if vals == nil {
		if err := b.chargeLocked(uint64(rows) * 8); err != nil {
			return columnar.Vector{}, err
		}
		vals = make([]float64, rows)
		for i := range rows {
			vals[i] = math.Float64frombits(binary.LittleEndian.Uint64(body[i*8:]))
		}
	}
	base.Float64 = vals
	return *base, nil
}

func (b *paxBlock) decIntervals(ctx context.Context, desc *decDescriptor, base *columnar.Vector, body []byte) (columnar.Vector, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	rows := base.Length
	if uint64(len(body)) != uint64(rows)*16 {
		return columnar.Vector{}, paxCorrupt("interval body %d bytes, want %d", len(body), rows*16)
	}
	if err := b.chargeLocked(uint64(rows) * 16); err != nil {
		return columnar.Vector{}, err
	}
	vals := make([]model.Interval, rows)
	for i := range rows {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return columnar.Vector{}, err
			}
		}
		off := i * 16
		vals[i] = model.Interval{
			Months:       int32(binary.LittleEndian.Uint32(body[off:])),
			Days:         int32(binary.LittleEndian.Uint32(body[off+4:])),
			Microseconds: int64(binary.LittleEndian.Uint64(body[off+8:])),
		}
	}
	base.Intervals = vals
	return *base, nil
}

// decByteStrings decodes plain length-prefixed entries and the legacy
// dictionary layout. Plain entries borrow the whole body as Data with Range
// views into it; dictionary entries borrow the entry region the same way and
// allocate only Ranges and per-lane codes.
func (b *paxBlock) decByteStrings(ctx context.Context, desc *decDescriptor, base *columnar.Vector, body []byte) (columnar.Vector, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	if desc.encoding == encodingDictionary {
		return b.decDictionary(ctx, desc, base, body)
	}
	rows := base.Length
	if uint64(len(body)) > math.MaxUint32 {
		return columnar.Vector{}, paxCorrupt("string body exceeds uint32 range views")
	}
	if err := b.chargeLocked(uint64(rows) * 8); err != nil {
		return columnar.Vector{}, err
	}
	ranges := make([]columnar.Range, rows)
	pos := 0
	for i := range rows {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return columnar.Vector{}, err
			}
		}
		if len(body)-pos < 4 {
			return columnar.Vector{}, paxCorrupt("string row %d truncated length", i)
		}
		length := binary.LittleEndian.Uint32(body[pos:])
		pos += 4
		if uint64(length) > uint64(len(body)-pos) {
			return columnar.Vector{}, paxCorrupt("string row %d length %d overflows body", i, length)
		}
		if paxIsNull(base.Nulls, i) && length != 0 {
			return columnar.Vector{}, paxCorrupt("string row %d NULL with %d payload bytes", i, length)
		}
		ranges[i] = columnar.Range{Offset: uint32(pos), Length: length}
		pos += int(length)
	}
	if pos != len(body) {
		return columnar.Vector{}, paxCorrupt("string body has %d trailing bytes", len(body)-pos)
	}
	base.Data = body
	base.Ranges = ranges
	return *base, nil
}

func (b *paxBlock) decDictionary(ctx context.Context, desc *decDescriptor, base *columnar.Vector, body []byte) (columnar.Vector, error) {
	rows := base.Length
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	if len(body) < 9 {
		return columnar.Vector{}, paxCorrupt("dictionary body %d bytes shorter than header", len(body))
	}
	dictCount := uint64(binary.LittleEndian.Uint32(body[0:]))
	total := binary.LittleEndian.Uint32(body[4:])
	width := int(body[8])
	if total != desc.rows {
		return columnar.Vector{}, paxCorrupt("dictionary total %d != node rows %d", total, desc.rows)
	}
	if width < 1 || width > 32 {
		return columnar.Vector{}, paxCorrupt("dictionary bit width %d out of [1,32]", width)
	}
	if dictCount > uint64(len(body))/4 {
		return columnar.Vector{}, paxCorrupt("dictionary count %d exceeds body", dictCount)
	}
	if err := b.chargeLocked(dictCount*8 + uint64(rows)*4); err != nil {
		return columnar.Vector{}, err
	}
	off := 9
	for i := uint64(0); i < dictCount; i++ {
		if len(body)-off < 4 {
			return columnar.Vector{}, paxCorrupt("dictionary entry %d truncated length", i)
		}
		length := binary.LittleEndian.Uint32(body[off:])
		off += 4
		if uint64(length) > uint64(len(body)-off) {
			return columnar.Vector{}, paxCorrupt("dictionary entry %d length %d overflows body", i, length)
		}
		off += int(length)
	}
	entryStart, entryEnd := 9, off
	if uint64(entryEnd-9) > math.MaxUint32 {
		return columnar.Vector{}, paxCorrupt("dictionary entries exceed uint32 range views")
	}
	codes := body[off:]
	if uint64(len(codes)) != (uint64(rows)*uint64(width)+7)/8 {
		return columnar.Vector{}, paxCorrupt("dictionary codes %d bytes, want %d", len(codes), (uint64(rows)*uint64(width)+7)/8)
	}
	dictRanges := make([]columnar.Range, dictCount)
	pos := entryStart
	for i := range dictRanges {
		length := binary.LittleEndian.Uint32(body[pos:])
		pos += 4
		dictRanges[i] = columnar.Range{Offset: uint32(pos - entryStart), Length: length}
		pos += int(length)
	}
	indices := make([]uint32, rows)
	bit := 0
	for i := range rows {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return columnar.Vector{}, err
			}
		}
		var code uint64
		for remaining, shift := width, 0; remaining > 0; {
			byteIdx := bit / 8
			offset := bit % 8
			n := min(remaining, 8-offset)
			part := uint64(codes[byteIdx]>>offset) & ((uint64(1) << uint(n)) - 1)
			code |= part << uint(shift)
			bit += n
			shift += n
			remaining -= n
		}
		if !paxIsNull(base.Nulls, i) && code >= dictCount {
			return columnar.Vector{}, paxCorrupt("row %d dictionary code %d >= %d entries", i, code, dictCount)
		}
		indices[i] = uint32(code)
	}
	base.Encoding = columnar.Dictionary
	base.Data = body[entryStart:entryEnd]
	base.Ranges = dictRanges
	base.Indices = indices
	return *base, nil
}

func (b *paxBlock) decVectorF32(ctx context.Context, desc *decDescriptor, base *columnar.Vector, body []byte) (columnar.Vector, error) {
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	rows := base.Length
	if uint64(len(body)) < uint64(rows)*8+4 {
		return columnar.Vector{}, paxCorrupt("float-vector body %d bytes too short for %d rows", len(body), rows)
	}
	if err := b.chargeLocked(uint64(rows) * 8); err != nil {
		return columnar.Vector{}, err
	}
	ranges := make([]columnar.Range, rows)
	for i := range rows {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return columnar.Vector{}, err
			}
		}
		ranges[i] = columnar.Range{
			Offset: binary.LittleEndian.Uint32(body[i*8:]),
			Length: binary.LittleEndian.Uint32(body[i*8+4:]),
		}
	}
	elemOff := rows * 8
	elemCount := uint64(binary.LittleEndian.Uint32(body[elemOff:]))
	elems := body[elemOff+4:]
	if uint64(len(elems)) != elemCount*4 {
		return columnar.Vector{}, paxCorrupt("float-vector elements %d bytes, want %d", len(elems), elemCount*4)
	}
	for i := range rows {
		span := ranges[i]
		if uint64(span.Offset)+uint64(span.Length) > elemCount {
			return columnar.Vector{}, paxCorrupt("row %d float-vector range exceeds %d elements", i, elemCount)
		}
		if paxIsNull(base.Nulls, i) && span.Length != 0 {
			return columnar.Vector{}, paxCorrupt("row %d NULL float-vector with %d elements", i, span.Length)
		}
	}
	var flat []float32
	flat = borrowPAXFloat32(elems, int(elemCount))
	if flat == nil {
		if err := b.chargeLocked(elemCount * 4); err != nil {
			return columnar.Vector{}, err
		}
		flat = make([]float32, elemCount)
		for i := range flat {
			flat[i] = math.Float32frombits(binary.LittleEndian.Uint32(elems[i*4:]))
		}
	}
	base.Ranges = ranges
	base.Float32 = flat
	return *base, nil
}

// decArray decodes value/shape ranges, the flattened dimension list, then the
// single nested child descriptor and payload that follow 8-byte padding.
func (b *paxBlock) decArray(ctx context.Context, desc *decDescriptor, base *columnar.Vector, payload []byte, abs uint64, depth int) (columnar.Vector, error) {
	rows := base.Length
	bitmapLen := len(base.Nulls)
	if uint64(len(payload)) < uint64(bitmapLen)+uint64(rows)*8*2+4 {
		return columnar.Vector{}, paxCorrupt("array header overflows payload")
	}
	fixed := payload[bitmapLen:]
	if err := b.chargeLocked(uint64(rows) * 16); err != nil {
		return columnar.Vector{}, err
	}
	values := make([]columnar.Range, rows)
	shapes := make([]columnar.Range, rows)
	for i := range rows {
		values[i] = columnar.Range{
			Offset: binary.LittleEndian.Uint32(fixed[i*8:]),
			Length: binary.LittleEndian.Uint32(fixed[i*8+4:]),
		}
		off := rows*8 + i*8
		shapes[i] = columnar.Range{
			Offset: binary.LittleEndian.Uint32(fixed[off:]),
			Length: binary.LittleEndian.Uint32(fixed[off+4:]),
		}
	}
	dimOff := rows * 8 * 2
	dimCount := binary.LittleEndian.Uint32(fixed[dimOff:])
	if uint64(dimCount) > (uint64(len(fixed))-uint64(dimOff)-4)/8 {
		return columnar.Vector{}, paxCorrupt("array dimension count %d overflows payload", dimCount)
	}
	if err := b.chargeLocked(uint64(dimCount)*8 + uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
		return columnar.Vector{}, err
	}
	dims := make([]model.ArrayDimension, dimCount)
	for i := range dims {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return columnar.Vector{}, err
			}
		}
		off := dimOff + 4 + i*8
		dims[i] = model.ArrayDimension{
			Length:     int32(binary.LittleEndian.Uint32(fixed[off:])),
			LowerBound: int32(binary.LittleEndian.Uint32(fixed[off+4:])),
		}
	}
	tableAbs := aligned8(abs + uint64(bitmapLen) + uint64(dimOff) + 4 + uint64(dimCount)*8)
	tableOff := tableAbs
	if tableOff < abs || tableOff+uint64(paxColumnSize) > abs+uint64(len(payload)) {
		return columnar.Vector{}, paxCorrupt("array child table outside payload")
	}
	for _, pad := range b.data[abs+uint64(bitmapLen)+uint64(dimOff)+4+uint64(dimCount)*8 : tableOff] {
		if pad != 0 {
			return columnar.Vector{}, paxCorrupt("nonzero array header padding")
		}
	}
	childOff := tableOff + uint64(paxColumnSize)
	childDesc, err := parsePAXDescriptor(b.data[tableOff:tableOff+uint64(paxColumnSize)], true)
	if err != nil {
		return columnar.Vector{}, err
	}
	payloadEnd := abs + uint64(len(payload))
	if childDesc.payload != childOff || childOff > payloadEnd || childDesc.length != payloadEnd-childOff {
		return columnar.Vector{}, paxCorrupt("array child payload leaves trailing or overlapping bytes")
	}
	child, err := b.decNode(ctx, &childDesc, childOff, childOff+childDesc.length, depth+1)
	if err != nil {
		return columnar.Vector{}, err
	}
	childRows := uint64(child.Length)
	for i := range rows {
		if i%1024 == 0 {
			if err := ctx.Err(); err != nil {
				return columnar.Vector{}, err
			}
		}
		span, shape := values[i], shapes[i]
		if uint64(span.Offset)+uint64(span.Length) > childRows {
			return columnar.Vector{}, paxCorrupt("row %d array range exceeds %d child lanes", i, childRows)
		}
		if uint64(shape.Offset)+uint64(shape.Length) > uint64(dimCount) {
			return columnar.Vector{}, paxCorrupt("row %d array shape exceeds %d dimensions", i, dimCount)
		}
		if paxIsNull(base.Nulls, i) {
			if span.Length != 0 || shape.Length != 0 {
				return columnar.Vector{}, paxCorrupt("row %d NULL array with payload ranges", i)
			}
			continue
		}
		if err := model.ValidateArrayShape(dims[shape.Offset:uint64(shape.Offset)+uint64(shape.Length)], uint64(span.Length)); err != nil {
			return columnar.Vector{}, fmt.Errorf("%w: row %d: %w", errPAXBlock, i, err)
		}
	}
	base.Ranges = values
	base.ShapeRanges = shapes
	base.Dimensions = dims
	base.Children = []columnar.Vector{child}
	return *base, nil
}

// decComposite decodes the child descriptor table after 8-byte padding, then
// each child payload back to back with no trailing bytes.
func (b *paxBlock) decComposite(ctx context.Context, desc *decDescriptor, base *columnar.Vector, payload []byte, abs uint64, depth int) (columnar.Vector, error) {
	rows := base.Length
	tableOff := aligned8(abs + uint64(len(base.Nulls)))
	if tableOff < abs || tableOff%8 != 0 || tableOff > abs+uint64(len(payload)) {
		return columnar.Vector{}, paxCorrupt("composite child table outside payload")
	}
	for _, pad := range payload[len(base.Nulls) : tableOff-abs] {
		if pad != 0 {
			return columnar.Vector{}, paxCorrupt("nonzero composite header padding")
		}
	}
	tableSize := uint64(desc.children) * uint64(paxColumnSize)
	if tableSize > uint64(len(payload))-(tableOff-abs) {
		return columnar.Vector{}, paxCorrupt("composite child table overflows payload")
	}
	if err := b.chargeLocked(uint64(desc.children) * uint64(unsafe.Sizeof(columnar.Vector{}))); err != nil {
		return columnar.Vector{}, err
	}
	children := make([]columnar.Vector, desc.children)
	next := tableOff + tableSize
	payloadEnd := abs + uint64(len(payload))
	for i := range children {
		if err := ctx.Err(); err != nil {
			return columnar.Vector{}, err
		}
		rec := b.data[tableOff+uint64(i)*uint64(paxColumnSize):]
		childDesc, err := parsePAXDescriptor(rec[:paxColumnSize], true)
		if err != nil {
			return columnar.Vector{}, err
		}
		if childDesc.payload != next || next > payloadEnd || childDesc.length > payloadEnd-next {
			return columnar.Vector{}, paxCorrupt("composite child %d payload outside parent", i)
		}
		child, err := b.decNode(ctx, &childDesc, next, next+childDesc.length, depth+1)
		if err != nil {
			return columnar.Vector{}, err
		}
		if child.Length != rows {
			return columnar.Vector{}, paxCorrupt("composite child %d has %d rows, want %d", i, child.Length, rows)
		}
		next += childDesc.length
		if i+1 < len(children) {
			aligned := aligned8(next)
			if aligned > payloadEnd {
				return columnar.Vector{}, paxCorrupt("composite child padding exceeds parent")
			}
			for _, pad := range b.data[next:aligned] {
				if pad != 0 {
					return columnar.Vector{}, paxCorrupt("nonzero composite child padding")
				}
			}
			next = aligned
		}
		children[i] = child
	}
	if next != payloadEnd {
		return columnar.Vector{}, paxCorrupt("composite children leave %d trailing bytes", payloadEnd-next)
	}
	base.Children = children
	return *base, nil
}
