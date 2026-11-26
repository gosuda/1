package exec

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/catalogrel"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exrecursive "github.com/gosuda/supersql-alpha/internal/query/exec/recursive"
	exsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
	"github.com/gosuda/supersql-alpha/internal/query/fragment"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func preflight(graph *fragment.FragmentGraph, env Environment) error {
	plan := graph.Plan()
	for id := physical.NodeID(1); id <= physical.NodeID(plan.NodeCount()); id++ {
		node, _ := plan.Node(id)
		meta := node.Metadata()
		gap := func(reason string) error {
			return &RequiredExecutionError{Node: id, Algorithm: meta.Algorithm, Reason: reason}
		}
		if _, ok := graph.Owner(id); !ok {
			return ErrInvalid
		}
		if err := partitionFlow(plan, id, node); err != nil {
			return err
		}
		switch meta.Algorithm {
		case physical.Values, physical.GenerateSeries, physical.GenerateSubscripts, physical.Unnest, physical.RowsFrom, physical.Filter, physical.Project, physical.ProjectSet, physical.Limit, physical.UnionAll:
		case physical.LocalGather:
		case physical.LocalHashRepartition, physical.LocalOrderedMerge:
			if env.Keys == nil || env.Exchange.MaxKeyBytes == 0 || env.Exchange.WorkBytes == 0 {
				return gap("exchange encoder and work limits required")
			}
		case physical.Apply:
			if env.Apply.MaxRowBytes == 0 || env.Apply.WorkBytes == 0 {
				return gap("Apply capture and row limits required")
			}
		case physical.RecursiveUnion:
			if env.Keys == nil || env.Recursive.MaxIterations == 0 || !validSpoolLimits(env.Recursive.Spool) {
				return gap("recursive worktable limits and encoder required")
			}
		case physical.WorktableScan:
		case physical.TableScan, physical.PrimaryKeyScan, physical.SecondaryScan:
			if env.Reader == nil {
				return gap("snapshot reader required")
			}
			if node.HasLookupReadSpan() {
				if _, ok := env.Keys.(storage.KeyDecoder); !ok {
					return gap("secondary lookup key decoder required")
				}
				if env.Join.MaxRecordBytes == 0 || env.Join.WorkBytes == 0 {
					return ErrInvalid
				}
			}
		case physical.CatalogScan:
			if env.Catalog == nil {
				return gap("logical catalog source required")
			}
		case physical.LookupJoin:
			if env.Reader == nil || env.Keys == nil {
				return gap("lookup snapshot reader and key encoder required")
			}
			if env.Join.MaxRecordBytes == 0 || env.Join.WorkBytes == 0 {
				return ErrInvalid
			}
		case physical.HashJoin, physical.NestedLoopJoin, physical.MergeJoin:
			if env.Keys == nil {
				return gap("SQL key encoder required")
			}
			j := env.Join
			invalidBytes := j.MaxRecordBytes == 0 || j.HashBytes == 0 || j.WorkBytes == 0 || j.PartitionBytes == 0
			if invalidBytes || j.MaxPartitions <= 0 || j.MaxDepth <= 0 {
				return ErrInvalid
			}
		case physical.HashAggregate, physical.StreamAggregate:
			if env.Keys == nil {
				return gap("SQL key encoder required")
			}
			a := env.Aggregate
			if a.MaxRecordBytes == 0 || a.GroupBytes == 0 || a.WorkBytes == 0 || a.PartitionMemoryBytes == 0 || a.MaxPartitions < 2 {
				return ErrInvalid
			}
		case physical.Sort, physical.TopK:
			if env.Keys == nil {
				return gap("SQL key encoder required")
			}
			if env.Sort.RunBytes == 0 || env.Sort.WorkBytes == 0 || env.Sort.MaxRecordBytes == 0 || env.Sort.MergeFanIn < 2 || env.Sort.MaxRuns < env.Sort.MergeFanIn {
				return fmt.Errorf("sort node %d limits: %w", id, ErrInvalid)
			}
		case physical.HashDistinct, physical.StreamDistinct, physical.HashSetOp:
			if env.Keys == nil {
				return gap("SQL key encoder required")
			}
			s := env.Set
			invalidBytes := s.MaxRecordBytes == 0 || s.GroupBytes == 0 || s.MaxSpillBytes == 0 || s.WorkBytes == 0
			if invalidBytes || s.MaxPasses <= 0 {
				return fmt.Errorf("set node %d limits: %w", id, ErrInvalid)
			}
		case physical.Window:
			if env.Keys == nil {
				return gap("SQL key encoder required")
			}
			w := env.Window
			if w.MaxRecordBytes == 0 || w.MaxPartitionBytes == 0 || w.MaxPartitionRows == 0 || w.WorkBytes == 0 {
				return fmt.Errorf("window node %d limits: %w", id, ErrInvalid)
			}
		case physical.Sample:
			logicalGraph, graphOK := plan.Graph(meta.Logical.Graph)
			if !graphOK {
				return ErrInvalid
			}
			logical, relationOK := logicalGraph.Rel(meta.Logical.Rel)
			if !relationOK {
				return ErrInvalid
			}
			if !logical.SampleRepeatable && env.SampleSeed == 0 {
				return gap("invocation sample seed required")
			}
		case physical.LockRows:
			if env.Keys == nil || env.Modify.Transaction == nil || env.Modify.MaxRecordBytes == 0 {
				return gap("row-lock transaction and key encoder required")
			}
		case physical.Modify:
			logicalGraph, graphOK := plan.Graph(meta.Logical.Graph)
			if !graphOK {
				return ErrInvalid
			}
			logical, relationOK := logicalGraph.Rel(meta.Logical.Rel)
			if !relationOK {
				return ErrInvalid
			}
			needsKeys := logical.ModifyTarget == ir.PhysicalModifyTarget || logical.ModifyTarget == ir.AutomaticViewModifyTarget
			if needsKeys && env.Keys == nil || env.Modify.Transaction == nil || env.MutationTargets == nil {
				return gap("mutation transaction and target capabilities required")
			}
		case physical.Materialize, physical.CTEMaterialize, physical.CTEScan:
			if !validSpoolLimits(env.Spool) {
				return fmt.Errorf("materialization node %d limits: %w", id, ErrInvalid)
			}
		default:
			return gap("algorithm has no streaming implementation")
		}
		if meta.Limit.WithTies && (node.Delivered().OrderingCount() == 0 || env.Keys == nil) {
			return gap("WITH TIES requires a resolved output ordering and SQL key encoder")
		}
		ordered := meta.Algorithm == physical.Sort || meta.Algorithm == physical.TopK || meta.Algorithm == physical.LocalOrderedMerge
		if ordered || meta.Limit.WithTies {
			for i := range node.Delivered().OrderingCount() {
				key, _ := node.Delivered().Ordering(i)
				if !canonicalOrderingOperator(env.Keys, key.Operator, key.Key.Type, key.Direction) {
					return gap("ordering operator lacks canonical SQL key semantics")
				}
			}
		}
	}
	return nil
}

