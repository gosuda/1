package rowstore

import (
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const (
	frameHeaderBytes = 32
	frameMagic       = 0x31575352 // RSW1
	// Includes the canonical codec's 4096-byte encoder and decoder bookkeeping.
	codecScratchBytes = 8192
)

// RSW1: magic:u32, columns:u32, offset:u64, payloadBytes:u64,
// payloadCRC32:u32, headerCRC32:u32, then one NSV1 composite value.
// Integers are little endian; IEEE CRCs cover payload and the first 28 bytes.
func (s *Spool) encode(ctx context.Context, row []model.Value) ([]byte, error) {
	if err := validateRow(ctx, row, s.layouts); err != nil {
		return nil, err
	}
	writer := frameWriter{data: s.scratch, used: frameHeaderBytes}
	if err := storage.EncodeValue(ctx, &writer, model.Value{Type: model.TypeComposite, ArrayVal: row}, s.limits.MaxRecordBytes-frameHeaderBytes); err != nil {
		return nil, err
	}
	frame := writer.data[:writer.used]
	binary.LittleEndian.PutUint32(frame[0:4], frameMagic)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(row)))
	binary.LittleEndian.PutUint64(frame[8:16], s.bytes)
	binary.LittleEndian.PutUint64(frame[16:24], uint64(len(frame)-frameHeaderBytes))
	binary.LittleEndian.PutUint32(frame[24:28], crc32.ChecksumIEEE(frame[frameHeaderBytes:]))
	binary.LittleEndian.PutUint32(frame[28:32], crc32.ChecksumIEEE(frame[:28]))
	return frame, context.Cause(ctx)
}

type frameWriter struct {
	data []byte
	used int
}

func (w *frameWriter) Write(data []byte) (int, error) {
	if len(data) > len(w.data)-w.used {
		return 0, io.ErrShortWrite
	}
	n := copy(w.data[w.used:], data)
	w.used += n
	return n, nil
}

func validateRow(ctx context.Context, row []model.Value, layouts []model.TypeLayout) error {
	if len(row) != len(layouts) {
		return columnar.ErrValueLayout
	}
	for i := range layouts {
		if err := validateValue(ctx, row[i], layouts[i], 0); err != nil {
			return err
		}
	}
	return nil
}

func validateValue(ctx context.Context, value model.Value, layout model.TypeLayout, depth int) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if depth >= 64 || value.Type != layout.Type.Storage {
		return columnar.ErrValueLayout
	}
	if value.IsNull {
		return nil
	}
	switch value.Type {
	case model.TypeArray:
		if err := model.ValidateArrayShape(value.Dimensions, uint64(len(value.ArrayVal))); err != nil {
			return err
		}
		for _, child := range value.ArrayVal {
			if err := validateValue(ctx, child, layout.Children[0], depth+1); err != nil {
				return err
			}
		}
	case model.TypeComposite:
		if len(value.ArrayVal) != len(layout.Children) || len(value.Dimensions) != 0 {
			return columnar.ErrValueLayout
		}
		for i := range layout.Children {
			if err := validateValue(ctx, value.ArrayVal[i], layout.Children[i], depth+1); err != nil {
				return err
			}
		}
	case model.TypeBoolean:
		if value.IntVal != 0 && value.IntVal != 1 {
			return columnar.ErrValueLayout
		}
	}
	return nil
}
