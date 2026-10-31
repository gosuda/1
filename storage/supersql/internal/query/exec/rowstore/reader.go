package rowstore

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// Reader pins an immutable extent. Values returned by NextRow and Read are borrowed
// until the next read (including a failed read) or Close. Read leaves the
// sequential cursor unchanged. Concurrent calls are serialized, but consumers
// must finish using borrowed values before another call begins.
type Reader struct {
	mu       sync.Mutex
	spool    *Spool
	spill    *runtime.Reader
	claim    *runtime.Reservation
	frame    []byte
	row      []model.Value
	limit    uint64
	next     uint64
	prefix   bool
	closed   bool
	closeErr error
}

func readerMetadata() uint64 { return uint64(unsafe.Sizeof(Reader{})) + 128 }

func (s *Spool) Reader(ctx context.Context) (*Reader, error) {
	return s.reader(ctx, false)
}

// PrefixReader captures the complete append extent, including an empty extent,
// without sealing the spool. EOF ends only this prefix; later appends require a
// new reader. Close releases the spool pin. A memory prefix acquires its runtime
// file-reader admission on its first read after migration, which may fail.
func (s *Spool) PrefixReader(ctx context.Context) (*Reader, error) {
	return s.reader(ctx, true)
}

func (s *Spool) reader(ctx context.Context, prefix bool) (*Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	if !prefix && !s.sealed {
		return nil, ErrNotSealed
	}
	if s.readers == s.limits.MaxReaders || readerMetadata() > s.limits.MetadataBytes-s.metadata {
		return nil, limitError("admit row spool reader")
	}
	claim, err := s.query.TryReserve(runtime.WorkMemory, int64(s.limits.MaxRecordBytes+s.limits.DecodeBytes+codecScratchBytes))
	if err != nil {
		return nil, err
	}
	var spill *runtime.Reader
	if s.spill != nil {
		spill, err = s.spill.Reader(ctx)
		if err != nil {
			claim.Release()
			return nil, err
		}
	}
	r := &Reader{spool: s, spill: spill, claim: claim, limit: s.bytes, prefix: prefix, frame: make([]byte, int(s.limits.MaxRecordBytes))}
	s.metadata += readerMetadata()
	s.readers++
	return r, nil
}

func (r *Reader) NextRow(ctx context.Context) (Position, []model.Value, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	position := Position(r.next)
	row, size, err := r.read(ctx, position, true)
	if err != nil {
		return position, nil, err
	}
	r.next += size
	return position, row, nil
}

func (r *Reader) Read(ctx context.Context, position Position) ([]model.Value, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, _, err := r.read(ctx, position, false)
	return row, err
}

// Seek positions the sequential cursor at a frame boundary or the captured end.
// It validates the frame header, not the payload (validated by NextRow), and
// invalidates borrowed values even on failure. Failure leaves the cursor intact.
func (r *Reader) Seek(ctx context.Context, position Position) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.row = nil
	if r.closed {
		return runtime.ErrClosed
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(r.spool.query.Context())); err != nil {
		return err
	}
	if uint64(position) != r.limit {
		if _, err := r.readHeader(ctx, uint64(position)); err != nil {
			return err
		}
	}
	r.next = uint64(position)
	return nil
}

func (r *Reader) read(ctx context.Context, position Position, sequential bool) ([]model.Value, uint64, error) {
	r.row = nil
	if r.closed {
		return nil, 0, runtime.ErrClosed
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(r.spool.query.Context())); err != nil {
		return nil, 0, err
	}
	offset := uint64(position)
	if sequential && offset == r.limit {
		return nil, 0, io.EOF
	}
	size, err := r.readHeader(ctx, offset)
	if err != nil {
		return nil, 0, err
	}
	header := r.frame[:frameHeaderBytes]
	payload := r.frame[frameHeaderBytes : frameHeaderBytes+int(size)]
	if err := r.readAt(ctx, payload, offset+frameHeaderBytes); err != nil {
		return nil, 0, err
	}
	if crc32.ChecksumIEEE(payload) != binary.LittleEndian.Uint32(header[24:28]) {
		return nil, 0, ErrCorrupt
	}
	value, err := storage.DecodeValue(ctx, payload, r.spool.limits.DecodeBytes)
	if err != nil {
		return nil, 0, errors.Join(ErrCorrupt, err)
	}
	if value.Type != model.TypeComposite || value.IsNull {
		return nil, 0, ErrCorrupt
	}
	if err := validateRow(ctx, value.ArrayVal, r.spool.layouts); err != nil {
		return nil, 0, errors.Join(ErrCorrupt, err)
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(r.spool.query.Context())); err != nil {
		return nil, 0, err
	}
	r.row = value.ArrayVal
	return r.row, size + frameHeaderBytes, nil
}

func (r *Reader) readHeader(ctx context.Context, offset uint64) (uint64, error) {
	if offset >= r.limit || r.limit-offset < frameHeaderBytes {
		return 0, ErrPosition
	}
	header := r.frame[:frameHeaderBytes]
	if err := r.readAt(ctx, header, offset); err != nil {
		return 0, err
	}
	if binary.LittleEndian.Uint32(header[:4]) != frameMagic || binary.LittleEndian.Uint64(header[8:16]) != offset {
		return 0, errors.Join(ErrPosition, ErrCorrupt)
	}
	if binary.LittleEndian.Uint32(header[4:8]) != uint32(len(r.spool.layouts)) || binary.LittleEndian.Uint32(header[28:32]) != crc32.ChecksumIEEE(header[:28]) {
		return 0, ErrCorrupt
	}
	size := binary.LittleEndian.Uint64(header[16:24])
	if size > r.spool.limits.MaxRecordBytes-frameHeaderBytes || size > r.limit-offset-frameHeaderBytes {
		return 0, ErrCorrupt
	}
	return size, nil
}

func (r *Reader) readAt(ctx context.Context, data []byte, offset uint64) error {
	if r.prefix && r.spill == nil {
		// The reader lock precedes the spool lock. Never retain a memory slice:
		// migration releases its reservation as soon as the file owns the bytes.
		r.spool.mu.Lock()
		defer r.spool.mu.Unlock()
		if r.spool.spill != nil {
			spill, err := r.spool.spill.Reader(ctx)
			if err != nil {
				return err
			}
			r.spill = spill
		}
	}
	if r.spill == nil {
		copy(data, r.spool.memory[int(offset):int(offset)+len(data)])
		return nil
	}
	n, err := r.spill.ReadAt(ctx, data, int64(offset))
	if n != len(data) {
		return errors.Join(ErrCorrupt, io.ErrUnexpectedEOF, err)
	}
	if err != nil {
		return errors.Join(ErrCorrupt, err)
	}
	return nil
}

func (r *Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	r.row, r.frame = nil, nil
	if r.spill != nil {
		r.closeErr = r.spill.Close()
	}
	r.claim.Release()
	s := r.spool
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readers--
	s.metadata -= readerMetadata()
	if s.closed && s.readers == 0 {
		s.cleanup()
		r.closeErr = errors.Join(r.closeErr, s.closeErr)
	}
	return r.closeErr
}
