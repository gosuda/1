package schema

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestJobStoreLifetimeJobsAndPruningWithSmallMaxJobs(t *testing.T) {
	f := newIndexRestartFixture(t)
	smallStore, err := NewJobStore(t.Context(), JobStoreOptions{
		Target:        storage.ObjectRef{ObjectID: 44, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1},
		Encoder:       f.keys,
		MetadataBytes: 1 << 20,
		RecordBytes:   64 << 10,
		KeyBytes:      4096,
		MaxJobs:       8,
		BatchRows:     4,
	})
	if err != nil {
		t.Fatal(err)
	}
	runTx := func(fn func(context.Context, *txn.Transaction) error) {
		t.Helper()
		opts := f.options
		opts.Resolver = smallStore
		tx, err := f.manager.Begin(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := tx.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}()
		if err := tx.Statement(t.Context(), func(ctx context.Context) error {
			return fn(ctx, tx)
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	// Run 35 lifetime jobs (far exceeding MaxJobs=8), mixing Succeeded, Failed,
	// and Canceled outcomes, plus one terminal job with a pending retirement.
	const totalJobs = 35
	const retiredJobID = JobID(5)
	for i := 1; i <= totalJobs; i++ {
		id := JobID(i)
		runTx(func(ctx context.Context, tx *txn.Transaction) error {
			job := Job{
				ID: id, TableID: 1, MutationID: model.ObjectID(2000 + i),
				Kind: AddIndexJob, State: JobPending, SourceVersion: 1, TargetVersion: 2, Revision: 1,
			}
			if err := smallStore.Create(ctx, tx, job); err != nil {
				return err
			}
			if id == retiredJobID {
				if err := smallStore.recordRetirements(ctx, tx, id, []storage.ObjectGeneration{{ObjectID: 1, Generation: 1}}); err != nil {
					return err
				}
			}
			var states []JobState
			switch i % 3 {
			case 0:
				states = []JobState{JobDeleteOnly, JobWriteOnly, JobBackfilling, JobValidating, JobPublic, JobSucceeded}
			case 1:
				states = []JobState{JobDeleteOnly, JobReverting, JobFailed}
			default:
				states = []JobState{JobDeleteOnly, JobReverting, JobCanceled}
			}
			for _, state := range states {
				next := job
				next.State = state
				next.Revision++
				if state == JobBackfilling {
					next.SnapshotTS, next.ProtectedTimestamp = 1, 1
				}
				if state == JobReverting {
					if i%3 == 1 {
						next.ErrorCode = "23505"
					} else {
						next.ErrorCode = "57014"
					}
				}
				if state == JobSucceeded || state == JobFailed || state == JobCanceled {
					next.ProtectedTimestamp = 0
				}
				if err := smallStore.Update(ctx, tx, job.Revision, next); err != nil {
					return err
				}
				job = next
			}
			return nil
		})
	}

	// Active scan must succeed with 0 active jobs even though 35 lifetime jobs completed.
	view, err := f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	activeCount := 0
	if err := smallStore.Scan(t.Context(), view, func(Job) error {
		activeCount++
		return nil
	}); err != nil {
		_ = view.Close()
		t.Fatalf("Scan after 35 lifetime jobs with MaxJobs=8 failed: %v", err)
	}
	if activeCount != 0 {
		_ = view.Close()
		t.Fatalf("activeCount = %d, want 0", activeCount)
	}
	// Terminal jobs remain readable via Get and ScanTerminal.
	var terminalIDs []JobID
	if err := smallStore.ScanTerminal(t.Context(), view, func(job Job) error {
		terminalIDs = append(terminalIDs, job.ID)
		return nil
	}); err != nil {
		_ = view.Close()
		t.Fatalf("ScanTerminal failed: %v", err)
	}
	if len(terminalIDs) != totalJobs {
		_ = view.Close()
		t.Fatalf("ScanTerminal count = %d, want %d", len(terminalIDs), totalJobs)
	}
	firstJob, err := smallStore.Get(t.Context(), view, 1)
	if err != nil || !firstJob.Terminal() {
		_ = view.Close()
		t.Fatalf("Get(1) = %+v, %v", firstJob, err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}

	// DeleteTerminalJob refuses to delete a job with pending retirements, and
	// deletes a clean terminal job.
	opts := f.options
	opts.Resolver = smallStore
	tx, err := f.manager.Begin(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Statement(t.Context(), func(ctx context.Context) error {
		return smallStore.DeleteTerminalJob(ctx, tx, retiredJobID)
	}); !errors.Is(err, ErrJobConflict) {
		_ = tx.Close(context.Background())
		t.Fatalf("DeleteTerminalJob with pending retirement = %v, want ErrJobConflict", err)
	}
	if err := tx.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	runTx(func(ctx context.Context, tx *txn.Transaction) error {
		return smallStore.DeleteTerminalJob(ctx, tx, 1)
	})
	view, err = f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := smallStore.Get(t.Context(), view, 1); !errors.Is(err, storage.ErrRowNotFound) {
		_ = view.Close()
		t.Fatalf("Get(1) after DeleteTerminalJob = %v, want ErrRowNotFound", err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}

	// PruneTerminalJobs keeps the newest 4 terminal jobs and skips retiredJobID.
	var pruned int
	runTx(func(ctx context.Context, tx *txn.Transaction) error {
		scanView, err := tx.ReadView(ctx, []txn.ReadDependency{{
			Target:    smallStore.Target(),
			KeyLayout: smallStore.KeyLayout(),
			Spans: []storage.KeySpan{
				{Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{jobKindActive})}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{jobKindActive + 1})}}},
				{Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{jobKindJobRetirement})}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{jobKindJobRetirement + 1})}}},
				{Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{jobKindTerminal})}, Inclusive: true}, Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{jobKindTerminal + 1})}}},
			},
		}})
		if err != nil {
			return err
		}
		var pruneErr error
		pruned, pruneErr = smallStore.PruneTerminalJobs(ctx, scanView, tx, 4)
		return errors.Join(pruneErr, scanView.Close())
	})
	// 34 remaining before prune: keep newest 4 (32..35) + skip retiredJobID (5) => 29 pruned.
	if pruned != 29 {
		t.Fatalf("PruneTerminalJobs pruned = %d, want 29", pruned)
	}
	view, err = f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var remaining []JobID
	if err := smallStore.ScanTerminal(t.Context(), view, func(job Job) error {
		remaining = append(remaining, job.ID)
		return nil
	}); err != nil {
		_ = view.Close()
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	wantRemaining := []JobID{retiredJobID, 32, 33, 34, 35}
	if fmt.Sprint(remaining) != fmt.Sprint(wantRemaining) {
		t.Fatalf("remaining terminal jobs = %v, want %v", remaining, wantRemaining)
	}

	// MaxJobs is still enforced on concurrent active jobs.
	for i := 1; i <= 9; i++ {
		id := JobID(100 + i)
		tableID := model.ObjectID(500 + i)
		runTx(func(ctx context.Context, tx *txn.Transaction) error {
			return smallStore.Create(ctx, tx, Job{
				ID: id, TableID: tableID, MutationID: model.ObjectID(3000 + i),
				Kind: AddIndexJob, State: JobPending, SourceVersion: 1, TargetVersion: 2, Revision: 1,
			})
		})
	}
	view, err = f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = smallStore.Scan(t.Context(), view, func(Job) error { return nil })
	err = errors.Join(err, view.Close())
	if !errors.Is(err, ErrJobLimit) {
		t.Fatalf("Scan with 9 active jobs (MaxJobs=8) = %v, want ErrJobLimit", err)
	}
}

