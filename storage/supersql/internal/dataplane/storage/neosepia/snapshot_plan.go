package neosepia

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func (reader *snapshotReader) PlanScan(ctx context.Context, request storage.PushdownRequest) (storage.PushdownPlan, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if reader.closed {
		return storage.PushdownPlan{}, storage.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return storage.PushdownPlan{}, err
	}
	limits := reader.limits
	if reader.planCount >= limits.MaxPlans || reader.planSerial == math.MaxUint64 {
		return storage.PushdownPlan{}, columnar.ErrBatchBudget
	}
	available := limits.PlanBytes - reader.planBytes
	used := uint64(unsafe.Sizeof(boundScan{}))
	if used > available {
		return storage.PushdownPlan{}, columnar.ErrBatchBudget
	}
	owned, copied, err := storage.CopyPushdownRequest(ctx, request, available-used)
	if err != nil {
		return storage.PushdownPlan{}, err
	}
	used += copied
	if err := owned.Validate(ctx); err != nil {
		return storage.PushdownPlan{}, err
	}
	spans, spanBytes, err := compileSpans(ctx, owned, reader.encoder, min(limits.KeyBytes, available-used))
	if err != nil {
		return storage.PushdownPlan{}, err
	}
	used += spanBytes
	program, exact, err := compilePredicate(ctx, owned.Predicate, owned.Layout, limits.MaxNodes, min(limits.PredicateBytes, available-used))
	if err != nil {
		return storage.PushdownPlan{}, err
	}
	programBytes := predicateOwnedBytes(program)
	if programBytes > available-used {
		return storage.PushdownPlan{}, columnar.ErrBatchBudget
	}
	used += programBytes
	projection := owned.ColumnIDs
	if !exact {
		var projectionBytes uint64
		projection, projectionBytes, err = residualProjection(ctx, owned, limits.MaxNodes, available-used)
		if err != nil {
			return storage.PushdownPlan{}, err
		}
		used += projectionBytes
	}
	public, publicBytes, err := storage.CopyPushdownRequest(ctx, owned, available-used)
	if err != nil {
		return storage.PushdownPlan{}, err
	}
	used += publicBytes
	plan := storage.PushdownPlan{Request: public, Projection: public.ColumnIDs, SnapshotTS: reader.spec.Timestamp, FenceID: reader.spec.Fence.ID}
	if exact {
		plan.Accepted = public.Predicate
		plan.AppliedLimit = public.SafeLimit
	} else {
		plan.Residual = public.Predicate
		bytes := uint64(len(projection)) * uint64(unsafe.Sizeof(model.ColumnID(0)))
		if bytes > available-used {
			return storage.PushdownPlan{}, columnar.ErrBatchBudget
		}
		used += bytes
		plan.Projection = make([]model.ColumnID, len(projection))
		copy(plan.Projection, projection)
	}
	orderBytes := uint64(len(public.KeyLayout)) * uint64(unsafe.Sizeof(storage.ScanOrdering{}))
	if orderBytes > available-used {
		return storage.PushdownPlan{}, columnar.ErrBatchBudget
	}
	used += orderBytes
	plan.Ordering = make([]storage.ScanOrdering, len(public.KeyLayout))
	for i, column := range public.KeyLayout {
		descending, nullsFirst := column.Descending, column.NullsFirst
		if public.Direction == storage.ScanReverse {
			descending = !descending
			nullsFirst = !nullsFirst
		}
		plan.Ordering[i] = storage.ScanOrdering{KeyOrdinal: uint32(i), Type: column.Type, Descending: descending, NullsFirst: nullsFirst}
	}
	if err := reader.pin.visitSnapshot(ctx, owned.Target, reader.spec.Timestamp, func(file *sstFile) error {
		first, end, err := file.spanCandidates(ctx, spans, reader.spec.Timestamp)
		if err != nil {
			return err
		}
		for block := first; block < end; block++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			entry := file.entry(block)
			plan.Estimate.Rows += float64(entry.rows)
			plan.Estimate.VisibilityChecks += float64(entry.rows)
			plan.Estimate.ReadBytes += float64(entry.length)
		}
		return nil
	}); err != nil {
		return storage.PushdownPlan{}, err
	}
	digest, err := storage.HashPushdownPlan(ctx, plan, limits.PlanBytes)
	if err != nil {
		return storage.PushdownPlan{}, err
	}
	copy(plan.Binding[:16], reader.identity[:])
	reader.planSerial++
	binary.LittleEndian.PutUint64(plan.Binding[16:24], reader.planSerial)
	copy(plan.Binding[24:], digest[:8])
	reader.plans = &boundScan{
		next: reader.plans, binding: plan.Binding, digest: digest, bytes: used,
		spec: paxScanSpec{
			Target: owned.Target, Layout: owned.Layout, Projection: projection, Spans: spans, Predicate: program, Limit: plan.AppliedLimit,
			MaxRows: limits.MaxRows, MaxBytes: limits.ScanBytes, PredicateBytes: limits.PredicateBytes,
			Overlay: reader.spec.Overlay, OverlayLimits: mergeLimits{MaxBytes: limits.OverlayBytes, MaxKeyBytes: limits.KeyBytes}, OverlayBlockBytes: limits.OverlayBlockBytes,
		},
	}
	reader.planCount++
	reader.planBytes += used
	return plan, nil
}

