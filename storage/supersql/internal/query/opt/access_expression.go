package opt

import (
	"context"
	"reflect"
	"slices"

	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

type accessScalarExpression struct {
	AccessExpression
	object model.ObjectID
}

type accessExpressionProof struct {
	optimizer *optimizer
}

func (o *optimizer) accessExpression(expression AccessExpression) accessScalarExpression {
	return accessScalarExpression{AccessExpression: expression}
}

func (o *optimizer) queryExpression(object model.ObjectID, root ir.ScalarID) accessScalarExpression {
	return accessScalarExpression{AccessExpression: AccessExpression{Graph: o.graph, Root: root}, object: object}
}

func (p *accessExpressionProof) column(expression accessScalarExpression, node ir.Scalar) (model.ColumnID, bool) {
	if expression.object != 0 {
		if node.Kind != ir.Slot {
			return 0, false
		}
		slot, ok := expression.Graph.Slot(node.Slot)
		return slot.Column, ok && slot.Object == expression.object && slot.Column != 0
	}
	if node.Kind != ir.Param || node.Parameter == 0 || int(node.Parameter) > len(expression.Parameters) {
		return 0, false
	}
	column := expression.Parameters[node.Parameter-1].Column
	return column, column != 0
}

func accessLayoutEqual(left *ir.Graph, leftID ir.LayoutID, right *ir.Graph, rightID ir.LayoutID) bool {
	leftType, leftOK := left.Type(leftID)
	rightType, rightOK := right.Type(rightID)
	if !leftOK || !rightOK || leftType != rightType || left.LayoutChildCount(leftID) != right.LayoutChildCount(rightID) {
		return false
	}
	for i := range left.LayoutChildCount(leftID) {
		leftChild, _ := left.LayoutChild(leftID, i)
		rightChild, _ := right.LayoutChild(rightID, i)
		if !accessLayoutEqual(left, leftChild, right, rightChild) {
			return false
		}
	}
	return true
}

func (p *accessExpressionProof) constant(expression accessScalarExpression, id ir.ScalarID) (model.Value, error) {
	remaining := p.optimizer.options.MaxBytes - p.optimizer.bytes
	value, err := expression.Graph.CloneConstant(p.optimizer.ctx, id, remaining)
	if err != nil {
		return model.Value{}, err
	}
	used, err := model.ValueBytes(p.optimizer.ctx, value, remaining)
	if err != nil {
		return model.Value{}, err
	}
	if !p.optimizer.charge(used, 1) {
		return model.Value{}, p.optimizer.err
	}
	return value, nil
}

func (p *accessExpressionProof) equal(left accessScalarExpression, leftID ir.ScalarID, right accessScalarExpression, rightID ir.ScalarID, depth uint32) (bool, error) {
	if depth >= p.optimizer.options.IR.MaxDepth || !p.optimizer.step(1) {
		if depth >= p.optimizer.options.IR.MaxDepth {
			p.optimizer.err = ErrLimit
		}
		return false, p.optimizer.err
	}
	leftNode, leftOK := left.Graph.Scalar(leftID)
	rightNode, rightOK := right.Graph.Scalar(rightID)
	if !leftOK || !rightOK || !accessLayoutEqual(left.Graph, leftNode.Layout, right.Graph, rightNode.Layout) {
		return false, nil
	}
	leftColumn, leftIsColumn := p.column(left, leftNode)
	rightColumn, rightIsColumn := p.column(right, rightNode)
	leftMeta, rightMeta := leftNode.Metadata(), rightNode.Metadata()
	leftMeta.Scope, rightMeta.Scope = 0, 0
	leftMeta.Span, rightMeta.Span = ir.Span{}, ir.Span{}
	leftMeta.Layout, rightMeta.Layout = 0, 0
	leftMeta.Signature, rightMeta.Signature = 0, 0
	if leftIsColumn || rightIsColumn {
		if !leftIsColumn || !rightIsColumn || leftColumn != rightColumn {
			return false, nil
		}
		leftMeta.Kind, rightMeta.Kind = 0, 0
		leftMeta.Slot, rightMeta.Slot = 0, 0
		leftMeta.Parameter, rightMeta.Parameter = 0, 0
	}
	if leftMeta != rightMeta || leftNode.OperandCount() != rightNode.OperandCount() {
		return false, nil
	}
	if (leftNode.Signature == 0) != (rightNode.Signature == 0) {
		return false, nil
	}
	if leftNode.Signature != 0 {
		leftSignature, leftFound := left.Graph.Signature(leftNode.Signature)
		rightSignature, rightFound := right.Graph.Signature(rightNode.Signature)
		if !leftFound || !rightFound {
			return false, nil
		}
		leftSignatureMeta, rightSignatureMeta := leftSignature.Metadata(), rightSignature.Metadata()
		leftSignatureMeta.Result, rightSignatureMeta.Result = 0, 0
		if leftSignatureMeta != rightSignatureMeta || leftSignature.ParameterCount() != rightSignature.ParameterCount() {
			return false, nil
		}
		for i := range leftSignature.ParameterCount() {
			leftLayout, _ := left.Graph.SignatureParameter(leftNode.Signature, i)
			rightLayout, _ := right.Graph.SignatureParameter(rightNode.Signature, i)
			if !accessLayoutEqual(left.Graph, leftLayout, right.Graph, rightLayout) {
				return false, nil
			}
		}
	}
	if leftNode.Kind == ir.Const {
		leftValue, err := p.constant(left, leftID)
		if err != nil {
			return false, err
		}
		rightValue, err := p.constant(right, rightID)
		if err != nil || !reflect.DeepEqual(leftValue, rightValue) {
			return false, err
		}
	}
	for i := range leftNode.OperandCount() {
		leftOperand, _ := left.Graph.Operand(leftID, i)
		rightOperand, _ := right.Graph.Operand(rightID, i)
		equal, err := p.equal(left, leftOperand, right, rightOperand, depth+1)
		if err != nil || !equal {
			return equal, err
		}
	}
	return true, nil
}

func accessConjunction(node ir.Scalar) bool {
	return node.Kind == ir.Bool && node.BoolOp == ir.And
}

func accessDisjunction(node ir.Scalar) bool {
	return node.Kind == ir.Bool && node.BoolOp == ir.Or
}

func (p *accessExpressionProof) booleanConstant(expression accessScalarExpression, id ir.ScalarID) (model.Value, bool, error) {
	node, ok := expression.Graph.Scalar(id)
	if !ok || node.Kind != ir.Const {
		return model.Value{}, false, nil
	}
	value, err := p.constant(expression, id)
	return value, true, err
}

func (p *accessExpressionProof) implies(antecedent accessScalarExpression, antecedentID ir.ScalarID, consequent accessScalarExpression, consequentID ir.ScalarID, depth uint32) (bool, error) {
	if depth >= p.optimizer.options.IR.MaxDepth || !p.optimizer.step(1) {
		if depth >= p.optimizer.options.IR.MaxDepth {
			p.optimizer.err = ErrLimit
		}
		return false, p.optimizer.err
	}
	equal, err := p.equal(antecedent, antecedentID, consequent, consequentID, depth+1)
	if err != nil || equal {
		return equal, err
	}
	left, leftOK := antecedent.Graph.Scalar(antecedentID)
	right, rightOK := consequent.Graph.Scalar(consequentID)
	if !leftOK || !rightOK {
		return false, nil
	}
	if value, constant, err := p.booleanConstant(consequent, consequentID); err != nil || constant && !value.IsNull && value.Type == model.TypeBoolean && value.IntVal != 0 {
		return err == nil && constant && value.IntVal != 0, err
	}
	if value, constant, err := p.booleanConstant(antecedent, antecedentID); err != nil || constant && (value.IsNull || value.Type == model.TypeBoolean && value.IntVal == 0) {
		return err == nil && constant, err
	}
	if accessConjunction(right) {
		for i := range right.OperandCount() {
			child, _ := consequent.Graph.Operand(consequentID, i)
			proved, err := p.implies(antecedent, antecedentID, consequent, child, depth+1)
			if err != nil || !proved {
				return proved, err
			}
		}
		return true, nil
	}
	if accessConjunction(left) {
		for i := range left.OperandCount() {
			child, _ := antecedent.Graph.Operand(antecedentID, i)
			proved, err := p.implies(antecedent, child, consequent, consequentID, depth+1)
			if err != nil || proved {
				return proved, err
			}
		}
	}
	if accessDisjunction(left) {
		for i := range left.OperandCount() {
			child, _ := antecedent.Graph.Operand(antecedentID, i)
			proved, err := p.implies(antecedent, child, consequent, consequentID, depth+1)
			if err != nil || !proved {
				return proved, err
			}
		}
		return true, nil
	}
	if accessDisjunction(right) {
		for i := range right.OperandCount() {
			child, _ := consequent.Graph.Operand(consequentID, i)
			proved, err := p.implies(antecedent, antecedentID, consequent, child, depth+1)
			if err != nil || proved {
				return proved, err
			}
		}
	}
	if right.Kind == ir.NullTest && right.Negated && left.Kind == ir.Call && left.Strict {
		wanted, _ := consequent.Graph.Operand(consequentID, 0)
		for i := range left.OperandCount() {
			actual, _ := antecedent.Graph.Operand(antecedentID, i)
			equal, err := p.equal(antecedent, actual, consequent, wanted, depth+1)
			if err != nil || equal {
				return equal, err
			}
		}
	}
	return p.comparisonImplies(antecedent, antecedentID, consequent, consequentID, depth+1)
}

func (p *accessExpressionProof) comparisonImplies(antecedent accessScalarExpression, antecedentID ir.ScalarID, consequent accessScalarExpression, consequentID ir.ScalarID, depth uint32) (bool, error) {
	left, _ := antecedent.Graph.Scalar(antecedentID)
	right, _ := consequent.Graph.Scalar(consequentID)
	if left.Kind != ir.Call || right.Kind != ir.Call || !left.Strict || !right.Strict || left.OperandCount() != 2 || right.OperandCount() != 2 {
		return false, nil
	}
	leftExpression, _ := antecedent.Graph.Operand(antecedentID, 0)
	leftConstant, _ := antecedent.Graph.Operand(antecedentID, 1)
	rightExpression, _ := consequent.Graph.Operand(consequentID, 0)
	rightConstant, _ := consequent.Graph.Operand(consequentID, 1)
	leftExpressionNode, _ := antecedent.Graph.Scalar(leftExpression)
	rightExpressionNode, _ := consequent.Graph.Scalar(rightExpression)
	reverseLeft, reverseRight := leftExpressionNode.Kind == ir.Const, rightExpressionNode.Kind == ir.Const
	if reverseLeft {
		leftExpression, leftConstant = leftConstant, leftExpression
	}
	if reverseRight {
		rightExpression, rightConstant = rightConstant, rightExpression
	}
	leftConstantNode, _ := antecedent.Graph.Scalar(leftConstant)
	rightConstantNode, _ := consequent.Graph.Scalar(rightConstant)
	if leftConstantNode.Kind != ir.Const || rightConstantNode.Kind != ir.Const {
		return false, nil
	}
	same, err := p.equal(antecedent, leftExpression, consequent, rightExpression, depth+1)
	if err != nil || !same {
		return false, err
	}
	leftSignature, leftOK := antecedent.Graph.Signature(left.Signature)
	rightSignature, rightOK := consequent.Graph.Signature(right.Signature)
	if !leftOK || !rightOK || leftSignature.InputCollation != rightSignature.InputCollation {
		return false, nil
	}
	leftValue, err := p.constant(antecedent, leftConstant)
	if err != nil {
		return false, err
	}
	rightValue, err := p.constant(consequent, rightConstant)
	if err != nil || leftValue.IsNull || rightValue.IsNull {
		return false, err
	}
	for leftMeaning := range builtin.RowComparisonInterpretations(left.Operator) {
		for rightMeaning := range builtin.RowComparisonInterpretations(right.Operator) {
			if leftMeaning.Family != rightMeaning.Family {
				continue
			}
			leftType, leftConstantType := leftMeaning.Left, leftMeaning.Right
			rightType, rightConstantType := rightMeaning.Left, rightMeaning.Right
			if reverseLeft {
				leftType, leftConstantType = leftConstantType, leftType
			}
			if reverseRight {
				rightType, rightConstantType = rightConstantType, rightType
			}
			if leftType != rightType {
				continue
			}
			comparator := accessConstantComparator(leftMeaning.Family, leftConstantType, rightConstantType)
			if comparator == 0 {
				continue
			}
			leftStrategy, rightStrategy := leftMeaning.Strategy, rightMeaning.Strategy
			if reverseLeft && leftStrategy != 3 && leftStrategy != 6 {
				leftStrategy = 6 - leftStrategy
			}
			if reverseRight && rightStrategy != 3 && rightStrategy != 6 {
				rightStrategy = 6 - rightStrategy
			}
			options := consequent.Builtin
			options.Collation = leftSignature.InputCollation
			compare, err := builtin.Resolve(comparator, options)
			if err != nil {
				if cause := context.Cause(p.optimizer.ctx); cause != nil {
					return false, cause
				}
				continue
			}
			compared, err := compare.Call(p.optimizer.ctx, []model.Value{leftValue, rightValue}, builtin.CallBudget{MemoryBytes: p.optimizer.options.MaxBytes - p.optimizer.bytes})
			if err != nil {
				if cause := context.Cause(p.optimizer.ctx); cause != nil {
					return false, cause
				}
				continue
			}
			pairs := [5][2]int{{-1, -1}, {0, 0}, {1, 1}}
			count := 3
			if compared.IntVal < 0 {
				pairs = [5][2]int{{-1, -1}, {0, -1}, {1, -1}, {1, 0}, {1, 1}}
				count = 5
			}
			if compared.IntVal > 0 {
				pairs = [5][2]int{{-1, -1}, {-1, 0}, {-1, 1}, {0, 1}, {1, 1}}
				count = 5
			}
			proved := true
			for _, pair := range pairs[:count] {
				if accessStrategy(leftStrategy, pair[0]) && !accessStrategy(rightStrategy, pair[1]) {
					proved = false
					break
				}
			}
			if proved {
				return true, nil
			}
		}
	}
	return false, nil
}

func accessConstantComparator(family model.OID, left, right model.TypeID) model.FunctionID {
	for operator := range builtin.OperatorsNamed("=") {
		if operator.Left != left || operator.Right != right {
			continue
		}
		for meaning := range builtin.RowComparisonInterpretations(operator.ID) {
			if meaning.Family == family && meaning.Strategy == 3 {
				return meaning.CompareFunction
			}
		}
	}
	return 0
}

func accessStrategy(strategy uint8, sign int) bool {
	switch strategy {
	case 1:
		return sign < 0
	case 2:
		return sign <= 0
	case 3:
		return sign == 0
	case 4:
		return sign >= 0
	case 5:
		return sign > 0
	case 6:
		return sign != 0
	default:
		return false
	}
}

func (o *optimizer) predicateImplies(object model.ObjectID, predicate ir.ScalarID, required AccessExpression) bool {
	if predicate == 0 || required.Graph == nil || required.Root == 0 {
		return false
	}
	proof := accessExpressionProof{optimizer: o}
	proved, err := proof.implies(o.queryExpression(object, predicate), predicate, o.accessExpression(required), required.Root, 0)
	if err != nil {
		o.err = err
	}
	return proved
}

func (o *optimizer) findAccessExpression(object model.ObjectID, root ir.ScalarID, required AccessExpression, depth uint32) ir.ScalarID {
	if root == 0 || required.Graph == nil || depth >= o.options.IR.MaxDepth || !o.step(1) {
		if depth >= o.options.IR.MaxDepth {
			o.err = ErrLimit
		}
		return 0
	}
	proof := accessExpressionProof{optimizer: o}
	equal, err := proof.equal(o.queryExpression(object, root), root, o.accessExpression(required), required.Root, depth+1)
	if err != nil {
		o.err = err
		return 0
	}
	if equal {
		return root
	}
	node, _ := o.graph.Scalar(root)
	for i := range node.OperandCount() {
		child, _ := o.graph.Operand(root, i)
		if found := o.findAccessExpression(object, child, required, depth+1); found != 0 {
			return found
		}
	}
	return 0
}

func (o *optimizer) accessStatisticOrdinal(object model.ObjectID, id ir.ScalarID, path AccessPath, depth uint32) (int, bool) {
	proof := accessExpressionProof{optimizer: o}
	for ordinal, expression := range path.KeyExpressions {
		if expression.Graph == nil {
			continue
		}
		equal, err := proof.equal(o.queryExpression(object, id), id, o.accessExpression(expression), expression.Root, depth+1)
		if err != nil {
			o.err = err
			return 0, false
		}
		if equal {
			return ordinal, true
		}
	}
	return 0, false
}
func (o *optimizer) accessDirectStatisticKey(object model.ObjectID, id ir.ScalarID, path AccessPath) (int, model.SlotID, bool) {
	node, ok := o.graph.Scalar(id)
	if !ok || node.Kind != ir.Slot {
		return 0, 0, false
	}
	column, ok := (&accessExpressionProof{optimizer: o}).column(o.queryExpression(object, id), node)
	if !ok {
		return 0, 0, false
	}
	for ordinal, key := range path.Keys {
		if ordinal < len(path.KeyExpressions) && path.KeyExpressions[ordinal].Graph == nil && key.ColumnID == column {
			return ordinal, node.Slot, true
		}
	}
	return 0, 0, false
}

func (o *optimizer) accessPathPredicateConjunct(object model.ObjectID, id ir.ScalarID, required AccessExpression, requiredID ir.ScalarID, depth uint32) bool {
	if depth >= o.options.IR.MaxDepth || !o.step(1) {
		if o.err == nil {
			o.err = ErrLimit
		}
		return false
	}
	proof := accessExpressionProof{optimizer: o}
	equal, err := proof.equal(o.queryExpression(object, id), id, o.accessExpression(required), requiredID, depth+1)
	if err != nil {
		o.err = err
		return false
	}
	if equal {
		return true
	}
	node, ok := required.Graph.Scalar(requiredID)
	if !ok || !accessConjunction(node) {
		return false
	}
	for i := range node.OperandCount() {
		child, _ := required.Graph.Operand(requiredID, i)
		if o.accessPathPredicateConjunct(object, id, required, child, depth+1) {
			return true
		}
		if o.err != nil {
			return false
		}
	}
	return false
}


func accessStatisticOperator(path AccessPath, ordinal int, operatorID model.OperatorID) (string, bool) {
	if ordinal < 0 || ordinal >= len(path.Operators) || path.Operators[ordinal] == 0 {
		return "", false
	}
	operator, ok := builtin.LookupOperator(operatorID)
	if !ok {
		return "", false
	}
	switch operator.Name {
	case "=", "<>", "<", "<=", ">", ">=":
	default:
		return "", false
	}
	for actual := range builtin.RowComparisonInterpretations(operatorID) {
		for expected := range builtin.RowComparisonInterpretations(path.Operators[ordinal]) {
			if actual.Family == expected.Family && expected.Strategy == 3 {
				return operator.Name, true
			}
		}
	}
	return "", false
}

func accessStatisticColumn(table *statistics.Table, ordinal int) (statistics.Column, bool) {
	id := model.ColumnID(ordinal + 1)
	for _, column := range table.Columns {
		if column.ID == id {
			return column, true
		}
	}
	return statistics.Column{}, false
}

func reverseAccessStatisticOperator(operator string) string {
	switch operator {
	case "<":
		return ">"
	case "<=":
		return ">="
	case ">":
		return "<"
	case ">=":
		return "<="
	default:
		return operator
	}
}

func (o *optimizer) accessStatisticSelectivity(object model.ObjectID, id ir.ScalarID, path AccessPath, table *statistics.Table, depth uint32) (float64, bool) {
	if id == 0 || !o.step(1) {
		return 0, false
	}
	if depth >= o.options.IR.MaxDepth {
		o.err = ErrLimit
		return 0, false
	}
	if path.Predicate.Graph != nil && o.accessPathPredicateConjunct(object, id, path.Predicate, path.Predicate.Root, depth+1) {
		return 1, true
	}
	if o.err != nil {
		return 0, false
	}
	scalar, _ := o.graph.Scalar(id)
	if scalar.Kind == ir.Bool && (scalar.BoolOp == ir.And || scalar.BoolOp == ir.Or) {
		result, known := 1.0, false
		for i := range scalar.OperandCount() {
			child, _ := o.graph.Operand(id, i)
			selectivity, childKnown := o.accessStatisticSelectivity(object, child, path, table, depth+1)
			if scalar.BoolOp == ir.And {
				if childKnown {
					result *= selectivity
					known = true
				}
				continue
			}
			if !childKnown {
				return 0, false
			}
			result *= 1 - selectivity
			known = true
		}
		if scalar.BoolOp == ir.Or && known {
			result = 1 - result
		}
		return result, known
	}
	if scalar.Kind == ir.NullTest && scalar.OperandCount() == 1 {
		operand, _ := o.graph.Operand(id, 0)
		ordinal, expressionMatched := o.accessStatisticOrdinal(object, operand, path, depth+1)
		var column statistics.Column
		var found bool
		if expressionMatched {
			column, found = accessStatisticColumn(table, ordinal)
		} else {
			_, slot, directMatched := o.accessDirectStatisticKey(object, operand, path)
			if directMatched {
				column, found = o.column(slot)
			}
		}
		if !found {
			return 0, false
		}
		if scalar.Negated {
			return 1 - column.NullFraction, true
		}
		return column.NullFraction, true
	}
	if scalar.Kind != ir.Call || scalar.OperandCount() != 2 {
		return 0, false
	}
	leftID, _ := o.graph.Operand(id, 0)
	rightID, _ := o.graph.Operand(id, 1)
	ordinal, expressionMatched := o.accessStatisticOrdinal(object, leftID, path, depth+1)
	directSlot, directMatched := model.SlotID(0), false
	if !expressionMatched {
		ordinal, directSlot, directMatched = o.accessDirectStatisticKey(object, leftID, path)
	}
	constantID, reversed := rightID, false
	if !expressionMatched && !directMatched {
		ordinal, expressionMatched = o.accessStatisticOrdinal(object, rightID, path, depth+1)
		if !expressionMatched {
			ordinal, directSlot, directMatched = o.accessDirectStatisticKey(object, rightID, path)
		}
		constantID, reversed = leftID, true
	}
	constant, _ := o.graph.Scalar(constantID)
	operator, supported := accessStatisticOperator(path, ordinal, scalar.Operator)
	if (!expressionMatched && !directMatched) || constant.Kind != ir.Const || !supported {
		return 0, false
	}
	if reversed {
		operator = reverseAccessStatisticOperator(operator)
	}
	if directMatched {
		return o.constantSelectivity(directSlot, constantID, operator)
	}
	column, found := accessStatisticColumn(table, ordinal)
	if !found {
		return 0, false
	}
	value, ok := o.statisticConstant(constantID)
	if !ok {
		return 0, false
	}
	return o.statisticValueSelectivity(column, value, operator)
}

func (o *optimizer) accessPathSelectivity(object model.ObjectID, predicate ir.ScalarID, path AccessPath, table *statistics.Table) float64 {
	selectivity, known := o.accessStatisticSelectivity(object, predicate, path, table, 0)
	if !known {
		return 1
	}
	return min(1, max(0, selectivity))
}

func equalAccessExpression(left, right AccessExpression) bool {
	if left.Graph != right.Graph || left.Root != right.Root || len(left.Parameters) != len(right.Parameters) || !reflect.DeepEqual(left.Builtin, right.Builtin) {
		return false
	}
	for i := range left.Parameters {
		if left.Parameters[i].Column != right.Parameters[i].Column || !model.TypeLayoutsEqual(left.Parameters[i].Layout, right.Parameters[i].Layout) {
			return false
		}
	}
	return true
}

func equalAccessExpressions(left, right []AccessExpression) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !equalAccessExpression(left[i], right[i]) {
			return false
		}
	}
	return true
}

func equalLookupAccess(left, right LookupAccess) bool {
	return left.Enabled == right.Enabled &&
		left.OwnerColumn == right.OwnerColumn &&
		left.Target == right.Target &&
		equalAccessColumns(left.Layout, right.Layout) &&
		slices.Equal(left.ColumnMap, right.ColumnMap) &&
		storage.KeyLayoutsEqual(left.Keys, right.Keys)
}
