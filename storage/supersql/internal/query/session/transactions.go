package session

import (
	"context"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"math"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/schema"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
)

type TransactionManager interface {
	Begin(context.Context, txn.TransactionOptions) (*txn.Transaction, error)
}

// Manager, Settings, and the collaborators in Options are borrowed. Settings
// must not be mutated outside Transactions while a transaction block is active.
// The manager must outlive unresolved commits, including after Close.
type AdvisoryLockConfig struct {
	Manager   *locks.AdvisoryLockManager
	SessionID uint64
}

type TransactionConfig struct {
	Manager      TransactionManager
	Settings     *Settings
	Options      txn.TransactionOptions
	SequenceDDL  *SequenceDDLConfig
	NamedObjects SessionNamedObjectOptions
	Advisory     AdvisoryLockConfig
	// TypeRuntime describes how declared-type runtimes are built for statement
	// bodies that bind keys outside the prepared-statement path. Nil disables
	// declared-type key resolution.
	TypeRuntime *DeclaredTypeRuntimeOptions
	// TemporalClock is borrowed; its host owns startup, assurance, and shutdown.
	TemporalClock clock.Clock
}

type TransactionStart struct {
	Characteristics  BeginOptions
	Historical       model.Timestamp
	ImplicitReadOnly bool
}

type EndResult struct {
	Committed bool
	Result    storage.ApplyResult
	Chained   bool
}

type transactionCharacteristics struct {
	isolation  string
	readOnly   bool
	deferrable bool
}

func (characteristics transactionCharacteristics) start() TransactionStart {
	return TransactionStart{Characteristics: BeginOptions{
		Isolation:  characteristics.isolation,
		ReadOnly:   &characteristics.readOnly,
		Deferrable: &characteristics.deferrable,
	}}
}

type sessionTransactionKey struct{}

type sessionTransactionOperation struct {
	owner  *Transactions
	active atomic.Bool
}

// Transactions owns each begun transaction and serializes session commands.
// Returned views are caller-closed; transaction end also closes retained views.
// TemporalRuntime and RandomRuntime may be called from a Statement callback.
type constraintModeID struct {
	Object     model.ObjectID
	Constraint model.ObjectID
}

type constraintModeState struct {
	all   *bool
	named map[constraintModeID]bool
}

type Transactions struct {
	manager       TransactionManager
	settingMu     sync.Mutex
	settings      *Settings
	options       txn.TransactionOptions
	advisory      AdvisoryLockConfig
	gate          chan struct{}
	life          context.Context
	cancel        context.CancelCauseFunc
	cleanupDone   chan struct{}
	closed        bool
	closeError    error
	closeReturned bool
	fence         error
	temporalClock clock.Clock
	temporal      temporalFrame
	randomMu      sync.Mutex
	randomState   [2]uint64

	active                  bool
	historical              model.Timestamp
	transaction             *txn.Transaction
	generation              uint64
	sampleSerial            uint64
	indexSubmission         *schema.IndexSubmission
	sequences               *sequencePublications
	failure                 error
	savepoints              []txn.Savepoint
	advisoryMarks           []locks.AdvisorySavepoint
	deferredTriggers        []deferredRoutineTrigger
	deferredMarks           []int
	deferredConstraints     []deferredRelationalConstraint
	deferredConstraintMarks []int
	deferredConstraintBytes uint64
	constraintModes         constraintModeState
	constraintSavepoints    []constraintModeState
	typeRuntime             *DeclaredTypeRuntimeOptions
	statementTypes          builtin.TypeResolver
	// declaredTypes reuses the declared-type definitions scanned for a statement
	// that reads exactly the same catalog content and overlay.
	declaredTypes declaredTypeDefinitionsCache
	// declaredRoutines reuses the routine definitions discovered for declared-type
	// runtimes that read the same catalog content, seeds, and limits.
	declaredRoutines declaredRoutineCache
	// discoveries reuses the catalog discovery suffixes of statements that read
	// the same catalog content; locks are still acquired per statement.
	discoveries      catalogDiscoveryCache
	named            sessionNamedObjects
	cursorSavepoints []uint64
	introspecting    atomic.Bool
	chain            bool
	chainModes       transactionCharacteristics
	transactionTimer *sessionTimeout
	idleTimer        *sessionTimeout
}

var (
	errSessionTransactionConfig  = &model.Error{Code: "22023", Message: "session transactions require a manager and idle settings"}
	errSessionTransactionState   = &model.Error{Code: "XX000", Message: "session and transaction state disagree"}
	errSessionTransactionReentry = &model.Error{Code: "25001", Message: "session operations cannot reenter a transaction command"}
	errSessionStatement          = &model.Error{Code: "22023", Message: "transaction statement requires a callback"}
	errSessionStatementPanic     = &model.Error{Code: "25P02", Message: "session transaction statement did not complete"}
)