func canonicalOrderingOperator(keys storage.KeyEncoder, operator model.OperatorID, typ model.Type, direction ir.Direction) bool {
	op, ok := builtin.LookupOperator(operator)
	name := "<"
	if direction == ir.Descending {
		name = ">"
	}
	if !ok || op.Left != op.Right || op.Result != 16 || op.Name != name {
		return false
	}
	if builtin.KeyOrderingEquivalent(typ, op.Left) {
		return true
	}
	equivalence, declared := keys.(interface {
		OrderingEquivalent(model.Type, model.TypeID) bool
	})
	return declared && equivalence.OrderingEquivalent(typ, op.Left)
}

func (p *Portal) copyParameters() error {
	claim, err := p.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return err
	}
	p.claims = append(p.claims, claim)
	source := p.env.Parameters
	used := uint64(len(source)) * uint64(unsafe.Sizeof(expr.ParameterValue{}))
	if used > p.env.Limits.ParameterBytes {
		return columnar.ErrBatchBudget
	}
	p.env.Parameters = make([]expr.ParameterValue, len(source))
	for i, param := range source {
		if param.Number == 0 {
			return ErrInvalid
		}
		for _, prior := range source[:i] {
			if prior.Number == param.Number {
				return ErrInvalid
			}
		}
		layout, size, err := model.CopyTypeLayout(p.ctx, param.Layout, p.env.Limits.ParameterBytes-used)
		if err != nil {
			return err
		}
		used += size
		if err = layout.Validate(); err != nil {
			return err
		}
		value, size, err := model.CopyValue(p.ctx, param.Value, p.env.Limits.ParameterBytes-used)
		if err != nil {
			return err
		}
		used += size
		if value.Type != layout.Type.Storage {
			return ErrInvalid
		}
		if p.env.Builtin.Types != nil || p.env.Builtin.InformationSchemaTypes != nil {
			output := min(p.env.Limits.Expressions.MaxOutputBytes, uint64(math.MaxInt64))
			budget := builtin.CallBudget{MemoryBytes: p.env.Limits.Expressions.MaxStateBytes, OutputBytes: int64(output)}
			if err := builtin.ValidateResolvedValue(p.ctx, p.env.Builtin, layout, value, budget, p.env.Limits.Expressions.MaxDepth); err != nil {
				return err
			}
		}
		p.env.Parameters[i] = expr.ParameterValue{Number: param.Number, Layout: layout, Value: value}
	}
	return claim.Resize(used)
}

