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
	"sync/atomic"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var nextSSTFileID atomic.Uint64

// sstCore holds the immutable index, bloom filter, zone maps, blob directory,
// and underlying I/O runtime shared across hardlinked fragments of one inode.
type sstCore struct {
	fileID       uint64
	directFile   *os.File
	pool         *fdPool
	cache        *paxBlockCache
	target       storage.ObjectRef
	limits       sstLimits
	index        []byte
	entries      []byte
	ranges       []byte
	blobs        []byte
	zones        []byte
	keys         []byte
	zoneBytes    []byte
	bloom        []byte
	bloomProbes  uint32
	count        int
	rangeCount   int
	blobCount    int
	zoneCount    int
	rows         uint64
	tier         runTier
	level        uint8
	historyUntil model.Timestamp
	timeWindow   uint64
	entryBytes   int
	headerBytes  uint64
	coreRefs     atomic.Int64
}

func (c *sstCore) entry(i int) sstEntry {
	return decodeSSTEntry(c.entries[i*c.entryBytes : (i+1)*c.entryBytes])
}

func (c *sstCore) key(r columnar.Range) []byte {
	return c.keys[r.Offset : uint64(r.Offset)+uint64(r.Length)]
}

func (c *sstCore) blobEntry(i int) sstBlobEntry {
	return decodeSSTBlobEntry(c.blobs[i*sstBlobEntrySize : (i+1)*sstBlobEntrySize])
}

func (c *sstCore) zoneEntry(i int) sstZoneEntry {
	return decodeSSTZoneEntry(c.zones[i*sstZoneEntrySize : (i+1)*sstZoneEntrySize])
}

func (c *sstCore) zoneSlice(r columnar.Range) []byte {
	return c.zoneBytes[r.Offset : uint64(r.Offset)+uint64(r.Length)]
}

