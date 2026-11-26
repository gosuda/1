package pgwire

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

var parseOpts = zpgparse.ParseOptions{
	AllowSystemTime: true,
	AllowOnlineDDL:  true,
}

// splitSimpleQuery splits a PostgreSQL Simple Query ('Q') string into
// individual non-empty statements using zpgparse.
func splitSimpleQuery(input string) ([]string, []*zpgparse.RawStmt, error) {
	if strings.TrimSpace(input) == "" {
		return nil, nil, nil
	}
	// Try standard zpgparse.SplitWithParser first, falling back to ParseExtended
	// when SuperSQL extensions (AS OF SYSTEM TIME, ONLINE DDL) are used.
	ext, err := zpgparse.ParseExtended(input, parseOpts)
	if err != nil {
		return nil, nil, err
	}
	if ext == nil || ext.ParseResult == nil || len(ext.ParseResult.Stmts) == 0 {
		return nil, nil, nil
	}
	if len(ext.TemporalClauses) == 0 && len(ext.OnlineDDLClauses) == 0 {
		if split, splitErr := zpgparse.SplitWithParser(input, true); splitErr == nil && len(split) == len(ext.ParseResult.Stmts) {
			return split, ext.ParseResult.Stmts, nil
		}
	}
	stmts := make([]string, 0, len(ext.ParseResult.Stmts))
	raws := make([]*zpgparse.RawStmt, 0, len(ext.ParseResult.Stmts))
	for _, raw := range ext.ParseResult.Stmts {
		if raw == nil || raw.Stmt == nil {
			continue
		}
		start := int(raw.StmtLocation)
		end := len(input)
		if raw.StmtLen != 0 && start+int(raw.StmtLen) <= len(input) {
			end = start + int(raw.StmtLen)
		}
		if start < 0 || start > len(input) || end < start {
			continue
		}
		text := strings.TrimSpace(input[start:end])
		if text == "" {
			continue
		}
		stmts = append(stmts, text)
		raws = append(raws, raw)
	}
	return stmts, raws, nil
}

// parseSingleStatement parses an Extended Query ('P') statement string.
// Returns (nil, nil) for an empty query string (which is valid in PostgreSQL).
func parseSingleStatement(input string) (*zpgparse.RawStmt, error) {
	if strings.TrimSpace(input) == "" {
		return nil, nil
	}
	ext, err := zpgparse.ParseExtended(input, parseOpts)
	if err != nil {
		return nil, err
	}
	if ext == nil || ext.ParseResult == nil || len(ext.ParseResult.Stmts) == 0 {
		return nil, nil
	}
	if len(ext.ParseResult.Stmts) > 1 {
		return nil, &wireError{
			Severity: "ERROR",
			Code:     "42601",
			Message:  "cannot insert multiple commands into a prepared statement",
		}
	}
	return ext.ParseResult.Stmts[0], nil
}

// stmtReturnsRows reports whether a parsed statement produces a result row set
// (RowDescription + DataRows) rather than NoData.
func stmtReturnsRows(raw *zpgparse.RawStmt) bool {
	if raw == nil || raw.Stmt == nil {
		return false
	}
	n := raw.Stmt
	if sel := n.GetSelectStmt(); sel != nil {
		return sel.IntoClause == nil
	}
	if ins := n.GetInsertStmt(); ins != nil {
		return ins.ReturningClause != nil && len(ins.ReturningClause.Exprs) > 0
	}
	if upd := n.GetUpdateStmt(); upd != nil {
		return upd.ReturningClause != nil && len(upd.ReturningClause.Exprs) > 0
	}
	if del := n.GetDeleteStmt(); del != nil {
		return del.ReturningClause != nil && len(del.ReturningClause.Exprs) > 0
	}
	if mrg := n.GetMergeStmt(); mrg != nil {
		return mrg.ReturningClause != nil && len(mrg.ReturningClause.Exprs) > 0
	}
	if n.GetVariableShowStmt() != nil || n.GetExplainStmt() != nil {
		return true
	}
	if fetch := n.GetFetchStmt(); fetch != nil {
		return !fetch.Ismove
	}
	return false
}