type invocation struct {
	portal          *Portal
	graph           *fragment.FragmentGraph
	plan            *physical.Plan
	used            uint64
	prepared        *preparedPlan
	domain          physical.NodeID
	current         physical.NodeID
	programIndex    int
	preparing       bool
	captures        *columnar.Batch
	parent          *invocation
	shared          []*sharedProducer
	residualProgram bool
	captureValues   []model.Value
	worktableRoot   physical.NodeID
	delta           exrecursive.Worktable
}

func (c *invocation) charge(bytes uint64) error {
	if bytes > c.portal.env.Limits.MetadataBytes-c.used {
		return columnar.ErrBatchBudget
	}
	c.used += bytes
	return nil
}

func (c *invocation) build() (storage.BatchCursor, error) {
	p := c.portal
	claim, err := p.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return nil, err
	}
	p.claims = append(p.claims, claim)
	if err = c.charge(uint64(c.plan.NodeCount()) * 64); err != nil {
		return nil, err
	}
	if err = c.charge(uint64(c.plan.NodeCount()) * 2048); err != nil {
		return nil, err
	}
	consumers := make([]uint32, c.plan.NodeCount()+1)
	for id := physical.NodeID(1); id <= physical.NodeID(c.plan.NodeCount()); id++ {
		node, _ := c.plan.Node(id)
		for i := range node.ChildCount() {
			child, _ := node.Child(i)
			consumers[child]++
		}
		if node.Metadata().Algorithm == physical.CTEScan {
			consumers[node.Metadata().Worktable.Producer]++
		}
	}
	cursors := make([]storage.BatchCursor, c.plan.NodeCount()+1)
	shared := make([]*sharedProducer, c.plan.NodeCount()+1)
	c.shared = shared
	for id := physical.NodeID(1); id <= physical.NodeID(c.plan.NodeCount()); id++ {
		prepared := &c.prepared.nodes[id]
		if prepared.domain != c.domain {
			continue
		}
		c.current, c.programIndex = id, 0
		if err = p.ctx.Err(); err != nil {
			return nil, err
		}
		node, layouts := prepared.node, prepared.layouts
		// Reusable input/evaluation/compaction headers and physical selections are
		// owned here, rather than by columnar.Builder's payload account.
		width := len(node.Output)
		for _, childID := range node.Children {
			child, _ := c.plan.Node(childID)
			width = max(width, int(child.OutputCount()))
		}
		width = max(width, len(node.Scan.Projection))
		if err = c.charge(uint64(width)*(4*uint64(unsafe.Sizeof(columnar.Vector{}))+4*uint64(unsafe.Sizeof(model.Value{}))+128) + uint64(p.env.Limits.BatchRows)*8); err != nil {
			return nil, err
		}
		if cap(p.env.row) < width {
			p.env.row = make([]model.Value, width)
		}
		if err = c.charge(uint64(len(node.Children)) * uint64(unsafe.Sizeof(storage.BatchCursor(nil)))); err != nil {
			return nil, err
		}
		children := make([]storage.BatchCursor, len(node.Children))
		for i, child := range node.Children {
			if (node.Algorithm == physical.Apply || node.Algorithm == physical.RecursiveUnion) && i == 1 {
				continue
			}
			children[i] = cursors[child]
			if shared[child] != nil {
				children[i], err = c.sharedReader(shared[child], shared[child].layouts)
				if err != nil {
					return nil, err
				}
				if p.env.Instrumentation != nil {
					childNode, _ := c.plan.Node(child)
					if childNode.Metadata().Algorithm == physical.CTEScan && consumers[child] > 1 {
						children[i] = p.env.Instrumentation.wrap(child, physical.CTEScan, children[i])
					}
				}
			}
			if children[i] == nil {
				return nil, ErrInvalid
			}
		}
		if node.Algorithm == physical.CTEMaterialize {
			if p.env.Instrumentation != nil {
				children[0] = p.env.Instrumentation.wrap(id, node.Algorithm, children[0])
			}
			shared[id], err = c.sharedProducer(children[0], layouts)
			if err != nil {
				return nil, err
			}
			if id == c.root() {
				cursors[id], err = c.sharedReader(shared[id], layouts)
				if err != nil {
					return nil, err
				}
			}
			continue
		}
		if node.Algorithm == physical.CTEScan {
			producer := c.producer(node.Worktable.Producer)
			if producer == nil {
				return nil, ErrInvalid
			}
			if consumers[id] > 1 {
				shared[id] = producer
				continue
			}
			cursors[id], err = c.sharedReader(producer, layouts)
			if err != nil {
				return nil, err
			}
			if p.env.Instrumentation != nil {
				cursors[id] = p.env.Instrumentation.wrap(id, node.Algorithm, cursors[id])
			}
			continue
		}
		baseContext := p.ctx
		if p.env.Instrumentation != nil {
			p.ctx = p.env.Instrumentation.withNode(p.ctx, id)
		}
		cursor, err := c.node(id, node, layouts, children)
		p.ctx = baseContext
		if err != nil {
			return nil, fmt.Errorf("open execution node %d (algorithm %d): %w", id, node.Algorithm, err)
		}
		cursor, err = c.preservePartition(node, children, cursor)
		if err != nil {
			return nil, err
		}
		if p.env.Instrumentation != nil {
			cursor = p.env.Instrumentation.wrap(id, node.Algorithm, cursor)
			if observed, ok := cursor.(*modifyObservation); ok && id == c.plan.Root() {
				p.mutation = observed
			}
		}
		if consumers[id] > 1 {
			if !validSpoolLimits(p.env.Spool) {
				return nil, errors.Join(fmt.Errorf("shared producer %d requires spool limits: %w", id, ErrInvalid), cursor.Close())
			}
			shared[id], err = c.sharedProducer(cursor, layouts)
			if err != nil {
				return nil, err
			}
		} else {
			cursors[id] = cursor
			p.owned = append(p.owned, cursor)
		}
	}
	if c.domain == 0 {
		root := c.prepared.nodes[c.plan.Root()].node
		if err = c.charge(uint64(len(root.StatementDependencies)) * uint64(unsafe.Sizeof((*sharedProducer)(nil)))); err != nil {
			return nil, err
		}
		p.mandatory = make([]*sharedProducer, 0, len(root.StatementDependencies))
		for _, dependency := range root.StatementDependencies {
			if shared[dependency] == nil {
				return nil, ErrInvalid
			}
			p.mandatory = append(p.mandatory, shared[dependency])
		}
	}
	if err = claim.Resize(c.used); err != nil {
		return nil, err
	}
	return cursors[c.root()], nil
}

