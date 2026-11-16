package neosepia

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// These decoder tests construct PAX v2 blocks by calling the sibling
// encodeBlock entry point once per test, then exercise only decoder behavior:
// lazy access, error taxonomy, budgets, ownership and Close. They assert wire
// compatibility without duplicating encoder coverage.

func decTestLimits() blockLimits {
	return blockLimits{MaxRows: 1024, MaxBytes: 1 << 26, MaxDecodedBytes: 1 << 26}
}

func decTestTarget() storage.ObjectRef {
	return storage.ObjectRef{ObjectID: 7, Generation: 2, DescriptorVersion: 3, EncodingVersion: 1}
}

func decTestIntBatch(t *testing.T, values []int64, nulls []bool) *columnar.Batch {
	t.Helper()
	n := len(values)
	vec := columnar.Vector{
		Type:     model.Type{ID: 23, Storage: model.TypeInt64},
		Encoding: columnar.Flat,
		Length:   n,
		Int64:    append([]int64(nil), values...),
	}
	if len(nulls) != 0 {
		vec.Nulls = make([]byte, (n+7)/8)
		for i, null := range nulls {
			if null {
				vec.Nulls[i/8] |= 1 << uint(i%8)
			}
		}
	}
	if err := vec.Validate(); err != nil {
		t.Fatal(err)
	}
	return &columnar.Batch{Columns: []columnar.Vector{vec}, Length: n}
}

func decTestKeyVectors(t *testing.T, keys [][]byte) (columnar.Vector, columnar.Vector) {
	t.Helper()
	dataLen := 0
	for _, k := range keys {
		dataLen += len(k)
	}
	kv := columnar.Vector{Type: model.Type{ID: 17, Storage: model.TypeBytes}, Encoding: columnar.Flat, Length: len(keys)}
	kv.Data = make([]byte, 0, dataLen)
	kv.Ranges = make([]columnar.Range, len(keys))
	for i, k := range keys {
		start := len(kv.Data)
		kv.Data = append(kv.Data, k...)
		kv.Ranges[i] = columnar.Range{Offset: uint32(start), Length: uint32(len(k))}
	}
	if err := kv.Validate(); err != nil {
		t.Fatal(err)
	}
	return kv, columnar.Vector{}
}

func decTestVersions(t *testing.T, keys [][]byte, kinds []storage.MutationOp) blockVersions {
	t.Helper()
	kv, ev := decTestKeyVectors(t, keys)
	ts := make([]model.Timestamp, len(keys))
	ord := make([]uint64, len(keys))
	for i := range keys {
		ts[i] = model.Timestamp(100 + i)
		ord[i] = uint64(i + 1)
	}
	return blockVersions{Keys: kv, EndKeys: ev, CommitTS: ts, Ordinals: ord, Kinds: kinds}
}

func TestDecodeVisibilityWithoutPayloadDecode(t *testing.T) {
	batch := decTestIntBatch(t, []int64{3, 1, 2}, nil)
	keys := [][]byte{{'a'}, {'b'}, {'c'}}
	kinds := []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert, storage.MutationDelete}
	raw, err := encodeBlock(t.Context(), decTestTarget(), decTestVersions(t, keys, kinds), batch, []model.ColumnID{1}, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	blk, err := openBlock(raw, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blk.Close(); err != nil {
			t.Error(err)
		}
	})
	if blk.Rows() != 3 {
		t.Fatalf("rows = %d, want 3", blk.Rows())
	}
	if got := blk.Target(); got != decTestTarget() {
		t.Fatalf("target = %+v, want %+v", got, decTestTarget())
	}
	for i, want := range keys {
		key, end, ts, ord, kind, err := blk.Version(i)
		if err != nil {
			t.Fatal(err)
		}
		if string(key) != string(want) || len(end) != 0 {
			t.Fatalf("row %d key = %q/%q, want %q/empty", i, key, end, want)
		}
		if ts != model.Timestamp(100+i) || ord != uint64(i+1) || kind != kinds[i] {
			t.Fatalf("row %d version = %d/%d/%v", i, ts, ord, kind)
		}
	}
	zone, err := blk.Zone(1)
	if err != nil {
		t.Fatal(err)
	}
	if !zone.Valid || zone.Min != 1 || zone.Max != 3 {
		t.Fatalf("zone = %+v, want valid [1,3]", zone)
	}
	col, err := blk.Column(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := col.Validate(); err != nil {
		t.Fatal(err)
	}
	if col.Int64[0] != 3 || col.Int64[1] != 1 || col.Int64[2] != 2 {
		t.Fatalf("column payload = %v", col.Int64)
	}
}

