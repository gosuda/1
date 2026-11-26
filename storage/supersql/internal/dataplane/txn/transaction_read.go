package txn

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type transactionDependency struct {
	target     storage.ObjectRef
	occurrence model.ScanOccurrence
	eligible   bool
	keyLayout  []storage.KeyColumn
	spans      []lockRequest
}

func pointLock(target storage.ObjectRef, key []byte) lockRequest {
	bound := lockBound{Key: key, Inclusive: true}
	return lockRequest{ObjectID: target.ObjectID, Generation: target.Generation, Lower: bound, Upper: bound}
}
func (transaction *Transaction) lockIntent(ctx context.Context, target storage.ObjectRef) error {
	acquired, err := transaction.lockTableIntentWithPolicy(ctx, target, lockIntent, lockWait)
	if err == nil && !acquired {
		return errTransactionOptions
	}
	return err
}

func (transaction *Transaction) lockWriteIntent(ctx context.Context, target storage.ObjectRef) error {
	acquired, err := transaction.lockTableIntentWithPolicy(ctx, target, lockTableRowExclusive, lockWait)
	if err == nil && !acquired {
		return errTransactionOptions
	}
	return err
}

// snapshotResult is one opened reader together with the identity of the catalog
// content it exposes.
type snapshotResult struct {
	reader    storage.SnapshotReader
	timestamp model.Timestamp
	identity  CatalogReadIdentity
}

func (transaction *Transaction) snapshot(ctx context.Context) (snapshotResult, error) {
	if transaction.mode == ReadWrite {
		overlay, overlayVersion, err := transaction.overlay.FreezeVersion(ctx)
		if err != nil {
			return snapshotResult{}, err
		}
		opened, openErr := transaction.manager.openTransactionSnapshot(ctx, overlay, overlayVersion)
		if openErr == nil && opened.reader != nil {
			transaction.dataAccessed.Store(true)
		}
		err = errors.Join(openErr, overlay.Close())
		if err != nil {
			if opened.reader != nil {
				err = errors.Join(err, opened.reader.Close())
			}
			return snapshotResult{}, err
		}
		if opened.identity.Overlay != 0 {
			opened.identity.OverlayOwner = transaction.id
		}
		return snapshotResult{reader: opened.reader, timestamp: opened.timestamp, identity: opened.identity}, nil
	}
	if transaction.base == nil {
		if transaction.mode == HistoricalReadOnly {
			base, err := transaction.manager.HistoricalSnapshot(ctx, transaction.timestamp)
			if err != nil {
				return snapshotResult{}, err
			}
			transaction.base = base
		} else {
			opened, err := transaction.manager.openTransactionSnapshot(ctx, nil, 0)
			if err != nil {
				return snapshotResult{}, err
			}
			transaction.base, transaction.timestamp = opened.reader, opened.timestamp
		}
		transaction.dataAccessed.Store(true)
	}
	opened, err := transaction.manager.historicalSnapshot(ctx, transaction.timestamp)
	if err != nil {
		return snapshotResult{}, err
	}
	return snapshotResult{reader: opened.reader, timestamp: transaction.timestamp, identity: opened.identity}, nil
}

func (transaction *Transaction) Get(ctx context.Context, request storage.ReadRequest) (model.Row, error) {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return model.Row{}, err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return model.Row{}, err
	}
	if err := request.Validate(operation); err != nil {
		return model.Row{}, err
	}
	if !transaction.reserveResource() {
		return model.Row{}, errTransactionBudget
	}
	defer transaction.resources.Add(-1)
	var position lockPosition
	if transaction.owner != nil {
		position, err = transaction.manager.locks.savepoint(transaction.owner)
		if err != nil {
			return model.Row{}, transactionError(err)
		}
		if transaction.mode == ReadWrite {
			if err := transaction.lockIntent(operation, request.Target); err != nil {
				return model.Row{}, transactionError(errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position)))
			}
			if _, err := transaction.manager.locks.acquire(operation, transaction.owner, pointLock(request.Target, request.Key), lockShare, lockWait); err != nil {
				err = errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position))
				return model.Row{}, transactionError(err)
			}
		}
	}
	result, err := transaction.snapshot(operation)
	if err != nil {
		if position.owner != nil {
			err = errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position))
		}
		return model.Row{}, transactionError(err)
	}
	reader := result.reader
	row, getErr := reader.Get(operation, request)
	closeErr := reader.Close()
	err = errors.Join(getErr, closeErr)
	if err == nil {
		return row, nil
	}
	if position.owner != nil && (!errors.Is(getErr, storage.ErrRowNotFound) || closeErr != nil) {
		err = errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position))
	}
	return model.Row{}, transactionError(err)
}

