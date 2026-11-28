package txn

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"
	"time"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var (
	errLockConfig        = errors.New("invalid lock registry limits")
	errLockRequest       = errors.New("invalid lock request")
	errLockOwner         = errors.New("invalid lock owner")
	errLockOwnerState    = errors.New("lock owner is not active")
	errLockOwnerBusy     = errors.New("lock owner has an acquisition in progress")
	errLockPosition      = errors.New("invalid lock savepoint")
	errLockOwnerCapacity = &model.Error{Code: "53300", Message: "lock owner limit exceeded"}
	errLockCapacity      = &model.Error{Code: "53200", Message: "lock registry capacity exceeded"}
	errLockSequence      = &model.Error{Code: "54000", Message: "lock registry sequence exhausted"}
	errLockUnavailable   = &model.Error{Code: "55P03", Message: "could not obtain lock"}
	errLockWounded       = &model.Error{Code: "40001", Message: "transaction wounded by an older lock waiter"}
	ErrLockTimeout       = &model.Error{Code: "55P03", Message: "canceling statement due to lock timeout", Cause: context.DeadlineExceeded}
)

type lockMode uint8

const (
	lockKeyShare lockMode = iota
	lockShare
	lockNoKeyUpdate
	lockUpdate
	lockSchema
	lockIntent
	lockTableRowShare
	lockTableRowExclusive
	lockTableShareUpdateExclusive
	lockTableShare
	lockTableShareRowExclusive
	lockTableExclusive
)

type lockWaitPolicy uint8

const (
	lockWait lockWaitPolicy = iota
	lockNoWait
	lockSkip
)

// WithLockTimeout bounds only time spent blocked on a lock. The monotonic
// timeout starts at the first conflict; zero disables a timeout inherited from
// an outer request context.
func WithLockTimeout(ctx context.Context, timeout time.Duration) context.Context {
	return locks.WithLockTimeout(ctx, timeout)
}

// Finite empty keys are distinct from unbounded ends. Encoded keys must share
// one order-preserving encoding within an object generation.
type lockBound struct {
	Key       []byte
	Inclusive bool
	// Prefix endpoints address every extension of a partial composite key.
	Prefix    bool
	Unbounded bool
}

// A point has equal inclusive bounds; a table has two unbounded ends. Gaps and
// ranges use the same interval representation, even when no rows exist.
type lockRequest struct {
	ObjectID     model.ObjectID
	Generation   uint64
	Lower, Upper lockBound
}

type lockOwnerState uint8

const (
	lockOwnerActive lockOwnerState = iota
	lockOwnerAborting
	lockOwnerCommitting
	lockOwnerEnded
)

type lockOwner struct {
	registry  *lockRegistry
	id        model.TxnID
	age       uint64
	ctx       context.Context
	waitOwner locks.WaitOwner
	cancel    context.CancelCauseFunc
	state     lockOwnerState
	pending   int
	sequence  uint64
	locks     *lockRecord
}

type lockRecord struct {
	request  lockRequest
	mode     lockMode
	ordinal  uint64
	size     uint64
	previous *lockRecord
}

type lockPosition struct {
	owner   *lockOwner
	ordinal uint64
}

type lockRegistry struct {
	mu                  sync.Mutex
	owners              map[model.TxnID]*lockOwner
	maxOwners, maxLocks int
	maxBytes, usedBytes uint64
	lockCount           int
	age                 uint64
	changed             chan struct{}
	waitGraph           *locks.WaitGraph
}

