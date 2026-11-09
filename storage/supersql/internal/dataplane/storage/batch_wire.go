package storage

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// MaxAllocation includes immutable metadata and each cursor's frame and decoded
// value buffers. Independent cursors each receive this budget, not a shared pool.
type BatchDecodeLimits struct{ MaxBytes, MaxAllocation, MaxMutationBytes uint64 }

// EncodeMutationBatch streams canonical bytes and can leave a prefix on error.
// The caller must discard a failed frame; the destination is never retained.
func EncodeMutationBatch(ctx context.Context, dst io.Writer, batch MutationBatch) error {
	if dst == nil {
		return ErrInvalidEncoding
	}
	checksum, err := batch.encode(ctx, true, nil, dst)
	if err != nil {
		return err
	}
	if checksum != batch.ChecksumCRC {
		return ErrChecksumMismatch
	}
	return nil
}

// OpenEncodedMutationBatch borrows a region beginning at source offset zero.
// The caller keeps it immutable and alive until all sequence cursors close.
// Layout metadata is owned; mutations are decoded one at a time on each cursor.
func OpenEncodedMutationBatch(ctx context.Context, source io.ReaderAt, size uint64, limits BatchDecodeLimits) (MutationBatch, error) {
	if err := ctx.Err(); err != nil {
		return MutationBatch{}, err
	}
	if source == nil || size == 0 {
		return MutationBatch{}, ErrInvalidEncoding
	}
	if size > limits.MaxBytes || size > math.MaxInt64 || limits.MaxAllocation > uint64(math.MaxInt) || limits.MaxMutationBytes == 0 {
		return MutationBatch{}, ErrDecodeBudget
	}
	overhead := uint64(unsafe.Sizeof(encodedBatchSequence{}))
	if overhead > limits.MaxAllocation {
		return MutationBatch{}, ErrDecodeBudget
	}
	region := decodeRegion{ctx: ctx, source: source, size: size}
	d := mutationDecoder{region: &region, budget: limits.MaxAllocation - overhead}
	b := MutationBatch{EncodingVersion: uint32(d.number(4))}
	readBatchFixed(&d, b.TxID[:])
	b.CommitTS = model.Timestamp(d.number(8))
	readBatchFixed(&d, b.Epoch.Assurance[:])
	readBatchFixed(&d, b.Epoch.LeapHistory[:])
	readBatchFixed(&d, b.Epoch.Config[:])
	count := d.count(uint64(unsafe.Sizeof(ObjectLayout{})), 36)
	if d.err != nil {
		return MutationBatch{}, d.err
	}
	b.Layouts = make([]ObjectLayout, count)
	for i := range b.Layouts {
		l := &b.Layouts[i]
		l.Target = ObjectRef{ObjectID: model.ObjectID(d.number(8)), Generation: d.number(8), DescriptorVersion: d.number(8), EncodingVersion: uint32(d.number(4))}
		columns := d.count(uint64(unsafe.Sizeof(ColumnLayout{})), 26)
		if d.err != nil {
			return MutationBatch{}, d.err
		}
		l.Columns = make([]ColumnLayout, columns)
		for j := range l.Columns {
			column := &l.Columns[j]
			column.ID = model.ColumnID(d.number(4))
			layout := decodeBatchType(&d, 0)
			column.Type, column.Children = layout.Type, layout.Children
			flags := d.number(1)
			if flags > 3 {
				return MutationBatch{}, ErrInvalidEncoding
			}
			column.Nullable, column.HasMissingValue = flags&1 != 0, flags&2 != 0
			if column.HasMissingValue {
				column.MissingValue = d.value(0)
			}
			if d.err != nil {
				return MutationBatch{}, d.err
			}
		}
	}
	mutations := d.number(8)
	if d.err != nil {
		return MutationBatch{}, d.err
	}
	if mutations == 0 || mutations > d.remaining()/8 {
		return MutationBatch{}, ErrInvalidEncoding
	}
	sequence := &encodedBatchSequence{source: source, offset: region.offset, size: size, count: mutations, limits: limits, metadata: limits.MaxAllocation - d.budget}
	b.Sequence = sequence
	checksum, err := b.checksum(ctx, true, nil)
	if err != nil {
		return MutationBatch{}, err
	}
	b.ChecksumCRC = checksum
	return b, nil
}

