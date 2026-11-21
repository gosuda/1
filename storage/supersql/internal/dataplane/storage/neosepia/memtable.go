package neosepia

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"slices"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/ordered"
)

const (
	defaultMemtableMaxBytes     = uint64(4 << 20)
	defaultMemtableMaxMutations = uint64(4096)
	minMemtableBudgetBytes      = uint64(16 << 10)
)

func memtableMaxBytes(limits Limits) uint64 {
	return min(defaultMemtableMaxBytes, limits.WriteBytes/4, limits.OverlayBytes/2, limits.ResidentBytes/4, limits.PublicationBytes/4)
}

func memtableEnabled(limits Limits) bool {
	return limits.MaxFiles > 4 && memtableMaxBytes(limits) >= minMemtableBudgetBytes
}

// memVersionKey orders versions in exact PAX/MVCC order:
// key ascending, commitTS descending, ordinal descending.
type memVersionKey struct {
	key      []byte
	commitTS model.Timestamp
	ordinal  uint64
}

func (a memVersionKey) Compare(b memVersionKey) int {
	if c := bytes.Compare(a.key, b.key); c != 0 {
		return c
	}
	if a.commitTS != b.commitTS {
		if a.commitTS > b.commitTS {
			return -1
		}
		return 1
	}
	if a.ordinal != b.ordinal {
		if a.ordinal > b.ordinal {
			return -1
		}
		return 1
	}
	return 0
}

type memEntry struct {
	target    storage.ObjectRef
	key       []byte
	endKey    []byte
	commitTS  model.Timestamp
	ordinal   uint64
	op        storage.MutationOp
	columnIDs []model.ColumnID
	values    []model.Value
	bytes     uint64
}

type memTargetState struct {
	entries ordered.Tree[memVersionKey, *memEntry]
	ranges  []*memEntry
	layouts map[uint64]storage.ObjectLayout
	bytes   uint64
	rows    uint64
}

func (s *memTargetState) cloneShallow() *memTargetState {
	if s == nil {
		return &memTargetState{layouts: make(map[uint64]storage.ObjectLayout)}
	}
	layouts := make(map[uint64]storage.ObjectLayout, len(s.layouts))
	for k, v := range s.layouts {
		layouts[k] = v
	}
	ranges := make([]*memEntry, len(s.ranges))
	copy(ranges, s.ranges)
	return &memTargetState{
		entries: s.entries,
		ranges:  ranges,
		layouts: layouts,
		bytes:   s.bytes,
		rows:    s.rows,
	}
}

type memCommitRecord struct {
	decision runDecision
	epoch    clock.Epoch
}

type memPAXBlock struct {
	data        []byte
	target      storage.ObjectRef
	rows        int
	columns     int
	descriptors []byte
	vis         []byte
	blobs       []toastBlob
	blobEntries []sstBlobEntry
	minCommitTS model.Timestamp
	maxCommitTS model.Timestamp
}

func (mb *memPAXBlock) openView(maxBytes uint64, memory *sourceMemory) (*paxBlock, error) {
	encoded := uint64(len(mb.data))
	if encoded > maxBytes {
		return nil, columnar.ErrBatchBudget
	}
	decodedBudget := maxBytes - encoded
	if decodedBudget < uint64(unsafe.Sizeof(paxBlock{})) {
		return nil, columnar.ErrBatchBudget
	}
	reserved := encoded + uint64(unsafe.Sizeof(paxBlock{}))
	if err := memory.reserve(reserved); err != nil {
		return nil, err
	}
	b := &paxBlock{
		data:         mb.data,
		memory:       memory,
		encodedBytes: encoded,
		target:       mb.target,
		rows:         mb.rows,
		columns:      mb.columns,
		descriptors:  mb.descriptors,
		vis:          mb.vis,
		budget:       decodedBudget,
		used:         uint64(unsafe.Sizeof(paxBlock{})),
	}
	if len(mb.blobs) > 0 {
		b.blobReader = memBlobReader{blobs: mb.blobs}
		b.blobs = mb.blobEntries
	}
	return b, nil
}

// memtableSnapshot is an immutable, concurrency-safe view of buffered committed
// mutations captured at OpenSnapshot time.
type memtableSnapshot struct {
	targets       map[objectGeneration]*memTargetState
	commits       []memCommitRecord
	firstPos      uint64
	lastPos       uint64
	firstCommitTS model.Timestamp
	lastCommitTS  model.Timestamp
	totalBytes    uint64
	totalMuts     uint64

	blockMu   sync.Mutex
	paxBlocks map[objectGeneration][]*memPAXBlock
}

