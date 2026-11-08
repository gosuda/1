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

const sstMagic = "NSSST003"
const sstHeaderSize = 80
const sstEntrySize = 72
const sstIndexHeaderSize = 16
const sstIndexHashSize = 32

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
	keys          []byte
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
	return &sstWriter{file: file, target: target, limits: limits, offset: sstHeaderSize, budget: encBudget{limit: limits.MaxIndexBytes}}, nil
}

func (w *sstWriter) Append(ctx context.Context, data []byte) (err error) {
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
	keyBytes := rangeKeyBytes
	for _, key := range [3][]byte{minimum, maximum, coverage} {
		if uint64(len(key)) > w.limits.MaxIndexBytes-keyBytes {
			return columnar.ErrBatchBudget
		}
		keyBytes += uint64(len(key))
	}
	indexSize := uint64(sstIndexHeaderSize+sstIndexHashSize) + uint64(len(w.entries)+1)*sstEntrySize + (uint64(len(w.ranges))+uint64(rangeCount))*sstRangeEntrySize + uint64(len(w.keys)) + keyBytes
	if indexSize > w.limits.MaxIndexBytes || uint64(len(data)) > w.limits.MaxBytes-w.offset || indexSize > w.limits.MaxBytes-w.offset-uint64(len(data)) {
		return columnar.ErrBatchBudget
	}
	if err := w.reserveEntry(); err != nil {
		return err
	}
	if err := w.reserveKeys(keyBytes); err != nil {
		return err
	}
	if err := w.reserveRangeEntries(rangeCount); err != nil {
		return err
	}
	keyMark, rangeMark := len(w.keys), len(w.ranges)
	defer func() {
		if err != nil {
			w.keys = w.keys[:keyMark]
			w.ranges = w.ranges[:rangeMark]
		}
	}()
	entry := sstEntry{offset: w.offset, length: uint64(len(data)), rows: uint32(block.Rows()), crc: binary.LittleEndian.Uint32(data[56:])}
	entry.minCommitTS, entry.maxCommitTS = firstTimestamp, firstTimestamp
	for row := 1; row < block.Rows(); row++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, _, timestamp, _, _, err := block.Version(row)
		if err != nil {
			return err
		}
		entry.minCommitTS = min(entry.minCommitTS, timestamp)
		entry.maxCommitTS = max(entry.maxCommitTS, timestamp)
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
	w.entries = append(w.entries, entry)
	w.offset += uint64(len(data))
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
	indexLength := uint64(sstIndexHeaderSize+sstIndexHashSize) + uint64(len(w.entries))*sstEntrySize + uint64(len(w.ranges))*sstRangeEntrySize + uint64(len(w.keys))
	output := io.NewOffsetWriter(w.file, int64(w.offset))
	hash := sha256.New()
	writer := io.MultiWriter(output, hash)
	var prefix [sstIndexHeaderSize]byte
	binary.LittleEndian.PutUint32(prefix[:], uint32(len(w.entries)))
	binary.LittleEndian.PutUint32(prefix[4:8], uint32(len(w.ranges)))
	binary.LittleEndian.PutUint64(prefix[8:], uint64(len(w.keys)))
	if _, err := writer.Write(prefix[:]); err != nil {
		return err
	}
	var encoded [sstEntrySize]byte
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
	if _, err := writer.Write(w.keys); err != nil {
		return err
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
}

func (limits sstLimits) validate() error {
	if limits.MaxBytes < sstHeaderSize+sstIndexHeaderSize+sstIndexHashSize || limits.MaxBytes > math.MaxInt64 || limits.MaxIndexBytes == 0 || limits.MaxIndexBytes > math.MaxUint32 || limits.MaxBlocks == 0 {
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
	return string(data[:8]) == sstMagic && data[60] <= byte(inactiveTier) && data[61] < MaxLevels && binary.LittleEndian.Uint16(data[62:]) == 0 && paxChecksum(data[:sstHeaderSize]) == binary.LittleEndian.Uint32(data[56:])
}
