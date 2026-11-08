package runtime

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"sync"
)

const ioChunkBytes = 64 << 10

// SpillFile must obey os.File's positional I/O and truncate contracts. A failed
// write may change only the requested extent. CreateTemp returns an empty file
// in the supplied directory; implementations must not fall back to another path.
type SpillFile interface {
	Name() string
	ReadAt([]byte, int64) (int, error)
	WriteAt([]byte, int64) (int, error)
	Truncate(int64) error
	Stat() (os.FileInfo, error)
	Close() error
}

type FileSystem interface {
	CreateTemp(directory, pattern string) (SpillFile, error)
	Remove(name string) error
}

type OSFileSystem struct{}

func (OSFileSystem) CreateTemp(directory, pattern string) (SpillFile, error) {
	if directory == "" {
		return nil, ErrInvalidConfig
	}
	return os.CreateTemp(directory, pattern)
}
func (OSFileSystem) Remove(name string) error { return os.Remove(name) }

// Spill owns the writer reference. Close prevents new readers/writes, but the
// file and its charge survive until the last admitted Reader closes. WaitClosed
// joins physical cleanup. Query.Close performs both operations for every file.
type Spill struct {
	query       *Query
	file        SpillFile
	claim       *Reservation
	mu          sync.Mutex
	size        int64
	readers     int
	ownerClosed bool
	fenced      error
	cleanupErr  error
	done        chan struct{}
	cleaned     bool
}

func (q *Query) NewSpill(ctx context.Context) (*Spill, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.Context().Err() != nil {
		return nil, errors.Join(ErrClosed, context.Cause(q.Context()))
	}
	if ctx.Err() != nil {
		return nil, context.Cause(ctx)
	}
	if q.config.SpillMode == SpillDisabled {
		return nil, resourceError("53200", "create query spill", ErrSpillDisabled)
	}
	if len(q.files) == q.config.MaxFiles {
		return nil, resourceError("53200", "admit spill file", ErrBudget)
	}
	claim, err := q.spill.reserve(0)
	if err != nil {
		return nil, err
	}
	file, err := q.fs.CreateTemp(q.config.Directory, "supersql-query-*")
	if err != nil {
		claim.Release()
		return nil, diskError("create query spill", err)
	}
	spill := &Spill{query: q, file: file, claim: claim, done: make(chan struct{})}
	q.files[spill] = struct{}{}
	q.peakFiles = max(q.peakFiles, len(q.files))
	if err := errors.Join(context.Cause(ctx), context.Cause(q.Context())); err != nil {
		q.mu.Unlock()
		closeErr := spill.Close()
		q.mu.Lock()
		return nil, errors.Join(err, closeErr)
	}
	return spill, nil
}

func (s *Spill) check(ctx context.Context) error {
	if s.cleaned || s.ownerClosed {
		return ErrClosed
	}
	if s.fenced != nil {
		return s.fenced
	}
	return s.checkContext(ctx)
}

func (s *Spill) checkContext(ctx context.Context) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	return context.Cause(s.query.Context())
}

func (s *Spill) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// Append is atomic with respect to readers. It charges the complete requested
// extent before writing and truncates back on failure, including cancellation.
// Failed restoration fences all further I/O and conservatively retains charge.
func (s *Spill) Append(ctx context.Context, data []byte) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	offset := s.size
	if int64(len(data)) > math.MaxInt64-offset {
		return 0, resourceError("53200", "grow spill extent", ErrBudget)
	}
	if len(data) == 0 {
		return offset, nil
	}
	if err := s.claim.Resize(offset + int64(len(data))); err != nil {
		return 0, err
	}
	written := 0
	for written < len(data) {
		if err := s.checkContext(ctx); err != nil {
			return 0, s.restore(offset, err)
		}
		end := written + min(ioChunkBytes, len(data)-written)
		n, err := s.file.WriteAt(data[written:end], offset+int64(written))
		if err == nil && n != end-written {
			err = io.ErrShortWrite
		}
		if err != nil {
			return 0, s.restore(offset, diskError("append query spill", err))
		}
		written = end
	}
	if err := s.checkContext(ctx); err != nil {
		return 0, s.restore(offset, err)
	}
	s.size += int64(len(data))
	return offset, nil
}

func (s *Spill) restore(size int64, cause error) error {
	if err := s.file.Truncate(size); err != nil {
		stat, statErr := s.file.Stat()
		if statErr == nil {
			s.size = stat.Size()
		}
		s.fenced = errors.Join(ErrFenced, cause, diskError("restore query spill extent", err), diskError("inspect failed spill extent", statErr))
		return s.fenced
	}
	s.size = size
	return errors.Join(cause, s.claim.Resize(size))
}