// Index entries and key fences borrow one bounded wire buffer, not one object
// per block. ReadBlock decodes no columns until its caller requests them.
// Hardlinked fragments share one *sstCore without duplicating FDs or indexes.
type sstFile struct {
	mu           sync.RWMutex
	core         *sstCore
	isFragment   bool
	target       storage.ObjectRef
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
	headerBytes := uint64(sstHeaderSize)
	entryBytes := uint64(sstEntrySizeV4)
	indexHeaderSize := uint64(sstIndexHeaderSizeV4)
	magic := string(header[:8])
	isV4 := false
	switch magic {
	case sstMagic:
		isV4 = true
		if _, err := file.ReadAt(header[64:], 64); err != nil {
			return nil, errors.Join(storage.ErrCorruptStorage, err)
		}
		if !sstHeaderValid(header[:]) {
			return nil, storage.ErrCorruptStorage
		}
	case sstMagicV3:
		entryBytes = sstEntrySizeV3
		indexHeaderSize = sstIndexHeaderSizeV3
		if _, err := file.ReadAt(header[64:], 64); err != nil {
			return nil, errors.Join(storage.ErrCorruptStorage, err)
		}
		if !sstHeaderValid(header[:]) {
			return nil, storage.ErrCorruptStorage
		}
	case sstMagicV2:
		headerBytes, entryBytes, indexHeaderSize = 64, sstEntrySizeV2, sstIndexHeaderSizeV3
		if binary.LittleEndian.Uint32(header[60:64]) != 0 || paxChecksum(header[:64]) != binary.LittleEndian.Uint32(header[56:60]) {
			return nil, storage.ErrCorruptStorage
		}
	default:
		return nil, storage.ErrCorruptStorage
	}
	target := storage.ObjectRef{
		ObjectID:          model.ObjectID(binary.LittleEndian.Uint64(header[8:])),
		Generation:        binary.LittleEndian.Uint64(header[16:]),
		DescriptorVersion: binary.LittleEndian.Uint64(header[24:]),
		EncodingVersion:   binary.LittleEndian.Uint32(header[32:]),
	}
	if err := target.Validate(); err != nil {
		return nil, errors.Join(storage.ErrCorruptStorage, err)
	}
	count := uint64(binary.LittleEndian.Uint32(header[36:]))
	offset, length := binary.LittleEndian.Uint64(header[40:]), binary.LittleEndian.Uint64(header[48:])
	if count == 0 || count > uint64(limits.MaxBlocks) || offset < headerBytes || offset > uint64(info.Size()) {
		return nil, storage.ErrCorruptStorage
	}
	if length != uint64(info.Size())-offset || length < indexHeaderSize+sstIndexHashSize+count*entryBytes {
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
	bodyEnd := length - sstIndexHashSize
	rangeCount := uint64(binary.LittleEndian.Uint32(index[4:8]))
	keyBytes := binary.LittleEndian.Uint64(index[8:16])
	var blobCount, zoneCount, bloomBytes, bloomProbes, zoneBytesLen uint64
	if isV4 {
		blobCount = uint64(binary.LittleEndian.Uint32(index[16:20]))
		zoneCount = uint64(binary.LittleEndian.Uint32(index[20:24]))
		bloomBytes = uint64(binary.LittleEndian.Uint32(index[24:28]))
		bloomProbes = uint64(binary.LittleEndian.Uint32(index[28:32]))
		zoneBytesLen = binary.LittleEndian.Uint64(index[32:40])
		if binary.LittleEndian.Uint64(index[40:48]) != 0 {
			return nil, storage.ErrCorruptStorage
		}
		if (bloomBytes == 0) != (bloomProbes == 0) || bloomBytes%8 != 0 || bloomProbes > 32 {
			return nil, storage.ErrCorruptStorage
		}
	}
	cursor := indexHeaderSize
	entriesEnd := cursor + count*entryBytes
	if entriesEnd > bodyEnd {
		return nil, storage.ErrCorruptStorage
	}
	entriesSlice := index[cursor:entriesEnd]
	cursor = entriesEnd

	if rangeCount > (bodyEnd-cursor)/sstRangeEntrySize {
		return nil, storage.ErrCorruptStorage
	}
	rangesEnd := cursor + rangeCount*sstRangeEntrySize
	rangesSlice := index[cursor:rangesEnd]
	cursor = rangesEnd

	if blobCount > (bodyEnd-cursor)/sstBlobEntrySize {
		return nil, storage.ErrCorruptStorage
	}
	blobsEnd := cursor + blobCount*sstBlobEntrySize
	blobsSlice := index[cursor:blobsEnd]
	cursor = blobsEnd

	if zoneCount > (bodyEnd-cursor)/sstZoneEntrySize {
		return nil, storage.ErrCorruptStorage
	}
	zonesEnd := cursor + zoneCount*sstZoneEntrySize
	zonesSlice := index[cursor:zonesEnd]
	cursor = zonesEnd

	if keyBytes > bodyEnd-cursor {
		return nil, storage.ErrCorruptStorage
	}
	keysEnd := cursor + keyBytes
	keysSlice := index[cursor:keysEnd]
	cursor = keysEnd

	if zoneBytesLen > bodyEnd-cursor {
		return nil, storage.ErrCorruptStorage
	}
	zoneBytesEnd := cursor + zoneBytesLen
	zoneBytesSlice := index[cursor:zoneBytesEnd]
	cursor = zoneBytesEnd

	if bloomBytes != bodyEnd-cursor {
		return nil, storage.ErrCorruptStorage
	}
	bloomSlice := index[cursor:bodyEnd]

	core := &sstCore{
		fileID:       nextSSTFileID.Add(1),
		directFile:   file,
		target:       target,
		limits:       limits,
		index:        index,
		entries:      entriesSlice,
		ranges:       rangesSlice,
		blobs:        blobsSlice,
		zones:        zonesSlice,
		keys:         keysSlice,
		zoneBytes:    zoneBytesSlice,
		bloom:        bloomSlice,
		bloomProbes:  uint32(bloomProbes),
		count:        int(count),
		rangeCount:   int(rangeCount),
		blobCount:    int(blobCount),
		zoneCount:    int(zoneCount),
		entryBytes:   int(entryBytes),
		headerBytes:  headerBytes,
		tier:         runTier(header[60]),
		level:        header[61],
		historyUntil: model.Timestamp(binary.LittleEndian.Uint64(header[64:])),
		timeWindow:   binary.LittleEndian.Uint64(header[72:]),
	}
	core.coreRefs.Store(1)
	if core.historyUntil < 0 || (core.tier == inactiveTier && (core.historyUntil == 0 || core.level != 0)) || (core.tier == activeTier && (core.historyUntil != 0 || core.timeWindow != 0)) {
		return nil, storage.ErrCorruptStorage
	}
	result := newSSTFileFromCore(core, false)
	if err := result.validate(ctx, offset); err != nil {
		return nil, err
	}
	result.rows = core.rows
	return result, nil
}

func newSSTFileFromCore(core *sstCore, isFragment bool) *sstFile {
	return &sstFile{
		core:         core,
		isFragment:   isFragment,
		target:       core.target,
		count:        core.count,
		rangeCount:   core.rangeCount,
		rows:         core.rows,
		tier:         core.tier,
		level:        core.level,
		historyUntil: core.historyUntil,
		timeWindow:   core.timeWindow,
		references:   1,
		entryBytes:   core.entryBytes,
		headerBytes:  core.headerBytes,
	}
}

func (s *sstFile) attachRuntime(pool *fdPool, cache *paxBlockCache) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return
	}
	s.core.cache = cache
	if pool != nil && s.physicalPath != "" && s.core.pool == nil {
		f := s.core.directFile
		s.core.directFile = nil
		s.core.pool = pool
		if f != nil {
			pool.adopt(s.core.fileID, f)
		}
	}
}

