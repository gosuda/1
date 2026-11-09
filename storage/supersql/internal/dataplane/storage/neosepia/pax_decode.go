package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// errColumnMissing reports a column ID absent from the block. It is distinct
// from corruption: the block is valid, the column is simply not stored.
var errColumnMissing = errors.New("pax column missing from block")

var errVisibilityKeyBounds = errors.New("key slice outside visibility region")
var errVisibilityKeyOverlap = errors.New("key slice overlaps record table")

var paxNativeLE = func() bool {
	var v uint16 = 1
	return *(*byte)(unsafe.Pointer(&v)) == 1
}()

// decDescriptor is one validated 64-byte column descriptor.
type decDescriptor struct {
	id       model.ColumnID
	storage  model.DataType
	encoding columnEncoding
	flags    uint16
	rows     uint32
	nulls    uint32
	typ      model.Type
	children uint32
	payload  uint64
	length   uint64
	min      int64
	max      int64
}

// paxBlock is a validated PAX v2 block retaining immutable bytes until Close.
// Columns decode lazily on first Column call and are cached; visibility is
// available without decoding any payload.
type paxBlock struct {
	mu           sync.Mutex
	data         []byte
	memory       *sourceMemory
	encodedBytes uint64
	target       storage.ObjectRef
	rows         int
	columns      int
	descriptors  []byte
	vis          []byte
	budget       uint64
	used         uint64
	cache        []cachedPAXColumn
	closed       bool
	zonePruned   bool
}

type cachedPAXColumn struct {
	id     model.ColumnID
	vector columnar.Vector
}

func paxCorrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errPAXBlock, fmt.Sprintf(format, args...))
}

func fmtWrapPAX(err error) error {
	return errors.Join(errPAXBlock, err)
}

