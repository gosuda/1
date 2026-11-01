package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Producer invariant failures are distinct from corruption in stored bytes.
var errPAXEncode = errors.New("invalid PAX block encoding input")

const encMaxDepth = 64

const encNodeOverhead = 512

// encBudget caps encoder temporary allocations against MaxDecodedBytes.
// The final owned block is bounded separately by MaxBytes.
type encBudget struct {
	limit uint64
	used  uint64
}

func encBudgetErr(limit uint64) error {
	return fmt.Errorf("pax encode: temporary allocation exceeds %d byte budget: %w", limit, columnar.ErrBatchBudget)
}

func encMulOk(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxUint64/b {
		return 0, false
	}
	return a * b, true
}

func encAddOk(a, b uint64) (uint64, bool) {
	if b > math.MaxUint64-a {
		return 0, false
	}
	return a + b, true
}

func encAlignOk(n uint64) (uint64, bool) {
	n, ok := encAddOk(n, 7)
	if !ok {
		return 0, false
	}
	return n &^ 7, true
}

func (b *encBudget) charge(n uint64) error {
	if n > b.limit-b.used {
		return fmt.Errorf("pax encode: %d temporary bytes exceed %d byte budget: %w", n, b.limit, columnar.ErrBatchBudget)
	}
	b.used += n
	return nil
}

func (b *encBudget) chargeMul(a, c uint64) error {
	sz, ok := encMulOk(a, c)
	if !ok {
		return encBudgetErr(b.limit)
	}
	return b.charge(sz)
}

// Each node keeps only its own payload prefix. Child bodies are copied directly
// to the final block, rather than duplicated through every enclosing array.
type encNode struct {
	columnID      model.ColumnID
	storage       model.DataType
	encoding      columnEncoding
	flags         uint16
	rows          int
	nulls         int
	typ           model.Type
	min, max      int64
	payload       []byte
	payloadLength uint64
	children      []*encNode
	childDescPos  []int
	childPayRel   []int
	abs           uint64
}

// encVisibility holds validated, borrowed version metadata.
type encVisibility struct {
	keyData   []byte
	keyRanges []columnar.Range
	endData   []byte
	endRanges []columnar.Range
	ts        []model.Timestamp
	ordinals  []uint64
	kinds     []storage.MutationOp
	keyTotal  uint64
	endTotal  uint64
}

func encodeBlock(ctx context.Context, target storage.ObjectRef, versions blockVersions, batch *columnar.Batch, columnIDs []model.ColumnID, limits blockLimits) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limits.MaxRows <= 0 || limits.MaxBytes == 0 || limits.MaxDecodedBytes == 0 ||
		limits.MaxBytes > uint64(math.MaxInt) || limits.MaxDecodedBytes > uint64(math.MaxInt) {
		return nil, fmt.Errorf("pax encode: invalid block limits %+v: %w", limits, errPAXEncode)
	}
	if err := target.Validate(); err != nil {
		return nil, fmt.Errorf("pax encode: invalid target: %w: %w", err, errPAXEncode)
	}
	if batch == nil {
		return nil, fmt.Errorf("pax encode: nil batch: %w", errPAXEncode)
	}
	if batch.Selection != nil {
		return nil, fmt.Errorf("pax encode: batch must have no selection: %w", errPAXEncode)
	}
	rows := batch.Length
	if rows <= 0 {
		return nil, fmt.Errorf("pax encode: block must be nonempty: %w", errPAXEncode)
	}
	if rows > limits.MaxRows {
		return nil, fmt.Errorf("pax encode: %d rows exceed MaxRows %d: %w", rows, limits.MaxRows, columnar.ErrBatchBudget)
	}
	if uint64(rows) > math.MaxUint32 {
		return nil, fmt.Errorf("pax encode: %d rows exceed uint32 range: %w", rows, columnar.ErrBatchBudget)
	}
	if len(batch.Columns) != len(columnIDs) {
		return nil, fmt.Errorf("pax encode: %d columns do not match %d column IDs: %w", len(batch.Columns), len(columnIDs), errPAXEncode)
	}
	if len(columnIDs) > math.MaxUint32 {
		return nil, fmt.Errorf("pax encode: %d columns exceed uint32 range: %w", len(columnIDs), columnar.ErrBatchBudget)
	}
	for i, id := range columnIDs {
		if id == 0 {
			return nil, fmt.Errorf("pax encode: root column ID must be nonzero: %w", errPAXEncode)
		}
		if i > 0 && id <= columnIDs[i-1] {
			return nil, fmt.Errorf("pax encode: column IDs must be strictly increasing: %w", errPAXEncode)
		}
	}
	budget := &encBudget{limit: limits.MaxDecodedBytes}
	vis, err := encValidateVersions(ctx, versions, rows, budget)
	if err != nil {
		return nil, err
	}
	nodes := make([]*encNode, len(batch.Columns))
	for i := range batch.Columns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		vec := &batch.Columns[i]
		if vec.Type.ID == 0 || !vec.Type.Storage.Valid() {
			return nil, fmt.Errorf("pax encode: column %d has invalid type identity: %w", columnIDs[i], errPAXEncode)
		}
		if err := vec.Validate(); err != nil {
			return nil, fmt.Errorf("pax encode: column %d invalid vector: %w: %w", columnIDs[i], err, errPAXEncode)
		}
		nodes[i], err = encEncodeNode(ctx, vec, rows, columnIDs[i], budget, 0)
		if err != nil {
			return nil, err
		}
	}
	return encAssemble(ctx, target, vis, nodes, rows, limits)
}

