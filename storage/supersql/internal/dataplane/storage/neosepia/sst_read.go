package neosepia

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Index entries and key fences borrow one bounded wire buffer, not one object
// per block. ReadBlock decodes no columns until its caller requests them.
type sstFile struct {
	mu           sync.RWMutex
	file         *os.File
	target       storage.ObjectRef
	limits       sstLimits
	index        []byte
	entries      []byte
	keys         []byte
	count        int
	rangeCount   int
	rows         uint64
	descriptor   preparedRun
	physicalPath string
	reclaimPath  string
	tier         runTier
	level        uint8
	historyUntil model.Timestamp
	timeWindow   uint64
	references   uint64
	entryBytes   int
	headerBytes  uint64
}

// The handle takes ownership of file only on success.
func openSST(ctx context.Context, file *os.File, limits sstLimits) (*sstFile, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := limits.validate(); err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 64 || uint64(info.Size()) > limits.MaxBytes {
		return nil, storage.ErrCorruptStorage
	}
	var header [sstHeaderSize]byte
	if _, err := file.ReadAt(header[:64], 0); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	headerBytes, entryBytes := uint64(sstHeaderSize), uint64(sstEntrySize)
	switch string(header[:8]) {
	case sstMagic:
		if _, err := file.ReadAt(header[64:], 64); err != nil {
			return nil, errors.Join(storage.ErrCorruptStorage, err)
		}
		if !sstHeaderValid(header[:]) {
			return nil, storage.ErrCorruptStorage
		}
	case "NSSST002":
		headerBytes, entryBytes = 64, 56
		if binary.LittleEndian.Uint32(header[60:64]) != 0 || paxChecksum(header[:64]) != binary.LittleEndian.Uint32(header[56:60]) {
			return nil, storage.ErrCorruptStorage
		}
	default:
		return nil, storage.ErrCorruptStorage
	}
	target := storage.ObjectRef{ObjectID: model.ObjectID(binary.LittleEndian.Uint64(header[8:])), Generation: binary.LittleEndian.Uint64(header[16:]), DescriptorVersion: binary.LittleEndian.Uint64(header[24:]), EncodingVersion: binary.LittleEndian.Uint32(header[32:])}
	if err := target.Validate(); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	count := uint64(binary.LittleEndian.Uint32(header[36:]))
	offset, length := binary.LittleEndian.Uint64(header[40:]), binary.LittleEndian.Uint64(header[48:])
	if count == 0 || count > uint64(limits.MaxBlocks) || offset < headerBytes || offset > uint64(info.Size()) {
		return nil, storage.ErrCorruptStorage
	}
	if length != uint64(info.Size())-offset || length < sstIndexHeaderSize+sstIndexHashSize+count*entryBytes {
		return nil, storage.ErrCorruptStorage
	}
	overhead := uint64(unsafe.Sizeof(sstFile{}))
	if overhead > limits.MaxIndexBytes || length > limits.MaxIndexBytes-overhead {
		return nil, columnar.ErrBatchBudget
	}
	index := make([]byte, length)
	if _, err := file.ReadAt(index, int64(offset)); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	digest := sha256.Sum256(index[:len(index)-sstIndexHashSize])
	if digest != [32]byte(index[len(index)-sstIndexHashSize:]) || uint64(binary.LittleEndian.Uint32(index)) != count {
		return nil, storage.ErrCorruptStorage
	}
	keyBytes := binary.LittleEndian.Uint64(index[8:])
	rangeOffset := uint64(sstIndexHeaderSize) + count*entryBytes
	rangeCount := uint64(binary.LittleEndian.Uint32(index[4:8]))
	if rangeCount > (length-rangeOffset-sstIndexHashSize)/sstRangeEntrySize {
		return nil, storage.ErrCorruptStorage
	}
	keyOffset := rangeOffset + rangeCount*sstRangeEntrySize
	if keyBytes != length-keyOffset-sstIndexHashSize {
		return nil, storage.ErrCorruptStorage
	}
	result := &sstFile{file: file, target: target, limits: limits, index: index, entries: index[sstIndexHeaderSize:rangeOffset], keys: index[keyOffset : len(index)-sstIndexHashSize], count: int(count), rangeCount: int(rangeCount)}
	result.entryBytes, result.headerBytes = int(entryBytes), headerBytes
	result.references = 1
	result.tier, result.level = runTier(header[60]), header[61]
	result.historyUntil = model.Timestamp(binary.LittleEndian.Uint64(header[64:]))
	result.timeWindow = binary.LittleEndian.Uint64(header[72:])
	if result.historyUntil < 0 || (result.tier == inactiveTier && (result.historyUntil == 0 || result.level != 0)) || (result.tier == activeTier && (result.historyUntil != 0 || result.timeWindow != 0)) {
		return nil, storage.ErrCorruptStorage
	}
	if err := result.validate(ctx, offset); err != nil {
		return nil, err
	}
	return result, nil
}

