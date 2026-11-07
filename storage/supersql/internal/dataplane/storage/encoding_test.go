package storage_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func encodedFixture() storage.Mutation {
	return storage.NewUpsertMutation(
		storage.ObjectRef{ObjectID: 7, Generation: 2, DescriptorVersion: 3, EncodingVersion: 1},
		[]byte("key"), []model.ColumnID{1, 70000, 90000},
		[]model.Value{
			model.Float64Val(math.Float64frombits(0x7ff8000000000017)),
			{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -4}}, ArrayVal: []model.Value{model.BytesVal([]byte{0, 255}), model.NullVal(model.TypeBytes)}},
			{Type: model.TypeInterval, IntervalVal: model.Interval{Months: -2, Days: 4, Microseconds: -1234567}},
		},
	)
}

func TestCanonicalMutationPreservesTypedBitsAndOwnsBuffers(t *testing.T) {
	m := encodedFixture()
	m.Ordinal = 9
	var encoded bytes.Buffer
	if err := storage.EncodeMutation(&encoded, m); err != nil {
		t.Fatal(err)
	}
	size, err := storage.EncodedMutationSize(m)
	if err != nil {
		t.Fatal(err)
	}
	if size != uint64(encoded.Len()) {
		t.Fatalf("framed size %d, payload bytes %d", size, encoded.Len())
	}
	decoded, err := storage.DecodeMutation(encoded.Bytes(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	clear(encoded.Bytes())
	if string(decoded.Key) != "key" || decoded.Ordinal != 9 || decoded.Target.Generation != 2 {
		t.Fatal("lost key/version identity")
	}
	if decoded.ColumnIDs[1] != 70000 || decoded.ColumnIDs[2] != 90000 {
		t.Fatal("column IDs truncated to legacy PAX width")
	}
	if math.Float64bits(decoded.Values[0].FloatVal) != 0x7ff8000000000017 {
		t.Fatal("NaN payload changed")
	}
	array := decoded.Values[1]
	if array.Dimensions[0].LowerBound != -4 || !bytes.Equal(array.ArrayVal[0].BytesVal, []byte{0, 255}) || !array.ArrayVal[1].IsNull {
		t.Fatal("array shape/null/payload changed")
	}
	interval := decoded.Values[2].IntervalVal
	if interval.Months != -2 || interval.Days != 4 || interval.Microseconds != -1234567 {
		t.Fatalf("interval changed: %+v", interval)
	}
}

func TestMutationChecksumSeparatesFramingAndFloatingBits(t *testing.T) {
	target := encodedFixture().Target
	cases := []struct {
		name string
		a, b storage.Mutation
	}{
		{"string boundaries", storage.NewUpsertMutation(target, []byte("k"), []model.ColumnID{1, 2}, []model.Value{model.StringVal("ab"), model.StringVal("c")}), storage.NewUpsertMutation(target, []byte("k"), []model.ColumnID{1, 2}, []model.Value{model.StringVal("a"), model.StringVal("bc")})},
		{"signed zero", storage.NewUpsertMutation(target, []byte("k"), []model.ColumnID{1}, []model.Value{model.Float64Val(0)}), storage.NewUpsertMutation(target, []byte("k"), []model.ColumnID{1}, []model.Value{model.Float64Val(math.Copysign(0, -1))})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, err := storage.MutationChecksum(tc.a)
			if err != nil {
				t.Fatal(err)
			}
			b, err := storage.MutationChecksum(tc.b)
			if err != nil {
				t.Fatal(err)
			}
			if a == b {
				t.Fatal("distinct canonical mutations have identical checksum")
			}
		})
	}
}

func TestMutationDecoderRejectsTruncationAndAllocationOverrun(t *testing.T) {
	var encoded bytes.Buffer
	if err := storage.EncodeMutation(&encoded, encodedFixture()); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	for end := range len(data) {
		if _, err := storage.DecodeMutation(data[:end], 4096); err == nil {
			t.Fatalf("accepted truncated mutation at byte %d", end)
		}
	}
	if _, err := storage.DecodeMutation(data, 1); !errors.Is(err, storage.ErrDecodeBudget) {
		t.Fatalf("allocation bound: %v", err)
	}
	if _, err := storage.DecodeMutation(append(bytes.Clone(data), 0), 4096); !errors.Is(err, storage.ErrInvalidEncoding) {
		t.Fatalf("trailing data: %v", err)
	}
}

func TestDurableBatchRejectsOperationIntentAndOrdinalReuse(t *testing.T) {
	first := storage.NewUpsertMutation(storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, []byte("key"), []model.ColumnID{70000}, []model.Value{model.Int64Val(7)})
	first.Ordinal = 1
	second := storage.NewDeleteMutation(first.Target, []byte("other"))
	second.Ordinal = 2
	b := storage.MutationBatch{TxID: model.TxnID{1}, CommitTS: 42, EncodingVersion: storage.MutationEncodingVersion, Mutations: []storage.Mutation{first, second}}
	b.Layouts = []storage.ObjectLayout{{Target: first.Target, Columns: []storage.ColumnLayout{{ID: 70000, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}}}
	seal := func() {
		t.Helper()
		var err error
		b.ChecksumCRC, err = b.ComputeChecksum(t.Context())
		if err != nil {
			t.Fatal(err)
		}
	}
	seal()
	if err := b.ValidateNormalized(t.Context()); err != nil {
		t.Fatal(err)
	}
	b.Mutations[0].Op = storage.MutationUpdate
	seal()
	if err := b.ValidateNormalized(t.Context()); !errors.Is(err, storage.ErrUnnormalizedBatch) {
		t.Fatalf("unresolved update: %v", err)
	}
	b.Mutations[0].Op = storage.MutationUpsert
	b.Mutations[1].Ordinal = 1
	seal()
	if err := b.ValidateNormalized(t.Context()); !errors.Is(err, storage.ErrUnnormalizedBatch) {
		t.Fatalf("reused ordinal: %v", err)
	}
	b.Mutations[1].Ordinal = 2
	seal()
	b.Mutations[0].Target.Generation++
	b.Mutations[1].Target.Generation++
	b.Layouts[0].Target.Generation++
	if err := b.ValidateNormalized(t.Context()); !errors.Is(err, storage.ErrChecksumMismatch) {
		t.Fatalf("changed target: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := b.ComputeChecksum(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled checksum: %v", err)
	}
}

func FuzzDecodeMutation(f *testing.F) {
	var encoded bytes.Buffer
	if err := storage.EncodeMutation(&encoded, encodedFixture()); err != nil {
		f.Fatal(err)
	}
	f.Add(encoded.Bytes())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			t.Skip()
		}
		m, err := storage.DecodeMutation(data, 131072)
		if err != nil {
			return
		}
		var canonical bytes.Buffer
		if err := storage.EncodeMutation(&canonical, m); err != nil {
			t.Fatalf("accepted unencodable mutation: %v", err)
		}
		if !bytes.Equal(canonical.Bytes(), data) {
			t.Fatal("decoder accepted a noncanonical representation")
		}
	})
}