// encAbsentVector reports a zero Vector, the contract's marker for absent
// range ends when no range-delete records exist.
func encAbsentVector(v *columnar.Vector) bool {
	return v == nil ||
		(v.Type.ID == 0 && v.Length == 0 &&
			len(v.Nulls) == 0 && len(v.Indices) == 0 &&
			len(v.Int64) == 0 && len(v.Float64) == 0 &&
			len(v.Data) == 0 && len(v.Ranges) == 0 &&
			len(v.Float32) == 0 && len(v.Intervals) == 0 &&
			len(v.Children) == 0 && len(v.ShapeRanges) == 0 &&
			len(v.Dimensions) == 0)
}

// encLaneBytes returns the borrowed key bytes for one flat lane.
func encLaneBytes(v *columnar.Vector, row int) []byte {
	r := v.Ranges[row]
	return v.Data[r.Offset : uint64(r.Offset)+uint64(r.Length)]
}

func encValidateVersions(ctx context.Context, versions blockVersions, rows int, budget *encBudget) (*encVisibility, error) {
	if len(versions.CommitTS) != rows || len(versions.Ordinals) != rows || len(versions.Kinds) != rows {
		return nil, fmt.Errorf("pax encode: version arrays cover %d/%d/%d rows, want %d: %w",
			len(versions.CommitTS), len(versions.Ordinals), len(versions.Kinds), rows, errPAXEncode)
	}
	keys := &versions.Keys
	if err := keys.Validate(); err != nil {
		return nil, fmt.Errorf("pax encode: invalid key vector: %w: %w", err, errPAXEncode)
	}
	if keys.Encoding != columnar.Flat || keys.Offset != 0 || keys.Length != rows {
		return nil, fmt.Errorf("pax encode: key vector requires Flat Offset=0 Length=%d: %w", rows, errPAXEncode)
	}
	if keys.Type.Storage != model.TypeBytes {
		return nil, fmt.Errorf("pax encode: key vector storage %d is not bytes: %w", keys.Type.Storage, errPAXEncode)
	}
	endsAbsent := encAbsentVector(&versions.EndKeys)
	var ends *columnar.Vector
	if !endsAbsent {
		ends = &versions.EndKeys
		if err := ends.Validate(); err != nil {
			return nil, fmt.Errorf("pax encode: invalid end-key vector: %w: %w", err, errPAXEncode)
		}
		if ends.Encoding != columnar.Flat || ends.Offset != 0 || ends.Length != rows {
			return nil, fmt.Errorf("pax encode: end-key vector requires Flat Offset=0 Length=%d: %w", rows, errPAXEncode)
		}
		if ends.Type.Storage != model.TypeBytes {
			return nil, fmt.Errorf("pax encode: end-key vector storage %d is not bytes: %w", ends.Type.Storage, errPAXEncode)
		}
	}
	if err := budget.charge(uint64(unsafe.Sizeof(encVisibility{}))); err != nil {
		return nil, err
	}
	vis := &encVisibility{
		keyData:   keys.Data,
		keyRanges: keys.Ranges,
		ts:        versions.CommitTS,
		ordinals:  versions.Ordinals,
		kinds:     versions.Kinds,
	}
	if ends != nil {
		vis.endData, vis.endRanges = ends.Data, ends.Ranges
	}
	var prevKey []byte
	var prevTS model.Timestamp
	var prevOrd uint64
	for r := 0; r < rows; r++ {
		if r&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		ts := versions.CommitTS[r]
		if ts <= 0 {
			return nil, fmt.Errorf("pax encode: row %d has non-positive commit timestamp: %w", r, errPAXEncode)
		}
		ord := versions.Ordinals[r]
		if ord == 0 {
			return nil, fmt.Errorf("pax encode: row %d has zero ordinal: %w", r, errPAXEncode)
		}
		kind := versions.Kinds[r]
		switch kind {
		case storage.MutationUpsert, storage.MutationDelete, storage.MutationDeleteRange:
		default:
			return nil, fmt.Errorf("pax encode: row %d op %d is not a normalized mutation: %w", r, uint8(kind), errPAXEncode)
		}
		if keys.IsNull(r) {
			return nil, fmt.Errorf("pax encode: row %d has NULL key: %w", r, errPAXEncode)
		}
		key := encLaneBytes(keys, r)
		var end []byte
		if ends != nil {
			if ends.IsNull(r) {
				return nil, fmt.Errorf("pax encode: row %d has NULL range end: %w", r, errPAXEncode)
			}
			end = encLaneBytes(ends, r)
		}
		if kind == storage.MutationDeleteRange {
			if len(key) == 0 || len(end) == 0 {
				return nil, fmt.Errorf("pax encode: row %d range delete has empty bound: %w", r, errPAXEncode)
			}
			if bytes.Compare(key, end) >= 0 {
				return nil, fmt.Errorf("pax encode: row %d range [%x,%x) is empty: %w", r, key, end, errPAXEncode)
			}
		} else if len(end) != 0 {
			return nil, fmt.Errorf("pax encode: row %d point mutation has nonempty range end: %w", r, errPAXEncode)
		}
		if r > 0 {
			cmp := bytes.Compare(prevKey, key)
			switch {
			case cmp > 0:
				return nil, fmt.Errorf("pax encode: row %d key out of ascending order: %w", r, errPAXEncode)
			case cmp == 0 && prevTS < ts:
				return nil, fmt.Errorf("pax encode: row %d timestamp out of descending order: %w", r, errPAXEncode)
			case cmp == 0 && prevTS == ts && prevOrd <= ord:
				return nil, fmt.Errorf("pax encode: row %d duplicates key/version/ordinal: %w", r, errPAXEncode)
			}
		}
		keyTotal, ok := encAddOk(vis.keyTotal, uint64(len(key)))
		if !ok {
			return nil, encBudgetErr(budget.limit)
		}
		endTotal, ok := encAddOk(vis.endTotal, uint64(len(end)))
		if !ok {
			return nil, encBudgetErr(budget.limit)
		}
		vis.keyTotal, vis.endTotal = keyTotal, endTotal
		prevKey, prevTS, prevOrd = key, ts, ord
	}
	return vis, nil
}

