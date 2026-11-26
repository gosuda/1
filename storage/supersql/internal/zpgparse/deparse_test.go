package zpgparse

import (
	"testing"

	p "github.com/gosuda/supersql-alpha/internal/zpgparse/internal/pgproto"
)

func deparseSelectTree(expr *p.Node) *ParseResult {
	return &ParseResult{Stmts: []*p.RawStmt{{Stmt: &p.Node{Node: &p.Node_SelectStmt{SelectStmt: &p.SelectStmt{
		Op:         p.SetOperation_SETOP_NONE,
		TargetList: []*p.Node{p.MakeResTargetNodeWithVal(expr, -1)},
	}}}}}}
}

func requireDeparsedSQL(t *testing.T, tree *ParseResult, want string) {
	t.Helper()
	got, err := Deparse(tree)
	if err != nil {
		t.Fatalf("Deparse: %v", err)
	}
	if got != want {
		t.Fatalf("Deparse SQL\n got: %s\nwant: %s", got, want)
	}
}

func TestDeparsePreservesOperatorGrouping(t *testing.T) {
	product := p.MakeAExprNode(p.A_Expr_Kind_AEXPR_OP, []*p.Node{p.MakeStrNode("*")}, p.MakeAConstIntNode(2, -1), p.MakeAConstIntNode(3, -1), -1)
	sum := p.MakeAExprNode(p.A_Expr_Kind_AEXPR_OP, []*p.Node{p.MakeStrNode("+")}, p.MakeAConstIntNode(1, -1), product, -1)
	requireDeparsedSQL(t, deparseSelectTree(sum), "SELECT 1 + (2 * 3)")

	got, err := DeparseExpression(sum)
	if err != nil {
		t.Fatal(err)
	}
	if got != "1 + (2 * 3)" {
		t.Fatalf("DeparseExpression = %q; want %q", got, "1 + (2 * 3)")
	}
}

func TestDeparseQuotesIdentifiersAndLiterals(t *testing.T) {
	tree := deparseSelectTree(p.MakeAConstStrNode("x'\\y", -1))
	tree.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Name = "select"
	tree.Stmts[0].Stmt.GetSelectStmt().FromClause = []*p.Node{{Node: &p.Node_RangeVar{RangeVar: &p.RangeVar{Relname: "a\"b", Inh: true}}}}
	requireDeparsedSQL(t, tree, "SELECT E'x''\\\\y' AS \"select\" FROM \"a\"\"b\"")
}

func TestDeparseKeepsTimezoneInsideCast(t *testing.T) {
	zone := &p.Node{Node: &p.Node_FuncCall{FuncCall: &p.FuncCall{
		Funcname:   []*p.Node{p.MakeStrNode("pg_catalog"), p.MakeStrNode("timezone")},
		Args:       []*p.Node{p.MakeAConstStrNode("UTC", -1), p.MakeFuncCallNode([]*p.Node{p.MakeStrNode("now")}, nil, -1)},
		Funcformat: p.CoercionForm_COERCE_SQL_SYNTAX,
	}}}
	cast := &p.Node{Node: &p.Node_TypeCast{TypeCast: &p.TypeCast{
		Arg:      zone,
		TypeName: &p.TypeName{Names: []*p.Node{p.MakeStrNode("pg_catalog"), p.MakeStrNode("timestamp")}},
	}}}
	requireDeparsedSQL(t, deparseSelectTree(cast), "SELECT (now() AT TIME ZONE 'UTC')::timestamp")
}

func TestDeparseNestedJSONConstructors(t *testing.T) {
	object := &p.Node{Node: &p.Node_JsonObjectConstructor{JsonObjectConstructor: &p.JsonObjectConstructor{
		Exprs: []*p.Node{{Node: &p.Node_JsonKeyValue{JsonKeyValue: &p.JsonKeyValue{
			Key:   p.MakeAConstStrNode("k", -1),
			Value: &p.JsonValueExpr{RawExpr: p.MakeAConstIntNode(1, -1)},
		}}}},
	}}}
	array := &p.Node{Node: &p.Node_JsonArrayConstructor{JsonArrayConstructor: &p.JsonArrayConstructor{
		Exprs:        []*p.Node{{Node: &p.Node_JsonValueExpr{JsonValueExpr: &p.JsonValueExpr{RawExpr: object}}}},
		AbsentOnNull: true,
	}}}
	requireDeparsedSQL(t, deparseSelectTree(array), "SELECT JSON_ARRAY(JSON_OBJECT('k': 1))")
}

