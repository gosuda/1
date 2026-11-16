package txn

import (
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const mutationLogHeaderBytes = 20

// Reserve the canonical encoder's fixed workspace and bounded file-handle metadata.
const mutationLogWorkspaceBytes = 8192

var (
	errMutationLogBudget    = &model.Error{Code: "53200", Message: "transaction mutation log resource limit exceeded"}
	errMutationLogDirectory = &model.Error{Code: "22023", Message: "transaction spill directory must be explicit"}
	errMutationLogSealed    = errors.New("transaction mutation log is sealed")
	errMutationLogPosition  = errors.New("invalid transaction mutation log savepoint")
)

type mutationLogLimits struct {
	MemoryBytes, SpillBytes, CursorBytes uint64
	MaxCursors                           uint64
	SpillDirectory                       string
	DisableSpill                         bool
}

type mutationLogFile interface {
	io.ReaderAt
	io.WriterAt
	Truncate(int64) error
	Close() error
}

type mutationLog struct {
	mu                                                       sync.Mutex
	limits                                                   mutationLogLimits
	memoryOverhead                                           uint64
	buffer                                                   []byte
	file                                                     mutationLogFile
	path                                                     string
	count, bytes, ordinal, lastFrame, lastSerial, nextSerial uint64
	cursors                                                  uint64
	sealed, closed                                           bool
	failure, cleanupError                                    error
}

// The terminal frame serial distinguishes a retained prefix from a discarded
// branch even when replacement rows have identical sizes and ordinals.
type mutationLogPosition struct {
	owner                                    *mutationLog
	count, bytes, ordinal, lastFrame, serial uint64
}

func newMutationLog(limits mutationLogLimits) (*mutationLog, error) {
	if !limits.DisableSpill && limits.SpillDirectory == "" {
		return nil, errMutationLogDirectory
	}
	overhead := uint64(unsafe.Sizeof(mutationLog{})) + uint64(unsafe.Sizeof(mutationLogWriter{})) + mutationLogWorkspaceBytes
	// The owned directory, temporary filename and OS file name may coexist.
	overhead += 3 * (uint64(len(limits.SpillDirectory)) + 64)
	if limits.MemoryBytes < overhead || limits.CursorBytes < mutationLogCursorOverhead() {
		return nil, errMutationLogBudget
	}
	if limits.MaxCursors == 0 || limits.MaxCursors > math.MaxUint64/limits.CursorBytes {
		return nil, errMutationLogBudget
	}
	limits.SpillDirectory = strings.Clone(limits.SpillDirectory)
	return &mutationLog{limits: limits, memoryOverhead: overhead}, nil
}

func (l *mutationLog) mutable() error {
	if l.closed {
		return storage.ErrClosed
	}
	if l.failure != nil {
		return l.failure
	}
	if l.sealed {
		return errMutationLogSealed
	}
	return nil
}

func (l *mutationLog) position() mutationLogPosition {
	return mutationLogPosition{owner: l, count: l.count, bytes: l.bytes, ordinal: l.ordinal, lastFrame: l.lastFrame, serial: l.lastSerial}
}

func (l *mutationLog) savepoint() (mutationLogPosition, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.mutable(); err != nil {
		return mutationLogPosition{}, err
	}
	return l.position(), nil
}

func (l *mutationLog) mutationSize(m storage.Mutation) (uint64, error) {
	// ValidateShape may sort a temporary column-ID copy; charge its worst case
	// before asking the canonical codec to validate or encode the row.
	if len(m.ColumnIDs) > 64 && uint64(len(m.ColumnIDs)) > (l.limits.MemoryBytes-l.memoryOverhead-uint64(cap(l.buffer)))/4 {
		return 0, errMutationLogBudget
	}
	size, err := storage.EncodedMutationSize(m)
	if err != nil {
		return 0, err
	}
	if size > math.MaxInt-mutationLogHeaderBytes || size > (l.limits.CursorBytes-mutationLogCursorOverhead())/2 {
		return 0, errMutationLogBudget
	}
	return size, nil
}

func (l *mutationLog) append(ctx context.Context, mutations []storage.Mutation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.mutable(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if uint64(len(mutations)) > math.MaxUint64-l.nextSerial || uint64(len(mutations)) > math.MaxUint64-l.count {
		return errMutationLogBudget
	}
	total, ordinal := l.bytes, l.ordinal
	var scratch uint64
	for _, mutation := range mutations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(mutation.ColumnIDs) > 64 {
			if uint64(len(mutation.ColumnIDs)) > (l.limits.MemoryBytes-l.memoryOverhead)/4 {
				return errMutationLogBudget
			}
			scratch = max(scratch, uint64(len(mutation.ColumnIDs))*4)
		}
	}
	if err := l.reserve(ctx, l.bytes, scratch); err != nil {
		return err
	}
	for _, mutation := range mutations {
		if err := ctx.Err(); err != nil {
			return err
		}
		if mutation.Ordinal <= ordinal || !normalizedLogOperation(mutation.Op) {
			return storage.ErrUnnormalizedBatch
		}
		size, err := l.mutationSize(mutation)
		if err != nil {
			return err
		}
		frameBytes := size + mutationLogHeaderBytes
		if frameBytes > math.MaxInt64-total {
			return errMutationLogBudget
		}
		total += frameBytes
		ordinal = mutation.Ordinal
	}
	if len(mutations) == 0 {
		return ctx.Err()
	}
	if err := l.reserve(ctx, total, scratch); err != nil {
		return err
	}
	prefix := l.position()
	for _, mutation := range mutations {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, l.restore(prefix))
		}
		size, err := l.mutationSize(mutation)
		if err == nil {
			err = l.writeFrame(ctx, mutation, size)
		}
		if err != nil {
			return errors.Join(err, l.restore(prefix))
		}
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, l.restore(prefix))
	}
	return nil
}