// Hash borrowed dictionary bytes without allocating string keys.
func encHash(ctx context.Context, data []byte) (uint64, error) {
	const (
		offset = uint64(14695981039346656037)
		prime  = uint64(1099511628211)
	)
	h := offset
	for i, c := range data {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		h ^= uint64(c)
		h *= prime
	}
	return h, nil
}

// encLaneData returns the borrowed bytes for one variable-width lane.
func encLaneData(vec *columnar.Vector, row int) []byte {
	r := vec.Ranges[row]
	return vec.Data[r.Offset : uint64(r.Offset)+uint64(r.Length)]
}

func encEncodeNode(ctx context.Context, vec *columnar.Vector, rows int, colID model.ColumnID, budget *encBudget, depth int) (*encNode, error) {
	if depth > encMaxDepth {
		return nil, fmt.Errorf("pax encode: nesting depth exceeds %d: %w", encMaxDepth, errPAXEncode)
	}
	if vec.Encoding != columnar.Flat || vec.Offset != 0 || vec.Length != rows {
		return nil, fmt.Errorf("pax encode: node requires Flat Offset=0 Length=%d vector: %w", rows, errPAXEncode)
	}
	if n := vec.Type.Storage; !n.Valid() || n == model.TypeAny {
		return nil, fmt.Errorf("pax encode: unsupported storage type %d: %w", n, errPAXEncode)
	}
	if uint64(rows) > math.MaxUint32 {
		return nil, fmt.Errorf("pax encode: %d node rows exceed uint32 range: %w", rows, columnar.ErrBatchBudget)
	}
	if err := budget.charge(encNodeOverhead); err != nil {
		return nil, err
	}
	n := &encNode{
		columnID: colID,
		storage:  vec.Type.Storage,
		encoding: encodingPlain,
		rows:     rows,
		typ:      vec.Type,
	}
	for r := 0; r < rows; r++ {
		if r&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if vec.IsNull(r) {
			n.nulls++
		}
	}
	var payload []byte
	if n.nulls > 0 {
		size := encBitmapLen(rows, n.nulls)
		if err := budget.charge(uint64(size)); err != nil {
			return nil, err
		}
		payload = make([]byte, size)
		for r := 0; r < rows; r++ {
			if vec.IsNull(r) {
				payload[r/8] |= 1 << uint(r%8)
			}
		}
	}
	body, err := encEncodeBody(ctx, vec, rows, n, budget, depth)
	if err != nil {
		return nil, err
	}
	if len(payload) == 0 {
		n.payload = body
		n.payloadLength = max(n.payloadLength, uint64(len(body)))
		return n, nil
	}
	if err := budget.charge(uint64(len(payload)) + uint64(len(body))); err != nil {
		return nil, err
	}
	n.payload = make([]byte, len(payload)+len(body))
	copy(n.payload, payload)
	copy(n.payload[len(payload):], body)
	n.payloadLength = max(n.payloadLength, uint64(len(n.payload)))
	return n, nil
}