func encodeDependencyBound(ctx context.Context, encoder storage.KeyEncoder, layout []storage.KeyColumn, bound storage.KeyBound, budget uint64) (lockBound, error) {
	if bound.Unbounded {
		if len(bound.Values) != 0 || bound.Inclusive {
			return lockBound{}, storage.ErrRangeInvalid
		}
		return lockBound{Unbounded: true}, nil
	}
	if len(bound.Values) == 0 || len(bound.Values) > len(layout) {
		return lockBound{}, storage.ErrRangeInvalid
	}
	for i, value := range bound.Values {
		if value.Type != layout[i].Type.Storage {
			return lockBound{}, storage.ErrRangeInvalid
		}
	}
	key, err := encoder.EncodePrefix(ctx, layout, bound.Values, budget)
	if err != nil {
		return lockBound{}, err
	}
	if uint64(cap(key)) > budget {
		return lockBound{}, errTransactionBudget
	}
	return lockBound{Key: key, Inclusive: bound.Inclusive, Prefix: len(bound.Values) < len(layout)}, nil
}

func encodeDependencySpan(ctx context.Context, encoder storage.KeyEncoder, target storage.ObjectRef, layout []storage.KeyColumn, span storage.KeySpan, budget uint64) (lockRequest, uint64, error) {
	lower, err := encodeDependencyBound(ctx, encoder, layout, span.Lower, budget)
	if err != nil {
		return lockRequest{}, 0, err
	}
	used := uint64(cap(lower.Key))
	upper, err := encodeDependencyBound(ctx, encoder, layout, span.Upper, budget-used)
	if err != nil {
		return lockRequest{}, 0, err
	}
	request := lockRequest{ObjectID: target.ObjectID, Generation: target.Generation, Lower: lower, Upper: upper}
	if !validLockRequest(request) {
		return lockRequest{}, 0, storage.ErrRangeInvalid
	}
	return request, used + uint64(cap(upper.Key)), nil
}

func (transaction *Transaction) dependencies(ctx context.Context, supplied []ReadDependency) ([]transactionDependency, uint64, error) {
	// The declared set is copied within what the shared view allowance can still
	// take; the resulting charge is reserved when the view is created.
	memory := overlayMemory{limit: transaction.viewSpace.available()}
	if err := memory.add(uint64(len(supplied)), uint64(unsafe.Sizeof(transactionDependency{}))); err != nil {
		return nil, 0, errTransactionBudget
	}
	dependencies := make([]transactionDependency, len(supplied))
	for i, dependency := range supplied {
		if err := dependency.Target.Validate(); err != nil {
			return nil, 0, err
		}
		keyLayout, used, err := storage.CopyKeyLayout(ctx, dependency.KeyLayout, memory.limit-memory.used)
		if err != nil {
			if errors.Is(err, model.ErrValueBudget) {
				return nil, 0, errors.Join(errTransactionBudget, err)
			}
			return nil, 0, err
		}
		memory.used += used
		if err := memory.add(uint64(len(dependency.Spans)), uint64(unsafe.Sizeof(lockRequest{}))); err != nil {
			return nil, 0, errTransactionBudget
		}
		owned := transactionDependency{target: dependency.Target, occurrence: dependency.Occurrence, keyLayout: keyLayout, spans: make([]lockRequest, len(dependency.Spans))}
		for j, span := range dependency.Spans {
			request, used, err := encodeDependencySpan(ctx, transaction.encoder, dependency.Target, owned.keyLayout, span, memory.limit-memory.used)
			if err != nil {
				return nil, 0, err
			}
			memory.used += used
			owned.spans[j] = request
		}
		dependencies[i] = owned
	}
	return dependencies, memory.used, nil
}

func (transaction *Transaction) ReadView(ctx context.Context, dependencies []ReadDependency) (ReadView, error) {
	view, _, err := transaction.readView(ctx, dependencies, lockWait, nil)
	return view, err
}

