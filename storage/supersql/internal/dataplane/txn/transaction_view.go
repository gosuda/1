package txn

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// ReadView reports the immutable data/catalog timestamp actually selected when
// its dependency locks and snapshot pin were acquired.
type ReadView interface {
	storage.SnapshotReader
	Timestamp() model.Timestamp
}

type transactionView struct {
	mu                sync.Mutex
	transaction       *Transaction
	publicationOwner  *model.PublicationOwner
	reader            storage.SnapshotReader
	timestamp         model.Timestamp
	identity          CatalogReadIdentity
	life              context.Context
	cancel            context.CancelCauseFunc
	closed            atomic.Bool
	dependencies      []transactionDependency
	scopedOccurrences bool
	serial, used      uint64
	plans             []transactionPlan
	cursors           []*transactionCursor
	closeError        error
}

// viewBudget is a transaction's shared allowance for retained read views: each
// view's declared dependency set plus every scan plan it registers. Views draw
// from the allowance while they are open and return their charge when they
// close, so one long view may use the whole allowance while the transaction's
// total retained view memory stays bounded. Dividing the allowance by the
// maximum view count instead would reject a single view whose declaration is
// proportional to the catalog, which is exactly the shape binding produces.
type viewBudget struct {
	mu          sync.Mutex
	limit, used uint64
}

// available reports the allowance not yet charged to an open view.
func (budget *viewBudget) available() uint64 {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return budget.limit - budget.used
}

// charge reserves bytes for one view; it never waits for another view to close.
func (budget *viewBudget) charge(bytes uint64) error {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if bytes > budget.limit-budget.used {
		return errTransactionBudget
	}
	budget.used += bytes
	return nil
}

// release returns a view's charge; the caller must have charged the same bytes.
func (budget *viewBudget) release(bytes uint64) {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	budget.used -= bytes
}

// remaining reports the allowance left after already charged bytes, saturating
// at zero so a concurrent view that grew the charge cannot underflow the limit.
func (budget *viewBudget) remaining(charged uint64) uint64 {
	available := budget.available()
	if charged >= available {
		return 0
	}
	return available - charged
}

type transactionPlan struct {
	binding [32]byte
	bytes   uint64
}

func (view *transactionView) Timestamp() model.Timestamp { return view.timestamp }

// CatalogIdentity names the catalog rows this view exposes. The value is
// captured when the view's snapshot is opened, so it stays stable for the whole
// view and names exactly the content every read through the view sees.
func (view *transactionView) CatalogIdentity() CatalogReadIdentity { return view.identity }

// ObserveSnapshot forwards optional same-snapshot physical observations through
// the transaction wrapper without exposing its borrowed backend reader.
func (view *transactionView) ObserveSnapshot(ctx context.Context, target storage.ObjectRef) (storage.SnapshotObservation, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.closed.Load() {
		return storage.SnapshotObservation{}, storage.ErrClosed
	}
	observer, ok := view.reader.(storage.SnapshotObserver)
	if !ok {
		return storage.SnapshotObservation{Target: target, Snapshot: view.timestamp}, nil
	}
	operation, finish := linkedTransactionContext(ctx, view.life)
	defer finish()
	if err := context.Cause(operation); err != nil {
		return storage.SnapshotObservation{}, transactionError(err)
	}
	observation, err := observer.ObserveSnapshot(operation, target)
	return observation, transactionError(err)
}

// Linked operations join the cancellation callback before releasing ownership.
func linkedTransactionContext(ctx, life context.Context) (context.Context, func()) {
	operation, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	stop := context.AfterFunc(life, func() { cancel(context.Cause(life)); close(done) })
	return operation, func() {
		if stop() {
			close(done)
		}
		<-done
		cancel(context.Canceled)
	}
}

func (view *transactionView) covered(target storage.ObjectRef, request lockRequest) bool {
	for _, dependency := range view.dependencies {
		if dependency.target != target {
			continue
		}
		for _, span := range dependency.spans {
			if lockContains(span, request) {
				return true
			}
		}
	}
	return false
}

