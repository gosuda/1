package bind

import (
	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type resolvedOperator struct {
	operator  model.OperatorID
	signature builtin.Signature
	// userOP is non-nil when a SQL-defined operator won overload resolution.
	// Its implementation is a user routine, so it cannot be invoked through the
	// builtin signature path.
	userOP *userOperator
}

func (b *binder) resolveOperator(name string, args []expression, pos int) (resolvedOperator, error) {
	best := -1
	bestPreferred := -1
	ambiguous := false
	var selected resolvedOperator
	// A user-defined operator with this name participates in the same overload
	// resolution as pinned builtins. PostgreSQL prefers the search path, so a
	// user candidate wins only when its score is strictly better than any
	// builtin candidate's; an equal score is an ambiguity error.
	userBest := -1
	var userSelected userOperator
	userAmbiguous := false
	for _, candidate := range b.userOperatorsNamed(name) {
		score := b.userOperatorScore(candidate, args, pos)
		if score < 0 {
			continue
		}
		if score > userBest {
			userBest, userSelected, userAmbiguous = score, candidate, false
		} else if score == userBest {
			userAmbiguous = true
		}
	}
	assumed, hasAssumption := b.operatorAssumedArguments(args)
	var assumedSelected resolvedOperator
	assumedCount := 0
	for op := range builtin.OperatorsNamed(name) {
		unaryMismatch := len(args) == 1 && op.Left != 0
		binaryMismatch := len(args) == 2 && op.Left == 0
		if unaryMismatch || binaryMismatch {
			continue
		}
		sig, ok := builtin.LookupSignature(op.Function)
		if !ok {
			return selected, missing("operator implementation signature metadata", pos)
		}
		score := b.overloadScore(sig, args)
		if score < 0 {
			continue
		}
		if len(args) == 2 {
			left, right := b.argumentID(args[0]), b.argumentID(args[1])
			if left == 0 && right != 0 && op.Left == right && op.Right == right {
				score += 20
			}
			if right == 0 && left != 0 && op.Left == left && op.Right == left {
				score += 20
			}
		}
		preferred := b.preferredConversions(sig, args)
		better := score > best || (score == best && preferred > bestPreferred)
		if better {
			best = score
			bestPreferred = preferred
			selected = resolvedOperator{operator: op.ID, signature: sig}
			ambiguous = false
			assumedCount = 0
		} else if score == best && preferred == bestPreferred {
			ambiguous = true
		}
		matchesBest := score == best && preferred == bestPreferred
		if hasAssumption && matchesBest && b.overloadScore(sig, assumed[:]) >= 0 {
			assumedSelected = resolvedOperator{operator: op.ID, signature: sig}
			assumedCount++
		}
	}
	if b.castErr != nil {
		return selected, b.castErr
	}
	switch {
	case userBest > best:
		// A strictly better user candidate: the operator is user-defined.
		if userAmbiguous {
			return selected, bindError("42725", "operator is ambiguous", pos)
		}
		chosen := userSelected
		return resolvedOperator{operator: model.OperatorID(userSelected.descriptor.OID()), userOP: &chosen}, nil
	case userBest >= 0 && userBest == best:
		return selected, bindError("42725", "operator is ambiguous", pos)
	}
	if best < 0 {
		return selected, bindError("42883", "operator does not exist for argument types", pos)
	}
	if ambiguous && assumedCount == 1 {
		selected, ambiguous = assumedSelected, false
	}
	if ambiguous {
		return selected, bindError("42725", "operator is ambiguous", pos)
	}
	if err := ValidateBuiltinFunction(selected.signature.ID, pos); err != nil {
		return selected, err
	}
	return selected, nil
}

func (b *binder) operatorAssumedArguments(args []expression) ([2]expression, bool) {
	var assumed [2]expression
	if len(args) != 2 {
		return assumed, false
	}
	left, right := b.argumentID(args[0]), b.argumentID(args[1])
	if (left == 0) == (right == 0) {
		return assumed, false
	}
	known := 0
	if left == 0 {
		known = 1
	}
	typ := b.argumentType(b.argumentID(args[known]), args[known])
	for i, arg := range args {
		assumed[i] = expression{typ: typ, position: arg.position}
	}
	return assumed, true
}

func (b *binder) preferredConversions(sig builtin.Signature, args []expression) int {
	preferred := 0
	for i, arg := range args {
		source := b.argumentID(arg)
		if source == signatureArgument(sig, i) {
			continue
		}
		input, known := builtin.LookupType(source)
		target, found := builtin.LookupType(signatureArgument(sig, i))
		if !known || !found {
			continue
		}
		if target.Preferred && target.Category == input.Category {
			preferred++
		}
	}
	return preferred
}
func (b *binder) operator(s *scope, name string, args []expression, pos int) (expression, error) {
	selected, err := b.resolveOperator(name, args, pos)
	if err != nil {
		return expression{}, err
	}
	if selected.userOP != nil {
		return b.userOperator(s, selected.userOP.descriptor, selected.userOP.metadata, args, pos)
	}
	return b.invoke(s, selected.signature, selected.operator, args, ir.Call, 0, pos)
}
func (b *binder) operatorExpression(n *zpgparse.A_Expr, s *scope) (expression, error) {
	switch n.Kind {
	case zpgparse.A_Expr_Kind_AEXPR_BETWEEN, zpgparse.A_Expr_Kind_AEXPR_NOT_BETWEEN,
		zpgparse.A_Expr_Kind_AEXPR_BETWEEN_SYM, zpgparse.A_Expr_Kind_AEXPR_NOT_BETWEEN_SYM:
		return b.betweenExpression(n, s)
	}
	pos := int(n.Location) + 1
	parts, err := names(n.Name)
	if err != nil {
		return expression{}, err
	}
	if len(parts) == 0 || len(parts) > 2 {
		return expression{}, bindError("42601", "invalid operator name", pos)
	}
	if len(parts) == 2 && parts[0] != "pg_catalog" {
		return expression{}, bindError("42883", "operator does not exist", pos)
	}
	name := parts[len(parts)-1]
	if n.Kind == zpgparse.A_Expr_Kind_AEXPR_OP_ANY || n.Kind == zpgparse.A_Expr_Kind_AEXPR_OP_ALL {
		return b.quantifiedArray(n, s, name, pos)
	}
	if n.Kind == zpgparse.A_Expr_Kind_AEXPR_OP && n.Lexpr != nil && n.Lexpr.GetRowExpr() != nil && n.Rexpr != nil {
		if raw := n.Rexpr.GetSubLink(); raw != nil && raw.SubLinkType == zpgparse.SubLinkType_EXPR_SUBLINK {
			subquery := proto.Clone(raw).(*zpgparse.SubLink)
			subquery.SubLinkType = zpgparse.SubLinkType_ROWCOMPARE_SUBLINK
			subquery.Testexpr = n.Lexpr
			subquery.OperName = n.Name
			subquery.Location = n.Location
			return b.subquery(subquery, s)
		}
	}
	if n.Kind == zpgparse.A_Expr_Kind_AEXPR_NULLIF {
		return b.nullIfExpression(n, s, name)
	}
	if n.Kind == zpgparse.A_Expr_Kind_AEXPR_DISTINCT || n.Kind == zpgparse.A_Expr_Kind_AEXPR_NOT_DISTINCT {
		return b.distinctFrom(n, s, name)
	}
	if n.Kind == zpgparse.A_Expr_Kind_AEXPR_IN {
		lhs, err := b.scalar(n.Lexpr, s)
		if err != nil {
			return lhs, err
		}
		list := n.Rexpr.GetList()
		if list == nil {
			return lhs, bindError("42601", "invalid IN list", pos)
		}
		args, err := b.scalarList(list.Items, s)
		if err != nil {
			return lhs, err
		}
		if len(args) == 0 {
			return lhs, bindError("42601", "empty IN list", pos)
		}
		lhs, err = b.onceComparisonArgument(s, lhs)
		if err != nil {
			return lhs, err
		}
		comparisons := make([]expression, len(args))
		for i, arg := range args {
			comparisons[i], err = b.syntaxOperator(s, name, []expression{lhs, arg}, pos)
			if err != nil {
				return lhs, err
			}
		}
		if len(comparisons) == 1 {
			return comparisons[0], nil
		}
		op := ir.Or
		if name == "<>" {
			op = ir.And
		}
		return b.boolean(s, op, comparisons, pos)
	}
	if n.Kind != zpgparse.A_Expr_Kind_AEXPR_OP && n.Kind != zpgparse.A_Expr_Kind_AEXPR_LIKE && n.Kind != zpgparse.A_Expr_Kind_AEXPR_ILIKE && n.Kind != zpgparse.A_Expr_Kind_AEXPR_SIMILAR {
		return expression{}, missing("special operator expression", pos)
	}
	if n.Lexpr != nil && n.Rexpr != nil && n.Lexpr.GetRowExpr() != nil && n.Rexpr.GetRowExpr() != nil {
		var rows [2]expression
		for i, node := range []*zpgparse.Node{n.Lexpr, n.Rexpr} {
			fields, err := b.rowArguments(node.GetRowExpr().Args, s, pos)
			if err != nil {
				return expression{}, err
			}
			rows[i] = expression{record: recordConstructor, rowArgs: fields, rowSyntax: true, position: pos}
		}
		return b.syntaxOperator(s, name, rows[:], pos)
	}
	var args []expression
	if n.Lexpr != nil {
		arg, err := b.scalar(n.Lexpr, s)
		if err != nil {
			return arg, err
		}
		args = append(args, arg)
	}
	if n.Rexpr != nil {
		arg, err := b.scalar(n.Rexpr, s)
		if err != nil {
			return arg, err
		}
		args = append(args, arg)
	}
	return b.syntaxOperator(s, name, args, pos)
}
