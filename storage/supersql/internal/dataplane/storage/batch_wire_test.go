package storage_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestEncodedBatchStreamsBeyondItsAllocationBudget(t *testing.T) {
	b := emptyArrayBatch()
	prototype := b.Mutations[0]
	b.Mutations = make([]storage.Mutation, 1000)
	for i := range b.Mutations {
		b.Mutations[i] = prototype
		b.Mutations[i].Ordinal = uint64(i + 1)
	}
	sealLayoutBatch(t, &b)
	var wire bytes.Buffer
	if err := storage.EncodeMutationBatch(t.Context(), &wire, b); err != nil {
		t.Fatal(err)
	}
	decoded, err := storage.OpenEncodedMutationBatch(t.Context(), bytes.NewReader(wire.Bytes()), uint64(wire.Len()), storage.BatchDecodeLimits{MaxBytes: 1 << 20, MaxAllocation: 4096, MaxMutationBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	first, err := decoded.OpenMutations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	})
	second, err := decoded.OpenMutations(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	for i := range 1000 {
		a, err := first.Next(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if a.Ordinal != uint64(i+1) || a.Values[0].IsNull || len(a.Values[0].ArrayVal) != 0 {
			t.Fatal("streamed row image changed")
		}
	}
	a, err := second.Next(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if a.Ordinal != 1 {
		t.Fatal("one cursor advanced another")
	}
	if _, err := first.Next(t.Context()); !errors.Is(err, io.EOF) {
		t.Fatalf("end of batch: %v", err)
	}
	var again bytes.Buffer
	if err := storage.EncodeMutationBatch(t.Context(), &again, decoded); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire.Bytes(), again.Bytes()) {
		t.Fatal("batch framing changed after streaming replay")
	}
}

func TestEncodedBatchRejectsTruncationAndTrailingBytes(t *testing.T) {
	b := emptyArrayBatch()
	sealLayoutBatch(t, &b)
	var wire bytes.Buffer
	if err := storage.EncodeMutationBatch(t.Context(), &wire, b); err != nil {
		t.Fatal(err)
	}
	limits := storage.BatchDecodeLimits{MaxBytes: 65536, MaxAllocation: 65536, MaxMutationBytes: 4096}
	for end := range wire.Len() {
		if _, err := storage.OpenEncodedMutationBatch(t.Context(), bytes.NewReader(wire.Bytes()[:end]), uint64(end), limits); err == nil {
			t.Fatalf("accepted truncation at %d", end)
		}
	}
	extra := append(bytes.Clone(wire.Bytes()), 0)
	if _, err := storage.OpenEncodedMutationBatch(t.Context(), bytes.NewReader(extra), uint64(len(extra)), limits); err == nil {
		t.Fatal("accepted trailing bytes")
	}
	limits.MaxAllocation = 1
	if _, err := storage.OpenEncodedMutationBatch(t.Context(), bytes.NewReader(wire.Bytes()), uint64(wire.Len()), limits); !errors.Is(err, storage.ErrDecodeBudget) {
		t.Fatalf("metadata budget: %v", err)
	}
}

func FuzzEncodedBatchFraming(f *testing.F) {
	b := emptyArrayBatch()
	var err error
	b.ChecksumCRC, err = b.ComputeChecksum(f.Context())
	if err != nil {
		f.Fatal(err)
	}
	var wire bytes.Buffer
	if err := storage.EncodeMutationBatch(f.Context(), &wire, b); err != nil {
		f.Fatal(err)
	}
	f.Add(wire.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			return
		}
		decoded, err := storage.OpenEncodedMutationBatch(t.Context(), bytes.NewReader(data), uint64(len(data)), storage.BatchDecodeLimits{MaxBytes: 65536, MaxAllocation: 131072, MaxMutationBytes: 65536})
		if err != nil {
			return
		}
		var canonical bytes.Buffer
		if err := storage.EncodeMutationBatch(t.Context(), &canonical, decoded); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, canonical.Bytes()) {
			t.Fatal("accepted noncanonical batch framing")
		}
	})
}