// ReadViewForRowLocks acquires each occurrence's row policy before opening the
// snapshot. Untargeted dependencies retain ordinary strict-PCC waiting reads.
func (transaction *Transaction) ReadViewForRowLocks(ctx context.Context, dependencies []ReadDependency, policies []RowLockPolicy) (ReadView, error) {
	if len(policies) == 0 {
		return transaction.ReadView(ctx, dependencies)
	}
	for i, policy := range policies {
		if policy.Occurrence == 0 || policy.Strength > UpdateLock || policy.Wait > SkipLockedRow {
			return nil, errTransactionOptions
		}
		for _, prior := range policies[:i] {
			if prior.Occurrence == policy.Occurrence {
				return nil, errTransactionOptions
			}
		}
		matched := false
		for _, dependency := range dependencies {
			if dependency.Occurrence == policy.Occurrence {
				matched = true
				break
			}
		}
		if !matched {
			return nil, errTransactionOptions
		}
	}
	view, _, err := transaction.readView(ctx, dependencies, lockWait, policies)
	return view, err
}

// ReadViewSkipLocked opens a view only when every declared read dependency can
// be acquired immediately. A false result retains no lock acquired by the call.
func (transaction *Transaction) ReadViewSkipLocked(ctx context.Context, dependencies []ReadDependency) (ReadView, bool, error) {
	return transaction.readView(ctx, dependencies, lockSkip, nil)
}

func (transaction *Transaction) readView(ctx context.Context, dependencies []ReadDependency, policy lockWaitPolicy, rowPolicies []RowLockPolicy) (ReadView, bool, error) {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return nil, false, err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return nil, false, err
	}
	if len(rowPolicies) != 0 && transaction.mode != ReadWrite {
		return nil, false, ErrReadOnlyTransaction
	}
	if policy > lockSkip || len(rowPolicies) != 0 && policy != lockWait {
		return nil, false, errTransactionOptions
	}
	if err := transaction.closeViews(^uint64(0)); err != nil {
		return nil, false, err
	}
	if !transaction.reserveResource() {
		return nil, false, errTransactionBudget
	}
	var owned []transactionDependency
	var used uint64
	var position lockPosition
	acquired := true
	if transaction.mode == ReadWrite {
		owned, used, err = transaction.dependencies(operation, dependencies)
		if err == nil {
			position, err = transaction.manager.locks.savepoint(transaction.owner)
		}
		if err == nil {
			if len(rowPolicies) != 0 {
				var extra uint64
				owned, extra, err = transaction.lockRowPolicyDependencies(operation, owned, rowPolicies, transaction.viewSpace.remaining(used))
				used += extra
			} else {
				acquired, err = transaction.lockDependencies(operation, owned, policy)
			}
		}
	}
	if err != nil || !acquired {
		transaction.resources.Add(-1)
		if position.owner != nil {
			err = errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position))
		}
		return nil, false, transactionError(err)
	}
	// The view retains its dependency set until it closes; the allowance is
	// shared with the other views the transaction has open.
	if err := transaction.viewSpace.charge(used); err != nil {
		transaction.resources.Add(-1)
		if position.owner != nil {
			err = errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position))
		}
		return nil, false, err
	}
	result, err := transaction.snapshot(operation)
	if err != nil {
		transaction.resources.Add(-1)
		transaction.viewSpace.release(used)
		if position.owner != nil {
			err = errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position))
		}
		return nil, false, transactionError(err)
	}
	reader, timestamp := result.reader, result.timestamp
	if transaction.viewSerial == ^uint64(0) {
		transaction.resources.Add(-1)
		transaction.viewSpace.release(used)
		if position.owner != nil {
			err = errors.Join(err, transaction.manager.locks.rollback(transaction.owner, position))
		}
		return nil, false, errors.Join(errTransactionBudget, err, reader.Close())
	}
	transaction.viewSerial++
	life, cancel := context.WithCancelCause(transaction.life)
	view := &transactionView{transaction: transaction, reader: reader, timestamp: timestamp, identity: result.identity, life: life, cancel: cancel, dependencies: owned, scopedOccurrences: len(rowPolicies) != 0, serial: transaction.viewSerial, used: used}
	view.publicationOwner = transaction.PublicationOwner()
	transaction.views = append(transaction.views, view)
	return view, true, nil
}

