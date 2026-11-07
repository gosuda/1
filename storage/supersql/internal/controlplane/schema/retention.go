package schema

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

const DefaultHistoryRetention = 24 * time.Hour

type RetentionOptions struct {
	Clock             clock.Clock
	Backend           storage.RetentionBackend
	Protections       *JobProtections
	Jobs              *JobStore
	Manager           SchemaTransactions
	Transaction       txn.TransactionOptions
	MaxRetiredTargets uint64
}

var errRetentionOptions = &model.Error{Code: "22023", Message: "invalid history retention options"}

// RetentionCoordinator borrows all dependencies and starts no work of its own.
// The database owner initializes durable policy in its bootstrap transaction and
// stops all Advance calls before closing protections, manager, backend or clock.
// Changing the policy cannot recover history below the already-persisted floor.
type RetentionCoordinator struct {
	options RetentionOptions
	gate    chan struct{}
}

func NewRetentionCoordinator(options RetentionOptions) (*RetentionCoordinator, error) {
	missingRuntime := mutationPolicyNil(options.Clock) || mutationPolicyNil(options.Backend) || mutationPolicyNil(options.Manager)
	if missingRuntime || options.Protections == nil || options.Jobs == nil || options.MaxRetiredTargets == 0 {
		return nil, errRetentionOptions
	}
	if options.Jobs.normalizer == nil {
		return nil, errRetentionOptions
	}
	options.Protections.mu.Lock()
	sameStore := options.Protections.options.Jobs == options.Jobs
	options.Protections.mu.Unlock()
	if !sameStore {
		return nil, errRetentionOptions
	}
	options.Transaction.Mode = txn.ReadWrite
	options.Transaction.Timestamp = 0
	options.Transaction.Resolver = options.Jobs
	options.Transaction.KeyEncoder = options.Jobs.options.Encoder
	if _, _, err := options.Protections.Oldest(); err != nil {
		return nil, err
	}
	return &RetentionCoordinator{options: options, gate: make(chan struct{}, 1)}, nil
}

func (c *RetentionCoordinator) retirementDependency() txn.ReadDependency {
	kinds := [...]byte{2, 4, 5}
	spans := make([]storage.KeySpan, len(kinds))
	for i, kind := range kinds {
		spans[i] = storage.KeySpan{
			Lower: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{kind})}, Inclusive: true},
			Upper: storage.KeyBound{Values: []model.Value{model.BytesVal([]byte{kind + 1})}},
		}
	}
	return txn.ReadDependency{Target: c.options.Jobs.Target(), KeyLayout: c.options.Jobs.KeyLayout(), Spans: spans}
}

// Advance performs at most one collection. Clock, pin-race and collection errors
// are returned without retries; a later call recomputes from durable backend state.
func (c *RetentionCoordinator) Advance(ctx context.Context) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case c.gate <- struct{}{}:
	}
	defer func() { <-c.gate }()
	if err := ctx.Err(); err != nil {
		return err
	}
	transaction, err := c.options.Manager.Begin(ctx, c.options.Transaction)
	if transaction != nil {
		defer func() { err = errors.Join(err, transaction.Close(context.WithoutCancel(ctx))) }()
	}
	if err != nil {
		return err
	}
	if transaction == nil {
		return errRetentionOptions
	}
	// Keep the statement gate until collection returns. Wounding cancels its
	// context but cannot release the policy lock underneath a publication.
	if err := transaction.Statement(ctx, func(operation context.Context) error {
		policy, err := c.options.Jobs.HistoryPolicy(operation, transaction)
		if err != nil {
			return err
		}
		return c.advance(operation, transaction, policy.Retention)
	}); err != nil {
		return err
	}
	_, err = transaction.Commit(ctx)
	if transaction.State() == txn.Indeterminate {
		return errors.Join(storage.ErrCommitIndeterminate, err)
	}
	return err
}

