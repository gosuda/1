package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	sstZoneEntrySize = 24
	zonePrefixBytes  = 16

	sstZoneInt   uint8 = 1
	sstZoneBytes uint8 = 2

	zoneHasNonNull   uint8 = 1 << 0
	zoneHasNull      uint8 = 1 << 1
	zoneMinExact     uint8 = 1 << 2
	zoneMaxExact     uint8 = 1 << 3
	zoneMaxExclusive uint8 = 1 << 4
)

type sstZoneEntry struct {
	columnID model.ColumnID
	kind     uint8
	flags    uint8
	minInt   int64
	maxInt   int64
	minRange columnar.Range
	maxRange columnar.Range
}

func encodeSSTZoneEntry(dst []byte, z sstZoneEntry) {
	binary.LittleEndian.PutUint32(dst[0:4], uint32(z.columnID))
	dst[4] = z.kind
	dst[5] = z.flags
	binary.LittleEndian.PutUint16(dst[6:8], 0)
	if z.kind == sstZoneInt {
		binary.LittleEndian.PutUint64(dst[8:16], uint64(z.minInt))
		binary.LittleEndian.PutUint64(dst[16:24], uint64(z.maxInt))
	} else {
		binary.LittleEndian.PutUint32(dst[8:12], z.minRange.Offset)
		binary.LittleEndian.PutUint32(dst[12:16], z.minRange.Length)
		binary.LittleEndian.PutUint32(dst[16:20], z.maxRange.Offset)
		binary.LittleEndian.PutUint32(dst[20:24], z.maxRange.Length)
	}
}

func decodeSSTZoneEntry(src []byte) sstZoneEntry {
	z := sstZoneEntry{
		columnID: model.ColumnID(binary.LittleEndian.Uint32(src[0:4])),
		kind:     src[4],
		flags:    src[5],
	}
	if z.kind == sstZoneInt {
		z.minInt = int64(binary.LittleEndian.Uint64(src[8:16]))
		z.maxInt = int64(binary.LittleEndian.Uint64(src[16:24]))
	} else {
		z.minRange = columnar.Range{
			Offset: binary.LittleEndian.Uint32(src[8:12]),
			Length: binary.LittleEndian.Uint32(src[12:16]),
		}
		z.maxRange = columnar.Range{
			Offset: binary.LittleEndian.Uint32(src[16:20]),
			Length: binary.LittleEndian.Uint32(src[20:24]),
		}
	}
	return z
}

// nextPrefixBound computes the smallest byte slice strictly greater than every
// value starting with prefix (length <= zonePrefixBytes). Returns (bound, true)
// or (nil, false) if all bytes are 0xff (unbounded).
func nextPrefixBound(prefix []byte) ([]byte, bool) {
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] < 0xff {
			out := make([]byte, i+1)
			copy(out, prefix[:i+1])
			out[i]++
			return out, true
		}
	}
	return nil, false
}

// extractBlockZones builds per-column zone summaries for one PAX block during
// SST construction. Integer/timestamp/boolean bounds come directly from PAX
// descriptors; variable byte columns compute bounded-prefix min/max bounds.
func extractBlockZones(ctx context.Context, block *paxBlock, blobs []toastBlob, pool *[]byte, maxPool uint64) ([]sstZoneEntry, error) {
	if block.columns == 0 {
		return nil, nil
	}
	zones := make([]sstZoneEntry, 0, block.columns)
	for i := range block.columns {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		desc, err := parsePAXDescriptor(block.descriptors[i*paxColumnSize:], false)
		if err != nil {
			return nil, err
		}
		var flags uint8
		if desc.nulls > 0 {
			flags |= zoneHasNull
		}
		if desc.nulls < desc.rows {
			flags |= zoneHasNonNull
		}
		switch desc.storage {
		case model.TypeInt64, model.TypeTimestamp, model.TypeBoolean:
			z := sstZoneEntry{columnID: desc.id, kind: sstZoneInt, flags: flags}
			if desc.flags&paxZoneInteger != 0 {
				z.minInt = desc.min
				z.maxInt = desc.max
			} else if flags&zoneHasNonNull != 0 {
				z.minInt = math.MinInt64
				z.maxInt = math.MaxInt64
			}
			zones = append(zones, z)
		case model.TypeString, model.TypeBytes, model.TypeUUID, model.TypeNumeric:
			z, ok, err := extractVarBytesZone(ctx, block, &desc, blobs, flags, pool, maxPool)
			if err != nil {
				return nil, err
			}
			if ok {
				zones = append(zones, z)
			}
		}
	}
	return zones, nil
}

