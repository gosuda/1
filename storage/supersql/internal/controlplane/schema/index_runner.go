package schema

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type IndexJobTransactions interface {
	Begin(context.Context, txn.TransactionOptions) (*txn.Transaction, error)
	CurrentReadView(context.Context) (txn.ReadView, error)
	ClosedReadView(context.Context) (txn.ReadView, error)
	HistoricalSnapshot(context.Context, model.Timestamp) (storage.SnapshotReader, error)
}

type IndexJobRunnerOptions struct {
	Manager                   IndexJobTransactions
	Policies                  IndexPolicies
	Jobs                      *JobStore
	Lifecycle                 *IndexLifecycle
	Protections               *JobProtections
	Transaction               txn.TransactionOptions
	Scan                      BackfillScanLimits
	Backfill                  BackfillLimits
	Validation                IndexValidationLimits
	MaxRetries                int
	RetryDelay, MaxRetryDelay time.Duration
	Pace                      func(context.Context) error
}

var (
	errIndexRunnerOptions           = &model.Error{Code: "22023", Message: "invalid index job runner options"}
	errIndexRunnerCancelWaiting     = &model.Error{Code: "57014", Message: "index job cancellation is waiting", Cause: context.Canceled}
	errIndexRunnerProtectionPending = &model.Error{Code: "55000", Message: "index job protection ownership requires resolution"}
)

// A runner owns one job's execution, not its database-lifetime protection.
// Resolve remains available after Close to settle an indeterminate commit.
// The constructor context supplies the runner lifetime; Run's context only
// controls that invocation. Cancel is the only cancellation-intent operation.
type IndexJobRunner struct {
	id                        JobID
	options                   IndexJobRunnerOptions
	life                      context.Context
	stop                      context.CancelFunc
	gate                      chan struct{}
	mu                        sync.Mutex
	closed                    bool
	cancelWaiters             int
	activeRun                 context.CancelFunc
	last                      Job
	attempt, lease            uint64
	claimed                   bool
	pending                   *indexRunnerCommit
	failure                   *indexRunnerFailure
	cleanupErr                error
	scan                      *IndexKeyScan
	scanReader                storage.SnapshotReader
	scanJob                   Job
	scanTable, scanIndex      storage.ObjectRef
	keys                      [][]byte
	chunkReady, chunkComplete bool
}

type indexRunnerFailure struct {
	code string
	key  []byte
}

type indexRunnerOperation uint8

const (
	indexRunnerExecute indexRunnerOperation = iota
	indexRunnerCancel
	indexRunnerResolve
)

func (options IndexJobRunnerOptions) validate() error {
	missingRuntime := options.Manager == nil || options.Policies == nil || options.Lifecycle == nil
	missingStorage := options.Jobs == nil || options.Protections == nil
	if missingRuntime || missingStorage {
		return errIndexRunnerOptions
	}
	if options.Jobs.normalizer == nil {
		return errIndexRunnerOptions
	}
	invalidScan := options.Scan.BatchRows <= 0 || options.Scan.MaxKeyBytes == 0 || options.Scan.WorkBytes == 0
	if invalidScan || options.Scan.MaxKeyBytes > uint64(math.MaxInt)/16 || options.Scan.MaxKeyBytes > options.Jobs.options.KeyBytes {
		return errIndexRunnerOptions
	}
	invalidBackfill := options.Backfill.MaxRows == 0 || options.Backfill.MaxKeyBytes == 0 || options.Backfill.WorkBytes == 0
	invalidValidation := options.Validation.BatchRows <= 0 || options.Validation.MaxRows == 0 || options.Validation.MaxBytes == 0
	if invalidBackfill || invalidValidation || options.Backfill.WorkBytes > math.MaxInt {
		return errIndexRunnerOptions
	}
	invalidRetry := options.MaxRetries < 0 || options.RetryDelay <= 0 || options.MaxRetryDelay < options.RetryDelay
	invalidTransaction := options.Transaction.MaxViews < 0
	missingSpill := !options.Transaction.DisableSpill && options.Transaction.SpillDirectory == ""
	if invalidRetry || invalidTransaction || missingSpill {
		return errIndexRunnerOptions
	}
	if min(uint64(options.Scan.BatchRows), options.Backfill.MaxRows, options.Backfill.MaxKeyBytes/options.Scan.MaxKeyBytes) == 0 {
		return errIndexRunnerOptions
	}
	return nil
}

