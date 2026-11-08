package locks

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	ErrAdvisoryLockTimeout = &model.Error{Code: "55P03", Message: "canceling statement due to lock timeout", Cause: context.DeadlineExceeded}
	ErrAdvisoryLockUnavailable = &model.Error{Code: "55P03", Message: "could not obtain advisory lock"}
	errAdvisoryConfig = errors.New("invalid advisory lock manager limits")
	errAdvisoryOwner = errors.New("invalid advisory lock owner")
	errAdvisoryRequest = errors.New("invalid advisory lock request")
	errAdvisorySavepoint = errors.New("invalid advisory lock savepoint")
	errAdvisorySessionClosed = errors.New("advisory lock session closed")
	errAdvisoryKeyCapacity = &model.Error{Code: "53200", Message: "advisory lock key limit exceeded"}
	errAdvisoryOwnerCapacity = &model.Error{Code: "53300", Message: "advisory lock owner limit exceeded"}
	errAdvisoryWaiterCapacity = &model.Error{Code: "53200", Message: "advisory lock waiter limit exceeded"}
	errAdvisoryHoldCapacity = &model.Error{Code: "53200", Message: "advisory lock acquisition limit exceeded"}
	errAdvisoryByteCapacity = &model.Error{Code: "53200", Message: "advisory lock memory limit exceeded"}
)

type AdvisoryLockScope = model.AdvisoryLockScope

const (
	ScopeSession = model.AdvisorySessionScope
	ScopeTransaction = model.AdvisoryTransactionScope
)

type AdvisoryLockMode = model.AdvisoryLockMode

const (
	AdvisoryShared = model.AdvisoryShared
	AdvisoryExclusive = model.AdvisoryExclusive
)

type AdvisoryKey = model.AdvisoryKey

type AdvisoryOwner struct {
	SessionID     uint64
	TransactionID uint64
}

type AdvisoryLimits struct {
	MaxKeys    int
	MaxOwners  int
	MaxWaiters int
	MaxHolds   int
	MaxBytes   uint64
	WaitGraph  *WaitGraph
}

func DefaultAdvisoryLimits() AdvisoryLimits {
	return AdvisoryLimits{MaxKeys: 1 << 16, MaxOwners: 4096, MaxWaiters: 1 << 14, MaxHolds: 1 << 20, MaxBytes: 64 << 20}
}

type advisoryCounts struct {
	sessionShared, sessionExclusive int
	xactShared, xactExclusive       int
}

func (counts *advisoryCounts) empty() bool {
	return counts.sessionShared == 0 && counts.sessionExclusive == 0 && counts.xactShared == 0 && counts.xactExclusive == 0
}

func (counts *advisoryCounts) exclusive() bool {
	return counts.sessionExclusive != 0 || counts.xactExclusive != 0
}

type advisoryLockItem struct {
	holders map[uint64]*advisoryCounts
}

type advisoryAcquisition struct {
	key      AdvisoryKey
	mode     AdvisoryLockMode
	ordinal  uint64
	previous *advisoryAcquisition
}

type advisoryTransaction struct {
	sequence uint64
	head     *advisoryAcquisition
}

type advisoryOwnerState struct {
	held         map[AdvisoryKey]*advisoryCounts
	transactions map[uint64]*advisoryTransaction
	waiting      int
	cancelled    bool
}

type AdvisorySavepoint struct {
	owner    AdvisoryOwner
	ordinal  uint64
}

type AdvisoryLockManager struct {
	mu          sync.Mutex
	limits      AdvisoryLimits
	locks       map[AdvisoryKey]*advisoryLockItem
	owners      map[uint64]*advisoryOwnerState
	waiters     int
	holds       int
	usedBytes   uint64
	changed     chan struct{}
}

