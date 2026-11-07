package neosepia

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const sstRangeEntrySize = 32

type sstRangeEntry struct {
	start, end columnar.Range
	timestamp  model.Timestamp
	ordinal    uint64
}

func (w *sstWriter) measureRanges(ctx context.Context, block *paxBlock, maximum []byte) ([]byte, int, uint64, error) {
	coverage := maximum
	count := 0
	var keyBytes uint64
	for row := range block.Rows() {
		if row&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
		}
		start, end, _, _, kind, err := block.Version(row)
		if err != nil {
			return nil, 0, 0, err
		}
		if kind != storage.MutationDeleteRange {
			continue
		}
		if bytes.Compare(end, coverage) > 0 {
			coverage = end
		}
		count++
		for _, key := range [2][]byte{start, end} {
			if uint64(len(key)) > w.limits.MaxIndexBytes-keyBytes {
				return nil, 0, 0, columnar.ErrBatchBudget
			}
			keyBytes += uint64(len(key))
		}
	}
	return coverage, count, keyBytes, nil
}

func (w *sstWriter) reserveRangeEntries(extra int) error {
	needed := uint64(len(w.ranges)) + uint64(extra)
	if needed <= uint64(cap(w.ranges)) {
		return nil
	}
	if needed > min(uint64(math.MaxUint32), uint64(math.MaxInt)) {
		return columnar.ErrBatchBudget
	}
	capacity := max(4, int(needed), min(math.MaxInt/2, cap(w.ranges))*2)
	if err := w.budget.chargeMul(uint64(capacity), uint64(unsafe.Sizeof(sstRangeEntry{}))); err != nil {
		return err
	}
	next := make([]sstRangeEntry, len(w.ranges), capacity)
	copy(next, w.ranges)
	w.ranges = next
	return nil
}

func (w *sstWriter) appendRanges(ctx context.Context, block *paxBlock) error {
	for row := range block.Rows() {
		if row&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		start, end, timestamp, ordinal, kind, err := block.Version(row)
		if err != nil {
			return err
		}
		if kind != storage.MutationDeleteRange {
			continue
		}
		w.ranges = append(w.ranges, sstRangeEntry{start: w.appendKey(start), end: w.appendKey(end), timestamp: timestamp, ordinal: ordinal})
	}
	return nil
}

func (w *sstWriter) sortRanges(ctx context.Context) error {
	for i := len(w.ranges) / 2; i > 0; i-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.downRange(i-1, len(w.ranges))
	}
	for end := len(w.ranges) - 1; end > 0; end-- {
		if err := ctx.Err(); err != nil {
			return err
		}
		w.ranges[0], w.ranges[end] = w.ranges[end], w.ranges[0]
		w.downRange(0, end)
	}
	return ctx.Err()
}

func (w *sstWriter) rangeBefore(left, right int) bool {
	a, b := &w.ranges[left], &w.ranges[right]
	if order := bytes.Compare(w.key(a.end), w.key(b.end)); order != 0 {
		return order > 0
	}
	if order := storage.CompareVersion(a.timestamp, a.ordinal, b.timestamp, b.ordinal); order != 0 {
		return order > 0
	}
	return bytes.Compare(w.key(a.start), w.key(b.start)) < 0
}

func (w *sstWriter) downRange(index, length int) {
	for index < length/2 {
		child := index*2 + 1
		if child+1 < length && w.rangeBefore(child, child+1) {
			child++
		}
		if !w.rangeBefore(index, child) {
			return
		}
		w.ranges[index], w.ranges[child] = w.ranges[child], w.ranges[index]
		index = child
	}
}

func encodeSSTRangeEntry(data []byte, entry sstRangeEntry) {
	binary.LittleEndian.PutUint32(data[0:4], entry.start.Offset)
	binary.LittleEndian.PutUint32(data[4:8], entry.start.Length)
	binary.LittleEndian.PutUint32(data[8:12], entry.end.Offset)
	binary.LittleEndian.PutUint32(data[12:16], entry.end.Length)
	binary.LittleEndian.PutUint64(data[16:24], uint64(entry.timestamp))
	binary.LittleEndian.PutUint64(data[24:32], entry.ordinal)
}

func (s *sstFile) rangeEntry(index int) sstRangeEntry {
	offset := sstIndexHeaderSize + s.count*s.entryBytes + index*sstRangeEntrySize
	data := s.index[offset : offset+sstRangeEntrySize]
	return sstRangeEntry{start: columnar.Range{Offset: binary.LittleEndian.Uint32(data[0:4]), Length: binary.LittleEndian.Uint32(data[4:8])}, end: columnar.Range{Offset: binary.LittleEndian.Uint32(data[8:12]), Length: binary.LittleEndian.Uint32(data[12:16])}, timestamp: model.Timestamp(binary.LittleEndian.Uint64(data[16:24])), ordinal: binary.LittleEndian.Uint64(data[24:32])}
}

func (s *sstFile) validateRanges(ctx context.Context, rows uint64) error {
	if uint64(s.rangeCount) > rows {
		return storage.ErrCorruptStorage
	}
	var previous sstRangeEntry
	for i := range s.rangeCount {
		if err := ctx.Err(); err != nil {
			return err
		}
		entry := s.rangeEntry(i)
		for _, key := range [2]columnar.Range{entry.start, entry.end} {
			if key.Length == 0 || uint64(key.Offset)+uint64(key.Length) > uint64(len(s.keys)) {
				return storage.ErrCorruptStorage
			}
		}
		if entry.timestamp <= 0 || entry.ordinal == 0 || bytes.Compare(s.key(entry.start), s.key(entry.end)) >= 0 {
			return storage.ErrCorruptStorage
		}
		if i > 0 {
			order := bytes.Compare(s.key(previous.end), s.key(entry.end))
			versionOrder := storage.CompareVersion(previous.timestamp, previous.ordinal, entry.timestamp, entry.ordinal)
			reversedTie := versionOrder == 0 && bytes.Compare(s.key(previous.start), s.key(entry.start)) > 0
			reversedVersion := versionOrder < 0 || reversedTie
			reversedEndTie := order == 0 && reversedVersion
			if order < 0 || reversedEndTie {
				return storage.ErrCorruptStorage
			}
		}
		previous = entry
	}
	return nil
}