func (c *invocation) layout(graph *ir.Graph, id ir.LayoutID, depth int) (model.TypeLayout, error) {
	if err := c.portal.ctx.Err(); err != nil {
		return model.TypeLayout{}, err
	}
	if depth > 64 {
		return model.TypeLayout{}, ErrInvalid
	}
	typ, ok := graph.Type(id)
	if !ok {
		return model.TypeLayout{}, ErrInvalid
	}
	count := graph.LayoutChildCount(id)
	if err := c.charge(uint64(count) * uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
		return model.TypeLayout{}, err
	}
	result := model.TypeLayout{Type: typ, Children: make([]model.TypeLayout, count)}
	if names := graph.LayoutFieldNameCount(id); names != 0 {
		if typ.Storage != model.TypeComposite || names != count {
			return model.TypeLayout{}, ErrInvalid
		}
		if err := c.charge(uint64(names) * uint64(unsafe.Sizeof(""))); err != nil {
			return model.TypeLayout{}, err
		}
		result.FieldNames = make([]string, names)
		for i := range names {
			if err := c.portal.ctx.Err(); err != nil {
				return model.TypeLayout{}, err
			}
			name, ok := graph.LayoutFieldName(id, i)
			if !ok {
				return model.TypeLayout{}, ErrInvalid
			}
			if err := c.charge(uint64(len(name))); err != nil {
				return model.TypeLayout{}, err
			}
			result.FieldNames[i] = strings.Clone(name)
		}
	}
	for i := range count {
		child, _ := graph.LayoutChild(id, i)
		var err error
		result.Children[i], err = c.layout(graph, child, depth+1)
		if err != nil {
			return model.TypeLayout{}, err
		}
	}
	return result, nil
}
func (c *invocation) layouts(slots []physical.Slot) ([]model.TypeLayout, error) {
	if err := c.charge(uint64(len(slots)) * uint64(unsafe.Sizeof(model.TypeLayout{}))); err != nil {
		return nil, err
	}
	result := make([]model.TypeLayout, len(slots))
	for i, slot := range slots {
		graph, ok := c.plan.Graph(slot.Graph)
		if !ok {
			return nil, ErrInvalid
		}
		var err error
		result[i], err = c.layout(graph, slot.Layout, 0)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (c *invocation) compile(graphID physical.GraphID, roots []ir.ScalarID, slots []physical.Slot) (*expr.State, error) {
	program, err := c.program(graphID, roots, slots)
	if err != nil {
		return nil, err
	}
	return c.state(program)
}

func (c *invocation) program(graphID physical.GraphID, roots []ir.ScalarID, slots []physical.Slot) (*expr.Program, error) {
	graph, ok := c.plan.Graph(graphID)
	if !ok {
		return nil, ErrInvalid
	}
	return c.programIn(graphID, graph, roots, slots)
}

func (c *invocation) programIn(graphID physical.GraphID, graph *ir.Graph, roots []ir.ScalarID, slots []physical.Slot) (*expr.Program, error) {
	// A node lowers the same ordered scalar groups for every invocation.
	// Only immutable programs are cached; states belong to the current Portal.
	prepared := &c.prepared.nodes[c.current]
	index := c.programIndex
	c.programIndex++
	if index < len(prepared.programs) {
		return prepared.programs[index], nil
	}
	if !c.preparing {
		return nil, ErrInvalid
	}
	p := c.portal
	options := expr.Options{Limits: p.env.Limits.Expressions, Builtin: p.env.Builtin, Functions: p.env.Functions}
	if err := c.charge(uint64(len(slots)+len(prepared.node.Delivered.Parameters))*uint64(unsafe.Sizeof(expr.Input{})) + uint64(len(p.env.Parameters))*uint64(unsafe.Sizeof(expr.Parameter{})) + uint64(unsafe.Sizeof((*expr.Program)(nil)))); err != nil {
		return nil, err
	}
	for i, slot := range slots {
		if slot.Graph != graphID {
			return nil, ErrInvalid
		}
		options.Inputs = append(options.Inputs, expr.Input{Slot: slot.ID, Column: i})
	}
	if !c.residualProgram {
		for _, slot := range prepared.node.Delivered.Parameters {
			if joinColumn(slots, slot) >= 0 {
				continue
			}
			column := joinColumn(c.prepared.nodes[prepared.domain].captureSlots, slot)
			if column < 0 {
				return nil, ErrInvalid
			}
			options.Captures = append(options.Captures, expr.Input{Slot: slot.ID, Column: column})
		}
	}
	for port, subplan := range prepared.node.ScalarSubplans {
		if c.residualProgram || subplan.Scalar.Graph != graphID {
			continue
		}
		body := &c.prepared.nodes[subplan.Root]
		if err := c.charge(uint64(unsafe.Sizeof(expr.Subquery{})) + uint64(len(body.captureSlots))*uint64(unsafe.Sizeof(model.SlotID(0)))); err != nil {
			return nil, err
		}
		descriptor := expr.Subquery{Scalar: subplan.Scalar.Scalar, ID: scalarSubqueryID(c.current, uint32(port)), Captures: make([]model.SlotID, len(body.captureSlots))}
		for i, slot := range body.captureSlots {
			if slot.Graph != graphID {
				return nil, ErrInvalid
			}
			descriptor.Captures[i] = slot.ID
		}
		options.Subqueries = append(options.Subqueries, descriptor)
	}
	for _, param := range p.env.Parameters {
		options.Parameters = append(options.Parameters, expr.Parameter{Number: param.Number, Layout: param.Layout})
	}
	claim, err := p.env.Query.Allocator(runtime.WorkMemory).Reserve(0)
	if err != nil {
		return nil, err
	}
	program, err := expr.Compile(p.ctx, graph, roots, options)
	if err != nil {
		claim.Release()
		return nil, err
	}
	if err = claim.Resize(program.OwnedBytes()); err != nil {
		claim.Release()
		return nil, err
	}
	p.claims = append(p.claims, claim)
	prepared.programs = append(prepared.programs, program)
	return program, nil
}

func (c *invocation) node(id physical.NodeID, node physical.NodeSpec, layouts []model.TypeLayout, children []storage.BatchCursor) (storage.BatchCursor, error) {
	p := c.portal
	graph, _ := c.plan.Graph(node.Logical.Graph)
	rel, _ := graph.Rel(node.Logical.Rel)
	if node.Algorithm == physical.RowsFrom {
		return c.rowsFrom(node, layouts, children, rel.RowsFromOrdinality)
	}
	var input []physical.Slot
	if len(node.Children) > 0 {
		child, _ := c.plan.Node(node.Children[0])
		input = make([]physical.Slot, child.OutputCount())
		for i := range input {
			input[i], _ = child.Output(uint32(i))
		}
	}
	switch node.Algorithm {
	case physical.GenerateSeries, physical.GenerateSubscripts:
		return c.series(node, layouts)
	case physical.Unnest:
		return c.unnest(node, layouts)
	case physical.ProjectSet:
		return c.projectSet(node, layouts, input, children[0])
	case physical.Values:
		v := &valuesCursor{env: &p.env, layouts: layouts, rows: rel.Rows, evaluated: columnar.Batch{Columns: make([]columnar.Vector, 0, len(layouts))}}
		if len(layouts) == 0 {
			return v, nil
		}
		if rel.Rows > uint64(math.MaxInt)/uint64(len(layouts)) {
			return nil, ErrInvalid
		}
		if err := c.charge(rel.Rows * (uint64(unsafe.Sizeof((*expr.State)(nil))) + uint64(unsafe.Sizeof(expr.State{})) + uint64(len(layouts))*8)); err != nil {
			return nil, err
		}
		v.states = make([]*expr.State, rel.Rows)
		for row := range v.states {
			roots := make([]ir.ScalarID, len(layouts))
			for col := range roots {
				roots[col], _ = graph.Expression(node.Logical.Rel, uint32(row*len(layouts)+col))
			}
			var err error
			v.states[row], err = c.compile(node.Logical.Graph, roots, nil)
			if err != nil {
				return nil, errors.Join(err, v.Close())
			}
		}
		return v, nil
	case physical.TableScan, physical.PrimaryKeyScan, physical.SecondaryScan:
		if c.preparing {
			if !node.Scan.Residual.Empty() {
				if _, _, err := c.residual(node); err != nil {
					return nil, err
				}
			}
			if node.Scan.Lookup.Enabled {
				return c.secondaryIndexLookup(node, nil)
			}
			return nil, nil
		}
		return c.scan(node)
	case physical.CatalogScan:
		if c.preparing {
			return nil, nil
		}
		return p.env.Catalog.OpenCatalog(p.ctx, catalogrel.ScanRequest{
			Relation:   node.Catalog.Relation,
			Columns:    node.Catalog.Columns,
			BatchRows:  p.env.Limits.BatchRows,
			BatchBytes: p.env.Limits.BatchBytes,
		})
	case physical.LocalGather, physical.LocalHashRepartition, physical.LocalOrderedMerge:
		return c.localExchange(node, layouts, children)
	case physical.Apply:
		return c.apply(id, node, layouts, children[0])
	case physical.RecursiveUnion:
		return c.recursive(id, node, layouts, children[0])
	case physical.WorktableScan:
		return c.worktable(node)
	case physical.HashJoin, physical.NestedLoopJoin, physical.MergeJoin, physical.LookupJoin:
		return c.join(node, layouts, children)
	case physical.HashAggregate, physical.StreamAggregate:
		return c.aggregate(id, node, layouts, children)
	case physical.HashDistinct, physical.StreamDistinct, physical.HashSetOp:
		return c.setOperation(id, node, layouts, children)
	case physical.Window:
		return c.window(id, node, layouts, children)
	case physical.Sample:
		return c.sample(id, node, children)
	case physical.LockRows:
		return c.lockRows(id, node, layouts, children)
	case physical.Modify:
		return c.modify(id, node, children)
	case physical.Filter, physical.Project:
		var roots []ir.ScalarID
		if node.Algorithm == physical.Filter {
			roots = []ir.ScalarID{rel.Predicate}
		} else {
			var err error
			roots, err = projectionRoots(graph, node)
			if err != nil {
				return nil, err
			}
		}
		var projection []int
		if node.Algorithm == physical.Project {
			projection = directProjection(graph, node.Logical.Graph, roots, input)
		}
		inputLayouts, err := c.layouts(input)
		if err != nil {
			return nil, err
		}
		var state *expr.State
		if len(roots) > 0 && projection == nil {
			state, err = c.compile(node.Logical.Graph, roots, input)
			if err != nil {
				return nil, err
			}
		}
		return &expressionCursor{input: children[0], state: state, env: &p.env, layouts: inputLayouts, projection: projection, filter: node.Algorithm == physical.Filter, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(input))}, evaluated: columnar.Batch{Columns: make([]columnar.Vector, 0, max(1, len(layouts)))}, compact: columnar.Batch{Columns: make([]columnar.Vector, 0, len(input))}, selection: make([]uint32, 0, p.env.Limits.BatchRows)}, nil
	case physical.Limit, physical.TopK:
		if node.Delivered.Distribution.Kind != physical.SingleNode {
			return c.partitionSort(node, input, layouts, children[0])
		}
		offset, count := node.Limit.Offset, node.Limit.Count
		if node.Limit.RuntimeBounds {
			var err error
			offset, err = c.bound(node.Logical.Graph, rel.Offset, true)
			if err != nil {
				return nil, err
			}
			count, err = c.bound(node.Logical.Graph, rel.Count, false)
			if err != nil {
				return nil, err
			}
		}
		cursor := &limitCursor{input: children[0], env: &p.env, layouts: layouts, offset: offset, remaining: count, withTies: node.Limit.WithTies, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(input))}}
		if node.Limit.WithTies {
			var err error
			cursor.keys, cursor.keyColumns, err = c.orderColumns(node.Delivered.Ordering, node.Output, layouts)
			if err != nil {
				return nil, err
			}
		}
		if node.Algorithm == physical.TopK {
			var err error
			cursor.input, err = c.sort(children[0], node, input, layouts)
			if err != nil {
				return nil, err
			}
		}
		return cursor, nil
	case physical.UnionAll:
		return &unionCursor{inputs: children}, nil
	case physical.Materialize:
		if node.Delivered.Distribution.Kind != physical.SingleNode {
			return c.partitionMaterialize(node, layouts, children[0])
		}
		return &materializeCursor{input: children[0], env: &p.env, layouts: layouts, source: columnar.Batch{Columns: make([]columnar.Vector, 0, len(input))}}, nil
	case physical.Sort:
		if node.Delivered.Distribution.Kind != physical.SingleNode {
			return c.partitionSort(node, input, layouts, children[0])
		}
		return c.sort(children[0], node, input, layouts)
	}
	return nil, ErrInvalid
}

