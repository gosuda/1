package columnar

import "errors"

const DefaultBatchSize = 1024

var ErrInvalidBatch = errors.New("invalid columnar batch")

// Batch owns its column headers but borrows vector payloads and selection lanes
// until the producing cursor's next Next or Close. Retaining operators must copy
// into their budgeted state or obtain an immutable-buffer pin from the producer.
// A non-nil empty Selection represents an empty filtered batch, not EOF.
type Batch struct {
	Columns []Vector
	// Length is the physical extent of every vector in Columns. With a non-nil
	// Selection only the selected lanes are logical rows, so Rows may be shorter
	// than Length and lane values index the physical vectors directly.
	Length    int
	Selection []uint32
	// MaxRows caps logical output from a producer. Zero accepts its own bound;
	// negative values are invalid. Producers and Reset preserve this request.
	MaxRows int
}

func (b *Batch) Rows() int {
	if b.Selection != nil {
		return len(b.Selection)
	}
	return b.Length
}

func (b *Batch) Lane(row int) int {
	if row < 0 || row >= b.Rows() {
		panic("columnar row out of bounds")
	}
	if b.Selection != nil {
		return int(b.Selection[row])
	}
	return row
}

// Reset releases borrowed references while retaining only the owned header
// capacity. It does not close the cursor or mutate borrowed payload buffers.
func (b *Batch) Reset() {
	clear(b.Columns[:cap(b.Columns)])
	b.Columns = b.Columns[:0]
	b.Length = 0
	b.Selection = nil
}

// Validate reports whether the batch is internally consistent and within the
// producing request. maxRows caps the logical row count (Rows), not the physical
// vector extent: a Selection-filtered producer legitimately publishes full-extent
// columns with a short selection, so Length is bounded only by the lane bounds
// below and by the extent of every column. Without a Selection, Rows == Length,
// so the physical extent stays capped by maxRows.
func (b *Batch) Validate(maxRows int) error {
	if maxRows <= 0 || b.Length < 0 || b.MaxRows < 0 {
		return ErrInvalidBatch
	}
	if b.Rows() > maxRows {
		return ErrInvalidBatch
	}
	if b.MaxRows > 0 && b.Rows() > b.MaxRows {
		return ErrInvalidBatch
	}
	for _, lane := range b.Selection {
		if uint64(lane) >= uint64(b.Length) {
			return ErrInvalidBatch
		}
	}
	for i := range b.Columns {
		if b.Columns[i].Length < b.Length {
			return ErrInvalidBatch
		}
		if err := b.Columns[i].Validate(); err != nil {
			return err
		}
	}
	return nil
}