func encEncodeBody(ctx context.Context, vec *columnar.Vector, rows int, n *encNode, budget *encBudget, depth int) ([]byte, error) {
	switch vec.Type.Storage {
	case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
		return encEncodeIntegers(ctx, vec, rows, n, budget)
	case model.TypeFloat64:
		return encEncodeFloat64(ctx, vec, rows, budget)
	case model.TypeInterval:
		return encEncodeInterval(ctx, vec, rows, budget)
	case model.TypeString, model.TypeBytes, model.TypeJSONB, model.TypeUUID, model.TypeNumeric:
		return encEncodeVarBytes(ctx, vec, rows, n, budget)
	case model.TypeVectorF32:
		return encEncodeVectorF32(ctx, vec, rows, budget)
	case model.TypeArray:
		return encEncodeArray(ctx, vec, rows, n, budget, depth)
	case model.TypeComposite:
		return encEncodeComposite(ctx, vec, rows, n, budget, depth)
	default:
		return nil, fmt.Errorf("pax encode: unsupported storage type %d: %w", vec.Type.Storage, errPAXEncode)
	}
}

// encEncodeIntegers densely encodes every lane, including NULL lanes at
// their stored zero slots, so decodeIntegers validates against rows.
func encEncodeIntegers(ctx context.Context, vec *columnar.Vector, rows int, n *encNode, budget *encBudget) ([]byte, error) {
	if err := budget.chargeMul(uint64(rows), 8); err != nil {
		return nil, err
	}
	values := vec.Int64[:rows]
	found := false
	for r := 0; r < rows; r++ {
		if r&4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if vec.IsNull(r) {
			continue
		}
		v := values[r]
		if !found {
			n.min, n.max, found = v, v, true
		} else {
			n.min, n.max = min(n.min, v), max(n.max, v)
		}
	}
	if found {
		n.flags = paxZoneInteger
	}
	body, enc := encodeIntegers(make([]byte, 0, rows*8), values)
	n.encoding = enc
	return body, nil
}

func encEncodeFloat64(ctx context.Context, vec *columnar.Vector, rows int, budget *encBudget) ([]byte, error) {
	if err := budget.chargeMul(uint64(rows), 8); err != nil {
		return nil, err
	}
	body := make([]byte, rows*8)
	for r := 0; r < rows; r++ {
		if r&4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		binary.LittleEndian.PutUint64(body[r*8:], math.Float64bits(vec.Float64[r]))
	}
	return body, nil
}

func encEncodeInterval(ctx context.Context, vec *columnar.Vector, rows int, budget *encBudget) ([]byte, error) {
	if err := budget.chargeMul(uint64(rows), 16); err != nil {
		return nil, err
	}
	body := make([]byte, rows*16)
	for r := 0; r < rows; r++ {
		if r&4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		iv := vec.Intervals[r]
		off := r * 16
		binary.LittleEndian.PutUint32(body[off:], uint32(iv.Months))
		binary.LittleEndian.PutUint32(body[off+4:], uint32(iv.Days))
		binary.LittleEndian.PutUint64(body[off+8:], uint64(iv.Microseconds))
	}
	return body, nil
}

// encCheckAllRanges validates every lane's range, including NULL lanes whose
// slots are encoded verbatim.
func encCheckAllRanges(ranges []columnar.Range, rows, size int) error {
	if len(ranges) < rows {
		return fmt.Errorf("pax encode: %d ranges cover %d rows: %w", len(ranges), rows, errPAXEncode)
	}
	for r := 0; r < rows; r++ {
		if uint64(ranges[r].Offset)+uint64(ranges[r].Length) > uint64(size) {
			return fmt.Errorf("pax encode: row %d range exceeds pool of %d: %w", r, size, errPAXEncode)
		}
	}
	return nil
}