func (s *sstFile) residentCost() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cost := uint64(unsafe.Sizeof(sstFile{})) + uint64(cap(s.descriptor.LowerKey)+cap(s.descriptor.UpperKey))
	if s.core != nil && !s.isFragment {
		cost += uint64(cap(s.core.index))
	}
	return cost
}

func (s *sstFile) isClosed() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.core == nil
}

func (s *sstFile) readAtLocked(p []byte, off int64) (int, error) {
	core := s.core
	if core == nil {
		return 0, storage.ErrClosed
	}
	if core.pool != nil {
		entry, err := core.pool.acquire(core.fileID, s.physicalPath)
		if err != nil {
			return 0, err
		}
		defer core.pool.release(entry)
		return entry.file.ReadAt(p, off)
	}
	if core.directFile == nil {
		return 0, storage.ErrClosed
	}
	return core.directFile.ReadAt(p, off)
}

func (s *sstFile) statFile() (os.FileInfo, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	core := s.core
	if core == nil {
		return nil, storage.ErrClosed
	}
	if core.pool != nil {
		entry, err := core.pool.acquire(core.fileID, s.physicalPath)
		if err != nil {
			return nil, err
		}
		defer core.pool.release(entry)
		return entry.file.Stat()
	}
	if core.directFile == nil {
		return nil, storage.ErrClosed
	}
	return core.directFile.Stat()
}

func (s *sstFile) readBlob(ctx context.Context, entry sstBlobEntry, dst []byte, memory *sourceMemory) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.core == nil {
		return storage.ErrClosed
	}
	if uint64(len(dst)) != entry.length {
		return storage.ErrCorruptStorage
	}
	if len(dst) == 0 {
		return nil
	}
	n, err := s.readAtLocked(dst, int64(entry.offset))
	memory.observe(storage.ScanObservation{
		ReadBytes: storage.ObservationCount{Value: uint64(n), Known: true},
	})
	if err != nil {
		return errors.Join(storage.ErrCorruptStorage, err)
	}
	if uint64(n) != entry.length {
		return storage.ErrCorruptStorage
	}
	return nil
}

