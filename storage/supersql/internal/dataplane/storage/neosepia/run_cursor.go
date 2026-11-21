package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Leases are confined to one scan invocation. A selection can retain its block
// while the source advances, without copying column payloads or visibility keys.
type blockLease struct {
	block      *paxBlock
	references int
}

func (lease *blockLease) retain() *blockLease { lease.references++; return lease }
func (lease *blockLease) release() error {
	lease.references--
	if lease.references != 0 {
		return nil
	}
	block := lease.block
	lease.block = nil
	memory := block.memory
	err := block.Close()
	memory.release(uint64(unsafe.Sizeof(blockLease{})))
	return err
}

type versionRow struct {
	lease     *blockLease
	row       int
	key, end  []byte
	timestamp model.Timestamp
	ordinal   uint64
	kind      storage.MutationOp
}

// The run is borrowed and must remain pinned by the snapshot. Next borrows its
// row until Next/Close; retaining the lease extends only that block's lifetime.
// maxBytes is per block; boundary validation can hold two blocks. Shared source
// memory remains charged until the last lease releases each block.
var errBlockZonePruned = errors.New("block pruned by SST zone map")

type runVersionCursor struct {
	run                 *sstFile
	nextBlock, endBlock int
	maxBytes            uint64
	memory              *sourceMemory
	lease               *blockLease
	row                 int
	closed              bool
	reverse             *reverseRunState
	snapshotTS          model.Timestamp
	lowerKey, upperKey  []byte
	pointKey            []byte
	sequence            *runSequenceState
	mem                 *memCursorState
	predicate           *predicateProgram
	zonePruneSafe       bool
	yieldPruned         bool
}

func newRunVersionCursor(run *sstFile, first, end int, maxBytes uint64, memory *sourceMemory) (*runVersionCursor, error) {
	if run == nil {
		return nil, storage.ErrInvalidScanPlan
	}
	run.mu.RLock()
	defer run.mu.RUnlock()
	if run.core == nil {
		return nil, storage.ErrClosed
	}
	if first < 0 || first > end || end > run.count {
		return nil, storage.ErrInvalidScanPlan
	}
	if maxBytes <= uint64(unsafe.Sizeof(blockLease{})) {
		return nil, columnar.ErrBatchBudget
	}
	first, end = run.fragmentBlocks(first, end)
	return &runVersionCursor{run: run, nextBlock: first, endBlock: end, maxBytes: maxBytes, memory: memory}, nil
}

func (cursor *runVersionCursor) Next(ctx context.Context) (_ versionRow, err error) {
	if cursor.closed {
		return versionRow{}, io.EOF
	}
	defer func() {
		if err != nil && err != errBlockZonePruned {
			err = iterationError(err, cursor.Close())
		}
	}()
	if err := ctx.Err(); err != nil {
		return versionRow{}, err
	}
	if cursor.mem != nil {
		return cursor.nextMem(ctx)
	}
	if cursor.sequence != nil {
		return cursor.nextSequence(ctx)
	}
	if cursor.reverse != nil {
		return cursor.nextReverse(ctx)
	}
	for {
		if err := ctx.Err(); err != nil {
			return versionRow{}, err
		}
		for cursor.lease == nil || cursor.row >= cursor.lease.block.Rows() {
			if cursor.nextBlock == cursor.endBlock {
				return versionRow{}, io.EOF
			}
			if err := cursor.advanceBlock(ctx); err != nil {
				return versionRow{}, err
			}
		}
		key, end, timestamp, ordinal, kind, err := cursor.lease.block.Version(cursor.row)
		if err != nil {
			return versionRow{}, err
		}
		if cursor.pointKey != nil && bytes.Compare(key, cursor.pointKey) > 0 {
			return versionRow{}, io.EOF
		}
		result := versionRow{lease: cursor.lease, row: cursor.row, key: key, end: end, timestamp: timestamp, ordinal: ordinal, kind: kind}
		cursor.row++
		if !cursor.run.containsKey(key) || (len(cursor.lowerKey) != 0 && bytes.Compare(key, cursor.lowerKey) < 0) || (len(cursor.upperKey) != 0 && bytes.Compare(key, cursor.upperKey) >= 0) {
			continue
		}
		return result, nil
	}
}