type ws2FaultyPolicies struct {
	base IndexPolicies
	mu   sync.Mutex
	// failRemaining maps JobID to remaining fault count and error to return.
	failRemaining map[JobID]int
	failErr       map[JobID]error
	attempts      map[JobID]int
}

func (p *ws2FaultyPolicies) Policy(ctx context.Context, job Job) (IndexWritePolicy, error) {
	p.mu.Lock()
	p.attempts[job.ID]++
	if rem := p.failRemaining[job.ID]; rem > 0 {
		p.failRemaining[job.ID] = rem - 1
		err := p.failErr[job.ID]
		p.mu.Unlock()
		return IndexWritePolicy{}, err
	}
	p.mu.Unlock()
	return p.base.Policy(ctx, job)
}

func (p *ws2FaultyPolicies) attemptCount(id JobID) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.attempts[id]
}

// ws2MultiTableResolver resolves mutations for both table 1 and table 10.
type ws2MultiTableResolver struct {
	layout []storage.ColumnLayout
}

func (r ws2MultiTableResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if (target.ObjectID != 1 && target.ObjectID != 10) || target.Generation != 1 || target.EncodingVersion != 1 || target.DescriptorVersion == 0 {
		return nil, storage.ErrInvalidTarget
	}
	return txn.NewRowNormalizer(ctx, target, []txn.ColumnRule{{Layout: r.layout[0], KeyPosition: 1}, {Layout: r.layout[1]}}, 4<<20)
}