func decodeSSTEntry(data []byte) sstEntry {
	e := sstEntry{
		offset: binary.LittleEndian.Uint64(data),
		length: binary.LittleEndian.Uint64(data[8:]),
		rows:   binary.LittleEndian.Uint32(data[16:]),
		crc:    binary.LittleEndian.Uint32(data[20:]),
	}
	ranges := [4]*columnar.Range{&e.minimum, &e.maximum, &e.coverage, &e.prefixCoverage}
	for i, r := range ranges {
		r.Offset, r.Length = binary.LittleEndian.Uint32(data[24+i*8:]), binary.LittleEndian.Uint32(data[28+i*8:])
	}
	if len(data) == sstEntrySizeV2 {
		// V2 has no timestamp summaries; unknown bounds must never prune data.
		e.maxCommitTS = model.Timestamp(math.MaxInt64)
	} else {
		e.minCommitTS = model.Timestamp(binary.LittleEndian.Uint64(data[56:]))
		e.maxCommitTS = model.Timestamp(binary.LittleEndian.Uint64(data[64:]))
	}
	if len(data) >= sstEntrySizeV4 {
		e.firstBlob = binary.LittleEndian.Uint32(data[72:])
		e.blobCount = binary.LittleEndian.Uint32(data[76:])
		e.firstZone = binary.LittleEndian.Uint32(data[80:])
		e.zoneCount = binary.LittleEndian.Uint32(data[84:])
	}
	return e
}

func (s *sstFile) entry(i int) sstEntry { return s.core.entry(i) }
func (s *sstFile) key(r columnar.Range) []byte {
	return s.core.key(r)
}

func (s *sstFile) validate(ctx context.Context, indexOffset uint64) error {
	core := s.core
	offset := core.headerBytes
	var previous sstEntry
	var rows uint64
	var expectedBlob uint64
	var expectedZone uint64
	for i := range core.count {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := core.entry(i)
		if (core.entryBytes >= sstEntrySizeV3 && e.minCommitTS <= 0) || e.maxCommitTS < e.minCommitTS {
			return storage.ErrCorruptStorage
		}
		if e.rows == 0 || uint64(e.rows) > uint64(core.limits.Block.MaxRows) {
			return storage.ErrCorruptStorage
		}
		rows += uint64(e.rows)
		if e.offset != offset || e.length == 0 || e.length%8 != 0 || e.length > core.limits.Block.MaxBytes || e.length > indexOffset-offset {
			return storage.ErrCorruptStorage
		}
		for _, r := range [4]columnar.Range{e.minimum, e.maximum, e.coverage, e.prefixCoverage} {
			if uint64(r.Offset)+uint64(r.Length) > uint64(len(core.keys)) {
				return storage.ErrCorruptStorage
			}
		}
		if bytes.Compare(core.key(e.minimum), core.key(e.maximum)) > 0 || bytes.Compare(core.key(e.maximum), core.key(e.coverage)) > 0 {
			return storage.ErrCorruptStorage
		}
		prefix := core.key(e.coverage)
		if i > 0 {
			if bytes.Compare(core.key(previous.maximum), core.key(e.minimum)) > 0 {
				return storage.ErrCorruptStorage
			}
			if bytes.Compare(core.key(previous.prefixCoverage), prefix) > 0 {
				prefix = core.key(previous.prefixCoverage)
			}
		}
		if !bytes.Equal(prefix, core.key(e.prefixCoverage)) {
			return storage.ErrCorruptStorage
		}
		offset += e.length
		if uint64(e.firstBlob) != expectedBlob || uint64(e.blobCount) > uint64(core.blobCount)-expectedBlob {
			return storage.ErrCorruptStorage
		}
		for b := uint64(0); b < uint64(e.blobCount); b++ {
			be := core.blobEntry(int(expectedBlob + b))
			padded := aligned8(be.length)
			if be.offset != offset || padded > indexOffset-offset {
				return storage.ErrCorruptStorage
			}
			offset += padded
		}
		expectedBlob += uint64(e.blobCount)
		if uint64(e.firstZone) != expectedZone || uint64(e.zoneCount) > uint64(core.zoneCount)-expectedZone {
			return storage.ErrCorruptStorage
		}
		var prevCol model.ColumnID
		for z := uint64(0); z < uint64(e.zoneCount); z++ {
			ze := core.zoneEntry(int(expectedZone + z))
			if ze.columnID == 0 || (z > 0 && ze.columnID <= prevCol) {
				return storage.ErrCorruptStorage
			}
			prevCol = ze.columnID
			switch ze.kind {
			case sstZoneInt:
				if ze.flags&zoneHasNonNull != 0 && ze.minInt > ze.maxInt {
					return storage.ErrCorruptStorage
				}
			case sstZoneBytes:
				if uint64(ze.minRange.Offset)+uint64(ze.minRange.Length) > uint64(len(core.zoneBytes)) ||
					uint64(ze.maxRange.Offset)+uint64(ze.maxRange.Length) > uint64(len(core.zoneBytes)) {
					return storage.ErrCorruptStorage
				}
			default:
				return storage.ErrCorruptStorage
			}
		}
		expectedZone += uint64(e.zoneCount)
		previous = e
	}
	if offset != indexOffset || expectedBlob != uint64(core.blobCount) || expectedZone != uint64(core.zoneCount) {
		return storage.ErrCorruptStorage
	}
	core.rows = rows
	return s.validateRanges(ctx, rows)
}