func TestDecodeMissingColumnAndBounds(t *testing.T) {
	batch := decTestIntBatch(t, []int64{1}, nil)
	keys := [][]byte{{'k'}}
	kinds := []storage.MutationOp{storage.MutationUpsert}
	raw, err := encodeBlock(t.Context(), decTestTarget(), decTestVersions(t, keys, kinds), batch, []model.ColumnID{5}, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	blk, err := openBlock(raw, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blk.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := blk.Column(t.Context(), 6); !errors.Is(err, errColumnMissing) {
		t.Fatalf("unknown column = %v, want errColumnMissing", err)
	}
	if _, err := blk.Zone(6); !errors.Is(err, errColumnMissing) {
		t.Fatalf("unknown zone = %v, want errColumnMissing", err)
	}
	if _, _, _, _, _, err := blk.Version(1); !errors.Is(err, errPAXBlock) {
		t.Fatalf("out-of-range version = %v, want errPAXBlock", err)
	}
	if _, _, _, _, _, err := blk.Version(-1); !errors.Is(err, errPAXBlock) {
		t.Fatalf("negative version = %v, want errPAXBlock", err)
	}
}

func TestDecodeRejectsTruncatedChecksumAndMagic(t *testing.T) {
	batch := decTestIntBatch(t, []int64{9}, nil)
	raw, err := encodeBlock(t.Context(), decTestTarget(), decTestVersions(t, [][]byte{{'k'}}, []storage.MutationOp{storage.MutationUpsert}), batch, []model.ColumnID{1}, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]byte(nil), raw...)
	bad[len(bad)-1] ^= 0xff
	if _, err := openBlock(bad, decTestLimits()); !errors.Is(err, errPAXBlock) {
		t.Fatalf("checksum corruption = %v, want errPAXBlock", err)
	}
	bad = append([]byte(nil), raw...)
	binary.LittleEndian.PutUint32(bad[0:], 0xdeadbeef)
	// Repair the checksum so the magic check is the failure, not the CRC.
	binary.LittleEndian.PutUint32(bad[56:], paxChecksum(bad))
	if _, err := openBlock(bad, decTestLimits()); !errors.Is(err, errPAXBlock) {
		t.Fatalf("bad magic = %v, want errPAXBlock", err)
	}
	if _, err := openBlock(raw[:len(raw)-1], decTestLimits()); !errors.Is(err, errPAXBlock) {
		t.Fatalf("truncation = %v, want errPAXBlock", err)
	}
}

func TestDecodeBudgetAndCancellation(t *testing.T) {
	batch := decTestIntBatch(t, []int64{1, 2}, nil)
	raw, err := encodeBlock(t.Context(), decTestTarget(), decTestVersions(t, [][]byte{{'a'}, {'b'}}, []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert}), batch, []model.ColumnID{1}, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	tiny := blockLimits{MaxRows: 1024, MaxBytes: 1 << 26, MaxDecodedBytes: 1}
	if _, err := openBlock(raw, tiny); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("open budget = %v, want ErrBatchBudget", err)
	}
	blk, err := openBlock(raw, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blk.Close(); err != nil {
			t.Error(err)
		}
	})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := blk.Column(canceled, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled decode = %v, want context.Canceled", err)
	}
}