// stmtCommandVerb returns the PostgreSQL command tag verb for row-returning
// statements.
func stmtCommandVerb(raw *zpgparse.RawStmt) string {
	if raw == nil || raw.Stmt == nil {
		return "SELECT"
	}
	n := raw.Stmt
	switch {
	case n.GetSelectStmt() != nil:
		return "SELECT"
	case n.GetInsertStmt() != nil:
		return "INSERT"
	case n.GetUpdateStmt() != nil:
		return "UPDATE"
	case n.GetDeleteStmt() != nil:
		return "DELETE"
	case n.GetMergeStmt() != nil:
		return "MERGE"
	case n.GetVariableShowStmt() != nil:
		return "SHOW"
	case n.GetExplainStmt() != nil:
		return "EXPLAIN"
	case n.GetFetchStmt() != nil:
		if n.GetFetchStmt().Ismove {
			return "MOVE"
		}
		return "FETCH"
	default:
		return "SELECT"
	}
}

func formatRowCommandTag(verb string, rowCount int64) string {
	switch verb {
	case "SHOW", "EXPLAIN":
		return verb
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", rowCount)
	case "UPDATE", "DELETE", "SELECT", "MOVE", "FETCH", "MERGE":
		return fmt.Sprintf("%s %d", verb, rowCount)
	default:
		return fmt.Sprintf("SELECT %d", rowCount)
	}
}

type txStmtKind int

const (
	txStmtNone txStmtKind = iota
	txStmtBegin
	txStmtCommit
	txStmtRollback
	txStmtRollbackTo
	txStmtSavepoint
	txStmtRelease
	txStmtPrepareTx
)

type txStmtInfo struct {
	kind  txStmtKind
	chain bool
}

func classifyTxStmt(raw *zpgparse.RawStmt) txStmtInfo {
	if raw == nil || raw.Stmt == nil {
		return txStmtInfo{}
	}
	tx := raw.Stmt.GetTransactionStmt()
	if tx == nil {
		return txStmtInfo{}
	}
	switch tx.Kind {
	case zpgparse.TransactionStmtKind_TRANS_STMT_BEGIN,
		zpgparse.TransactionStmtKind_TRANS_STMT_START:
		return txStmtInfo{kind: txStmtBegin}
	case zpgparse.TransactionStmtKind_TRANS_STMT_COMMIT:
		return txStmtInfo{kind: txStmtCommit, chain: tx.Chain}
	case zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK:
		return txStmtInfo{kind: txStmtRollback, chain: tx.Chain}
	case zpgparse.TransactionStmtKind_TRANS_STMT_ROLLBACK_TO:
		return txStmtInfo{kind: txStmtRollbackTo}
	case zpgparse.TransactionStmtKind_TRANS_STMT_SAVEPOINT:
		return txStmtInfo{kind: txStmtSavepoint}
	case zpgparse.TransactionStmtKind_TRANS_STMT_RELEASE:
		return txStmtInfo{kind: txStmtRelease}
	case zpgparse.TransactionStmtKind_TRANS_STMT_PREPARE:
		return txStmtInfo{kind: txStmtPrepareTx}
	default:
		return txStmtInfo{}
	}
}

type tableRef struct {
	schema string
	table  string
	alias  string
}

func (r tableRef) qualifiedSQL() string {
	if r.schema != "" {
		return quoteIdent(r.schema) + "." + quoteIdent(r.table)
	}
	return quoteIdent(r.table)
}

func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}

type paramColHint struct {
	table    tableRef
	colName  string // empty if positional (e.g. INSERT without column list)
	colIndex int    // 0-based position when colName == ""
}

type paramAnalysis struct {
	maxParam     int
	explicitOIDs map[int]uint32
	colHints     map[int]paramColHint
}

