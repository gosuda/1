package exec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) ginScalarValue(graph physical.GraphID, root ir.ScalarID) (value model.Value, err error) {
	state, err := c.compile(graph, []ir.ScalarID{root}, nil)
	if err != nil {
		return model.Value{}, err
	}
	if c.preparing {
		return model.Value{}, nil
	}
	defer func() { err = errors.Join(err, state.Close()) }()
	input := columnar.Batch{Length: 1}
	var out columnar.Batch
	if err = state.Eval(c.portal.ctx, &input, c.portal.env.Parameters, &out, expr.Direct); err != nil {
		return model.Value{}, err
	}
	limit := c.portal.env.Limits.BatchBytes
	if limit == 0 {
		limit = 1 << 20
	}
	value, _, err = columnar.CopyValue(c.portal.ctx, &out.Columns[0], 0, limit)
	return value, err
}

// ginScanPrepare compiles and caches the GIN query RHS expression during
// statement preparation without opening any storage cursor.
func (c *invocation) ginScanPrepare(node physical.NodeSpec) error {
	if _, err := c.ginScalarValue(node.Scan.GIN.QueryExpr.Graph, node.Scan.GIN.QueryExpr.Scalar); err != nil {
		return err
	}
	if _, err := c.layouts(node.Output); err != nil {
		return err
	}
	return nil
}

// ginScan executes a GIN inverted index scan:
//  1. Evaluates the RHS query expression once at open time.
//  2. Extracts the GIN query token plan via builtin.ExtractGINQueryPlan.
//  3. If the query cannot be narrowed safely (ok == false or 0 tokens), falls
//     back to a full base-table scan (the parent Filter rechecks every row).
//  4. Prefix-scans the GIN posting index for each token, intersecting (when
//     RequireAll) or unioning (when !RequireAll) candidate base primary keys.
//  5. Fetches each candidate row from the base table via SnapshotReader.Get
//     (honoring MVCC visibility and overlay), packing projected output batches.
func (c *invocation) ginScan(node physical.NodeSpec) (storage.BatchCursor, error) {
	p := c.portal
	ctx := p.ctx
	spec := node.Scan
	ginSpec := spec.GIN
	lookup := spec.Lookup

	rhsVal, err := c.ginScalarValue(ginSpec.QueryExpr.Graph, ginSpec.QueryExpr.Scalar)
	if err != nil {
		return nil, err
	}
	outLayouts, err := c.layouts(node.Output)
	if err != nil {
		return nil, err
	}

	// Build output slot -> base table column ID mapping.
	baseColIDs := make([]model.ColumnID, 0, len(node.Output))
	outBaseIdx := make([]int, len(node.Output))
	for i, slot := range node.Output {
		graph, _ := c.plan.Graph(slot.Graph)
		info, _ := graph.Slot(slot.ID)
		baseCol := lookup.PhysicalColumn(info.Column)
		if baseCol == 0 {
			return nil, ErrCapability
		}
		idx := slices.Index(baseColIDs, baseCol)
		if idx < 0 {
			idx = len(baseColIDs)
			baseColIDs = append(baseColIDs, baseCol)
		}
		outBaseIdx[i] = idx
	}

	maxBytes := p.env.Limits.MetadataBytes
	if maxBytes == 0 {
		maxBytes = 1 << 20
	}
	plan, usable, err := builtin.ExtractGINQueryPlan(ctx, ginSpec.OperatorID, ginSpec.Layout, rhsVal, ginSpec.OpClass, p.env.Keys, maxBytes)
	if err != nil {
		return nil, err
	}
	if !usable || len(plan.Tokens) == 0 {
		return c.ginFallbackBaseScan(node)
	}

	candidates, claim, err := c.ginCollectCandidates(ctx, spec, plan)
	if err != nil {
		return nil, err
	}
	return &ginScanCursor{
		env:        &p.env,
		baseTarget: lookup.BaseRequest.Target,
		baseLayout: lookup.BaseRequest.Layout,
		baseColIDs: baseColIDs,
		outBaseIdx: outBaseIdx,
		outLayouts: outLayouts,
		candidates: candidates,
		claim:      claim,
	}, nil
}

