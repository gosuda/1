package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (mb *memPAXBlock) keyAt(row int) []byte {
	rec := mb.vis[row*paxVersionSize:]
	keyOff := uint64(binary.LittleEndian.Uint32(rec[24:]))
	keyLen := uint64(binary.LittleEndian.Uint32(rec[28:]))
	return mb.vis[keyOff : keyOff+keyLen]
}

func reverseFirstMemBlock(blocks []*memPAXBlock, key []byte, last int) int {
	low, high := 0, last+1
	for low < high {
		mid := low + (high-low)/2
		mb := blocks[mid]
		if mb.rows == 0 || bytes.Compare(mb.keyAt(mb.rows-1), key) < 0 {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low
}

func reverseFirstMemRow(mb *memPAXBlock, key []byte) int {
	low, high := 0, mb.rows
	for low < high {
		mid := low + (high-low)/2
		if bytes.Compare(mb.keyAt(mid), key) < 0 {
			low = mid + 1
		} else {
			high = mid
		}
	}
	return low
}

type memRangeItem struct {
	start, end []byte
	timestamp  model.Timestamp
	ordinal    uint64
}

type memCursorState struct {
	blocks     []*memPAXBlock
	direction  storage.ScanDirection
	nextBlock  int
	row        int
	lease      *blockLease
	leaseBlock int
	// Reverse scan state
	revRanges         []memRangeItem
	revRangeAt        int
	revKey            []byte
	revGroupBlock     int
	revGroupRow       int
	revGroupLastBlock int
	revGroupLastRow   int
	revGroupActive    bool
	revBlock          int
	revRow            int
	revPoint          versionRow
	revHasPt          bool
}

func newMemVersionCursor(blocks []*memPAXBlock, direction storage.ScanDirection, maxBytes uint64, memory *sourceMemory) *runVersionCursor {
	state := &memCursorState{
		blocks:     blocks,
		direction:  direction,
		leaseBlock: -1,
		revBlock:   len(blocks) - 1,
		revRow:     -1,
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
				kOff := uint64(binary.LittleEndian.Uint32(rec[24:]))
				kLen := uint64(binary.LittleEndian.Uint32(rec[28:]))
				eOff := uint64(binary.LittleEndian.Uint32(rec[32:]))
				eLen := uint64(binary.LittleEndian.Uint32(rec[36:]))
				ts := model.Timestamp(int64(binary.LittleEndian.Uint64(rec[0:])))
				ord := binary.LittleEndian.Uint64(rec[8:])
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

func (cursor *runVersionCursor) ensureMemLease(blockIdx int) error {
	state := cursor.mem
	if state.lease != nil && state.leaseBlock == blockIdx {
		return nil
	}
	if state.lease != nil {
		if err := state.lease.release(); err != nil {
			state.lease = nil
			state.leaseBlock = -1
			return err
		}
		state.lease = nil
		state.leaseBlock = -1
	}
	lease, err := cursor.openMemLease(state.blocks[blockIdx])
	if err != nil {
		return err
	}
	state.lease = lease
	state.leaseBlock = blockIdx
	return nil
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
					state.leaseBlock = -1
					return versionRow{}, err
				}
				state.lease = nil
				state.leaseBlock = -1
			}
			for state.nextBlock < len(state.blocks) && cursor.snapshotTS > 0 && state.blocks[state.nextBlock].minCommitTS > cursor.snapshotTS {
				state.nextBlock++
			}
			if state.nextBlock >= len(state.blocks) {
				return versionRow{}, io.EOF
			}
			if err := cursor.ensureMemLease(state.nextBlock); err != nil {
				return versionRow{}, err
			}
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
		// Stream remaining versions of the current cross-block key group in
		// forward block/row order (strictly descending commitTS, ordinal).
		if state.revGroupActive {
			for state.revGroupBlock <= state.revGroupLastBlock {
				mb := state.blocks[state.revGroupBlock]
				endRow := mb.rows - 1
				if state.revGroupBlock == state.revGroupLastBlock {
					endRow = state.revGroupLastRow
				}
				if mb.rows == 0 || (cursor.snapshotTS > 0 && mb.minCommitTS > cursor.snapshotTS) || state.revGroupRow > endRow {
					state.revGroupBlock++
					state.revGroupRow = 0
					continue
				}
				break
			}
			if state.revGroupBlock > state.revGroupLastBlock {
				state.revGroupActive = false
				continue
			}
			if err := cursor.ensureMemLease(state.revGroupBlock); err != nil {
				return versionRow{}, err
			}
			rowIdx := state.revGroupRow
			state.revGroupRow++
			key, end, ts, ord, kind, err := state.lease.block.Version(rowIdx)
			if err != nil {
				return versionRow{}, err
			}
			if !bytes.Equal(key, state.revKey) {
				return versionRow{}, storage.ErrCorruptStorage
			}
			if kind == storage.MutationDeleteRange {
				continue
			}
			return versionRow{lease: state.lease, row: rowIdx, key: key, end: end, timestamp: ts, ordinal: ord, kind: kind}, nil
		}

		// Skip empty or snapshot-pruned blocks to find the next key's trailing block.
		for state.revBlock >= 0 {
			mb := state.blocks[state.revBlock]
			if mb.rows == 0 || (cursor.snapshotTS > 0 && mb.minCommitTS > cursor.snapshotTS) {
				state.revBlock--
				state.revRow = -1
				continue
			}
			break
		}
		if state.revBlock < 0 {
			if state.lease != nil {
				if err := state.lease.release(); err != nil {
					state.lease = nil
					state.leaseBlock = -1
					return versionRow{}, err
				}
				state.lease = nil
				state.leaseBlock = -1
			}
			return versionRow{}, io.EOF
		}

		lastBlock := state.revBlock
		endRow := state.revRow
		if endRow < 0 {
			endRow = state.blocks[lastBlock].rows - 1
		}
		targetKey := state.blocks[lastBlock].keyAt(endRow)
		state.revKey = append(state.revKey[:0], targetKey...)

		firstBlock := reverseFirstMemBlock(state.blocks, state.revKey, lastBlock)
		if firstBlock > lastBlock {
			return versionRow{}, storage.ErrCorruptStorage
		}
		firstRow := reverseFirstMemRow(state.blocks[firstBlock], state.revKey)
		if firstRow >= state.blocks[firstBlock].rows || !bytes.Equal(state.blocks[firstBlock].keyAt(firstRow), state.revKey) {
			return versionRow{}, storage.ErrCorruptStorage
		}

		// Position predecessor strictly before [firstBlock:firstRow].
		if firstRow > 0 {
			state.revBlock = firstBlock
			state.revRow = firstRow - 1
		} else {
			state.revBlock = firstBlock - 1
			state.revRow = -1
		}

		state.revGroupBlock = firstBlock
		state.revGroupRow = firstRow
		state.revGroupLastBlock = lastBlock
		state.revGroupLastRow = endRow
		state.revGroupActive = true
	}
}

func (state *memCursorState) close() error {
	var err error
	if state.lease != nil {
		err = errors.Join(err, state.lease.release())
		state.lease = nil
	}
	state.leaseBlock = -1
	state.blocks = nil
	state.revRanges = nil
	state.revRangeAt = 0
	state.revKey = nil
	state.revGroupBlock = 0
	state.revGroupRow = 0
	state.revGroupLastBlock = 0
	state.revGroupLastRow = 0
	state.revGroupActive = false
	state.revBlock = -1
	state.revRow = -1
	state.revPoint = versionRow{}
	state.revHasPt = false
	return err
}
