package schema

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type IndexJobSchedulerOptions struct {
	Runner       IndexJobRunnerOptions
	MaxRunning   int
	MemoryBytes  uint64
	PollInterval time.Duration
}

var (
	errIndexSchedulerOptions    = &model.Error{Code: "22023", Message: "invalid index job scheduler options"}
	ErrIndexJobSchedulerStopped = &model.Error{Code: "55000", Message: "index job scheduler execution has stopped"}
	ErrIndexJobSchedulerRunning = &model.Error{Code: "55006", Message: "index job scheduler is already running"}
	ErrIndexJobUnsupported      = &model.Error{Code: "0A000", Message: "schema job kind is not supported by the index scheduler"}
	errIndexSchedulerJobRunning = &model.Error{Code: "55006", Message: "index job is still running locally"}
)

// IndexJobSchedulerError identifies the durable job whose execution or resource
// ownership failed. Resolve accepts ID even after the scheduler's Close fails.
type IndexJobSchedulerError struct {
	ID    JobID
	Cause error
}

func (err *IndexJobSchedulerError) Error() string {
	return fmt.Sprintf("index job %d: %v", err.ID, err.Cause)
}

func (err *IndexJobSchedulerError) Unwrap() error { return err.Cause }

type indexSchedulerJob struct {
	id              JobID
	table           model.ObjectID
	runner          *IndexJobRunner
	running         bool
	cancelRequested bool
	cancelErr       error
	retainedErr     error
}

// IndexJobScheduler borrows every collaborator in Runner, including an already
// restored protection registry. Only Run starts workers; it may be called once.
// MaxRunning bounds both workers and retained failed owners, not job history.
// MemoryBytes covers scheduler/slot storage; runners and journal scans retain
// their separately configured resource limits. No durable resume state is cached.
//
// Close stops admission and joins Run before closing runners. The database owner
// must also stop retention work, resolve failed ownership through Resolve, and
// only then close the registry and backend. Cancel is the only method that writes
// cancellation intent; stopping Run or Wait never cancels a durable job.
type IndexJobScheduler struct {
	options IndexJobSchedulerOptions
	life    context.Context
	stop    context.CancelFunc

	// gate serializes journal admission and slot ownership, never worker execution.
	gate    chan struct{}
	wake    chan struct{}
	slots   []*indexSchedulerJob
	workers sync.WaitGroup

	mu        sync.Mutex
	closed    bool
	started   bool
	stopping  bool
	runCancel context.CancelFunc
	runDone   chan struct{}
	runErr    error
}

// NewIndexJobScheduler allocates bounded bookkeeping without starting goroutines.
// ctx is the database execution lifetime, not the lifetime of a DDL caller.
func NewIndexJobScheduler(ctx context.Context, options IndexJobSchedulerOptions) (*IndexJobScheduler, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if options.MaxRunning <= 0 || options.PollInterval <= 0 || options.MemoryBytes == 0 {
		return nil, errIndexSchedulerOptions
	}
	base := uint64(unsafe.Sizeof(IndexJobScheduler{}))
	entry := uint64(unsafe.Sizeof(indexSchedulerJob{}) + unsafe.Sizeof((*indexSchedulerJob)(nil)) + unsafe.Sizeof(IndexJobRunner{}))
	if base > options.MemoryBytes || uint64(options.MaxRunning) > (options.MemoryBytes-base)/(2*entry) {
		return nil, ErrJobLimit
	}
	if err := options.Runner.validate(); err != nil {
		return nil, err
	}
	if _, _, err := options.Runner.Protections.Oldest(); err != nil {
		return nil, err
	}
	life, stop := context.WithCancel(ctx)
	return &IndexJobScheduler{
		options: options,
		life:    life,
		stop:    stop,
		gate:    make(chan struct{}, 1),
		wake:    make(chan struct{}, 1),
		slots:   make([]*indexSchedulerJob, 0, options.MaxRunning),
	}, nil
}

// Run rescans the durable journal immediately and after each poll or completion.
// Infrastructure, unsupported-job and cleanup failures stop admission and join
// workers. Durable failed/canceled jobs remain observable through Status/Wait and
// do not stop unrelated jobs. Unknown outcomes retain their concrete runner.
func (s *IndexJobScheduler) Run(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	if err := s.executionError(); err != nil {
		s.mu.Unlock()
		return err
	}
	if s.started {
		s.mu.Unlock()
		return ErrIndexJobSchedulerRunning
	}
	operation, cancel := context.WithCancel(ctx)
	s.started = true
	s.runCancel = cancel
	s.runDone = make(chan struct{})
	s.mu.Unlock()
	stopLifetime := context.AfterFunc(s.life, cancel)

	err := s.poll(operation)
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	cancel()
	stopLifetime()
	s.workers.Wait()
	s.mu.Lock()
	err = errors.Join(err, s.runErr)
	s.runCancel = nil
	close(s.runDone)
	s.mu.Unlock()
	return err
}

func (s *IndexJobScheduler) poll(ctx context.Context) error {
	ticker := time.NewTicker(s.options.PollInterval)
	defer ticker.Stop()
	for {
		if err := s.scan(ctx); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		case <-s.wake:
		}
	}
}

