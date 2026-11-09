package exec

import (
	"context"
	"errors"
	"io"
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/routing"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exjoin "github.com/gosuda/supersql-alpha/internal/query/exec/join"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

type indexOwnerLookupFactory struct {
	env     *Environment
	scan    physical.ScanSpec
	decoder storage.KeyDecoder
	owner   int
}

func (c *invocation) secondaryIndexLookup(node physical.NodeSpec, input storage.BatchCursor) (storage.BatchCursor, error) {
	env, lookup := &c.portal.env, node.Scan.Lookup
	closeInput := func(err error) error {
		if input == nil {
			return err
		}
		return errors.Join(err, input.Close())
	}
	decoder, ok := env.Keys.(storage.KeyDecoder)
	if !ok {
		return nil, closeInput(ErrCapability)
	}
	count := len(node.Scan.Projection) + len(lookup.BaseProjection) + len(node.Output)
	bytes := uint64(count)*uint64(unsafe.Sizeof(model.TypeLayout{})) + uint64(len(node.Output))*uint64(unsafe.Sizeof(exjoin.Column{}))
	if err := c.charge(bytes); err != nil {
		return nil, closeInput(err)
	}
	options := env.Join
	options.Kind, options.MarkKind, options.ExactLookup = ir.InnerJoin, 0, true
	options.Query, options.Encoder = env.Query, env.Keys
	options.BatchRows, options.BatchBytes = env.Limits.BatchRows, env.Limits.BatchBytes
	options.Keys, options.Residual, options.ResidualInputs, options.Parameters = nil, nil, nil, nil
	options.LeftLayouts = make([]model.TypeLayout, len(node.Scan.Projection))
	for i, id := range node.Scan.Projection {
		layout, ok := lookupColumnLayout(node.Scan.Request.Layout, id)
		if !ok {
			return nil, closeInput(ErrCapability)
		}
		options.LeftLayouts[i] = layout
	}
	options.RightLayouts = make([]model.TypeLayout, len(lookup.BaseProjection))
	for i, id := range lookup.BaseProjection {
		layout, ok := lookupColumnLayout(lookup.BaseRequest.Layout, id)
		if !ok {
			return nil, closeInput(ErrCapability)
		}
		options.RightLayouts[i] = layout
	}
	var err error
	options.OutputLayouts, err = c.layouts(node.Output)
	if err != nil {
		return nil, closeInput(err)
	}
	options.Output = make([]exjoin.Column, len(node.Output))
	for i, slot := range node.Output {
		graph, _ := c.plan.Graph(slot.Graph)
		info, _ := graph.Slot(slot.ID)
		if column := node.Scan.PhysicalColumn(info.Column); column != 0 {
			position := slices.Index(node.Scan.Projection, column)
			if position >= 0 {
				options.Output[i] = exjoin.Column{Side: exjoin.Left, Index: position}
				continue
			}
		}
		column := lookup.PhysicalColumn(info.Column)
		position := slices.Index(lookup.BaseProjection, column)
		if column == 0 || position < 0 {
			return nil, closeInput(ErrCapability)
		}
		options.Output[i] = exjoin.Column{Side: exjoin.Right, Index: position}
	}
	owner := slices.Index(node.Scan.Projection, lookup.OwnerColumn)
	if owner < 0 {
		return nil, closeInput(ErrCapability)
	}
	if err := exjoin.CheckLookupSupport(c.portal.ctx, options); err != nil {
		return nil, closeInput(err)
	}
	if c.preparing {
		return nil, nil
	}
	base := physical.ScanSpec{
		Source:          lookup.BaseRequest.Target,
		ColumnMap:       lookup.BaseColumnMap,
		CapabilityEpoch: node.Scan.CapabilityEpoch,
		Request:         lookup.BaseRequest,
		Accepted:        lookup.BaseAccepted,
		Residual:        lookup.BaseResidual,
		Projection:      lookup.BaseProjection,
	}
	factory := &indexOwnerLookupFactory{env: env, scan: base, decoder: decoder, owner: owner}
	return exjoin.NewLookup(c.portal.ctx, input, factory, options)
}

func lookupColumnLayout(columns []storage.ColumnLayout, id model.ColumnID) (model.TypeLayout, bool) {
	for _, column := range columns {
		if column.ID == id {
			return model.TypeLayout{Type: column.Type, Children: column.Children}, true
		}
	}
	return model.TypeLayout{}, false
}

func (f *indexOwnerLookupFactory) Open(ctx context.Context, left []model.Value) (storage.BatchCursor, error) {
	limit := f.env.Limits.MetadataBytes
	fixed := uint64(unsafe.Sizeof(lookupProbe{})) + uint64(unsafe.Sizeof(scanCursor{})) + uint64(unsafe.Sizeof(exactRowCursor{}))
	fixed += uint64(len(f.scan.Projection))*uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(len(f.scan.Request.KeyLayout))*uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(storage.PushdownPlan{}))
	if f.owner < 0 || f.owner >= len(left) || left[f.owner].IsNull || left[f.owner].Type != model.TypeBytes || fixed >= limit {
		return nil, ErrCapability
	}
	claim, err := f.env.Query.Allocator(runtime.WorkMemory).Reserve(limit)
	if err != nil {
		return nil, err
	}
	probe := &lookupProbe{claim: claim}
	decodeBytes := (limit - fixed) / 2
	if decodeBytes == 0 {
		return nil, errors.Join(columnar.ErrBatchBudget, probe.Close())
	}
	values, err := f.decoder.DecodeKey(ctx, f.scan.Request.KeyLayout, left[f.owner].BytesVal, decodeBytes)
	if err != nil {
		return nil, errors.Join(err, probe.Close())
	}
	prefix := storage.KeySpan{Lower: storage.KeyBound{Values: values, Inclusive: true}, Upper: storage.KeyBound{Values: values, Inclusive: true}}
	limits := routing.DefaultLimits()
	limits.MaxBytes = limit - fixed - decodeBytes
	spans, err := routing.Intersect(ctx, f.scan.Request.Spans, []storage.KeySpan{prefix}, f.scan.Request.KeyLayout, f.env.Keys, limits)
	if err != nil {
		return nil, errors.Join(err, probe.Close())
	}
	if len(spans) != 1 {
		return nil, errors.Join(ErrCapability, probe.Close())
	}
	spec := f.scan
	spec.Request.Spans = spans
	decision, err := scanDecision(ctx, storage.PushdownPlan{Request: spec.Request, Accepted: spec.Accepted, Residual: spec.Residual, Projection: spec.Projection}, limit)
	if err != nil {
		return nil, errors.Join(err, probe.Close())
	}
	cursor := &scanCursor{reader: f.env.Reader, spec: spec, decision: decision, maxBytes: limit, releasePlan: true, ignoreOrdering: true}
	probe.cursor = &exactRowCursor{cursor: cursor}
	return probe, nil
}

