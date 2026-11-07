package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"hash/crc32"
	"io"

	"github.com/gosuda/supersql-alpha/internal/model"
)

// ComputeChecksum hashes the canonical batch, excluding checksum fields. It
// accepts operation intents; ValidateNormalized additionally enforces the
// backend's full-image/tombstone boundary and ordered nonzero ordinals.
func (b MutationBatch) ComputeChecksum(ctx context.Context) (uint32, error) {
	return b.checksum(ctx, false, nil)
}

func (b MutationBatch) ValidateNormalized(ctx context.Context) error {
	checksum, err := b.checksum(ctx, true, nil)
	if err != nil {
		return err
	}
	if checksum != b.ChecksumCRC {
		return ErrChecksumMismatch
	}
	return nil
}

// Identity validates the durable batch and returns its collision-resistant
// identity. CRC32 alone must never decide whether a reused TxID is idempotent.
func (b MutationBatch) Identity(ctx context.Context) ([32]byte, error) {
	digest := sha256.New()
	checksum, err := b.checksum(ctx, true, digest)
	if err != nil {
		return [32]byte{}, err
	}
	if checksum != b.ChecksumCRC {
		return [32]byte{}, ErrChecksumMismatch
	}
	var identity [32]byte
	digest.Sum(identity[:0])
	return identity, nil
}

func (b MutationBatch) Count() uint64 {
	if b.Sequence != nil {
		return b.Sequence.Count()
	}
	return uint64(len(b.Mutations))
}

func (b MutationBatch) OpenMutations(ctx context.Context) (MutationCursor, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if b.Sequence != nil {
		if b.Mutations != nil {
			return nil, ErrInvalidMutation
		}
		return b.Sequence.Open(ctx)
	}
	return &sliceMutationCursor{mutations: b.Mutations}, nil
}

func (b MutationBatch) checksum(ctx context.Context, normalized bool, digest hash.Hash) (result uint32, err error) {
	return b.encode(ctx, normalized, digest, nil)
}

func (b MutationBatch) encode(ctx context.Context, normalized bool, digest hash.Hash, destination io.Writer) (result uint32, err error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if b.EncodingVersion != MutationEncodingVersion {
		return 0, ErrInvalidEncoding
	}
	if b.TxID == (model.TxnID{}) || b.CommitTS <= 0 {
		return 0, ErrInvalidMutation
	}
	if b.Sequence != nil && b.Mutations != nil {
		return 0, ErrInvalidMutation
	}
	count := b.Count()
	if count == 0 {
		return 0, ErrBatchEmpty
	}
	if err := b.validateLayouts(ctx); err != nil {
		return 0, err
	}
	cursor, err := b.OpenMutations(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	h := checksumWriter{ctx: ctx, digest: digest, destination: destination}
	e := mutationEncoder{writer: &h}
	e.number(uint64(b.EncodingVersion), 4)
	e.bytes(b.TxID[:])
	e.number(uint64(b.CommitTS), 8)
	e.bytes(b.Epoch.Assurance[:])
	e.bytes(b.Epoch.LeapHistory[:])
	e.bytes(b.Epoch.Config[:])
	e.number(uint64(len(b.Layouts)), 8)
	for _, layout := range b.Layouts {
		e.objectLayout(layout)
	}
	e.number(count, 8)
	var previousOrdinal uint64
	for range count {
		m, nextErr := cursor.Next(ctx)
		if nextErr != nil {
			if nextErr == io.EOF {
				return 0, ErrInvalidMutation
			}
			return 0, nextErr
		}
		if normalized {
			if m.Ordinal <= previousOrdinal {
				return 0, ErrUnnormalizedBatch
			}
			previousOrdinal = m.Ordinal
			switch m.Op {
			case MutationUpsert, MutationDelete, MutationDeleteRange:
			default:
				return 0, ErrUnnormalizedBatch
			}
			if err := b.validateRowLayout(ctx, m); err != nil {
				return 0, err
			}
		}
		size, sizeErr := EncodedMutationSize(m)
		if sizeErr != nil {
			return 0, sizeErr
		}
		e.number(size, 8)
		e.flush()
		h.mutation = 0
		e.mutation(m)
		e.flush()
		if e.err != nil {
			return 0, e.err
		}
		if m.ChecksumCRC != 0 && h.mutation != m.ChecksumCRC {
			return 0, ErrChecksumMismatch
		}
	}
	_, err = cursor.Next(ctx)
	if err != io.EOF {
		if err == nil {
			return 0, ErrInvalidMutation
		}
		return 0, err
	}
	return h.batch, nil
}

type checksumWriter struct {
	ctx         context.Context
	batch       uint32
	mutation    uint32
	digest      hash.Hash
	destination io.Writer
}

func (w *checksumWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if w.destination != nil {
		n, err := w.destination.Write(data)
		if err != nil {
			return n, err
		}
		if n != len(data) {
			return n, io.ErrShortWrite
		}
	}
	w.batch = crc32.Update(w.batch, crc32.IEEETable, data)
	w.mutation = crc32.Update(w.mutation, crc32.IEEETable, data)
	if w.digest != nil {
		if _, err := w.digest.Write(data); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

type sliceMutationCursor struct {
	mutations []Mutation
	position  int
	closed    bool
}

func (c *sliceMutationCursor) Next(ctx context.Context) (Mutation, error) {
	if c.closed {
		return Mutation{}, ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return Mutation{}, err
	}
	if c.position == len(c.mutations) {
		return Mutation{}, io.EOF
	}
	m := c.mutations[c.position]
	c.position++
	return m, nil
}

func (c *sliceMutationCursor) Close() error {
	c.mutations = nil
	c.closed = true
	return nil
}