// memtable buffers durably WAL-committed mutations in memory until flushed into
// coalesced NSSST004 PAX SST runs. Protected by Backend.mu.
type memtable struct {
	targets       map[objectGeneration]*memTargetState
	commits       []memCommitRecord
	firstPos      uint64
	lastPos       uint64
	firstCommitTS model.Timestamp
	lastCommitTS  model.Timestamp
	totalBytes    uint64
	totalMuts     uint64
	cachedSnap    *memtableSnapshot
}

func (m *memtable) empty() bool {
	return m == nil || len(m.commits) == 0
}

func (m *memtable) shouldFlush(limits Limits) bool {
	if m.empty() {
		return false
	}
	return m.totalBytes >= memtableMaxBytes(limits) || m.totalMuts >= defaultMemtableMaxMutations
}

func (m *memtable) pendingRunCount() uint64 {
	if m.empty() {
		return 0
	}
	return uint64(len(m.commits)) * max(uint64(1), uint64(len(m.targets)))
}

func (m *memtable) reset() {
	m.targets = nil
	m.commits = nil
	m.firstPos = 0
	m.lastPos = 0
	m.firstCommitTS = 0
	m.lastCommitTS = 0
	m.totalBytes = 0
	m.totalMuts = 0
	m.cachedSnap = nil
}

func (m *memtable) snapshot() *memtableSnapshot {
	if m.empty() {
		return nil
	}
	if m.cachedSnap != nil {
		return m.cachedSnap
	}
	targets := make(map[objectGeneration]*memTargetState, len(m.targets))
	for k, v := range m.targets {
		targets[k] = v.cloneShallow()
	}
	commits := make([]memCommitRecord, len(m.commits))
	copy(commits, m.commits)
	snap := &memtableSnapshot{
		targets:       targets,
		commits:       commits,
		firstPos:      m.firstPos,
		lastPos:       m.lastPos,
		firstCommitTS: m.firstCommitTS,
		lastCommitTS:  m.lastCommitTS,
		totalBytes:    m.totalBytes,
		totalMuts:     m.totalMuts,
	}
	m.cachedSnap = snap
	return snap
}

func (m *memtable) validateFence(fence storage.ReadFence) (handled bool, err error) {
	if m.empty() {
		return false, nil
	}
	if fence.LogPosition > m.lastPos {
		return true, storage.ErrSnapshotNotClosed
	}
	if fence.LogPosition >= m.firstPos {
		idx := int(fence.LogPosition - m.firstPos)
		if m.commits[idx].decision.CommitTS > fence.ClosedTS {
			return true, storage.ErrSnapshotNotClosed
		}
		if idx+1 < len(m.commits) && m.commits[idx+1].decision.CommitTS <= fence.ClosedTS {
			return true, storage.ErrSnapshotNotClosed
		}
		return true, nil
	}
	// fence.LogPosition < m.firstPos => ClosedTS must strictly precede firstCommitTS.
	if m.firstCommitTS <= fence.ClosedTS {
		return true, storage.ErrSnapshotNotClosed
	}
	return false, nil
}

func copyObjectLayout(ctx context.Context, layout storage.ObjectLayout) (storage.ObjectLayout, error) {
	cols := make([]storage.ColumnLayout, len(layout.Columns))
	for i, c := range layout.Columns {
		cols[i] = c
		if len(c.Children) > 0 {
			cols[i].Children = slices.Clone(c.Children)
		}
		if c.HasMissingValue {
			v, _, err := model.CopyValue(ctx, c.MissingValue, 64<<20)
			if err != nil {
				return storage.ObjectLayout{}, err
			}
			cols[i].MissingValue = v
		}
	}
	return storage.ObjectLayout{Target: layout.Target, Columns: cols}, nil
}