// maxLocks and maxBytes include pending acquisitions and record metadata, not
// just granted keys. Owner metadata is bounded separately by maxOwners.
func newLockRegistry(maxOwners, maxLocks int, maxBytes uint64, waitGraphs ...*locks.WaitGraph) (*lockRegistry, error) {
	if maxOwners <= 0 || maxLocks <= 0 || maxBytes < uint64(unsafe.Sizeof(lockRecord{})) || len(waitGraphs) > 1 {
		return nil, errLockConfig
	}
	var waitGraph *locks.WaitGraph
	if len(waitGraphs) == 1 {
		waitGraph = waitGraphs[0]
	}
	return &lockRegistry{
		owners:    make(map[model.TxnID]*lockOwner),
		maxOwners: maxOwners, maxLocks: maxLocks, maxBytes: maxBytes,
		changed: make(chan struct{}), waitGraph: waitGraph,
	}, nil
}

func (r *lockRegistry) register(ctx context.Context, id model.TxnID, waitOwners ...locks.WaitOwner) (*lockOwner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	if id == (model.TxnID{}) {
		return nil, errLockOwner
	}
	if _, exists := r.owners[id]; exists {
		return nil, errLockOwner
	}
	if len(r.owners) == r.maxOwners {
		return nil, errLockOwnerCapacity
	}
	if r.age == ^uint64(0) {
		return nil, errLockSequence
	}
	r.age++
	var waitOwner locks.WaitOwner
	if len(waitOwners) > 1 {
		return nil, errLockOwner
	}
	if len(waitOwners) == 1 {
		waitOwner = waitOwners[0]
	}
	owner := &lockOwner{registry: r, id: id, age: r.age, waitOwner: waitOwner}
	owner.ctx, owner.cancel = context.WithCancelCause(ctx)
	r.owners[id] = owner
	return owner, nil
}

// acquire borrows request until return and clones its finite keys before waiting
// or granting. Cancellation does not remove any previously granted lock.
func (r *lockRegistry) acquire(ctx context.Context, owner *lockOwner, request lockRequest, mode lockMode, policy lockWaitPolicy) (bool, error) {
	return r.acquireWithConflict(ctx, owner, request, mode, mode, policy)
}

// acquireWaitFor waits for conflictMode compatibility but retains only grantMode.
func (r *lockRegistry) acquireWaitFor(ctx context.Context, owner *lockOwner, request lockRequest, grantMode, conflictMode lockMode) (bool, error) {
	return r.acquireWithConflict(ctx, owner, request, grantMode, conflictMode, lockWait)
}

