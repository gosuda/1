package neosepia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	sstMagic             = "NSSST004"
	sstMagicV3           = "NSSST003"
	sstMagicV2           = "NSSST002"
	sstHeaderSize        = 80
	sstEntrySizeV2       = 56
	sstEntrySizeV3       = 72
	sstEntrySizeV4       = 88
	sstEntrySize         = sstEntrySizeV4
	sstIndexHeaderSizeV3 = 16
	sstIndexHeaderSizeV4 = 48
	sstIndexHeaderSize   = sstIndexHeaderSizeV4
	sstIndexHashSize     = 32
)

type sstLimits struct {
	Block         blockLimits
	MaxBytes      uint64
	MaxIndexBytes uint64
	MaxBlocks     uint32
}

type sstEntry struct {
	offset, length                             uint64
	rows, crc                                  uint32
	minimum, maximum, coverage, prefixCoverage columnar.Range
	minCommitTS, maxCommitTS                   model.Timestamp
	firstBlob, blobCount                       uint32
	firstZone, zoneCount                       uint32
}

// Coverage includes range tombstone ends, not just the last stored start key.
// Prefix maxima locate tombstones whose starts precede a point lookup's block.
type sstWriter struct {
	file          *os.File
	target        storage.ObjectRef
	limits        sstLimits
	offset        uint64
	entries       []sstEntry
	ranges        []sstRangeEntry
	blobs         []sstBlobEntry
	zones         []sstZoneEntry
	keys          []byte
	zoneBytes     []byte
	bloom         bloomBuilder
	budget        encBudget
	failure       error
	finished      bool
	lastTimestamp model.Timestamp
	lastOrdinal   uint64
	tier          runTier
	level         uint8
	historyUntil  model.Timestamp
	timeWindow    uint64
}

// The caller owns the empty file and publishes it only after Finish succeeds.
func newSSTWriter(file *os.File, target storage.ObjectRef, limits sstLimits) (*sstWriter, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != 0 {
		return nil, storage.ErrInvalidTarget
	}
	return &sstWriter{
		file:   file,
		target: target,
		limits: limits,
		offset: sstHeaderSize,
		bloom:  newBloomBuilder(limits.MaxIndexBytes),
		budget: encBudget{limit: limits.MaxIndexBytes},
	}, nil
}

func (w *sstWriter) Append(ctx context.Context, data []byte) error {
	return w.AppendWithBlobs(ctx, data, nil)
}