// stageBatch copies a durably WAL-committed MutationBatch into the memtable.
func (m *memtable) stageBatch(ctx context.Context, decision runDecision, epoch clock.Epoch, batch storage.MutationBatch, maxRowBytes uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.targets == nil {
		m.targets = make(map[objectGeneration]*memTargetState)
	}
	cursor, err := batch.OpenMutations(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cursor.Close() }()

	if maxRowBytes == 0 {
		maxRowBytes = 64 << 20
	}
	addedTargets := make(map[objectGeneration]*memTargetState)
	var addedBytes uint64
	var addedMuts uint64
	for {
		mut, nextErr := cursor.Next(ctx)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nextErr
		}
		key := targetGeneration(mut.Target)
		state := addedTargets[key]
		if state == nil {
			state = m.targets[key].cloneShallow()
			addedTargets[key] = state
		}
		if _, exists := state.layouts[mut.Target.DescriptorVersion]; !exists {
			layout, ok := batch.Layout(mut.Target)
			if !ok {
				return storage.ErrInvalidLayout
			}
			ownedLayout, err := copyObjectLayout(ctx, layout)
			if err != nil {
				return err
			}
			state.layouts[mut.Target.DescriptorVersion] = ownedLayout
		}
		entry := &memEntry{
			target:   mut.Target,
			key:      bytes.Clone(mut.Key),
			endKey:   bytes.Clone(mut.EndKey),
			commitTS: decision.CommitTS,
			ordinal:  mut.Ordinal,
			op:       mut.Op,
		}
		entryBytes := uint64(unsafe.Sizeof(memEntry{})) + uint64(len(entry.key)+len(entry.endKey))
		if mut.Op == storage.MutationUpsert {
			entry.columnIDs = slices.Clone(mut.ColumnIDs)
			entry.values = make([]model.Value, len(mut.Values))
			entryBytes += uint64(len(entry.columnIDs))*4 + uint64(len(entry.values))*uint64(unsafe.Sizeof(model.Value{}))
			for i, v := range mut.Values {
				owned, alloc, err := model.CopyValue(ctx, v, maxRowBytes)
				if err != nil {
					return err
				}
				entry.values[i] = owned
				entryBytes += alloc
			}
		}
		entry.bytes = entryBytes
		vk := memVersionKey{key: entry.key, commitTS: entry.commitTS, ordinal: entry.ordinal}
		nextTree, _, err := state.entries.Set(vk, entry, 64<<20)
		if err != nil {
			return err
		}
		state.entries = nextTree
		if entry.op == storage.MutationDeleteRange {
			state.ranges = append(state.ranges, entry)
		}
		state.bytes += entryBytes
		state.rows++
		addedBytes += entryBytes
		addedMuts++
	}
	for k, v := range addedTargets {
		m.targets[k] = v
	}
	if len(m.commits) == 0 {
		m.firstPos = decision.Position
		m.firstCommitTS = decision.CommitTS
	}
	m.lastPos = decision.Position
	m.lastCommitTS = decision.CommitTS
	m.commits = append(m.commits, memCommitRecord{decision: decision, epoch: epoch})
	m.totalBytes += addedBytes
	m.totalMuts += addedMuts
	m.cachedSnap = nil
	return nil
}

// lookup performs an O(log N) point read on the immutable memtableSnapshot.
// Returns (row, found, deleted, err). When found==true, the memtable is
// authoritative (strictly newer than all flushed SSTs).
func (snap *memtableSnapshot) lookup(ctx context.Context, request storage.ReadRequest, timestamp model.Timestamp, maxBytes uint64) (model.Row, bool, bool, error) {
	if snap == nil || len(snap.commits) == 0 || timestamp < snap.firstCommitTS {
		return model.Row{}, false, false, nil
	}
	if err := ctx.Err(); err != nil {
		return model.Row{}, false, false, err
	}
	state := snap.targets[targetGeneration(request.Target)]
	if state == nil || state.rows == 0 {
		return model.Row{}, false, false, nil
	}

	// Find newest visible point version for request.Key with commitTS <= timestamp.
	var point *memEntry
	cur := state.entries.Seek(memVersionKey{key: request.Key, commitTS: timestamp, ordinal: math.MaxUint64}, false)
	for {
		k, e, ok := cur.Next()
		if !ok || !bytes.Equal(k.key, request.Key) {
			break
		}
		if e.op == storage.MutationDeleteRange {
			continue
		}
		if e.commitTS <= timestamp {
			point = e
			break
		}
	}

	// Check if any visible range tombstone suppresses point (or older SST versions).
	var coveringRange *memEntry
	for _, r := range state.ranges {
		if r.commitTS > timestamp {
			continue
		}
		if bytes.Compare(r.key, request.Key) <= 0 && bytes.Compare(request.Key, r.endKey) < 0 {
			if coveringRange == nil || storage.CompareVersion(r.commitTS, r.ordinal, coveringRange.commitTS, coveringRange.ordinal) > 0 {
				coveringRange = r
			}
		}
	}

	if point == nil && coveringRange == nil {
		return model.Row{}, false, false, nil
	}
	if coveringRange != nil && (point == nil || storage.CompareVersion(coveringRange.commitTS, coveringRange.ordinal, point.commitTS, point.ordinal) > 0) {
		return model.Row{}, true, true, nil
	}
	if point.op == storage.MutationDelete {
		return model.Row{}, true, true, nil
	}
	if err := validateScanTarget(request.Target, point.target); err != nil {
		return model.Row{}, false, false, err
	}
	row, err := materializeMemPoint(ctx, point, request, maxBytes)
	if err != nil {
		return model.Row{}, false, false, err
	}
	return row, true, false, nil
}

