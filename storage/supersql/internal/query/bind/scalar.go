package bind

import (
	"strconv"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type expression struct {
	id             ir.ScalarID
	sourceSlot     model.SlotID
	typ            model.TypeLayout
	nullable       bool
	effects        ir.Effects
	position       int
	parameter      uint32
	value          model.Value
	literal        *zpgparse.Node
	reads          []model.SlotID
	inferred       model.TypeID
	collation      collationState
	record         recordSource
	rowArgs        []expression
	recordFields   []column
	routineOutputs []model.RoutineParameter
	// Row comparison syntax is independent of descriptor provenance.
	rowSyntax bool
}

func (b *binder) addScalar(s *scope, e expression, payload ir.Scalar, args []expression) (expression, error) {
	return b.addScalarComparisons(s, e, payload, args, nil)
}

func (b *binder) addScalarComparisons(s *scope, e expression, payload ir.Scalar, args []expression, comparisons []ir.SignatureID) (expression, error) {
	if payload.Kind == ir.Row {
		e.record = recordConstructor
		e.rowArgs = args
	}
	switch payload.Kind {
	case ir.Case, ir.Coalesce, ir.Array, ir.ArrayStack:
		state, err := b.commonCollation(args)
		if err != nil {
			return e, err
		}
		e.typ, e.collation = b.resultCollation(e.typ, state)
	}
	var err error
	e.typ, err = b.presentationLayout(e.typ, 0)
	if err != nil {
		return e, err
	}
	layout, err := b.layout(e.typ)
	if err != nil {
		return e, err
	}
	ids := make([]ir.ScalarID, len(args))
	for i, arg := range args {
		ids[i] = arg.id
		e.effects = e.effects.Union(arg.effects)
		e.reads = append(e.reads, arg.reads...)
	}
	payload.Scope = s.id
	payload.Layout = layout
	payload.Nullable = e.nullable
	payload.Effects = e.effects
	payload.Span = sourceSpan(e.position)
	e.sourceSlot = payload.Slot
	e.id, err = b.builder.AddScalar(b.ctx, ir.ScalarSpec{Scalar: payload, Operands: ids, Comparisons: comparisons, Value: e.value})
	return e, err
}

func (b *binder) once(s *scope, input expression) (expression, error) {
	// Unknown literals and parameters acquire their type at the comparison.
	if input.id == 0 {
		return input, nil
	}
	return b.addScalar(s, expression{
		typ: input.typ, nullable: input.nullable, position: input.position,
		collation: b.expressionCollation(input), record: input.record,
		recordFields: input.recordFields, rowArgs: input.rowArgs, rowSyntax: input.rowSyntax,
	}, ir.Scalar{Kind: ir.Once}, []expression{input})
}
func (b *binder) scalar(node *zpgparse.Node, s *scope) (expression, error) {
	if err := b.enter(); err != nil {
		return expression{}, err
	}
	defer func() { b.depth-- }()
	if node == nil {
		return expression{nullable: true, value: model.NullVal(model.TypeString)}, nil
	}
	if value := node.GetSetToDefault(); value != nil {
		return expression{}, bindError("42601", "DEFAULT is not allowed in this context", int(value.Location)+1)
	}
	if n := node.GetMergeSupportFunc(); n != nil {
		for current := s; current != nil; current = current.parent {
			if current.mergeActionSlot != 0 {
				return b.reference(s, column{ResultColumn: ResultColumn{Slot: current.mergeActionSlot, Layout: b.primitive(25)}}, int(n.Location)+1)
			}
		}
		return expression{}, bindError("42601", "MERGE_ACTION() can only be used in the RETURNING list of a MERGE command", int(n.Location)+1)
	}
	if grouped, matched, err := b.groupedExpression(node, s, nil); matched {
		return grouped, err
	}
	if grouping := node.GetGroupingFunc(); grouping != nil {
		return b.groupingFunction(grouping, s)
	}
	if n := node.GetBooleanTest(); n != nil {
		return b.booleanTest(n, s)
	}
	if n := node.GetAConst(); n != nil {
		e := expression{position: int(n.Location) + 1, literal: node}
		if n.Isnull {
			e.nullable = true
			e.value = model.NullVal(model.TypeString)
			return e, nil
		}
		switch {
		case n.GetIval() != nil:
			e.typ = b.primitive(23)
			e.value = model.Int64Val(int64(n.GetIval().Ival))
		case n.GetFval() != nil:
			text := n.GetFval().Fval
			if integer, err := strconv.ParseInt(text, 10, 64); err == nil {
				id := model.TypeID(20)
				if integer == int64(int32(integer)) {
					id = 23
				}
				e.typ = b.primitive(id)
				e.value = model.Int64Val(integer)
			} else {
				e.value = model.StringVal(text)
				e.inferred = 1700
				return e, nil
			}
		case n.GetBoolval() != nil:
			e.typ = b.primitive(16)
			e.value = model.BoolVal(n.GetBoolval().Boolval)
		case n.GetSval() != nil:
			e.value = model.StringVal(n.GetSval().Sval)
			return e, nil
		case n.GetBsval() != nil:
			e.value = model.StringVal(n.GetBsval().Bsval)
			return b.inputLiteral(s, e, b.primitive(1560))
		default:
			return e, bindError("42601", "invalid constant", e.position)
		}
		return b.addScalar(s, e, ir.Scalar{Kind: ir.Const}, nil)
	}
	if n := node.GetSqlvalueFunction(); n != nil {
		return b.sqlValueFunction(n, s)
	}
	if n := node.GetParamRef(); n != nil {
		pos := int(n.Location) + 1
		if n.Number <= 0 {
			return expression{}, bindError("42P02", "invalid parameter number", pos)
		}
		number := uint32(n.Number)
		if number > b.options.Limits.MaxOperands {
			return expression{}, bindError("54000", "parameter limit exceeded", pos)
		}
		b.maxParam = max(b.maxParam, number)
		e := expression{parameter: number, nullable: true, position: pos}
		if typ, ok := b.params[number]; ok {
			return b.resolveUnknown(s, e, typ)
		}
		return e, nil
	}
	if n := node.GetColumnRef(); n != nil {
		parts, err := names(n.Fields)
		if err != nil {
			return expression{}, err
		}
		c, err := s.lookup(parts, int(n.Location)+1)
		if err != nil {
			return b.wholeRowReference(s, parts, int(n.Location)+1, err)
		}
		if grouped, ok := s.groupSlots[c.Slot]; ok && !s.inAggregate {
			c = grouped
		}
		return b.reference(s, c, int(n.Location)+1)
	}
	if n := node.GetCollateClause(); n != nil {
		return b.collate(n, s)
	}
	if n := node.GetTypeCast(); n != nil {
		typ, err := b.typeName(n.TypeName)
		if err != nil {
			return expression{}, err
		}
		if array := n.Arg.GetAArrayExpr(); array != nil && typ.Type.Storage == model.TypeArray && len(typ.Children) == 1 {
			return b.arrayConstructor(array, s, &typ)
		}
		if row := n.Arg.GetRowExpr(); row != nil && typ.Type.Storage == model.TypeComposite && typ.Type.ID != 2249 {
			args, err := b.rowArguments(row.Args, s, int(row.Location)+1)
			if err != nil {
				return expression{}, err
			}
			return b.coerceRecord(s, expression{typ: b.primitive(2249), record: recordConstructor, rowArgs: args, position: int(row.Location) + 1}, typ, ir.ExplicitCast)
		}
		e, err := b.scalar(n.Arg, s)
		if err != nil {
			return e, err
		}
		e, err = b.coerce(s, e, typ, ir.ExplicitCast)
		e.rowSyntax = false
		return e, err
	}
	if n := node.GetAIndirection(); n != nil {
		return b.indirection(n, s)
	}
	if n := node.GetAExpr(); n != nil {
		return b.operatorExpression(n, s)
	}
	if n := node.GetFuncCall(); n != nil {
		return b.function(n, s)
	}
	if n := node.GetBoolExpr(); n != nil {
		args, err := b.scalarList(n.Args, s)
		if err != nil {
			return expression{}, err
		}
		op := ir.And
		switch n.Boolop {
		case zpgparse.BoolExprType_OR_EXPR:
			op = ir.Or
		case zpgparse.BoolExprType_NOT_EXPR:
			op = ir.Not
		}
		for i := range args {
			args[i], err = b.coerce(s, args[i], b.primitive(16), ir.ImplicitCast)
			if err != nil {
				return expression{}, err
			}
		}
		return b.boolean(s, op, args, int(n.Location)+1)
	}
	if n := node.GetNullTest(); n != nil {
		arg, err := b.scalar(n.Arg, s)
		if err != nil {
			return arg, err
		}
		arg, err = b.defaultType(s, arg)
		if err != nil {
			return arg, err
		}
		return b.addScalar(s, expression{typ: b.primitive(16), position: int(n.Location) + 1}, ir.Scalar{Kind: ir.NullTest, Negated: n.Nulltesttype == zpgparse.NullTestType_IS_NOT_NULL}, []expression{arg})
	}
	if n := node.GetMinMaxExpr(); n != nil {
		return b.minMax(n, s)
	}
	if n := node.GetCoalesceExpr(); n != nil {
		restore := b.restrictSRFs("COALESCE")
		defer restore()
		args, err := b.scalarList(n.Args, s)
		if err != nil {
			return expression{}, err
		}
		typ, err := b.commonType(args)
		if err != nil {
			return expression{}, err
		}
		nullable := true
		for i := range args {
			args[i], err = b.coerceCommon(s, args[i], typ)
			if err != nil {
				return expression{}, err
			}
			nullable = nullable && args[i].nullable
		}
		return b.addScalar(s, expression{typ: typ, nullable: nullable, position: int(n.Location) + 1}, ir.Scalar{Kind: ir.Coalesce}, args)
	}
	if n := node.GetCaseExpr(); n != nil {
		return b.caseExpression(n, s)
	}
	if n := node.GetRowExpr(); n != nil {
		args, err := b.rowArguments(n.Args, s, int(n.Location)+1)
		if err != nil {
			return expression{}, err
		}
		typ := b.primitive(2249)
		typ.Children = make([]model.TypeLayout, len(args))
		resolved := args
		for _, arg := range args {
			if arg.typ.Type.ID == 0 {
				resolved = append([]expression(nil), args...)
				break
			}
		}
		for i := range args {
			if args[i].typ.Type.ID == 0 && args[i].parameter != 0 && b.argumentID(args[i]) == 0 {
				return expression{}, bindError("42P18", "could not determine record field parameter type", args[i].position)
			}
			if args[i].typ.Type.ID == 0 && args[i].inferred == 0 && args[i].parameter == 0 {
				field := args[i]
				field.typ = b.primitive(705)
				resolved[i], err = b.addScalar(s, field, ir.Scalar{Kind: ir.Const}, nil)
			} else {
				resolved[i], err = b.defaultType(s, args[i])
			}
			if err != nil {
				return expression{}, err
			}
			typ.Children[i] = resolved[i].typ
		}
		row, err := b.addScalar(s, expression{typ: typ, position: int(n.Location) + 1}, ir.Scalar{Kind: ir.Row}, resolved)
		row.rowArgs, row.rowSyntax = args, true
		return row, err
	}
	if n := node.GetAArrayExpr(); n != nil {
		return b.arrayConstructor(n, s, nil)
	}
	if n := node.GetJsonValueExpr(); n != nil {
		return b.jsonFormattedValue(n, s, 0)
	}
	if n := node.GetJsonParseExpr(); n != nil {
		return b.jsonParse(n, s)
	}
	if n := node.GetJsonScalarExpr(); n != nil {
		return b.jsonScalar(n, s)
	}
	if n := node.GetJsonSerializeExpr(); n != nil {
		return b.jsonSerialize(n, s)
	}
	if n := node.GetJsonArrayQueryConstructor(); n != nil {
		return b.jsonArrayQueryConstructor(n, s)
	}
	if n := node.GetJsonArrayConstructor(); n != nil {
		return b.jsonArrayConstructor(n, s)
	}
	if n := node.GetJsonObjectConstructor(); n != nil {
		return b.jsonObjectConstructor(n, s)
	}
	if n := node.GetJsonArrayAgg(); n != nil {
		return b.jsonArrayAggregate(n, s)
	}
	if n := node.GetJsonObjectAgg(); n != nil {
		return b.jsonObjectAggregate(n, s)
	}
	if n := node.GetJsonIsPredicate(); n != nil {
		return b.jsonIsPredicate(n, s)
	}
	if n := node.GetJsonFuncExpr(); n != nil {
		return b.jsonFunction(n, s)
	}
	if n := node.GetSubLink(); n != nil {
		return b.subquery(n, s)
	}
	return expression{}, missing("scalar AST form", 0)
}
func (b *binder) scalarList(nodes []*zpgparse.Node, s *scope) ([]expression, error) {
	if uint64(len(nodes)) > uint64(b.options.Limits.MaxOperands) {
		return nil, bindError("54000", "expression operand limit exceeded", 0)
	}
	args := make([]expression, len(nodes))
	for i, n := range nodes {
		arg, err := b.scalar(n, s)
		if err != nil {
			return nil, err
		}
		args[i] = arg
	}
	return args, nil
}
func (b *binder) boolean(s *scope, op ir.BoolOp, args []expression, pos int) (expression, error) {
	e := expression{typ: b.primitive(16), position: pos}
	for _, arg := range args {
		e.nullable = e.nullable || arg.nullable
	}
	return b.addScalar(s, e, ir.Scalar{Kind: ir.Bool, BoolOp: op}, args)
}
func (b *binder) caseExpression(n *zpgparse.CaseExpr, s *scope) (expression, error) {
	restore := b.restrictSRFs("CASE")
	defer restore()
	var base expression
	var err error
	if n.Arg != nil {
		base, err = b.scalar(n.Arg, s)
		if err != nil {
			return base, err
		}
	}
	if n.Arg != nil {
		base, err = b.once(s, base)
		if err != nil {
			return base, err
		}
	}
	conditions := make([]expression, len(n.Args))
	results := make([]expression, len(n.Args)+1)
	for i, node := range n.Args {
		arm := node.GetCaseWhen()
		if arm == nil {
			return expression{}, bindError("42601", "invalid CASE arm", int(n.Location)+1)
		}
		conditions[i], err = b.scalar(arm.Expr, s)
		if err != nil {
			return expression{}, err
		}
		if n.Arg != nil {
			conditions[i], err = b.operator(s, "=", []expression{base, conditions[i]}, int(arm.Location)+1)
		} else {
			conditions[i], err = b.coerce(s, conditions[i], b.primitive(16), ir.ImplicitCast)
		}
		if err != nil {
			return expression{}, err
		}
		results[i], err = b.scalar(arm.Result, s)
		if err != nil {
			return expression{}, err
		}
	}
	results[len(results)-1], err = b.scalar(n.Defresult, s)
	if err != nil {
		return expression{}, err
	}
	typ, err := b.commonType(results)
	if err != nil {
		return expression{}, err
	}
	args := make([]expression, 0, 2*len(conditions)+1)
	nullable := false
	for i, result := range results {
		result, err = b.coerceCommon(s, result, typ)
		if err != nil {
			return expression{}, err
		}
		nullable = nullable || result.nullable
		if i < len(conditions) {
			args = append(args, conditions[i])
		}
		args = append(args, result)
	}
	return b.addScalar(s, expression{typ: typ, nullable: nullable, position: int(n.Location) + 1}, ir.Scalar{Kind: ir.Case}, args)
}
func (b *binder) subquery(n *zpgparse.SubLink, s *scope) (expression, error) {
	child, err := b.newScope(s)
	if err != nil {
		return expression{}, err
	}
	if n.Subselect.GetSelectStmt() == nil {
		return expression{}, bindError("42601", "subquery must be SELECT", int(n.Location)+1)
	}
	rel, err := b.selectStatement(n.Subselect.GetSelectStmt(), child, nil)
	if err != nil {
		return expression{}, err
	}
	position := int(n.Location) + 1
	e := expression{typ: b.primitive(16), effects: rel.effects, position: position, reads: b.captures[child.id]}
	payload := ir.Scalar{Kind: ir.Subquery, Relation: rel.id}
	switch n.SubLinkType {
	case zpgparse.SubLinkType_EXISTS_SUBLINK:
		payload.SubqueryKind = ir.ExistsSubquery
	case zpgparse.SubLinkType_EXPR_SUBLINK:
		if len(rel.columns) != 1 {
			return e, bindError("42601", "subquery must return only one column", position)
		}
		payload.SubqueryKind = ir.ScalarSubquery
		e.typ = rel.columns[0].Layout
		e.nullable = true
		if rel.columns[0].record == recordDeclared {
			e.record = recordDeclared
			e.recordFields = rel.columns[0].recordFields
		}
		if b.collatable(e.typ.Type) {
			e.collation = collationState{e.typ.Type.Collation, collationImplicit, position}
		}
		e.effects.MayError = true
	case zpgparse.SubLinkType_ANY_SUBLINK, zpgparse.SubLinkType_ALL_SUBLINK:
		return b.quantifiedSubquery(n, s, child, rel)
	case zpgparse.SubLinkType_ROWCOMPARE_SUBLINK:
		left, err := b.scalar(n.Testexpr, s)
		if err != nil {
			return e, err
		}
		name, err := subqueryOperatorName(n.OperName)
		if err != nil {
			return e, err
		}
		return b.comparisonSubquery(s, child, rel, left, name, ir.RowSubquery, position)
	case zpgparse.SubLinkType_ARRAY_SUBLINK:
		if len(rel.columns) != 1 {
			return e, bindError("42601", "subquery must return only one column", position)
		}
		output := rel.columns[0].Layout
		if output.Type.Storage == model.TypeArray && output.Type.ID != 22 && output.Type.ID != 30 {
			e.typ = output
		} else {
			e.typ, err = b.companionArrayLayout(output, position)
			if err != nil {
				return e, err
			}
		}
		e.nullable = false
		e.effects.MayError = true
		payload.SubqueryKind = ir.ArraySubquery
	default:
		return e, missing("subquery form", position)
	}
	return b.addScalar(s, e, payload, nil)
}