func (w *sstWriter) AppendWithBlobs(ctx context.Context, data []byte, blobs []toastBlob) (err error) {
	if w.finished {
		return storage.ErrClosed
	}
	if w.failure != nil {
		return w.failure
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if uint64(len(w.entries)) >= uint64(w.limits.MaxBlocks) {
		return columnar.ErrBatchBudget
	}
	if uint64(len(w.blobs))+uint64(len(blobs)) > math.MaxUint32 {
		return columnar.ErrBatchBudget
	}
	block, err := openBlock(data, w.limits.Block)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, block.Close()) }()
	if err := validateScanTarget(w.target, block.Target()); err != nil {
		return err
	}
	minimum, _, firstTimestamp, firstOrdinal, _, err := block.Version(0)
	if err != nil {
		return err
	}
	maximum, _, lastTimestamp, lastOrdinal, _, err := block.Version(block.Rows() - 1)
	if err != nil {
		return err
	}
	coverage, rangeCount, rangeKeyBytes, err := w.measureRanges(ctx, block, maximum)
	if err != nil {
		return err
	}
	if len(w.entries) != 0 {
		comparison := bytes.Compare(w.key(w.entries[len(w.entries)-1].maximum), minimum)
		versionNotDescending := firstTimestamp > w.lastTimestamp || (firstTimestamp == w.lastTimestamp && firstOrdinal >= w.lastOrdinal)
		if comparison > 0 || (comparison == 0 && versionNotDescending) {
			return errPAXEncode
		}
	}
	var blobDiskBytes uint64
	for _, b := range blobs {
		padded := aligned8(uint64(len(b.data)))
		if padded > w.limits.MaxBytes-blobDiskBytes {
			return columnar.ErrBatchBudget
		}
		blobDiskBytes += padded
	}
	keyBytes := rangeKeyBytes
	for _, key := range [3][]byte{minimum, maximum, coverage} {
		if uint64(len(key)) > w.limits.MaxIndexBytes-keyBytes {
			return columnar.ErrBatchBudget
		}
		keyBytes += uint64(len(key))
	}
	// Extract SST-index-level per-block column zone summaries.
	zonePoolMark := len(w.zoneBytes)
	maxZonePool := w.limits.MaxIndexBytes / 4
	blockZones, err := extractBlockZones(ctx, block, blobs, &w.zoneBytes, maxZonePool)
	if err != nil {
		w.zoneBytes = w.zoneBytes[:zonePoolMark]
		return err
	}
	if uint64(len(w.zones))+uint64(len(blockZones)) > math.MaxUint32 {
		w.zoneBytes = w.zoneBytes[:zonePoolMark]
		blockZones = nil
	}
	baseIndexSize := uint64(sstIndexHeaderSizeV4+sstIndexHashSize) +
		uint64(len(w.entries)+1)*sstEntrySizeV4 +
		(uint64(len(w.ranges))+uint64(rangeCount))*sstRangeEntrySize +
		(uint64(len(w.blobs))+uint64(len(blobs)))*sstBlobEntrySize +
		uint64(len(w.keys)) + keyBytes
	zoneIndexExtra := (uint64(len(w.zones))+uint64(len(blockZones)))*sstZoneEntrySize + uint64(len(w.zoneBytes))
	if baseIndexSize+zoneIndexExtra > w.limits.MaxIndexBytes {
		// Drop this block's optional zone summaries rather than failing a tight index budget.
		w.zoneBytes = w.zoneBytes[:zonePoolMark]
		blockZones = nil
		zoneIndexExtra = uint64(len(w.zones))*sstZoneEntrySize + uint64(len(w.zoneBytes))
	}
	indexSize := baseIndexSize + zoneIndexExtra
	payloadBytes := uint64(len(data))
	if payloadBytes > w.limits.MaxBytes-blobDiskBytes {
		w.zoneBytes = w.zoneBytes[:zonePoolMark]
		return columnar.ErrBatchBudget
	}
	payloadBytes += blobDiskBytes
	if indexSize > w.limits.MaxIndexBytes || payloadBytes > w.limits.MaxBytes-w.offset || indexSize > w.limits.MaxBytes-w.offset-payloadBytes {
		w.zoneBytes = w.zoneBytes[:zonePoolMark]
		return columnar.ErrBatchBudget
	}
	if err := w.reserveEntry(); err != nil {
		w.zoneBytes = w.zoneBytes[:zonePoolMark]
		return err
	}
	if err := w.reserveKeys(keyBytes); err != nil {
		w.zoneBytes = w.zoneBytes[:zonePoolMark]
		return err
	}
	if err := w.reserveRangeEntries(rangeCount); err != nil {
		w.zoneBytes = w.zoneBytes[:zonePoolMark]
		return err
	}
	keyMark, rangeMark, blobMark, zoneMark := len(w.keys), len(w.ranges), len(w.blobs), len(w.zones)
	defer func() {
		if err != nil {
			w.keys = w.keys[:keyMark]
			w.ranges = w.ranges[:rangeMark]
			w.blobs = w.blobs[:blobMark]
			w.zones = w.zones[:zoneMark]
			w.zoneBytes = w.zoneBytes[:zonePoolMark]
		}
	}()
	entry := sstEntry{
		offset:    w.offset,
		length:    uint64(len(data)),
		rows:      uint32(block.Rows()),
		crc:       binary.LittleEndian.Uint32(data[56:]),
		firstBlob: uint32(len(w.blobs)),
		blobCount: uint32(len(blobs)),
		firstZone: uint32(len(w.zones)),
		zoneCount: uint32(len(blockZones)),
	}
	entry.minCommitTS, entry.maxCommitTS = firstTimestamp, firstTimestamp
	for row := 0; row < block.Rows(); row++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, _, timestamp, _, kind, err := block.Version(row)
		if err != nil {
			return err
		}
		entry.minCommitTS = min(entry.minCommitTS, timestamp)
		entry.maxCommitTS = max(entry.maxCommitTS, timestamp)
		if kind != storage.MutationDeleteRange {
			w.bloom.addKey(key)
		}
	}
	entry.minimum = w.appendKey(minimum)
	entry.maximum = w.appendKey(maximum)
	entry.coverage = w.appendKey(coverage)
	entry.prefixCoverage = entry.coverage
	if len(w.entries) != 0 {
		previous := w.entries[len(w.entries)-1].prefixCoverage
		if bytes.Compare(w.key(previous), coverage) > 0 {
			entry.prefixCoverage = previous
		}
	}
	if rangeCount != 0 {
		if err := w.appendRanges(ctx, block); err != nil {
			return err
		}
	}
	n, writeErr := w.file.WriteAt(data, int64(w.offset))
	if writeErr != nil {
		w.failure = writeErr
		return writeErr
	}
	if n != len(data) {
		w.failure = io.ErrShortWrite
		return w.failure
	}
	nextOffset := w.offset + uint64(len(data))
	var zeroPad [8]byte
	for _, b := range blobs {
		if err := ctx.Err(); err != nil {
			w.failure = err
			return err
		}
		if len(b.data) > 0 {
			written, bErr := w.file.WriteAt(b.data, int64(nextOffset))
			if bErr != nil {
				w.failure = bErr
				return bErr
			}
			if written != len(b.data) {
				w.failure = io.ErrShortWrite
				return w.failure
			}
		}
		padded := aligned8(uint64(len(b.data)))
		if pad := padded - uint64(len(b.data)); pad > 0 {
			written, pErr := w.file.WriteAt(zeroPad[:pad], int64(nextOffset+uint64(len(b.data))))
			if pErr != nil {
				w.failure = pErr
				return pErr
			}
			if uint64(written) != pad {
				w.failure = io.ErrShortWrite
				return w.failure
			}
		}
		w.blobs = append(w.blobs, sstBlobEntry{
			offset: nextOffset,
			length: uint64(len(b.data)),
			crc:    b.crc,
		})
		nextOffset += padded
	}
	w.zones = append(w.zones, blockZones...)
	w.entries = append(w.entries, entry)
	w.offset = nextOffset
	w.lastTimestamp, w.lastOrdinal = lastTimestamp, lastOrdinal
	return nil
}