func analyzeParams(raw *zpgparse.RawStmt) paramAnalysis {
	out := paramAnalysis{
		explicitOIDs: make(map[int]uint32),
		colHints:     make(map[int]paramColHint),
	}
	if raw == nil || raw.Stmt == nil {
		return out
	}

	// 1. Walk the entire AST to discover all ParamRef numbers and direct TypeCasts.
	walkProtoMessage(raw.Stmt.ProtoReflect(), func(msg protoreflect.Message) {
		switch node := msg.Interface().(type) {
		case *zpgparse.ParamRef:
			if n := int(node.Number); n > out.maxParam {
				out.maxParam = n
			}
		case *zpgparse.TypeCast:
			if node.Arg != nil {
				if pr := node.Arg.GetParamRef(); pr != nil && pr.Number > 0 {
					if oid := extractTypeNameOID(node.TypeName); oid != 0 {
						if _, exists := out.explicitOIDs[int(pr.Number)]; !exists {
							out.explicitOIDs[int(pr.Number)] = oid
						}
					}
				}
			}
		case *zpgparse.SelectStmt:
			if node.LimitCount != nil {
				if pr := node.LimitCount.GetParamRef(); pr != nil && pr.Number > 0 {
					if _, exists := out.explicitOIDs[int(pr.Number)]; !exists {
						out.explicitOIDs[int(pr.Number)] = oidInt8
					}
				}
			}
			if node.LimitOffset != nil {
				if pr := node.LimitOffset.GetParamRef(); pr != nil && pr.Number > 0 {
					if _, exists := out.explicitOIDs[int(pr.Number)]; !exists {
						out.explicitOIDs[int(pr.Number)] = oidInt8
					}
				}
			}
		}
	})

	if out.maxParam == 0 {
		return out
	}

	// 2. Collect table-column hints for DML/SELECT statements so Describe('S')
	// can resolve exact column OIDs for parameters without explicit casts.
	collectTableHints(raw.Stmt, &out)
	return out
}

func walkProtoMessage(msg protoreflect.Message, visit func(protoreflect.Message)) {
	if !msg.IsValid() {
		return
	}
	visit(msg)
	msg.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		switch {
		case fd.IsList():
			if fd.Kind() == protoreflect.MessageKind {
				list := v.List()
				for i := 0; i < list.Len(); i++ {
					walkProtoMessage(list.Get(i).Message(), visit)
				}
			}
		case fd.IsMap():
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				v.Map().Range(func(_ protoreflect.MapKey, mv protoreflect.Value) bool {
					walkProtoMessage(mv.Message(), visit)
					return true
				})
			}
		case fd.Kind() == protoreflect.MessageKind:
			walkProtoMessage(v.Message(), visit)
		}
		return true
	})
}

func extractTypeNameOID(tn *zpgparse.TypeName) uint32 {
	if tn == nil || len(tn.Names) == 0 {
		return 0
	}
	isArray := len(tn.ArrayBounds) > 0
	var lastName string
	for _, n := range tn.Names {
		if s := n.GetString_(); s != nil {
			lastName = s.Sval
		}
	}
	if lastName == "" {
		return 0
	}
	return typeNameToOID(lastName, isArray)
}

func rangeVarRef(rv *zpgparse.RangeVar) (tableRef, bool) {
	if rv == nil || rv.Relname == "" {
		return tableRef{}, false
	}
	alias := rv.Relname
	if rv.Alias != nil && rv.Alias.Aliasname != "" {
		alias = rv.Alias.Aliasname
	}
	return tableRef{
		schema: rv.Schemaname,
		table:  rv.Relname,
		alias:  alias,
	}, true
}