func NewTransactions(ctx context.Context, config TransactionConfig) (*Transactions, error) {
	if err := ctx.Err(); err != nil {
		return nil, sessionTransactionError(err)
	}
	if config.Manager == nil || config.Settings == nil || config.Settings.InTransaction() {
		return nil, errSessionTransactionConfig
	}
	advisoryConfigured := config.Advisory.Manager != nil || config.Advisory.SessionID != 0
	if advisoryConfigured && (config.Advisory.Manager == nil || config.Advisory.SessionID == 0) {
		return nil, errSessionTransactionConfig
	}
	if advisoryConfigured {
		waitOwner := locks.WaitOwner(config.Advisory.SessionID)
		if config.Options.WaitOwner != 0 && config.Options.WaitOwner != waitOwner {
			return nil, errSessionTransactionConfig
		}
		config.Options.WaitOwner = waitOwner
	}
	var randomSeed [16]byte
	if _, err := cryptorand.Read(randomSeed[:]); err != nil {
		return nil, err
	}
	sequences, err := newSequencePublications(config.SequenceDDL, config.Settings.limits.MaxSavepoints)
	if err != nil {
		return nil, err
	}
	named, err := newSessionNamedObjects(config.NamedObjects)
	if err != nil {
		return nil, err
	}
	transactions := &Transactions{
		manager: config.Manager, settings: config.Settings, options: config.Options, advisory: config.Advisory,
		gate: make(chan struct{}, 1), cleanupDone: make(chan struct{}),
		sequences:     sequences,
		named:         named,
		temporalClock: config.TemporalClock,
		randomState:   [2]uint64{binary.LittleEndian.Uint64(randomSeed[:8]), binary.LittleEndian.Uint64(randomSeed[8:])},
	}
	transactions.typeRuntime = config.TypeRuntime
	if keyed, ok := transactions.options.KeyEncoder.(*builtin.KeyEncoder); ok {
		transactions.options.KeyEncoder = keyed.WithTypes(&sessionTypeResolver{owner: transactions})
	}
	transactions.life, transactions.cancel = context.WithCancelCause(ctx)
	context.AfterFunc(transactions.life, func() {
		defer close(transactions.cleanupDone)
		transactions.gate <- struct{}{}
		defer func() { <-transactions.gate }()
		transactions.closed = true
		transactions.chain = false
		transactions.closeError = errors.Join(transactions.closeActive(context.WithoutCancel(transactions.life)), transactions.closeNamedObjects())
		if transactions.advisory.Manager != nil {
			transactions.advisory.Manager.CloseSession(transactions.advisory.SessionID)
		}
	})
	return transactions, nil
}

func (transactions *Transactions) RandomUint64(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	transactions.randomMu.Lock()
	defer transactions.randomMu.Unlock()
	left, right := transactions.randomState[0], transactions.randomState[1]
	if left|right == 0 {
		right = 1
	}
	result := bits.RotateLeft64(left*5, 7) * 9
	right ^= left
	transactions.randomState[0] = bits.RotateLeft64(left, 24) ^ right ^ right<<16
	transactions.randomState[1] = bits.RotateLeft64(right, 37)
	return result, nil
}

func (transactions *Transactions) SeedRandom(ctx context.Context, seed float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if math.IsNaN(seed) || seed < -1 || seed > 1 {
		return &model.Error{Code: "22023", Message: "setseed parameter must be between -1 and 1"}
	}
	value := math.Float64bits(seed)
	next := func() uint64 {
		value += 0x9e3779b97f4a7c15
		mixed := value
		mixed = (mixed ^ mixed>>30) * 0xbf58476d1ce4e5b9
		mixed = (mixed ^ mixed>>27) * 0x94d049bb133111eb
		return mixed ^ mixed>>31
	}
	transactions.randomMu.Lock()
	transactions.randomState = [2]uint64{next(), next()}
	transactions.randomMu.Unlock()
	return nil
}

func sessionTransactionError(err error) error {
	var state *model.Error
	if errors.As(err, &state) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &model.Error{Code: "57014", Message: "session transaction operation canceled", Cause: err}
	}
	return err
}

func (transactions *Transactions) enter(ctx context.Context) (context.Context, func(), error) {
	return transactions.enterOperation(ctx, false)
}