func (w *sstWriter) reserveEntry() error {
	if len(w.entries) < cap(w.entries) {
		return nil
	}
	capacity := max(4, cap(w.entries)*2)
	capacity = min(capacity, int(w.limits.MaxBlocks))
	if err := w.budget.chargeMul(uint64(capacity), uint64(unsafe.Sizeof(sstEntry{}))); err != nil {
		return err
	}
	entries := make([]sstEntry, len(w.entries), capacity)
	copy(entries, w.entries)
	w.entries = entries
	return nil
}

func (w *sstWriter) reserveKeys(extra uint64) error {
	needed := uint64(len(w.keys)) + extra
	if needed <= uint64(cap(w.keys)) {
		return nil
	}
	capacity := max(256, uint64(cap(w.keys))*2, needed)
	capacity = min(capacity, w.limits.MaxIndexBytes)
	if capacity < needed {
		return columnar.ErrBatchBudget
	}
	if err := w.budget.charge(capacity); err != nil {
		return err
	}
	keys := make([]byte, len(w.keys), int(capacity))
	copy(keys, w.keys)
	w.keys = keys
	return nil
}

func (w *sstWriter) appendKey(key []byte) columnar.Range {
	r := columnar.Range{Offset: uint32(len(w.keys)), Length: uint32(len(key))}
	w.keys = append(w.keys, key...)
	return r
}

func (w *sstWriter) key(r columnar.Range) []byte {
	return w.keys[r.Offset : uint64(r.Offset)+uint64(r.Length)]
}

