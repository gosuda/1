package rowstore

import (
	"context"
	"errors"
	"math"
	"sync"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrSealed    = errors.New("row spool is sealed")
	ErrNotSealed = errors.New("row spool is not sealed")
	ErrPosition  = errors.New("invalid row spool position")
	ErrCorrupt   = errors.New("corrupt row spool record")
)

// Position is a byte offset, valid only in the spool that returned it.
type Position uint64

// All limits are explicit. MemoryBytes may be zero to force spill; other limits
// must be positive. MaxBytes and MaxRecordBytes include frame headers.
// DecodeBytes bounds one decoded row, including nested headers and temporaries.
// MetadataBytes covers the owned schema, append row headers and active readers.
type Limits struct {
	MemoryBytes    uint64
	MaxBytes       uint64
	MaxRows        uint64
	MaxRecordBytes uint64
	MetadataBytes  uint64
	DecodeBytes    uint64
	MaxReaders     int
}

// Spool borrows its query until Close returns. Appends are serialized; readers
// have independent cursors. Close blocks until every admitted Reader closes.
type Spool struct {
	mu            sync.Mutex
	query         *runtime.Query
	limits        Limits
	layouts       []model.TypeLayout
	row           []model.Value
	metadata      uint64
	metadataClaim *runtime.Reservation
	appendClaim   *runtime.Reservation
	memoryClaim   *runtime.Reservation
	memory        []byte
	scratch       []byte
	spill         *runtime.Spill
	bytes         uint64
	rows          uint64
	readers       int
	sealed        bool
	closed        bool
	failure       error
	closeErr      error
	done          chan struct{}
}

func limitError(operation string) error {
	return &model.Error{Code: "53200", Message: operation, Cause: runtime.ErrBudget}
}

func New(ctx context.Context, query *runtime.Query, layouts []model.TypeLayout, limits Limits) (*Spool, error) {
	invalidExtent := limits.MaxRows == 0 || limits.MaxBytes == 0 || limits.MaxRecordBytes < frameHeaderBytes
	invalidWorkspace := limits.MetadataBytes == 0 || limits.DecodeBytes == 0 || limits.MaxReaders <= 0
	if query == nil || invalidExtent || invalidWorkspace {
		return nil, runtime.ErrInvalidConfig
	}
	for _, size := range [...]uint64{limits.MemoryBytes, limits.MaxBytes, limits.MaxRecordBytes, limits.MetadataBytes, limits.DecodeBytes} {
		if size > uint64(math.MaxInt) {
			return nil, runtime.ErrInvalidConfig
		}
	}
	if limits.MaxRecordBytes > uint64(math.MaxInt)-codecScratchBytes || limits.DecodeBytes > uint64(math.MaxInt)-limits.MaxRecordBytes-codecScratchBytes {
		return nil, runtime.ErrInvalidConfig
	}
	if err := errors.Join(context.Cause(ctx), context.Cause(query.Context())); err != nil {
		return nil, err
	}
	fixed := uint64(unsafe.Sizeof(Spool{})) + 256
	unit := uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(unsafe.Sizeof(model.Value{}))
	if fixed > limits.MetadataBytes || uint64(len(layouts)) > (limits.MetadataBytes-fixed)/unit || uint64(len(layouts)) > math.MaxUint32 {
		return nil, limitError("admit row spool schema")
	}
	claim, err := query.TryReserve(runtime.WorkMemory, int64(limits.MetadataBytes))
	if err != nil {
		return nil, err
	}
	s := &Spool{query: query, limits: limits, metadataClaim: claim, metadata: fixed + uint64(len(layouts))*unit, done: make(chan struct{})}
	s.layouts = make([]model.TypeLayout, len(layouts))
	for i := range layouts {
		layout, used, err := model.CopyTypeLayout(ctx, layouts[i], limits.MetadataBytes-s.metadata)
		if err != nil {
			claim.Release()
			return nil, err
		}
		s.layouts[i] = layout
		s.metadata += used
	}
	s.appendClaim, err = query.TryReserve(runtime.WorkMemory, int64(limits.MaxRecordBytes+limits.DecodeBytes+codecScratchBytes))
	if err != nil {
		claim.Release()
		return nil, err
	}
	s.row = make([]model.Value, len(layouts))
	s.scratch = make([]byte, int(limits.MaxRecordBytes))
	if err := s.check(ctx); err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return s, nil
}