func projectionRoots(graph *ir.Graph, node physical.NodeSpec) ([]ir.ScalarID, error) {
	relation, _ := graph.Rel(node.Logical.Rel)
	roots := make([]ir.ScalarID, len(node.Output))
	for i, slot := range node.Output {
		for j := range relation.BindingCount() {
			binding, _ := graph.Binding(node.Logical.Rel, j)
			if binding.Slot == slot.ID {
				roots[i] = binding.Expr
				break
			}
		}
		if roots[i] == 0 {
			return nil, ErrInvalid
		}
	}
	return roots, nil
}

func directProjection(graph *ir.Graph, graphID physical.GraphID, roots []ir.ScalarID, input []physical.Slot) []int {
	projection := make([]int, len(roots))
	for i, root := range roots {
		scalar, _ := graph.Scalar(root)
		if scalar.Kind != ir.Slot {
			return nil
		}
		projection[i] = joinColumn(input, physical.Slot{Graph: graphID, ID: scalar.Slot})
		if projection[i] < 0 {
			return nil
		}
	}
	return projection
}

func (c *invocation) bound(graph physical.GraphID, root ir.ScalarID, offset bool) (result uint64, err error) {
	if root == 0 {
		return rowCountValue(model.NullVal(model.TypeInt64), offset)
	}
	state, err := c.compile(graph, []ir.ScalarID{root}, nil)
	if err != nil {
		return 0, err
	}
	if c.preparing {
		return 0, nil
	}
	defer func() { err = errors.Join(err, state.Close()) }()
	input := columnar.Batch{Length: 1}
	var out columnar.Batch
	if err = state.Eval(c.portal.ctx, &input, c.portal.env.Parameters, &out, expr.Direct); err != nil {
		return 0, err
	}
	value, _, err := columnar.CopyValue(c.portal.ctx, &out.Columns[0], 0, 0)
	if err != nil {
		return 0, err
	}
	return rowCountValue(value, offset)
}