func (w *sstWriter) Finish(ctx context.Context) error {
	if w.finished {
		return storage.ErrClosed
	}
	if w.failure != nil {
		return w.failure
	}
	if len(w.entries) == 0 {
		return storage.ErrBatchEmpty
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.finished = true
	if err := w.sortRanges(ctx); err != nil {
		return err
	}
	baseIndexLen := uint64(sstIndexHeaderSizeV4+sstIndexHashSize) +
		uint64(len(w.entries))*sstEntrySizeV4 +
		uint64(len(w.ranges))*sstRangeEntrySize +
		uint64(len(w.blobs))*sstBlobEntrySize +
		uint64(len(w.zones))*sstZoneEntrySize +
		uint64(len(w.keys)) +
		uint64(len(w.zoneBytes))
	var maxBloom uint64
	if baseIndexLen < w.limits.MaxIndexBytes && baseIndexLen < w.limits.MaxBytes-w.offset {
		maxBloom = min(w.limits.MaxIndexBytes-baseIndexLen, w.limits.MaxBytes-w.offset-baseIndexLen)
		// Also leave room for sstCore/sstFile overhead on open.
		overhead := uint64(unsafe.Sizeof(sstFile{})) + uint64(unsafe.Sizeof(sstCore{}))
		if maxBloom > overhead {
			maxBloom -= overhead
		} else {
			maxBloom = 0
		}
	}
	bloomBits, bloomProbes := w.bloom.finish(maxBloom)
	indexLength := baseIndexLen + uint64(len(bloomBits))

	output := io.NewOffsetWriter(w.file, int64(w.offset))
	hash := sha256.New()
	writer := io.MultiWriter(output, hash)
	var prefix [sstIndexHeaderSizeV4]byte
	binary.LittleEndian.PutUint32(prefix[0:4], uint32(len(w.entries)))
	binary.LittleEndian.PutUint32(prefix[4:8], uint32(len(w.ranges)))
	binary.LittleEndian.PutUint64(prefix[8:16], uint64(len(w.keys)))
	binary.LittleEndian.PutUint32(prefix[16:20], uint32(len(w.blobs)))
	binary.LittleEndian.PutUint32(prefix[20:24], uint32(len(w.zones)))
	binary.LittleEndian.PutUint32(prefix[24:28], uint32(len(bloomBits)))
	binary.LittleEndian.PutUint32(prefix[28:32], bloomProbes)
	binary.LittleEndian.PutUint64(prefix[32:40], uint64(len(w.zoneBytes)))
	binary.LittleEndian.PutUint64(prefix[40:48], 0)
	if _, err := writer.Write(prefix[:]); err != nil {
		return err
	}
	var encoded [sstEntrySizeV4]byte
	for _, entry := range w.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		encodeSSTEntry(encoded[:], entry)
		if _, err := writer.Write(encoded[:]); err != nil {
			return err
		}
	}
	var encodedRange [sstRangeEntrySize]byte
	for _, entry := range w.ranges {
		if err := ctx.Err(); err != nil {
			return err
		}
		encodeSSTRangeEntry(encodedRange[:], entry)
		if _, err := writer.Write(encodedRange[:]); err != nil {
			return err
		}
	}
	var encodedBlob [sstBlobEntrySize]byte
	for _, entry := range w.blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		encodeSSTBlobEntry(encodedBlob[:], entry)
		if _, err := writer.Write(encodedBlob[:]); err != nil {
			return err
		}
	}
	var encodedZone [sstZoneEntrySize]byte
	for _, entry := range w.zones {
		if err := ctx.Err(); err != nil {
			return err
		}
		encodeSSTZoneEntry(encodedZone[:], entry)
		if _, err := writer.Write(encodedZone[:]); err != nil {
			return err
		}
	}
	if _, err := writer.Write(w.keys); err != nil {
		return err
	}
	if len(w.zoneBytes) > 0 {
		if _, err := writer.Write(w.zoneBytes); err != nil {
			return err
		}
	}
	if len(bloomBits) > 0 {
		if _, err := writer.Write(bloomBits); err != nil {
			return err
		}
	}
	var digest [32]byte
	hash.Sum(digest[:0])
	if _, err := output.Write(digest[:]); err != nil {
		return err
	}
	var header [sstHeaderSize]byte
	copy(header[:], sstMagic)
	binary.LittleEndian.PutUint64(header[8:], uint64(w.target.ObjectID))
	binary.LittleEndian.PutUint64(header[16:], w.target.Generation)
	binary.LittleEndian.PutUint64(header[24:], w.target.DescriptorVersion)
	binary.LittleEndian.PutUint32(header[32:], w.target.EncodingVersion)
	binary.LittleEndian.PutUint32(header[36:], uint32(len(w.entries)))
	binary.LittleEndian.PutUint64(header[40:], w.offset)
	binary.LittleEndian.PutUint64(header[48:], indexLength)
	header[60], header[61] = byte(w.tier), w.level
	binary.LittleEndian.PutUint64(header[64:], uint64(w.historyUntil))
	binary.LittleEndian.PutUint64(header[72:], w.timeWindow)
	binary.LittleEndian.PutUint32(header[56:], paxChecksum(header[:]))
	n, err := w.file.WriteAt(header[:], 0)
	if err != nil {
		return err
	}
	if n != len(header) {
		return io.ErrShortWrite
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return w.file.Sync()
}

