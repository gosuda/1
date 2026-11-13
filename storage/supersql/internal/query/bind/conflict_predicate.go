package bind

import (
	"reflect"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func (b *binder) conflictExpressionEqual(a *BoundIndexExpression, ai ir.ScalarID, c *BoundIndexExpression, ci ir.ScalarID) (bool, error) {
	remaining := b.options.Limits.MaxNodes
	return b.conflictEqual(a, ai, c, ci, &remaining)
}
func (b *binder) conflictEqual(a *BoundIndexExpression, ai ir.ScalarID, c *BoundIndexExpression, ci ir.ScalarID, remaining *uint32) (bool, error) {
	if err := b.ctx.Err(); err != nil {
		return false, err
	}
	if *remaining == 0 {
		return false, bindError("54000", "conflict inference expression limit exceeded", 0)
	}
	*remaining--
	x, _ := a.Graph.Scalar(ai)
	y, _ := c.Graph.Scalar(ci)
	if !conflictLayoutEqual(a.Graph, x.Layout, c.Graph, y.Layout) {
		return false, nil
	}
	xm, ym := x.Metadata(), y.Metadata()
	xm.Scope, ym.Scope = 0, 0
	xm.Span, ym.Span = ir.Span{}, ir.Span{}
	xm.Layout, ym.Layout = 0, 0
	xm.Signature, ym.Signature = 0, 0
	if x.Kind == ir.Param && y.Kind == ir.Param {
		if x.Parameter == 0 || y.Parameter == 0 || int(x.Parameter) > len(a.Parameters) || int(y.Parameter) > len(c.Parameters) {
			return false, bindError("XX001", "invalid inference parameter", 0)
		}
		if a.Parameters[x.Parameter-1].Column != c.Parameters[y.Parameter-1].Column {
			return false, nil
		}
		xm.Parameter, ym.Parameter = 0, 0
	}
	if xm != ym || x.OperandCount() != y.OperandCount() {
		return false, nil
	}
	if (x.Signature == 0) != (y.Signature == 0) {
		return false, nil
	}
	if x.Signature != 0 {
		xs, _ := a.Graph.Signature(x.Signature)
		ys, _ := c.Graph.Signature(y.Signature)
		xx, yy := xs.Metadata(), ys.Metadata()
		xx.Result, yy.Result = 0, 0
		if xx != yy || xs.ParameterCount() != ys.ParameterCount() {
			return false, nil
		}
		for i := range xs.ParameterCount() {
			xl, _ := a.Graph.SignatureParameter(x.Signature, i)
			yl, _ := c.Graph.SignatureParameter(y.Signature, i)
			if !conflictLayoutEqual(a.Graph, xl, c.Graph, yl) {
				return false, nil
			}
		}
	}
	if x.Kind == ir.Const {
		xv, err := a.Graph.CloneConstant(b.ctx, ai, b.options.Limits.MaxBytes)
		if err != nil {
			return false, err
		}
		yv, err := c.Graph.CloneConstant(b.ctx, ci, b.options.Limits.MaxBytes)
		if err != nil {
			return false, err
		}
		if !reflect.DeepEqual(xv, yv) {
			return false, nil
		}
	}
	for i := range x.OperandCount() {
		ax, _ := a.Graph.Operand(ai, i)
		cy, _ := c.Graph.Operand(ci, i)
		equal, err := b.conflictEqual(a, ax, c, cy, remaining)
		if err != nil || !equal {
			return equal, err
		}
	}
	return true, nil
}
func conflictLayoutEqual(a *ir.Graph, ai ir.LayoutID, b *ir.Graph, bi ir.LayoutID) bool {
	at, _ := a.Type(ai)
	bt, _ := b.Type(bi)
	if at != bt || a.LayoutChildCount(ai) != b.LayoutChildCount(bi) {
		return false
	}
	for i := range a.LayoutChildCount(ai) {
		ac, _ := a.LayoutChild(ai, i)
		bc, _ := b.LayoutChild(bi, i)
		if !conflictLayoutEqual(a, ac, b, bc) {
			return false
		}
	}
	return true
}

type conflictTruth struct {
	expression *BoundIndexExpression
	scalar     ir.ScalarID
	negated    bool
}

// Every recursive rule preserves implication under SQL's true-only predicate
// semantics. Unsupported atoms remain unproved rather than approximated.
func (b *binder) conflictImplies(a *BoundIndexExpression, ai ir.ScalarID, c *BoundIndexExpression, ci ir.ScalarID, remaining *uint32) (bool, error) {
	return b.conflictTruthImplies(conflictTruth{expression: a, scalar: ai}, conflictTruth{expression: c, scalar: ci}, remaining)
}

func (b *binder) conflictTruthImplies(antecedent, consequent conflictTruth, remaining *uint32) (bool, error) {
	a, ai, an := antecedent.expression, antecedent.scalar, antecedent.negated
	c, ci, cn := consequent.expression, consequent.scalar, consequent.negated
	if err := b.ctx.Err(); err != nil {
		return false, err
	}
	if *remaining == 0 {
		return false, bindError("54000", "conflict predicate proof limit exceeded", 0)
	}
	*remaining--
	x, _ := a.Graph.Scalar(ai)
	y, _ := c.Graph.Scalar(ci)
	if x.Kind == ir.Bool && x.BoolOp == ir.Not {
		child, _ := a.Graph.Operand(ai, 0)
		return b.conflictTruthImplies(conflictTruth{a, child, !an}, consequent, remaining)
	}
	if y.Kind == ir.Bool && y.BoolOp == ir.Not {
		child, _ := c.Graph.Operand(ci, 0)
		return b.conflictTruthImplies(antecedent, conflictTruth{c, child, !cn}, remaining)
	}
	id, negated, ok, err := b.conflictBooleanAtom(a, ai, an)
	if err != nil {
		return false, err
	}
	if ok {
		return b.conflictTruthImplies(conflictTruth{a, id, negated}, consequent, remaining)
	}
	id, negated, ok, err = b.conflictBooleanAtom(c, ci, cn)
	if err != nil {
		return false, err
	}
	if ok {
		return b.conflictTruthImplies(antecedent, conflictTruth{c, id, negated}, remaining)
	}
	if an == cn {
		equal, err := b.conflictEqual(a, ai, c, ci, remaining)
		if err != nil || equal {
			return equal, err
		}
	}
	if y.Kind == ir.Const {
		v, err := c.Graph.CloneConstant(b.ctx, ci, b.options.Limits.MaxBytes)
		return err == nil && !v.IsNull && v.Type == model.TypeBoolean && (v.IntVal != 0) != cn, err
	}
	if x.Kind == ir.Const {
		v, err := a.Graph.CloneConstant(b.ctx, ai, b.options.Limits.MaxBytes)
		if err != nil {
			return false, err
		}
		if v.IsNull || v.Type == model.TypeBoolean && (v.IntVal != 0) == an {
			return true, nil
		}
	}
	if conflictConjunction(y, cn) {
		for i := range y.OperandCount() {
			id, _ := c.Graph.Operand(ci, i)
			ok, err := b.conflictTruthImplies(antecedent, conflictTruth{c, id, cn}, remaining)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	}
	if conflictConjunction(x, !an) {
		for i := range x.OperandCount() {
			id, _ := a.Graph.Operand(ai, i)
			ok, err := b.conflictTruthImplies(conflictTruth{a, id, an}, consequent, remaining)
			if err != nil || !ok {
				return ok, err
			}
		}
		return true, nil
	}
	if conflictConjunction(x, an) {
		for i := range x.OperandCount() {
			id, _ := a.Graph.Operand(ai, i)
			ok, err := b.conflictTruthImplies(conflictTruth{a, id, an}, consequent, remaining)
			if err != nil || ok {
				return ok, err
			}
		}
	}
	if conflictConjunction(y, !cn) {
		for i := range y.OperandCount() {
			id, _ := c.Graph.Operand(ci, i)
			ok, err := b.conflictTruthImplies(antecedent, conflictTruth{c, id, cn}, remaining)
			if err != nil || ok {
				return ok, err
			}
		}
	}
	if y.Kind == ir.NullTest && y.Negated != cn && x.Kind == ir.Call && x.Strict {
		wanted, _ := c.Graph.Operand(ci, 0)
		for i := range x.OperandCount() {
			actual, _ := a.Graph.Operand(ai, i)
			ok, err := b.conflictEqual(a, actual, c, wanted, remaining)
			if err != nil || ok {
				return ok, err
			}
		}
	}
	return b.conflictComparisonImplies(a, ai, an, c, ci, cn, remaining)
}

func (b *binder) conflictComparisonImplies(a *BoundIndexExpression, ai ir.ScalarID, negateA bool, c *BoundIndexExpression, ci ir.ScalarID, negateC bool, remaining *uint32) (bool, error) {
	x, _ := a.Graph.Scalar(ai)
	y, _ := c.Graph.Scalar(ci)
	if !conflictStrictBinary(x) || !conflictStrictBinary(y) {
		return false, nil
	}
	ax, _ := a.Graph.Operand(ai, 0)
	av, _ := a.Graph.Operand(ai, 1)
	cy, _ := c.Graph.Operand(ci, 0)
	cv, _ := c.Graph.Operand(ci, 1)
	axn, _ := a.Graph.Scalar(ax)
	cyn, _ := c.Graph.Scalar(cy)
	reverseA, reverseC := axn.Kind == ir.Const, cyn.Kind == ir.Const
	if reverseA {
		ax, av = av, ax
	}
	if reverseC {
		cy, cv = cv, cy
	}
	an, _ := a.Graph.Scalar(av)
	cn, _ := c.Graph.Scalar(cv)
	if an.Kind != ir.Const || cn.Kind != ir.Const {
		return false, nil
	}
	same, err := b.conflictEqual(a, ax, c, cy, remaining)
	if err != nil || !same {
		return false, err
	}
	as, _ := a.Graph.Signature(x.Signature)
	cs, _ := c.Graph.Signature(y.Signature)
	if as.InputCollation != cs.InputCollation {
		return false, nil
	}
	va, err := a.Graph.CloneConstant(b.ctx, av, b.options.Limits.MaxBytes)
	if err != nil {
		return false, err
	}
	vc, err := c.Graph.CloneConstant(b.ctx, cv, b.options.Limits.MaxBytes)
	if err != nil {
		return false, err
	}
	if va.IsNull || vc.IsNull {
		return false, nil
	}
	for left := range builtin.RowComparisonInterpretations(x.Operator) {
		for right := range builtin.RowComparisonInterpretations(y.Operator) {
			if left.Family != right.Family {
				continue
			}
			leftType, leftConstant := left.Left, left.Right
			rightType, rightConstant := right.Left, right.Right
			if reverseA {
				leftType, leftConstant = leftConstant, leftType
			}
			if reverseC {
				rightType, rightConstant = rightConstant, rightType
			}
			if leftType != rightType {
				continue
			}
			comparator := conflictConstantComparator(left.Family, leftConstant, rightConstant)
			if comparator == 0 {
				continue
			}
			leftStrategy, rightStrategy := left.Strategy, right.Strategy
			if reverseA && leftStrategy != 3 && leftStrategy != 6 {
				leftStrategy = 6 - leftStrategy
			}
			if reverseC && rightStrategy != 3 && rightStrategy != 6 {
				rightStrategy = 6 - rightStrategy
			}
			if negateA {
				leftStrategy = conflictNegateStrategy(leftStrategy)
			}
			if negateC {
				rightStrategy = conflictNegateStrategy(rightStrategy)
			}
			options := a.Builtin
			options.Collation = as.InputCollation
			compare, err := builtin.Resolve(comparator, options)
			if err != nil {
				return false, err
			}
			compared, err := compare.Call(b.ctx, []model.Value{va, vc}, builtin.CallBudget{OutputBytes: 0, MemoryBytes: b.options.Limits.MaxBytes})
			if err != nil {
				return false, err
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
			proves := true
			for _, pair := range pairs[:count] {
				if conflictStrategy(leftStrategy, pair[0]) && !conflictStrategy(rightStrategy, pair[1]) {
					proves = false
					break
				}
			}
			if proves {
				return true, nil
			}
		}
	}
	return false, nil
}
func conflictStrategy(strategy uint8, sign int) bool {
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
	}
	return false
}

func conflictConstantComparator(family model.OID, left, right model.TypeID) model.FunctionID {
	for operator := range builtin.OperatorsNamed("=") {
		if operator.Left != left || operator.Right != right {
			continue
		}
		for interpretation := range builtin.RowComparisonInterpretations(operator.ID) {
			if interpretation.Family == family && interpretation.Strategy == 3 {
				return interpretation.CompareFunction
			}
		}
	}
	return 0
}

func conflictNegateStrategy(strategy uint8) uint8 {
	switch strategy {
	case 1:
		return 4
	case 2:
		return 5
	case 3:
		return 6
	case 4:
		return 1
	case 5:
		return 2
	case 6:
		return 3
	}
	return 0
}

func (b *binder) conflictBooleanAtom(bound *BoundIndexExpression, id ir.ScalarID, negated bool) (ir.ScalarID, bool, bool, error) {
	node, _ := bound.Graph.Scalar(id)
	if node.Kind != ir.Call || node.OperandCount() != 2 {
		return 0, false, false, nil
	}
	operator, ok := builtin.LookupOperator(node.Operator)
	booleanOperands := operator.Left == 16 && operator.Right == 16
	equality := operator.Name == "=" || operator.Name == "<>"
	if !ok || !booleanOperands || !equality {
		return 0, false, false, nil
	}
	left, _ := bound.Graph.Operand(id, 0)
	right, _ := bound.Graph.Operand(id, 1)
	constant, _ := bound.Graph.Scalar(right)
	if constant.Kind != ir.Const {
		left, right = right, left
		constant, _ = bound.Graph.Scalar(right)
	}
	if constant.Kind != ir.Const {
		return 0, false, false, nil
	}
	value, err := bound.Graph.CloneConstant(b.ctx, right, b.options.Limits.MaxBytes)
	if err != nil {
		return 0, false, false, err
	}
	if value.IsNull {
		return 0, false, false, nil
	}
	invert := value.IntVal == 0
	if operator.Name == "<>" {
		invert = !invert
	}
	return left, negated != invert, true, nil
}

func conflictConjunction(node ir.Scalar, negated bool) bool {
	if node.Kind != ir.Bool {
		return false
	}
	if negated {
		return node.BoolOp == ir.Or
	}
	return node.BoolOp == ir.And
}

func conflictStrictBinary(node ir.Scalar) bool {
	return node.Kind == ir.Call && node.Strict && node.OperandCount() == 2
}