func (transactions *Transactions) enterOperation(ctx context.Context, recovery bool) (context.Context, func(), error) {
	if token, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation); ok && token.owner == transactions && token.active.Load() {
		if _, allowed := transactions.routineReentry(ctx); allowed && !recovery {
			return ctx, func() {}, nil
		}
		return nil, nil, errSessionTransactionReentry
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, sessionTransactionError(err)
	}
	select {
	case transactions.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, sessionTransactionError(ctx.Err())
	}
	transactions.idleTimer.stop()
	transactions.idleTimer = nil
	if err := ctx.Err(); err != nil {
		<-transactions.gate
		return nil, nil, sessionTransactionError(err)
	}
	if !recovery {
		if err := context.Cause(transactions.life); err != nil {
			<-transactions.gate
			return nil, nil, sessionTransactionError(err)
		}
		if transactions.closed {
			<-transactions.gate
			return nil, nil, storage.ErrClosed
		}
		if transactions.indexSubmission != nil {
			<-transactions.gate
			return nil, nil, errSessionIndexPending
		}
		if transactions.fence != nil && !transactions.active {
			<-transactions.gate
			return nil, nil, transactions.fence
		}
	}
	token := &sessionTransactionOperation{owner: transactions}
	token.active.Store(true)
	operation := context.WithValue(ctx, sessionTransactionKey{}, token)
	if recovery && transactions.closed {
		return operation, func() {
			token.active.Store(false)
			<-transactions.gate
		}, nil
	}
	operation, cancel := context.WithCancelCause(operation)
	joined := make(chan struct{})
	stop := context.AfterFunc(transactions.life, func() {
		cancel(context.Cause(transactions.life))
		close(joined)
	})
	moment := readSQLMoment(operation, transactions.temporalClock)
	transactionMoment := transactions.temporal.transaction
	if !transactions.active {
		transactionMoment = moment
	}
	transactions.temporal = temporalFrame{clock: transactions.temporalClock, transaction: transactionMoment, statement: moment}
	statementCancel := func() {}
	if timeout := transactions.settings.Resources().StatementTimeout; timeout > 0 && !recovery {
		operation, statementCancel = context.WithTimeout(operation, timeout)
	}
	return operation, func() {
		statementCancel()
		if transactions.active && !transactions.closed && context.Cause(transactions.life) == nil {
			transactions.idleTimer = transactions.startTimeout(transactions.settings.Resources().IdleInTransactionTimeout, "25P03", "terminating connection due to idle-in-transaction timeout")
		}
		token.active.Store(false)
		if stop() {
			close(joined)
		}
		<-joined
		cancel(context.Canceled)
		<-transactions.gate
	}, nil
}

func (transactions *Transactions) Active() bool {
	transactions.gate <- struct{}{}
	defer func() { <-transactions.gate }()
	return transactions.active
}

// ActiveIn is Active for callers that may run inside one of this session's
// own operations, such as statement rewrite callbacks during CREATE TABLE AS or
// routine bodies. That operation holds the gate and waits on the callback, so
// Active would deadlock there; the holder's state is read directly instead.
func (transactions *Transactions) ActiveIn(ctx context.Context) bool {
	if token, ok := ctx.Value(sessionTransactionKey{}).(*sessionTransactionOperation); ok && token.owner == transactions && token.active.Load() {
		return transactions.active
	}
	return transactions.Active()
}

func (transactions *Transactions) Failed() bool {
	transactions.gate <- struct{}{}
	defer func() { <-transactions.gate }()
	return transactions.failed()
}

func (transactions *Transactions) failed() bool {
	return transactions.active && (transactions.failure != nil || transactions.fence != nil || transactions.transaction == nil || transactions.transaction.State() != txn.Active)
}

func (transactions *Transactions) MarkFailed(err error) {
	transactions.gate <- struct{}{}
	defer func() { <-transactions.gate }()
	transactions.markFailed(err)
}

func (transactions *Transactions) markFailed(err error) {
	if transactions.active && transactions.failure == nil && err != nil {
		transactions.failure = err
	}
}

func (transactions *Transactions) fail(err error) error {
	transactions.markFailed(err)
	return err
}

func (transactions *Transactions) failedError() error {
	return &model.Error{
		Code: "25P02", Message: "current transaction is aborted, commands ignored until end of transaction block",
		Cause: errors.Join(transactions.failure, transactions.fence),
	}
}

func (transactions *Transactions) ordinary() error {
	if !transactions.active {
		return settingError("25P01", "no active transaction")
	}
	if transactions.transaction == nil || !transactions.settings.InTransaction() {
		return transactions.abortPair(errSessionTransactionState)
	}
	if transactions.transaction.State() == txn.Indeterminate {
		return storage.ErrCommitIndeterminate
	}
	if transactions.failed() {
		return transactions.failedError()
	}
	if transactions.sequences.blocked() {
		return errSequencePublicationPending
	}
	return nil
}

func (transactions *Transactions) Begin(ctx context.Context, start TransactionStart) error {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return transactions.begin(operation, start)
}

