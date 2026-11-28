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

type lockIndexKind uint8

const (
	lockIndexWide lockIndexKind = iota
	lockIndexPoint
	lockIndexPrefix
)

type lockObjectGen struct {
	objectID   model.ObjectID
	generation uint64
}

type nonWoundingContextKey struct{}

// WithNonWounding marks background maintenance transactions (such as
// auto-ANALYZE) so wound-wait conflict checks never abort active foreground
// user transactions on their behalf.
func WithNonWounding(ctx context.Context) context.Context {
	return context.WithValue(ctx, nonWoundingContextKey{}, true)
}

func isNonWounding(ctx context.Context) bool {
	enabled, _ := ctx.Value(nonWoundingContextKey{}).(bool)
	return enabled
}

type lockOwner struct {
	registry    *lockRegistry
	id          model.TxnID
	age         uint64
	nonWounding bool
	ctx         context.Context
	waitOwner   locks.WaitOwner
	cancel      context.CancelCauseFunc
	state       lockOwnerState
	pending     int
	sequence    uint64
	locks       *lockRecord
	wideLocks   []*lockRecord
	pointLocks  map[lockObjectGen]map[string][]*lockRecord
	prefixLocks map[lockObjectGen]map[string][]*lockRecord
	schemaLocks map[model.ObjectID][]*lockRecord
}

type lockRecord struct {
	owner         *lockOwner
	request       lockRequest
	mode          lockMode
	retainedShare bool
	indexKind     lockIndexKind
	inSchemaMap   bool
	ordinal       uint64
	size          uint64
	previous      *lockRecord
	bucketIndex   int
	ownerIndex    int
	schemaIndex   int
}

type objectLockBucket struct {
	wide          []*lockRecord
	points        map[uint64]map[string][]*lockRecord
	prefixes      map[uint64]map[string][]*lockRecord
	ownerCounts   map[*lockOwner]int
	total         int
	changed       chan struct{}
	waiters       int
	pendingNotify bool
}

type lockPosition struct {
	owner   *lockOwner
	ordinal uint64
}