// ginFallbackBaseScan streams all rows from the base table mapped to
// node.Output when a GIN query expression yields no pruning tokens (e.g. `{}`
// containment, NULL RHS, negated tsquery, or prefix tsquery). The parent Filter
// operator evaluates the exact SQL predicate on every row.
func (c *invocation) ginFallbackBaseScan(node physical.NodeSpec) (storage.BatchCursor, error) {
	p := c.portal
	lookup := node.Scan.Lookup
	baseSpec := physical.ScanSpec{
		Source:          lookup.BaseRequest.Target,
		ColumnMap:       lookup.BaseColumnMap,
		CapabilityEpoch: node.Scan.CapabilityEpoch,
		Request:         lookup.BaseRequest,
		Accepted:        lookup.BaseAccepted,
		Residual:        lookup.BaseResidual,
		Projection:      lookup.BaseProjection,
	}
	decision, err := scanDecision(p.ctx, storage.PushdownPlan{
		Request:    baseSpec.Request,
		Accepted:   baseSpec.Accepted,
		Residual:   baseSpec.Residual,
		Projection: baseSpec.Projection,
	}, p.env.Limits.MetadataBytes)
	if err != nil {
		return nil, err
	}
	input := &scanCursor{
		reader:         p.env.Reader,
		spec:           baseSpec,
		decision:       decision,
		maxBytes:       p.env.Limits.MetadataBytes,
		releasePlan:    true,
		ignoreOrdering: true,
	}
	mapping := make([]int, len(node.Output))
	for i, slot := range node.Output {
		graph, _ := c.plan.Graph(slot.Graph)
		info, _ := graph.Slot(slot.ID)
		baseCol := lookup.PhysicalColumn(info.Column)
		mapping[i] = slices.Index(lookup.BaseProjection, baseCol)
		if mapping[i] < 0 {
			return nil, errors.Join(ErrCapability, input.Close())
		}
	}
	return &expressionCursor{
		input:      input,
		env:        &p.env,
		projection: mapping,
		source:     columnar.Batch{Columns: make([]columnar.Vector, 0, len(lookup.BaseProjection))},
	}, nil
}

const ginPostingEntryOverhead = 48

func (c *invocation) ginCollectCandidates(ctx context.Context, spec physical.ScanSpec, plan builtin.GINQueryPlan) (_ [][]byte, claim columnar.Allocation, err error) {
	claim, err = c.portal.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil && claim != nil {
			claim.Release()
			claim = nil
		}
	}()

	var acc map[string]struct{}
	var accBytes uint64
	for i, tok := range plan.Tokens {
		if err := ctx.Err(); err != nil {
			return nil, claim, err
		}
		postings, tokBytes, scanErr := c.ginScanTokenPostings(ctx, spec, tok, claim, accBytes)
		if scanErr != nil {
			return nil, claim, scanErr
		}
		if plan.RequireAll {
			if len(postings) == 0 {
				claim.Release()
				return nil, nil, nil
			}
			if i == 0 {
				acc = postings
				accBytes = tokBytes
			} else {
				for pk := range acc {
					if _, ok := postings[pk]; !ok {
						delete(acc, pk)
						entryBytes := uint64(len(pk)) + ginPostingEntryOverhead
						if accBytes >= entryBytes {
							accBytes -= entryBytes
						}
					}
				}
				if len(acc) == 0 {
					claim.Release()
					return nil, nil, nil
				}
				if err := claim.Resize(accBytes); err != nil {
					return nil, claim, err
				}
			}
		} else {
			if acc == nil {
				acc = postings
				accBytes = tokBytes
			} else {
				for pk := range postings {
					if _, exists := acc[pk]; !exists {
						acc[pk] = struct{}{}
						accBytes += uint64(len(pk)) + ginPostingEntryOverhead
					}
				}
				if err := claim.Resize(accBytes); err != nil {
					return nil, claim, err
				}
			}
		}
	}
	if len(acc) == 0 {
		claim.Release()
		return nil, nil, nil
	}
	var outBytes uint64
	out := make([][]byte, 0, len(acc))
	for pk := range acc {
		out = append(out, []byte(pk))
		outBytes += uint64(len(pk)) + 24
	}
	if err := claim.Resize(outBytes); err != nil {
		return nil, claim, err
	}
	slices.SortFunc(out, bytes.Compare)
	return out, claim, nil
}

