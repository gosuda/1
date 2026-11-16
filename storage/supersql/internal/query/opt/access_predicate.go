package opt

import (
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/query/physical"
)

const maxStoragePredicateNodes = 2048

type storagePredicateBuilder struct {
	optimizer *optimizer
	relation  ir.RelID
	outputs   []physical.Slot
	mapping   []physical.ScanColumn
	nodes     []storage.PredicateNode
	constants []model.Value
	nodeCount int
	constantCount int
}

func (o *optimizer) scanPredicate(relation ir.RelID, outputs []physical.Slot, mapping []physical.ScanColumn) storage.Predicate {
	root := o.predicates[relation]
	if root == 0 {
		return storage.Predicate{}
	}
	capacity := min(uint64(root)*2, uint64(o.options.Plan.MaxOperands), uint64(maxStoragePredicateNodes))
	builder := storagePredicateBuilder{
		optimizer: o, relation: relation, outputs: outputs, mapping: mapping,
		nodes: allocate[storage.PredicateNode](o, capacity),
		constants: allocate[model.Value](o, capacity),
	}
	if o.err != nil {
		return storage.Predicate{}
	}
	_, supported := builder.lower(root, true, 0)
	if !supported || builder.nodeCount == 0 || o.err != nil {
		return storage.Predicate{}
	}
	predicate, err := storage.NewPredicate(builder.nodes[:builder.nodeCount], nil, builder.constants[:builder.constantCount])
	if err != nil {
		o.err = err
		return storage.Predicate{}
	}
	return predicate
}

func (b *storagePredicateBuilder) append(node storage.PredicateNode) (uint32, bool) {
	if b.nodeCount >= len(b.nodes) || !b.optimizer.step(1) {
		return 0, false
	}
	index := b.nodeCount
	b.nodes[index] = node
	b.nodeCount++
	return uint32(index), true
}

func (b *storagePredicateBuilder) rollback(nodes, constants int) {
	clear(b.nodes[nodes:b.nodeCount])
	clear(b.constants[constants:b.constantCount])
	b.nodeCount, b.constantCount = nodes, constants
}

func (b *storagePredicateBuilder) scalarType(scalar ir.Scalar) (model.Type, bool) {
	typ, ok := b.optimizer.graph.Type(scalar.Layout)
	return typ, ok
}

func (b *storagePredicateBuilder) column(slot model.SlotID) (model.ColumnID, bool) {
	for i, output := range b.outputs {
		predicateSlot := output.ID
		if i < len(b.optimizer.predicateSlots[b.relation]) && b.optimizer.predicateSlots[b.relation][i] != 0 {
			predicateSlot = b.optimizer.predicateSlots[b.relation][i]
		}
		if slot != output.ID && slot != predicateSlot {
			continue
		}
		info, ok := b.optimizer.graph.Slot(output.ID)
		if !ok {
			return 0, false
		}
		column := (physical.ScanSpec{ColumnMap: b.mapping}).PhysicalColumn(info.Column)
		return column, column != 0
	}
	return 0, false
}

func (b *storagePredicateBuilder) operand(id ir.ScalarID, depth uint32) (uint32, bool) {
	if depth >= b.optimizer.options.IR.MaxDepth || !b.optimizer.step(1) {
		if depth >= b.optimizer.options.IR.MaxDepth {
			b.optimizer.err = ErrLimit
		}
		return 0, false
	}
	scalar, ok := b.optimizer.graph.Scalar(id)
	if !ok {
		b.optimizer.err = ErrInvalid
		return 0, false
	}
	typ, ok := b.scalarType(scalar)
	if !ok {
		b.optimizer.err = ErrInvalid
		return 0, false
	}
	switch scalar.Kind {
	case ir.Slot:
		column, found := b.column(scalar.Slot)
		if !found {
			return 0, false
		}
		return b.append(storage.PredicateNode{Kind: storage.PredicateColumn, Type: typ, ColumnID: column})
	case ir.Const:
		if b.constantCount >= len(b.constants) {
			b.optimizer.err = ErrLimit
			return 0, false
		}
		remaining := b.optimizer.options.MaxBytes - b.optimizer.bytes
		value, err := b.optimizer.graph.CloneConstant(b.optimizer.ctx, id, remaining)
		if err != nil {
			b.optimizer.err = err
			return 0, false
		}
		used, err := model.ValueBytes(b.optimizer.ctx, value, remaining)
		if err != nil {
			b.optimizer.err = err
			return 0, false
		}
		if !b.optimizer.charge(used, 1) {
			return 0, false
		}
		index := b.constantCount
		b.constants[index] = value
		b.constantCount++
		return b.append(storage.PredicateNode{Kind: storage.PredicateConstant, Type: typ, ConstantIndex: uint32(index)})
	default:
		return 0, false
	}
}