func (transaction *Transaction) lockDependencies(ctx context.Context, dependencies []transactionDependency, policy lockWaitPolicy) (bool, error) {
	for _, dependency := range dependencies {
		acquired, err := transaction.lockTableIntentWithPolicy(ctx, dependency.target, lockIntent, policy)
		if err != nil || !acquired {
			return false, err
		}
		for _, span := range dependency.spans {
			acquired, err := transaction.manager.locks.acquire(ctx, transaction.owner, span, lockShare, policy)
			if err != nil || !acquired {
				return false, err
			}
		}
	}
	return true, nil
}
func rowLockPolicyForOccurrence(policies []RowLockPolicy, occurrence model.ScanOccurrence) (RowLockPolicy, bool) {
	for _, policy := range policies {
		if policy.Occurrence == occurrence {
			return policy, true
		}
	}
	return RowLockPolicy{}, false
}

func (transaction *Transaction) lockRowPolicyDependencies(ctx context.Context, dependencies []transactionDependency, policies []RowLockPolicy, budget uint64) ([]transactionDependency, uint64, error) {
	var used uint64
	unit := uint64(unsafe.Sizeof(lockRequest{}))
	for i, dependency := range dependencies {
		policy, targeted := rowLockPolicyForOccurrence(policies, dependency.occurrence)
		tableMode := lockIntent
		if targeted {
			tableMode = lockTableRowShare
		}
		acquired, err := transaction.lockTableIntentWithPolicy(ctx, dependency.target, tableMode, lockWait)
		if err != nil {
			return nil, 0, err
		}
		if !acquired {
			return nil, 0, errTransactionOptions
		}
		if !targeted {
			for _, span := range dependency.spans {
				acquired, err = transaction.manager.locks.acquire(ctx, transaction.owner, span, lockShare, lockWait)
				if err != nil || !acquired {
					return nil, 0, err
				}
			}
			continue
		}
		conflictMode := max(lockShare, rowLockMode(policy.Strength))
		if policy.Wait == WaitForRowLock {
			for _, span := range dependency.spans {
				acquired, err = transaction.manager.locks.acquireWaitFor(ctx, transaction.owner, span, lockShare, conflictMode)
				if err != nil || !acquired {
					return nil, 0, err
				}
			}
			continue
		}
		if policy.Wait == NoWaitForRowLock {
			for _, span := range dependency.spans {
				if err := transaction.manager.locks.acquireNoWaitFor(ctx, transaction.owner, span, lockShare, conflictMode); err != nil {
					return nil, 0, err
				}
			}
			continue
		}
		if policy.Wait != SkipLockedRow {
			return nil, 0, errTransactionOptions
		}
		retained := dependency
		retained.eligible = true
		retained.spans = nil
		for _, span := range dependency.spans {
			if used >= budget || (budget-used)/unit == 0 {
				return nil, 0, errTransactionBudget
			}
			maxParts := int(min((budget-used)/unit, uint64(^uint(0)>>1)))
			parts, bytes, err := transaction.manager.locks.acquireEligible(ctx, transaction.owner, span, lockShare, conflictMode, maxParts)
			if err != nil {
				return nil, 0, err
			}
			if bytes > budget-used {
				return nil, 0, errTransactionBudget
			}
			used += bytes
			before := cap(retained.spans)
			retained.spans = append(retained.spans, parts...)
			growth := uint64(cap(retained.spans)-before) * unit
			if growth > budget-used {
				return nil, 0, errTransactionBudget
			}
			used += growth
		}
		dependencies[i] = retained
	}
	return dependencies, used, nil
}

func lockContains(outer, inner lockRequest) bool {
	if outer.ObjectID != inner.ObjectID || outer.Generation != inner.Generation {
		return false
	}
	if !outer.Lower.Unbounded {
		if inner.Lower.Unbounded {
			return false
		}
		order := compareLockBounds(outer.Lower, inner.Lower, lowerLockEdge(outer.Lower), lowerLockEdge(inner.Lower))
		excludedBoundary := order == 0 && !outer.Lower.Inclusive && inner.Lower.Inclusive
		if order > 0 || excludedBoundary {
			return false
		}
	}
	if !outer.Upper.Unbounded {
		if inner.Upper.Unbounded {
			return false
		}
		order := compareLockBounds(outer.Upper, inner.Upper, -lowerLockEdge(outer.Upper), -lowerLockEdge(inner.Upper))
		excludedBoundary := order == 0 && !outer.Upper.Inclusive && inner.Upper.Inclusive
		if order < 0 || excludedBoundary {
			return false
		}
	}
	return true
}