func (transactions *Transactions) begin(ctx context.Context, start TransactionStart) error {
	if transactions.active {
		if err := transactions.ordinary(); err != nil {
			return err
		}
		return transactions.fail(settingError("25001", "a transaction is already active"))
	}
	if transactions.sequences.blocked() {
		return errSequencePublicationPending
	}
	if transactions.fence != nil {
		return transactions.fence
	}
	if err := ctx.Err(); err != nil {
		return sessionTransactionError(err)
	}
	if start.Historical < 0 {
		return settingError("22023", "historical transaction timestamp must be positive")
	}
	if transactions.settings.InTransaction() {
		transactions.fence = errSessionTransactionState
		return transactions.fence
	}
	if transactions.advisory.Manager != nil && transactions.generation == ^uint64(0) {
		return &model.Error{Code: "54000", Message: "session transaction generation exhausted"}
	}
	characteristics := start.Characteristics
	if start.Historical > 0 {
		readOnly := true
		characteristics.ReadOnly = &readOnly
	}
	staged := *transactions.settings
	if err := staged.Begin(characteristics); err != nil {
		return err
	}
	options := transactions.options
	// A transaction retains what every statement in it wrote: normalized
	// mutations, admitted target metadata and registered scan plans all live
	// until the transaction ends. The session's work memory is the memory it
	// already grants one statement, so the transaction budget follows it
	// upward; a fixed process-wide default smaller than one statement's grant
	// rejects a long transaction while it is still well inside the session's
	// declared memory. The composition's configured budget stays the floor so a
	// deliberately small work_mem cannot make transactions unusable.
	if memory := uint64(transactions.settings.Resources().EffectiveWorkMemoryBytes); memory > options.MemoryBytes {
		options.MemoryBytes = memory
	}
	// Writes to session-temporary relations resolve their row normalizer from
	// the session: a temporary descriptor is never published, so the durable
	// policy chain cannot rebuild it.
	if len(transactions.named.temporary) != 0 {
		options.Resolver = temporaryMutationResolver{named: &transactions.named, base: options.Resolver}
	}
	options.Mode, options.Timestamp = txn.ReadWrite, 0
	if staged.TransactionPolicy().ReadOnly || start.ImplicitReadOnly {
		options.Mode = txn.ReadOnly
	}
	if start.Historical > 0 {
		options.Mode, options.Timestamp = txn.HistoricalReadOnly, start.Historical
	}
	transaction, err := transactions.manager.Begin(ctx, options)
	if err == nil && transaction == nil {
		err = errSessionTransactionState
	}
	if err != nil {
		if transaction != nil {
			err = errors.Join(err, transaction.Close(context.WithoutCancel(ctx)))
		}
		return err
	}
	*transactions.settings = staged
	transactions.active, transactions.transaction = true, transaction
	transactions.generation++
	clear(transactions.advisoryMarks)
	transactions.advisoryMarks = transactions.advisoryMarks[:0]
	transactions.temporal.transaction = transactions.temporal.statement
	transactions.historical = start.Historical
	transactions.failure = nil
	clear(transactions.deferredTriggers)
	transactions.deferredTriggers = transactions.deferredTriggers[:0]
	clear(transactions.deferredMarks)
	transactions.deferredMarks = transactions.deferredMarks[:0]
	clear(transactions.deferredConstraints)
	transactions.deferredConstraints = transactions.deferredConstraints[:0]
	clear(transactions.deferredConstraintMarks)
	transactions.deferredConstraintMarks = transactions.deferredConstraintMarks[:0]
	transactions.deferredConstraintBytes = 0
	transactions.constraintModes = constraintModeState{}
	clear(transactions.constraintSavepoints)
	transactions.constraintSavepoints = transactions.constraintSavepoints[:0]
	transactions.transactionTimer = transactions.startTimeout(staged.Resources().TransactionTimeout, "25P04", "terminating connection due to transaction timeout")
	return nil
}

func (transactions *Transactions) characteristics() transactionCharacteristics {
	return transactionCharacteristics{
		isolation:  transactions.settings.current[transactionIsolation].text,
		readOnly:   transactions.settings.current[transactionReadOnly].flag,
		deferrable: transactions.settings.current[transactionDeferrable].flag,
	}
}

func (transactions *Transactions) Commit(ctx context.Context, chain bool) (EndResult, error) {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return EndResult{}, err
	}
	defer finish()
	return transactions.commit(operation, chain)
}

func (transactions *Transactions) commit(ctx context.Context, chain bool) (EndResult, error) {
	if !transactions.active {
		return EndResult{}, settingError("25P01", "no active transaction")
	}
	if transactions.transaction == nil || !transactions.settings.InTransaction() {
		return EndResult{}, transactions.abortPair(errSessionTransactionState)
	}
	if transactions.transaction.State() == txn.Indeterminate {
		return EndResult{}, storage.ErrCommitIndeterminate
	}
	modes := transactions.characteristics()
	if transactions.failed() && transactions.transaction.State() != txn.Committed {
		err := transactions.endRollback(ctx)
		result := EndResult{}
		if !transactions.active && chain {
			chainErr := transactions.begin(ctx, modes.start())
			result.Chained = chainErr == nil
			err = errors.Join(err, chainErr)
		}
		return result, err
	}
	if err := transactions.prepareCursorsForCommit(ctx); err != nil {
		return EndResult{}, transactions.fail(err)
	}
	if err := transactions.flushDeferredEvents(ctx, nil, true); err != nil {
		return EndResult{}, transactions.fail(err)
	}
	if err := transactions.applyTemporaryCommitActions(ctx); err != nil {
		return EndResult{}, transactions.fail(err)
	}
	applied, err := transactions.transaction.Commit(ctx)
	result := EndResult{Result: applied}
	switch transactions.transaction.State() {
	case txn.Committed:
		result.Committed = true
		err = errors.Join(err, transactions.finishSettings(true))
	case txn.Aborted:
		err = errors.Join(err, transactions.finishSettings(false))
	case txn.Indeterminate:
		transactions.chain, transactions.chainModes = chain, modes
		return result, errors.Join(storage.ErrCommitIndeterminate, err)
	default:
		if err == nil {
			err = errSessionTransactionState
		}
		return result, transactions.fail(err)
	}
	if chain {
		chainErr := transactions.begin(ctx, modes.start())
		result.Chained = chainErr == nil
		err = errors.Join(err, chainErr)
	}
	return result, err
}