func addSecondTableToFixture(t *testing.T, f *indexRestartFixture, tableID model.ObjectID, name string) {
	t.Helper()
	integer := f.layout[0].Type
	table, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: tableID, OID: model.OID(10000 + tableID),
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectTable, Name: name},
		Version:  1,
		State:    model.StatePublic,
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Table: &model.TableMetadata{
			ColumnIDHighWatermark: 2,
			Columns: []model.ColumnDescriptor{
				{ID: 1, Name: "id", State: model.StatePublic, Layout: model.TypeLayout{Type: integer}},
				{ID: 2, Name: "value", State: model.StatePublic, Layout: model.TypeLayout{Type: integer}},
			},
			PrimaryKey: []model.KeyElement{{Column: 1, Type: integer}},
		},
	}, f.limits)
	if err != nil {
		t.Fatal(err)
	}
	resolvers := restartResolvers{}
	for _, layout := range f.catalog.Layouts() {
		normalizer, err := txn.NewRowNormalizer(t.Context(), layout.Target, []txn.ColumnRule{{Layout: layout.Columns[0], KeyPosition: 1}, {Layout: layout.Columns[1]}}, 4<<20)
		if err != nil {
			t.Fatal(err)
		}
		resolvers[layout.Target] = normalizer
	}
	opts := f.options
	opts.Resolver = resolvers
	tx, err := f.manager.Begin(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := tx.Close(context.Background()); err != nil {
			t.Error(err)
		}
	}()
	deps, err := f.catalog.CreateDependencies(t.Context(), table)
	if err != nil {
		t.Fatal(err)
	}
	view, err := tx.ReadView(t.Context(), deps)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := f.catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		_ = view.Close()
		t.Fatal(err)
	}
	change, err := f.catalog.Create(t.Context(), snap, table)
	err = errors.Join(err, snap.Close(), view.Close())
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.ApplyMutations(t.Context(), change.Mutations); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func submitIndexJobForTable(t *testing.T, f *indexRestartFixture, tableID, indexID model.ObjectID, indexName string, jobID JobID) Job {
	t.Helper()
	view, err := f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	table, err := f.descriptor(t, view, view.Timestamp(), tableID)
	err = errors.Join(err, view.Close())
	if err != nil {
		t.Fatal(err)
	}
	physical := table.Physical()
	tableRef := storage.ObjectRef{
		ObjectID:          tableID,
		Generation:        physical.Generation,
		DescriptorVersion: table.Version(),
		EncodingVersion:   physical.EncodingVersion,
	}
	index, err := model.NewDescriptor(t.Context(), model.DescriptorSpec{
		ID: indexID, OID: model.OID(20000 + indexID), Version: 1, State: model.StateAbsent,
		Name:     model.NamespaceName{Parent: 2, Kind: model.ObjectIndex, Name: indexName},
		Physical: model.PhysicalIdentity{Generation: 1, EncodingVersion: 1},
		Index:    &model.IndexMetadata{Table: tableID, Unique: false, Keys: []model.KeyElement{{Column: 2, Type: f.layout[1].Type}}},
	}, f.limits)
	if err != nil {
		t.Fatal(err)
	}
	job, err := NewAddIndexJob(jobID, tableRef, index.ID())
	if err != nil {
		t.Fatal(err)
	}
	submission, err := f.lifecycle.SubmitIntent(t.Context(), IndexSubmissionOptions{
		Manager: f.manager, Policies: f.policies, Table: tableRef, Index: index, Job: job, Transaction: f.options,
	})
	if submission != nil {
		defer func() {
			if err := submission.Close(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestIndexJobSchedulerSurvivesRetryExhaustionAndEventWakeup(t *testing.T) {
	t.Run("survives_retry_exhaustion_and_runs_subsequent_jobs", func(t *testing.T) {
		f := newIndexRestartFixture(t)
		addSecondTableToFixture(t, f, 10, "items_two")
		var err error
		f.policies, err = NewIndexPolicyProvider(t.Context(), IndexPolicyOptions{
			Catalog: f.catalog, Views: f.manager, Jobs: f.jobs,
			Encoders: func(context.Context, *catalog.CatalogSnapshot) (KeyCodec, error) { return f.keys, nil },
			Limits:   f.indexLimits, Base: ws2MultiTableResolver{layout: f.layout},
		})
		if err != nil {
			t.Fatal(err)
		}

		// Job 1 fails 40001 enough times to exhaust MaxRetries=1 on its first run
		// AND hits a non-retryable per-job policy error on Job 2's first attempt;
		// Job 3 has no faults and must succeed while scheduler stays alive.
		faults := &ws2FaultyPolicies{
			base: f.policies,
			failRemaining: map[JobID]int{
				1: 3, // > MaxRetries+1 (=2) so runner #1 exits with 40001!
				2: 1, // non-retryable non-corrupt per-job error on first try!
			},
			failErr: map[JobID]error{
				1: &model.Error{Code: "40001", Message: "simulated contention"},
				2: errors.New("transient per-job policy lookup failure"),
			},
			attempts: make(map[JobID]int),
		}

		scheduler, err := NewIndexJobScheduler(t.Context(), IndexJobSchedulerOptions{
			Runner: IndexJobRunnerOptions{
				Manager:       f.manager,
				Policies:      faults,
				Jobs:          f.jobs,
				Lifecycle:     f.lifecycle,
				Protections:   f.protections,
				Transaction:   f.options,
				Scan:          BackfillScanLimits{BatchRows: 2, MaxKeyBytes: 4096, WorkBytes: 4 << 20},
				Backfill:      BackfillLimits{MaxRows: 2, MaxKeyBytes: 8192, WorkBytes: 4 << 20},
				Validation:    IndexValidationLimits{BatchRows: 2, MaxRows: 100, MaxBytes: 8 << 20},
				MaxRetries:    1,
				RetryDelay:    5 * time.Millisecond,
				MaxRetryDelay: 25 * time.Millisecond,
			},
			MaxRunning:   2,
			MemoryBytes:  16 << 20,
			PollInterval: 10 * time.Second, // large poll interval: retries must use backoff timer!
		})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancelRun := context.WithCancel(t.Context())
		runDone := make(chan error, 1)
		go func() {
			runDone <- scheduler.Run(runCtx)
		}()
		t.Cleanup(func() {
			cancelRun()
			_ = scheduler.Close()
			<-runDone
		})

		job1 := submitIndexJobForTable(t, f, 1, 301, "idx_retry_exhaust", 1)
		job2 := submitIndexJobForTable(t, f, 10, 302, "idx_nonretryable", 2)

		waitCtx, cancelWait := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancelWait()
		done1, err := scheduler.Wait(waitCtx, job1.ID)
		if err != nil || done1.State != JobSucceeded {
			t.Fatalf("job1 after retry exhaustion + backoff = %+v, %v", done1, err)
		}
		done2, err := scheduler.Wait(waitCtx, job2.ID)
		if err != nil || done2.State != JobSucceeded {
			t.Fatalf("job2 after transient non-retryable error = %+v, %v", done2, err)
		}

		// Scheduler must STILL be alive and execute a subsequent job promptly.
		job3 := submitIndexJobForTable(t, f, 1, 303, "idx_subsequent", 3)
		done3, err := scheduler.Wait(waitCtx, job3.ID)
		if err != nil || done3.State != JobSucceeded {
			t.Fatalf("subsequent job3 = %+v, %v", done3, err)
		}
		if faults.attemptCount(job1.ID) < 4 {
			t.Fatalf("expected job1 to exhaust runner retries and retry via scheduler backoff; attempts=%d", faults.attemptCount(job1.ID))
		}

		select {
		case runErr := <-runDone:
			t.Fatalf("scheduler exited prematurely: %v", runErr)
		default:
		}
	})

	t.Run("event_wakeup_triggers_promptly_with_large_poll_interval", func(t *testing.T) {
		f := newIndexRestartFixture(t)
		scheduler, err := NewIndexJobScheduler(t.Context(), IndexJobSchedulerOptions{
			Runner: IndexJobRunnerOptions{
				Manager:       f.manager,
				Policies:      f.policies,
				Jobs:          f.jobs,
				Lifecycle:     f.lifecycle,
				Protections:   f.protections,
				Transaction:   f.options,
				Scan:          BackfillScanLimits{BatchRows: 2, MaxKeyBytes: 4096, WorkBytes: 4 << 20},
				Backfill:      BackfillLimits{MaxRows: 2, MaxKeyBytes: 8192, WorkBytes: 4 << 20},
				Validation:    IndexValidationLimits{BatchRows: 2, MaxRows: 100, MaxBytes: 8 << 20},
				MaxRetries:    2,
				RetryDelay:    5 * time.Millisecond,
				MaxRetryDelay: 50 * time.Millisecond,
			},
			MaxRunning:   2,
			MemoryBytes:  16 << 20,
			PollInterval: 10 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		runCtx, cancelRun := context.WithCancel(t.Context())
		runDone := make(chan error, 1)
		go func() {
			runDone <- scheduler.Run(runCtx)
		}()
		t.Cleanup(func() {
			cancelRun()
			_ = scheduler.Close()
			<-runDone
		})

		// Let the scheduler complete its initial idle scan and park on PollInterval=10s.
		time.Sleep(30 * time.Millisecond)

		start := time.Now()
		job := submitIndexJobForTable(t, f, 1, 401, "idx_event_wake", 1)
		waitCtx, cancelWait := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancelWait()
		completed, err := scheduler.Wait(waitCtx, job.ID)
		elapsed := time.Since(start)
		if err != nil || completed.State != JobSucceeded {
			t.Fatalf("event-woken job = %+v, %v (elapsed=%v)", completed, err, elapsed)
		}
		if elapsed >= 2*time.Second {
			t.Fatalf("event wakeup took %v (expected well under 10s PollInterval)", elapsed)
		}
	})
}

type pinObservingRetentionBackend struct {
	real            storage.RetentionBackend
	collectCalls    atomic.Int32
	sawPinInCollect atomic.Bool
	observedState   storage.RetentionState
	mu              sync.Mutex
}

func (b *pinObservingRetentionBackend) RetentionState(ctx context.Context) (storage.RetentionState, error) {
	return b.real.RetentionState(ctx)
}

func (b *pinObservingRetentionBackend) Collect(ctx context.Context, request storage.GCRequest) error {
	b.collectCalls.Add(1)
	state, err := b.real.RetentionState(ctx)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.observedState = state
	b.mu.Unlock()
	if state.HasPin {
		b.sawPinInCollect.Store(true)
	}
	return b.real.Collect(ctx, request)
}

func TestRetentionCoordinatorClosesSnapshotReaderBeforeCollect(t *testing.T) {
	f := newIndexRestartFixture(t)

	// Install durable history retention policy (1ns so clock advance makes history collectible).
	opts := f.options
	opts.Resolver = f.jobs
	tx, err := f.manager.Begin(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Statement(t.Context(), func(ctx context.Context) error {
		return f.jobs.CreateHistoryPolicy(ctx, tx, time.Nanosecond)
	}); err != nil {
		_ = tx.Close(context.Background())
		t.Fatal(err)
	}
	if _, err := tx.Commit(t.Context()); err != nil {
		_ = tx.Close(context.Background())
		t.Fatal(err)
	}
	if err := tx.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Persist a terminal job with a retirement record plus 20 extra terminal jobs
	// (exceeding MaxJobs=16) so we also verify automatic pruning during Advance.
	target := storage.ObjectGeneration{ObjectID: 1, Generation: 1}
	retiredAt := persistTerminalRetirement(t, f, 10, target)
	for i := 11; i <= 30; i++ {
		id := JobID(i)
		tx, err := f.manager.Begin(t.Context(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Statement(t.Context(), func(ctx context.Context) error {
			job := Job{ID: id, TableID: 1, MutationID: model.ObjectID(1000 + id), Kind: AddIndexJob, State: JobPending, SourceVersion: 1, TargetVersion: 2, Revision: 1}
			if err := f.jobs.Create(ctx, tx, job); err != nil {
				return err
			}
			for _, state := range []JobState{JobDeleteOnly, JobWriteOnly, JobBackfilling, JobValidating, JobPublic, JobSucceeded} {
				next := job
				next.State = state
				next.Revision++
				if state == JobBackfilling {
					next.SnapshotTS, next.ProtectedTimestamp = 1, 1
				}
				if state == JobSucceeded {
					next.ProtectedTimestamp = 0
				}
				if err := f.jobs.Update(ctx, tx, job.Revision, next); err != nil {
					return err
				}
				job = next
			}
			return nil
		}); err != nil {
			_ = tx.Close(context.Background())
			t.Fatal(err)
		}
		res, err := tx.Commit(t.Context())
		if err != nil {
			_ = tx.Close(context.Background())
			t.Fatal(err)
		}
		if err := tx.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		retiredAt = max(retiredAt, res.CommitTS)
	}

	// Advance clock past retiredAt so RetentionCoordinator collects at a live boundary.
	f.clock.mu.Lock()
	f.clock.now = max(f.clock.now, clock.Instant(retiredAt+100))
	f.clock.mu.Unlock()
	// Ensure ClosedTS reaches the latest committed timestamp via a closed read view.
	v, err := f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Close(); err != nil {
		t.Fatal(err)
	}

	observer := &pinObservingRetentionBackend{real: f.backend}
	coordinator, err := NewRetentionCoordinator(RetentionOptions{
		Clock:             f.clock,
		Backend:           observer,
		Protections:       f.protections,
		Jobs:              f.jobs,
		Manager:           f.manager,
		Transaction:       f.options,
		MaxRetiredTargets: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Advance(t.Context()); err != nil {
		t.Fatalf("Advance failed: %v", err)
	}
	if observer.collectCalls.Load() != 1 {
		t.Fatalf("Collect calls = %d, want 1", observer.collectCalls.Load())
	}
	if observer.sawPinInCollect.Load() {
		observer.mu.Lock()
		st := observer.observedState
		observer.mu.Unlock()
		t.Fatalf("RetentionCoordinator held an active snapshot pin during Backend.Collect: %+v", st)
	}
	if _, _, err := readRetirement(t, f, 10); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("retirement record for job 10 still present after Advance: %v", err)
	}

	// Verify excess terminal jobs were automatically pruned to MaxJobs (16).
	view, err := f.manager.CurrentReadView(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	terminalCount := 0
	if err := f.jobs.ScanTerminal(t.Context(), view, func(Job) error {
		terminalCount++
		return nil
	}); err != nil {
		_ = view.Close()
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if uint64(terminalCount) > f.jobs.options.MaxJobs {
		t.Fatalf("terminal job count after Advance = %d, want <= %d", terminalCount, f.jobs.options.MaxJobs)
	}
}
