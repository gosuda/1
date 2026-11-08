package neosepia

import (
	"context"
	"crypto/rand"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type readLimits struct {
	SourceBytes, BlockBytes, MergeBytes, KeyBytes uint64
	ScanBytes, RowBytes, PredicateBytes           uint64
	OverlayBytes, OverlayBlockBytes, PlanBytes    uint64
	MaxRows, MaxNodes, MaxPlans                   int
}

type snapshotReader struct {
	mu         sync.Mutex
	pin        *runPin
	spec       storage.SnapshotSpec
	encoder    storage.KeyEncoder
	limits     readLimits
	plans      *boundScan
	planCount  int
	planBytes  uint64
	planSerial uint64
	identity   [16]byte
	closed     bool
}

type boundScan struct {
	next            *boundScan
	binding, digest [32]byte
	bytes           uint64
	active          atomic.Int64
	spec            paxScanSpec
}

var _ storage.SnapshotReader = (*snapshotReader)(nil)

// The backend authenticates and pins the published fence before construction.
// Ownership of pin transfers even when construction fails.
func newSnapshotReader(ctx context.Context, pin *runPin, spec storage.SnapshotSpec, encoder storage.KeyEncoder, limits readLimits) (storage.SnapshotReader, error) {
	if pin == nil {
		return nil, storage.ErrClosed
	}
	fail := func(err error) (storage.SnapshotReader, error) { return nil, errors.Join(err, pin.Close()) }
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := spec.Validate(); err != nil {
		return fail(err)
	}
	if limits.MaxRows <= 0 || limits.MaxNodes <= 0 || limits.MaxPlans <= 0 {
		return fail(columnar.ErrBatchBudget)
	}
	for _, maximum := range [...]uint64{limits.SourceBytes, limits.BlockBytes, limits.MergeBytes, limits.KeyBytes, limits.ScanBytes, limits.RowBytes, limits.PredicateBytes, limits.OverlayBytes, limits.OverlayBlockBytes, limits.PlanBytes} {
		if maximum == 0 {
			return fail(columnar.ErrBatchBudget)
		}
	}
	if err := pin.bindTimestamp(ctx, spec.Timestamp); err != nil {
		return fail(err)
	}
	if spec.Overlay != nil {
		retained, err := spec.Overlay.Retain()
		if err != nil {
			return fail(err)
		}
		spec.Overlay = retained
	}
	reader := &snapshotReader{pin: pin, spec: spec, encoder: encoder, limits: limits}
	rand.Read(reader.identity[:])
	return reader, nil
}

func (reader *snapshotReader) Get(ctx context.Context, request storage.ReadRequest) (model.Row, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return model.Row{}, storage.ErrClosed
	}
	limits := reader.limits
	return reader.pin.get(ctx, reader.spec.Timestamp, reader.spec.Overlay, request, pinnedPointLimits{
		SourceBytes: limits.SourceBytes, BlockBytes: limits.BlockBytes,
		Merge: mergeLimits{MaxBytes: limits.MergeBytes, MaxKeyBytes: limits.KeyBytes},
		Point: pointReadLimits{ScanBytes: limits.ScanBytes, RowBytes: limits.RowBytes},
	})
}

func (reader *snapshotReader) VectorizedScan(ctx context.Context, plan storage.PushdownPlan) (storage.BatchCursor, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return nil, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var bound *boundScan
	for candidate := reader.plans; candidate != nil; candidate = candidate.next {
		if candidate.binding == plan.Binding {
			bound = candidate
			break
		}
	}
	if bound == nil {
		return nil, storage.ErrInvalidScanPlan
	}
	digest, err := storage.HashPushdownPlan(ctx, plan, reader.limits.PlanBytes)
	if err != nil {
		return nil, errors.Join(storage.ErrInvalidScanPlan, err)
	}
	if digest != bound.digest {
		return nil, storage.ErrInvalidScanPlan
	}
	limits := reader.limits
	cursor, err := reader.pin.scan(ctx, reader.spec.Timestamp, plan.Request.Direction, bound.spec, pinnedScanLimits{
		SourceBytes: limits.SourceBytes, BlockBytes: limits.BlockBytes,
		Merge:   mergeLimits{MaxBytes: limits.MergeBytes, MaxKeyBytes: limits.KeyBytes},
		Reverse: mergeLimits{MaxBytes: limits.MergeBytes, MaxKeyBytes: limits.KeyBytes},
	})
	if err != nil {
		return nil, err
	}
	bound.active.Add(1)
	cursor.bound = bound
	return cursor, nil
}

func (reader *snapshotReader) ReleasePlan(plan storage.PushdownPlan) error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return nil
	}
	for link := &reader.plans; *link != nil; link = &(*link).next {
		bound := *link
		if bound.binding != plan.Binding {
			continue
		}
		if bound.active.Load() != 0 {
			return storage.ErrInvalidScanPlan
		}
		*link = bound.next
		bound.next = nil
		reader.planCount--
		reader.planBytes -= bound.bytes
		return nil
	}
	return storage.ErrInvalidScanPlan
}

func (reader *snapshotReader) Close() error {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return nil
	}
	reader.closed = true
	for reader.plans != nil {
		bound := reader.plans
		reader.plans = bound.next
		bound.next = nil
	}
	reader.planCount = 0
	reader.planBytes = 0
	var overlayErr error
	if reader.spec.Overlay != nil {
		overlayErr = reader.spec.Overlay.Close()
		reader.spec.Overlay = nil
	}
	return errors.Join(overlayErr, reader.pin.Close())
}