func (r *lockRegistry) acquireWithConflict(ctx context.Context, owner *lockOwner, request lockRequest, grantMode, conflictMode lockMode, policy lockWaitPolicy) (bool, error) {
	if !validLockRequest(request) || !validLockMode(grantMode) || !validLockMode(conflictMode) || policy > lockSkip || isTableLockMode(grantMode) != isTableLockMode(conflictMode) {
		return false, errLockRequest
	}
	r.mu.Lock()
	var waitDuration time.Duration
	waited := false
	defer func() {
		r.mu.Unlock()
		if waited {
			locks.ReportLockWait(ctx, waitDuration)
		}
	}()
	if err := r.active(owner); err != nil {
		return false, err
	}
	if err := context.Cause(ctx); err != nil {
		return false, err
	}
	covered := false
	for record := owner.locks; record != nil; record = record.previous {
		if record.mode != grantMode || !lockContains(record.request, request) {
			continue
		}
		if grantMode == conflictMode {
			return true, nil
		}
		covered = true
		break
	}
	if policy != lockWait {
		conflict, _ := r.conflicts(owner, request, conflictMode, false, nil)
		if conflict {
			if policy == lockNoWait {
				return false, errLockUnavailable
			}
			return false, nil
		}
		if covered {
			return true, nil
		}
	}
	size := uint64(unsafe.Sizeof(lockRecord{}))
	if !covered {
		if size > r.maxBytes-r.usedBytes || uint64(len(request.Lower.Key)) > r.maxBytes-r.usedBytes-size {
			return false, errLockCapacity
		}
		size += uint64(len(request.Lower.Key))
		if uint64(len(request.Upper.Key)) > r.maxBytes-r.usedBytes-size || r.lockCount == r.maxLocks {
			return false, errLockCapacity
		}
		size += uint64(len(request.Upper.Key))
		r.usedBytes += size
		r.lockCount++
	}
	owner.pending++
	granted := covered
	defer func() {
		owner.pending--
		if !covered && !granted {
			r.usedBytes -= size
			r.lockCount--
		}
	}()
	defer r.waitGraph.EndWait(owner.waitOwner)
	if !covered {
		request.Lower.Key = cloneLockKey(request.Lower.Key)
		request.Upper.Key = cloneLockKey(request.Upper.Key)
	}
	timeout, timeoutConfigured := locks.LockTimeout(ctx)
	var waitContext context.Context
	var stopWait context.CancelFunc
	var blockers []locks.WaitOwner
	for {
		if err := r.active(owner); err != nil {
			return false, err
		}
		if err := context.Cause(ctx); err != nil {
			return false, err
		}
		if waitContext != nil {
			if err := context.Cause(waitContext); err != nil {
				return false, err
			}
		}
		conflict := false
		conflict, blockers = r.conflicts(owner, request, conflictMode, true, blockers[:0])
		if !conflict {
			if covered {
				return true, nil
			}
			if owner.sequence == ^uint64(0) {
				return false, errLockSequence
			}
			owner.sequence++
			owner.locks = &lockRecord{request: request, mode: grantMode, ordinal: owner.sequence, size: size, previous: owner.locks}
			granted = true
			return true, nil
		}
		if err := r.waitGraph.BeginWait(owner.waitOwner, blockers); err != nil {
			return false, err
		}
		if waitContext == nil && timeoutConfigured && timeout > 0 {
			waitContext, stopWait = context.WithTimeoutCause(ctx, timeout, ErrLockTimeout)
			// Registered here, not before the assignment: the cancel function only
			// exists once the wait context does, and this branch runs at most once.
			defer func() { stopWait() }()
		}
		var timeoutDone <-chan struct{}
		if waitContext != nil {
			timeoutDone = waitContext.Done()
		}
		waited = true
		waitStarted := time.Now()
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-owner.ctx.Done():
		case <-timeoutDone:
		case <-changed:
		}
		r.mu.Lock()
		waitDuration += time.Since(waitStarted)
	}
}

