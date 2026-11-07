package zpgparse

import p "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/pgproto"

func (w *deparseWriter) parenthesizedIn(node *p.Node, parens bool, context deparseExpressionContext) {
	if parens {
		w.text("(")
	}
	w.exprIn(node, context)
	if parens {
		w.text(")")
	}
}

func (w *deparseWriter) cExpr(node *p.Node) {
	if node == nil {
		return
	}
	switch node.Node.(type) {
	case *p.Node_ColumnRef, *p.Node_AConst, *p.Node_ParamRef, *p.Node_AIndirection,
		*p.Node_CaseExpr, *p.Node_SubLink, *p.Node_AArrayExpr, *p.Node_RowExpr,
		*p.Node_GroupingFunc, *p.Node_FuncCall, *p.Node_SqlvalueFunction,
		*p.Node_MinMaxExpr, *p.Node_CoalesceExpr, *p.Node_XmlExpr,
		*p.Node_XmlSerialize, *p.Node_JsonObjectAgg, *p.Node_JsonArrayAgg,
		*p.Node_JsonObjectConstructor, *p.Node_JsonArrayConstructor,
		*p.Node_JsonArrayQueryConstructor:
		w.exprIn(node, deparseExpressionNone)
	default:
		w.parenthesized(node, true)
	}
}

func (w *deparseWriter) bExpr(node *p.Node) {
	if node == nil {
		return
	}
	if node.GetXmlExpr() != nil {
		w.exprIn(node, deparseExpressionNone)
		return
	}
	if n := node.GetAExpr(); n != nil {
		switch n.Kind.String() {
		case "AEXPR_OP", "AEXPR_DISTINCT", "AEXPR_NOT_DISTINCT":
			w.operatorExpr(n, deparseExpressionNone)
			return
		}
	}
	if n := node.GetBoolExpr(); n != nil && n.Boolop.String() == "NOT_EXPR" {
		w.boolExpr(n)
		return
	}
	w.cExpr(node)
}

func (w *deparseWriter) windowless(node *p.Node) {
	if node == nil {
		w.invalid("nil function expression")
		return
	}
	switch n := node.Node.(type) {
	case *p.Node_TypeCast:
		w.text("CAST(")
		w.expr(n.TypeCast.Arg)
		w.text(" AS ")
		w.typeName(n.TypeCast.TypeName)
		w.text(")")
	case *p.Node_FuncCall, *p.Node_SqlvalueFunction, *p.Node_CoalesceExpr,
		*p.Node_MinMaxExpr, *p.Node_XmlExpr, *p.Node_XmlSerialize:
		w.exprIn(node, deparseExpressionNone)
	default:
		w.invalid("unpermitted windowless function expression %T", node.Node)
	}
}

