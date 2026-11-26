package exec

import (
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/exchange"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) parallelScanEligible(node physical.NodeSpec) bool {
	env, spec := &c.portal.env, node.Scan
	if spec.Lookup.Enabled || env.Query.Parallelism() < 2 || env.Keys == nil || env.Exchange.MaxKeyBytes == 0 || env.Exchange.WorkBytes == 0 {
		return false
	}
	if len(spec.Request.Spans) < 2 || len(spec.Request.KeyLayout) == 0 || !(node.Estimate.Rows >= 65536) || math.IsInf(node.Estimate.Rows, 0) {
		return false
	}
	if spec.Request.SafeLimit.Enabled || spec.AppliedLimit.Enabled || spec.LimitOwner != 0 {
		return false
	}
	owner, ok := c.graph.Owner(c.current)
	if !ok {
		return false
	}
	pipeline, ok := c.graph.Pipeline(owner)
	if !ok || !pipeline.ParallelEligible() {
		return false
	}
	// Only the delivered prefix needs to survive the gather boundary. Storage
	// may advertise further key columns that its projection does not expose.
	if len(node.Delivered.Ordering) > len(spec.Ordering) {
		return false
	}
	for _, order := range spec.Ordering[:len(node.Delivered.Ordering)] {
		if uint64(order.KeyOrdinal) >= uint64(len(spec.Request.KeyLayout)) {
			return false
		}
		key := spec.Request.KeyLayout[order.KeyOrdinal]
		if order.Type != key.Type || !slices.Contains(spec.Projection, key.ColumnID) {
			return false
		}
	}
	return true
}

func (c *invocation) scanInput(node physical.NodeSpec, decision [32]byte) (_ storage.BatchCursor, err error) {
	env, spec := &c.portal.env, node.Scan
	if !c.parallelScanEligible(node) {
		return &scanCursor{reader: env.Reader, spec: spec, decision: decision, maxBytes: env.Limits.MetadataBytes}, nil
	}
	groups := min(env.Query.Parallelism(), len(spec.Request.Spans))
	limit := env.Limits.MetadataBytes
	fixed := uint64(unsafe.Sizeof(parallelScanCursor{})) + uint64(unsafe.Sizeof(storage.KeySpan{}))
	for _, allocation := range [...]struct{ count, bytes uint64 }{
		{uint64(groups), uint64(unsafe.Sizeof(scanCursor{})) + uint64(unsafe.Sizeof(storage.PushdownPlan{})) + uint64(unsafe.Sizeof(storage.BatchCursor(nil)))},
		{uint64(len(spec.Projection)), uint64(unsafe.Sizeof(model.TypeLayout{}))},
		{uint64(len(node.Delivered.Ordering)), uint64(unsafe.Sizeof(exsort.Key{}))},
	} {
		if fixed > limit || allocation.count > (limit-fixed)/allocation.bytes {
			return nil, columnar.ErrBatchBudget
		}
		fixed += allocation.count * allocation.bytes
	}
	if fixed >= limit {
		return nil, columnar.ErrBatchBudget
	}
	claim, err := env.Query.Allocator(runtime.WorkMemory).Reserve(limit)
	if err != nil {
		return nil, err
	}
	var inputs []storage.BatchCursor
	defer func() {
		if err != nil {
			for _, input := range inputs {
				if input != nil {
					err = errors.Join(err, input.Close())
				}
			}
			claim.Release()
		}
	}()
	limits := routing.DefaultLimits()
	limits.MaxBytes = limit - fixed
	full := storage.KeySpan{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}
	spans, err := routing.Intersect(c.portal.ctx, spec.Request.Spans, []storage.KeySpan{full}, spec.Request.KeyLayout, env.Keys, limits)
	if err != nil {
		return nil, err
	}
	if len(spans) < 2 {
		// Canonicalization is the last eligibility proof, before group creation.
		claim.Release()
		return &scanCursor{reader: env.Reader, spec: spec, decision: decision, maxBytes: limit}, nil
	}
	groups = min(groups, len(spans))
	layouts := make([]model.TypeLayout, len(spec.Projection))
	for i, id := range spec.Projection {
		found := false
		for _, column := range spec.Request.Layout {
			if column.ID == id {
				layouts[i] = model.TypeLayout{Type: column.Type, Children: column.Children}
				found = true
				break
			}
		}
		if !found {
			return nil, ErrCapability
		}
	}
	sources := exchange.Options{
		Query: env.Query, Layouts: layouts, BatchRows: env.Limits.BatchRows, BatchBytes: env.Limits.BatchBytes,
		QueueBytes: env.Exchange.WorkBytes, MetadataBytes: limit, ParallelEligible: true, EstimatedRows: node.Estimate.Rows,
	}
	keys := make([]exsort.Key, len(node.Delivered.Ordering))
	for i, order := range spec.Ordering[:len(keys)] {
		column := slices.Index(spec.Projection, spec.Request.KeyLayout[order.KeyOrdinal].ColumnID)
		if layouts[column].Type != order.Type {
			return nil, ErrCapability
		}
		keys[i] = exsort.Key{Column: column, Descending: order.Descending, NullsFirst: order.NullsFirst, InputCollation: order.Type.Collation}
	}
	inputs = make([]storage.BatchCursor, groups)
	start := 0
	for i := range groups {
		end := start + len(spans)/groups
		if i < len(spans)%groups {
			end++
		}
		group := spec
		group.Request.Spans = spans[start:end:end]
		decision, err := scanDecision(c.portal.ctx, storage.PushdownPlan{Request: group.Request, Accepted: group.Accepted, Residual: group.Residual, Projection: group.Projection, Ordering: group.Ordering, AppliedLimit: group.AppliedLimit}, limit)
		if err != nil {
			return nil, err
		}
		inputs[i] = &scanCursor{reader: env.Reader, spec: group, decision: decision, maxBytes: limit, releasePlan: true}
		start = end
	}
	var gathered storage.BatchCursor
	if len(keys) == 0 {
		gathered, err = exchange.NewGather(c.portal.ctx, inputs, sources)
	} else {
		gathered, err = exchange.NewOrderedMerge(c.portal.ctx, inputs, exchange.MergeOptions{
			Sources: sources, Keys: keys, Encoder: env.Keys, MaxKeyBytes: env.Exchange.MaxKeyBytes, WorkBytes: env.Exchange.WorkBytes,
		})
	}
	// Both exchange constructors take every input, including on failure.
	inputs = nil
	if err != nil {
		return nil, err
	}
	return &parallelScanCursor{input: gathered, claim: claim}, nil
}

type parallelScanCursor struct {
	input    storage.BatchCursor
	claim    columnar.Allocation
	closed   bool
	closeErr error
}

func (s *parallelScanCursor) Next(ctx context.Context, out *columnar.Batch) error {
	if s.closed {
		out.Reset()
		return io.EOF
	}
	return s.input.Next(ctx, out)
}

func (s *parallelScanCursor) Close() error {
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	s.closeErr = s.input.Close()
	s.input = nil
	s.claim.Release()
	s.claim = nil
	return s.closeErr
}