func openBlock(data []byte, limits blockLimits) (*paxBlock, error) {
	if limits.MaxRows <= 0 || limits.MaxBytes == 0 || limits.MaxDecodedBytes == 0 {
		return nil, columnar.ErrBatchBudget
	}
	if uint64(len(data)) > limits.MaxBytes {
		return nil, columnar.ErrBatchBudget
	}
	if len(data) < paxHeaderSize {
		return nil, paxCorrupt("block %d bytes shorter than %d-byte header", len(data), paxHeaderSize)
	}
	if magic := binary.LittleEndian.Uint32(data[0:]); magic != paxMagic {
		return nil, paxCorrupt("bad magic %#x", magic)
	}
	if version := binary.LittleEndian.Uint32(data[4:]); version != paxVersion {
		return nil, paxCorrupt("unsupported format version %d", version)
	}
	rows := uint64(binary.LittleEndian.Uint32(data[8:]))
	columns := uint64(binary.LittleEndian.Uint32(data[12:]))
	if rows == 0 {
		return nil, paxCorrupt("empty block")
	}
	if rows > uint64(limits.MaxRows) {
		return nil, columnar.ErrBatchBudget
	}
	target := storage.ObjectRef{
		ObjectID:          model.ObjectID(binary.LittleEndian.Uint64(data[16:])),
		Generation:        binary.LittleEndian.Uint64(data[24:]),
		DescriptorVersion: binary.LittleEndian.Uint64(data[32:]),
		EncodingVersion:   binary.LittleEndian.Uint32(data[40:]),
	}
	if err := target.Validate(); err != nil {
		return nil, fmt.Errorf("%w: invalid target: %w", errPAXBlock, err)
	}
	visLen := uint64(binary.LittleEndian.Uint32(data[44:]))
	blockLen := binary.LittleEndian.Uint64(data[48:])
	if blockLen != uint64(len(data)) {
		return nil, paxCorrupt("header block length %d != %d actual bytes", blockLen, len(data))
	}
	if reserved := binary.LittleEndian.Uint32(data[60:]); reserved != 0 {
		return nil, paxCorrupt("nonzero header reserved field")
	}
	if columns > (math.MaxUint64-paxHeaderSize)/paxColumnSize {
		return nil, paxCorrupt("column count %d overflows descriptor table", columns)
	}
	descEnd := uint64(paxHeaderSize) + columns*uint64(paxColumnSize)
	recordsSize := rows * uint64(paxVersionSize)
	if visLen < recordsSize || visLen > blockLen || descEnd > blockLen-visLen {
		return nil, paxCorrupt("visibility region exceeds block")
	}
	payloadBase := aligned8(descEnd + visLen)
	if payloadBase > blockLen {
		return nil, paxCorrupt("visibility padding exceeds block")
	}
	if got := paxChecksum(data); got != binary.LittleEndian.Uint32(data[56:]) {
		return nil, paxCorrupt("checksum mismatch")
	}
	b := &paxBlock{
		data:        data,
		target:      target,
		rows:        int(rows),
		budget:      limits.MaxDecodedBytes,
		columns:     int(columns),
		descriptors: data[paxHeaderSize:descEnd],
		vis:         data[descEnd : descEnd+visLen],
	}
	if err := b.chargeLocked(uint64(unsafe.Sizeof(paxBlock{}))); err != nil {
		return nil, err
	}
	prevEnd := payloadBase
	var previousColumn model.ColumnID
	for i := uint64(0); i < columns; i++ {
		rec := data[uint64(paxHeaderSize)+i*uint64(paxColumnSize):]
		desc, err := parsePAXDescriptor(rec, false)
		if err != nil {
			return nil, err
		}
		if i > 0 && desc.id <= previousColumn {
			return nil, paxCorrupt("root column IDs not strictly ascending")
		}
		if uint64(desc.rows) != rows {
			return nil, paxCorrupt("root column %d has %d rows, want %d", desc.id, desc.rows, rows)
		}
		if desc.payload < payloadBase || desc.length > blockLen || desc.payload > blockLen-desc.length {
			return nil, paxCorrupt("root column %d payload outside block", desc.id)
		}
		if desc.payload < prevEnd {
			return nil, paxCorrupt("root column %d payload overlaps predecessor", desc.id)
		}
		for _, pad := range data[prevEnd:desc.payload] {
			if pad != 0 {
				return nil, paxCorrupt("nonzero padding before root column %d", desc.id)
			}
		}
		prevEnd = aligned8(desc.payload + desc.length)
		if prevEnd < desc.payload || prevEnd > blockLen {
			return nil, paxCorrupt("root column %d payload end overflows block", desc.id)
		}
		previousColumn = desc.id
	}
	visRegion := data[descEnd : descEnd+visLen]
	var prevKey []byte
	var prevTS int64
	var prevOrdinal uint64
	for i := uint64(0); i < rows; i++ {
		rec := visRegion[i*uint64(paxVersionSize):]
		ts := int64(binary.LittleEndian.Uint64(rec[0:]))
		if ts <= 0 {
			return nil, paxCorrupt("row %d has non-positive commit timestamp", i)
		}
		ordinal := binary.LittleEndian.Uint64(rec[8:])
		if ordinal == 0 {
			return nil, paxCorrupt("row %d has zero ordinal", i)
		}
		op := storage.MutationOp(rec[16])
		switch op {
		case storage.MutationUpsert, storage.MutationDelete, storage.MutationDeleteRange:
		default:
			return nil, paxCorrupt("row %d has non-normalized op %d", i, uint8(op))
		}
		if !isZeroBytes(rec[17:24]) {
			return nil, paxCorrupt("row %d has nonzero visibility reserved bytes", i)
		}
		keyOff := uint64(binary.LittleEndian.Uint32(rec[24:]))
		keyLen := uint64(binary.LittleEndian.Uint32(rec[28:]))
		endOff := uint64(binary.LittleEndian.Uint32(rec[32:]))
		endLen := uint64(binary.LittleEndian.Uint32(rec[36:]))
		key, err := sliceVisKey(visRegion, recordsSize, keyOff, keyLen)
		if err != nil {
			return nil, paxCorrupt("row %d key: %v", i, err)
		}
		end, err := sliceVisKey(visRegion, recordsSize, endOff, endLen)
		if err != nil {
			return nil, paxCorrupt("row %d end key: %v", i, err)
		}
		if op == storage.MutationDeleteRange {
			if len(key) == 0 || len(end) == 0 {
				return nil, paxCorrupt("row %d range delete has empty bound", i)
			}
			if bytes.Compare(key, end) >= 0 {
				return nil, paxCorrupt("row %d range delete has unordered bounds", i)
			}
		} else if len(end) != 0 {
			return nil, paxCorrupt("row %d point mutation has nonempty end key", i)
		}
		comparison := bytes.Compare(key, prevKey)
		if i > 0 && comparison < 0 {
			return nil, paxCorrupt("row %d key out of order", i)
		}
		if i > 0 && comparison == 0 {
			if ts > prevTS || (ts == prevTS && ordinal >= prevOrdinal) {
				return nil, paxCorrupt("row %d duplicates key/version/ordinal order", i)
			}
		}
		prevKey, prevTS, prevOrdinal = key, ts, ordinal
	}
	for _, pad := range data[descEnd+visLen : payloadBase] {
		if pad != 0 {
			return nil, paxCorrupt("nonzero visibility padding")
		}
	}
	for _, pad := range data[prevEnd:] {
		if pad != 0 {
			return nil, paxCorrupt("nonzero trailing block padding")
		}
	}
	return b, nil
}