// mayContainKey checks the SST bloom filter (and range tombstone coverage) to
// skip disk I/O for absent point keys.
func (s *sstFile) mayContainKey(key []byte) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	core := s.core
	if core == nil {
		return true
	}
	if core.rangeCount > 0 {
		maxEnd := core.key(s.rangeEntry(0).end)
		if bytes.Compare(key, maxEnd) < 0 {
			return true
		}
	}
	return bloomMayContain(core.bloom, core.bloomProbes, key)
}

// hasRangeCovering reports whether any range tombstone in the SST covers key.
func (s *sstFile) hasRangeCovering(key []byte) bool {
	if s.rangeCount == 0 {
		return false
	}
	// Ranges are sorted descending by end key.
	for i := 0; i < s.rangeCount; i++ {
		r := s.rangeEntry(i)
		if bytes.Compare(s.key(r.end), key) <= 0 {
			return false
		}
		if bytes.Compare(s.key(r.start), key) <= 0 {
			return true
		}
	}
	return false
}

// Candidates returns a conservative half-open block interval. Coverage is checked
// again per block, avoiding payload reads for holes under a long range tombstone.
func (s *sstFile) Candidates(ctx context.Context, key []byte, timestamp model.Timestamp) (int, int, error) {
	if err := ctx.Err(); err != nil {
		return 0, 0, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.core == nil {
		return 0, 0, storage.ErrClosed
	}
	if s.core.rangeCount == 0 || bytes.Compare(key, s.core.key(s.rangeEntry(0).end)) >= 0 {
		if !bloomMayContain(s.core.bloom, s.core.bloomProbes, key) {
			return 0, 0, nil
		}
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
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		first++
	}
	for first < lo && !s.entryMayContainPoint(lo-1, key, timestamp) {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		lo--
	}
	return first, lo, nil
}

func (s *sstFile) entryMayContainPoint(index int, key []byte, timestamp model.Timestamp) bool {
	entry := s.entry(index)
	return (timestamp == 0 || entry.minCommitTS <= timestamp) && bytes.Compare(s.key(entry.minimum), key) <= 0 && bytes.Compare(key, s.key(entry.coverage)) <= 0
}

func (s *sstFile) blockBlobs(e sstEntry) []sstBlobEntry {
	if e.blobCount == 0 {
		return nil
	}
	blobs := make([]sstBlobEntry, int(e.blobCount))
	for i := range blobs {
		blobs[i] = s.core.blobEntry(int(e.firstBlob) + i)
	}
	return blobs
}

// The budget covers newly owned block bytes plus the decoder's cached payloads.
func (s *sstFile) ReadBlock(ctx context.Context, index int, maxBytes uint64, memory *sourceMemory) (*paxBlock, error) {
	return s.readBlock(ctx, index, maxBytes, memory, true)
}

func (s *sstFile) readBlockUncached(ctx context.Context, index int, maxBytes uint64, memory *sourceMemory) (*paxBlock, error) {
	return s.readBlock(ctx, index, maxBytes, memory, false)
}

func (s *sstFile) readBlock(ctx context.Context, index int, maxBytes uint64, memory *sourceMemory, useCache bool) (*paxBlock, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	core := s.core
	if core == nil {
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
	limits := core.limits.Block
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

	cacheKey := blockCacheKey{fileID: core.fileID, blockIndex: index}
	if useCache {
		if cached := core.cache.get(cacheKey); cached != nil {
			block := &paxBlock{
				data:         cached.data,
				shared:       cached,
				memory:       memory,
				encodedBytes: e.length,
				target:       cached.target,
				rows:         cached.rows,
				columns:      cached.columns,
				descriptors:  cached.descriptors,
				vis:          cached.vis,
				budget:       limits.MaxDecodedBytes,
				used:         uint64(unsafe.Sizeof(paxBlock{})),
			}
			if e.blobCount > 0 {
				block.blobReader = s
				block.blobs = s.blockBlobs(e)
			}
			reserved = 0
			return block, nil
		}
	}

	data = make([]byte, e.length)
	n, readErr := s.readAtLocked(data, int64(e.offset))
	memory.observe(storage.ScanObservation{
		ReadBytes:  storage.ObservationCount{Value: uint64(n), Known: true},
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
	if e.blobCount > 0 {
		block.blobReader = s
		block.blobs = s.blockBlobs(e)
	}
	reserved = 0
	if validateScanTarget(s.target, block.Target()) != nil || (s.entryBytes == sstEntrySizeV2 && block.Target() != s.target) || uint64(block.Rows()) != uint64(e.rows) {
		return nil, errors.Join(storage.ErrCorruptStorage, block.Close())
	}
	if useCache && core.cache != nil {
		cached := &cachedPAXBlock{
			data:        block.data,
			target:      block.target,
			rows:        block.rows,
			columns:     block.columns,
			descriptors: block.descriptors,
			vis:         block.vis,
			sizeBytes:   uint64(len(block.data)) + uint64(unsafe.Sizeof(cachedPAXBlock{})),
		}
		core.cache.put(cacheKey, cached)
		block.shared = cached
	}
	return block, nil
}

func (s *sstFile) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.core == nil {
		return nil
	}
	s.references--
	if s.references != 0 {
		return nil
	}
	core := s.core
	reclaim := s.reclaimPath
	s.core = nil
	var err error
	if core.coreRefs.Add(-1) == 0 {
		if core.pool != nil {
			err = errors.Join(err, core.pool.evict(core.fileID))
		}
		if core.directFile != nil {
			err = errors.Join(err, core.directFile.Close())
			core.directFile = nil
		}
		core.index, core.entries, core.ranges, core.blobs, core.zones, core.keys, core.zoneBytes, core.bloom = nil, nil, nil, nil, nil, nil, nil, nil
	}
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
	if s.core != nil {
		s.reclaimPath = s.physicalPath
	}
}