func (r *lockRegistry) acquireNoWaitFor(ctx context.Context, owner *lockOwner, request lockRequest, grantMode, conflictMode lockMode) error {
	if !validLockRequest(request) || !validLockMode(grantMode) || !validLockMode(conflictMode) || isTableLockMode(grantMode) || isTableLockMode(conflictMode) {
		return errLockRequest
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.active(owner); err != nil {
		return err
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	if conflict, _ := r.conflicts(owner, request, conflictMode, false, nil); conflict {
		return errLockUnavailable
	}
	if owner.sequence == ^uint64(0) {
		return errLockSequence
	}
	if r.lockCount == r.maxLocks {
		return errLockCapacity
	}
	size := uint64(unsafe.Sizeof(lockRecord{})) + uint64(len(request.Lower.Key)) + uint64(len(request.Upper.Key))
	if size > r.maxBytes-r.usedBytes {
		return errLockCapacity
	}
	request.Lower.Key = cloneLockKey(request.Lower.Key)
	request.Upper.Key = cloneLockKey(request.Upper.Key)
	owner.sequence++
	owner.locks = &lockRecord{request: request, mode: grantMode, ordinal: owner.sequence, size: size, previous: owner.locks}
	r.usedBytes += size
	r.lockCount++
	return nil
}

// acquireEligible atomically excludes conflicting row intervals and grants
// shared locks for every surviving fragment. It never waits or wounds holders.
func (r *lockRegistry) acquireEligible(ctx context.Context, owner *lockOwner, request lockRequest, grantMode, conflictMode lockMode, maxParts int) ([]lockRequest, uint64, error) {
	if !validLockRequest(request) || !validLockMode(grantMode) || !validLockMode(conflictMode) || isTableLockMode(grantMode) || isTableLockMode(conflictMode) || maxParts <= 0 {
		return nil, 0, errLockRequest
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.active(owner); err != nil {
		return nil, 0, err
	}
	if err := context.Cause(ctx); err != nil {
		return nil, 0, err
	}
	conflicts := make([]lockRequest, 0)
	for _, holder := range r.owners {
		if holder == owner {
			continue
		}
		for record := holder.locks; record != nil; record = record.previous {
			if isTableLockMode(record.mode) || !lockRequestsConflict(request, conflictMode, record.request, record.mode) {
				continue
			}
			if len(conflicts) == maxParts {
				return nil, 0, errLockCapacity
			}
			conflicts = append(conflicts, record.request)
		}
	}
	slices.SortFunc(conflicts, compareLockRequestLower)
	parts := make([]lockRequest, 0, min(maxParts, len(conflicts)+1))
	lower := request.Lower
	blockedToEnd := false
	for _, conflict := range conflicts {
		if lockEndsBefore(conflict.Upper, lower) {
			continue
		}
		if !conflict.Lower.Unbounded {
			left := lockRequest{ObjectID: request.ObjectID, Generation: request.Generation, Lower: lower, Upper: complementLockBound(conflict.Lower)}
			if validLockRequest(left) && lockContains(request, left) {
				if len(parts) == maxParts {
					return nil, 0, errLockCapacity
				}
				parts = append(parts, left)
			}
		}
		if conflict.Upper.Unbounded {
			blockedToEnd = true
			break
		}
		next := complementLockBound(conflict.Upper)
		if compareLockLower(next, lower) > 0 {
			lower = next
		}
	}
	tail := lockRequest{ObjectID: request.ObjectID, Generation: request.Generation, Lower: lower, Upper: request.Upper}
	if !blockedToEnd && validLockRequest(tail) && lockContains(request, tail) {
		if len(parts) == maxParts {
			return nil, 0, errLockCapacity
		}
		parts = append(parts, tail)
	}
	if len(parts) == 0 {
		return nil, uint64(cap(parts)) * uint64(unsafe.Sizeof(lockRequest{})), nil
	}
	if owner.sequence > ^uint64(0)-uint64(len(parts)) {
		return nil, 0, errLockSequence
	}
	if r.lockCount > r.maxLocks-len(parts) {
		return nil, 0, errLockCapacity
	}
	total := uint64(0)
	for _, part := range parts {
		size := uint64(unsafe.Sizeof(lockRecord{})) + uint64(len(part.Lower.Key)) + uint64(len(part.Upper.Key))
		if size > r.maxBytes-r.usedBytes-total {
			return nil, 0, errLockCapacity
		}
		total += size
	}
	for i := range parts {
		part := &parts[i]
		part.Lower.Key = cloneLockKey(part.Lower.Key)
		part.Upper.Key = cloneLockKey(part.Upper.Key)
		size := uint64(unsafe.Sizeof(lockRecord{})) + uint64(len(part.Lower.Key)) + uint64(len(part.Upper.Key))
		owner.sequence++
		owner.locks = &lockRecord{request: *part, mode: grantMode, ordinal: owner.sequence, size: size, previous: owner.locks}
		r.usedBytes += size
		r.lockCount++
	}
	return parts, uint64(cap(parts)) * uint64(unsafe.Sizeof(lockRequest{})), nil
}

func compareLockRequestLower(a, b lockRequest) int {
	order := compareLockLower(a.Lower, b.Lower)
	if order != 0 {
		return order
	}
	return compareLockUpper(a.Upper, b.Upper)
}

func compareLockLower(a, b lockBound) int {
	order := compareLockBounds(a, b, lowerLockEdge(a), lowerLockEdge(b))
	if order == 0 && a.Inclusive != b.Inclusive {
		if a.Inclusive {
			return -1
		}
		return 1
	}
	return order
}

func compareLockUpper(a, b lockBound) int {
	order := compareLockBounds(a, b, -lowerLockEdge(a), -lowerLockEdge(b))
	if order == 0 && a.Inclusive != b.Inclusive {
		if a.Inclusive {
			return 1
		}
		return -1
	}
	return order
}
func complementLockBound(bound lockBound) lockBound {
	if !bound.Unbounded {
		bound.Inclusive = !bound.Inclusive
	}
	return bound
}

func cloneLockKey(key []byte) []byte {
	if len(key) == 0 {
		return nil
	}
	owned := make([]byte, len(key))
	copy(owned, key)
	return owned
}

func validLockRequest(request lockRequest) bool {
	if request.ObjectID == 0 || request.Generation == 0 {
		return false
	}
	for _, bound := range [...]lockBound{request.Lower, request.Upper} {
		finiteProperties := len(bound.Key) != 0 || bound.Inclusive || bound.Prefix
		if bound.Unbounded && finiteProperties {
			return false
		}
	}
	if request.Lower.Unbounded || request.Upper.Unbounded {
		return true
	}
	order := compareLockBounds(request.Lower, request.Upper, lowerLockEdge(request.Lower), -lowerLockEdge(request.Upper))
	return order < 0 || (order == 0 && request.Lower.Inclusive && request.Upper.Inclusive)
}

func validLockMode(mode lockMode) bool {
	return mode <= lockTableExclusive
}

func lockRequestsConflict(a lockRequest, am lockMode, b lockRequest, bm lockMode) bool {
	if a.ObjectID != b.ObjectID {
		return false
	}
	if am == lockSchema || bm == lockSchema {
		return true
	}
	if a.Generation != b.Generation || !lockModesConflict(am, bm) {
		return false
	}
	return !lockEndsBefore(a.Upper, b.Lower) && !lockEndsBefore(b.Upper, a.Lower)
}

func lockModesConflict(a, b lockMode) bool {
	if isTableLockMode(a) || isTableLockMode(b) {
		if !isTableLockMode(a) || !isTableLockMode(b) {
			return false
		}
		access := [...]uint8{
			1 << 7,
			1<<6 | 1<<7,
			1<<4 | 1<<5 | 1<<6 | 1<<7,
			1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7,
			1<<2 | 1<<3 | 1<<5 | 1<<6 | 1<<7,
			1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7,
			1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7,
			1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7,
		}
		return access[tableLockRank(a)]&(1<<tableLockRank(b)) != 0
	}
	if a == lockUpdate || b == lockUpdate {
		return true
	}
	if a == lockKeyShare || b == lockKeyShare {
		return false
	}
	return a == lockNoKeyUpdate || b == lockNoKeyUpdate
}

func isTableLockMode(mode lockMode) bool {
	return mode == lockIntent || mode == lockSchema || mode >= lockTableRowShare
}

func tableLockRank(mode lockMode) uint8 {
	switch mode {
	case lockIntent:
		return 0
	case lockTableRowShare:
		return 1
	case lockTableRowExclusive:
		return 2
	case lockTableShareUpdateExclusive:
		return 3
	case lockTableShare:
		return 4
	case lockTableShareRowExclusive:
		return 5
	case lockTableExclusive:
		return 6
	default:
		return 7
	}
}

func lockEndsBefore(upper, lower lockBound) bool {
	if upper.Unbounded || lower.Unbounded {
		return false
	}
	order := compareLockBounds(upper, lower, -lowerLockEdge(upper), lowerLockEdge(lower))
	return order < 0 || (order == 0 && (!upper.Inclusive || !lower.Inclusive))
}

// Partial key ends are ordered as virtual suffix infinities; no successor key
// arithmetic can overflow at an all-ones byte prefix.
func lowerLockEdge(bound lockBound) int {
	if !bound.Prefix {
		return 0
	}
	if bound.Inclusive {
		return -1
	}
	return 1
}

func compareLockBounds(a, b lockBound, aEdge, bEdge int) int {
	order := bytes.Compare(a.Key, b.Key)
	if order == 0 {
		return cmp.Compare(aEdge, bEdge)
	}
	if aEdge != 0 && bytes.HasPrefix(b.Key, a.Key) {
		return aEdge
	}
	if bEdge != 0 && bytes.HasPrefix(a.Key, b.Key) {
		return -bEdge
	}
	return order
}

// Cancellation is a request to abort, not cleanup acknowledgement. Aborting and
// committing owners retain conflicts until rollback or release removes records.
func (r *lockRegistry) conflicts(owner *lockOwner, request lockRequest, mode lockMode, wound bool, blockers []locks.WaitOwner) (bool, []locks.WaitOwner) {
	conflict := false
	for _, holder := range r.owners {
		if holder == owner {
			continue
		}
		for record := holder.locks; record != nil; record = record.previous {
			if !lockRequestsConflict(request, mode, record.request, record.mode) {
				continue
			}
			conflict = true
			wounded := wound && owner.age < holder.age && holder.state == lockOwnerActive
			if wounded {
				holder.state = lockOwnerAborting
				holder.cancel(errLockWounded)
			} else if holder.waitOwner != 0 && holder.state == lockOwnerActive {
				blockers = append(blockers, holder.waitOwner)
			}
			break
		}
	}
	return conflict, blockers
}

func (r *lockRegistry) active(owner *lockOwner) error {
	if owner == nil || owner.registry != r {
		return errLockOwner
	}
	if cause := context.Cause(owner.ctx); cause != nil {
		if owner.state == lockOwnerActive {
			owner.state = lockOwnerAborting
		}
		return cause
	}
	if owner.state != lockOwnerActive {
		return errLockOwnerState
	}
	return nil
}

func (r *lockRegistry) savepoint(owner *lockOwner) (lockPosition, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.active(owner); err != nil {
		return lockPosition{}, err
	}
	if owner.pending != 0 {
		return lockPosition{}, errLockOwnerBusy
	}
	position := lockPosition{owner: owner}
	if owner.locks != nil {
		position.ordinal = owner.locks.ordinal
	}
	return position, nil
}

// rollback acknowledges cleanup of writes made after position. Wounded owners
// must abort completely; they cannot restore activity by rolling back a savepoint.
func (r *lockRegistry) rollback(owner *lockOwner, position lockPosition) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.active(owner); err != nil {
		return err
	}
	if owner.pending != 0 {
		return errLockOwnerBusy
	}
	if position.owner != owner || position.ordinal > owner.sequence {
		return errLockPosition
	}
	for owner.locks != nil && owner.locks.ordinal > position.ordinal {
		r.pop(owner)
	}
	r.notify()
	return nil
}

func (r *lockRegistry) beginCommit(owner *lockOwner) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.active(owner); err != nil {
		return err
	}
	if owner.pending != 0 {
		return errLockOwnerBusy
	}
	owner.state = lockOwnerCommitting
	return nil
}

// release is the caller's acknowledgement that abort/commit cleanup is complete.
// It is idempotent and wakes pending acquisitions, including this owner's.
func (r *lockRegistry) release(owner *lockOwner) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if owner == nil || owner.registry != r {
		return errLockOwner
	}
	if owner.state == lockOwnerEnded {
		return nil
	}
	for owner.locks != nil {
		r.pop(owner)
	}
	owner.state = lockOwnerEnded
	owner.cancel(context.Canceled)
	owner.cancel = nil
	delete(r.owners, owner.id)
	r.notify()
	return nil
}

func (r *lockRegistry) pop(owner *lockOwner) {
	record := owner.locks
	owner.locks = record.previous
	r.usedBytes -= record.size
	r.lockCount--
	*record = lockRecord{}
}

func (r *lockRegistry) notify() {
	close(r.changed)
	r.changed = make(chan struct{})
}