func (cursor *runVersionCursor) readLease(ctx context.Context, index int) (*blockLease, error) {
	header := uint64(unsafe.Sizeof(blockLease{}))
	if err := cursor.memory.reserve(header); err != nil {
		return nil, err
	}
	block, err := cursor.run.ReadBlock(ctx, index, cursor.maxBytes-header, cursor.memory)
	if err != nil {
		cursor.memory.release(header)
		return nil, err
	}
	return &blockLease{block: block, references: 1}, nil
}

func (cursor *runVersionCursor) canZonePruneBlock(index int, checkPrev bool) bool {
	if cursor.predicate == nil || !cursor.zonePruneSafe || cursor.run == nil || cursor.run.rangeCount != 0 {
		return false
	}
	core := cursor.run.core
	if core == nil || core.zoneCount == 0 {
		return false
	}
	// Ensure no key in this block shares a boundary key with a neighboring block,
	// so skipping this block cannot expose an older version of the same key.
	if index+1 < cursor.run.count {
		if bytes.Compare(cursor.run.key(cursor.run.entry(index).maximum), cursor.run.key(cursor.run.entry(index+1).minimum)) >= 0 {
			return false
		}
	}
	if checkPrev && index > 0 {
		if bytes.Compare(cursor.run.key(cursor.run.entry(index-1).maximum), cursor.run.key(cursor.run.entry(index).minimum)) >= 0 {
			return false
		}
	}
	return true
}

func (cursor *runVersionCursor) advanceBlock(ctx context.Context) error {
	for cursor.nextBlock < cursor.endBlock {
		if err := ctx.Err(); err != nil {
			return err
		}
		skip := (cursor.snapshotTS > 0 && cursor.run.entry(cursor.nextBlock).minCommitTS > cursor.snapshotTS) ||
			(cursor.pointKey != nil && !cursor.run.entryMayContainPoint(cursor.nextBlock, cursor.pointKey, cursor.snapshotTS))
		zonePruned := false
		if !skip && cursor.canZonePruneBlock(cursor.nextBlock, false) {
			mayMatch, err := cursor.predicate.sstZoneMayMatch(ctx, cursor.run, cursor.nextBlock)
			if err != nil {
				return err
			}
			if !mayMatch {
				skip = true
				zonePruned = true
			}
		}
		if !skip {
			break
		}
		cursor.memory.observe(storage.ScanObservation{BlocksPruned: storage.ObservationCount{Value: 1, Known: true}})
		cursor.nextBlock++
		if zonePruned && cursor.yieldPruned {
			if cursor.lease != nil {
				if err := cursor.lease.release(); err != nil {
					cursor.lease = nil
					return err
				}
				cursor.lease = nil
				cursor.row = 0
			}
			return errBlockZonePruned
		}
	}
	if cursor.nextBlock == cursor.endBlock {
		return io.EOF
	}
	next, err := cursor.readLease(ctx, cursor.nextBlock)
	if err != nil {
		return err
	}
	if cursor.lease != nil {
		if err := checkRunBoundary(cursor.lease.block, next.block); err != nil {
			return errors.Join(err, next.release())
		}
		if err := cursor.lease.release(); err != nil {
			cursor.lease = nil
			return errors.Join(err, next.release())
		}
	}
	startRow := 0
	if cursor.pointKey != nil && !cursor.run.hasRangeCovering(cursor.pointKey) {
		idx, err := next.block.searchKey(cursor.pointKey)
		if err != nil {
			return errors.Join(err, next.release())
		}
		startRow = idx
	} else if len(cursor.lowerKey) != 0 && cursor.run.rangeCount == 0 {
		idx, err := next.block.searchKey(cursor.lowerKey)
		if err != nil {
			return errors.Join(err, next.release())
		}
		startRow = idx
	}
	cursor.lease = next
	cursor.row = startRow
	cursor.nextBlock++
	return nil
}