func encEncodeVarBytes(ctx context.Context, vec *columnar.Vector, rows int, n *encNode, budget *encBudget) ([]byte, error) {
	if err := encCheckAllRanges(vec.Ranges, rows, len(vec.Data)); err != nil {
		return nil, err
	}
	plainSize := uint64(rows) * 4
	for r := 0; r < rows; r++ {
		if r&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if vec.IsNull(r) {
			continue
		}
		plainSize, _ = encAddOk(plainSize, uint64(vec.Ranges[r].Length))
		if plainSize > math.MaxUint32 {
			return nil, fmt.Errorf("pax encode: variable body exceeds uint32 range: %w", columnar.ErrBatchBudget)
		}
	}
	if rows == 0 {
		return nil, nil
	}
	tableSize := uint64(1)
	for tableSize < uint64(rows)*2 {
		tableSize *= 2
	}
	if err := budget.chargeMul(tableSize, 4); err != nil {
		return nil, err
	}
	if err := budget.chargeMul(uint64(rows), 8); err != nil {
		return nil, err
	}
	buckets := make([]uint32, int(tableSize))
	order := make([]uint32, 0, rows)
	codes := make([]uint32, rows)
	dictBytes := uint64(0)
	considerDict := false
	for r := 0; r < rows; r++ {
		if r&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if vec.IsNull(r) {
			continue
		}
		considerDict = true
		data := encLaneData(vec, r)
		h, err := encHash(ctx, data)
		if err != nil {
			return nil, err
		}
		slot := h & (tableSize - 1)
		for probes := 0; ; probes++ {
			if probes&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			entry := buckets[slot]
			if entry == 0 {
				code := uint32(len(order))
				order = append(order, uint32(r))
				buckets[slot] = code + 1
				codes[r] = code
				entrySize, ok := encAddOk(4, uint64(len(data)))
				if !ok {
					return nil, encBudgetErr(budget.limit)
				}
				dictBytes, ok = encAddOk(dictBytes, entrySize)
				if !ok {
					return nil, encBudgetErr(budget.limit)
				}
				break
			}
			code := entry - 1
			if bytes.Equal(encLaneData(vec, int(order[code])), data) {
				codes[r] = code
				break
			}
			slot = (slot + 1) & (tableSize - 1)
		}
	}
	if considerDict && len(order) > 0 {
		width := max(1, bits.Len32(uint32(len(order))-1))
		if width <= 32 {
			bitsTotal, ok := encMulOk(uint64(rows), uint64(width))
			if !ok {
				return nil, encBudgetErr(budget.limit)
			}
			bitsTotal, ok = encAddOk(bitsTotal, 7)
			if !ok {
				return nil, encBudgetErr(budget.limit)
			}
			packedSize := bitsTotal / 8
			dictSize, ok := encAddOk(9, dictBytes)
			if !ok {
				return nil, encBudgetErr(budget.limit)
			}
			dictSize, ok = encAddOk(dictSize, packedSize)
			if !ok {
				return nil, encBudgetErr(budget.limit)
			}
			if dictSize < plainSize {
				return encEncodeDictionary(ctx, vec, order, codes, rows, width, n, budget)
			}
		}
	}
	if err := budget.charge(plainSize); err != nil {
		return nil, err
	}
	body := make([]byte, 0, plainSize)
	var lenBuf [4]byte
	for r := 0; r < rows; r++ {
		if vec.IsNull(r) {
			body = append(body, 0, 0, 0, 0)
			continue
		}
		data := encLaneData(vec, r)
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))
		body = append(body, lenBuf[:]...)
		body = append(body, data...)
	}
	n.encoding = encodingPlain
	return body, nil
}

func encEncodeDictionary(ctx context.Context, vec *columnar.Vector, dict []uint32, codes []uint32, rows, width int, n *encNode, budget *encBudget) ([]byte, error) {
	bitsTotal, ok := encMulOk(uint64(rows), uint64(width))
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	bitsTotal, ok = encAddOk(bitsTotal, 7)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	packedSize := bitsTotal / 8
	size := uint64(9)
	for _, entry := range dict {
		entrySize, ok := encAddOk(4, uint64(len(encLaneData(vec, int(entry)))))
		if !ok {
			return nil, encBudgetErr(budget.limit)
		}
		size, ok = encAddOk(size, entrySize)
		if !ok {
			return nil, encBudgetErr(budget.limit)
		}
	}
	size, ok = encAddOk(size, packedSize)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	if err := budget.charge(size); err != nil {
		return nil, err
	}
	body := make([]byte, 0, size)
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(dict)))
	body = append(body, lenBuf[:]...)
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(rows))
	body = append(body, lenBuf[:]...)
	body = append(body, byte(width))
	for _, entry := range dict {
		data := encLaneData(vec, int(entry))
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(data)))
		body = append(body, lenBuf[:]...)
		body = append(body, data...)
	}
	start := len(body)
	body = body[:int(size)]
	packed := body[start:]
	for i, code := range codes {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		bitPos := uint64(i) * uint64(width)
		for k := 0; k < width; {
			byteIdx := bitPos / 8
			bitOff := bitPos % 8
			take := min(width-k, 8-int(bitOff))
			chunk := byte((code >> uint(k)) & uint32((1<<uint(take))-1))
			packed[byteIdx] |= chunk << bitOff
			bitPos += uint64(take)
			k += take
		}
	}
	n.encoding = encodingDictionary
	return body, nil
}