func rowCountValue(value model.Value, offset bool) (uint64, error) {
	if value.IsNull {
		if offset {
			return 0, nil
		}
		return math.MaxUint64, nil
	}
	if value.Type != model.TypeInt64 {
		return 0, ErrInvalid
	}
	if value.IntVal >= 0 {
		return uint64(value.IntVal), nil
	}
	code, message := "2201W", "LIMIT must not be negative"
	if offset {
		code, message = "2201X", "OFFSET must not be negative"
	}
	return 0, &model.Error{Code: code, Message: message}
}

func (c *invocation) orderColumns(order []physical.OrderKey, slots []physical.Slot, layouts []model.TypeLayout) ([]storage.KeyColumn, []int, error) {
	if err := c.charge(uint64(len(order)) * (uint64(unsafe.Sizeof(storage.KeyColumn{})) + uint64(unsafe.Sizeof(int(0))))); err != nil {
		return nil, nil, err
	}
	keys := make([]storage.KeyColumn, len(order))
	columns := make([]int, len(order))
	for i, key := range order {
		columns[i] = -1
		for col, slot := range slots {
			if slot.Graph == key.Key.Slot.Graph && slot.ID == key.Key.Slot.ID {
				columns[i] = col
				break
			}
		}
		if columns[i] < 0 || columns[i] >= len(layouts) {
			return nil, nil, ErrInvalid
		}
		keys[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: key.Key.Type, Children: layouts[columns[i]].Children, Descending: key.Direction == ir.Descending, NullsFirst: key.Nulls == ir.NullsFirst}
	}
	keyBytes, err := storage.KeyLayoutBytes(c.portal.ctx, keys, c.portal.env.Limits.MetadataBytes-c.used)
	if err != nil {
		return nil, nil, err
	}
	if err := c.charge(keyBytes); err != nil {
		return nil, nil, err
	}
	keys, _, err = storage.CopyKeyLayout(c.portal.ctx, keys, keyBytes)
	if err != nil {
		return nil, nil, err
	}
	return keys, columns, nil
}
func (c *invocation) sort(input storage.BatchCursor, node physical.NodeSpec, slots []physical.Slot, layouts []model.TypeLayout) (storage.BatchCursor, error) {
	options := c.portal.env.Sort
	options.Query = c.portal.env.Query
	options.Encoder = c.portal.env.Keys
	options.Layouts = layouts
	options.BatchRows = c.portal.env.Limits.BatchRows
	options.BatchBytes = c.portal.env.Limits.BatchBytes
	keys, columns, err := c.orderColumns(node.Delivered.Ordering, slots, layouts)
	if err != nil {
		return nil, err
	}
	options.Keys = make([]exsort.Key, len(keys))
	for i, key := range keys {
		options.Keys[i] = exsort.Key{Column: columns[i], Descending: key.Descending, NullsFirst: key.NullsFirst}
	}
	if c.preparing {
		return nil, nil
	}
	return exsort.New(c.portal.ctx, input, options)
}