func (transactions *Transactions) Rollback(ctx context.Context, chain bool) error {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return transactions.rollback(operation, chain)
}

func (transactions *Transactions) rollback(ctx context.Context, chain bool) error {
	if !transactions.active {
		return settingError("25P01", "no active transaction")
	}
	modes := transactions.characteristics()
	err := transactions.endRollback(ctx)
	if !transactions.active && chain {
		err = errors.Join(err, transactions.begin(ctx, modes.start()))
	}
	return err
}

func (transactions *Transactions) endRollback(ctx context.Context) error {
	if transactions.transaction == nil {
		return transactions.abortPair(errSessionTransactionState)
	}
	if transactions.transaction.State() == txn.Indeterminate {
		return storage.ErrCommitIndeterminate
	}
	if !transactions.settings.InTransaction() {
		return transactions.abortPair(errSessionTransactionState)
	}
	// Once accepted, rollback must join cleanup even if the command is canceled.
	err := errors.Join(transactions.finishTransactionCursors(false), transactions.transaction.Close(context.WithoutCancel(ctx)))
	switch transactions.transaction.State() {
	case txn.Aborted:
		return errors.Join(err, transactions.finishSettings(false))
	case txn.Committed:
		return errors.Join(settingError("25000", "cannot roll back a committed transaction"), err, transactions.finishSettings(true))
	case txn.Indeterminate:
		return errors.Join(storage.ErrCommitIndeterminate, err)
	default:
		return transactions.fail(errors.Join(errSessionTransactionState, err))
	}
}

func (transactions *Transactions) finishSettings(committed bool) error {
	transactions.transactionTimer.stop()
	transactions.transactionTimer = nil
	if transactions.advisory.Manager != nil {
		transactions.advisory.Manager.ReleaseTransactionLocks(transactions.currentAdvisoryOwner())
	}
	err := transactions.finishTransactionCursors(committed)
	if committed {
		err = errors.Join(err, transactions.settings.Commit())
	} else {
		err = errors.Join(err, transactions.settings.Rollback())
	}
	transactions.named.finishTemporary(committed)
	if err != nil {
		transactions.fence = errors.Join(transactions.fence, errSessionTransactionState, err)
	}
	transactions.active, transactions.transaction = false, nil
	transactions.historical = 0
	transactions.temporal.transaction = sqlMoment{}
	transactions.failure = nil
	clear(transactions.savepoints)
	transactions.savepoints = transactions.savepoints[:0]
	clear(transactions.deferredTriggers)
	clear(transactions.advisoryMarks)
	transactions.advisoryMarks = transactions.advisoryMarks[:0]
	transactions.deferredTriggers = transactions.deferredTriggers[:0]
	clear(transactions.deferredMarks)
	transactions.deferredMarks = transactions.deferredMarks[:0]
	clear(transactions.deferredConstraints)
	transactions.deferredConstraints = transactions.deferredConstraints[:0]
	clear(transactions.deferredConstraintMarks)
	transactions.deferredConstraintMarks = transactions.deferredConstraintMarks[:0]
	transactions.deferredConstraintBytes = 0
	transactions.constraintModes = constraintModeState{}
	clear(transactions.constraintSavepoints)
	transactions.constraintSavepoints = transactions.constraintSavepoints[:0]
	clear(transactions.cursorSavepoints)
	transactions.cursorSavepoints = transactions.cursorSavepoints[:0]
	transactions.chain, transactions.chainModes = false, transactionCharacteristics{}
	transactions.sequences.finish(committed)
	err = errors.Join(err, transactions.sequences.clean(context.WithoutCancel(transactions.life)))
	return err
}

// A failed second half cannot leave a partly coordinated transaction usable.
func (transactions *Transactions) abortPair(err error) error {
	transactions.fence = errors.Join(transactions.fence, err)
	transactions.markFailed(err)
	if transactions.transaction != nil {
		err = errors.Join(err, transactions.transaction.Close(context.WithoutCancel(transactions.life)))
	}
	return err
}

func (transactions *Transactions) Savepoint(ctx context.Context, name string) error {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return transactions.savepoint(operation, name)
}