func encEncodeVectorF32(ctx context.Context, vec *columnar.Vector, rows int, budget *encBudget) ([]byte, error) {
	if err := encCheckAllRanges(vec.Ranges, rows, len(vec.Float32)); err != nil {
		return nil, err
	}
	spine, ok := encMulOk(uint64(rows), 8)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	pool, ok := encMulOk(uint64(len(vec.Float32)), 4)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	total, ok := encAddOk(spine, 4)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	total, ok = encAddOk(total, pool)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	if total > math.MaxUint32 {
		return nil, fmt.Errorf("pax encode: float32 body exceeds uint32 range: %w", columnar.ErrBatchBudget)
	}
	if err := budget.charge(total); err != nil {
		return nil, err
	}
	body := make([]byte, 0, total)
	var numBuf [8]byte
	for r := 0; r < rows; r++ {
		if r&4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		rng := vec.Ranges[r]
		binary.LittleEndian.PutUint32(numBuf[:], rng.Offset)
		binary.LittleEndian.PutUint32(numBuf[4:], rng.Length)
		body = append(body, numBuf[:]...)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(vec.Float32)))
	body = append(body, lenBuf[:]...)
	for i, f := range vec.Float32 {
		if i&4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		binary.LittleEndian.PutUint32(lenBuf[:], math.Float32bits(f))
		body = append(body, lenBuf[:]...)
	}
	return body, nil
}

func encEncodeArray(ctx context.Context, vec *columnar.Vector, rows int, n *encNode, budget *encBudget, depth int) ([]byte, error) {
	if len(vec.Children) != 1 {
		return nil, fmt.Errorf("pax encode: array node has %d children: %w", len(vec.Children), errPAXEncode)
	}
	child := &vec.Children[0]
	if err := encCheckAllRanges(vec.Ranges, rows, child.Length); err != nil {
		return nil, err
	}
	if err := encCheckAllRanges(vec.ShapeRanges, rows, len(vec.Dimensions)); err != nil {
		return nil, err
	}
	if uint64(child.Length) > math.MaxUint32 || uint64(len(vec.Dimensions)) > math.MaxUint32 {
		return nil, fmt.Errorf("pax encode: array child exceeds uint32 range: %w", columnar.ErrBatchBudget)
	}
	for r := 0; r < rows; r++ {
		if r&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if vec.IsNull(r) {
			continue
		}
		sr := vec.ShapeRanges[r]
		dims := vec.Dimensions[sr.Offset : sr.Offset+sr.Length]
		if err := model.ValidateArrayShape(dims, uint64(vec.Ranges[r].Length)); err != nil {
			return nil, fmt.Errorf("pax encode: row %d invalid array shape: %w: %w", r, err, errPAXEncode)
		}
	}
	spine, ok := encMulOk(uint64(rows), 16)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	dimBytes, ok := encMulOk(uint64(len(vec.Dimensions)), 8)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	fixed, ok := encAddOk(spine, 4)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	fixed, ok = encAddOk(fixed, dimBytes)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	bitmap := uint64(encBitmapLen(rows, n.nulls))
	end, ok := encAddOk(bitmap, fixed)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	end, ok = encAlignOk(end)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	end, ok = encAddOk(end, paxColumnSize)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	fixed = end - bitmap
	if err := budget.charge(fixed); err != nil {
		return nil, err
	}
	body := make([]byte, 0, fixed)
	var numBuf [8]byte
	for r := 0; r < rows; r++ {
		vr := vec.Ranges[r]
		binary.LittleEndian.PutUint32(numBuf[:], vr.Offset)
		binary.LittleEndian.PutUint32(numBuf[4:], vr.Length)
		body = append(body, numBuf[:]...)
	}
	for r := 0; r < rows; r++ {
		sr := vec.ShapeRanges[r]
		binary.LittleEndian.PutUint32(numBuf[:], sr.Offset)
		binary.LittleEndian.PutUint32(numBuf[4:], sr.Length)
		body = append(body, numBuf[:]...)
	}
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(vec.Dimensions)))
	body = append(body, lenBuf[:]...)
	for _, dim := range vec.Dimensions {
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(dim.Length))
		body = append(body, lenBuf[:]...)
		binary.LittleEndian.PutUint32(lenBuf[:], uint32(dim.LowerBound))
		body = append(body, lenBuf[:]...)
	}
	// The child descriptor table starts at body offset len(body) within the
	// child fragment; align the fragment start so the absolute child
	// descriptor (n.abs + bitmap + len(body) + pad) is 8-aligned.
	prefix := len(body)
	encoded, err := encEncodeChildren(ctx, vec.Children, []int{child.Length}, n, budget, depth, prefix)
	if err != nil {
		return nil, err
	}
	body = append(body, encoded...)
	return body, nil
}