func extractVarBytesZone(ctx context.Context, block *paxBlock, desc *decDescriptor, blobs []toastBlob, flags uint8, pool *[]byte, maxPool uint64) (sstZoneEntry, bool, error) {
	z := sstZoneEntry{columnID: desc.id, kind: sstZoneBytes, flags: flags}
	if flags&zoneHasNonNull == 0 {
		return z, true, nil
	}
	payload := block.data[desc.payload : desc.payload+desc.length]
	var nulls []byte
	rest := payload
	rows := int(desc.rows)
	if desc.nulls > 0 {
		need := (rows + 7) / 8
		if len(rest) < need {
			return z, false, nil
		}
		nulls = rest[:need]
		rest = rest[need:]
	}

	var minVal, maxVal []byte
	var minTrunc, maxTrunc bool
	found := false
	consider := func(val []byte) {
		p := val
		trunc := false
		if len(p) > zonePrefixBytes {
			p = p[:zonePrefixBytes]
			trunc = true
		}
		if !found {
			minVal = append(minVal[:0], p...)
			maxVal = append(maxVal[:0], p...)
			minTrunc, maxTrunc = trunc, trunc
			found = true
			return
		}
		if c := bytes.Compare(p, minVal); c < 0 || (c == 0 && !trunc && minTrunc) {
			minVal = append(minVal[:0], p...)
			minTrunc = trunc
		}
		if c := bytes.Compare(p, maxVal); c > 0 || (c == 0 && trunc && !maxTrunc) {
			maxVal = append(maxVal[:0], p...)
			maxTrunc = trunc
		}
	}

	switch desc.encoding {
	case encodingPlain:
		pos := 0
		for r := range rows {
			if r&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return z, false, err
				}
			}
			if len(rest)-pos < 4 {
				return z, false, nil
			}
			n := int(binary.LittleEndian.Uint32(rest[pos:]))
			pos += 4
			if n < 0 || n > len(rest)-pos {
				return z, false, nil
			}
			if !paxIsNull(nulls, r) {
				consider(rest[pos : pos+n])
			}
			pos += n
		}
	case encodingDictionary:
		if len(rest) < 9 {
			return z, false, nil
		}
		dictCount := int(binary.LittleEndian.Uint32(rest[0:]))
		pos := 9
		for i := range dictCount {
			if i&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return z, false, err
				}
			}
			if len(rest)-pos < 4 {
				return z, false, nil
			}
			n := int(binary.LittleEndian.Uint32(rest[pos:]))
			pos += 4
			if n < 0 || n > len(rest)-pos {
				return z, false, nil
			}
			consider(rest[pos : pos+n])
			pos += n
		}
	case encodingLOB:
		pos := 0
		for r := range rows {
			if r&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return z, false, err
				}
			}
			if pos >= len(rest) {
				return z, false, nil
			}
			tag := rest[pos]
			pos++
			switch tag {
			case 0:
				if len(rest)-pos < 4 {
					return z, false, nil
				}
				n := int(binary.LittleEndian.Uint32(rest[pos:]))
				pos += 4
				if n < 0 || n > len(rest)-pos {
					return z, false, nil
				}
				if !paxIsNull(nulls, r) {
					consider(rest[pos : pos+n])
				}
				pos += n
			case 1:
				if len(rest)-pos < 12 {
					return z, false, nil
				}
				blobIdx := binary.LittleEndian.Uint32(rest[pos:])
				pos += 12
				if int(blobIdx) >= len(blobs) {
					return z, false, nil
				}
				consider(blobs[blobIdx].data)
			default:
				return z, false, nil
			}
		}
	default:
		return z, false, nil
	}

	if !found {
		return z, true, nil
	}
	if !minTrunc {
		z.flags |= zoneMinExact
	}
	var maxBound []byte
	if !maxTrunc {
		z.flags |= zoneMaxExact
		maxBound = maxVal
	} else if next, ok := nextPrefixBound(maxVal); ok {
		z.flags |= zoneMaxExclusive
		maxBound = next
	}
	needed := uint64(len(minVal)) + uint64(len(maxBound))
	if uint64(len(*pool))+needed > maxPool || uint64(len(*pool))+needed > math.MaxUint32 {
		return z, false, nil
	}
	z.minRange = columnar.Range{Offset: uint32(len(*pool)), Length: uint32(len(minVal))}
	*pool = append(*pool, minVal...)
	if len(maxBound) != 0 || z.flags&(zoneMaxExact|zoneMaxExclusive) != 0 {
		z.maxRange = columnar.Range{Offset: uint32(len(*pool)), Length: uint32(len(maxBound))}
		*pool = append(*pool, maxBound...)
	}
	return z, true, nil
}