// attachScanZonePruning enables pre-ReadBlock SST-index zone pruning on every
// source whose blocks cannot shadow an older version in any other source.
func attachScanZonePruning(sources []*runVersionCursor, pred *predicateProgram) {
	if len(sources) == 1 {
		sources[0].predicate = pred
		sources[0].zonePruneSafe = true
		sources[0].yieldPruned = true
		return
	}
	type sourceSpan struct {
		known  bool
		minTS  model.Timestamp
		maxTS  model.Timestamp
		lower  []byte
		upper  []byte
	}
	spans := make([]sourceSpan, len(sources))
	for i, src := range sources {
		if src.run == nil || src.sequence != nil || src.run.rangeCount != 0 {
			continue
		}
		f := src.run
		if src.nextBlock >= src.endBlock {
			continue
		}
		minTS := f.entry(src.nextBlock).minCommitTS
		maxTS := f.entry(src.nextBlock).maxCommitTS
		for b := src.nextBlock + 1; b < src.endBlock; b++ {
			e := f.entry(b)
			minTS = min(minTS, e.minCommitTS)
			maxTS = max(maxTS, e.maxCommitTS)
		}
		spans[i] = sourceSpan{
			known: true,
			minTS: minTS,
			maxTS: maxTS,
			lower: f.lowerBound(),
			upper: f.upperBound(),
		}
	}
	for i, src := range sources {
		if !spans[i].known {
			continue
		}
		// Safe if every other source either has disjoint keys or strictly newer timestamps
		// (so skipping a block in src can never resurrect an older version in another source).
		safe := true
		for j := range sources {
			if i == j {
				continue
			}
			if !spans[j].known {
				safe = false
				break
			}
			if bytes.Compare(spans[i].upper, spans[j].lower) <= 0 || bytes.Compare(spans[j].upper, spans[i].lower) <= 0 {
				continue
			}
			if spans[i].maxTS < spans[j].minTS {
				continue
			}
			safe = false
			break
		}
		if safe {
			src.predicate = pred
			src.zonePruneSafe = true
			src.yieldPruned = true
		}
	}
}

func checkRunBoundary(previous, next *paxBlock) error {
	lastKey, _, lastTimestamp, lastOrdinal, _, err := previous.Version(previous.Rows() - 1)
	if err != nil {
		return err
	}
	firstKey, _, firstTimestamp, firstOrdinal, _, err := next.Version(0)
	if err != nil {
		return err
	}
	order := bytes.Compare(lastKey, firstKey)
	versionNotDescending := firstTimestamp > lastTimestamp || (firstTimestamp == lastTimestamp && firstOrdinal >= lastOrdinal)
	if order > 0 || (order == 0 && versionNotDescending) {
		return storage.ErrCorruptStorage
	}
	return nil
}

func (cursor *runVersionCursor) Close() error {
	if cursor.closed {
		return nil
	}
	cursor.closed = true
	cursor.run = nil
	cursor.memory = nil
	var err error
	if state := cursor.mem; state != nil {
		err = state.close()
		cursor.mem = nil
	}
	if state := cursor.sequence; state != nil {
		err = errors.Join(err, state.current.Close())
		cursor.sequence = nil
	}
	if state := cursor.reverse; state != nil {
		cursor.reverse = nil
		if state.group != nil {
			err = state.group.Close()
		}
	}
	if cursor.lease == nil {
		return err
	}
	lease := cursor.lease
	cursor.lease = nil
	return errors.Join(err, lease.release())
}

func iterationError(cause, cleanup error) error {
	if cleanup == nil {
		return cause
	}
	if cause == io.EOF {
		return cleanup
	}
	return errors.Join(cause, cleanup)
}