type lockRegistry struct {
	mu                  sync.Mutex
	owners              map[model.TxnID]*lockOwner
	objects             map[model.ObjectID]*objectLockBucket
	maxOwners, maxLocks int
	maxBytes, usedBytes uint64
	lockCount           int
	age                 uint64
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
		objects:   make(map[model.ObjectID]*objectLockBucket),
		maxOwners: maxOwners, maxLocks: maxLocks, maxBytes: maxBytes,
		waitGraph: waitGraph,
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
	owner := &lockOwner{registry: r, id: id, age: r.age, nonWounding: isNonWounding(ctx), waitOwner: waitOwner}
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
	matched, covered, conflictCovered := coveredRecord(owner, request, grantMode, conflictMode)
	if conflictCovered {
		if grantMode == lockShare && matched.mode != lockShare {
			matched.retainedShare = true
		}
		return true, nil
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
			if grantMode == lockShare && matched.mode != lockShare {
				matched.retainedShare = true
			}
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
				if grantMode == lockShare && matched.mode != lockShare {
					matched.retainedShare = true
				}
				return true, nil
			}
			if owner.sequence == ^uint64(0) {
				return false, errLockSequence
			}
			r.addRecordLocked(owner, request, grantMode, size)
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
		bucket := r.bucketLocked(request.ObjectID)
		bucket.waiters++
		changed := bucket.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-owner.ctx.Done():
		case <-timeoutDone:
		case <-changed:
		}
		r.mu.Lock()
		bucket.waiters--
		if bucket.total == 0 && bucket.waiters == 0 {
			delete(r.objects, request.ObjectID)
		}
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
	matched, covered, conflictCovered := coveredRecord(owner, request, grantMode, conflictMode)
	if conflictCovered {
		if grantMode == lockShare && matched.mode != lockShare {
			matched.retainedShare = true
		}
		return nil
	}
	if conflict, _ := r.conflicts(owner, request, conflictMode, false, nil); conflict {
		return errLockUnavailable
	}
	if covered {
		if grantMode == lockShare && matched.mode != lockShare {
			matched.retainedShare = true
		}
		return nil
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
	r.addRecordLocked(owner, request, grantMode, size)
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
	if bucket := r.objects[request.ObjectID]; bucket != nil && bucket.total > 0 {
		if len(bucket.ownerCounts) > 1 || bucket.ownerCounts[owner] == 0 {
			for _, record := range bucket.wide {
				if record.owner == owner {
					continue
				}
				if isTableLockMode(record.mode) || !lockRequestsConflict(request, conflictMode, record.request, record.mode) {
					continue
				}
				if len(conflicts) == maxParts {
					return nil, 0, errLockCapacity
				}
				conflicts = append(conflicts, record.request)
			}
			if genPoints := bucket.points[request.Generation]; len(genPoints) != 0 {
				for _, records := range genPoints {
					for _, record := range records {
						if record.owner == owner {
							continue
						}
						if !lockRequestsConflict(request, conflictMode, record.request, record.mode) {
							continue
						}
						if len(conflicts) == maxParts {
							return nil, 0, errLockCapacity
						}
						conflicts = append(conflicts, record.request)
					}
				}
			}
			if genPrefixes := bucket.prefixes[request.Generation]; len(genPrefixes) != 0 {
				for _, records := range genPrefixes {
					for _, record := range records {
						if record.owner == owner {
							continue
						}
						if !lockRequestsConflict(request, conflictMode, record.request, record.mode) {
							continue
						}
						if len(conflicts) == maxParts {
							return nil, 0, errLockCapacity
						}
						conflicts = append(conflicts, record.request)
					}
				}
			}
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
		r.addRecordLocked(owner, *part, grantMode, size)
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

var tableLockConflictMask = [...]uint8{
	1 << 7,
	1<<6 | 1<<7,
	1<<4 | 1<<5 | 1<<6 | 1<<7,
	1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7,
	1<<2 | 1<<3 | 1<<5 | 1<<6 | 1<<7,
	1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7,
	1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7,
	1<<0 | 1<<1 | 1<<2 | 1<<3 | 1<<4 | 1<<5 | 1<<6 | 1<<7,
}

func lockModesConflict(a, b lockMode) bool {
	if isTableLockMode(a) || isTableLockMode(b) {
		if !isTableLockMode(a) || !isTableLockMode(b) {
			return false
		}
		return tableLockConflictMask[tableLockRank(a)]&(1<<tableLockRank(b)) != 0
	}
	if a == lockUpdate || b == lockUpdate {
		return true
	}
	if a == lockKeyShare || b == lockKeyShare {
		return false
	}
	return a == lockNoKeyUpdate || b == lockNoKeyUpdate
}

func lockModeSubsumes(held, requested lockMode) bool {
	if held == requested {
		return true
	}
	if isTableLockMode(held) != isTableLockMode(requested) {
		return false
	}
	if isTableLockMode(held) {
		hm := tableLockConflictMask[tableLockRank(held)]
		rm := tableLockConflictMask[tableLockRank(requested)]
		return hm&rm == rm
	}
	switch held {
	case lockUpdate:
		return true
	case lockNoKeyUpdate:
		return requested == lockShare || requested == lockKeyShare
	case lockShare:
		return requested == lockKeyShare
	default:
		return false
	}
}

func isPointLockRequest(r lockRequest) bool {
	return !r.Lower.Unbounded && !r.Upper.Unbounded &&
		r.Lower.Inclusive && r.Upper.Inclusive &&
		!r.Lower.Prefix && !r.Upper.Prefix &&
		bytes.Equal(r.Lower.Key, r.Upper.Key)
}

func isExactPrefixLockRequest(r lockRequest) bool {
	return !r.Lower.Unbounded && !r.Upper.Unbounded &&
		r.Lower.Inclusive && r.Upper.Inclusive &&
		r.Lower.Prefix && r.Upper.Prefix &&
		bytes.Equal(r.Lower.Key, r.Upper.Key)
}

func commonPrefixLen(a, b []byte) int {
	n := min(len(a), len(b))
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

func coveredRecord(owner *lockOwner, request lockRequest, grantMode, conflictMode lockMode) (*lockRecord, bool, bool) {
	var matched *lockRecord
	checkSlice := func(records []*lockRecord, verifyContains bool) (*lockRecord, bool) {
		for _, record := range records {
			if !lockModeSubsumes(record.mode, grantMode) {
				continue
			}
			if verifyContains && !lockContains(record.request, request) {
				continue
			}
			if lockModeSubsumes(record.mode, conflictMode) {
				return record, true
			}
			if matched == nil {
				matched = record
			}
		}
		return nil, false
	}
	if !isTableLockMode(grantMode) {
		// lockSchema (ACCESS EXCLUSIVE) conflicts with every lock mode across all
		// generations and keys of the object, so it subsumes any row lock on that
		// object. For lockShare we additionally require lockContains so a savepoint
		// rollback downgrading retainedShare -> lockShare still covers request.
		// lockTableExclusive does not conflict with lockIntent or row locks and
		// therefore cannot subsume row locks.
		if schemas := owner.schemaLocks[request.ObjectID]; len(schemas) != 0 {
			for _, record := range schemas {
				if grantMode != lockShare || lockContains(record.request, request) {
					return record, true, true
				}
			}
		}
		og := lockObjectGen{objectID: request.ObjectID, generation: request.Generation}
		isPoint := isPointLockRequest(request)
		if isPoint {
			if genPoints := owner.pointLocks[og]; genPoints != nil {
				if rec, full := checkSlice(genPoints[string(request.Lower.Key)], false); full {
					return rec, true, true
				}
			}
		}
		if oPrefixes := owner.prefixLocks[og]; len(oPrefixes) != 0 {
			if isPoint || isExactPrefixLockRequest(request) {
				key := request.Lower.Key
				if len(key)+1 <= len(oPrefixes) {
					for i := len(key); i >= 0; i-- {
						if rec, full := checkSlice(oPrefixes[string(key[:i])], false); full {
							return rec, true, true
						}
					}
				} else {
					for _, recs := range oPrefixes {
						if rec, full := checkSlice(recs, true); full {
							return rec, true, true
						}
					}
				}
			} else if !request.Lower.Unbounded && !request.Upper.Unbounded {
				prefixLen := commonPrefixLen(request.Lower.Key, request.Upper.Key)
				if prefixLen+1 <= len(oPrefixes) {
					for i := prefixLen; i >= 0; i-- {
						if rec, full := checkSlice(oPrefixes[string(request.Lower.Key[:i])], true); full {
							return rec, true, true
						}
					}
				} else {
					for _, recs := range oPrefixes {
						if rec, full := checkSlice(recs, true); full {
							return rec, true, true
						}
					}
				}
			}
		}
	}
	if rec, full := checkSlice(owner.wideLocks, true); full {
		return rec, true, true
	}
	return matched, matched != nil, false
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
	bucket := r.objects[request.ObjectID]
	if bucket == nil || bucket.total == 0 {
		return false, blockers
	}
	if len(bucket.ownerCounts) == 1 && bucket.ownerCounts[owner] > 0 {
		return false, blockers
	}
	conflict := false
	if mode == lockSchema {
		for holder := range bucket.ownerCounts {
			if holder == owner {
				continue
			}
			conflict = true
			if !wound {
				return true, blockers
			}
			if !owner.nonWounding && owner.age < holder.age && holder.state == lockOwnerActive {
				holder.state = lockOwnerAborting
				holder.cancel(errLockWounded)
			} else if holder.waitOwner != 0 && holder.state == lockOwnerActive {
				blockers = append(blockers, holder.waitOwner)
			}
		}
		return conflict, blockers
	}
	checkRecord := func(record *lockRecord) bool {
		holder := record.owner
		if holder == owner {
			return false
		}
		if !lockRequestsConflict(request, mode, record.request, record.mode) {
			return false
		}
		conflict = true
		if !wound {
			return true
		}
		if !owner.nonWounding && owner.age < holder.age && holder.state == lockOwnerActive {
			holder.state = lockOwnerAborting
			holder.cancel(errLockWounded)
		} else if holder.waitOwner != 0 && holder.state == lockOwnerActive {
			blockers = append(blockers, holder.waitOwner)
		}
		return false
	}
	for _, record := range bucket.wide {
		if checkRecord(record) {
			return true, blockers
		}
	}
	if isTableLockMode(mode) {
		return conflict, blockers
	}
	genPoints := bucket.points[request.Generation]
	genPrefixes := bucket.prefixes[request.Generation]
	if isPointLockRequest(request) {
		if len(genPoints) != 0 {
			for _, record := range genPoints[string(request.Lower.Key)] {
				if checkRecord(record) {
					return true, blockers
				}
			}
		}
		if len(genPrefixes) != 0 {
			key := request.Lower.Key
			if len(key)+1 <= len(genPrefixes) {
				for i := len(key); i >= 0; i-- {
					for _, record := range genPrefixes[string(key[:i])] {
						if checkRecord(record) {
							return true, blockers
						}
					}
				}
			} else {
				for _, records := range genPrefixes {
					for _, record := range records {
						if checkRecord(record) {
							return true, blockers
						}
					}
				}
			}
		}
		return conflict, blockers
	}
	for _, records := range genPoints {
		for _, record := range records {
			if checkRecord(record) {
				return true, blockers
			}
		}
	}
	for _, records := range genPrefixes {
		for _, record := range records {
			if checkRecord(record) {
				return true, blockers
			}
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

// rollback acknowledges cleanup of writes made after position under Strict 2PL.
// Write/update and exclusive locks acquired after position are released while
// lockShare read locks are retained until transaction commit or full abort.
// Wounded owners must abort completely; they cannot restore activity by rolling
// back a savepoint.
func (r *lockRegistry) rollback(owner *lockOwner, position lockPosition) error {
	return r.rollbackLocks(owner, position, true)
}

// rollbackAttempt removes every lock acquired after position by an atomic
// acquisition call that failed or skipped before returning data.
func (r *lockRegistry) rollbackAttempt(owner *lockOwner, position lockPosition) error {
	return r.rollbackLocks(owner, position, false)
}

func (r *lockRegistry) rollbackLocks(owner *lockOwner, position lockPosition, retainReadLocks bool) error {
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
	var dirtyBuf [8]*objectLockBucket
	dirty := dirtyBuf[:0]
	for prev := &owner.locks; *prev != nil && (*prev).ordinal > position.ordinal; {
		record := *prev
		if retainReadLocks {
			if record.mode == lockShare {
				prev = &record.previous
				continue
			}
			if record.retainedShare {
				if record.inSchemaMap {
					removeOwnerSchemaLock(owner, record)
				}
				record.mode = lockShare
				record.retainedShare = false
				r.markBucketDirtyLocked(record.request.ObjectID, &dirty)
				prev = &record.previous
				continue
			}
		}
		*prev = record.previous
		r.removeRecordLocked(record, &dirty, true)
	}
	r.notifyBucketsLocked(dirty)
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
	var dirtyBuf [8]*objectLockBucket
	dirty := dirtyBuf[:0]
	for owner.locks != nil {
		record := owner.locks
		owner.locks = record.previous
		r.removeRecordLocked(record, &dirty, false)
	}
	owner.wideLocks = nil
	owner.pointLocks = nil
	owner.prefixLocks = nil
	owner.schemaLocks = nil
	owner.state = lockOwnerEnded
	owner.cancel(context.Canceled)
	owner.cancel = nil
	delete(r.owners, owner.id)
	r.notifyBucketsLocked(dirty)
	return nil
}

func (r *lockRegistry) bucketLocked(objectID model.ObjectID) *objectLockBucket {
	bucket := r.objects[objectID]
	if bucket == nil {
		bucket = &objectLockBucket{
			ownerCounts: make(map[*lockOwner]int),
			changed:     make(chan struct{}),
		}
		r.objects[objectID] = bucket
	}
	return bucket
}

func insertKeyedLock(target *map[uint64]map[string][]*lockRecord, gen uint64, key string, record *lockRecord) int {
	if *target == nil {
		*target = make(map[uint64]map[string][]*lockRecord)
	}
	genMap := (*target)[gen]
	if genMap == nil {
		genMap = make(map[string][]*lockRecord)
		(*target)[gen] = genMap
	}
	slice := genMap[key]
	idx := len(slice)
	genMap[key] = append(slice, record)
	return idx
}

func removeKeyedLock(target map[uint64]map[string][]*lockRecord, gen uint64, key string, idx int) {
	genMap := target[gen]
	slice := genMap[key]
	last := len(slice) - 1
	if idx != last {
		moved := slice[last]
		slice[idx] = moved
		moved.bucketIndex = idx
	}
	slice[last] = nil
	slice = slice[:last]
	if len(slice) == 0 {
		delete(genMap, key)
		if len(genMap) == 0 {
			delete(target, gen)
		}
	} else {
		genMap[key] = slice
	}
}

func insertOwnerKeyedLock(target *map[lockObjectGen]map[string][]*lockRecord, og lockObjectGen, key string, record *lockRecord) int {
	if *target == nil {
		*target = make(map[lockObjectGen]map[string][]*lockRecord)
	}
	oMap := (*target)[og]
	if oMap == nil {
		oMap = make(map[string][]*lockRecord)
		(*target)[og] = oMap
	}
	slice := oMap[key]
	idx := len(slice)
	oMap[key] = append(slice, record)
	return idx
}

func removeOwnerKeyedLock(target map[lockObjectGen]map[string][]*lockRecord, og lockObjectGen, key string, idx int) {
	oMap := target[og]
	slice := oMap[key]
	last := len(slice) - 1
	if idx != last {
		moved := slice[last]
		slice[idx] = moved
		moved.ownerIndex = idx
	}
	slice[last] = nil
	slice = slice[:last]
	if len(slice) == 0 {
		delete(oMap, key)
		if len(oMap) == 0 {
			delete(target, og)
		}
	} else {
		oMap[key] = slice
	}
}

func (r *lockRegistry) addRecordLocked(owner *lockOwner, request lockRequest, mode lockMode, size uint64) *lockRecord {
	owner.sequence++
	bucket := r.bucketLocked(request.ObjectID)
	kind := lockIndexWide
	if !isTableLockMode(mode) {
		if isPointLockRequest(request) {
			kind = lockIndexPoint
		} else if isExactPrefixLockRequest(request) {
			kind = lockIndexPrefix
		}
	}
	inSchema := mode == lockSchema
	record := &lockRecord{
		owner:       owner,
		request:     request,
		mode:        mode,
		indexKind:   kind,
		inSchemaMap: inSchema,
		ordinal:     owner.sequence,
		size:        size,
		previous:    owner.locks,
	}
	owner.locks = record
	switch kind {
	case lockIndexPoint:
		key := string(request.Lower.Key)
		og := lockObjectGen{objectID: request.ObjectID, generation: request.Generation}
		record.bucketIndex = insertKeyedLock(&bucket.points, request.Generation, key, record)
		record.ownerIndex = insertOwnerKeyedLock(&owner.pointLocks, og, key, record)
	case lockIndexPrefix:
		key := string(request.Lower.Key)
		og := lockObjectGen{objectID: request.ObjectID, generation: request.Generation}
		record.bucketIndex = insertKeyedLock(&bucket.prefixes, request.Generation, key, record)
		record.ownerIndex = insertOwnerKeyedLock(&owner.prefixLocks, og, key, record)
	default:
		record.bucketIndex = len(bucket.wide)
		bucket.wide = append(bucket.wide, record)
		record.ownerIndex = len(owner.wideLocks)
		owner.wideLocks = append(owner.wideLocks, record)
	}
	if inSchema {
		if owner.schemaLocks == nil {
			owner.schemaLocks = make(map[model.ObjectID][]*lockRecord)
		}
		sSlice := owner.schemaLocks[request.ObjectID]
		record.schemaIndex = len(sSlice)
		owner.schemaLocks[request.ObjectID] = append(sSlice, record)
	}
	bucket.total++
	bucket.ownerCounts[owner]++
	return record
}

func removeOwnerSchemaLock(owner *lockOwner, record *lockRecord) {
	objectID := record.request.ObjectID
	slice := owner.schemaLocks[objectID]
	last := len(slice) - 1
	if record.schemaIndex != last {
		moved := slice[last]
		slice[record.schemaIndex] = moved
		moved.schemaIndex = record.schemaIndex
	}
	slice[last] = nil
	slice = slice[:last]
	if len(slice) == 0 {
		delete(owner.schemaLocks, objectID)
	} else {
		owner.schemaLocks[objectID] = slice
	}
	record.inSchemaMap = false
}

func (r *lockRegistry) removeRecordLocked(record *lockRecord, dirty *[]*objectLockBucket, updateOwnerIndex bool) {
	owner := record.owner
	objectID := record.request.ObjectID
	if bucket := r.objects[objectID]; bucket != nil {
		switch record.indexKind {
		case lockIndexPoint:
			removeKeyedLock(bucket.points, record.request.Generation, string(record.request.Lower.Key), record.bucketIndex)
		case lockIndexPrefix:
			removeKeyedLock(bucket.prefixes, record.request.Generation, string(record.request.Lower.Key), record.bucketIndex)
		default:
			last := len(bucket.wide) - 1
			if record.bucketIndex != last {
				moved := bucket.wide[last]
				bucket.wide[record.bucketIndex] = moved
				moved.bucketIndex = record.bucketIndex
			}
			bucket.wide[last] = nil
			bucket.wide = bucket.wide[:last]
		}
		bucket.total--
		if remaining := bucket.ownerCounts[owner] - 1; remaining <= 0 {
			delete(bucket.ownerCounts, owner)
		} else {
			bucket.ownerCounts[owner] = remaining
		}
		if bucket.waiters > 0 {
			if !bucket.pendingNotify {
				bucket.pendingNotify = true
				*dirty = append(*dirty, bucket)
			}
		} else if bucket.total == 0 {
			delete(r.objects, objectID)
		}
	}
	if updateOwnerIndex && owner != nil {
		switch record.indexKind {
		case lockIndexPoint:
			og := lockObjectGen{objectID: objectID, generation: record.request.Generation}
			removeOwnerKeyedLock(owner.pointLocks, og, string(record.request.Lower.Key), record.ownerIndex)
		case lockIndexPrefix:
			og := lockObjectGen{objectID: objectID, generation: record.request.Generation}
			removeOwnerKeyedLock(owner.prefixLocks, og, string(record.request.Lower.Key), record.ownerIndex)
		default:
			last := len(owner.wideLocks) - 1
			if record.ownerIndex != last {
				moved := owner.wideLocks[last]
				owner.wideLocks[record.ownerIndex] = moved
				moved.ownerIndex = record.ownerIndex
			}
			owner.wideLocks[last] = nil
			owner.wideLocks = owner.wideLocks[:last]
		}
		if record.inSchemaMap {
			removeOwnerSchemaLock(owner, record)
		}
	}
	r.usedBytes -= record.size
	r.lockCount--
	*record = lockRecord{}
}

func (r *lockRegistry) markBucketDirtyLocked(objectID model.ObjectID, dirty *[]*objectLockBucket) {
	if bucket := r.objects[objectID]; bucket != nil && bucket.waiters > 0 && !bucket.pendingNotify {
		bucket.pendingNotify = true
		*dirty = append(*dirty, bucket)
	}
}

func (r *lockRegistry) notifyBucketsLocked(dirty []*objectLockBucket) {
	for _, bucket := range dirty {
		bucket.pendingNotify = false
		close(bucket.changed)
		bucket.changed = make(chan struct{})
	}
}
