package storage

import (
	"bytes"
	"context"
	"io"

	"github.com/gosuda/supersql-alpha/internal/model"
)

const valueWireMagic = 0x3156534e // NSV1

// EncodeValue streams the same typed value representation used by mutations.
// A failed encoding can leave a prefix; callers must discard it. maxBytes bounds
// the complete encoded value, including its independent format header.
func EncodeValue(ctx context.Context, destination io.Writer, value model.Value, maxBytes uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if destination == nil {
		return ErrInvalidEncoding
	}
	bounded := valueWireWriter{ctx: ctx, destination: destination, remaining: maxBytes}
	encoder := mutationEncoder{writer: &bounded}
	encoder.number(valueWireMagic, 4)
	encoder.value(value, 0)
	encoder.flush()
	if encoder.err != nil {
		return encoder.err
	}
	return ctx.Err()
}

// DecodeValue owns variable-width payloads. maxAllocation bounds its owned and
// temporary decoded storage; the caller separately owns/accounts for input data.
func DecodeValue(ctx context.Context, data []byte, maxAllocation uint64) (model.Value, error) {
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	reader := bytes.NewReader(data)
	region := decodeRegion{ctx: ctx, source: reader, size: uint64(len(data))}
	decoder := mutationDecoder{region: &region, budget: maxAllocation}
	magic := decoder.number(4)
	if decoder.err != nil {
		return model.Value{}, decoder.err
	}
	if magic != valueWireMagic {
		return model.Value{}, ErrInvalidEncoding
	}
	value := decoder.value(0)
	if decoder.err != nil {
		return model.Value{}, decoder.err
	}
	if region.offset != region.size {
		return model.Value{}, ErrInvalidEncoding
	}
	if err := ctx.Err(); err != nil {
		return model.Value{}, err
	}
	return value, nil
}

type valueWireWriter struct {
	ctx         context.Context
	destination io.Writer
	remaining   uint64
}

func (writer *valueWireWriter) Write(data []byte) (int, error) {
	if err := writer.ctx.Err(); err != nil {
		return 0, err
	}
	if uint64(len(data)) > writer.remaining {
		return 0, model.ErrValueBudget
	}
	n, err := writer.destination.Write(data)
	if n < 0 || n > len(data) {
		return 0, io.ErrShortWrite
	}
	writer.remaining -= uint64(n)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}