func isZeroBytes(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func sliceVisKey(region []byte, tableSize, off, length uint64) ([]byte, error) {
	if length > uint64(len(region)) || off > uint64(len(region))-length {
		return nil, errVisibilityKeyBounds
	}
	if length > 0 && off < tableSize {
		return nil, errVisibilityKeyOverlap
	}
	return region[off : off+length], nil
}

// parsePAXDescriptor validates one 64-byte descriptor record. Root records
// require a nonzero column ID; nested records require zero.
func parsePAXDescriptor(rec []byte, nested bool) (decDescriptor, error) {
	var d decDescriptor
	id := binary.LittleEndian.Uint32(rec[0:])
	if nested {
		if id != 0 {
			return d, paxCorrupt("nested descriptor has nonzero column ID %d", id)
		}
	} else if id == 0 {
		return d, paxCorrupt("root descriptor has zero column ID")
	}
	d.id = model.ColumnID(id)
	st := model.DataType(rec[4])
	if !st.Valid() {
		return d, paxCorrupt("invalid storage type %d", uint8(st))
	}
	d.storage = st
	d.encoding = columnEncoding(rec[5])
	if !allowedPAXEncoding(st, d.encoding) {
		return d, paxCorrupt("storage type %d rejects encoding %d", uint8(st), uint8(d.encoding))
	}
	d.flags = binary.LittleEndian.Uint16(rec[6:])
	if d.flags&^paxZoneInteger != 0 {
		return d, paxCorrupt("unknown column flags %#x", d.flags)
	}
	d.rows = binary.LittleEndian.Uint32(rec[8:])
	d.nulls = binary.LittleEndian.Uint32(rec[12:])
	if uint64(d.nulls) > uint64(d.rows) {
		return d, paxCorrupt("null count %d exceeds row count %d", d.nulls, d.rows)
	}
	typeID := binary.LittleEndian.Uint32(rec[16:])
	if typeID == 0 {
		return d, paxCorrupt("zero SQL type ID")
	}
	d.typ = model.Type{
		ID:        model.TypeID(typeID),
		Modifier:  int32(binary.LittleEndian.Uint32(rec[20:])),
		Collation: model.CollationID(binary.LittleEndian.Uint32(rec[24:])),
		Storage:   st,
	}
	d.children = binary.LittleEndian.Uint32(rec[28:])
	switch st {
	case model.TypeArray:
		if d.children != 1 {
			return d, paxCorrupt("array node has %d children, want 1", d.children)
		}
	case model.TypeComposite:
	default:
		if d.children != 0 {
			return d, paxCorrupt("scalar node has %d children", d.children)
		}
	}
	d.payload = binary.LittleEndian.Uint64(rec[32:])
	d.length = binary.LittleEndian.Uint64(rec[40:])
	if d.payload%8 != 0 {
		return d, paxCorrupt("payload offset %d not 8-aligned", d.payload)
	}
	d.min = int64(binary.LittleEndian.Uint64(rec[48:]))
	d.max = int64(binary.LittleEndian.Uint64(rec[56:]))
	if d.flags&paxZoneInteger != 0 {
		switch st {
		case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		default:
			return d, paxCorrupt("zone flag on non-integer storage type %d", uint8(st))
		}
		if d.nulls == d.rows {
			return d, paxCorrupt("zone flag with no non-NULL values")
		}
		if d.min > d.max {
			return d, paxCorrupt("inverted zone bounds")
		}
	}
	return d, nil
}

func allowedPAXEncoding(st model.DataType, enc columnEncoding) bool {
	switch st {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		return enc == encodingPlain || enc == encodingRLE || enc == encodingDelta
	case model.TypeString, model.TypeBytes, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		return enc == encodingPlain || enc == encodingDictionary
	case model.TypeFloat64, model.TypeInterval, model.TypeVectorF32, model.TypeArray, model.TypeComposite:
		return enc == encodingPlain
	}
	return false
}

func (b *paxBlock) Target() storage.ObjectRef {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.target
}

func (b *paxBlock) Rows() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rows
}