func materializeMemPoint(ctx context.Context, entry *memEntry, request storage.ReadRequest, maxBytes uint64) (model.Row, error) {
	fixed := uint64(unsafe.Sizeof(model.Row{})) + uint64(len(entry.key))
	valueSize := uint64(unsafe.Sizeof(model.Value{}))
	if fixed > maxBytes || uint64(len(request.ColumnIDs)) > (maxBytes-fixed)/valueSize {
		return model.Row{}, columnar.ErrBatchBudget
	}
	used := fixed + uint64(len(request.ColumnIDs))*valueSize
	result := model.Row{
		Key:      make([]byte, len(entry.key)),
		CommitTS: entry.commitTS,
		Ordinal:  entry.ordinal,
		Values:   make([]model.Value, len(request.ColumnIDs)),
	}
	copy(result.Key, entry.key)
	for i, id := range request.ColumnIDs {
		layout, ok := scanColumnLayout(request.Layout, id)
		if !ok {
			return model.Row{}, storage.ErrInvalidLayout
		}
		var val model.Value
		pos, found := slices.BinarySearch(entry.columnIDs, id)
		if found {
			val = entry.values[pos]
		} else if layout.HasMissingValue {
			val = layout.MissingValue
		} else if layout.Nullable {
			val = model.NullVal(layout.Type.Storage)
		} else {
			return model.Row{}, storage.ErrCorruptStorage
		}
		if val.IsNull && !layout.Nullable {
			return model.Row{}, storage.ErrUnnormalizedBatch
		}
		owned, allocated, err := model.CopyValue(ctx, val, maxBytes-used)
		if errors.Is(err, model.ErrValueBudget) {
			return model.Row{}, errors.Join(columnar.ErrBatchBudget, err)
		}
		if err != nil {
			return model.Row{}, err
		}
		result.Values[i] = owned
		used += allocated
	}
	return result, nil
}

// targetBlocks lazily encodes the memtableSnapshot's entries for target into
// immutable in-memory PAX blocks for VectorizedScan and PlanScan.
func (snap *memtableSnapshot) targetBlocks(ctx context.Context, target storage.ObjectRef, limits blockLimits) ([]*memPAXBlock, error) {
	if snap == nil || len(snap.commits) == 0 {
		return nil, nil
	}
	key := targetGeneration(target)
	state := snap.targets[key]
	if state == nil || state.rows == 0 {
		return nil, nil
	}
	snap.blockMu.Lock()
	if cached, ok := snap.paxBlocks[key]; ok {
		snap.blockMu.Unlock()
		return cached, nil
	}
	snap.blockMu.Unlock()

	blocks, err := buildMemTargetBlocks(ctx, state, 0, limits)
	if err != nil {
		return nil, err
	}

	snap.blockMu.Lock()
	if snap.paxBlocks == nil {
		snap.paxBlocks = make(map[objectGeneration][]*memPAXBlock)
	}
	if cached, ok := snap.paxBlocks[key]; ok {
		snap.blockMu.Unlock()
		return cached, nil
	}
	snap.paxBlocks[key] = blocks
	snap.blockMu.Unlock()
	return blocks, nil
}