type exactRowCursor struct {
	cursor     storage.BatchCursor
	seen, done bool
}

func (c *exactRowCursor) Next(ctx context.Context, out *columnar.Batch) error {
	if c.done {
		out.Reset()
		return io.EOF
	}
	for {
		err := c.cursor.Next(ctx, out)
		if err == io.EOF {
			c.done = true
			out.Reset()
			if !c.seen {
				return ErrCapability
			}
			return io.EOF
		}
		if err != nil {
			return err
		}
		if out.Rows() == 0 {
			continue
		}
		if c.seen || out.Rows() != 1 {
			return ErrCapability
		}
		c.seen = true
		return nil
	}
}

func (c *exactRowCursor) Close() error {
	c.done = true
	return c.cursor.Close()
}

type lookupFactory struct {
	env      *Environment
	scan     physical.ScanSpec
	keys     []exjoin.Key
	mapping  []int
	program  *expr.Program
	layouts  []model.TypeLayout
	captures *columnar.Batch
}

func (c *invocation) lookup(node physical.NodeSpec, children []storage.BatchCursor, options exjoin.Options) (storage.BatchCursor, error) {
	right := &c.prepared.nodes[node.Children[1]]
	if right.node.Algorithm != physical.PrimaryKeyScan && right.node.Algorithm != physical.SecondaryScan {
		return nil, ErrCapability
	}
	spec := right.node.Scan
	wrongTarget := node.Join.Lookup != spec.Request.Target || node.Join.LookupIndex != spec.Request.IndexID
	limited := spec.Request.SafeLimit.Enabled || spec.AppliedLimit.Enabled || spec.LimitOwner != 0
	if wrongTarget || limited || len(options.Keys) == 0 || len(options.Keys) > len(spec.Request.KeyLayout) {
		return nil, ErrCapability
	}
	for i, key := range node.Join.Keys {
		graph, _ := c.plan.Graph(key.Right.Slot.Graph)
		info, ok := graph.Slot(key.Right.Slot.ID)
		if !ok || spec.PhysicalColumn(info.Column) != spec.Request.KeyLayout[i].ColumnID || key.Right.Type != spec.Request.KeyLayout[i].Type {
			return nil, ErrCapability
		}
		column := options.Keys[i].Right
		if column < 0 || column >= len(options.RightLayouts) {
			return nil, ErrCapability
		}
		expected := spec.Request.KeyLayout[i]
		expected.Type = options.RightLayouts[column].Type
		expected.Children = options.RightLayouts[column].Children
		if !storage.KeyLayoutsEqual([]storage.KeyColumn{expected}, spec.Request.KeyLayout[i:i+1]) {
			return nil, ErrCapability
		}
	}
	if err := exjoin.CheckLookupSupport(c.portal.ctx, options); err != nil {
		return nil, err
	}
	if c.preparing {
		return nil, nil
	}
	if err := c.charge(uint64(unsafe.Sizeof(lookupFactory{})) + uint64(len(right.node.Output))*uint64(unsafe.Sizeof(int(0)))); err != nil {
		return nil, err
	}
	factory := &lookupFactory{env: &c.portal.env, scan: spec, keys: options.Keys, mapping: make([]int, len(right.node.Output)), captures: c.captures}
	for i, slot := range right.node.Output {
		graph, _ := c.plan.Graph(slot.Graph)
		info, _ := graph.Slot(slot.ID)
		factory.mapping[i] = slices.Index(spec.Projection, spec.PhysicalColumn(info.Column))
		if factory.mapping[i] < 0 {
			return nil, ErrCapability
		}
	}
	if !spec.Residual.Empty() {
		if len(right.programs) != 1 {
			return nil, ErrInvalid
		}
		factory.program, factory.layouts = right.programs[0], right.residualLayouts
	}
	// The immutable RHS node supplies metadata, never a full-scan input stream.
	if children[1] != nil {
		if err := children[1].Close(); err != nil {
			return nil, err
		}
	}
	return exjoin.NewLookup(c.portal.ctx, children[0], factory, options)
}