func TestDeparseMultipleAssignment(t *testing.T) {
	source := &p.Node{Node: &p.Node_RowExpr{RowExpr: &p.RowExpr{
		RowFormat: p.CoercionForm_COERCE_IMPLICIT_CAST,
		Args:      []*p.Node{p.MakeAConstIntNode(1, -1), p.MakeAConstIntNode(2, -1)},
	}}}
	update := &p.UpdateStmt{Relation: &p.RangeVar{Relname: "t", Inh: true}}
	for i, name := range []string{"a", "b"} {
		update.TargetList = append(update.TargetList, p.MakeResTargetNodeWithNameAndVal(name,
			&p.Node{Node: &p.Node_MultiAssignRef{MultiAssignRef: &p.MultiAssignRef{Source: source, Colno: int32(i + 1), Ncolumns: 2}}}, -1))
	}
	tree := &ParseResult{Stmts: []*p.RawStmt{{Stmt: &p.Node{Node: &p.Node_UpdateStmt{UpdateStmt: update}}}}}
	requireDeparsedSQL(t, tree, "UPDATE t SET (a, b) = (1, 2)")
}

func TestDeparseWindowFrameExclusion(t *testing.T) {
	column := func(name string) *p.Node { return p.MakeColumnRefNode([]*p.Node{p.MakeStrNode(name)}, -1) }
	call := &p.Node{Node: &p.Node_FuncCall{FuncCall: &p.FuncCall{
		Funcname: []*p.Node{p.MakeStrNode("sum")}, Args: []*p.Node{column("v")},
		Over: &p.WindowDef{
			OrderClause:  []*p.Node{p.MakeSortByNode(column("id"), p.SortByDir_SORTBY_DEFAULT, p.SortByNulls_SORTBY_NULLS_DEFAULT, -1)},
			FrameOptions: deparseFrameNondefault | deparseFrameRows | deparseFrameBetween | deparseFrameStartOffsetPreceding | deparseFrameEndCurrentRow | deparseFrameExcludeTies,
			StartOffset:  p.MakeAConstIntNode(2, -1),
		},
	}}}
	requireDeparsedSQL(t, deparseSelectTree(call), "SELECT sum(v) OVER (ORDER BY id ROWS BETWEEN 2 PRECEDING AND CURRENT ROW EXCLUDE TIES)")
}

// TestDeparseAlterColumnGenericOptionsMatchesReference pins the reference
// spacing of ALTER ... OPTIONS subcommands. The reference deparser appends
// ") " from its generic-options helper and only then trims a single trailing
// space, so a non-final OPTIONS subcommand keeps one space before the ", " the
// command list adds; the port reproduces that by writing two spaces and
// trimming one. Do not "clean up" the space: it is byte-identical to
// pg_query_go v6 (v6.2.3-0.20260902212127-e6a9b9881a8b, the oracle that
// generated testdata/postgresql-18.4.json.gz) and required by
// TestPostgreSQLCorpusDeparse for src/test/regress/sql/foreign_data.sql:406
// and :452.
func TestDeparseAlterColumnGenericOptionsMatchesReference(t *testing.T) {
	for _, tt := range []struct {
		sql  string
		want string
	}{
		{
			sql:  "ALTER FOREIGN TABLE ft ALTER COLUMN c OPTIONS (ADD x '1'), ALTER COLUMN d OPTIONS (SET y '2')",
			want: "ALTER FOREIGN TABLE ft ALTER COLUMN c OPTIONS (ADD x '1') , ALTER COLUMN d OPTIONS (SET y '2')",
		},
		{
			sql:  "ALTER FOREIGN TABLE ft OPTIONS (ADD x '1'), ALTER COLUMN d OPTIONS (SET y '2')",
			want: "ALTER FOREIGN TABLE ft OPTIONS (ADD x '1') , ALTER COLUMN d OPTIONS (SET y '2')",
		},
		{
			sql:  "ALTER FOREIGN TABLE ft ALTER COLUMN c OPTIONS (DROP x), ALTER COLUMN d OPTIONS (ADD y '2')",
			want: "ALTER FOREIGN TABLE ft ALTER COLUMN c OPTIONS (DROP x) , ALTER COLUMN d OPTIONS (ADD y '2')",
		},
		{
			sql:  "ALTER FOREIGN TABLE ft OPTIONS (ADD x '1')",
			want: "ALTER FOREIGN TABLE ft OPTIONS (ADD x '1')",
		},
		{
			sql:  "ALTER TABLE t ALTER COLUMN c OPTIONS (ADD x '1'), ALTER COLUMN d SET DEFAULT 1",
			want: "ALTER TABLE t ALTER COLUMN c OPTIONS (ADD x '1') , ALTER COLUMN d SET DEFAULT 1",
		},
	} {
		tree, err := Parse(tt.sql)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tt.sql, err)
		}
		requireDeparsedSQL(t, tree, tt.want)
	}
}

func TestDeparseRejectsAnalyzedExpression(t *testing.T) {
	tree := deparseSelectTree(&p.Node{Node: &p.Node_FuncExpr{FuncExpr: &p.FuncExpr{Funcid: 42}}})
	got, err := Deparse(tree)
	if err == nil || got != "" {
		t.Fatalf("Deparse analyzed expression = %q, %v; want empty SQL and an error", got, err)
	}
}
