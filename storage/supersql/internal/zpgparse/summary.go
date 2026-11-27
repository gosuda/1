package zpgparse

import (
	"google.golang.org/protobuf/reflect/protoreflect"
	"slices"
	"strings"
)

type summaryRange struct {
	node    *RangeVar
	context SummaryResult_Context
}
type summaryState struct {
	result *SummaryResult
	ranges []summaryRange
	filter bool
}

func Summary(input string, truncateLimit int) (*SummaryResult, error) {
	tree, err := Parse(input)
	if err != nil {
		return nil, err
	}
	s := summaryState{result: &SummaryResult{}}
	for _, stmt := range tree.Stmts {
		s.walk(stmt.ProtoReflect())
	}
	for _, rv := range s.ranges {
		n := rv.node
		name := summaryRelationName(n)
		if !slices.Contains(s.result.CteNames, n.Relname) {
			s.result.Tables = append(s.result.Tables, &SummaryResult_Table{Name: name, SchemaName: n.Schemaname, TableName: n.Relname, Context: rv.context})
		}
		if n.Alias != nil {
			if s.result.Aliases == nil {
				s.result.Aliases = make(map[string]string)
			}
			s.result.Aliases[n.Alias.Aliasname] = name
		}
	}
	var statementWalk func(protoreflect.Message) bool
	statementWalk = func(m protoreflect.Message) bool {
		name := string(m.Descriptor().Name())
		if summaryStatementTypes[name] && !slices.Contains(s.result.StatementTypes, name) {
			s.result.StatementTypes = append(s.result.StatementTypes, name)
		}
		if name == "ExplainStmt" || name == "PrepareStmt" || name == "ViewStmt" || name == "CreateTableAsStmt" {
			utilityChildren(m, []string{"query"}, func(child protoreflect.Message) { utilityWalk(child, statementWalk) })
		}
		return true
	}
	for _, stmt := range tree.Stmts {
		utilityWalk(stmt.Stmt.ProtoReflect(), statementWalk)
	}
	if truncateLimit != -1 {
		s.result.TruncatedQuery, err = summaryTruncate(tree, truncateLimit)
		if err != nil {
			return nil, err
		}
	}
	return s.result, nil
}
func summaryQuote(name string) string {
	scan, err := Scan(name)
	if err == nil && summaryUnquotedToken(scan, name) {
		safe := name != ""
		for i, c := range name {
			if !summaryUnquotedRune(c, i) {
				safe = false
				break
			}
		}
		if safe {
			return name
		}
	}
	return "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
}
func summaryUnquotedToken(scan *ScanResult, name string) bool {
	if len(scan.Tokens) != 1 {
		return false
	}
	token := scan.Tokens[0]
	if int(token.Start) != 0 || int(token.End) != len(name) {
		return false
	}
	return token.KeywordKind == KeywordKind_NO_KEYWORD || token.KeywordKind == KeywordKind_UNRESERVED_KEYWORD
}
func summaryUnquotedRune(c rune, offset int) bool {
	if c == '_' || c >= 'a' && c <= 'z' {
		return true
	}
	if offset == 0 {
		return false
	}
	return c >= '0' && c <= '9' || c == '$'
}
func summaryRelationName(n *RangeVar) string {
	if n.Schemaname != "" {
		return summaryQuote(n.Schemaname) + "." + summaryQuote(n.Relname)
	}
	return summaryQuote(n.Relname)
}
func (s *summaryState) addRange(n *Node, c SummaryResult_Context) {
	if n == nil {
		return
	}
	switch {
	case n.GetRangeVar() != nil:
		s.ranges = append(s.ranges, summaryRange{n.GetRangeVar(), c})
	case n.GetJoinExpr() != nil:
		j := n.GetJoinExpr()
		s.addRange(j.Larg, c)
		s.addRange(j.Rarg, c)
	case n.GetRangeTableSample() != nil:
		s.addRange(n.GetRangeTableSample().Relation, c)
	}
}
func (s *summaryState) addRelation(n *RangeVar, c SummaryResult_Context) {
	if n != nil {
		s.ranges = append(s.ranges, summaryRange{n, c})
	}
}
func (s *summaryState) addRanges(nodes []*Node, c SummaryResult_Context) {
	for _, n := range nodes {
		s.addRange(n, c)
	}
}
func summaryNames(nodes []*Node) []string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if v := n.GetString_(); v != nil {
			parts = append(parts, v.Sval)
		}
	}
	return parts
}
func (s *summaryState) addFunction(nodes []*Node, c SummaryResult_Context) {
	parts := summaryNames(nodes)
	if len(parts) == 0 {
		return
	}
	f := &SummaryResult_Function{Name: strings.Join(parts, "."), FunctionName: parts[len(parts)-1], Context: c}
	if len(parts) == 2 || len(parts) == 3 {
		f.SchemaName = parts[len(parts)-2]
	}
	s.result.Functions = append(s.result.Functions, f)
}
func (s *summaryState) walk(m protoreflect.Message) {
	if !m.IsValid() {
		return
	}
	m = utilityUnwrap(m)
	name := string(m.Descriptor().Name())
	fields := utilityWalkFields[name]
	switch n := m.Interface().(type) {
	case *RawStmt:
		fields = []string{"stmt"}
	case *ExplainStmt, *PrepareStmt:
		fields = []string{"query"}
	case *CallStmt:
		fields = []string{"funccall"}
	case *CommonTableExpr:
		s.result.CteNames = append(s.result.CteNames, n.Ctename)
	case *SelectStmt:
		if s.filter {
			return
		}
		if n.Op == SetOperation_SETOP_NONE {
			s.addRanges(n.FromClause, SummaryResult_Select)
		}
		if n.IntoClause != nil {
			s.addRelation(n.IntoClause.Rel, SummaryResult_DDL)
		}
		if n.WhereClause != nil {
			s.filter = true
			s.walk(n.WhereClause.ProtoReflect())
			s.filter = false
		}
	case *InsertStmt:
		s.addRelation(n.Relation, SummaryResult_DML)
	case *UpdateStmt:
		s.addRelation(n.Relation, SummaryResult_DML)
		s.addRanges(n.FromClause, SummaryResult_Select)
	case *MergeStmt:
		s.addRelation(n.Relation, SummaryResult_DML)
	case *DeleteStmt:
		s.addRelation(n.Relation, SummaryResult_DML)
		s.addRanges(n.UsingClause, SummaryResult_Select)
	case *CopyStmt:
		s.addRelation(n.Relation, SummaryResult_Select)
		if n.Query != nil {
			s.walk(n.Query.ProtoReflect())
		}
	case *AlterTableStmt:
		if n.Objtype != ObjectType_OBJECT_INDEX {
			s.addRelation(n.Relation, SummaryResult_DDL)
		}
	case *CreateStmt:
		s.addRelation(n.Relation, SummaryResult_DDL)
	case *CreateTableAsStmt:
		if n.Into != nil {
			s.addRelation(n.Into.Rel, SummaryResult_DDL)
		}
		if n.Query != nil {
			s.walk(n.Query.ProtoReflect())
		}
	case *TruncateStmt:
		s.addRanges(n.Relations, SummaryResult_DDL)
	case *ViewStmt:
		s.addRelation(n.View, SummaryResult_DDL)
		if n.Query != nil {
			s.walk(n.Query.ProtoReflect())
		}
	case *IndexStmt:
		s.indexStmt(n)
	case *CreateTrigStmt:
		s.addRelation(n.Relation, SummaryResult_DDL)
	case *RuleStmt:
		s.addRelation(n.Relation, SummaryResult_DDL)
	case *RefreshMatViewStmt:
		s.addRelation(n.Relation, SummaryResult_DDL)
	case *VacuumStmt:
		for _, r := range n.Rels {
			if v := r.GetVacuumRelation(); v != nil {
				s.addRelation(v.Relation, SummaryResult_DDL)
			}
		}
	case *LockStmt:
		s.addRanges(n.Relations, SummaryResult_DDL)
	case *CreateFunctionStmt:
		s.addFunction(n.Funcname, SummaryResult_DDL)
	case *FuncCall:
		s.addFunction(n.Funcname, SummaryResult_Call)
	case *GrantStmt:
		s.grantStmt(n)
	case *DropStmt:
		s.dropStmt(n)
	case *RenameStmt:
		s.renameStmt(n)
	case *ColumnRef:
		s.filterColumn(n)
	}
	utilityChildren(m, fields, s.walk)
}
func (s *summaryState) indexStmt(n *IndexStmt) {
	s.addRelation(n.Relation, SummaryResult_DDL)
	for _, p := range n.IndexParams {
		if e := p.GetIndexElem(); e != nil && e.Expr != nil {
			s.walk(e.Expr.ProtoReflect())
		}
	}
	if n.WhereClause != nil {
		s.walk(n.WhereClause.ProtoReflect())
	}
}
func (s *summaryState) grantStmt(n *GrantStmt) {
	switch n.Objtype {
	case ObjectType_OBJECT_TABLE:
		s.addRanges(n.Objects, SummaryResult_DDL)
	case ObjectType_OBJECT_FUNCTION, ObjectType_OBJECT_PROCEDURE, ObjectType_OBJECT_ROUTINE:
		for _, o := range n.Objects {
			if a := o.GetObjectWithArgs(); a != nil {
				s.addFunction(a.Objname, SummaryResult_DDL)
			}
		}
	}
}
func (s *summaryState) dropStmt(n *DropStmt) {
	for i, o := range n.Objects {
		if n.RemoveType == ObjectType_OBJECT_FUNCTION && i == 0 {
			if a := o.GetObjectWithArgs(); a != nil {
				s.addFunction(a.Objname, SummaryResult_DDL)
			}
		}
		switch n.RemoveType {
		case ObjectType_OBJECT_TABLE, ObjectType_OBJECT_RULE, ObjectType_OBJECT_TRIGGER:
			s.dropRelation(o, n.RemoveType)
		}
	}
}
func (s *summaryState) dropRelation(o *Node, kind ObjectType) {
	l := o.GetList()
	if l == nil {
		return
	}
	parts := summaryNames(l.Items)
	if kind != ObjectType_OBJECT_TABLE && len(parts) > 0 {
		parts = parts[:len(parts)-1]
	}
	if len(parts) == 0 {
		return
	}
	rv := &RangeVar{Relname: parts[len(parts)-1]}
	if len(parts) > 1 {
		rv.Schemaname = parts[len(parts)-2]
	}
	s.addRelation(rv, SummaryResult_DDL)
}
func (s *summaryState) renameStmt(n *RenameStmt) {
	if n.RenameType == ObjectType_OBJECT_TABLE && n.Relation != nil {
		s.addRelation(n.Relation, SummaryResult_DDL)
		s.addRelation(&RangeVar{Schemaname: n.Relation.Schemaname, Relname: n.Newname}, SummaryResult_DDL)
		return
	}
	switch n.RenameType {
	case ObjectType_OBJECT_FUNCTION, ObjectType_OBJECT_PROCEDURE, ObjectType_OBJECT_ROUTINE:
		if a := n.Object.GetObjectWithArgs(); a != nil {
			s.addFunction(a.Objname, SummaryResult_DDL)
			s.result.Functions = append(s.result.Functions, &SummaryResult_Function{Name: n.Newname, FunctionName: n.Newname, Context: SummaryResult_DDL})
		}
	}
}
func (s *summaryState) filterColumn(n *ColumnRef) {
	if !s.filter || len(n.Fields) == 0 || n.Fields[len(n.Fields)-1].GetString_() == nil {
		return
	}
	parts := summaryNames(n.Fields)
	fc := &SummaryResult_FilterColumn{Column: parts[len(parts)-1]}
	if len(parts) == 2 {
		fc.TableName = parts[0]
	} else if len(parts) == 3 {
		fc.SchemaName = parts[0]
		fc.TableName = parts[1]
	}
	s.result.FilterColumns = append(s.result.FilterColumns, fc)
}