func decodeSSTEntry(data []byte) sstEntry {
	e := sstEntry{offset: binary.LittleEndian.Uint64(data), length: binary.LittleEndian.Uint64(data[8:]), rows: binary.LittleEndian.Uint32(data[16:]), crc: binary.LittleEndian.Uint32(data[20:])}
	ranges := [4]*columnar.Range{&e.minimum, &e.maximum, &e.coverage, &e.prefixCoverage}
	for i, r := range ranges {
		r.Offset, r.Length = binary.LittleEndian.Uint32(data[24+i*8:]), binary.LittleEndian.Uint32(data[28+i*8:])
	}
	if len(data) == 56 {
		// V2 has no timestamp summaries; unknown bounds must never prune data.
		e.maxCommitTS = model.Timestamp(math.MaxInt64)
	} else {
		e.minCommitTS = model.Timestamp(binary.LittleEndian.Uint64(data[56:]))
		e.maxCommitTS = model.Timestamp(binary.LittleEndian.Uint64(data[64:]))
	}
	return e
}

func (s *sstFile) entry(i int) sstEntry { return decodeSSTEntry(s.entries[i*s.entryBytes:(i+1)*s.entryBytes]) }
func (s *sstFile) key(r columnar.Range) []byte {
	return s.keys[r.Offset : uint64(r.Offset)+uint64(r.Length)]
}

func (s *sstFile) validate(ctx context.Context, indexOffset uint64) error {
	offset := s.headerBytes
	var previous sstEntry
	var rows uint64
	for i := range s.count {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := s.entry(i)
		if (s.entryBytes == sstEntrySize && e.minCommitTS <= 0) || e.maxCommitTS < e.minCommitTS {
			return storage.ErrCorruptStorage
		}
		if e.rows == 0 || uint64(e.rows) > uint64(s.limits.Block.MaxRows) {
			return storage.ErrCorruptStorage
		}
		rows += uint64(e.rows)
		if e.offset != offset || e.length == 0 || e.length%8 != 0 || e.length > s.limits.Block.MaxBytes || e.length > indexOffset-offset {
			return storage.ErrCorruptStorage
		}
		for _, r := range [4]columnar.Range{e.minimum, e.maximum, e.coverage, e.prefixCoverage} {
			if uint64(r.Offset)+uint64(r.Length) > uint64(len(s.keys)) {
				return storage.ErrCorruptStorage
			}
		}
		if bytes.Compare(s.key(e.minimum), s.key(e.maximum)) > 0 || bytes.Compare(s.key(e.maximum), s.key(e.coverage)) > 0 {
			return storage.ErrCorruptStorage
		}
		prefix := s.key(e.coverage)
		if i > 0 {
			if bytes.Compare(s.key(previous.maximum), s.key(e.minimum)) > 0 {
				return storage.ErrCorruptStorage
			}
			if bytes.Compare(s.key(previous.prefixCoverage), prefix) > 0 {
				prefix = s.key(previous.prefixCoverage)
			}
		}
		if !bytes.Equal(prefix, s.key(e.prefixCoverage)) {
			return storage.ErrCorruptStorage
		}
		offset += e.length
		previous = e
	}
	if offset != indexOffset {
		return storage.ErrCorruptStorage
	}
	s.rows = rows
	return s.validateRanges(ctx, rows)
}