func (transactions *Transactions) savepoint(ctx context.Context, name string) error {
	if err := transactions.ordinary(); err != nil {
		return err
	}
	if len(transactions.savepoints) != len(transactions.settings.savepoints) || len(transactions.savepoints) != len(transactions.advisoryMarks) || len(transactions.savepoints) != len(transactions.deferredMarks) || len(transactions.savepoints) != len(transactions.deferredConstraintMarks) || len(transactions.savepoints) != len(transactions.constraintSavepoints) || len(transactions.savepoints) != len(transactions.cursorSavepoints) || len(transactions.savepoints) != len(transactions.named.temporaryMarks) {
		return transactions.abortPair(errSessionTransactionState)
	}
	if err := transactions.settings.Savepoint(name); err != nil {
		return transactions.fail(err)
	}
	mark, err := transactions.transaction.Savepoint(ctx)
	if err != nil {
		if undoErr := transactions.settings.Release(name); undoErr != nil {
			return transactions.abortPair(errors.Join(err, undoErr))
		}
		return transactions.fail(err)
	}
	advisoryMark := locks.AdvisorySavepoint{}
	if transactions.advisory.Manager != nil {
		advisoryMark, err = transactions.advisory.Manager.TransactionSavepoint(transactions.currentAdvisoryOwner())
		if err != nil {
			undoErr := errors.Join(transactions.transaction.ReleaseSavepoint(ctx, mark), transactions.settings.Release(name))
			if undoErr != nil {
				return transactions.abortPair(errors.Join(err, undoErr))
			}
			return transactions.fail(err)
		}
	}
	transactions.savepoints = append(transactions.savepoints, mark)
	transactions.advisoryMarks = append(transactions.advisoryMarks, advisoryMark)
	transactions.deferredMarks = append(transactions.deferredMarks, len(transactions.deferredTriggers))
	transactions.deferredConstraintMarks = append(transactions.deferredConstraintMarks, len(transactions.deferredConstraints))
	transactions.constraintSavepoints = append(transactions.constraintSavepoints, cloneConstraintModes(transactions.constraintModes))
	transactions.cursorSavepoints = append(transactions.cursorSavepoints, transactions.named.cursorSerial)
	transactions.named.temporaryMarks = append(transactions.named.temporaryMarks, len(transactions.named.temporaryChanges))
	if transactions.sequences != nil {
		transactions.sequences.marks = append(transactions.sequences.marks, len(transactions.sequences.changes))
	}
	return nil
}

func (transactions *Transactions) namedSavepoint(name string) (int, error) {
	if name == "" || !transactions.settings.validString(name) {
		return 0, settingError("22023", "invalid savepoint name")
	}
	index, err := transactions.settings.savepointIndex(name)
	if err != nil {
		return 0, err
	}
	if len(transactions.savepoints) != len(transactions.settings.savepoints) || len(transactions.savepoints) != len(transactions.advisoryMarks) || len(transactions.savepoints) != len(transactions.deferredMarks) || len(transactions.savepoints) != len(transactions.deferredConstraintMarks) || len(transactions.savepoints) != len(transactions.constraintSavepoints) || len(transactions.savepoints) != len(transactions.cursorSavepoints) || len(transactions.savepoints) != len(transactions.named.temporaryMarks) || index >= len(transactions.savepoints) {
		return 0, transactions.abortPair(errSessionTransactionState)
	}
	return index, nil
}

func (transactions *Transactions) RollbackTo(ctx context.Context, name string) error {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return transactions.rollbackTo(operation, name)
}

func (transactions *Transactions) rollbackTo(ctx context.Context, name string) error {
	if !transactions.active {
		return settingError("25P01", "no active transaction")
	}
	if transactions.transaction == nil || !transactions.settings.InTransaction() {
		return transactions.abortPair(errSessionTransactionState)
	}
	if transactions.transaction.State() == txn.Indeterminate {
		return storage.ErrCommitIndeterminate
	}
	if transactions.fence != nil || transactions.transaction.State() != txn.Active {
		return transactions.failedError()
	}
	index, err := transactions.namedSavepoint(name)
	if err != nil {
		return err
	}
	if transactions.deferredMarks[index] < 0 || transactions.deferredMarks[index] > len(transactions.deferredTriggers) || transactions.deferredConstraintMarks[index] < 0 || transactions.deferredConstraintMarks[index] > len(transactions.deferredConstraints) {
		return transactions.abortPair(errSessionTransactionState)
	}
	if err := transactions.transaction.RollbackTo(ctx, transactions.savepoints[index]); err != nil {
		return err
	}
	if transactions.advisory.Manager != nil {
		if err := transactions.advisory.Manager.RollbackTransactionLocks(transactions.currentAdvisoryOwner(), transactions.advisoryMarks[index]); err != nil {
			return transactions.abortPair(err)
		}
	}
	if err := transactions.settings.RollbackTo(name); err != nil {
		return transactions.abortPair(err)
	}
	var cleanupErr error
	if transactions.sequences != nil {
		transactions.sequences.abandon(transactions.sequences.marks[index])
		transactions.sequences.marks = transactions.sequences.marks[:index+1]
		cleanupErr = transactions.sequences.clean(context.WithoutCancel(ctx))
	}
	if index >= len(transactions.named.temporaryMarks) {
		return transactions.abortPair(errSessionTransactionState)
	}
	transactions.named.rollbackTemporary(transactions.named.temporaryMarks[index])
	clear(transactions.named.temporaryMarks[index+1:])
	transactions.named.temporaryMarks = transactions.named.temporaryMarks[:index+1]
	if index >= len(transactions.deferredMarks) {
		return transactions.abortPair(errSessionTransactionState)
	}
	clear(transactions.deferredTriggers[transactions.deferredMarks[index]:])
	transactions.deferredTriggers = transactions.deferredTriggers[:transactions.deferredMarks[index]]
	clear(transactions.deferredMarks[index+1:])
	transactions.deferredMarks = transactions.deferredMarks[:index+1]
	if index >= len(transactions.deferredConstraintMarks) {
		return transactions.abortPair(errSessionTransactionState)
	}
	clear(transactions.deferredConstraints[transactions.deferredConstraintMarks[index]:])
	transactions.deferredConstraints = transactions.deferredConstraints[:transactions.deferredConstraintMarks[index]]
	clear(transactions.deferredConstraintMarks[index+1:])
	transactions.deferredConstraintMarks = transactions.deferredConstraintMarks[:index+1]
	transactions.deferredConstraintBytes = 0
	for _, pending := range transactions.deferredConstraints {
		if ^uint64(0)-transactions.deferredConstraintBytes < pending.bytes {
			return transactions.abortPair(errSessionTransactionState)
		}
		transactions.deferredConstraintBytes += pending.bytes
	}
	if index >= len(transactions.cursorSavepoints) {
		return transactions.abortPair(errSessionTransactionState)
	}
	cleanupErr = errors.Join(cleanupErr, transactions.rollbackCursorsTo(transactions.cursorSavepoints[index]))
	clear(transactions.cursorSavepoints[index+1:])
	transactions.cursorSavepoints = transactions.cursorSavepoints[:index+1]
	if index >= len(transactions.constraintSavepoints) {
		return transactions.abortPair(errSessionTransactionState)
	}
	transactions.constraintModes = cloneConstraintModes(transactions.constraintSavepoints[index])
	clear(transactions.constraintSavepoints[index+1:])
	transactions.constraintSavepoints = transactions.constraintSavepoints[:index+1]
	clear(transactions.savepoints[index+1:])
	transactions.savepoints = transactions.savepoints[:index+1]
	clear(transactions.advisoryMarks[index+1:])
	transactions.advisoryMarks = transactions.advisoryMarks[:index+1]
	transactions.failure = nil
	return cleanupErr
}