// buildMemTargetBlocks encodes all entries in state (optionally capped at
// maxCommitTS if > 0) into sorted PAX blocks with TOAST blob extraction.
func buildMemTargetBlocks(ctx context.Context, state *memTargetState, maxCommitTS model.Timestamp, limits blockLimits) ([]*memPAXBlock, error) {
	var blocks []*memPAXBlock
	var chunk []*memEntry
	var chunkDesc uint64
	var chunkBytes uint64

	flushChunk := func() error {
		if len(chunk) == 0 {
			return nil
		}
		layout, ok := state.layouts[chunkDesc]
		if !ok {
			return storage.ErrInvalidLayout
		}
		mb, err := encodeMemEntriesBlock(ctx, layout, chunk, limits)
		if err != nil {
			return err
		}
		blocks = append(blocks, mb)
		chunk = chunk[:0]
		chunkBytes = 0
		return nil
	}

	cur := state.entries.First(false)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		_, e, ok := cur.Next()
		if !ok {
			break
		}
		if maxCommitTS > 0 && e.commitTS > maxCommitTS {
			continue
		}
		estBytes := uint64(len(e.key)+len(e.endKey)) + paxVersionSize + 64
		if e.op == storage.MutationUpsert {
			for _, v := range e.values {
				switch v.Type {
				case model.TypeBytes:
					if len(v.BytesVal) > toastThreshold {
						estBytes += 13
					} else {
						estBytes += 4 + uint64(len(v.BytesVal))
					}
				case model.TypeString:
					if len(v.StrVal) > toastThreshold {
						estBytes += 13
					} else {
						estBytes += 4 + uint64(len(v.StrVal))
					}
				default:
					estBytes += 16
				}
			}
		}
		if len(chunk) > 0 && (e.target.DescriptorVersion != chunkDesc || len(chunk) >= limits.MaxRows || chunkBytes+estBytes > limits.MaxBytes/2) {
			if err := flushChunk(); err != nil {
				return nil, err
			}
		}
		chunkDesc = e.target.DescriptorVersion
		chunk = append(chunk, e)
		chunkBytes += estBytes
	}
	if err := flushChunk(); err != nil {
		return nil, err
	}
	return blocks, nil
}

func encodeMemEntriesBlock(ctx context.Context, layout storage.ObjectLayout, entries []*memEntry, limits blockLimits) (*memPAXBlock, error) {
	count := len(entries)
	cols := len(layout.Columns)
	layouts := make([]model.TypeLayout, cols+2)
	keyType := model.Type{ID: 17, Storage: model.TypeBytes}
	layouts[0] = model.TypeLayout{Type: keyType}
	layouts[1] = model.TypeLayout{Type: keyType}
	colIDs := make([]model.ColumnID, cols)
	for i, c := range layout.Columns {
		layouts[i+2] = model.TypeLayout{Type: c.Type, Children: c.Children}
		colIDs[i] = c.ID
	}
	builderBudget := max(limits.MaxDecodedBytes, uint64(64<<20))
	builder, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: count, MaxBytes: builderBudget})
	if err != nil {
		return nil, err
	}
	defer func() { _ = builder.Close() }()

	rowVals := make([]model.Value, cols+2)
	commitTS := make([]model.Timestamp, count)
	ordinals := make([]uint64, count)
	kinds := make([]storage.MutationOp, count)
	minTS := entries[0].commitTS
	maxTS := entries[0].commitTS
	hasEnd := false
	for i, e := range entries {
		commitTS[i] = e.commitTS
		ordinals[i] = e.ordinal
		kinds[i] = e.op
		minTS = min(minTS, e.commitTS)
		maxTS = max(maxTS, e.commitTS)
		if e.op == storage.MutationDeleteRange {
			hasEnd = true
		}
		rowVals[0] = model.BytesVal(e.key)
		rowVals[1] = model.BytesVal(e.endKey)
		if e.op == storage.MutationUpsert {
			copy(rowVals[2:], e.values)
		} else {
			for cIdx, col := range layout.Columns {
				rowVals[cIdx+2] = model.NullVal(col.Type.Storage)
			}
		}
		if err := builder.Append(ctx, rowVals); err != nil {
			return nil, err
		}
		clear(rowVals)
	}
	var batch columnar.Batch
	if err := builder.Publish(&batch); err != nil {
		return nil, err
	}
	versions := blockVersions{
		Keys:     batch.Columns[0],
		CommitTS: commitTS,
		Ordinals: ordinals,
		Kinds:    kinds,
	}
	if hasEnd {
		versions.EndKeys = batch.Columns[1]
	}
	payload := columnar.Batch{Length: count, Columns: batch.Columns[2:]}
	data, blobs, err := encodeBlockWithBlobs(ctx, layout.Target, versions, &payload, colIDs, limits)
	if err != nil {
		return nil, err
	}
	parsed, err := openBlock(data, limits)
	if err != nil {
		return nil, err
	}
	mb := &memPAXBlock{
		data:        parsed.data,
		target:      parsed.target,
		rows:        parsed.rows,
		columns:     parsed.columns,
		descriptors: parsed.descriptors,
		vis:         parsed.vis,
		blobs:       blobs,
		minCommitTS: minTS,
		maxCommitTS: maxTS,
	}
	if len(blobs) > 0 {
		mb.blobEntries = make([]sstBlobEntry, len(blobs))
		for i, b := range blobs {
			mb.blobEntries[i] = sstBlobEntry{offset: uint64(i), length: uint64(len(b.data)), crc: b.crc}
		}
	}
	_ = parsed.Close()
	return mb, nil
}

