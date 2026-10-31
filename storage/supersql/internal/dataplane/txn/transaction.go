package txn

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type TransactionMode uint8

const (
	ReadWrite TransactionMode = iota
	ReadOnly
	HistoricalReadOnly
)

type TransactionState uint8

const (
	Active TransactionState = iota
	Aborting
	Committing
	Committed
	Aborted
	Indeterminate
)

// MutationResolver borrows immutable, pinned row policies through transaction end.
// It provides row completion, not SQL constraint or index expansion.
type MutationResolver interface {
	ResolveMutation(context.Context, storage.ObjectRef) (*RowNormalizer, error)
}

// MutationResolverExtension adds private statement targets while retaining the transaction's base policies.
type MutationResolverExtension interface {
	WithBaseResolver(MutationResolver) MutationResolver
}

// DeleteRangeResolver identifies targets whose affected rows must be expanded
// through higher-level index, constraint and trigger policies.
type DeleteRangeResolver interface {
	RequiresDeleteRangeExpansion(context.Context, storage.ObjectRef) (bool, error)
}

// MutationApplier lends statement-scoped reads and writes to constraint/index
// maintenance. Its caller owns the surrounding atomic statement and transaction.
type MutationApplier interface {
	Get(context.Context, storage.ReadRequest) (model.Row, error)
	ApplyMutations(context.Context, []storage.Mutation) error
}

type TransactionOptions struct {
	Mode                    TransactionMode
	Timestamp               model.Timestamp
	Resolver                MutationResolver
	KeyEncoder              storage.KeyEncoder
	MemoryBytes, SpillBytes uint64
	SpillDirectory          string
	DisableSpill            bool
	MaxViews                int
	WaitOwner               locks.WaitOwner
}

// ReadDependency owns one protected physical span set. Occurrence is zero for
// catalog and other auxiliary reads not owned by a logical scan RTE.
type ReadDependency struct {
	Target     storage.ObjectRef
	Occurrence model.ScanOccurrence
	KeyLayout  []storage.KeyColumn
	Spans      []storage.KeySpan
}

// RowLockPolicy applies only to dependencies with the matching scan occurrence.
type RowLockPolicy struct {
	Occurrence model.ScanOccurrence
	Strength   RowLockStrength
	Wait       RowLockWait
}

var (
	errTransactionOptions         = &model.Error{Code: "22023", Message: "invalid transaction options"}
	errTransactionInactive        = &model.Error{Code: "25P02", Message: "transaction is not active"}
	errTransactionSavepoint       = &model.Error{Code: "3B001", Message: "savepoint is not on the retained transaction branch"}
	ErrDeleteRangeExpansion       = &model.Error{Code: "55000", Message: "range delete requires affected-row expansion"}
	errTransactionCoverage        = &model.Error{Code: "55000", Message: "read is outside declared transaction dependencies", Cause: storage.ErrInvalidScanPlan}
	errTransactionBudget          = &model.Error{Code: "53200", Message: "transaction resource limit exceeded"}
	errTransactionReentry         = &model.Error{Code: "25001", Message: "transaction lifecycle cannot change during expression evaluation"}
	ErrTransactionModeAfterAccess = &model.Error{Code: "25001", Message: "transaction mode must be set before data access"}
)

// transactionCoverageError reports an undeclared read with the exact object and
// key it targeted, so a coverage failure names the read instead of only the
// rule. An eight-byte key is also rendered as its big-endian identity, which is
// how the catalog collections address descriptor, OID, and namespace rows.
func transactionCoverageError(target storage.ObjectRef, key []byte) error {
	detail := fmt.Sprintf("object %d generation %d descriptor version %d encoding version %d key %x", target.ObjectID, target.Generation, target.DescriptorVersion, target.EncodingVersion, key)
	if len(key) == 8 {
		detail += fmt.Sprintf(" identity %d", binary.BigEndian.Uint64(key))
	}
	return &model.Error{
		Code:    errTransactionCoverage.Code,
		Message: errTransactionCoverage.Message,
		Detail:  detail,
		Cause:   storage.ErrInvalidScanPlan,
	}
}

type transactionTokenKey struct{}
type transactionToken struct {
	transaction *Transaction
	active      atomic.Bool
}

type transactionTarget struct {
	normalizer *RowNormalizer
	layout     storage.ObjectLayout
	bytes      uint64
}

