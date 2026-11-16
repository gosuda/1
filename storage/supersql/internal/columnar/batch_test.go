package columnar_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func batchColumn(length int) columnar.Vector {
	payload := make([]int64, length)
	for row := range payload {
		payload[row] = int64(row)
	}
	return columnar.Vector{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: length, Int64: payload}
}

// TestBatchSelectionRowCapIsLogical pins the row-cap contract: a
// Selection-filtered producer publishes full-extent columns, so the physical
// Length may exceed the row request while Rows (len(Selection)) is the logical
// output and lane values index the physical vectors. The PAX scan cursor relies
// on this when a predicate or LIMIT shortens the selection of a leased block.
func TestBatchSelectionRowCapIsLogical(t *testing.T) {
	b := columnar.Batch{Columns: []columnar.Vector{batchColumn(3)}, Length: 3, Selection: []uint32{0, 1}, MaxRows: 2}
	if err := b.Validate(2); err != nil {
		t.Fatalf("filtered batch with full-extent columns: %v", err)
	}
	if b.Rows() != 2 || b.Lane(0) != 0 || b.Lane(1) != 1 {
		t.Fatalf("logical rows = %d over lanes %d, %d; want 2 over 0, 1", b.Rows(), b.Lane(0), b.Lane(1))
	}
}

func TestBatchValidateRejectsLogicalRowsAboveRequest(t *testing.T) {
	b := columnar.Batch{Columns: []columnar.Vector{batchColumn(3)}, Length: 3, Selection: []uint32{0, 1, 2}}
	if err := b.Validate(2); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("three selected lanes for a two-row request: %v", err)
	}
	if err := b.Validate(3); err != nil {
		t.Fatalf("three selected lanes at the three-row request bound: %v", err)
	}
}

func TestBatchValidateCapsPhysicalExtentWithoutSelection(t *testing.T) {
	b := columnar.Batch{Columns: []columnar.Vector{batchColumn(3)}, Length: 3}
	if err := b.Validate(2); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("unfiltered batch longer than the request: %v", err)
	}
	if err := b.Validate(3); err != nil {
		t.Fatalf("unfiltered batch at the request bound: %v", err)
	}
}

func TestBatchValidateRejectsSelectionOutsideExtent(t *testing.T) {
	past := columnar.Batch{Columns: []columnar.Vector{batchColumn(3)}, Length: 3, Selection: []uint32{0, 3}}
	if err := past.Validate(2); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("lane past the physical extent: %v", err)
	}
	short := columnar.Batch{Columns: []columnar.Vector{batchColumn(2)}, Length: 3, Selection: []uint32{0, 1}}
	if err := short.Validate(2); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("column shorter than the physical extent: %v", err)
	}
}

func TestBatchValidateEnforcesOwnRowRequest(t *testing.T) {
	b := columnar.Batch{Columns: []columnar.Vector{batchColumn(3)}, Length: 3, Selection: []uint32{0, 1}, MaxRows: 1}
	if err := b.Validate(2); !errors.Is(err, columnar.ErrInvalidBatch) {
		t.Fatalf("batch exceeding its own MaxRows request: %v", err)
	}
}