func (b *storagePredicateBuilder) lower(id ir.ScalarID, allowPartialAnd bool, depth uint32) (uint32, bool) {
	if depth >= b.optimizer.options.IR.MaxDepth || !b.optimizer.step(1) {
		if depth >= b.optimizer.options.IR.MaxDepth {
			b.optimizer.err = ErrLimit
		}
		return 0, false
	}
	scalar, ok := b.optimizer.graph.Scalar(id)
	if !ok {
		b.optimizer.err = ErrInvalid
		return 0, false
	}
	typ, ok := b.scalarType(scalar)
	if !ok || typ.ID != 16 || typ.Storage != model.TypeBoolean || scalar.Effects != (ir.Effects{}) {
		return 0, false
	}
	startNodes, startConstants := b.nodeCount, b.constantCount
	switch scalar.Kind {
	case ir.Const:
		return b.operand(id, depth+1)
	case ir.Call:
		if scalar.Operator == 0 || scalar.OperandCount() != 2 {
			return 0, false
		}
		leftID, _ := b.optimizer.graph.Operand(id, 0)
		rightID, _ := b.optimizer.graph.Operand(id, 1)
		leftScalar, _ := b.optimizer.graph.Scalar(leftID)
		rightScalar, _ := b.optimizer.graph.Scalar(rightID)
		leftType, leftOK := b.scalarType(leftScalar)
		rightType, rightOK := b.scalarType(rightScalar)
		operator, operatorOK := builtin.LookupOperator(scalar.Operator)
		if !leftOK || !rightOK || !operatorOK || operator.Left != leftType.ID || operator.Right != rightType.ID || operator.Result != 16 {
			return 0, false
		}
		left, leftSupported := b.operand(leftID, depth+1)
		right, rightSupported := b.operand(rightID, depth+1)
		if !leftSupported || !rightSupported {
			b.rollback(startNodes, startConstants)
			return 0, false
		}
		var collation model.CollationID
		if scalar.Signature != 0 {
			signature, found := b.optimizer.graph.Signature(scalar.Signature)
			if !found {
				b.optimizer.err = ErrInvalid
				return 0, false
			}
			collation = signature.InputCollation
		}
		return b.append(storage.PredicateNode{Kind: storage.PredicateComparison, Type: typ, Operator: scalar.Operator, Collation: collation, Left: left, Right: right})
	case ir.NullTest:
		if scalar.OperandCount() != 1 {
			return 0, false
		}
		operandID, _ := b.optimizer.graph.Operand(id, 0)
		operand, supported := b.operand(operandID, depth+1)
		if !supported {
			b.rollback(startNodes, startConstants)
			return 0, false
		}
		return b.append(storage.PredicateNode{Kind: storage.PredicateNullTest, Type: typ, Left: operand, Negated: scalar.Negated})
	case ir.Bool:
		if scalar.BoolOp != ir.And && scalar.BoolOp != ir.Or || scalar.OperandCount() == 0 {
			return 0, false
		}
		var root uint32
		count := 0
		for i := range scalar.OperandCount() {
			childID, _ := b.optimizer.graph.Operand(id, i)
			childNodes, childConstants := b.nodeCount, b.constantCount
			child, supported := b.lower(childID, allowPartialAnd && scalar.BoolOp == ir.And, depth+1)
			if !supported {
				b.rollback(childNodes, childConstants)
				if scalar.BoolOp == ir.Or || !allowPartialAnd {
					b.rollback(startNodes, startConstants)
					return 0, false
				}
				continue
			}
			if count == 0 {
				root = child
				count++
				continue
			}
			kind := storage.PredicateAnd
			if scalar.BoolOp == ir.Or {
				kind = storage.PredicateOr
			}
			var appended bool
			root, appended = b.append(storage.PredicateNode{Kind: kind, Type: typ, Left: root, Right: child})
			if !appended {
				return 0, false
			}
			count++
		}
		return root, count != 0
	default:
		return 0, false
	}
}
