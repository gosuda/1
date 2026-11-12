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

func TestValueWirePreservesFloatingPointBits(t *testing.T) {
	for _, bits := range []uint64{0, 1 << 63, 0x7ff8000000001234, 0x7ff0000000000000, 0xfff0000000000000} {
		var encoded bytes.Buffer
		if err := storage.EncodeValue(t.Context(), &encoded, model.Float64Val(math.Float64frombits(bits)), 64); err != nil {
			t.Fatal(err)
		}
		value, err := storage.DecodeValue(t.Context(), encoded.Bytes(), 64)
		if err != nil {
			t.Fatal(err)
		}
		if value.Type != model.TypeFloat64 || value.IsNull || math.Float64bits(value.FloatVal) != bits {
			t.Fatalf("float bits changed: got %016x, want %016x", math.Float64bits(value.FloatVal), bits)
		}
	}
}

func TestValueWireOwnsNestedPayloadsAndPreservesNullTypes(t *testing.T) {
	original := model.Value{Type: model.TypeArray, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: math.MaxInt32 - 1}}, ArrayVal: []model.Value{model.BytesVal([]byte("payload")), model.NullVal(model.TypeBytes)}}
	var encoded bytes.Buffer
	if err := storage.EncodeValue(t.Context(), &encoded, original, 4096); err != nil {
		t.Fatal(err)
	}
	value, err := storage.DecodeValue(t.Context(), encoded.Bytes(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	clear(encoded.Bytes())
	original.ArrayVal[0].BytesVal[0] = 'X'
	if value.Type != model.TypeArray || len(value.Dimensions) != 1 || value.Dimensions[0].LowerBound != math.MaxInt32-1 || len(value.ArrayVal) != 2 {
		t.Fatalf("nested shape lost: %+v", value)
	}
	if string(value.ArrayVal[0].BytesVal) != "payload" || !value.ArrayVal[1].IsNull || value.ArrayVal[1].Type != model.TypeBytes {
		t.Fatalf("nested values aliased or lost type: %+v", value.ArrayVal)
	}
}

type cancelValueWriter struct {
	buffer bytes.Buffer
	cancel context.CancelFunc
}

func (w *cancelValueWriter) Write(data []byte) (int, error) {
	n, err := w.buffer.Write(data)
	w.cancel()
	return n, err
}

func TestValueWireRejectsBudgetAndCancellation(t *testing.T) {
	value := model.BytesVal(make([]byte, 32<<10))
	var encoded bytes.Buffer
	if err := storage.EncodeValue(t.Context(), &encoded, value, 64); !errors.Is(err, model.ErrValueBudget) {
		t.Fatalf("encoding exceeded byte quota: %v", err)
	}
	if encoded.Len() > 64 {
		t.Fatalf("encoded %d bytes under 64-byte quota", encoded.Len())
	}
	encoded.Reset()
	if err := storage.EncodeValue(t.Context(), &encoded, value, 64<<10); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.DecodeValue(t.Context(), encoded.Bytes(), 64); !errors.Is(err, storage.ErrDecodeBudget) {
		t.Fatalf("decode exceeded allocation quota: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	writer := &cancelValueWriter{cancel: cancel}
	if err := storage.EncodeValue(ctx, writer, value, 64<<10); !errors.Is(err, context.Canceled) {
		t.Fatalf("streaming encode ignored cancellation: %v", err)
	}
	if _, err := storage.DecodeValue(ctx, encoded.Bytes(), 64<<10); !errors.Is(err, context.Canceled) {
		t.Fatalf("decode ignored cancellation: %v", err)
	}
}

func TestValueWireRejectsTruncationAndTrailingData(t *testing.T) {
	var encoded bytes.Buffer
	if err := storage.EncodeValue(t.Context(), &encoded, model.StringVal("typed"), 1024); err != nil {
		t.Fatal(err)
	}
	complete := encoded.Bytes()
	for end := range complete {
		if _, err := storage.DecodeValue(t.Context(), complete[:end], 1024); err == nil {
			t.Fatalf("accepted prefix of %d/%d bytes", end, len(complete))
		}
	}
	trailing := append(bytes.Clone(complete), 0)
	if _, err := storage.DecodeValue(t.Context(), trailing, 1024); !errors.Is(err, storage.ErrInvalidEncoding) {
		t.Fatalf("accepted trailing data: %v", err)
	}
	complete[0] ^= 1
	if _, err := storage.DecodeValue(t.Context(), complete, 1024); !errors.Is(err, storage.ErrInvalidEncoding) {
		t.Fatalf("accepted unknown format header: %v", err)
	}
	encoded.Reset()
	if err := storage.EncodeValue(t.Context(), &encoded, model.Value{Type: model.TypeBoolean, IntVal: 2}, 1024); !errors.Is(err, storage.ErrInvalidEncoding) {
		t.Fatalf("accepted malformed boolean: %v", err)
	}
}