func collectTableHints(node *zpgparse.Node, out *paramAnalysis) {
	if node == nil {
		return
	}
	if ins := node.GetInsertStmt(); ins != nil {
		ref, ok := rangeVarRef(ins.Relation)
		if !ok {
			return
		}
		colNames := make([]string, 0, len(ins.Cols))
		for _, c := range ins.Cols {
			if rt := c.GetResTarget(); rt != nil {
				colNames = append(colNames, rt.Name)
			}
		}
		if ins.SelectStmt != nil {
			if sel := ins.SelectStmt.GetSelectStmt(); sel != nil {
				for _, rowList := range sel.ValuesLists {
					lst := rowList.GetList()
					if lst == nil {
						continue
					}
					for idx, item := range lst.Items {
						if pr := item.GetParamRef(); pr != nil && pr.Number > 0 {
							pnum := int(pr.Number)
							if _, has := out.explicitOIDs[pnum]; has {
								continue
							}
							if idx < len(colNames) {
								out.colHints[pnum] = paramColHint{table: ref, colName: colNames[idx]}
							} else if len(colNames) == 0 {
								out.colHints[pnum] = paramColHint{table: ref, colIndex: idx}
							}
						}
					}
				}
			}
		}
		return
	}
	if upd := node.GetUpdateStmt(); upd != nil {
		ref, ok := rangeVarRef(upd.Relation)
		if !ok {
			return
		}
		tables := map[string]tableRef{strings.ToLower(ref.alias): ref}
		for _, f := range upd.FromClause {
			collectFromTables(f, tables)
		}
		for _, tgt := range upd.TargetList {
			rt := tgt.GetResTarget()
			if rt == nil || rt.Val == nil {
				continue
			}
			if pr := rt.Val.GetParamRef(); pr != nil && pr.Number > 0 {
				pnum := int(pr.Number)
				if _, has := out.explicitOIDs[pnum]; !has {
					out.colHints[pnum] = paramColHint{table: ref, colName: rt.Name}
				}
			}
		}
		if upd.WhereClause != nil {
			collectExprColHints(upd.WhereClause, tables, ref, out)
		}
		return
	}
	if del := node.GetDeleteStmt(); del != nil {
		ref, ok := rangeVarRef(del.Relation)
		if !ok {
			return
		}
		tables := map[string]tableRef{strings.ToLower(ref.alias): ref}
		for _, u := range del.UsingClause {
			collectFromTables(u, tables)
		}
		if del.WhereClause != nil {
			collectExprColHints(del.WhereClause, tables, ref, out)
		}
		return
	}
	if sel := node.GetSelectStmt(); sel != nil {
		tables := make(map[string]tableRef)
		var single tableRef
		for _, f := range sel.FromClause {
			collectFromTables(f, tables)
		}
		if len(tables) == 1 {
			for _, ref := range tables {
				single = ref
			}
		}
		if sel.WhereClause != nil && len(tables) > 0 {
			collectExprColHints(sel.WhereClause, tables, single, out)
		}
	}
}

func collectFromTables(n *zpgparse.Node, tables map[string]tableRef) {
	if n == nil {
		return
	}
	if rv := n.GetRangeVar(); rv != nil {
		if ref, ok := rangeVarRef(rv); ok {
			tables[strings.ToLower(ref.alias)] = ref
			tables[strings.ToLower(ref.table)] = ref
		}
		return
	}
	if je := n.GetJoinExpr(); je != nil {
		collectFromTables(je.Larg, tables)
		collectFromTables(je.Rarg, tables)
	}
}

func collectExprColHints(exprNode *zpgparse.Node, tables map[string]tableRef, defaultTable tableRef, out *paramAnalysis) {
	if exprNode == nil {
		return
	}
	walkProtoMessage(exprNode.ProtoReflect(), func(msg protoreflect.Message) {
		ae, ok := msg.Interface().(*zpgparse.A_Expr)
		if !ok || ae.Lexpr == nil || ae.Rexpr == nil {
			return
		}
		// Check col <op> $N or $N <op> col.
		if cr := ae.Lexpr.GetColumnRef(); cr != nil {
			if pr := ae.Rexpr.GetParamRef(); pr != nil && pr.Number > 0 {
				recordColRefHint(int(pr.Number), cr, tables, defaultTable, out)
			}
		} else if cr := ae.Rexpr.GetColumnRef(); cr != nil {
			if pr := ae.Lexpr.GetParamRef(); pr != nil && pr.Number > 0 {
				recordColRefHint(int(pr.Number), cr, tables, defaultTable, out)
			}
		}
	})
}

func recordColRefHint(pnum int, cr *zpgparse.ColumnRef, tables map[string]tableRef, defaultTable tableRef, out *paramAnalysis) {
	if _, has := out.explicitOIDs[pnum]; has {
		return
	}
	if _, has := out.colHints[pnum]; has {
		return
	}
	var parts []string
	for _, f := range cr.Fields {
		if s := f.GetString_(); s != nil {
			parts = append(parts, s.Sval)
		}
	}
	if len(parts) == 1 && defaultTable.table != "" {
		out.colHints[pnum] = paramColHint{table: defaultTable, colName: parts[0]}
	} else if len(parts) >= 2 {
		tblAlias := strings.ToLower(parts[len(parts)-2])
		colName := parts[len(parts)-1]
		if ref, ok := tables[tblAlias]; ok {
			out.colHints[pnum] = paramColHint{table: ref, colName: colName}
		}
	}
}