func (transactions *Transactions) Release(ctx context.Context, name string) error {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return transactions.release(operation, name)
}

func (transactions *Transactions) release(ctx context.Context, name string) error {
	if err := transactions.ordinary(); err != nil {
		return err
	}
	index, err := transactions.namedSavepoint(name)
	if err != nil {
		return transactions.fail(err)
	}
	if err := transactions.transaction.ReleaseSavepoint(ctx, transactions.savepoints[index]); err != nil {
		return transactions.fail(err)
	}
	if err := transactions.settings.Release(name); err != nil {
		return transactions.abortPair(err)
	}
	clear(transactions.savepoints[index:])
	transactions.savepoints = transactions.savepoints[:index]
	clear(transactions.advisoryMarks[index:])
	transactions.advisoryMarks = transactions.advisoryMarks[:index]
	if index > len(transactions.deferredMarks) {
		return transactions.abortPair(errSessionTransactionState)
	}
	clear(transactions.deferredMarks[index:])
	transactions.deferredMarks = transactions.deferredMarks[:index]
	if index > len(transactions.deferredConstraintMarks) {
		return transactions.abortPair(errSessionTransactionState)
	}
	clear(transactions.deferredConstraintMarks[index:])
	transactions.deferredConstraintMarks = transactions.deferredConstraintMarks[:index]
	if index > len(transactions.constraintSavepoints) {
		return transactions.abortPair(errSessionTransactionState)
	}
	clear(transactions.constraintSavepoints[index:])
	transactions.constraintSavepoints = transactions.constraintSavepoints[:index]
	if index > len(transactions.cursorSavepoints) {
		return transactions.abortPair(errSessionTransactionState)
	}
	clear(transactions.cursorSavepoints[index:])
	transactions.cursorSavepoints = transactions.cursorSavepoints[:index]
	if index > len(transactions.named.temporaryMarks) {
		return transactions.abortPair(errSessionTransactionState)
	}
	clear(transactions.named.temporaryMarks[index:])
	transactions.named.temporaryMarks = transactions.named.temporaryMarks[:index]
	if transactions.sequences != nil {
		transactions.sequences.marks = transactions.sequences.marks[:index]
	}
	return nil
}

func (transactions *Transactions) markDataAccess() error {
	if !transactions.active || transactions.transaction == nil {
		return nil
	}
	if transactions.transaction.HasDataAccess() {
		if err := transactions.settings.MarkDataAccess(); err != nil {
			return transactions.abortPair(err)
		}
	}
	return nil
}

func (transactions *Transactions) ReadView(ctx context.Context, dependencies []txn.ReadDependency) (txn.ReadView, error) {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer finish()
	if err := transactions.ordinary(); err != nil {
		return nil, err
	}
	view, err := transactions.transaction.ReadView(operation, dependencies)
	if accessErr := transactions.markDataAccess(); accessErr != nil {
		if view != nil {
			accessErr = errors.Join(accessErr, view.Close())
		}
		return nil, transactions.fail(errors.Join(err, accessErr))
	}
	if err != nil {
		return nil, transactions.fail(err)
	}
	return view, nil
}