func (l *mutationLog) reserve(ctx context.Context, total, scratch uint64) error {
	if l.file != nil {
		if total > l.limits.SpillBytes {
			return errMutationLogBudget
		}
		return nil
	}
	available := l.limits.MemoryBytes - l.memoryOverhead - scratch
	if total <= uint64(cap(l.buffer)) && uint64(cap(l.buffer)) <= available {
		return nil
	}
	// Both arrays are live during growth; never hide the old capacity from the budget.
	if uint64(cap(l.buffer)) <= available && total <= available-uint64(cap(l.buffer)) && total <= math.MaxInt {
		capacity := min(max(total, 2*uint64(cap(l.buffer))), available-uint64(cap(l.buffer)), uint64(math.MaxInt))
		buffer := make([]byte, len(l.buffer), int(capacity))
		copy(buffer, l.buffer)
		l.buffer = buffer
		return nil
	}
	if l.limits.DisableSpill || total > l.limits.SpillBytes {
		return errMutationLogBudget
	}
	file, err := os.CreateTemp(l.limits.SpillDirectory, "supersql-txn-*")
	if err != nil {
		return err
	}
	writer := mutationLogWriter{ctx: ctx, file: file, remaining: uint64(len(l.buffer))}
	_, err = writer.Write(l.buffer)
	if err != nil {
		return errors.Join(err, file.Close(), os.Remove(file.Name()))
	}
	l.file, l.path = file, file.Name()
	l.buffer = nil
	return nil
}

// Frames contain little-endian payload length, CRC32 and a serial. The CRC
// covers length, serial and canonical payload, excluding only the CRC field.
func (l *mutationLog) writeFrame(ctx context.Context, mutation storage.Mutation, size uint64) error {
	start := l.bytes
	l.nextSerial++
	var header [mutationLogHeaderBytes]byte
	binary.LittleEndian.PutUint64(header[:8], size)
	binary.LittleEndian.PutUint64(header[12:], l.nextSerial)
	writer := mutationLogWriter{ctx: ctx, file: l.file, offset: start + mutationLogHeaderBytes, remaining: size}
	writer.checksum = crc32.Update(crc32.ChecksumIEEE(header[:8]), crc32.IEEETable, header[12:])
	if l.file == nil {
		l.buffer = l.buffer[:start+mutationLogHeaderBytes+size]
		writer.buffer = l.buffer[start+mutationLogHeaderBytes:]
	}
	if err := storage.EncodeMutation(&writer, mutation); err != nil {
		return err
	}
	if writer.remaining != 0 {
		return storage.ErrInvalidEncoding
	}
	binary.LittleEndian.PutUint32(header[8:12], writer.checksum)
	if l.file != nil {
		n, err := l.file.WriteAt(header[:], int64(start))
		if err != nil {
			return err
		}
		if n != len(header) {
			return io.ErrShortWrite
		}
	} else {
		copy(l.buffer[start:], header[:])
	}
	l.count++
	l.bytes = start + mutationLogHeaderBytes + size
	l.ordinal, l.lastFrame, l.lastSerial = mutation.Ordinal, start, l.nextSerial
	return nil
}

