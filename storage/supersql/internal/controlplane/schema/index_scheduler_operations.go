package schema

import (
	"context"
	"errors"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

// Status reads the current committed journal, including jobs this scheduler has
// never admitted. It remains usable after Run stops, until Close or lifetime expiry.
func (s *IndexJobScheduler) Status(ctx context.Context, id JobID) (Job, error) {
	operation, finish := s.operation(ctx)
	defer finish()
	if err := s.lock(operation); err != nil {
		return Job{}, err
	}
	defer s.unlock()
	if err := s.checkOpen(); err != nil {
		return Job{}, err
	}
	return s.journalJob(operation, id)
}

// Wait observes durable completion without retaining a waiter or terminal-job
// cache. Failed/canceled jobs return their durable SQLSTATE along with the Job.
// Canceling ctx only stops this wait, including when Run is executing the job.
func (s *IndexJobScheduler) Wait(ctx context.Context, id JobID) (Job, error) {
	operation, finish := s.operation(ctx)
	defer finish()
	ticker := time.NewTicker(s.options.PollInterval)
	defer ticker.Stop()
	for {
		job, err := s.Status(operation, id)
		if err != nil {
			return job, err
		}
		if job.Terminal() {
			return job, indexRunnerTerminalError(job)
		}
		select {
		case <-operation.Done():
			return job, operation.Err()
		case <-ticker.C:
		}
	}
}

// Cancel serializes explicit durable cancellation through the job's concrete
// runner. A job without a local runner needs a free slot; capacity exhaustion does
// not enqueue it. Caller cancellation may interrupt cancellation processing, so
// the returned Job or a later Status determines what became durable.
func (s *IndexJobScheduler) Cancel(ctx context.Context, id JobID) (Job, error) {
	operation, finish := s.operation(ctx)
	defer finish()
	if err := s.lock(operation); err != nil {
		return Job{}, err
	}
	defer s.unlock()
	if err := s.checkExecution(); err != nil {
		return Job{}, err
	}
	if _, _, err := s.options.Runner.Protections.Oldest(); err != nil {
		return Job{}, err
	}
	if slot := s.find(id); slot != nil {
		if !slot.running {
			return Job{}, indexSchedulerError(id, slot.retainedErr)
		}
		slot.cancelRequested = true
		job, err := slot.runner.Cancel(operation)
		if !indexSchedulerCancellation(err) && !indexSchedulerTerminal(err, job) {
			slot.cancelErr = err
		}
		return job, err
	}
	job, err := s.journalJob(operation, id)
	if err != nil {
		return job, err
	}
	if job.Terminal() {
		return job, indexRunnerTerminalError(job)
	}
	if len(s.slots) == s.options.MaxRunning {
		return job, ErrJobLimit
	}
	if s.occupied(id, job.TableID) {
		return job, ErrJobConflict
	}
	runner, err := NewIndexJobRunner(operation, id, s.options.Runner)
	if err != nil {
		return job, indexSchedulerError(id, err)
	}
	slot := &indexSchedulerJob{id: id, table: job.TableID, runner: runner}
	s.slots = append(s.slots, slot)
	job, err = runner.Cancel(operation)
	failure := err
	if indexSchedulerCancellation(failure) || indexSchedulerTerminal(failure, job) {
		failure = nil
	}
	finishErr := s.finish(slot, failure)
	if failure != nil {
		return job, finishErr
	}
	return job, errors.Join(err, finishErr)
}

// Resolve settles a retained runner without resuming its state machine. It is
// available after failed Close and ignores the expired execution lifetime; ctx
// controls only resolution. No local owner means the result is a fresh journal
// read, not proof that an unknown commit owned elsewhere has been resolved.
func (s *IndexJobScheduler) Resolve(ctx context.Context, id JobID) (Job, error) {
	if err := s.lock(ctx); err != nil {
		return Job{}, err
	}
	defer s.unlock()
	slot := s.find(id)
	if slot == nil {
		return s.journalJob(ctx, id)
	}
	if slot.running {
		return Job{}, indexSchedulerError(id, errIndexSchedulerJobRunning)
	}
	job, err := slot.runner.Resolve(ctx)
	err = errors.Join(err, slot.runner.Close())
	if err != nil {
		slot.retainedErr = err
		return job, indexSchedulerError(id, err)
	}
	s.retire(slot)
	return job, nil
}

func (s *IndexJobScheduler) journalJob(ctx context.Context, id JobID) (job Job, err error) {
	if id == 0 {
		return Job{}, ErrJobRecord
	}
	view, err := s.options.Runner.Manager.CurrentReadView(ctx)
	if err != nil {
		return Job{}, err
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	return s.options.Runner.Jobs.Get(ctx, view, id)
}

func (s *IndexJobScheduler) operation(ctx context.Context) (context.Context, func()) {
	operation, cancel := context.WithCancel(ctx)
	stopLifetime := context.AfterFunc(s.life, cancel)
	return operation, func() {
		stopLifetime()
		cancel()
	}
}

func (s *IndexJobScheduler) checkOpen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return storage.ErrClosed
	}
	return s.life.Err()
}