func (b *paxBlock) Version(row int) (key, end []byte, ts model.Timestamp, ordinal uint64, kind storage.MutationOp, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, nil, 0, 0, 0, storage.ErrClosed
	}
	if row < 0 || row >= b.rows {
		return nil, nil, 0, 0, 0, paxCorrupt("version row %d out of range [0,%d)", row, b.rows)
	}
	rec := b.vis[row*paxVersionSize:]
	keyOffset := uint64(binary.LittleEndian.Uint32(rec[24:]))
	keyLength := uint64(binary.LittleEndian.Uint32(rec[28:]))
	endOffset := uint64(binary.LittleEndian.Uint32(rec[32:]))
	endLength := uint64(binary.LittleEndian.Uint32(rec[36:]))
	return b.vis[keyOffset : keyOffset+keyLength], b.vis[endOffset : endOffset+endLength],
		model.Timestamp(int64(binary.LittleEndian.Uint64(rec))),
		binary.LittleEndian.Uint64(rec[8:]), storage.MutationOp(rec[16]), nil
}

func (b *paxBlock) findColumn(id model.ColumnID) int {
	lo, hi := 0, b.columns
	for lo < hi {
		mid := (lo + hi) / 2
		if model.ColumnID(binary.LittleEndian.Uint32(b.descriptors[mid*paxColumnSize:])) < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < b.columns && model.ColumnID(binary.LittleEndian.Uint32(b.descriptors[lo*paxColumnSize:])) == id {
		return lo
	}
	return -1
}

func (b *paxBlock) Zone(id model.ColumnID) (columnZone, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return columnZone{}, storage.ErrClosed
	}
	if i := b.findColumn(id); i >= 0 {
		descriptor, err := parsePAXDescriptor(b.descriptors[i*paxColumnSize:], false)
		if err != nil {
			return columnZone{}, err
		}
		return columnZone{Min: descriptor.min, Max: descriptor.max, Valid: descriptor.flags&paxZoneInteger != 0}, nil
	}
	return columnZone{}, errColumnMissing
}

func uintptrOf(b []byte) uintptr {
	if len(b) == 0 {
		return 0
	}
	return uintptr(unsafe.Pointer(&b[0]))
}

func borrowPAXFloat64(body []byte, rows int) []float64 {
	if len(body) == 0 || len(body) != rows*8 || !paxNativeLE {
		return nil
	}
	if uintptrOf(body)%8 != 0 {
		return nil
	}
	return unsafe.Slice((*float64)(unsafe.Pointer(&body[0])), rows)
}