func (c *RetentionCoordinator) collectRetirements(ctx context.Context, reader storage.SnapshotReader, boundary model.Timestamp) ([]storage.ObjectRetirement, []JobID, []RetirementID, error) {
	capacity := min(c.options.MaxRetiredTargets, 64)
	retired := make([]storage.ObjectRetirement, 0, capacity)
	jobs := make([]JobID, 0, capacity)
	ordinary := make([]RetirementID, 0, capacity)
	appendRecord := func(retiredAt model.Timestamp, objects []storage.ObjectGeneration) (bool, error) {
		if retiredAt > boundary {
			return false, nil
		}
		remaining := c.options.MaxRetiredTargets - uint64(len(retired))
		if uint64(len(objects)) > remaining {
			if len(retired) == 0 {
				return false, ErrJobLimit
			}
			return false, nil
		}
		for _, object := range objects {
			retirement, err := storage.NewObjectRetirement(object, retiredAt)
			if err != nil {
				return false, err
			}
			retired = append(retired, retirement)
		}
		return true, nil
	}
	err := c.options.Jobs.Scan(ctx, reader, func(job Job) error {
		if !job.Terminal() {
			return nil
		}
		retiredAt, objects, err := c.options.Jobs.retirements(ctx, reader, job.ID)
		if errors.Is(err, storage.ErrRowNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		included, err := appendRecord(retiredAt, objects)
		if err != nil {
			return err
		}
		if included {
			jobs = append(jobs, job.ID)
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	err = c.options.Jobs.scanOrdinaryRetirements(ctx, reader, func(id RetirementID) error {
		retiredAt, objects, err := c.options.Jobs.ordinaryRetirements(ctx, reader, id)
		if err != nil {
			return err
		}
		included, err := appendRecord(retiredAt, objects)
		if err != nil {
			return err
		}
		if included {
			ordinary = append(ordinary, id)
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, err
	}
	slices.SortFunc(retired, func(left, right storage.ObjectRetirement) int {
		a, b := left.Target(), right.Target()
		if a.ObjectID != b.ObjectID {
			return cmp.Compare(uint64(a.ObjectID), uint64(b.ObjectID))
		}
		return cmp.Compare(a.Generation, b.Generation)
	})
	for i := 1; i < len(retired); i++ {
		if retired[i-1].Target() == retired[i].Target() {
			return nil, nil, nil, ErrJobRecord
		}
	}
	return retired, jobs, ordinary, nil
}

func (c *RetentionCoordinator) advance(ctx context.Context, transaction *txn.Transaction, retention time.Duration) (err error) {
	oldestJob, hasJob, err := c.options.Protections.Oldest()
	if err != nil {
		return err
	}
	state, err := c.options.Backend.RetentionState(ctx)
	if err != nil {
		return err
	}
	invalidFrontier := state.HistoryFloor < 0 || state.ClosedTS < state.HistoryFloor
	invalidPin := state.OldestPin < 0 || (!state.HasPin && state.OldestPin != 0)
	if invalidFrontier || invalidPin {
		return storage.ErrCorruptStorage
	}
	reading, err := c.options.Clock.Read(ctx)
	if err != nil {
		return err
	}
	boundary, err := retentionCutoff(reading, state.LeapHistoryID, retention)
	if errors.Is(err, model.ErrTimestampRange) {
		// The clock is younger than the retention window: nothing is old
		// enough to retire, so there is no boundary to collect.
		return nil
	}
	if err != nil {
		return err
	}
	boundary = min(boundary, state.ClosedTS)
	if state.HasPin {
		boundary = min(boundary, state.OldestPin)
	}
	if hasJob {
		boundary = min(boundary, oldestJob)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if boundary <= state.HistoryFloor {
		return nil
	}
	reader, err := transaction.ReadView(ctx, []txn.ReadDependency{c.retirementDependency()})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, reader.Close()) }()
	retired, jobs, ordinary, err := c.collectRetirements(ctx, reader, boundary)
	if err != nil {
		return err
	}
	if err := c.options.Backend.Collect(ctx, storage.GCRequest{Boundary: boundary, Retired: retired}); err != nil {
		return fmt.Errorf("advance retained history to %d: %w", boundary, errors.Join(err, ctx.Err()))
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, id := range jobs {
		if err := c.options.Jobs.deleteRetirements(ctx, transaction, id); err != nil {
			return err
		}
	}
	for _, id := range ordinary {
		if err := c.options.Jobs.deleteOrdinaryRetirements(ctx, transaction, id); err != nil {
			return err
		}
	}
	return nil
}

func retentionCutoff(reading clock.Reading, history [32]byte, retention time.Duration) (model.Timestamp, error) {
	if reading.Earliest < 0 || reading.Latest < reading.Earliest {
		return 0, clock.ErrClockRange
	}
	uncertified := reading.Status != clock.Synced && reading.Status != clock.Holdover
	if uncertified || reading.Epoch.Assurance == ([16]byte{}) || reading.Epoch.Config == ([32]byte{}) {
		return 0, clock.ErrClockUnavailable
	}
	if history == ([32]byte{}) || reading.Epoch.LeapHistory != history {
		return 0, clock.ErrClockEpochChanged
	}
	// Both values use SI nanoseconds. Add rejects nonpositive cutoffs instead of
	// wrapping them into a future collection boundary.
	return model.Timestamp(reading.Earliest).Add(-retention)
}