func predicateOwnedBytes(program *predicateProgram) uint64 {
	if program == nil {
		return 0
	}
	used := uint64(unsafe.Sizeof(predicateProgram{})) +
		uint64(cap(program.cols))*uint64(unsafe.Sizeof(model.ColumnID(0))) +
		uint64(cap(program.colTypes))*uint64(unsafe.Sizeof(model.Type{})) +
		uint64(cap(program.colKinds))*uint64(unsafe.Sizeof(predColKind(0))) +
		uint64(cap(program.consts))*uint64(unsafe.Sizeof(model.Value{})) +
		uint64(cap(program.nodes))*uint64(unsafe.Sizeof(predCompiledNode{}))
	for _, node := range program.nodes {
		used += uint64(cap(node.members)) * uint64(unsafe.Sizeof(predScalarRef{}))
	}
	return used
}

func residualProjection(ctx context.Context, request storage.PushdownRequest, maxNodes int, maxBytes uint64) ([]model.ColumnID, uint64, error) {
	reachable, err := predReachable(ctx, request.Predicate, min(maxNodes, int(min(maxBytes/2048, uint64(math.MaxInt)))))
	if err != nil {
		return nil, 0, err
	}
	used := uint64(len(reachable)) * 2048
	unit := uint64(unsafe.Sizeof(model.ColumnID(0)))
	if len(request.ColumnIDs) > math.MaxInt-len(reachable) {
		return nil, 0, columnar.ErrBatchBudget
	}
	slots := uint64(len(request.ColumnIDs))*2 + uint64(len(reachable))*2
	if slots > (maxBytes-used)/unit {
		return nil, 0, columnar.ErrBatchBudget
	}
	used += slots * unit
	existing := make([]model.ColumnID, len(request.ColumnIDs))
	copy(existing, request.ColumnIDs)
	slices.Sort(existing)
	extra := make([]model.ColumnID, 0, len(reachable))
	for _, index := range reachable {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		node := request.Predicate.Node(index)
		if node.Kind == storage.PredicateColumn {
			extra = append(extra, node.ColumnID)
		}
	}
	slices.Sort(extra)
	extra = slices.Compact(extra)
	result := make([]model.ColumnID, len(request.ColumnIDs), len(request.ColumnIDs)+len(reachable))
	copy(result, request.ColumnIDs)
	for _, column := range extra {
		if _, found := slices.BinarySearch(existing, column); !found {
			result = append(result, column)
		}
	}
	return result, used, nil
}