func NewIndexJobRunner(ctx context.Context, id JobID, options IndexJobRunnerOptions) (*IndexJobRunner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == 0 {
		return nil, errIndexRunnerOptions
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	rows := min(uint64(options.Scan.BatchRows), options.Backfill.MaxRows, options.Backfill.MaxKeyBytes/options.Scan.MaxKeyBytes)
	options.Scan.BatchRows = int(rows)
	if _, _, err := options.Protections.Oldest(); err != nil {
		return nil, err
	}
	life, stop := context.WithCancel(ctx)
	runner := &IndexJobRunner{id: id, options: options, life: life, stop: stop, gate: make(chan struct{}, 1)}
	job, err := runner.readJob(ctx)
	if err != nil {
		stop()
		return nil, err
	}
	runner.last = job
	runner.attempt = job.Attempt
	if !job.Terminal() {
		if job.Attempt == math.MaxUint64 {
			stop()
			return nil, ErrJobTransition
		}
		runner.lease = job.Attempt + 1
	}
	return runner, nil
}

func (r *IndexJobRunner) Run(ctx context.Context) (Job, error) {
	operation, finish, err := r.enter(ctx, indexRunnerExecute)
	if err != nil {
		return Job{}, err
	}
	defer finish()
	job, err := r.drive(operation, false)
	err = errors.Join(err, r.closeScan())
	return cloneIndexRunnerJob(job), err
}

// Cancel interrupts a local Run before serializing durable cancellation intent.
// A published index or an existing failure is never replaced by cancellation.
func (r *IndexJobRunner) Cancel(ctx context.Context) (Job, error) {
	if err := ctx.Err(); err != nil {
		return Job{}, err
	}
	r.mu.Lock()
	r.cancelWaiters++
	if r.activeRun != nil {
		r.activeRun()
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.cancelWaiters--
		r.mu.Unlock()
	}()
	operation, finish, err := r.enter(ctx, indexRunnerCancel)
	if err != nil {
		return Job{}, err
	}
	defer finish()
	job, err := r.drive(operation, true)
	err = errors.Join(err, r.closeScan())
	return cloneIndexRunnerJob(job), err
}

// Resolve settles only retained ownership and reloads the durable job. It does
// not rerun a chunk, publish a descriptor, or resume background execution.
func (r *IndexJobRunner) Resolve(ctx context.Context) (Job, error) {
	operation, finish, err := r.enter(ctx, indexRunnerResolve)
	if err != nil {
		return Job{}, err
	}
	defer finish()
	if err := r.resolvePending(operation); err != nil {
		return cloneIndexRunnerJob(r.last), errors.Join(err, r.cleanupErr)
	}
	job, err := r.readJob(operation)
	if err == nil {
		r.last = job
	}
	return cloneIndexRunnerJob(r.last), errors.Join(err, r.cleanupErr)
}

func (r *IndexJobRunner) Close() error {
	r.mu.Lock()
	r.closed = true
	r.stop()
	if r.activeRun != nil {
		r.activeRun()
	}
	r.mu.Unlock()
	r.gate <- struct{}{}
	defer func() { <-r.gate }()
	err := r.closeScan()
	if r.pending != nil {
		if r.pending.transaction.State() == txn.Indeterminate {
			return errors.Join(err, r.cleanupErr, storage.ErrCommitIndeterminate)
		}
		err = errors.Join(err, r.finishPending(context.WithoutCancel(r.life)))
	}
	return errors.Join(err, r.cleanupErr)
}

func (r *IndexJobRunner) enter(ctx context.Context, mode indexRunnerOperation) (context.Context, func(), error) {
	run, resolving := mode == indexRunnerExecute, mode == indexRunnerResolve
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	operation, cancel := context.WithCancel(ctx)
	stopLifetime := func() bool { return false }
	if !resolving {
		stopLifetime = context.AfterFunc(r.life, cancel)
	}
	select {
	case r.gate <- struct{}{}:
	case <-operation.Done():
		stopLifetime()
		cancel()
		return nil, nil, operation.Err()
	}
	r.mu.Lock()
	var err error
	if r.closed && !resolving {
		err = storage.ErrClosed
	} else if run && r.cancelWaiters != 0 {
		err = errIndexRunnerCancelWaiting
	} else if err = operation.Err(); err == nil && run {
		r.activeRun = cancel
	}
	r.mu.Unlock()
	finish := func() {
		r.mu.Lock()
		if run {
			r.activeRun = nil
		}
		r.mu.Unlock()
		stopLifetime()
		cancel()
		<-r.gate
	}
	if err != nil {
		finish()
		return nil, nil, err
	}
	return operation, finish, nil
}

func (r *IndexJobRunner) readJob(ctx context.Context) (job Job, err error) {
	view, err := r.options.Manager.CurrentReadView(ctx)
	if err != nil {
		return Job{}, err
	}
	defer func() { err = errors.Join(err, r.cleanup(view.Close())) }()
	job, err = r.options.Jobs.Get(ctx, view, r.id)
	if err != nil {
		return Job{}, err
	}
	if job.Terminal() {
		err = r.options.Protections.Release(ctx, view, r.id)
	}
	return job, err
}

func (r *IndexJobRunner) cleanup(err error) error {
	if err != nil {
		r.cleanupErr = errors.Join(r.cleanupErr, err)
	}
	return err
}

func cloneIndexRunnerJob(job Job) Job {
	job.ResumeKey = bytes.Clone(job.ResumeKey)
	job.FailureKey = bytes.Clone(job.FailureKey)
	return job
}
