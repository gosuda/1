package storage

import (
	"errors"
	"fmt"

	"github.com/gosuda/supersql-alpha/internal/model"
)

type PredicateKind uint8

const (
	PredicateColumn PredicateKind = iota + 1
	PredicateConstant
	PredicateComparison
	PredicateNullTest
	PredicateIn
	PredicateAnd
	PredicateOr
)

// Operands precede their consumer in the flat node array. IN arguments occupy
// a range in the separate edge array; no node contains a heap pointer.
type PredicateNode struct {
	Kind          PredicateKind
	Type          model.Type
	Operator      model.OperatorID
	Collation     model.CollationID
	ColumnID      model.ColumnID
	ConstantIndex uint32
	Left          uint32
	Right         uint32
	FirstArgument uint32
	ArgumentCount uint32
	Negated       bool
}

// The zero predicate is TRUE. A false predicate is an explicit Boolean constant.
// Nodes, edges and constants are immutable; Subtree shares their storage.
type Predicate struct {
	nodes     []PredicateNode
	edges     []uint32
	constants []model.Value
	root      uint32
}

var ErrInvalidPredicate = errors.New("invalid typed storage predicate")

// NewPredicate takes ownership of the input slices and all constant payloads on
// success. Callers must relinquish mutable aliases. The final node is the root.
func NewPredicate(nodes []PredicateNode, edges []uint32, constants []model.Value) (Predicate, error) {
	if len(nodes) == 0 {
		if len(edges) != 0 || len(constants) != 0 {
			return Predicate{}, ErrInvalidPredicate
		}
		return Predicate{}, nil
	}
	if uint64(len(nodes)) > uint64(^uint32(0)) {
		return Predicate{}, ErrInvalidPredicate
	}
	p := Predicate{nodes: nodes, edges: edges, constants: constants, root: uint32(len(nodes) - 1)}
	for i := range nodes {
		if err := p.validateNode(uint32(i)); err != nil {
			return Predicate{}, fmt.Errorf("predicate node %d: %w", i, err)
		}
	}
	if !booleanType(nodes[p.root].Type) {
		return Predicate{}, ErrInvalidPredicate
	}
	return p, nil
}

func (p Predicate) Empty() bool                     { return len(p.nodes) == 0 }
func (p Predicate) Root() uint32                    { return p.root }
func (p Predicate) NodeCount() int                  { return len(p.nodes) }
func (p Predicate) Node(index uint32) PredicateNode { return p.nodes[index] }

// Constant returns a borrowed immutable datum, including its nested payloads.
func (p Predicate) Constant(index uint32) model.Value { return p.constants[index] }
func (p Predicate) Argument(index uint32) uint32      { return p.edges[index] }

func (p Predicate) Subtree(root uint32) (Predicate, error) {
	if uint64(root) >= uint64(len(p.nodes)) {
		return Predicate{}, ErrInvalidPredicate
	}
	if !booleanType(p.nodes[root].Type) {
		return Predicate{}, ErrInvalidPredicate
	}
	p.root = root
	return p, nil
}

func (p Predicate) validateNode(index uint32) error {
	n := p.nodes[index]
	if n.Type.ID == 0 || !n.Type.Storage.Valid() {
		return ErrInvalidPredicate
	}
	if n.Negated && n.Kind != PredicateNullTest && n.Kind != PredicateIn {
		return ErrInvalidPredicate
	}
	switch n.Kind {
	case PredicateColumn:
		if n.ColumnID == 0 {
			return ErrInvalidPredicate
		}
	case PredicateConstant:
		if uint64(n.ConstantIndex) >= uint64(len(p.constants)) {
			return ErrInvalidPredicate
		}
		if p.constants[n.ConstantIndex].Type != n.Type.Storage {
			return ErrInvalidPredicate
		}
	case PredicateComparison:
		if n.Left >= index || n.Right >= index || n.Operator == 0 {
			return ErrInvalidPredicate
		}
		if !booleanType(n.Type) {
			return ErrInvalidPredicate
		}
	case PredicateNullTest:
		if n.Left >= index || !booleanType(n.Type) {
			return ErrInvalidPredicate
		}
	case PredicateIn:
		if n.Left >= index || n.Operator == 0 || !booleanType(n.Type) {
			return ErrInvalidPredicate
		}
		end := uint64(n.FirstArgument) + uint64(n.ArgumentCount)
		if end > uint64(len(p.edges)) {
			return ErrInvalidPredicate
		}
		for _, arg := range p.edges[n.FirstArgument:end] {
			if arg >= index {
				return ErrInvalidPredicate
			}
		}
	case PredicateAnd, PredicateOr:
		if n.Left >= index || n.Right >= index {
			return ErrInvalidPredicate
		}
		if !booleanType(n.Type) {
			return ErrInvalidPredicate
		}
		if !booleanType(p.nodes[n.Left].Type) || !booleanType(p.nodes[n.Right].Type) {
			return ErrInvalidPredicate
		}
	default:
		return ErrInvalidPredicate
	}
	return nil
}

func booleanType(t model.Type) bool {
	return t.ID == 16 && t.Storage == model.TypeBoolean
}