const (
	advisoryOwnerBytes       = uint64(unsafe.Sizeof(advisoryOwnerState{})) + 128
	advisoryItemBytes        = uint64(unsafe.Sizeof(advisoryLockItem{})) + 96
	advisoryHolderBytes      = uint64(unsafe.Sizeof(advisoryCounts{})) + 64
	advisoryTransactionBytes = uint64(unsafe.Sizeof(advisoryTransaction{})) + 64
	advisoryAcquisitionBytes = uint64(unsafe.Sizeof(advisoryAcquisition{}))
	advisoryWaiterBytes      uint64 = 64
)

func NewAdvisoryLockManagerWithLimits(limits AdvisoryLimits) (*AdvisoryLockManager, error) {
	minimum := advisoryOwnerBytes + advisoryItemBytes + advisoryHolderBytes
	if limits.MaxKeys <= 0 || limits.MaxOwners <= 0 || limits.MaxWaiters <= 0 || limits.MaxHolds <= 0 || limits.MaxBytes < minimum {
		return nil, errAdvisoryConfig
	}
	if limits.WaitGraph == nil {
		graph, err := NewWaitGraph(DefaultWaitGraphLimits())
		if err != nil {
			return nil, errors.Join(errAdvisoryConfig, err)
		}
		limits.WaitGraph = graph
	}
	return &AdvisoryLockManager{
		limits: limits,
		locks: make(map[AdvisoryKey]*advisoryLockItem),
		owners: make(map[uint64]*advisoryOwnerState),
		changed: make(chan struct{}),
	}, nil
}

func KeyFromBigInt(key int64) AdvisoryKey {
	return AdvisoryKey{Space: model.AdvisoryBigIntKey, Value: uint64(key)}
}

func KeyFromPair(first, second int32) AdvisoryKey {
	return AdvisoryKey{Space: model.AdvisoryIntPairKey, Value: uint64(uint32(first))<<32 | uint64(uint32(second))}
}

func validAdvisoryOwner(owner AdvisoryOwner, scope AdvisoryLockScope) bool {
	return owner.SessionID != 0 && (scope != ScopeTransaction || owner.TransactionID != 0)
}

func validAdvisoryKey(key AdvisoryKey) bool {
	return key.Space == model.AdvisoryBigIntKey || key.Space == model.AdvisoryIntPairKey
}

func (manager *AdvisoryLockManager) Lock(ctx context.Context, owner AdvisoryOwner, key AdvisoryKey, mode AdvisoryLockMode, scope AdvisoryLockScope) error {
	_, err := manager.acquire(ctx, owner, key, mode, scope, true)
	return err
}

func (manager *AdvisoryLockManager) TryLock(owner AdvisoryOwner, key AdvisoryKey, mode AdvisoryLockMode, scope AdvisoryLockScope) (bool, error) {
	return manager.acquire(context.Background(), owner, key, mode, scope, false)
}