// Transaction serializes statements. Only synchronous bound-expression calls
// carrying the active private token can enter the current statement recursively.
type Transaction struct {
	manager                                   *Manager
	id                                        model.TxnID
	mode                                      TransactionMode
	state                                     atomic.Uint32
	publications                              publicationRegistry
	dataAccessed                              atomic.Bool
	gate                                      chan struct{}
	life                                      context.Context
	cancel                                    context.CancelCauseFunc
	stopCleanup                               func() bool
	cleanupDone                               chan struct{}
	owner                                     *lockOwner
	resolver                                  MutationResolver
	encoder                                   storage.KeyEncoder
	log                                       *mutationLog
	overlay                                   *Overlay
	base                                      storage.SnapshotReader
	timestamp                                 model.Timestamp
	maxViews                                  int
	viewSpace                                 viewBudget
	scratchBytes, metadataLimit, metadataUsed uint64
	scratchUsed                               uint64
	resources                                 atomic.Int64
	views                                     []*transactionView
	viewSerial, ordinal                       uint64
	rowIdentitySerial                         uint64
	targets                                   []transactionTarget
	marks                                     []*transactionMark
	statementDepth                            int
	result                                    storage.ApplyResult
	terminalError                             error
}

func (transaction *Transaction) ID() model.TxnID { return transaction.id }

// HasDataAccess records snapshot acquisition, including access later rolled back
// to a savepoint. Transaction-characteristic guards must not forget that access.
func (transaction *Transaction) HasDataAccess() bool { return transaction.dataAccessed.Load() }

// SetReadOnly changes the actual transaction mode before its first data access.
// An initially writable transaction retains its write capability across a
// read-only interval so rolling back to an earlier savepoint can restore it.
func (transaction *Transaction) SetReadOnly(ctx context.Context, readOnly bool) error {
	operation, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if err := transaction.active(operation); err != nil {
		return err
	}
	if (transaction.mode != ReadWrite) == readOnly {
		return nil
	}
	if transaction.dataAccessed.Load() || transaction.base != nil {
		return ErrTransactionModeAfterAccess
	}
	if transaction.mode == HistoricalReadOnly || transaction.log == nil {
		return ErrReadOnlyTransaction
	}
	position, err := transaction.manager.locks.savepoint(transaction.owner)
	if err != nil {
		return transactionError(err)
	}
	if position.ordinal != 0 {
		return ErrTransactionModeAfterAccess
	}
	if readOnly {
		transaction.mode = ReadOnly
	} else {
		transaction.mode = ReadWrite
	}
	return nil
}

func (transaction *Transaction) State() TransactionState {
	return TransactionState(transaction.state.Load())
}
func (transaction *Transaction) setState(state TransactionState) {
	transaction.state.Store(uint32(state))
	if transaction.publications.owner != nil {
		switch state {
		case Committing, Indeterminate:
			transaction.publications.owner.Decide(model.PublicationPending)
		case Committed:
			transaction.publications.owner.Decide(model.PublicationCommitted)
		case Aborting, Aborted:
			transaction.publications.owner.Decide(model.PublicationAborted)
		}
	}
}

func transactionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errLockWounded) {
		return errors.Join(errLockWounded, err)
	}
	if errors.Is(err, ErrLockTimeout) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &model.Error{Code: "57014", Message: "transaction operation canceled", Cause: err}
	}
	return err
}

func (transaction *Transaction) enter(ctx context.Context) (context.Context, func(), bool, error) {
	if token, ok := ctx.Value(transactionTokenKey{}).(*transactionToken); ok && token.transaction == transaction && token.active.Load() {
		return ctx, func() {}, true, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, false, transactionError(err)
	}
	select {
	case transaction.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, false, transactionError(ctx.Err())
	}
	operation, cancel := context.WithCancelCause(ctx)
	joined := make(chan struct{})
	stop := context.AfterFunc(transaction.life, func() { cancel(context.Cause(transaction.life)); close(joined) })
	token := &transactionToken{transaction: transaction}
	token.active.Store(true)
	operation = context.WithValue(operation, transactionTokenKey{}, token)
	return operation, func() {
		token.active.Store(false)
		if stop() {
			close(joined)
		}
		<-joined
		cancel(context.Canceled)
		<-transaction.gate
	}, false, nil
}