func (c *invocation) ginScanTokenPostings(ctx context.Context, spec physical.ScanSpec, tok []byte, claim columnar.Allocation, baseBytes uint64) (_ map[string]struct{}, usedBytes uint64, err error) {
	p := c.portal
	ownerCol := spec.Lookup.OwnerColumn
	req := storage.PushdownRequest{
		Target:     spec.Request.Target,
		Occurrence: spec.Request.Occurrence,
		IndexID:    spec.Request.IndexID,
		Layout:     spec.Request.Layout,
		KeyLayout:  spec.Request.KeyLayout,
		ColumnIDs:  []model.ColumnID{ownerCol},
		Direction:  storage.ScanForward,
		Spans: []storage.KeySpan{{
			Lower: storage.KeyBound{Values: []model.Value{model.BytesVal(tok)}, Inclusive: true},
			Upper: storage.KeyBound{Values: []model.Value{model.BytesVal(tok)}, Inclusive: true},
		}},
	}
	pushdownPlan, err := p.env.Reader.PlanScan(ctx, req)
	if err == nil || pushdownPlan.Binding != ([32]byte{}) {
		defer func() { err = errors.Join(err, p.env.Reader.ReleasePlan(pushdownPlan)) }()
	}
	if err != nil {
		return nil, 0, err
	}
	ownerIdx := slices.Index(pushdownPlan.Projection, ownerCol)
	if ownerIdx < 0 {
		return nil, 0, ErrCapability
	}
	cursor, err := p.env.Reader.VectorizedScan(ctx, pushdownPlan)
	if cursor != nil {
		defer func() { err = errors.Join(err, cursor.Close()) }()
	}
	if err != nil {
		return nil, 0, err
	}
	postings := make(map[string]struct{})
	batch := columnar.Batch{
		MaxRows: p.env.Limits.BatchRows,
		Columns: make([]columnar.Vector, 0, len(pushdownPlan.Projection)),
	}
	defer batch.Reset()
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		batch.Reset()
		if err := cursor.Next(ctx, &batch); err != nil {
			if errors.Is(err, io.EOF) {
				return postings, usedBytes, nil
			}
			return nil, 0, err
		}
		for row := range batch.Rows() {
			lane := batch.Lane(row)
			val, _, copyErr := columnar.CopyValue(ctx, &batch.Columns[ownerIdx], lane, p.env.Limits.BatchBytes)
			if copyErr != nil {
				return nil, 0, copyErr
			}
			if !val.IsNull && val.Type == model.TypeBytes && len(val.BytesVal) > 0 {
				pk := string(val.BytesVal)
				if _, exists := postings[pk]; !exists {
					usedBytes += uint64(len(pk)) + ginPostingEntryOverhead
					if claim != nil {
						if resizeErr := claim.Resize(baseBytes + usedBytes); resizeErr != nil {
							return nil, 0, resizeErr
						}
					}
					postings[pk] = struct{}{}
				}
			}
		}
	}
}

type ginScanCursor struct {
	env        *Environment
	baseTarget storage.ObjectRef
	baseLayout []storage.ColumnLayout
	baseColIDs []model.ColumnID
	outBaseIdx []int
	outLayouts []model.TypeLayout
	candidates [][]byte
	claim      columnar.Allocation
	next       int
	output     *columnar.Builder
	closed     bool
	closeErr   error
}

func (g *ginScanCursor) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if g.closed {
		return io.EOF
	}
	if g.next >= len(g.candidates) {
		if g.output != nil {
			err := g.output.Close()
			g.output = nil
			if err != nil {
				return err
			}
		}
		return io.EOF
	}
	var err error
	if g.output == nil {
		g.output, err = builder(g.env, g.outLayouts)
		if err != nil {
			return err
		}
	} else {
		g.output.Reset()
	}
	limit := batchRows(out, g.env)
	rowValues := make([]model.Value, len(g.outLayouts))
	req := storage.ReadRequest{
		Target:    g.baseTarget,
		ColumnIDs: g.baseColIDs,
		Layout:    g.baseLayout,
	}
	for g.next < len(g.candidates) && g.output.Rows() < limit {
		if err := ctx.Err(); err != nil {
			return err
		}
		req.Key = g.candidates[g.next]
		g.next++
		row, getErr := g.env.Reader.Get(ctx, req)
		if getErr != nil {
			if errors.Is(getErr, storage.ErrRowNotFound) {
				continue
			}
			return getErr
		}
		if row.Deleted {
			continue
		}
		if len(row.Values) != len(g.baseColIDs) {
			return ErrCapability
		}
		for i, srcIdx := range g.outBaseIdx {
			rowValues[i] = row.Values[srcIdx]
		}
		if err := g.output.Append(ctx, rowValues); err != nil {
			return err
		}
	}
	if g.output.Rows() == 0 {
		return io.EOF
	}
	return g.output.Publish(out)
}

func (g *ginScanCursor) Close() error {
	if g.closed {
		return g.closeErr
	}
	g.closed = true
	g.candidates = nil
	if g.claim != nil {
		g.claim.Release()
		g.claim = nil
	}
	if g.output != nil {
		g.closeErr = errors.Join(g.closeErr, g.output.Close())
		g.output = nil
	}
	return g.closeErr
}