// spanLowerBound returns the first values of a span's lower bound, which for a
// catalog collection is the row's encoded key.
func spanLowerBound(span storage.KeySpan) []byte {
	if len(span.Lower.Values) == 0 || span.Lower.Values[0].Type != model.TypeBytes {
		return nil
	}
	return span.Lower.Values[0].BytesVal
}

func (view *transactionView) dependencyOccurrenceMatches(dependency transactionDependency, occurrence model.ScanOccurrence) bool {
	return dependency.occurrence == occurrence || !view.scopedOccurrences && dependency.occurrence == 0
}

func (view *transactionView) coveredScan(target storage.ObjectRef, occurrence model.ScanOccurrence, request lockRequest) bool {
	for _, dependency := range view.dependencies {
		if dependency.target != target || !view.dependencyOccurrenceMatches(dependency, occurrence) {
			continue
		}
		for _, span := range dependency.spans {
			if lockContains(span, request) {
				return true
			}
		}
	}
	return false
}

func (view *transactionView) Get(ctx context.Context, request storage.ReadRequest) (model.Row, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.closed.Load() {
		return model.Row{}, storage.ErrClosed
	}
	operation, finish := linkedTransactionContext(ctx, view.life)
	defer finish()
	if err := context.Cause(operation); err != nil {
		return model.Row{}, transactionError(err)
	}
	if view.transaction.mode == ReadWrite && !view.covered(request.Target, pointLock(request.Target, request.Key)) {
		return model.Row{}, transactionCoverageError(request.Target, request.Key)
	}
	row, err := view.reader.Get(operation, request)
	return row, transactionError(err)
}

func (view *transactionView) PlanScan(ctx context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.closed.Load() {
		return storage.PushdownPlan{}, storage.ErrClosed
	}
	operation, finish := linkedTransactionContext(ctx, view.life)
	defer finish()
	if err := context.Cause(operation); err != nil {
		return storage.PushdownPlan{}, transactionError(err)
	}
	if len(view.plans) >= view.transaction.maxViews {
		return storage.PushdownPlan{}, errTransactionBudget
	}
	if err := request.Validate(operation); err != nil {
		return storage.PushdownPlan{}, err
	}
	if view.transaction.mode == ReadWrite {
		declared := false
		eligible := false
		for _, dependency := range view.dependencies {
			if dependency.target == request.Target && view.dependencyOccurrenceMatches(dependency, request.Occurrence) && storage.KeyLayoutsEqual(dependency.keyLayout, request.KeyLayout) {
				declared = true
				eligible = eligible || dependency.eligible
			}
		}
		if !declared {
			return storage.PushdownPlan{}, transactionCoverageError(request.Target, nil)
		}
		if eligible {
			spans, err := view.admittedEncodedSpans(operation, request)
			if err != nil {
				return storage.PushdownPlan{}, err
			}
			request.Spans = nil
			request.EncodedSpans = spans
		} else {
			for _, span := range request.Spans {
				encoded, _, err := encodeDependencySpan(operation, view.transaction.encoder, request.Target, request.KeyLayout, span, view.transaction.viewSpace.remaining(view.used))
				if err != nil {
					return storage.PushdownPlan{}, err
				}
				if !view.coveredScan(request.Target, request.Occurrence, encoded) {
					return storage.PushdownPlan{}, transactionCoverageError(request.Target, spanLowerBound(span))
				}
			}
		}
	}
	// Charge the immutable request that the backend retains for this plan, plus
	// its returned plan metadata. Failed planning does not grow the registration set.
	overhead := uint64(unsafe.Sizeof(storage.PushdownPlan{}))
	var registryGrowth uint64
	if len(view.plans) == cap(view.plans) {
		registryGrowth = uint64(unsafe.Sizeof(transactionPlan{}))
	}
	available := view.transaction.viewSpace.available()
	if overhead+registryGrowth > available {
		return storage.PushdownPlan{}, errTransactionBudget
	}
	owned, bytes, err := storage.CopyPushdownRequest(operation, request, (available-overhead-registryGrowth)/3)
	if err != nil {
		return storage.PushdownPlan{}, errors.Join(errTransactionBudget, err)
	}
	charged := bytes*3 + overhead + registryGrowth
	if err := view.transaction.viewSpace.charge(charged); err != nil {
		return storage.PushdownPlan{}, err
	}
	plan, err := view.reader.PlanScan(operation, owned)
	if err != nil {
		view.transaction.viewSpace.release(charged)
		return storage.PushdownPlan{}, transactionError(err)
	}
	if registryGrowth != 0 {
		plans := make([]transactionPlan, len(view.plans), len(view.plans)+1)
		copy(plans, view.plans)
		view.plans = plans
	}
	view.used += charged
	view.plans = append(view.plans, transactionPlan{binding: plan.Binding, bytes: charged - registryGrowth})
	return plan, nil
}

