package exec

import (
	"context"
	"errors"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	exrecursive "github.com/gosuda/supersql-alpha/internal/query/exec/recursive"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

func (c *invocation) recursive(id physical.NodeID, node physical.NodeSpec, layouts []model.TypeLayout, anchor storage.BatchCursor) (storage.BatchCursor, error) {
	graph, _ := c.plan.Graph(node.Logical.Graph)
	relation, _ := graph.Rel(node.Logical.Rel)
	options := c.portal.env.Recursive
	options.BaseColumns, options.Search, options.SearchColumn = 0, exrecursive.NoSearch, 0
	options.SearchKeys = nil
	options.Cycle, options.CycleMarkColumn, options.CyclePathColumn = false, 0, 0
	options.CycleKeys = nil
	options.CycleValue, options.CycleDefault = model.Value{}, model.Value{}
	options.CycleContinue, options.MaxKeyBytes = false, 0
	options.Layouts, options.All = layouts, relation.All
	options.Query, options.Encoder = c.portal.env.Query, c.portal.env.Keys
	options.BatchRows, options.BatchBytes = c.portal.env.Limits.BatchRows, c.portal.env.Limits.BatchBytes
	options.Step = &recursiveFactory{owner: c, root: node.Children[1], producer: id}
	if err := c.recursiveMetadata(graph, node.Logical.Rel, relation, &options); err != nil {
		return nil, err
	}
	var markProgram *expr.Program
	var err error
	if options.Cycle {
		markProgram, err = c.program(node.Logical.Graph, []ir.ScalarID{relation.Recursion.CycleValue, relation.Recursion.CycleDefault, relation.Recursion.CycleContinue}, nil)
		if err != nil {
			return nil, err
		}
	}
	if c.preparing {
		if (options.Search == exrecursive.DepthFirst || options.Cycle) && options.Spool.MaxReaders < 2 {
			return nil, ErrInvalid
		}
		if !relation.All {
			limits := options.Seen
			if limits.WorkBytes == 0 || limits.MaxKeyBytes == 0 || limits.MaxKeys == 0 || limits.MaxLevels <= 0 {
				return nil, ErrInvalid
			}
			if err := c.preflightRecursiveKeys(layouts, nil, limits.MaxKeyBytes); err != nil {
				return nil, err
			}
		}
		if options.Cycle {
			if options.MaxKeyBytes == 0 {
				return nil, ErrInvalid
			}
			if err := c.preflightRecursiveKeys(layouts, options.CycleKeys, options.MaxKeyBytes); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	if err := c.charge(uint64(unsafe.Sizeof(recursiveFactory{}))); err != nil {
		return nil, err
	}
	if options.Cycle {
		state, err := c.state(markProgram)
		if err != nil {
			return nil, err
		}
		var values [3]model.Value
		input := columnar.Batch{Length: 1}
		if evalErr := state.EvalBorrowedRow(c.portal.ctx, &input, c.portal.env.Parameters, values[:], expr.Direct); evalErr != nil {
			return nil, errors.Join(evalErr, state.Close())
		}
		if values[2].Type != model.TypeBoolean {
			return nil, errors.Join(ErrInvalid, state.Close())
		}
		options.CycleValue, options.CycleDefault = values[0], values[1]
		options.CycleContinue = !values[2].IsNull && values[2].IntVal != 0
		cursor, newErr := exrecursive.New(c.portal.ctx, anchor, options)
		closeErr := state.Close()
		if err := errors.Join(newErr, closeErr); err != nil {
			if cursor != nil {
				err = errors.Join(err, cursor.Close())
			}
			return nil, err
		}
		return cursor, nil
	}
	return exrecursive.New(c.portal.ctx, anchor, options)
}

func (c *invocation) recursiveMetadata(graph *ir.Graph, relationID ir.RelID, relation ir.Rel, options *exrecursive.Options) error {
	generated := 0
	switch relation.Recursion.Search {
	case ir.NoSearch:
		options.Search = exrecursive.NoSearch
	case ir.BreadthFirst:
		options.Search = exrecursive.BreadthFirst
		generated++
	case ir.DepthFirst:
		options.Search = exrecursive.DepthFirst
		generated++
	default:
		return ErrInvalid
	}
	if relation.Recursion.CycleMark != 0 {
		options.Cycle = true
		generated += 2
		options.MaxKeyBytes = options.Seen.MaxKeyBytes
	}
	options.BaseColumns = len(options.Layouts) - generated
	if options.BaseColumns < 0 {
		return ErrInvalid
	}
	column := func(slot model.SlotID) (int, error) {
		for i := range relation.OutputCount() {
			output, _ := graph.Output(relationID, i)
			if output == slot {
				return int(i), nil
			}
		}
		return 0, ErrInvalid
	}
	var err error
	if options.Search != exrecursive.NoSearch {
		options.SearchColumn, err = column(relation.Recursion.SearchSlot)
		if err != nil {
			return err
		}
		if err := c.charge(uint64(relation.Recursion.SearchKeyCount()) * uint64(unsafe.Sizeof(int(0)))); err != nil {
			return err
		}
		options.SearchKeys = make([]int, relation.Recursion.SearchKeyCount())
		for i := range relation.Recursion.SearchKeyCount() {
			slot, _ := graph.SearchKey(relationID, i)
			options.SearchKeys[i], err = column(slot)
			if err != nil {
				return err
			}
		}
	}
	if options.Cycle {
		options.CycleMarkColumn, err = column(relation.Recursion.CycleMark)
		if err != nil {
			return err
		}
		options.CyclePathColumn, err = column(relation.Recursion.CyclePath)
		if err != nil {
			return err
		}
		if err := c.charge(uint64(relation.Recursion.CycleKeyCount()) * uint64(unsafe.Sizeof(int(0)))); err != nil {
			return err
		}
		options.CycleKeys = make([]int, relation.Recursion.CycleKeyCount())
		for i := range relation.Recursion.CycleKeyCount() {
			slot, _ := graph.CycleKey(relationID, i)
			options.CycleKeys[i], err = column(slot)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *invocation) preflightRecursiveKeys(layouts []model.TypeLayout, columns []int, maxBytes uint64) error {
	count := len(layouts)
	if columns != nil {
		count = len(columns)
	}
	if err := c.charge(uint64(count) * uint64(unsafe.Sizeof(storage.KeyColumn{}))); err != nil {
		return err
	}
	keys := make([]storage.KeyColumn, count)
	for i := range keys {
		index := i
		if columns != nil {
			index = columns[i]
		}
		if index < 0 || index >= len(layouts) {
			return ErrInvalid
		}
		layout := layouts[index]
		keys[i] = storage.KeyColumn{ColumnID: model.ColumnID(i + 1), Type: layout.Type, Children: layout.Children}
	}
	_, err := c.portal.env.Keys.EncodePrefix(c.portal.ctx, keys, nil, maxBytes)
	return err
}

func (c *invocation) worktable(node physical.NodeSpec) (storage.BatchCursor, error) {
	producer := node.Worktable.Producer
	if c.preparing {
		_, err := c.worktableDomain(c.domain, producer)
		return nil, err
	}
	for owner := c; owner != nil; owner = owner.parent {
		if owner.worktableRoot == producer && owner.delta != nil {
			return owner.delta.Open(c.portal.ctx)
		}
	}
	return nil, ErrInvalid
}

func (c *invocation) worktableDomain(domain, producer physical.NodeID) (physical.NodeID, error) {
	for remaining := len(c.prepared.nodes); domain != 0 && remaining > 0; remaining-- {
		if err := c.portal.ctx.Err(); err != nil {
			return 0, err
		}
		root := &c.prepared.nodes[domain]
		if root.recursion == producer {
			return domain, nil
		}
		domain = root.parentDomain
	}
	return 0, &RequiredExecutionError{Node: c.current, Algorithm: physical.WorktableScan, Reason: "worktable has no enclosing recursive iteration"}
}

type recursiveFactory struct {
	owner          *invocation
	root, producer physical.NodeID
}

func (f *recursiveFactory) Open(ctx context.Context, delta exrecursive.Worktable) (_ storage.BatchCursor, err error) {
	parent := f.owner
	portal := &Portal{env: parent.portal.env, borrowed: true}
	portal.env.row = nil
	portal.ctx, portal.cancel = context.WithCancel(ctx)
	defer func() {
		if err != nil {
			err = errors.Join(err, portal.Close())
		}
	}()
	inner := &invocation{portal: portal, graph: parent.graph, plan: parent.plan, prepared: parent.prepared, domain: f.root, parent: parent, captures: parent.captures, captureValues: parent.captureValues, worktableRoot: f.producer, delta: delta}
	portal.root, err = inner.build()
	if err != nil {
		return nil, err
	}
	return portal, nil
}

func (c *invocation) referencesWorktable(root, producer physical.NodeID) (bool, error) {
	count := uint64(c.plan.NodeCount()) + 1
	if err := c.charge(count * (uint64(unsafe.Sizeof(bool(false))) + uint64(unsafe.Sizeof(physical.NodeID(0))))); err != nil {
		return false, err
	}
	seen := make([]bool, count)
	stack := make([]physical.NodeID, 0, count)
	push := func(id physical.NodeID) {
		if id != 0 && !seen[id] {
			seen[id] = true
			stack = append(stack, id)
		}
	}
	push(root)
	for len(stack) != 0 {
		if err := c.portal.ctx.Err(); err != nil {
			return false, err
		}
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		node := c.prepared.nodes[id].node
		if node.Algorithm == physical.WorktableScan && node.Worktable.Producer == producer {
			return true, nil
		}
		for _, child := range node.Children {
			push(child)
		}
		for _, dependency := range node.StatementDependencies {
			push(dependency)
		}
		for _, subplan := range node.ScalarSubplans {
			push(subplan.Root)
		}
		if node.Algorithm == physical.CTEScan {
			push(node.Worktable.Producer)
		}
	}
	return false, nil
}
