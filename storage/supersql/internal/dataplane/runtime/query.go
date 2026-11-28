package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
)

const (
	DefaultWorkBytes   int64 = 64 << 20
	DefaultSpillBytes  int64 = 1 << 30
	DefaultOutputBytes int64 = 64 << 20
)

type SpillMode uint8

const (
	SpillEnabled SpillMode = iota
	SpillDisabled
)

type QueryConfig struct {
	// Zero byte/count limits select defaults. Negative values are invalid.
	WorkBytes   int64
	OutputBytes int64
	SpillBytes  int64
	SpillMode   SpillMode
	// Directory is mandatory when spill is enabled; it is never created here.
	Directory       string
	MaxFiles        int
	MaxReaders      int
	MaxReservations int
}

type MemoryKind uint8

const (
	WorkMemory MemoryKind = iota
	OutputMemory
)

type QueryUsage struct {
	Work        Usage
	Output      Usage
	Spill       Usage
	Files       int
	PeakFiles   int
	Readers     int
	PeakReaders int
}

// Query owns its group, buffers, reservations and spill files, but borrows the
// database pool, shared memory budget and filesystem. External readers must
// close before Close can finish; cancellation never unlinks beneath a reader.
// Direct callers must stop accessing borrowed buffers before calling Close.
type Query struct {
	group        *Group
	work         *Budget
	output       *Budget
	spill        *Budget
	config       QueryConfig
	fs           FileSystem
	mu           sync.Mutex
	closed       bool
	files        map[*Spill]struct{}
	buffers      map[*Buffer]struct{}
	readers      int
	peakReaders  int
	peakFiles    int
	firstCleanup error
	stopCancel   func() bool
	closeOnce    sync.Once
	closeErr     error
}

func NewQuery(ctx context.Context, pool *Pool, sharedMemory *Budget, fs FileSystem, config QueryConfig) (*Query, error) {
	negativeBytes := config.WorkBytes < 0 || config.OutputBytes < 0 || config.SpillBytes < 0
	negativeCounts := config.MaxFiles < 0 || config.MaxReaders < 0 || config.MaxReservations < 0
	if pool == nil || sharedMemory == nil || negativeBytes || negativeCounts || config.SpillMode > SpillDisabled {
		return nil, ErrInvalidConfig
	}
	if config.WorkBytes == 0 {
		config.WorkBytes = DefaultWorkBytes
	}
	if config.OutputBytes == 0 {
		config.OutputBytes = DefaultOutputBytes
	}
	if config.SpillBytes == 0 {
		config.SpillBytes = DefaultSpillBytes
	}
	if config.MaxFiles == 0 {
		config.MaxFiles = 64
	}
	if config.MaxReaders == 0 {
		config.MaxReaders = 128
	}
	if config.MaxReservations == 0 {
		config.MaxReservations = 4096
	}
	if config.SpillMode == SpillEnabled {
		if fs == nil || config.Directory == "" {
			return nil, fmt.Errorf("%w: spill filesystem and directory required", ErrInvalidConfig)
		}
		dir, err := filepath.Abs(config.Directory)
		if err != nil {
			return nil, fmt.Errorf("resolve spill directory: %w", err)
		}
		config.Directory = dir
	}
	group, err := pool.NewGroup(ctx)
	if err != nil {
		return nil, err
	}
	spill, err := NewBudget(config.SpillBytes, config.MaxFiles)
	if err != nil {
		return nil, errors.Join(err, group.Close())
	}
	q := &Query{group: group, work: sharedMemory.child(config.WorkBytes, config.MaxReservations), output: sharedMemory.child(config.OutputBytes, config.MaxReservations), spill: spill, config: config, fs: fs, files: make(map[*Spill]struct{}), buffers: make(map[*Buffer]struct{})}
	// Publish the callback under the same lock it takes through Close, including
	// when the parent was canceled during construction.
	q.mu.Lock()
	q.stopCancel = context.AfterFunc(group.Context(), func() { _ = q.Close() })
	q.mu.Unlock()
	return q, nil
}

func (q *Query) Context() context.Context { return q.group.Context() }
func (q *Query) Tasks() *Group            { return q.group }

// Parallelism is the database pool's configured worker count, fixed at creation.
func (q *Query) Parallelism() int { return q.group.pool.parallelism }

func (q *Query) memory(kind MemoryKind) (*Budget, error) {
	switch kind {
	case WorkMemory:
		return q.work, nil
	case OutputMemory:
		return q.output, nil
	default:
		return nil, ErrInvalidConfig
	}
}

// TryReserve debits both the named query account and the shared database cap.
// Caller allocations must fit this claim, including retained slice capacity.
func (q *Query) TryReserve(kind MemoryKind, bytes int64) (*Reservation, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.Context().Err() != nil {
		return nil, errors.Join(ErrClosed, context.Cause(q.Context()))
	}
	budget, err := q.memory(kind)
	if err != nil {
		return nil, err
	}
	return budget.TryReserve(bytes)
}

// Buffer has fixed capacity; Bytes is borrowed until Buffer.Close or Query.Close.
// It must not be appended beyond capacity or retained after that lifetime.
type Buffer struct {
	query *Query
	claim *Reservation
	data  []byte
}

func (q *Query) NewBuffer(kind MemoryKind, bytes int64) (*Buffer, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || q.Context().Err() != nil {
		return nil, errors.Join(ErrClosed, context.Cause(q.Context()))
	}
	if bytes <= 0 || uint64(bytes) > uint64(^uint(0)>>1) {
		return nil, ErrInvalidConfig
	}
	budget, err := q.memory(kind)
	if err != nil {
		return nil, err
	}
	claim, err := budget.TryReserve(bytes)
	if err != nil {
		return nil, err
	}
	buffer := &Buffer{query: q, claim: claim, data: make([]byte, int(bytes))}
	q.buffers[buffer] = struct{}{}
	return buffer, nil
}

func (b *Buffer) Bytes() []byte {
	b.query.mu.Lock()
	defer b.query.mu.Unlock()
	return b.data
}

func (b *Buffer) Close() error {
	b.query.mu.Lock()
	defer b.query.mu.Unlock()
	b.data = nil
	b.claim.Release()
	delete(b.query.buffers, b)
	return nil
}

func (q *Query) Usage() QueryUsage {
	q.mu.Lock()
	defer q.mu.Unlock()
	return QueryUsage{Work: q.work.Usage(), Output: q.output.Usage(), Spill: q.spill.Usage(), Files: len(q.files), PeakFiles: q.peakFiles, Readers: q.readers, PeakReaders: q.peakReaders}
}

func (q *Query) Close() error {
	q.closeOnce.Do(func() {
		q.mu.Lock()
		q.closed = true
		q.stopCancel()
		q.mu.Unlock()
		q.closeErr = q.group.Close()
		q.mu.Lock()
		files := make([]*Spill, 0, len(q.files))
		for file := range q.files {
			files = append(files, file)
		}
		q.mu.Unlock()
		for _, file := range files {
			_ = file.Close()
		}
		for _, file := range files {
			q.closeErr = errors.Join(q.closeErr, file.WaitClosed())
		}
		q.mu.Lock()
		q.closeErr = errors.Join(q.closeErr, q.firstCleanup)
		for buffer := range q.buffers {
			buffer.data = nil
			buffer.claim.Release()
		}
		q.buffers = nil
		q.mu.Unlock()
		q.work.close()
		q.output.close()
		// Failed unlink retains its disk charge: the file may still occupy space.
	})
	return q.closeErr
}