func (s *IndexJobScheduler) scan(ctx context.Context) (err error) {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	if err := s.checkExecution(); err != nil {
		return err
	}
	if _, _, err := s.options.Runner.Protections.Oldest(); err != nil {
		return err
	}
	view, err := s.options.Runner.Manager.ClosedReadView(ctx)
	if errors.Is(err, txn.ErrNoClosedFrontier) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	return s.options.Runner.Jobs.Scan(ctx, view, func(job Job) error {
		if job.Terminal() {
			return nil
		}
		if err := s.checkExecution(); err != nil {
			return err
		}
		if len(s.slots) == s.options.MaxRunning || s.occupied(job.ID, job.TableID) {
			return nil
		}
		runner, err := NewIndexJobRunner(ctx, job.ID, s.options.Runner)
		if err != nil {
			return indexSchedulerError(job.ID, err)
		}
		slot := &indexSchedulerJob{id: job.ID, table: job.TableID, runner: runner, running: true}
		s.slots = append(s.slots, slot)
		s.workers.Add(1)
		go s.runJob(ctx, slot)
		return nil
	})
}

func (s *IndexJobScheduler) runJob(ctx context.Context, slot *indexSchedulerJob) {
	defer s.workers.Done()
	job, err := slot.runner.Run(ctx)
	// Completion must acquire ownership even after cancellation. Close never holds
	// this gate while joining workers, and Cancel does not wait for their wrappers.
	s.gate <- struct{}{}
	defer s.unlock()
	slot.running = false
	if indexSchedulerTerminal(err, job) {
		err = nil
	}
	if (ctx.Err() != nil || slot.cancelRequested) && indexSchedulerCancellation(err) {
		err = nil
	}
	err = errors.Join(err, slot.cancelErr)
	s.finish(slot, err)
	s.notify()
}

// finish requires gate and an idle runner. A failed Close preserves the concrete
// owner in its slot so neither a rescan nor a new Cancel can replace it.
func (s *IndexJobScheduler) finish(slot *indexSchedulerJob, operationErr error) error {
	closeErr := slot.runner.Close()
	err := errors.Join(operationErr, closeErr)
	if closeErr != nil || errors.Is(operationErr, storage.ErrCommitIndeterminate) || errors.Is(operationErr, errIndexRunnerProtectionPending) {
		slot.retainedErr = err
	} else {
		s.retire(slot)
	}
	if err != nil {
		s.fail(indexSchedulerError(slot.id, err))
	}
	return err
}

func (s *IndexJobScheduler) fail(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.stopping = true
	s.runErr = errors.Join(s.runErr, err)
	cancel := s.runCancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Close may block on in-flight storage calls. It never closes borrowed registry,
// manager, policy, or journal capabilities. Its error describes retained owners;
// the caller must also collect Run's execution error before closing the backend.
func (s *IndexJobScheduler) Close() error {
	s.mu.Lock()
	s.closed = true
	s.stopping = true
	done, cancel := s.runDone, s.runCancel
	s.mu.Unlock()
	s.stop()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	s.gate <- struct{}{}
	defer s.unlock()
	var err error
	for index := 0; index < len(s.slots); {
		slot := s.slots[index]
		closeErr := slot.runner.Close()
		if closeErr != nil {
			slot.retainedErr = closeErr
		}
		if slot.retainedErr != nil {
			err = errors.Join(err, indexSchedulerError(slot.id, slot.retainedErr))
			index++
			continue
		}
		s.retire(slot)
	}
	return err
}

func (s *IndexJobScheduler) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case s.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			s.unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *IndexJobScheduler) unlock() { <-s.gate }

func (s *IndexJobScheduler) checkExecution() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.executionError()
}

func (s *IndexJobScheduler) executionError() error {
	if s.closed {
		return storage.ErrClosed
	}
	if s.stopping {
		return ErrIndexJobSchedulerStopped
	}
	return s.life.Err()
}

func (s *IndexJobScheduler) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *IndexJobScheduler) occupied(id JobID, table model.ObjectID) bool {
	for _, slot := range s.slots {
		if slot.id == id || slot.table == table {
			return true
		}
	}
	return false
}

func (s *IndexJobScheduler) find(id JobID) *indexSchedulerJob {
	for _, slot := range s.slots {
		if slot.id == id {
			return slot
		}
	}
	return nil
}

func (s *IndexJobScheduler) retire(slot *indexSchedulerJob) {
	for index, candidate := range s.slots {
		if candidate == slot {
			copy(s.slots[index:], s.slots[index+1:])
			s.slots[len(s.slots)-1] = nil
			s.slots = s.slots[:len(s.slots)-1]
			return
		}
	}
}

func indexSchedulerError(id JobID, err error) error {
	if err == nil {
		return nil
	}
	return &IndexJobSchedulerError{ID: id, Cause: err}
}

// errors.Is alone would also accept cancellation joined with a cleanup failure.
func indexSchedulerCancellation(err error) bool {
	return allCauses(err, func(err error) bool { return err == context.Canceled || err == context.DeadlineExceeded })
}

func indexSchedulerTerminal(err error, job Job) bool {
	return allCauses(err, func(err error) bool {
		terminal, ok := err.(*indexJobTerminalError)
		return ok && terminal.id == job.ID && terminal.state == job.State && terminal.failure.Code == job.ErrorCode
	})
}
