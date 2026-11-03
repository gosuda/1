package storage_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSnapshotRequestCannotManufactureLatest(t *testing.T) {
	fence := storage.ReadFence{ID: [16]byte{1}, ClosedTS: 80, LogPosition: 17}
	for _, timestamp := range []model.Timestamp{0, -1, 81} {
		spec := storage.SnapshotSpec{Timestamp: timestamp, Fence: fence}
		if err := spec.Validate(); !errors.Is(err, storage.ErrSnapshotNotClosed) {
			t.Fatalf("timestamp %d: %v", timestamp, err)
		}
	}
	if err := (storage.SnapshotSpec{Timestamp: 42, Fence: fence}).Validate(); err != nil {
		t.Fatalf("exact retained timestamp: %v", err)
	}
	fence.ID = [16]byte{}
	if err := (storage.SnapshotSpec{Timestamp: 42, Fence: fence}).Validate(); !errors.Is(err, storage.ErrSnapshotNotClosed) {
		t.Fatalf("anonymous fence: %v", err)
	}
}

func TestStorageProjectionPreservesDuplicateColumnRequests(t *testing.T) {
	integer := model.Type{ID: 20, Storage: model.TypeInt64}
	r := storage.ReadRequest{
		Target:    encodedFixture().Target,
		Layout:    []storage.ColumnLayout{{ID: 3, Type: integer}, {ID: 70000, Type: integer}},
		ColumnIDs: []model.ColumnID{70000, 3, 70000},
	}
	if err := r.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.ColumnIDs = []model.ColumnID{4}
	if err := r.Validate(t.Context()); !errors.Is(err, storage.ErrInvalidProjection) {
		t.Fatalf("unbound column: %v", err)
	}
}

func TestScanBoundsRejectAmbiguousUnboundedEndpoints(t *testing.T) {
	integer := model.Type{ID: 20, Storage: model.TypeInt64}
	r := storage.PushdownRequest{
		Target:    encodedFixture().Target,
		Layout:    []storage.ColumnLayout{{ID: 1, Type: integer}},
		KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: integer}, {ColumnID: 1, Type: integer}},
		Spans:     []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}},
	}
	if err := r.Validate(t.Context()); err != nil {
		t.Fatalf("full scan over repeated index key column: %v", err)
	}
	r.Spans[0].Lower.Values = []model.Value{model.Int64Val(0)}
	if err := r.Validate(t.Context()); !errors.Is(err, storage.ErrRangeInvalid) {
		t.Fatalf("finite and unbounded endpoint: %v", err)
	}
	r.Spans[0].Lower = storage.KeyBound{Inclusive: true, Values: []model.Value{model.StringVal("0")}}
	if err := r.Validate(t.Context()); !errors.Is(err, storage.ErrRangeInvalid) {
		t.Fatalf("wrong endpoint type: %v", err)
	}
}