func (manager *AdvisoryLockManager) acquire(ctx context.Context, owner AdvisoryOwner, key AdvisoryKey, mode AdvisoryLockMode, scope AdvisoryLockScope, wait bool) (bool, error) {
	if ctx == nil || !validAdvisoryOwner(owner, scope) || !validAdvisoryKey(key) || mode != AdvisoryShared && mode != AdvisoryExclusive || scope != ScopeSession && scope != ScopeTransaction {
		return false, errAdvisoryRequest
	}
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	manager.mu.Lock()
	var waitDuration time.Duration
	waited := false
	defer func() {
		manager.mu.Unlock()
		if waited {
			ReportLockWait(ctx, waitDuration)
		}
	}()
	state := manager.owners[owner.SessionID]
	waiting := false
	defer func() {
		if waiting {
			manager.releaseWaiter(owner.SessionID, state)
		}
		manager.limits.WaitGraph.EndWait(WaitOwner(owner.SessionID))
	}()
	var waitContext context.Context
	var stopWait context.CancelFunc
	var blockers []WaitOwner
	for {
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		if waitContext != nil {
			if err := context.Cause(waitContext); err != nil {
				return false, err
			}
		}
		if state != nil && state.cancelled {
			return false, errAdvisorySessionClosed
		}
		item := manager.locks[key]
		blockers = manager.conflictingOwners(blockers[:0], item, owner.SessionID, mode)
		if len(blockers) == 0 {
			if err := manager.grant(owner, key, mode, scope, state, item); err != nil {
				return false, err
			}
			return true, nil
		}
		if !wait {
			return false, nil
		}
		if !waiting {
			var err error
			state, err = manager.reserveWaiter(owner.SessionID, state)
			if err != nil {
				return false, err
			}
			waiting = true
		}
		if err := manager.limits.WaitGraph.BeginWait(WaitOwner(owner.SessionID), blockers); err != nil {
			return false, err
		}
		if waitContext == nil {
			if timeout, configured := LockTimeout(ctx); configured && timeout > 0 {
				waitContext, stopWait = context.WithTimeoutCause(ctx, timeout, ErrAdvisoryLockTimeout)
				// Registered here, not before the assignment: the cancel function
				// only exists once the wait context does, and this branch runs at
				// most once because waitContext stays non-nil afterwards.
				defer func() { stopWait() }()
			}
		}
		var timeoutDone <-chan struct{}
		if waitContext != nil {
			timeoutDone = waitContext.Done()
		}
		changed := manager.changed
		waited = true
		waitStarted := time.Now()
		manager.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-timeoutDone:
		case <-changed:
		}
		manager.mu.Lock()
		waitDuration += time.Since(waitStarted)
	}
}

func (manager *AdvisoryLockManager) conflictingOwners(dst []WaitOwner, item *advisoryLockItem, sessionID uint64, mode AdvisoryLockMode) []WaitOwner {
	if item == nil {
		return dst
	}
	for holderID, holder := range item.holders {
		if holderID == sessionID || holder.empty() {
			continue
		}
		if mode == AdvisoryExclusive || holder.exclusive() {
			dst = append(dst, WaitOwner(holderID))
		}
	}
	return dst
}

func (manager *AdvisoryLockManager) reserveWaiter(sessionID uint64, state *advisoryOwnerState) (*advisoryOwnerState, error) {
	if manager.waiters == manager.limits.MaxWaiters {
		return state, errAdvisoryWaiterCapacity
	}
	bytes := advisoryWaiterBytes
	if state == nil {
		if len(manager.owners) == manager.limits.MaxOwners {
			return nil, errAdvisoryOwnerCapacity
		}
		bytes += advisoryOwnerBytes
	}
	if bytes > manager.limits.MaxBytes-manager.usedBytes {
		return state, errAdvisoryByteCapacity
	}
	if state == nil {
		state = &advisoryOwnerState{held: make(map[AdvisoryKey]*advisoryCounts), transactions: make(map[uint64]*advisoryTransaction)}
		manager.owners[sessionID] = state
	}
	state.waiting++
	manager.waiters++
	manager.usedBytes += bytes
	return state, nil
}

func (manager *AdvisoryLockManager) releaseWaiter(sessionID uint64, state *advisoryOwnerState) {
	if state == nil || state.waiting == 0 {
		return
	}
	state.waiting--
	manager.waiters--
	manager.usedBytes -= advisoryWaiterBytes
	manager.removeOwnerIfEmpty(sessionID, state)
}

