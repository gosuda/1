package bind

import (
	"slices"
	"unsafe"

	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type boundGrouping struct {
	nodes   []*zpgparse.Node
	values  []expression
	sets    [][]uint32
	omitted []bool
}

type groupingExpansion struct {
	b           *binder
	statement   *zpgparse.SelectStmt
	scope       *scope
	input       []column
	result      boundGrouping
	work, bytes uint64
}

func (b *binder) groupingExpressions(statement *zpgparse.SelectStmt, s *scope, input []column) (boundGrouping, error) {
	if len(statement.GroupClause) == 0 {
		return boundGrouping{}, nil
	}
	e := groupingExpansion{b: b, statement: statement, scope: s, input: input, work: uint64(b.options.Limits.MaxNodes) + uint64(b.options.Limits.MaxOperands)}
	flat := true
	for _, node := range statement.GroupClause {
		if node.GetGroupingSet() != nil || groupingTuple(node) != nil {
			flat = false
			break
		}
	}
	if flat {
		return e.flat()
	}
	leaves, err := e.leaves(statement.GroupClause, 0)
	if err != nil {
		return boundGrouping{}, err
	}
	if err := e.reserve(uint64(leaves), uint64(unsafe.Sizeof(expression{}))+uint64(unsafe.Sizeof((*zpgparse.Node)(nil)))); err != nil {
		return boundGrouping{}, err
	}
	e.result.nodes = make([]*zpgparse.Node, 0, leaves)
	e.result.values = make([]expression, 0, leaves)
	e.result.sets, err = e.product(statement.GroupClause, 0)
	if err != nil {
		return boundGrouping{}, err
	}
	if statement.GroupDistinct {
		if err := e.deduplicate(); err != nil {
			return boundGrouping{}, err
		}
	}
	if err := e.reserve(uint64(len(e.result.values)), 5); err != nil {
		return boundGrouping{}, err
	}
	counts := make([]uint32, len(e.result.values))
	for _, set := range e.result.sets {
		for _, index := range set {
			if err := b.groupingStep(&e.work); err != nil {
				return boundGrouping{}, err
			}
			counts[index]++
		}
	}
	e.result.omitted = make([]bool, len(counts))
	for i, count := range counts {
		e.result.omitted[i] = uint64(count) != uint64(len(e.result.sets))
	}
	return e.result, nil
}

func (e *groupingExpansion) flat() (boundGrouping, error) {
	count := len(e.statement.GroupClause)
	unit := uint64(unsafe.Sizeof(expression{})) + uint64(unsafe.Sizeof((*zpgparse.Node)(nil))) + 4
	if err := e.reserve(uint64(count), unit); err != nil {
		return boundGrouping{}, err
	}
	sets, err := e.setList(1)
	if err != nil {
		return boundGrouping{}, err
	}
	result := boundGrouping{nodes: make([]*zpgparse.Node, count), values: make([]expression, count), sets: sets}
	result.sets[0] = make([]uint32, count)
	for i, node := range e.statement.GroupClause {
		if err := e.b.groupingStep(&e.work); err != nil {
			return boundGrouping{}, err
		}
		result.nodes[i], result.values[i], err = e.b.groupingExpression(node, e.statement, e.scope, e.input, &e.work)
		if err != nil {
			return boundGrouping{}, err
		}
		result.sets[0][i] = uint32(i)
	}
	return result, nil
}

func (e *groupingExpansion) compareSets(a, b []uint32) (int, error) {
	if err := e.b.groupingStep(&e.work); err != nil {
		return 0, err
	}
	for i := 0; i < min(len(a), len(b)); i++ {
		if err := e.b.groupingStep(&e.work); err != nil {
			return 0, err
		}
		if a[i] < b[i] {
			return -1, nil
		}
		if a[i] > b[i] {
			return 1, nil
		}
	}
	if len(a) < len(b) {
		return -1, nil
	}
	if len(a) > len(b) {
		return 1, nil
	}
	return 0, nil
}

func (e *groupingExpansion) deduplicate() error {
	var comparisonErr error
	slices.SortFunc(e.result.sets, func(a, b []uint32) int {
		if comparisonErr != nil {
			return 0
		}
		var order int
		order, comparisonErr = e.compareSets(a, b)
		return order
	})
	if comparisonErr != nil {
		return comparisonErr
	}
	unique := e.result.sets[:0]
	for _, set := range e.result.sets {
		if err := e.b.groupingStep(&e.work); err != nil {
			return err
		}
		if len(unique) != 0 {
			order, err := e.compareSets(unique[len(unique)-1], set)
			if err != nil {
				return err
			}
			if order == 0 {
				continue
			}
		}
		unique = append(unique, set)
	}
	clear(e.result.sets[len(unique):])
	e.result.sets = unique
	return nil
}

func (e *groupingExpansion) reserve(count, size uint64) error {
	if err := e.b.groupingStep(&e.work); err != nil {
		return err
	}
	if size != 0 && count > (e.b.options.Limits.MaxBytes-e.bytes)/size {
		return bindError("54000", "grouping-set expansion memory limit exceeded", 0)
	}
	e.bytes += count * size
	return nil
}
func (e *groupingExpansion) enter(depth uint32) error {
	if err := e.b.groupingStep(&e.work); err != nil {
		return err
	}
	if depth >= e.b.options.Limits.MaxDepth {
		return bindError("54000", "grouping-set expansion depth limit exceeded", 0)
	}
	return nil
}
func groupingTuple(node *zpgparse.Node) []*zpgparse.Node {
	row := node.GetRowExpr()
	if row != nil && row.RowFormat == zpgparse.CoercionForm_COERCE_IMPLICIT_CAST {
		return row.Args
	}
	return nil
}
func (e *groupingExpansion) leaves(nodes []*zpgparse.Node, depth uint32) (int, error) {
	if err := e.enter(depth); err != nil {
		return 0, err
	}
	count := 0
	for _, node := range nodes {
		children := groupingTuple(node)
		if set := node.GetGroupingSet(); set != nil {
			children = set.Content
		}
		n := 1
		if children != nil || node.GetGroupingSet() != nil {
			var err error
			n, err = e.leaves(children, depth+1)
			if err != nil {
				return 0, err
			}
		}
		if uint64(count)+uint64(n) > uint64(e.b.options.Limits.MaxSlots) {
			return 0, bindError("54000", "too many grouping expressions", 0)
		}
		count += n
	}
	return count, nil
}
func (e *groupingExpansion) setList(count int) ([][]uint32, error) {
	if count < 0 || uint64(count) > uint64(e.b.options.Limits.MaxOperands) {
		return nil, bindError("54000", "too many grouping sets", 0)
	}
	if err := e.reserve(uint64(count), uint64(unsafe.Sizeof([]uint32{}))); err != nil {
		return nil, err
	}
	return make([][]uint32, count), nil
}
func (e *groupingExpansion) empty() ([][]uint32, error) { return e.setList(1) }

func (e *groupingExpansion) key(node *zpgparse.Node) ([][]uint32, error) {
	resolved, value, err := e.b.groupingExpression(node, e.statement, e.scope, e.input, &e.work)
	if err != nil {
		return nil, err
	}
	index := len(e.result.nodes)
	for i, prior := range e.result.nodes {
		if err := e.b.groupingStep(&e.work); err != nil {
			return nil, err
		}
		sameSlot := value.sourceSlot != 0 && value.sourceSlot == e.result.values[i].sourceSlot
		if sameSlot || sameGroupingTarget(prior, resolved, e.scope) {
			index = i
			break
		}
	}
	if index == len(e.result.nodes) {
		e.result.nodes = append(e.result.nodes, resolved)
		e.result.values = append(e.result.values, value)
	}
	sets, err := e.setList(1)
	if err != nil {
		return nil, err
	}
	if err := e.reserve(1, 4); err != nil {
		return nil, err
	}
	sets[0] = []uint32{uint32(index)}
	return sets, nil
}

func (e *groupingExpansion) merge(left, right []uint32) ([]uint32, error) {
	if err := e.reserve(uint64(len(left)+len(right)), 4); err != nil {
		return nil, err
	}
	out := make([]uint32, 0, len(left)+len(right))
	for i, j := 0, 0; i < len(left) || j < len(right); {
		if err := e.b.groupingStep(&e.work); err != nil {
			return nil, err
		}
		switch {
		case j == len(right) || i < len(left) && left[i] < right[j]:
			out = append(out, left[i])
			i++
		case i == len(left) || right[j] < left[i]:
			out = append(out, right[j])
			j++
		default:
			out = append(out, left[i])
			i++
			j++
		}
	}
	return out, nil
}
func (e *groupingExpansion) cross(left, right [][]uint32) ([][]uint32, error) {
	if len(right) != 0 && uint64(len(left)) > uint64(e.b.options.Limits.MaxOperands)/uint64(len(right)) {
		return nil, bindError("54000", "too many grouping sets", 0)
	}
	result, err := e.setList(len(left) * len(right))
	if err != nil {
		return nil, err
	}
	at := 0
	for _, a := range left {
		for _, b := range right {
			result[at], err = e.merge(a, b)
			if err != nil {
				return nil, err
			}
			at++
		}
	}
	return result, nil
}
func (e *groupingExpansion) concatenate(left, right [][]uint32) ([][]uint32, error) {
	result, err := e.setList(len(left) + len(right))
	if err != nil {
		return nil, err
	}
	copy(result, left)
	copy(result[len(left):], right)
	return result, nil
}
func (e *groupingExpansion) product(nodes []*zpgparse.Node, depth uint32) ([][]uint32, error) {
	if err := e.enter(depth); err != nil {
		return nil, err
	}
	result, err := e.empty()
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		factor, err := e.expand(node, depth+1)
		if err != nil {
			return nil, err
		}
		result, err = e.cross(result, factor)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
func (e *groupingExpansion) expand(node *zpgparse.Node, depth uint32) ([][]uint32, error) {
	if err := e.enter(depth); err != nil {
		return nil, err
	}
	set := node.GetGroupingSet()
	if set == nil {
		if tuple := groupingTuple(node); tuple != nil {
			return e.product(tuple, depth+1)
		}
		return e.key(node)
	}
	switch set.Kind {
	case zpgparse.GroupingSetKind_GROUPING_SET_EMPTY:
		return e.empty()
	case zpgparse.GroupingSetKind_GROUPING_SET_SIMPLE:
		return e.product(set.Content, depth+1)
	case zpgparse.GroupingSetKind_GROUPING_SET_SETS:
		var result [][]uint32
		for _, item := range set.Content {
			alternative, err := e.expand(item, depth+1)
			if err != nil {
				return nil, err
			}
			result, err = e.concatenate(result, alternative)
			if err != nil {
				return nil, err
			}
		}
		if len(result) == 0 {
			return e.empty()
		}
		return result, nil
	case zpgparse.GroupingSetKind_GROUPING_SET_ROLLUP:
		prefix, err := e.empty()
		if err != nil {
			return nil, err
		}
		result := prefix
		for _, item := range set.Content {
			factor, err := e.expand(item, depth+1)
			if err != nil {
				return nil, err
			}
			prefix, err = e.cross(prefix, factor)
			if err != nil {
				return nil, err
			}
			result, err = e.concatenate(prefix, result)
			if err != nil {
				return nil, err
			}
		}
		return result, nil
	case zpgparse.GroupingSetKind_GROUPING_SET_CUBE:
		result, err := e.empty()
		if err != nil {
			return nil, err
		}
		for _, item := range set.Content {
			factor, err := e.expand(item, depth+1)
			if err != nil {
				return nil, err
			}
			included, err := e.cross(result, factor)
			if err != nil {
				return nil, err
			}
			result, err = e.concatenate(included, result)
			if err != nil {
				return nil, err
			}
		}
		return result, nil
	default:
		return nil, bindError("42601", "invalid grouping-set kind", int(set.Location)+1)
	}
}