func readBatchFixed(d *mutationDecoder, destination []byte) {
	if d.number(8) != uint64(len(destination)) {
		if d.err == nil {
			d.err = ErrInvalidEncoding
		}
		return
	}
	if d.err == nil {
		d.err = d.region.read(destination)
	}
}

func decodeBatchType(d *mutationDecoder, depth int) model.TypeLayout {
	if depth > maxValueDepth {
		d.err = ErrInvalidEncoding
		return model.TypeLayout{}
	}
	result := model.TypeLayout{Type: model.Type{ID: model.TypeID(d.number(4)), Storage: model.DataType(d.number(1)), Modifier: int32(d.number(4)), Collation: model.CollationID(d.number(4))}}
	count := d.count(uint64(unsafe.Sizeof(model.TypeLayout{})), 21)
	if d.err != nil {
		return model.TypeLayout{}
	}
	if count != 0 {
		result.Children = make([]model.TypeLayout, count)
	}
	for i := range result.Children {
		result.Children[i] = decodeBatchType(d, depth+1)
		if d.err != nil {
			return model.TypeLayout{}
		}
	}
	return result
}

type decodeRegion struct {
	ctx          context.Context
	source       io.ReaderAt
	offset, size uint64
	scratch      [8]byte
}

func (r *decodeRegion) read(destination []byte) error {
	if uint64(len(destination)) > r.size-r.offset {
		return ErrInvalidEncoding
	}
	for len(destination) != 0 {
		if err := r.ctx.Err(); err != nil {
			return err
		}
		chunk := destination[:min(len(destination), 4096)]
		n, err := r.source.ReadAt(chunk, int64(r.offset))
		if n != len(chunk) {
			return errors.Join(ErrInvalidEncoding, err, io.ErrUnexpectedEOF)
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		r.offset += uint64(n)
		destination = destination[n:]
	}
	return nil
}

type encodedBatchSequence struct {
	source              io.ReaderAt
	offset, size, count uint64
	metadata            uint64
	limits              BatchDecodeLimits
}

func (s *encodedBatchSequence) Count() uint64 { return s.count }
func (s *encodedBatchSequence) Open(ctx context.Context) (MutationCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if uint64(unsafe.Sizeof(encodedBatchCursor{})) > s.limits.MaxAllocation-s.metadata {
		return nil, ErrDecodeBudget
	}
	return &encodedBatchCursor{sequence: s, offset: s.offset, remaining: s.count}, nil
}

type encodedBatchCursor struct {
	sequence          *encodedBatchSequence
	offset, remaining uint64
	frame             []byte
}

func (c *encodedBatchCursor) Next(ctx context.Context) (Mutation, error) {
	if c.sequence == nil {
		return Mutation{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Mutation{}, err
	}
	s := c.sequence
	if c.remaining == 0 {
		if c.offset != s.size {
			return Mutation{}, ErrInvalidEncoding
		}
		return Mutation{}, io.EOF
	}
	reader := decodeRegion{ctx: ctx, source: s.source, offset: c.offset, size: s.size}
	if err := reader.read(reader.scratch[:]); err != nil {
		return Mutation{}, err
	}
	size := binary.LittleEndian.Uint64(reader.scratch[:])
	if size == 0 || size > reader.size-reader.offset {
		return Mutation{}, ErrInvalidEncoding
	}
	available := s.limits.MaxAllocation - s.metadata - uint64(unsafe.Sizeof(encodedBatchCursor{}))
	if size > s.limits.MaxMutationBytes || size > available || size > uint64(math.MaxInt) {
		return Mutation{}, ErrDecodeBudget
	}
	if size > uint64(cap(c.frame)) {
		if uint64(cap(c.frame)) > available-size {
			return Mutation{}, ErrDecodeBudget
		}
		c.frame = make([]byte, size)
	} else {
		c.frame = c.frame[:size]
	}
	if err := reader.read(c.frame); err != nil {
		return Mutation{}, err
	}
	mutation, err := DecodeMutation(c.frame, available-uint64(cap(c.frame)))
	if err != nil {
		return Mutation{}, err
	}
	c.offset = reader.offset
	c.remaining--
	return mutation, nil
}

func (c *encodedBatchCursor) Close() error {
	c.sequence = nil
	c.frame = nil
	return nil
}