func (transaction *Transaction) active(ctx context.Context) error {
	cause := context.Cause(transaction.life)
	if errors.Is(cause, errLockWounded) {
		return errLockWounded
	}
	if cause != nil && transaction.State() == Aborting {
		return transactionError(cause)
	}
	if transaction.State() != Active {
		return errTransactionInactive
	}
	if cause != nil {
		return transactionError(cause)
	}
	return transactionError(context.Cause(ctx))
}

func (transaction *Transaction) reserveResource() bool {
	for {
		current := transaction.resources.Load()
		if current >= int64(transaction.maxViews) {
			return false
		}
		if transaction.resources.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (transaction *Transaction) closeViews(after uint64) error {
	for _, view := range transaction.views {
		if view.serial > after {
			view.cancel(context.Canceled)
		}
	}
	var err error
	retained := transaction.views[:0]
	for _, view := range transaction.views {
		if view.serial > after || view.closed.Load() {
			err = errors.Join(err, view.Close())
		} else {
			retained = append(retained, view)
		}
	}
	clear(transaction.views[len(retained):])
	transaction.views = retained
	return err
}

// cleanupLocked releases data ownership before acknowledging lock cleanup.
func (transaction *Transaction) cleanupLocked() error {
	err := transaction.closeViews(0)
	transaction.publications.finish()
	if transaction.base != nil {
		err = errors.Join(err, transaction.base.Close())
		transaction.base = nil
	}
	if transaction.overlay != nil {
		err = errors.Join(err, transaction.overlay.Close())
		transaction.overlay = nil
	}
	if transaction.log != nil {
		err = errors.Join(err, transaction.log.Close())
		transaction.log = nil
	}
	for _, mark := range transaction.marks {
		mark.valid = false
		mark.checkpoint = transactionCheckpoint{}
	}
	transaction.marks = nil
	transaction.targets = nil
	transaction.resolver, transaction.encoder = nil, nil
	if transaction.State() == Aborting {
		transaction.setState(Aborted)
	}
	if transaction.owner != nil {
		err = errors.Join(err, transaction.manager.locks.release(transaction.owner))
		transaction.owner = nil
	}
	transaction.manager.mu.Lock()
	delete(transaction.manager.transactions, transaction.id)
	if err != nil {
		transaction.manager.transactionCloseError = errors.Join(transaction.manager.transactionCloseError, err)
	}
	transaction.manager.mu.Unlock()
	if transaction.stopCleanup != nil && transaction.stopCleanup() {
		close(transaction.cleanupDone)
		transaction.manager.transactionCleanups.Done()
	}
	transaction.cancel(context.Canceled)
	transaction.terminalError = errors.Join(transaction.terminalError, err)
	return err
}

func (transaction *Transaction) abortLocked(force bool) error {
	if transaction.State() == Committed || transaction.State() == Aborted {
		return transaction.terminalError
	}
	if transaction.State() == Indeterminate && !force {
		return storage.ErrCommitIndeterminate
	}
	transaction.setState(Aborting)
	err := transaction.cleanupLocked()
	transaction.setState(Aborted)
	return err
}

func (transaction *Transaction) Rollback(ctx context.Context) error {
	if token, ok := ctx.Value(transactionTokenKey{}).(*transactionToken); ok && token.transaction == transaction && token.active.Load() {
		return errTransactionReentry
	}
	if err := ctx.Err(); err != nil {
		return transactionError(err)
	}
	if transaction.State() == Indeterminate {
		return storage.ErrCommitIndeterminate
	}
	if transaction.state.CompareAndSwap(uint32(Active), uint32(Aborting)) {
		transaction.publications.owner.Decide(model.PublicationAborted)
		transaction.cancel(context.Canceled)
	}
	_, finish, _, err := transaction.enter(ctx)
	if err != nil {
		return err
	}
	err = transaction.abortLocked(false)
	terminal := transaction.State() == Aborted || transaction.State() == Committed
	finish()
	if terminal {
		<-transaction.cleanupDone
	}
	return err
}

// Close preserves the invocation token for reentry rejection, while cancellation
// cannot make an external owner skip joined cleanup.
func (transaction *Transaction) Close(ctx context.Context) error {
	return transaction.Rollback(context.WithoutCancel(ctx))
}

func (transaction *Transaction) shutdown() error {
	transaction.cancel(context.Canceled)
	transaction.gate <- struct{}{}
	err := transaction.abortLocked(true)
	<-transaction.gate
	<-transaction.cleanupDone
	return err
}