// flushToRuns writes commits [firstPos .. maxPos] from the memtable into
// coalesced NSSST004 PAX SST files and a run manifest (NSRUNS01 if single
// commit, NSRUNS02 if multiple), admits them into runs, and trims the memtable.
func (m *memtable) flushToRuns(ctx context.Context, runsRoot *os.Root, set *runSet, limits Limits, c *storageCipher, maxPos uint64) (err error) {
	if m.empty() {
		return nil
	}
	if maxPos == 0 || maxPos > m.lastPos {
		maxPos = m.lastPos
	}
	if maxPos < m.firstPos {
		return nil
	}
	flushCount := int(maxPos - m.firstPos + 1)
	flushedCommits := m.commits[:flushCount]
	maxCommitTS := flushedCommits[len(flushedCommits)-1].decision.CommitTS
	isPartial := flushCount < len(m.commits)

	prepLimits := limits.preparation()
	prepLimits.File.cipher = c
	prepared := &preparedRuns{root: runsRoot, metadataBytes: uint64(unsafe.Sizeof(preparedRuns{}))}
	defer func() {
		if err != nil {
			err = errors.Join(err, prepared.Close())
		}
	}()

	targetKeys := make([]objectGeneration, 0, len(m.targets))
	for k := range m.targets {
		targetKeys = append(targetKeys, k)
	}
	slices.SortFunc(targetKeys, func(a, b objectGeneration) int {
		if c := cmp.Compare(a.object, b.object); c != 0 {
			return c
		}
		return cmp.Compare(a.generation, b.generation)
	})

	scratch := make([]byte, 32<<10)
	for _, tk := range targetKeys {
		state := m.targets[tk]
		blocks, err := buildMemTargetBlocks(ctx, state, maxCommitTS, prepLimits.File.Block)
		if err != nil {
			return err
		}
		if len(blocks) == 0 {
			continue
		}
		// Use the latest DescriptorVersion among flushed blocks as the SST header target.
		sstTarget := blocks[0].target
		for _, b := range blocks[1:] {
			if b.target.DescriptorVersion > sstTarget.DescriptorVersion {
				sstTarget = b.target
			}
		}

		var file *os.File
		var writer *sstWriter
		var sstName string
		var sstRows uint64
		openNext := func() error {
			if uint64(len(prepared.files)) >= prepLimits.MaxRuns {
				return columnar.ErrBatchBudget
			}
			sstName = preparedRunName(maxPos, len(prepared.files))
			f, openErr := runsRoot.OpenFile(sstName, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
			if openErr != nil {
				return openErr
			}
			w, wErr := newSSTWriter(f, sstTarget, prepLimits.File)
			if wErr != nil {
				_ = f.Close()
				_ = runsRoot.Remove(sstName)
				return wErr
			}
			file, writer, sstRows = f, w, 0
			prepared.files = append(prepared.files, preparedRun{Name: sstName, Target: sstTarget})
			return nil
		}
		finishCurrent := func() error {
			if writer == nil {
				return nil
			}
			finishErr := writer.Finish(ctx)
			var info os.FileInfo
			var checksum [32]byte
			if finishErr == nil {
				info, finishErr = file.Stat()
			}
			if finishErr == nil {
				checksum, finishErr = checksumRunFile(ctx, file, info.Size(), scratch)
			}
			closeErr := file.Close()
			file, writer = nil, nil
			if err := errors.Join(finishErr, closeErr); err != nil {
				return err
			}
			prepared.files[len(prepared.files)-1] = preparedRun{
				Name:     sstName,
				Target:   sstTarget,
				Rows:     sstRows,
				Bytes:    uint64(info.Size()),
				Checksum: checksum,
			}
			return nil
		}

		for _, mb := range blocks {
			if writer == nil {
				if err := openNext(); err != nil {
					return err
				}
			}
			appendErr := writer.AppendWithBlobs(ctx, mb.data, mb.blobs)
			if errors.Is(appendErr, columnar.ErrBatchBudget) && sstRows > 0 {
				if err := finishCurrent(); err != nil {
					return err
				}
				if err := openNext(); err != nil {
					return err
				}
				appendErr = writer.AppendWithBlobs(ctx, mb.data, mb.blobs)
			}
			if appendErr != nil {
				_ = file.Close()
				return appendErr
			}
			sstRows += uint64(mb.rows)
		}
		if err := finishCurrent(); err != nil {
			return err
		}
	}

	decisions := make([]runDecision, len(flushedCommits))
	commitTSs := make([]model.Timestamp, len(flushedCommits))
	for i, c := range flushedCommits {
		decisions[i] = c.decision
		commitTSs[i] = c.decision.CommitTS
	}
	var admission *runAdmission
	if len(decisions) == 1 {
		if err := prepared.persistManifest(ctx, decisions[0], prepLimits.MetadataBytes); err != nil {
			return err
		}
		if err := syncCommitDirectory(runsRoot); err != nil {
			return err
		}
		admission, err = set.admitDecision(ctx, runsRoot, decisions[0], prepared.files, prepLimits.File, limits.PublicationBytes)
		if err != nil {
			return err
		}
	} else {
		coalesced, err := makeCoalescedDecision(decisions)
		if err != nil {
			return err
		}
		if err := prepared.persistCoalescedManifest(ctx, coalesced, prepLimits.MetadataBytes); err != nil {
			return err
		}
		if err := syncCommitDirectory(runsRoot); err != nil {
			return err
		}
		admission, err = set.admitCoalescedDecision(ctx, runsRoot, coalesced, commitTSs, prepared.files, prepLimits.File, limits.PublicationBytes)
		if err != nil {
			return err
		}
	}
	if err := admission.publish(ctx); err != nil {
		return errors.Join(err, admission.Close())
	}
	prepared.detach()

	if !isPartial {
		m.reset()
		return nil
	}
	// Trim flushed prefix [firstPos .. maxPos] and retain suffix [maxPos+1 .. lastPos].
	remainingCommits := slices.Clone(m.commits[flushCount:])
	nextTargets := make(map[objectGeneration]*memTargetState)
	var nextBytes, nextMuts uint64
	for tk, state := range m.targets {
		var kept *memTargetState
		cur := state.entries.First(false)
		for {
			k, e, ok := cur.Next()
			if !ok {
				break
			}
			if e.commitTS <= maxCommitTS {
				continue
			}
			if kept == nil {
				kept = &memTargetState{layouts: make(map[uint64]storage.ObjectLayout)}
			}
			if layout, ok := state.layouts[e.target.DescriptorVersion]; ok {
				kept.layouts[e.target.DescriptorVersion] = layout
			}
			t, _, err := kept.entries.Set(k, e, 64<<20)
			if err != nil {
				return err
			}
			kept.entries = t
			if e.op == storage.MutationDeleteRange {
				kept.ranges = append(kept.ranges, e)
			}
			kept.bytes += e.bytes
			kept.rows++
			nextBytes += e.bytes
			nextMuts++
		}
		if kept != nil {
			nextTargets[tk] = kept
		}
	}
	m.targets = nextTargets
	m.commits = remainingCommits
	m.firstPos = remainingCommits[0].decision.Position
	m.firstCommitTS = remainingCommits[0].decision.CommitTS
	m.lastPos = remainingCommits[len(remainingCommits)-1].decision.Position
	m.lastCommitTS = remainingCommits[len(remainingCommits)-1].decision.CommitTS
	m.totalBytes = nextBytes
	m.totalMuts = nextMuts
	m.cachedSnap = nil
	return nil
}