func TestDecodeCloseReleasesReferences(t *testing.T) {
	batch := decTestIntBatch(t, []int64{4}, nil)
	raw, err := encodeBlock(t.Context(), decTestTarget(), decTestVersions(t, [][]byte{{'k'}}, []storage.MutationOp{storage.MutationUpsert}), batch, []model.ColumnID{1}, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	blk, err := openBlock(raw, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	first, err := blk.Column(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := blk.Close(); err != nil {
		t.Fatal(err)
	}
	if err := blk.Close(); err != nil {
		t.Fatalf("second close = %v, want nil", err)
	}
	if _, err := blk.Column(t.Context(), 1); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("column after close = %v, want ErrClosed", err)
	}
	if _, _, _, _, _, err := blk.Version(0); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("version after close = %v, want ErrClosed", err)
	}
	if _, err := blk.Zone(1); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("zone after close = %v, want ErrClosed", err)
	}
	if first.Int64[0] != 4 {
		t.Fatalf("cached column changed across close: %v", first.Int64)
	}
}

func TestDecodeBooleanAndDictionary(t *testing.T) {
	boolVec := columnar.Vector{Type: model.Type{ID: 16, Storage: model.TypeBoolean}, Encoding: columnar.Flat, Length: 3, Int64: []int64{1, 0, 0}, Nulls: []byte{0b100}}
	strVec := columnar.Vector{Type: model.Type{ID: 25, Storage: model.TypeString}, Encoding: columnar.Flat, Length: 3, Data: []byte("aab"), Ranges: []columnar.Range{{Offset: 0, Length: 1}, {Offset: 0, Length: 1}, {Offset: 2, Length: 1}}}
	batch := &columnar.Batch{Columns: []columnar.Vector{boolVec, strVec}, Length: 3}
	for _, v := range batch.Columns {
		if err := v.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := encodeBlock(t.Context(), decTestTarget(), decTestVersions(t, [][]byte{{'a'}, {'b'}, {'c'}}, []storage.MutationOp{storage.MutationUpsert, storage.MutationUpsert, storage.MutationUpsert}), batch, []model.ColumnID{1, 2}, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	blk, err := openBlock(raw, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blk.Close(); err != nil {
			t.Error(err)
		}
	})
	boolean, err := blk.Column(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if boolean.IsNull(0) || !boolean.IsNull(2) || boolean.Int64[1] != 0 {
		t.Fatalf("boolean decode wrong: %+v", boolean)
	}
	text, err := blk.Column(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := text.Validate(); err != nil {
		t.Fatal(err)
	}
	want := []string{"a", "a", "b"}
	for i, w := range want {
		var got string
		if text.Encoding == columnar.Dictionary {
			r := text.Ranges[text.Indices[i]]
			got = string(text.Data[r.Offset : r.Offset+r.Length])
		} else {
			r := text.Ranges[i]
			got = string(text.Data[r.Offset : r.Offset+r.Length])
		}
		if got != w {
			t.Fatalf("row %d = %q, want %q", i, got, w)
		}
	}
}

func TestDecodeZeroColumnBlockExposesVisibility(t *testing.T) {
	batch := &columnar.Batch{Length: 2}
	keys := [][]byte{{'a'}, {'b'}}
	kinds := []storage.MutationOp{storage.MutationUpsert, storage.MutationDelete}
	raw, err := encodeBlock(t.Context(), decTestTarget(), decTestVersions(t, keys, kinds), batch, nil, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	blk, err := openBlock(raw, decTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blk.Close(); err != nil {
			t.Error(err)
		}
	})
	if blk.Rows() != 2 {
		t.Fatalf("rows = %d, want 2", blk.Rows())
	}
	for i, want := range keys {
		key, _, _, _, kind, err := blk.Version(i)
		if err != nil {
			t.Fatal(err)
		}
		if string(key) != string(want) || kind != kinds[i] {
			t.Fatalf("row %d = %q/%v, want %q/%v", i, key, kind, want, kinds[i])
		}
	}
	if _, err := blk.Column(t.Context(), 1); !errors.Is(err, errColumnMissing) {
		t.Fatalf("column on zero-column block = %v, want errColumnMissing", err)
	}
	if zone, err := blk.Zone(1); !errors.Is(err, errColumnMissing) || zone.Valid {
		t.Fatalf("zone on zero-column block = %+v/%v, want missing", zone, err)
	}
}