func (manager *AdvisoryLockManager) grant(owner AdvisoryOwner, key AdvisoryKey, mode AdvisoryLockMode, scope AdvisoryLockScope, state *advisoryOwnerState, item *advisoryLockItem) error {
	if manager.holds == manager.limits.MaxHolds {
		return errAdvisoryHoldCapacity
	}
	var bytes uint64
	if state == nil {
		if len(manager.owners) == manager.limits.MaxOwners {
			return errAdvisoryOwnerCapacity
		}
		bytes += advisoryOwnerBytes
	}
	if item == nil {
		if len(manager.locks) == manager.limits.MaxKeys {
			return errAdvisoryKeyCapacity
		}
		bytes += advisoryItemBytes
	}
	var holder *advisoryCounts
	if state != nil {
		holder = state.held[key]
	}
	if holder == nil {
		bytes += advisoryHolderBytes
	}
	var transaction *advisoryTransaction
	if scope == ScopeTransaction && state != nil {
		transaction = state.transactions[owner.TransactionID]
	}
	if transaction != nil && transaction.sequence == math.MaxUint64 {
		return errAdvisoryHoldCapacity
	}
	if scope == ScopeTransaction {
		bytes += advisoryAcquisitionBytes
		if transaction == nil {
			bytes += advisoryTransactionBytes
		}
	}
	if bytes > manager.limits.MaxBytes-manager.usedBytes {
		return errAdvisoryByteCapacity
	}
	if state == nil {
		state = &advisoryOwnerState{held: make(map[AdvisoryKey]*advisoryCounts), transactions: make(map[uint64]*advisoryTransaction)}
		manager.owners[owner.SessionID] = state
	}
	if item == nil {
		item = &advisoryLockItem{holders: make(map[uint64]*advisoryCounts)}
		manager.locks[key] = item
	}
	if holder == nil {
		holder = &advisoryCounts{}
		state.held[key] = holder
		item.holders[owner.SessionID] = holder
	}
	if scope == ScopeSession {
		if mode == AdvisoryExclusive {
			holder.sessionExclusive++
		} else {
			holder.sessionShared++
		}
	} else {
		if transaction == nil {
			transaction = &advisoryTransaction{}
			state.transactions[owner.TransactionID] = transaction
		}
		transaction.sequence++
		transaction.head = &advisoryAcquisition{key: key, mode: mode, ordinal: transaction.sequence, previous: transaction.head}
		if mode == AdvisoryExclusive {
			holder.xactExclusive++
		} else {
			holder.xactShared++
		}
	}
	manager.holds++
	manager.usedBytes += bytes
	return nil
}