// lookupZone finds the SST-index zone entry for (blockIndex, columnID) in O(log C).
func (s *sstFile) lookupZone(blockIndex int, id model.ColumnID) (sstZoneEntry, bool) {
	core := s.core
	if core == nil || core.zoneCount == 0 {
		return sstZoneEntry{}, false
	}
	e := s.entry(blockIndex)
	if e.zoneCount == 0 {
		return sstZoneEntry{}, false
	}
	first := int(e.firstZone)
	lo, hi := 0, int(e.zoneCount)
	for lo < hi {
		mid := lo + (hi-lo)/2
		z := core.zoneEntry(first + mid)
		if z.columnID < id {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	if lo < int(e.zoneCount) {
		z := core.zoneEntry(first + lo)
		if z.columnID == id {
			return z, true
		}
	}
	return sstZoneEntry{}, false
}

// sstZoneMayMatch evaluates a compiled predicate against the SST-index-level
// zone map for one block BEFORE calling ReadBlock (pread). Returning false
// proves no row in the block can satisfy the predicate.
func (p *predicateProgram) sstZoneMayMatch(ctx context.Context, file *sstFile, blockIndex int) (bool, error) {
	if p == nil || p.trivialTrue || file == nil || file.core == nil || file.core.zoneCount == 0 {
		return true, nil
	}
	return p.sstZoneNodeMayMatch(ctx, file, blockIndex, p.root)
}

func (p *predicateProgram) sstZoneNodeMayMatch(ctx context.Context, file *sstFile, blockIndex int, index uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	node := &p.nodes[index]
	switch node.kind {
	case predBoolConst:
		return !node.bnull && node.bval, nil
	case predAnd, predOr:
		left, err := p.sstZoneNodeMayMatch(ctx, file, blockIndex, node.a)
		if err != nil || (node.kind == predAnd && !left) || (node.kind == predOr && left) {
			return left, err
		}
		return p.sstZoneNodeMayMatch(ctx, file, blockIndex, node.b)
	case predBoolColumn:
		z, ok := file.lookupZone(blockIndex, p.cols[node.col])
		if !ok {
			return true, nil
		}
		if z.flags&zoneHasNonNull == 0 {
			return false, nil
		}
		return z.kind != sstZoneInt || z.maxInt >= 1, nil
	case predNullTest:
		if node.boolChild || node.left.col < 0 {
			return true, nil
		}
		z, ok := file.lookupZone(blockIndex, p.cols[node.left.col])
		if !ok {
			return true, nil
		}
		if node.negated {
			// IS NOT NULL: requires at least one non-null value.
			return z.flags&zoneHasNonNull != 0, nil
		}
		// IS NULL: requires at least one null value.
		return z.flags&zoneHasNull != 0, nil
	case predCompare:
		column, constant, op := node.left, node.right, node.op
		if column.col < 0 {
			column, constant = constant, column
			op = flipPredOp(op)
		}
		if column.col < 0 || constant.cnst < 0 {
			return true, nil
		}
		val := p.consts[constant.cnst]
		if val.IsNull {
			return false, nil
		}
		z, ok := file.lookupZone(blockIndex, p.cols[column.col])
		if !ok {
			return true, nil
		}
		if z.flags&zoneHasNonNull == 0 {
			return false, nil
		}
		return zoneCompareMayMatch(file.core, z, node.family, op, val), nil
	case predIn:
		if node.negated || node.left.col < 0 {
			return true, nil
		}
		z, ok := file.lookupZone(blockIndex, p.cols[node.left.col])
		if !ok {
			return true, nil
		}
		if z.flags&zoneHasNonNull == 0 {
			return false, nil
		}
		for _, memberRef := range node.members {
			if memberRef.cnst < 0 {
				return true, nil
			}
			member := p.consts[memberRef.cnst]
			if member.IsNull {
				continue
			}
			if zoneCompareMayMatch(file.core, z, node.family, predEq, member) {
				return true, nil
			}
		}
		return false, nil
	}
	return true, nil
}

func flipPredOp(op predCmpOp) predCmpOp {
	switch op {
	case predLt:
		return predGt
	case predLe:
		return predGe
	case predGt:
		return predLt
	case predGe:
		return predLe
	default:
		return op
	}
}

func zoneCompareMayMatch(core *sstCore, z sstZoneEntry, family predFamily, op predCmpOp, val model.Value) bool {
	switch family {
	case predInt, predTimestamp, predBool:
		if z.kind != sstZoneInt {
			return true
		}
		v := val.IntVal
		switch op {
		case predEq:
			return v >= z.minInt && v <= z.maxInt
		case predNe:
			return z.minInt != v || z.maxInt != v
		case predLt:
			return z.minInt < v
		case predLe:
			return z.minInt <= v
		case predGt:
			return z.maxInt > v
		case predGe:
			return z.maxInt >= v
		}
	case predText:
		if z.kind != sstZoneBytes {
			return true
		}
		v := []byte(val.StrVal)
		minB := core.zoneSlice(z.minRange)
		hasMaxExact := z.flags&zoneMaxExact != 0
		hasMaxExcl := z.flags&zoneMaxExclusive != 0
		var maxB []byte
		if hasMaxExact || hasMaxExcl {
			maxB = core.zoneSlice(z.maxRange)
		}
		switch op {
		case predEq:
			if bytes.Compare(v, minB) < 0 {
				return false
			}
			if hasMaxExact && bytes.Compare(v, maxB) > 0 {
				return false
			}
			if hasMaxExcl && bytes.Compare(v, maxB) >= 0 {
				return false
			}
			return true
		case predNe:
			if z.flags&zoneMinExact != 0 && hasMaxExact && bytes.Equal(minB, v) && bytes.Equal(maxB, v) {
				return false
			}
			return true
		case predLt:
			return bytes.Compare(minB, v) < 0
		case predLe:
			return bytes.Compare(minB, v) <= 0
		case predGt:
			if hasMaxExact || hasMaxExcl {
				return bytes.Compare(maxB, v) > 0
			}
			return true
		case predGe:
			if hasMaxExact {
				return bytes.Compare(maxB, v) >= 0
			}
			if hasMaxExcl {
				return bytes.Compare(maxB, v) > 0
			}
			return true
		}
	}
	return true
}