func borrowPAXFloat32(elems []byte, count int) []float32 {
	if len(elems) == 0 || len(elems) != count*4 || !paxNativeLE {
		return nil
	}
	if uintptrOf(elems)%4 != 0 {
		return nil
	}
	return unsafe.Slice((*float32)(unsafe.Pointer(&elems[0])), count)
}
func (b *paxBlock) Column(ctx context.Context, id model.ColumnID) (columnar.Vector, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return columnar.Vector{}, err
	}
	if b.closed {
		return columnar.Vector{}, storage.ErrClosed
	}
	for i := range b.cache {
		if b.cache[i].id == id {
			return b.cache[i].vector, nil
		}
	}
	i := b.findColumn(id)
	if i < 0 {
		return columnar.Vector{}, errColumnMissing
	}
	descriptor, err := parsePAXDescriptor(b.descriptors[i*paxColumnSize:], false)
	if err != nil {
		return columnar.Vector{}, err
	}
	if err := b.reserveCacheLocked(); err != nil {
		return columnar.Vector{}, err
	}
	before := b.used
	v, err := b.decNode(ctx, &descriptor, descriptor.payload, uint64(len(b.data)), 0)
	if err != nil {
		released := b.used - before
		b.used = before
		b.memory.release(released)
		return columnar.Vector{}, err
	}
	b.cache = append(b.cache, cachedPAXColumn{id: id, vector: v})
	b.memory.observe(storage.ScanObservation{DecodedBytes: storage.ObservationCount{Value: descriptor.length, Known: true}})
	return v, nil
}

func (b *paxBlock) observeZonePruned() {
	b.mu.Lock()
	if b.closed || b.zonePruned {
		b.mu.Unlock()
		return
	}
	b.zonePruned = true
	memory := b.memory
	b.mu.Unlock()
	memory.observe(storage.ScanObservation{BlocksPruned: storage.ObservationCount{Value: 1, Known: true}})
}

func (b *paxBlock) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	b.closed = true
	memory := b.memory
	released := b.encodedBytes + b.used
	b.data = nil
	b.descriptors = nil
	b.vis = nil
	b.cache = nil
	b.memory = nil
	b.encodedBytes = 0
	b.used = 0
	memory.release(released)
	return nil
}

func (b *paxBlock) chargeLocked(n uint64) error {
	if n > b.budget-b.used {
		return columnar.ErrBatchBudget
	}
	if err := b.memory.reserve(n); err != nil {
		return err
	}
	b.used += n
	return nil
}

func (b *paxBlock) reserveCacheLocked() error {
	if len(b.cache) < cap(b.cache) {
		return nil
	}
	size := uint64(unsafe.Sizeof(cachedPAXColumn{}))
	available := b.budget - b.used
	if b.memory != nil {
		available = min(available, b.memory.available())
	}
	available /= size
	if uint64(len(b.cache)+1) > available {
		return columnar.ErrBatchBudget
	}
	capacity := min(uint64(max(1, cap(b.cache)*2)), available)
	capacity = min(capacity, uint64(b.columns))
	if err := b.chargeLocked(capacity * size); err != nil {
		return err
	}
	cache := make([]cachedPAXColumn, len(b.cache), int(capacity))
	copy(cache, b.cache)
	released := uint64(cap(b.cache)) * size
	b.cache = cache
	b.used -= released
	b.memory.release(released)
	return nil
}

// verifyPAXNulls counts set bits over the logical lanes, rejects stray bits
// past the final lane, and requires the exact declared null count.
func verifyPAXNulls(bitmap []byte, rows int, expect uint32) error {
	if rows == 0 {
		if expect != 0 {
			return paxCorrupt("null count %d with zero rows", expect)
		}
		return nil
	}
	if len(bitmap) != (rows+7)/8 {
		return paxCorrupt("null bitmap %d bytes, want %d", len(bitmap), (rows+7)/8)
	}
	var count uint32
	for i := range rows {
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			count++
		}
	}
	for i := rows; i < len(bitmap)*8; i++ {
		if bitmap[i/8]&(1<<uint(i%8)) != 0 {
			return paxCorrupt("nonzero unused null bit %d", i)
		}
	}
	if count != expect {
		return paxCorrupt("null bitmap has %d nulls, want %d", count, expect)
	}
	return nil
}

func paxIsNull(bitmap []byte, row int) bool {
	return len(bitmap) != 0 && bitmap[row/8]&(1<<uint(row%8)) != 0
}

func borrowPAXInt64(body []byte) ([]int64, bool) {
	if len(body) == 0 || len(body)%8 != 0 || !paxNativeLE {
		return nil, false
	}
	if uintptrOf(body)%8 != 0 {
		return nil, false
	}
	return unsafe.Slice((*int64)(unsafe.Pointer(&body[0])), len(body)/8), true
}