func (f *lookupFactory) Open(ctx context.Context, left []model.Value) (storage.BatchCursor, error) {
	limit := f.env.Limits.MetadataBytes
	fixed := uint64(unsafe.Sizeof(lookupProbe{})) + uint64(unsafe.Sizeof(scanCursor{})) + 2*uint64(unsafe.Sizeof(expressionCursor{}))
	fixed += uint64(len(f.scan.Projection))*7*uint64(unsafe.Sizeof(columnar.Vector{})) + uint64(f.env.Limits.BatchRows)*4
	fixed += uint64(len(f.keys))*uint64(unsafe.Sizeof(model.Value{})) + uint64(unsafe.Sizeof(storage.PushdownPlan{}))
	if fixed >= limit {
		return nil, columnar.ErrBatchBudget
	}
	claim, err := f.env.Query.Allocator(runtime.WorkMemory).Reserve(limit)
	if err != nil {
		return nil, err
	}
	probe := &lookupProbe{claim: claim}
	values := make([]model.Value, len(f.keys))
	for i, key := range f.keys {
		if key.Left < 0 || key.Left >= len(left) {
			return nil, errors.Join(ErrInvalid, probe.Close())
		}
		values[i] = left[key.Left]
	}
	prefix := storage.KeySpan{Lower: storage.KeyBound{Values: values, Inclusive: true}, Upper: storage.KeyBound{Values: values, Inclusive: true}}
	limits := routing.DefaultLimits()
	limits.MaxBytes = limit - fixed
	spans, err := routing.Intersect(ctx, f.scan.Request.Spans, []storage.KeySpan{prefix}, f.scan.Request.KeyLayout, f.env.Keys, limits)
	if err != nil {
		return nil, errors.Join(err, probe.Close())
	}
	if len(spans) == 0 {
		claim.Release()
		probe.claim = nil
		return probe, nil
	}
	spec := f.scan
	spec.Request.Spans = spans
	decision, err := scanDecision(ctx, storage.PushdownPlan{Request: spec.Request, Accepted: spec.Accepted, Residual: spec.Residual, Projection: spec.Projection, Ordering: spec.Ordering, AppliedLimit: spec.AppliedLimit}, limit)
	if err != nil {
		return nil, errors.Join(err, probe.Close())
	}
	var input storage.BatchCursor = &scanCursor{reader: f.env.Reader, spec: spec, decision: decision, maxBytes: limit, releasePlan: true}
	probe.cursor = input
	if f.program != nil {
		state, err := expr.NewState(f.program, expr.StateOptions{Captures: f.captures, WorkAllocator: f.env.Query.Allocator(runtime.WorkMemory), OutputAllocator: f.env.Query.Allocator(runtime.OutputMemory), Sequences: f.env.Sequences, Relations: f.env.Relations, Temporal: f.env.Temporal, Settings: f.env.Settings, TextSearch: f.env.TextSearch, Advisory: f.env.Advisory, Native: f.env.Native})
		if err != nil {
			return nil, errors.Join(err, probe.Close())
		}
		input = &expressionCursor{input: input, state: state, env: f.env, layouts: f.layouts, filter: true,
			source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(f.layouts))}, evaluated: columnar.Batch{Columns: make([]columnar.Vector, 0, 1)},
			compact: columnar.Batch{Columns: make([]columnar.Vector, 0, len(f.layouts))}, selection: make([]uint32, 0, f.env.Limits.BatchRows)}
	}
	probe.cursor = &expressionCursor{input: input, env: f.env, projection: f.mapping, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(spec.Projection))}}
	return probe, nil
}

type lookupProbe struct {
	cursor   storage.BatchCursor
	claim    columnar.Allocation
	closed   bool
	closeErr error
}

func (p *lookupProbe) Next(ctx context.Context, out *columnar.Batch) error {
	out.Reset()
	if p.closed || p.cursor == nil {
		return io.EOF
	}
	return p.cursor.Next(ctx, out)
}

func (p *lookupProbe) Close() error {
	if p.closed {
		return p.closeErr
	}
	p.closed = true
	if p.cursor != nil {
		p.closeErr = p.cursor.Close()
		p.cursor = nil
	}
	if p.claim != nil {
		p.claim.Release()
		p.claim = nil
	}
	return p.closeErr
}
