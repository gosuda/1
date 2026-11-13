package builtin_test

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestKeyNeoSepiaTypedPrefixScans(t *testing.T) {
	encoder := keyEncoder(t)
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	identity := sha256.Sum256(history)
	scale := clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: identity, CanonicalLeapHistory: history}
	epoch := clock.Epoch{Assurance: [16]byte{1}, LeapHistory: identity, Config: [32]byte{1}}
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := neosepia.Open(t.Context(), neosepia.Config{Directory: t.TempDir(), Scale: scale, KeyEncoder: encoder, FenceVerifier: authority})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	target := storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	layout := []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 17, Storage: model.TypeBytes}, Nullable: true}, {ID: 2, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}
	keys := []storage.KeyColumn{{ColumnID: 1, Type: layout[0].Type, NullsFirst: true}, {ColumnID: 2, Type: layout[1].Type}}
	rows := [][]model.Value{
		{model.NullVal(model.TypeBytes), model.Int64Val(-1)},
		{model.BytesVal(nil), model.Int64Val(-2)},
		{model.BytesVal([]byte{0}), model.Int64Val(math.MinInt64)},
		{model.BytesVal([]byte{0}), model.Int64Val(math.MaxInt64)},
		{model.BytesVal([]byte{0, 0}), model.Int64Val(0)},
		{model.BytesVal([]byte{255}), model.Int64Val(1)},
	}
	batch := storage.MutationBatch{TxID: model.TxnID{1}, CommitTS: 20, Epoch: epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: layout}}}
	for i, row := range rows {
		batch.Mutations = append(batch.Mutations, storage.Mutation{Op: storage.MutationUpsert, Target: target, Key: encodeKey(t, encoder, keys, row...), Ordinal: uint64(i + 1), ColumnIDs: []model.ColumnID{1, 2}, Values: row})
	}
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	state, err := backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := authority.Issue(t.Context(), 20, state.LogPosition, epoch)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 20, Fence: fence})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, tc := range []struct {
		name string
		span storage.KeySpan
		want []int64
	}{
		{"unbounded", storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}, []int64{-1, -2, math.MinInt64, math.MaxInt64, 0, 1}},
		{"zero byte prefix", storage.KeySpan{Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{0})}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{0})}, Inclusive: true}}, []int64{math.MinInt64, math.MaxInt64}},
		{"empty is finite", storage.KeySpan{Lower: storage.KeyBound{Values: []model.Value{model.BytesVal(nil)}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.BytesVal(nil)}, Inclusive: true}}, []int64{-2}},
		{"NULL is finite", storage.KeySpan{Lower: storage.KeyBound{Values: []model.Value{model.NullVal(model.TypeBytes)}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.NullVal(model.TypeBytes)}, Inclusive: true}}, []int64{-1}},
		{"exclusive prefix", storage.KeySpan{Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{0})}}, Upper: storage.KeyBound{Unbounded: true}}, []int64{0, 1}},
		{"maximum integer endpoint", storage.KeySpan{Lower: storage.KeyBound{Values: rows[3], Inclusive: true}, Upper: storage.KeyBound{Values: rows[3], Inclusive: true}}, []int64{math.MaxInt64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
				plan, err := reader.PlanScan(t.Context(), storage.PushdownRequest{Target: target, Layout: layout, KeyLayout: keys, ColumnIDs: []model.ColumnID{2}, Spans: []storage.KeySpan{tc.span}, Direction: direction})
				if err != nil {
					t.Fatal(err)
				}
				cursor, err := reader.VectorizedScan(t.Context(), plan)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := cursor.Close(); err != nil {
						t.Error(err)
					}
				})
				var got []int64
				out := columnar.Batch{MaxRows: 1}
				for {
					err := cursor.Next(t.Context(), &out)
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					for i := range out.Rows() {
						value, _, err := columnar.CopyValue(t.Context(), &out.Columns[0], out.Lane(i), 100)
						if err != nil {
							t.Fatal(err)
						}
						got = append(got, value.IntVal)
					}
				}
				want := slices.Clone(tc.want)
				if direction == storage.ScanReverse {
					slices.Reverse(want)
				}
				if !slices.Equal(got, want) {
					t.Fatalf("direction %d: got %v, want %v", direction, got, want)
				}
			}
		})
	}
}

func TestKeyCanonicalizationDoesNotNormalizeMutationChecksum(t *testing.T) {
	encoder := keyEncoder(t)
	typ := model.Type{ID: 701, Storage: model.TypeFloat64}
	layout := []storage.KeyColumn{{ColumnID: 1, Type: typ}}
	target := storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}
	for _, pair := range [][2]uint64{{0, 1 << 63}, {0x7ff8000000000001, 0xfff8000000000002}} {
		var checksums [2]uint32
		var keys [2][]byte
		for i, bits := range pair {
			values := []model.Value{model.Float64Val(math.Float64frombits(bits))}
			keys[i] = encodeKey(t, encoder, layout, values...)
			batch := storage.MutationBatch{TxID: model.TxnID{1}, CommitTS: 20, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: []storage.ColumnLayout{{ID: 1, Type: typ}}}}, Mutations: []storage.Mutation{{Op: storage.MutationUpsert, Target: target, Key: keys[i], Ordinal: 1, ColumnIDs: []model.ColumnID{1}, Values: values}}}
			var err error
			checksums[i], err = batch.ComputeChecksum(t.Context())
			if err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(keys[0], keys[1]) || checksums[0] == checksums[1] {
			t.Fatalf("logical equality must preserve physical checksum distinction: keys %x, checksums %x", keys, checksums)
		}
	}
}