// NegotiateScan negotiates scan capabilities against the view's underlying
// snapshot reader and immediately releases the backend plan binding so it
// cannot be used to open a scan cursor without declared dependency coverage.
func (view *transactionView) NegotiateScan(ctx context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.closed.Load() {
		return storage.PushdownPlan{}, storage.ErrClosed
	}
	operation, finish := linkedTransactionContext(ctx, view.life)
	defer finish()
	if err := context.Cause(operation); err != nil {
		return storage.PushdownPlan{}, transactionError(err)
	}
	if err := request.Validate(operation); err != nil {
		return storage.PushdownPlan{}, err
	}
	available := view.transaction.viewSpace.available()
	owned, _, err := storage.CopyPushdownRequest(operation, request, available/3)
	if err != nil {
		return storage.PushdownPlan{}, errors.Join(errTransactionBudget, err)
	}
	plan, err := view.reader.PlanScan(operation, owned)
	if err == nil || plan.Binding != ([32]byte{}) {
		err = errors.Join(err, view.reader.ReleasePlan(plan))
	}
	if err != nil {
		return storage.PushdownPlan{}, transactionError(err)
	}
	return plan, nil
}

func (view *transactionView) admittedEncodedSpans(ctx context.Context, request storage.PushdownRequest) ([]storage.EncodedKeySpan, error) {
	unit := uint64(unsafe.Sizeof(storage.EncodedKeySpan{}))
	available := view.transaction.viewSpace.remaining(view.used)
	headerBudget := available / 2
	maxParts := int(min(headerBudget/unit, uint64(^uint(0)>>1)))
	if maxParts == 0 && len(request.Spans) != 0 {
		return nil, errTransactionBudget
	}
	keyBudget := available - headerBudget
	var keyUsed uint64
	parts := make([]lockRequest, 0, min(maxParts, len(request.Spans)))
	for _, span := range request.Spans {
		encoded, bytes, err := encodeDependencySpan(ctx, view.transaction.encoder, request.Target, request.KeyLayout, span, keyBudget-keyUsed)
		if err != nil {
			return nil, err
		}
		keyUsed += bytes
		for _, dependency := range view.dependencies {
			if dependency.target != request.Target || dependency.occurrence != request.Occurrence || !dependency.eligible || !storage.KeyLayoutsEqual(dependency.keyLayout, request.KeyLayout) {
				continue
			}
			for _, admitted := range dependency.spans {
				part, ok := intersectLockRequests(encoded, admitted)
				if !ok {
					continue
				}
				if len(parts) == maxParts {
					return nil, errTransactionBudget
				}
				parts = append(parts, part)
			}
		}
	}
	slices.SortFunc(parts, compareLockRequestLower)
	merged := parts[:0]
	for _, part := range parts {
		if len(merged) == 0 || lockEndsBefore(merged[len(merged)-1].Upper, part.Lower) {
			merged = append(merged, part)
			continue
		}
		if compareLockUpper(part.Upper, merged[len(merged)-1].Upper) > 0 {
			merged[len(merged)-1].Upper = part.Upper
		}
	}
	result := make([]storage.EncodedKeySpan, len(merged))
	for i, part := range merged {
		result[i] = storage.EncodedKeySpan{Lower: storageEncodedLockBound(part.Lower), Upper: storageEncodedLockBound(part.Upper)}
	}
	return result, nil
}

func intersectLockRequests(a, b lockRequest) (lockRequest, bool) {
	if a.ObjectID != b.ObjectID || a.Generation != b.Generation {
		return lockRequest{}, false
	}
	result := a
	if compareLockLower(b.Lower, result.Lower) > 0 {
		result.Lower = b.Lower
	}
	if compareLockUpper(b.Upper, result.Upper) < 0 {
		result.Upper = b.Upper
	}
	return result, validLockRequest(result) && lockContains(a, result) && lockContains(b, result)
}

