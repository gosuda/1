package neosepia

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type memRangeItem struct {
	start, end []byte
	timestamp  model.Timestamp
	ordinal    uint64
}

type memCursorState struct {
	blocks    []*memPAXBlock
	direction storage.ScanDirection
	nextBlock int
	row       int
	lease     *blockLease
	// Reverse scan state
	revRanges  []memRangeItem
	revRangeAt int
	revGroup   []int // row indices within current reverse key group
	revGroupAt int
	revBlock   int
	revRow     int
	revPoint   versionRow
	revHasPt   bool
}

func newMemVersionCursor(blocks []*memPAXBlock, direction storage.ScanDirection, maxBytes uint64, memory *sourceMemory) *runVersionCursor {
	state := &memCursorState{
		blocks:    blocks,
		direction: direction,
		revBlock:  len(blocks) - 1,
		revRow:    -1,
	}
	if direction == storage.ScanReverse {
		// Collect range tombstones across all memtable blocks, sorted by
		// end DESC, version DESC, start ASC (identical to SST range index).
		for _, mb := range blocks {
			for r := 0; r < mb.rows; r++ {
				rec := mb.vis[r*paxVersionSize:]
				op := storage.MutationOp(rec[16])
				if op != storage.MutationDeleteRange {
					continue
				}
				kOff := uint64(mb.vis[r*paxVersionSize+24]) | uint64(mb.vis[r*paxVersionSize+25])<<8 | uint64(mb.vis[r*paxVersionSize+26])<<16 | uint64(mb.vis[r*paxVersionSize+27])<<24
				kLen := uint64(mb.vis[r*paxVersionSize+28]) | uint64(mb.vis[r*paxVersionSize+29])<<8 | uint64(mb.vis[r*paxVersionSize+30])<<16 | uint64(mb.vis[r*paxVersionSize+31])<<24
				eOff := uint64(mb.vis[r*paxVersionSize+32]) | uint64(mb.vis[r*paxVersionSize+33])<<8 | uint64(mb.vis[r*paxVersionSize+34])<<16 | uint64(mb.vis[r*paxVersionSize+35])<<24
				eLen := uint64(mb.vis[r*paxVersionSize+36]) | uint64(mb.vis[r*paxVersionSize+37])<<8 | uint64(mb.vis[r*paxVersionSize+38])<<16 | uint64(mb.vis[r*paxVersionSize+39])<<24
				ts := model.Timestamp(int64(uint64(rec[0]) | uint64(rec[1])<<8 | uint64(rec[2])<<16 | uint64(rec[3])<<24 | uint64(rec[4])<<32 | uint64(rec[5])<<40 | uint64(rec[6])<<48 | uint64(rec[7])<<56))
				ord := uint64(rec[8]) | uint64(rec[9])<<8 | uint64(rec[10])<<16 | uint64(rec[11])<<24 | uint64(rec[12])<<32 | uint64(rec[13])<<40 | uint64(rec[14])<<48 | uint64(rec[15])<<56
				state.revRanges = append(state.revRanges, memRangeItem{
					start:     mb.vis[kOff : kOff+kLen],
					end:       mb.vis[eOff : eOff+eLen],
					timestamp: ts,
					ordinal:   ord,
				})
			}
		}
		slices.SortFunc(state.revRanges, func(a, b memRangeItem) int {
			if c := bytes.Compare(b.end, a.end); c != 0 {
				return c
			}
			if c := storage.CompareVersion(b.timestamp, b.ordinal, a.timestamp, a.ordinal); c != 0 {
				return c
			}
			return bytes.Compare(a.start, b.start)
		})
	}
	return &runVersionCursor{mem: state, maxBytes: maxBytes, memory: memory}
}

func (cursor *runVersionCursor) openMemLease(mb *memPAXBlock) (*blockLease, error) {
	header := uint64(unsafe.Sizeof(blockLease{}))
	if err := cursor.memory.reserve(header); err != nil {
		return nil, err
	}
	block, err := mb.openView(cursor.maxBytes-header, cursor.memory)
	if err != nil {
		cursor.memory.release(header)
		return nil, err
	}
	return &blockLease{block: block, references: 1}, nil
}