// Candidates returns a conservative half-open block interval. Coverage is checked
// again per block, avoiding payload reads for holes under a long range tombstone.
func (s *sstFile) Candidates(ctx context.Context, key []byte, timestamp model.Timestamp) (int, int, error) {
	if err := ctx.Err(); err != nil { return 0, 0, err }
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file == nil {
		return 0, 0, storage.ErrClosed
	}
	lo, hi := 0, s.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(s.key(s.entry(mid).prefixCoverage), key) < 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	first := lo
	lo, hi = first, s.count
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(s.key(s.entry(mid).minimum), key) <= 0 {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	first, lo = s.fragmentBlocks(first, lo)
	for first < lo && !s.entryMayContainPoint(first, key, timestamp) {
		if err := ctx.Err(); err != nil { return 0, 0, err }
		first++
	}
	for first < lo && !s.entryMayContainPoint(lo-1, key, timestamp) {
		if err := ctx.Err(); err != nil { return 0, 0, err }
		lo--
	}
	return first, lo, nil
}

func (s *sstFile) entryMayContainPoint(index int, key []byte, timestamp model.Timestamp) bool {
	entry := s.entry(index)
	return (timestamp == 0 || entry.minCommitTS <= timestamp) && bytes.Compare(s.key(entry.minimum), key) <= 0 && bytes.Compare(key, s.key(entry.coverage)) <= 0
}


// The budget covers newly owned block bytes plus the decoder's cached payloads.
func (s *sstFile) ReadBlock(ctx context.Context, index int, maxBytes uint64, memory *sourceMemory) (*paxBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.file == nil {
		return nil, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if index < 0 || index >= s.count {
		return nil, storage.ErrInvalidTarget
	}
	e := s.entry(index)
	if e.length > maxBytes {
		return nil, columnar.ErrBatchBudget
	}
	limits := s.limits.Block
	limits.MaxDecodedBytes = min(limits.MaxDecodedBytes, maxBytes-e.length)
	if limits.MaxDecodedBytes < uint64(unsafe.Sizeof(paxBlock{})) {
		return nil, columnar.ErrBatchBudget
	}
	reserved := e.length + uint64(unsafe.Sizeof(paxBlock{}))
	if err := memory.reserve(reserved); err != nil {
		return nil, err
	}
	var data []byte
	defer func() {
		if reserved != 0 {
			data = nil
			memory.release(reserved)
		}
	}()
	data = make([]byte, e.length)
	n, readErr := s.file.ReadAt(data, int64(e.offset))
	memory.observe(storage.ScanObservation{
		ReadBytes: storage.ObservationCount{Value: uint64(n), Known: true},
		BlocksRead: storage.ObservationCount{Value: 1, Known: true},
	})
	if readErr != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, readErr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(data) < paxHeaderSize || binary.LittleEndian.Uint32(data[56:]) != e.crc {
		return nil, storage.ErrCorruptStorage
	}
	block, err := openBlock(data, limits)
	if err != nil {
		return nil, err
	}
	data = nil
	block.memory = memory
	block.encodedBytes = e.length
	reserved = 0
	if validateScanTarget(s.target, block.Target()) != nil || (s.entryBytes == 56 && block.Target() != s.target) || uint64(block.Rows()) != uint64(e.rows) {
		return nil, errors.Join(storage.ErrCorruptStorage, block.Close())
	}
	return block, nil
}

func (s *sstFile) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	s.references--
	if s.references != 0 {
		return nil
	}
	file := s.file
	reclaim := s.reclaimPath
	s.file = nil
	s.index, s.entries, s.keys = nil, nil, nil
	err := file.Close()
	if reclaim != "" {
		removeErr := os.Remove(reclaim)
		if !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}
	return err
}

func (s *sstFile) markReclaim() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		s.reclaimPath = s.physicalPath
	}
}