func storageEncodedLockBound(bound lockBound) storage.EncodedKeyBound {
	return storage.EncodedKeyBound{Key: bound.Key, Inclusive: bound.Inclusive, Prefix: bound.Prefix, Unbounded: bound.Unbounded}
}

func (view *transactionView) ReleasePlan(plan storage.PushdownPlan) error {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.closed.Load() {
		return nil
	}
	index := slices.IndexFunc(view.plans, func(registered transactionPlan) bool {
		return registered.binding == plan.Binding
	})
	if index < 0 {
		return storage.ErrInvalidScanPlan
	}
	for _, cursor := range view.cursors {
		if cursor.binding == plan.Binding && !cursor.closed.Load() {
			return storage.ErrInvalidScanPlan
		}
	}
	if err := view.reader.ReleasePlan(plan); err != nil {
		return transactionError(err)
	}
	view.used -= view.plans[index].bytes
	view.transaction.viewSpace.release(view.plans[index].bytes)
	view.plans = slices.Delete(view.plans, index, index+1)
	return nil
}

func (view *transactionView) VectorizedScan(ctx context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.closed.Load() {
		return nil, storage.ErrClosed
	}
	operation, finish := linkedTransactionContext(ctx, view.life)
	defer finish()
	if err := context.Cause(operation); err != nil {
		return nil, transactionError(err)
	}
	if !slices.ContainsFunc(view.plans, func(registered transactionPlan) bool {
		return registered.binding == plan.Binding
	}) {
		return nil, storage.ErrInvalidScanPlan
	}
	retained := view.cursors[:0]
	for _, cursor := range view.cursors {
		if !cursor.closed.Load() {
			retained = append(retained, cursor)
		}
	}
	clear(view.cursors[len(retained):])
	view.cursors = retained
	if !view.transaction.reserveResource() {
		return nil, errTransactionBudget
	}
	cursor, err := view.reader.VectorizedScan(operation, plan)
	if err != nil {
		view.transaction.resources.Add(-1)
		return nil, transactionError(err)
	}
	life, cancel := context.WithCancelCause(view.life)
	wrapped := &transactionCursor{view: view, cursor: cursor, binding: plan.Binding, life: life, cancel: cancel}
	view.cursors = append(view.cursors, wrapped)
	return wrapped, nil
}

func (view *transactionView) Close() error {
	view.cancel(context.Canceled)
	view.mu.Lock()
	defer view.mu.Unlock()
	if view.closed.Load() {
		return view.closeError
	}
	for _, cursor := range view.cursors {
		view.closeError = errors.Join(view.closeError, cursor.Close())
	}
	view.closeError = errors.Join(view.closeError, view.reader.Close())
	view.reader = nil
	view.cursors, view.dependencies, view.plans = nil, nil, nil
	view.transaction.viewSpace.release(view.used)
	view.used = 0
	view.closed.Store(true)
	view.transaction.resources.Add(-1)
	return view.closeError
}

type transactionCursor struct {
	mu         sync.Mutex
	view       *transactionView
	cursor     storage.BatchCursor
	binding    [32]byte
	life       context.Context
	cancel     context.CancelCauseFunc
	closed     atomic.Bool
	closeError error
}

func (cursor *transactionCursor) Next(ctx context.Context, batch *columnar.Batch) error {
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if cursor.closed.Load() {
		return storage.ErrClosed
	}
	operation, finish := linkedTransactionContext(ctx, cursor.life)
	defer finish()
	if err := context.Cause(operation); err != nil {
		return transactionError(err)
	}
	return transactionError(cursor.cursor.Next(operation, batch))
}

func (cursor *transactionCursor) Close() error {
	cursor.cancel(context.Canceled)
	cursor.mu.Lock()
	defer cursor.mu.Unlock()
	if cursor.closed.Load() {
		return cursor.closeError
	}
	cursor.closeError = cursor.cursor.Close()
	cursor.cursor = nil
	cursor.closed.Store(true)
	cursor.view.transaction.resources.Add(-1)
	return cursor.closeError
}