// Statement lends the active transaction only for the synchronous callback.
// The callback must use its supplied context and must not retain the capability.
func (transactions *Transactions) Statement(ctx context.Context, fn func(context.Context, *txn.Transaction) error) error {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	if err := transactions.ordinary(); err != nil {
		return err
	}
	if fn == nil {
		return transactions.fail(errSessionStatement)
	}
	completed := false
	defer func() {
		if !completed {
			transactions.markFailed(errors.Join(errSessionStatementPanic, transactions.markDataAccess()))
		}
	}()
	err = transactions.transaction.Statement(operation, func(ctx context.Context) error {
		return fn(ctx, transactions.transaction)
	})
	err = errors.Join(err, transactions.markDataAccess())
	completed = true
	return transactions.fail(err)
}

// Resolve remains available after Close when local ownership is retained for
// an unknown commit. It never begins a chained transaction on a closed session.
func (transactions *Transactions) Resolve(ctx context.Context) (storage.CommitResolution, error) {
	operation, finish, err := transactions.enterOperation(ctx, true)
	if err != nil {
		return storage.CommitResolution{}, err
	}
	defer finish()
	if transactions.indexSubmission != nil {
		resolution, err := transactions.resolveIndex(operation)
		if transactions.closed {
			transactions.closeError = err
		}
		return resolution, err
	}
	if !transactions.active {
		if transactions.closed {
			return storage.CommitResolution{}, storage.ErrClosed
		}
		return storage.CommitResolution{}, settingError("25P01", "no active transaction")
	}
	resolution, err := transactions.resolve(operation)
	if transactions.closed {
		transactions.closeError = err
	}
	return resolution, err
}

func (transactions *Transactions) resolve(ctx context.Context) (storage.CommitResolution, error) {
	if transactions.transaction == nil {
		return storage.CommitResolution{}, transactions.abortPair(errSessionTransactionState)
	}
	chain, modes := transactions.chain, transactions.chainModes
	resolution, err := transactions.transaction.Resolve(ctx)
	switch transactions.transaction.State() {
	case txn.Committed:
		err = errors.Join(err, transactions.finishSettings(true))
	case txn.Aborted:
		err = errors.Join(err, transactions.finishSettings(false))
	case txn.Indeterminate:
		return resolution, errors.Join(storage.ErrCommitIndeterminate, err)
	default:
		return resolution, err
	}
	if chain && !transactions.closed {
		err = errors.Join(err, transactions.begin(ctx, modes.start()))
	}
	return resolution, err
}

func (transactions *Transactions) closeActive(ctx context.Context) error {
	transactions.idleTimer.stop()
	transactions.idleTimer = nil
	if transactions.indexSubmission != nil {
		return errors.Join(transactions.fence, transactions.closeIndex(ctx))
	}
	if !transactions.active {
		var temporaryErr error
		if len(transactions.named.temporary) != 0 {
			if temporaryErr = transactions.begin(ctx, TransactionStart{}); temporaryErr == nil {
				temporaryErr = transactions.transaction.Statement(ctx, func(statement context.Context) error {
					return transactions.discardSessionTemporary(statement)
				})
				if temporaryErr == nil {
					_, temporaryErr = transactions.commit(ctx, false)
				} else {
					temporaryErr = errors.Join(temporaryErr, transactions.endRollback(ctx))
				}
			}
		}
		return errors.Join(transactions.fence, temporaryErr, transactions.sequences.clean(ctx))
	}
	if transactions.transaction == nil {
		return transactions.abortPair(errSessionTransactionState)
	}
	switch transactions.transaction.State() {
	case txn.Indeterminate, txn.Committed:
		_, err := transactions.resolve(ctx)
		if !transactions.active && len(transactions.named.temporary) != 0 {
			err = errors.Join(err, transactions.closeActive(ctx))
		}
		return err
	default:
		err := transactions.endRollback(ctx)
		if !transactions.active && len(transactions.named.temporary) != 0 {
			err = errors.Join(err, transactions.closeActive(ctx))
		}
		return err
	}
}

func (transactions *Transactions) Close() error {
	transactions.cancel(storage.ErrClosed)
	<-transactions.cleanupDone
	transactions.gate <- struct{}{}
	defer func() { <-transactions.gate }()
	retained := transactions.active || transactions.indexSubmission != nil || transactions.sequences.blocked() || len(transactions.named.temporary) != 0
	if transactions.closeReturned && retained {
		transactions.closeError = transactions.closeActive(context.WithoutCancel(transactions.life))
	}
	transactions.closeReturned = true
	return transactions.closeError
}

type sessionTimeout struct {
	timer *time.Timer
	live  atomic.Bool
}

func (timeout *sessionTimeout) stop() {
	if timeout == nil {
		return
	}
	timeout.live.Store(false)
	timeout.timer.Stop()
}

func (transactions *Transactions) startTimeout(duration time.Duration, code, message string) *sessionTimeout {
	if duration <= 0 {
		return nil
	}
	timeout := &sessionTimeout{}
	timeout.live.Store(true)
	timeout.timer = time.AfterFunc(duration, func() {
		if timeout.live.CompareAndSwap(true, false) {
			transactions.cancel(settingError(code, message))
		}
	})
	return timeout
}