// buildDescribeProbe constructs a side-effect-free 0-row SELECT probe query
// that discovers exact result column names, OIDs, and modifiers through
// SuperSQL's binder. For SHOW and EXPLAIN it returns static columns directly.
func buildDescribeProbe(raw *zpgparse.RawStmt, rawSQL string) (staticCols []columnDesc, probeSQL string, ok bool) {
	if raw == nil || raw.Stmt == nil {
		return nil, "", false
	}
	n := raw.Stmt
	if show := n.GetVariableShowStmt(); show != nil {
		if strings.EqualFold(show.Name, "all") {
			return []columnDesc{
				{Name: "name", TypeOID: oidText, TypeSize: -1, TypeMod: -1},
				{Name: "setting", TypeOID: oidText, TypeSize: -1, TypeMod: -1},
				{Name: "description", TypeOID: oidText, TypeSize: -1, TypeMod: -1},
			}, "", true
		}
		return []columnDesc{
			{Name: strings.ToLower(show.Name), TypeOID: oidText, TypeSize: -1, TypeMod: -1},
		}, "", true
	}
	if n.GetExplainStmt() != nil {
		return []columnDesc{
			{Name: "QUERY PLAN", TypeOID: oidText, TypeSize: -1, TypeMod: -1},
		}, "", true
	}
	if sel := n.GetSelectStmt(); sel != nil && sel.IntoClause == nil {
		innerSQL := strings.TrimRight(strings.TrimSpace(rawSQL), ";")
		if len(sel.LockingClause) > 0 {
			cloned := proto.Clone(sel).(*zpgparse.SelectStmt)
			cloned.LockingClause = nil
			if deparsed, err := zpgparse.Deparse(&zpgparse.ParseResult{
				Stmts: []*zpgparse.RawStmt{{Stmt: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: cloned}}}},
			}); err == nil && deparsed != "" {
				innerSQL = deparsed
			}
		}
		return nil, "SELECT * FROM (" + innerSQL + "\n) AS _pgwire_desc WHERE false", true
	}
	// DML with RETURNING: synthesize SELECT <returning> FROM <relation> WHERE false.
	var rel *zpgparse.RangeVar
	var returning *zpgparse.ReturningClause
	var withClause *zpgparse.WithClause
	switch {
	case n.GetInsertStmt() != nil:
		s := n.GetInsertStmt()
		rel, returning, withClause = s.Relation, s.ReturningClause, s.WithClause
	case n.GetUpdateStmt() != nil:
		s := n.GetUpdateStmt()
		rel, returning, withClause = s.Relation, s.ReturningClause, s.WithClause
	case n.GetDeleteStmt() != nil:
		s := n.GetDeleteStmt()
		rel, returning, withClause = s.Relation, s.ReturningClause, s.WithClause
	case n.GetMergeStmt() != nil:
		s := n.GetMergeStmt()
		rel, returning, withClause = s.Relation, s.ReturningClause, s.WithClause
	}
	if rel != nil && returning != nil && len(returning.Exprs) > 0 {
		synth := &zpgparse.SelectStmt{
			TargetList: returning.Exprs,
			FromClause: []*zpgparse.Node{{Node: &zpgparse.Node_RangeVar{RangeVar: rel}}},
			WithClause: withClause,
			WhereClause: &zpgparse.Node{
				Node: &zpgparse.Node_AConst{
					AConst: &zpgparse.A_Const{
						Val: &zpgparse.A_Const_Boolval{
							Boolval: &zpgparse.Boolean{Boolval: false},
						},
					},
				},
			},
		}
		deparsed, err := zpgparse.Deparse(&zpgparse.ParseResult{
			Stmts: []*zpgparse.RawStmt{{Stmt: &zpgparse.Node{Node: &zpgparse.Node_SelectStmt{SelectStmt: synth}}}},
		})
		if err == nil && deparsed != "" {
			return nil, deparsed, true
		}
	}
	return nil, "", false
}