func encodeSSTEntry(data []byte, e sstEntry) {
	binary.LittleEndian.PutUint64(data, e.offset)
	binary.LittleEndian.PutUint64(data[8:], e.length)
	binary.LittleEndian.PutUint32(data[16:], e.rows)
	binary.LittleEndian.PutUint32(data[20:], e.crc)
	for i, r := range [4]columnar.Range{e.minimum, e.maximum, e.coverage, e.prefixCoverage} {
		binary.LittleEndian.PutUint32(data[24+i*8:], r.Offset)
		binary.LittleEndian.PutUint32(data[28+i*8:], r.Length)
	}
	binary.LittleEndian.PutUint64(data[56:], uint64(e.minCommitTS))
	binary.LittleEndian.PutUint64(data[64:], uint64(e.maxCommitTS))
	binary.LittleEndian.PutUint32(data[72:], e.firstBlob)
	binary.LittleEndian.PutUint32(data[76:], e.blobCount)
	binary.LittleEndian.PutUint32(data[80:], e.firstZone)
	binary.LittleEndian.PutUint32(data[84:], e.zoneCount)
}

func encodeSSTBlobEntry(data []byte, b sstBlobEntry) {
	binary.LittleEndian.PutUint64(data[0:8], b.offset)
	binary.LittleEndian.PutUint64(data[8:16], b.length)
	binary.LittleEndian.PutUint32(data[16:20], b.crc)
	binary.LittleEndian.PutUint32(data[20:24], 0)
}

func decodeSSTBlobEntry(data []byte) sstBlobEntry {
	return sstBlobEntry{
		offset: binary.LittleEndian.Uint64(data[0:8]),
		length: binary.LittleEndian.Uint64(data[8:16]),
		crc:    binary.LittleEndian.Uint32(data[16:20]),
	}
}

func (limits sstLimits) validate() error {
	if limits.MaxBytes < sstHeaderSize+sstIndexHeaderSizeV4+sstIndexHashSize || limits.MaxBytes > math.MaxInt64 || limits.MaxIndexBytes == 0 || limits.MaxIndexBytes > math.MaxUint32 || limits.MaxBlocks == 0 {
		return columnar.ErrBatchBudget
	}
	if limits.MaxIndexBytes > uint64(math.MaxInt) || uint64(limits.MaxBlocks) > uint64(math.MaxInt) {
		return columnar.ErrBatchBudget
	}
	if limits.Block.MaxRows <= 0 || limits.Block.MaxBytes == 0 || limits.Block.MaxBytes > uint64(math.MaxInt) || limits.Block.MaxDecodedBytes == 0 || limits.Block.MaxDecodedBytes > uint64(math.MaxInt) {
		return columnar.ErrBatchBudget
	}
	return nil
}

func sstHeaderValid(data []byte) bool {
	if len(data) != sstHeaderSize {
		return false
	}
	magic := string(data[:8])
	return (magic == sstMagic || magic == sstMagicV3) && data[60] <= byte(inactiveTier) && data[61] < MaxLevels && binary.LittleEndian.Uint16(data[62:]) == 0 && paxChecksum(data[:sstHeaderSize]) == binary.LittleEndian.Uint32(data[56:])
}