type mutationLogWriter struct {
	ctx               context.Context
	file              mutationLogFile
	buffer            []byte
	offset, remaining uint64
	checksum          uint32
}

func (w *mutationLogWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if uint64(len(data)) > w.remaining {
		return 0, storage.ErrInvalidEncoding
	}
	var n int
	var err error
	if w.file != nil {
		n, err = w.file.WriteAt(data, int64(w.offset))
	} else {
		n = copy(w.buffer, data)
		w.buffer = w.buffer[n:]
	}
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	w.offset += uint64(n)
	w.remaining -= uint64(n)
	w.checksum = crc32.Update(w.checksum, crc32.IEEETable, data[:n])
	return n, err
}

func (l *mutationLog) rollback(ctx context.Context, position mutationLogPosition) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.mutable(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if position.owner != l || position.count > l.count || position.bytes > l.bytes {
		return errMutationLogPosition
	}
	if position.count != 0 {
		if position.lastFrame > position.bytes || position.bytes-position.lastFrame < mutationLogHeaderBytes {
			return errMutationLogPosition
		}
		var header [mutationLogHeaderBytes]byte
		if err := l.readAt(header[:], position.lastFrame); err != nil {
			l.failure = err
			return err
		}
		if binary.LittleEndian.Uint64(header[12:]) != position.serial || binary.LittleEndian.Uint64(header[:8]) != position.bytes-position.lastFrame-mutationLogHeaderBytes {
			return errMutationLogPosition
		}
	} else if position.bytes != 0 || position.ordinal != 0 || position.serial != 0 {
		return errMutationLogPosition
	}
	return l.restore(position)
}

func (l *mutationLog) restore(position mutationLogPosition) error {
	if l.file != nil {
		if err := l.file.Truncate(int64(position.bytes)); err != nil {
			l.failure = err
			return err
		}
	} else {
		clear(l.buffer[position.bytes:])
		l.buffer = l.buffer[:position.bytes]
		if position.bytes == 0 {
			l.buffer = nil
		}
	}
	l.count, l.bytes, l.ordinal = position.count, position.bytes, position.ordinal
	l.lastFrame, l.lastSerial = position.lastFrame, position.serial
	return nil
}

func (l *mutationLog) seal(ctx context.Context) (storage.MutationSequence, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, storage.ErrClosed
	}
	if l.failure != nil {
		return nil, l.failure
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.sealed = true
	return l, nil
}

func (l *mutationLog) Count() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.count
}

func (l *mutationLog) Open(ctx context.Context) (storage.MutationCursor, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, storage.ErrClosed
	}
	if l.failure != nil {
		return nil, l.failure
	}
	if !l.sealed {
		return nil, errMutationLogSealed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.cursors >= l.limits.MaxCursors {
		return nil, errMutationLogBudget
	}
	l.cursors++
	return &mutationLogCursor{source: l, remaining: l.count, budget: l.limits.CursorBytes}, nil
}