func encEncodeComposite(ctx context.Context, vec *columnar.Vector, rows int, n *encNode, budget *encBudget, depth int) ([]byte, error) {
	for i := range vec.Children {
		if vec.Children[i].Length != rows {
			return nil, fmt.Errorf("pax encode: composite child %d has %d rows, want %d: %w", i, vec.Children[i].Length, rows, errPAXEncode)
		}
	}
	childRows := make([]int, len(vec.Children))
	for i := range vec.Children {
		childRows[i] = rows
	}
	if err := budget.chargeMul(uint64(len(vec.Children)), paxColumnSize); err != nil {
		return nil, err
	}
	encoded, err := encEncodeChildren(ctx, vec.Children, childRows, n, budget, depth, 0)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// encEncodeChildren encodes child nodes and positions their descriptor table
// and payloads so every absolute offset is 8-aligned. prefix is the number
// of parent-body bytes before this fragment (NULL bitmap plus fixed fields).
func encEncodeChildren(ctx context.Context, children []columnar.Vector, childRows []int, n *encNode, budget *encBudget, depth int, prefix int) ([]byte, error) {
	nodes := make([]*encNode, len(children))
	for i := range children {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		child := &children[i]
		if child.Type.ID == 0 || !child.Type.Storage.Valid() {
			return nil, fmt.Errorf("pax encode: nested child %d has invalid type identity: %w", i, errPAXEncode)
		}
		if err := child.Validate(); err != nil {
			return nil, fmt.Errorf("pax encode: nested child %d invalid vector: %w: %w", i, err, errPAXEncode)
		}
		node, err := encEncodeNode(ctx, child, childRows[i], 0, budget, depth+1)
		if err != nil {
			return nil, err
		}
		nodes[i] = node
	}
	n.children = nodes
	n.childDescPos = make([]int, len(nodes))
	n.childPayRel = make([]int, len(nodes))
	before, ok := encAddOk(uint64(encBitmapLen(n.rows, n.nulls)), uint64(prefix))
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	descriptorStart, ok := encAlignOk(before)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	descriptorBytes, ok := encMulOk(uint64(len(nodes)), paxColumnSize)
	if !ok {
		return nil, encBudgetErr(budget.limit)
	}
	position, ok := encAddOk(descriptorStart, descriptorBytes)
	if !ok || position > uint64(math.MaxInt) {
		return nil, encBudgetErr(budget.limit)
	}
	bodySize := position - before
	if err := budget.charge(bodySize); err != nil {
		return nil, err
	}
	body := make([]byte, int(bodySize))
	for i, node := range nodes {
		n.childDescPos[i] = int(descriptorStart) + i*paxColumnSize
		position, ok = encAlignOk(position)
		if !ok || position > uint64(math.MaxInt) {
			return nil, encBudgetErr(budget.limit)
		}
		n.childPayRel[i] = int(position)
		position, ok = encAddOk(position, node.payloadLength)
		if !ok {
			return nil, encBudgetErr(budget.limit)
		}
	}
	n.payloadLength = position
	return body, nil
}

// encBitmapLen is the NULL bitmap prefix length for a node payload.
func encBitmapLen(rows, nulls int) int {
	if nulls == 0 {
		return 0
	}
	q, r := rows/8, rows%8
	if r != 0 {
		q++
	}
	return q
}

// encPadAbs pads buf so absolute position abs+len(buf) is 8-aligned.
func encPadAbs(buf []byte, abs uint64, budget *encBudget) ([]byte, error) {
	pad := (8 - abs%8) % 8
	if pad == 0 {
		return buf, nil
	}
	if err := budget.charge(pad); err != nil {
		return nil, err
	}
	return append(buf, make([]byte, pad)...), nil
}

func encAssemble(ctx context.Context, target storage.ObjectRef, vis *encVisibility, nodes []*encNode, rows int, limits blockLimits) ([]byte, error) {
	descTable, ok := encMulOk(uint64(len(nodes)), paxColumnSize)
	if !ok {
		return nil, encBudgetErr(limits.MaxDecodedBytes)
	}
	visRecs, ok := encMulOk(uint64(rows), paxVersionSize)
	if !ok {
		return nil, encBudgetErr(limits.MaxDecodedBytes)
	}
	visLen, ok := encAddOk(visRecs, vis.keyTotal)
	if !ok {
		return nil, encBudgetErr(limits.MaxDecodedBytes)
	}
	visLen, ok = encAddOk(visLen, vis.endTotal)
	if !ok {
		return nil, encBudgetErr(limits.MaxDecodedBytes)
	}
	if visLen > math.MaxUint32 {
		return nil, fmt.Errorf("pax encode: visibility region of %d bytes exceeds uint32 range: %w", visLen, columnar.ErrBatchBudget)
	}
	head, ok := encAddOk(paxHeaderSize, descTable)
	if !ok {
		return nil, encBudgetErr(limits.MaxDecodedBytes)
	}
	head, ok = encAddOk(head, visLen)
	if !ok {
		return nil, encBudgetErr(limits.MaxDecodedBytes)
	}
	off, ok := encAlignOk(head)
	if !ok {
		return nil, encBudgetErr(limits.MaxDecodedBytes)
	}
	for _, n := range nodes {
		n.abs = off
		encLayoutChildren(n)
		next, ok := encAddOk(off, n.payloadLength)
		if !ok {
			return nil, encBudgetErr(limits.MaxDecodedBytes)
		}
		off, ok = encAlignOk(next)
		if !ok {
			return nil, encBudgetErr(limits.MaxDecodedBytes)
		}
	}
	total := off
	if total > limits.MaxBytes {
		return nil, fmt.Errorf("pax encode: block of %d bytes exceeds MaxBytes %d: %w", total, limits.MaxBytes, columnar.ErrBatchBudget)
	}
	out := make([]byte, total)
	binary.LittleEndian.PutUint32(out[0:], paxMagic)
	binary.LittleEndian.PutUint32(out[4:], paxVersion)
	binary.LittleEndian.PutUint32(out[8:], uint32(rows))
	binary.LittleEndian.PutUint32(out[12:], uint32(len(nodes)))
	binary.LittleEndian.PutUint64(out[16:], uint64(target.ObjectID))
	binary.LittleEndian.PutUint64(out[24:], target.Generation)
	binary.LittleEndian.PutUint64(out[32:], target.DescriptorVersion)
	binary.LittleEndian.PutUint32(out[40:], target.EncodingVersion)
	binary.LittleEndian.PutUint32(out[44:], uint32(visLen))
	binary.LittleEndian.PutUint64(out[48:], total)
	for i, n := range nodes {
		encWriteDescriptor(out[paxHeaderSize+i*paxColumnSize:], n, n.abs)
	}
	vbase := uint64(paxHeaderSize) + descTable
	keyCursor, endCursor := uint64(0), uint64(0)
	for r := 0; r < rows; r++ {
		if r&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		keyRange := vis.keyRanges[r]
		key := vis.keyData[keyRange.Offset : uint64(keyRange.Offset)+uint64(keyRange.Length)]
		var end []byte
		if vis.endRanges != nil {
			endRange := vis.endRanges[r]
			end = vis.endData[endRange.Offset : uint64(endRange.Offset)+uint64(endRange.Length)]
		}
		rec := out[vbase+uint64(r)*paxVersionSize:]
		binary.LittleEndian.PutUint64(rec[0:], uint64(vis.ts[r]))
		binary.LittleEndian.PutUint64(rec[8:], vis.ordinals[r])
		rec[16] = byte(vis.kinds[r])
		keyOff := visRecs + keyCursor
		endOff := visRecs + vis.keyTotal + endCursor
		binary.LittleEndian.PutUint32(rec[24:], uint32(keyOff))
		binary.LittleEndian.PutUint32(rec[28:], uint32(len(key)))
		binary.LittleEndian.PutUint32(rec[32:], uint32(endOff))
		binary.LittleEndian.PutUint32(rec[36:], uint32(len(end)))
		keyCursor += uint64(len(key))
		endCursor += uint64(len(end))
		copy(out[vbase+keyOff:], key)
		copy(out[vbase+endOff:], end)
	}
	for _, n := range nodes {
		copy(out[n.abs:], n.payload)
		encPatchChildren(out, n)
	}
	binary.LittleEndian.PutUint32(out[56:], paxChecksum(out))
	return out, nil
}

func encLayoutChildren(n *encNode) {
	for i, c := range n.children {
		c.abs = n.abs + uint64(n.childPayRel[i])
		encLayoutChildren(c)
	}
}

func encPatchChildren(out []byte, n *encNode) {
	for i, c := range n.children {
		copy(out[c.abs:], c.payload)
		encWriteDescriptor(out[n.abs+uint64(n.childDescPos[i]):], c, c.abs)
		encPatchChildren(out, c)
	}
}

func encWriteDescriptor(dst []byte, n *encNode, abs uint64) {
	binary.LittleEndian.PutUint32(dst[0:], uint32(n.columnID))
	dst[4] = byte(n.storage)
	dst[5] = byte(n.encoding)
	binary.LittleEndian.PutUint16(dst[6:], n.flags)
	binary.LittleEndian.PutUint32(dst[8:], uint32(n.rows))
	binary.LittleEndian.PutUint32(dst[12:], uint32(n.nulls))
	binary.LittleEndian.PutUint32(dst[16:], uint32(n.typ.ID))
	binary.LittleEndian.PutUint32(dst[20:], uint32(n.typ.Modifier))
	binary.LittleEndian.PutUint32(dst[24:], uint32(n.typ.Collation))
	binary.LittleEndian.PutUint32(dst[28:], uint32(len(n.children)))
	binary.LittleEndian.PutUint64(dst[32:], abs)
	binary.LittleEndian.PutUint64(dst[40:], n.payloadLength)
	binary.LittleEndian.PutUint64(dst[48:], uint64(n.min))
	binary.LittleEndian.PutUint64(dst[56:], uint64(n.max))
}
