package neosepia

import (
	"bytes"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type rangeOrdering uint8

const (
	rangeEndOrder rangeOrdering = iota
	rangeVersionOrder
)

type rangeHeap struct {
	order rangeOrdering
	slots []uint32
}
type activeRange struct {
	end       []byte
	timestamp model.Timestamp
	ordinal   uint64
	positions [2]int
}

// Only intervals crossing the scan position remain live. Two indexed heaps
// expire by boundary and select by version without stale heap entries.
// Reverse traversal stores inclusive start keys in the expiration heap.
type activeRanges struct {
	entries     []activeRange
	free        []uint32
	heaps       [2]rangeHeap
	used, limit uint64
	reverse     bool
}

func newActiveRanges(maxBytes uint64) activeRanges {
	return activeRanges{limit: maxBytes, heaps: [2]rangeHeap{{order: rangeEndOrder}, {order: rangeVersionOrder}}}
}

func (ranges *activeRanges) add(end []byte, timestamp model.Timestamp, ordinal uint64) error {
	if uint64(len(end)) > ranges.limit-ranges.used {
		return columnar.ErrBatchBudget
	}
	if len(ranges.free) == 0 {
		if uint64(len(ranges.entries)) >= min(uint64(math.MaxUint32), uint64(math.MaxInt)) {
			return columnar.ErrBatchBudget
		}
		if err := reserveRanges(&ranges.entries, len(ranges.entries)+1, &ranges.used, ranges.limit); err != nil {
			return err
		}
	}
	active := len(ranges.heaps[0].slots) + 1
	for i := range ranges.heaps {
		if err := reserveRanges(&ranges.heaps[i].slots, active, &ranges.used, ranges.limit); err != nil {
			return err
		}
	}
	slots := len(ranges.entries)
	if len(ranges.free) == 0 {
		slots++
	}
	if err := reserveRanges(&ranges.free, slots, &ranges.used, ranges.limit); err != nil {
		return err
	}
	if uint64(len(end)) > ranges.limit-ranges.used {
		return columnar.ErrBatchBudget
	}
	var slot uint32
	if len(ranges.free) != 0 {
		slot = ranges.free[len(ranges.free)-1]
		ranges.free = ranges.free[:len(ranges.free)-1]
	} else {
		slot = uint32(len(ranges.entries))
		ranges.entries = ranges.entries[:len(ranges.entries)+1]
	}
	ownedEnd := make([]byte, len(end))
	copy(ownedEnd, end)
	ranges.entries[slot] = activeRange{end: ownedEnd, timestamp: timestamp, ordinal: ordinal}
	ranges.used += uint64(len(end))
	for i := range ranges.heaps {
		heap := &ranges.heaps[i]
		ranges.entries[slot].positions[i] = len(heap.slots)
		heap.slots = append(heap.slots, slot)
		ranges.up(heap, len(heap.slots)-1)
	}
	return nil
}

// Keys must follow scan order. Forward ends exclude equality; reverse starts include it.
func (ranges *activeRanges) advance(key []byte) {
	for len(ranges.heaps[0].slots) != 0 {
		slot := ranges.heaps[0].slots[0]
		entry := &ranges.entries[slot]
		order := bytes.Compare(entry.end, key)
		live := order > 0
		if ranges.reverse {
			live = order <= 0
		}
		if live {
			return
		}
		ranges.remove(&ranges.heaps[0], entry.positions[0])
		ranges.remove(&ranges.heaps[1], entry.positions[1])
		ranges.used -= uint64(len(entry.end))
		*entry = activeRange{}
		ranges.free = append(ranges.free, slot)
	}
}

func (ranges *activeRanges) suppresses(timestamp model.Timestamp, ordinal uint64) bool {
	if len(ranges.heaps[1].slots) == 0 {
		return false
	}
	entry := &ranges.entries[ranges.heaps[1].slots[0]]
	return entry.timestamp > timestamp || (entry.timestamp == timestamp && entry.ordinal >= ordinal)
}

func (ranges *activeRanges) Close() { *ranges = activeRanges{} }

func (ranges *activeRanges) less(heap *rangeHeap, left, right int) bool {
	a, b := &ranges.entries[heap.slots[left]], &ranges.entries[heap.slots[right]]
	if heap.order == rangeEndOrder {
		if ranges.reverse {
			return bytes.Compare(a.end, b.end) > 0
		}
		return bytes.Compare(a.end, b.end) < 0
	}
	return a.timestamp > b.timestamp || (a.timestamp == b.timestamp && a.ordinal > b.ordinal)
}

func (ranges *activeRanges) swap(heap *rangeHeap, left, right int) {
	heap.slots[left], heap.slots[right] = heap.slots[right], heap.slots[left]
	ranges.entries[heap.slots[left]].positions[heap.order] = left
	ranges.entries[heap.slots[right]].positions[heap.order] = right
}

func (ranges *activeRanges) up(heap *rangeHeap, index int) {
	for index > 0 {
		parent := (index - 1) / 2
		if !ranges.less(heap, index, parent) {
			return
		}
		ranges.swap(heap, index, parent)
		index = parent
	}
}

func (ranges *activeRanges) down(heap *rangeHeap, index int) {
	for index < len(heap.slots)/2 {
		child := index*2 + 1
		if child+1 < len(heap.slots) && ranges.less(heap, child+1, child) {
			child++
		}
		if !ranges.less(heap, child, index) {
			return
		}
		ranges.swap(heap, index, child)
		index = child
	}
}

func (ranges *activeRanges) remove(heap *rangeHeap, index int) {
	last := len(heap.slots) - 1
	if index != last {
		ranges.swap(heap, index, last)
	}
	heap.slots = heap.slots[:last]
	if index == last {
		return
	}
	if index > 0 && ranges.less(heap, index, (index-1)/2) {
		ranges.up(heap, index)
		return
	}
	ranges.down(heap, index)
}

// Charge both arrays during growth; the old backing array remains live while
// copied. Failed reservation leaves the already-published heap contents intact.
func reserveRanges[T interface{ activeRange | uint32 }](values *[]T, length int, used *uint64, limit uint64) error {
	if length <= cap(*values) {
		return nil
	}
	capacity := max(length, min(math.MaxInt/2, cap(*values))*2, 4)
	size := uint64(unsafe.Sizeof(*new(T)))
	if uint64(capacity) > (limit-*used)/size {
		return columnar.ErrBatchBudget
	}
	next := make([]T, len(*values), capacity)
	copy(next, *values)
	*used += uint64(capacity-cap(*values)) * size
	*values = next
	return nil
}