func (l *mutationLog) readAt(data []byte, offset uint64) error {
	if l.file == nil {
		if offset > uint64(len(l.buffer)) || uint64(len(data)) > uint64(len(l.buffer))-offset {
			return io.ErrUnexpectedEOF
		}
		copy(data, l.buffer[offset:offset+uint64(len(data))])
		return nil
	}
	n, err := l.file.ReadAt(data, int64(offset))
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	if err == nil && n != len(data) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func (l *mutationLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.closed = true
	if l.cursors == 0 {
		l.cleanup()
	}
	return l.cleanupError
}

func (l *mutationLog) cleanup() {
	l.buffer = nil
	l.limits.SpillDirectory = ""
	if l.file != nil {
		l.cleanupError = errors.Join(l.cleanupError, l.file.Close(), os.Remove(l.path))
		l.file, l.path = nil, ""
	}
}

type mutationLogCursor struct {
	mu                                         sync.Mutex
	source                                     *mutationLog
	buffer                                     []byte
	header                                     [mutationLogHeaderBytes]byte
	offset, remaining, ordinal, serial, budget uint64
	failure, closeError                        error
	closed                                     bool
}

func mutationLogCursorOverhead() uint64 {
	return uint64(unsafe.Sizeof(mutationLogCursor{}))
}

func (c *mutationLogCursor) Next(ctx context.Context) (storage.Mutation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return storage.Mutation{}, storage.ErrClosed
	}
	if c.failure != nil {
		return storage.Mutation{}, c.failure
	}
	if err := ctx.Err(); err != nil {
		return storage.Mutation{}, err
	}
	mutation, err := c.next(ctx)
	if err != nil && err != io.EOF {
		c.failure = err
	}
	return mutation, err
}

func (c *mutationLogCursor) next(ctx context.Context) (storage.Mutation, error) {
	if c.remaining == 0 {
		if c.offset != c.source.bytes {
			return storage.Mutation{}, storage.ErrInvalidEncoding
		}
		return storage.Mutation{}, io.EOF
	}
	if err := c.source.readAt(c.header[:], c.offset); err != nil {
		return storage.Mutation{}, err
	}
	size := binary.LittleEndian.Uint64(c.header[:8])
	serial := binary.LittleEndian.Uint64(c.header[12:])
	if c.offset > c.source.bytes || c.source.bytes-c.offset < mutationLogHeaderBytes || size > c.source.bytes-c.offset-mutationLogHeaderBytes || serial <= c.serial {
		return storage.Mutation{}, storage.ErrInvalidEncoding
	}
	available := c.budget - mutationLogCursorOverhead()
	if size > math.MaxInt || size > available/2 {
		return storage.Mutation{}, errMutationLogBudget
	}
	if uint64(cap(c.buffer)) < size {
		c.buffer = nil
		c.buffer = make([]byte, int(size))
	} else {
		c.buffer = c.buffer[:size]
	}
	if err := c.source.readAt(c.buffer, c.offset+mutationLogHeaderBytes); err != nil {
		return storage.Mutation{}, err
	}
	checksum := crc32.Update(crc32.ChecksumIEEE(c.header[:8]), crc32.IEEETable, c.header[12:])
	if crc32.Update(checksum, crc32.IEEETable, c.buffer) != binary.LittleEndian.Uint32(c.header[8:12]) {
		return storage.Mutation{}, storage.ErrChecksumMismatch
	}
	// DecodeMutation charges its returned values. Reserve additional room for
	// ValidateShape's possible column-ID sorting scratch, bounded by frame size.
	retained := uint64(cap(c.buffer))
	if retained > available-size {
		return storage.Mutation{}, errMutationLogBudget
	}
	mutation, err := storage.DecodeMutation(c.buffer, available-retained-size)
	if errors.Is(err, storage.ErrDecodeBudget) {
		return storage.Mutation{}, errors.Join(errMutationLogBudget, err)
	}
	if err != nil {
		return storage.Mutation{}, err
	}
	if mutation.Ordinal <= c.ordinal || !normalizedLogOperation(mutation.Op) {
		return storage.Mutation{}, storage.ErrUnnormalizedBatch
	}
	if err := ctx.Err(); err != nil {
		return storage.Mutation{}, err
	}
	c.offset += mutationLogHeaderBytes + size
	c.remaining--
	c.ordinal, c.serial = mutation.Ordinal, serial
	return mutation, nil
}

func (c *mutationLogCursor) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return c.closeError
	}
	c.closed = true
	c.buffer = nil
	l := c.source
	c.source = nil
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cursors--
	if l.closed && l.cursors == 0 {
		l.cleanup()
		c.closeError = l.cleanupError
	}
	return c.closeError
}

func normalizedLogOperation(op storage.MutationOp) bool {
	return op == storage.MutationUpsert || op == storage.MutationDelete || op == storage.MutationDeleteRange
}