func (manager *AdvisoryLockManager) Unlock(owner AdvisoryOwner, key AdvisoryKey, mode AdvisoryLockMode) bool {
	if owner.SessionID == 0 || !validAdvisoryKey(key) || mode != AdvisoryShared && mode != AdvisoryExclusive {
		return false
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.owners[owner.SessionID]
	if state == nil {
		return false
	}
	holder := state.held[key]
	if holder == nil {
		return false
	}
	count := &holder.sessionShared
	if mode == AdvisoryExclusive {
		count = &holder.sessionExclusive
	}
	if *count == 0 {
		return false
	}
	*count--
	manager.holds--
	manager.removeHolderIfEmpty(owner.SessionID, key, state, holder)
	manager.notify()
	return true
}

func (manager *AdvisoryLockManager) TransactionSavepoint(owner AdvisoryOwner) (AdvisorySavepoint, error) {
	if !validAdvisoryOwner(owner, ScopeTransaction) {
		return AdvisorySavepoint{}, errAdvisoryOwner
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	mark := AdvisorySavepoint{owner: owner}
	if state := manager.owners[owner.SessionID]; state != nil {
		if transaction := state.transactions[owner.TransactionID]; transaction != nil {
			mark.ordinal = transaction.sequence
		}
	}
	return mark, nil
}

func (manager *AdvisoryLockManager) RollbackTransactionLocks(owner AdvisoryOwner, mark AdvisorySavepoint) error {
	if !validAdvisoryOwner(owner, ScopeTransaction) || mark.owner != owner {
		return errAdvisorySavepoint
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.owners[owner.SessionID]
	if state == nil {
		if mark.ordinal != 0 {
			return errAdvisorySavepoint
		}
		return nil
	}
	transaction := state.transactions[owner.TransactionID]
	if transaction == nil {
		if mark.ordinal != 0 {
			return errAdvisorySavepoint
		}
		return nil
	}
	if mark.ordinal > transaction.sequence {
		return errAdvisorySavepoint
	}
	changed := false
	for transaction.head != nil && transaction.head.ordinal > mark.ordinal {
		manager.popTransactionAcquisition(owner.SessionID, state, transaction)
		changed = true
	}
	if changed {
		manager.notify()
	}
	return nil
}

func (manager *AdvisoryLockManager) ReleaseTransactionLocks(owner AdvisoryOwner) {
	if !validAdvisoryOwner(owner, ScopeTransaction) {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.owners[owner.SessionID]
	if state == nil {
		return
	}
	transaction := state.transactions[owner.TransactionID]
	if transaction == nil {
		return
	}
	for transaction.head != nil {
		manager.popTransactionAcquisition(owner.SessionID, state, transaction)
	}
	delete(state.transactions, owner.TransactionID)
	manager.usedBytes -= advisoryTransactionBytes
	manager.removeOwnerIfEmpty(owner.SessionID, state)
	manager.notify()
}

func (manager *AdvisoryLockManager) popTransactionAcquisition(sessionID uint64, state *advisoryOwnerState, transaction *advisoryTransaction) {
	acquisition := transaction.head
	transaction.head = acquisition.previous
	holder := state.held[acquisition.key]
	if acquisition.mode == AdvisoryExclusive {
		holder.xactExclusive--
	} else {
		holder.xactShared--
	}
	manager.holds--
	manager.usedBytes -= advisoryAcquisitionBytes
	manager.removeHolderIfEmpty(sessionID, acquisition.key, state, holder)
	*acquisition = advisoryAcquisition{}
}

func (manager *AdvisoryLockManager) UnlockAllSession(sessionID uint64) {
	manager.releaseSession(sessionID, false)
}

// CloseSession cancels its waiters and releases session locks. Transaction
// locks survive an indeterminate commit until ReleaseTransactionLocks resolves it.
func (manager *AdvisoryLockManager) CloseSession(sessionID uint64) {
	manager.releaseSession(sessionID, true)
}

func (manager *AdvisoryLockManager) releaseSession(sessionID uint64, closeSession bool) {
	if sessionID == 0 {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	state := manager.owners[sessionID]
	if state == nil {
		return
	}
	state.cancelled = state.cancelled || closeSession
	changed := false
	for key, holder := range state.held {
		count := holder.sessionShared + holder.sessionExclusive
		if count != 0 {
			holder.sessionShared, holder.sessionExclusive = 0, 0
			manager.holds -= count
			changed = true
		}
		manager.removeHolderIfEmpty(sessionID, key, state, holder)
	}
	manager.removeOwnerIfEmpty(sessionID, state)
	if changed || closeSession {
		manager.notify()
	}
}

func (manager *AdvisoryLockManager) removeHolderIfEmpty(sessionID uint64, key AdvisoryKey, state *advisoryOwnerState, holder *advisoryCounts) {
	if !holder.empty() {
		return
	}
	delete(state.held, key)
	item := manager.locks[key]
	if item != nil {
		delete(item.holders, sessionID)
		if len(item.holders) == 0 {
			delete(manager.locks, key)
			manager.usedBytes -= advisoryItemBytes
		}
	}
	manager.usedBytes -= advisoryHolderBytes
}

func (manager *AdvisoryLockManager) removeOwnerIfEmpty(sessionID uint64, state *advisoryOwnerState) {
	if state.waiting != 0 || len(state.held) != 0 || len(state.transactions) != 0 {
		return
	}
	delete(manager.owners, sessionID)
	manager.usedBytes -= advisoryOwnerBytes
}

func (manager *AdvisoryLockManager) notify() {
	close(manager.changed)
	manager.changed = make(chan struct{})
}

type lockTimeoutKey struct{}

// WithLockTimeout bounds time blocked on each lock acquisition. The monotonic
// timer starts at the first conflict; zero disables an inherited timeout.
func WithLockTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return context.WithValue(ctx, lockTimeoutKey{}, timeout)
}

func LockTimeout(ctx context.Context) (time.Duration, bool) {
	if ctx == nil {
		return 0, false
	}
	timeout, configured := ctx.Value(lockTimeoutKey{}).(time.Duration)
	return timeout, configured
}