// Truncate only shrinks; readers forbid shrinkage so their captured extents
// remain immutable. Append is the sole growth path and cannot create holes.
func (s *Spill) Truncate(ctx context.Context, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return err
	}
	if size < 0 || size > s.size {
		return ErrInvalidConfig
	}
	if s.readers != 0 {
		return ErrReadersActive
	}
	if size == s.size {
		return nil
	}
	if err := s.file.Truncate(size); err != nil {
		// Unlike append rollback, lost bytes cannot be reconstructed.
		s.fenced = errors.Join(ErrFenced, diskError("truncate query spill", err))
		if stat, statErr := s.file.Stat(); statErr == nil {
			s.size = stat.Size()
		} else {
			s.fenced = errors.Join(s.fenced, diskError("inspect failed spill extent", statErr))
		}
		return s.fenced
	}
	s.size = size
	return s.claim.Resize(size)
}

func (s *Spill) ReadAt(ctx context.Context, data []byte, offset int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	return s.readAt(ctx, data, offset, s.size)
}

func (s *Spill) readAt(ctx context.Context, data []byte, offset, limit int64) (int, error) {
	if err := s.checkContext(ctx); err != nil {
		return 0, err
	}
	if offset < 0 {
		return 0, ErrInvalidConfig
	}
	if len(data) == 0 {
		return 0, nil
	}
	if offset >= limit {
		return 0, io.EOF
	}
	length := int(min(int64(len(data)), limit-offset))
	read := 0
	for read < length {
		if err := s.checkContext(ctx); err != nil {
			return read, err
		}
		chunk := min(ioChunkBytes, length-read)
		end := read + chunk
		n, err := s.file.ReadAt(data[read:end], offset+int64(read))
		if n < 0 || n > end-read {
			return read, diskError("read query spill", io.ErrUnexpectedEOF)
		}
		read += n
		if err != nil {
			if errors.Is(err, io.EOF) {
				return read, err
			}
			return read, diskError("read query spill", err)
		}
		if n != chunk {
			return read, diskError("read query spill", io.ErrUnexpectedEOF)
		}
	}
	if err := s.checkContext(ctx); err != nil {
		return read, err
	}
	if read != len(data) {
		return read, io.EOF
	}
	return read, nil
}

// Reader holds one immutable prefix and no additional OS file descriptor.
// Its lifetime is independently admitted against the query reader limit.
type Reader struct {
	spill  *Spill
	limit  int64
	closed bool
}

func (s *Spill) Reader(ctx context.Context) (*Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return nil, err
	}
	q := s.query
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrClosed
	}
	if q.readers == q.config.MaxReaders {
		return nil, resourceError("53200", "admit spill reader", ErrBudget)
	}
	s.readers++
	q.readers++
	q.peakReaders = max(q.peakReaders, q.readers)
	return &Reader{spill: s, limit: s.size}, nil
}

func (r *Reader) ReadAt(ctx context.Context, data []byte, offset int64) (int, error) {
	s := r.spill
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.closed || s.cleaned {
		return 0, ErrClosed
	}
	if s.fenced != nil {
		return 0, s.fenced
	}
	return s.readAt(ctx, data, offset, r.limit)
}

func (r *Reader) Close() error {
	s := r.spill
	s.mu.Lock()
	defer s.mu.Unlock()
	if !r.closed {
		r.closed = true
		s.readers--
		s.query.mu.Lock()
		s.query.readers--
		s.query.mu.Unlock()
	}
	if s.ownerClosed && s.readers == 0 {
		s.cleanup()
	}
	return s.cleanupErr
}

func (s *Spill) cleanup() {
	if s.cleaned {
		return
	}
	s.cleaned = true
	closeErr := s.file.Close()
	removeErr := s.query.fs.Remove(s.file.Name())
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	// Reclaim only when both the directory entry and open descriptor are gone.
	if closeErr == nil && removeErr == nil {
		s.claim.Release()
	}
	s.cleanupErr = errors.Join(s.fenced, diskError("close query spill", closeErr), diskError("remove query spill", removeErr))
	s.query.mu.Lock()
	delete(s.query.files, s)
	if s.cleanupErr != nil && s.query.firstCleanup == nil {
		s.query.firstCleanup = s.cleanupErr
	}
	s.query.mu.Unlock()
	close(s.done)
}

func (s *Spill) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerClosed = true
	if s.readers == 0 {
		s.cleanup()
	}
	return s.cleanupErr
}

func (s *Spill) WaitClosed() error {
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupErr
}