func (s *Spool) check(ctx context.Context) error {
	if s.closed {
		return runtime.ErrClosed
	}
	if s.failure != nil {
		return s.failure
	}
	return errors.Join(context.Cause(ctx), context.Cause(s.query.Context()))
}

func (s *Spool) AppendRow(ctx context.Context, row []model.Value) (Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendRow(ctx, row)
}

func (s *Spool) appendRow(ctx context.Context, row []model.Value) (Position, error) {
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	if s.sealed {
		return 0, ErrSealed
	}
	if s.rows == s.limits.MaxRows {
		return 0, limitError("append row spool row")
	}
	frame, err := s.encode(ctx, row)
	if err != nil {
		return 0, err
	}
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	if uint64(len(frame)) > s.limits.MaxBytes-s.bytes {
		return 0, limitError("append row spool bytes")
	}
	position := Position(s.bytes)
	if s.spill == nil && s.bytes+uint64(len(frame)) <= s.limits.MemoryBytes {
		if s.memoryClaim == nil {
			claim, err := s.query.TryReserve(runtime.WorkMemory, int64(s.limits.MemoryBytes))
			if err == nil {
				s.memoryClaim = claim
				s.memory = make([]byte, 0, int(s.limits.MemoryBytes))
			} else if !errors.Is(err, runtime.ErrBudget) {
				return 0, err
			}
		}
		if s.memoryClaim != nil {
			s.memory = append(s.memory, frame...)
			s.bytes += uint64(len(frame))
			s.rows++
			return position, nil
		}
	}
	if s.spill == nil {
		if err := s.migrate(ctx); err != nil {
			return 0, err
		}
	}
	if _, err := s.spill.Append(ctx, frame); err != nil {
		if errors.Is(err, runtime.ErrFenced) {
			s.failure = err
		}
		return 0, err
	}
	s.bytes += uint64(len(frame))
	s.rows++
	return position, nil
}

func (s *Spool) migrate(ctx context.Context) error {
	spill, err := s.query.NewSpill(ctx)
	if err != nil {
		return err
	}
	if _, err := spill.Append(ctx, s.memory); err != nil {
		closeErr := spill.Close()
		return errors.Join(err, closeErr, spill.WaitClosed())
	}
	// Append success establishes an in-process owned copy, not database durability.
	s.spill = spill
	s.memory = nil
	if s.memoryClaim != nil {
		s.memoryClaim.Release()
		s.memoryClaim = nil
	}
	return nil
}

// AppendBatch returns the admitted prefix length on failure; it retains no
// position directory. Selection order determines append order.
func (s *Spool) AppendBatch(ctx context.Context, batch *columnar.Batch) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	if s.sealed {
		return 0, ErrSealed
	}
	if batch == nil || len(batch.Columns) != len(s.layouts) {
		return 0, columnar.ErrInvalidBatch
	}
	if err := batch.Validate(math.MaxInt); err != nil {
		return 0, err
	}
	for i := range batch.Columns {
		if !batch.Columns[i].MatchesLayout(s.layouts[i]) {
			return 0, columnar.ErrValueLayout
		}
	}
	defer clear(s.row)
	for row := range batch.Rows() {
		clear(s.row)
		remaining := s.limits.DecodeBytes
		for i := range batch.Columns {
			value, used, err := columnar.CopyValue(ctx, &batch.Columns[i], batch.Lane(row), remaining)
			if err != nil {
				return row, err
			}
			s.row[i] = value
			remaining -= used
		}
		if _, err := s.appendRow(ctx, s.row); err != nil {
			return row, err
		}
	}
	return batch.Rows(), nil
}

func (s *Spool) Seal(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return err
	}
	if !s.sealed {
		s.sealed = true
		s.scratch = nil
		s.row = nil
		s.appendClaim.Release()
	}
	return nil
}

func (s *Spool) Rows() uint64  { s.mu.Lock(); defer s.mu.Unlock(); return s.rows }
func (s *Spool) Bytes() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.bytes }
func (s *Spool) Spilled() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.spill != nil }

func (s *Spool) cleanup() {
	if s.spill != nil {
		s.closeErr = errors.Join(s.spill.Close(), s.spill.WaitClosed())
	}
	s.memory, s.scratch, s.row, s.layouts = nil, nil, nil, nil
	if s.memoryClaim != nil {
		s.memoryClaim.Release()
	}
	s.appendClaim.Release()
	s.metadataClaim.Release()
	close(s.done)
}

func (s *Spool) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		if s.readers == 0 {
			s.cleanup()
		}
	}
	s.mu.Unlock()
	<-s.done
	return s.closeErr
}