func (cursor *runVersionCursor) nextMem(ctx context.Context) (versionRow, error) {
	state := cursor.mem
	if state.direction == storage.ScanReverse {
		return cursor.nextMemReverse(ctx)
	}
	for {
		if err := ctx.Err(); err != nil {
			return versionRow{}, err
		}
		if state.lease == nil || state.row >= state.lease.block.Rows() {
			if state.lease != nil {
				if err := state.lease.release(); err != nil {
					state.lease = nil
					return versionRow{}, err
				}
				state.lease = nil
			}
			for state.nextBlock < len(state.blocks) && cursor.snapshotTS > 0 && state.blocks[state.nextBlock].minCommitTS > cursor.snapshotTS {
				state.nextBlock++
			}
			if state.nextBlock >= len(state.blocks) {
				return versionRow{}, io.EOF
			}
			lease, err := cursor.openMemLease(state.blocks[state.nextBlock])
			if err != nil {
				return versionRow{}, err
			}
			state.lease = lease
			state.row = 0
			state.nextBlock++
		}
		key, end, ts, ord, kind, err := state.lease.block.Version(state.row)
		if err != nil {
			return versionRow{}, err
		}
		row := versionRow{lease: state.lease, row: state.row, key: key, end: end, timestamp: ts, ordinal: ord, kind: kind}
		state.row++
		return row, nil
	}
}

func (cursor *runVersionCursor) nextMemReverse(ctx context.Context) (versionRow, error) {
	state := cursor.mem
	if !state.revHasPt {
		pt, err := cursor.nextMemReversePoint(ctx)
		if err != nil && err != io.EOF {
			return versionRow{}, err
		}
		state.revPoint, state.revHasPt = pt, err == nil
	}
	if state.revRangeAt < len(state.revRanges) {
		r := state.revRanges[state.revRangeAt]
		if !state.revHasPt || bytes.Compare(r.end, state.revPoint.key) > 0 {
			state.revRangeAt++
			return versionRow{key: r.start, end: r.end, timestamp: r.timestamp, ordinal: r.ordinal, kind: storage.MutationDeleteRange}, nil
		}
	}
	if !state.revHasPt {
		return versionRow{}, io.EOF
	}
	pt := state.revPoint
	state.revPoint = versionRow{}
	state.revHasPt = false
	return pt, nil
}

func (cursor *runVersionCursor) nextMemReversePoint(ctx context.Context) (versionRow, error) {
	state := cursor.mem
	for {
		if err := ctx.Err(); err != nil {
			return versionRow{}, err
		}
		// Emit remaining versions of the current reverse key group (newest first).
		if state.revGroupAt < len(state.revGroup) {
			rowIdx := state.revGroup[state.revGroupAt]
			state.revGroupAt++
			key, end, ts, ord, kind, err := state.lease.block.Version(rowIdx)
			if err != nil {
				return versionRow{}, err
			}
			if kind == storage.MutationDeleteRange {
				continue
			}
			return versionRow{lease: state.lease, row: rowIdx, key: key, end: end, timestamp: ts, ordinal: ord, kind: kind}, nil
		}
		// Advance to previous block if needed.
		if state.lease == nil || state.revRow < 0 {
			if state.lease != nil {
				if err := state.lease.release(); err != nil {
					state.lease = nil
					return versionRow{}, err
				}
				state.lease = nil
			}
			for state.revBlock >= 0 && cursor.snapshotTS > 0 && state.blocks[state.revBlock].minCommitTS > cursor.snapshotTS {
				state.revBlock--
			}
			if state.revBlock < 0 {
				return versionRow{}, io.EOF
			}
			lease, err := cursor.openMemLease(state.blocks[state.revBlock])
			if err != nil {
				return versionRow{}, err
			}
			state.lease = lease
			state.revRow = lease.block.Rows() - 1
			state.revBlock--
		}
		// Gather all contiguous rows in this block sharing state.revRow's key,
		// emitting them in ascending row index order (newest version first!).
		targetKey, _, _, _, _, err := state.lease.block.Version(state.revRow)
		if err != nil {
			return versionRow{}, err
		}
		endRow := state.revRow
		startRow := endRow
		for startRow > 0 {
			prevKey, _, _, _, _, err := state.lease.block.Version(startRow - 1)
			if err != nil {
				return versionRow{}, err
			}
			if !bytes.Equal(prevKey, targetKey) {
				break
			}
			startRow--
		}
		state.revGroup = state.revGroup[:0]
		for r := startRow; r <= endRow; r++ {
			state.revGroup = append(state.revGroup, r)
		}
		state.revGroupAt = 0
		state.revRow = startRow - 1
	}
}

func (state *memCursorState) close() error {
	var err error
	if state.lease != nil {
		err = errors.Join(err, state.lease.release())
		state.lease = nil
	}
	state.blocks = nil
	state.revRanges = nil
	state.revGroup = nil
	return err
}
