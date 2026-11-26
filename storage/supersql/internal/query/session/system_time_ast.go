package session

import (
	"context"

	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func systemTimeClauses(ctx context.Context, parsed *zpgparse.ExtendedParseResult, statementIndex int, options SystemTimeOptions) ([]zpgparse.TemporalClause, bool, error) {
	if parsed == nil || parsed.ParseResult == nil || statementIndex < 0 || statementIndex >= len(parsed.ParseResult.Stmts) {
		return nil, false, systemTimeError("22023", "invalid system-time statement index", -1)
	}
	statements := parsed.ParseResult.Stmts
	if uint64(len(statements)) > uint64(options.Bind.Limits.MaxOperands) || len(parsed.TemporalClauses) > options.MaxClauses || len(parsed.OnlineDDLClauses) > options.MaxClauses-len(parsed.TemporalClauses) {
		return nil, false, systemTimeError("54000", "system-time statement or sidecar limit exceeded", -1)
	}
	for i, statement := range statements {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if statement == nil || statement.Stmt == nil || statement.StmtLocation < 0 || statement.StmtLen < 0 {
			return nil, false, systemTimeError("22023", "invalid system-time statement bounds", -1)
		}
		if sourceBytes := options.Bind.Limits.SourceBytes; sourceBytes != 0 && uint64(statement.StmtLocation)+uint64(statement.StmtLen) > uint64(sourceBytes) {
			return nil, false, systemTimeError("22023", "system-time statement exceeds the original source", -1)
		}
		if i > 0 && int64(statements[i-1].StmtLocation)+int64(statements[i-1].StmtLen) > int64(statement.StmtLocation) {
			return nil, false, systemTimeError("22023", "overlapping system-time statement bounds", -1)
		}
	}
	var online bool
	for _, clause := range parsed.OnlineDDLClauses {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if !systemTimeLocation(statements, clause.StatementIndex, clause.Location, options.Bind.Limits.SourceBytes) {
			return nil, false, systemTimeError("22023", "invalid ONLINE sidecar bounds", clause.Location)
		}
		online = online || clause.StatementIndex == statementIndex
	}
	count := 0
	var transaction bool
	for _, clause := range parsed.TemporalClauses {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		if !systemTimeLocation(statements, clause.StatementIndex, clause.Location, options.Bind.Limits.SourceBytes) || clause.Expression == nil || clause.Expression.Node == nil || clause.RelationLocation < -1 {
			return nil, false, systemTimeError("22023", "invalid system-time sidecar bounds", clause.Location)
		}
		if clause.RelationLocation >= 0 && (!systemTimeLocation(statements, clause.StatementIndex, clause.RelationLocation, options.Bind.Limits.SourceBytes) || clause.RelationLocation >= clause.Location) {
			return nil, false, systemTimeError("22023", "invalid system-time relation location", clause.Location)
		}
		if clause.StatementIndex != statementIndex {
			continue
		}
		isTransaction := clause.RelationLocation == -1
		if count != 0 && transaction != isTransaction {
			return nil, false, systemTimeError("0A000", "relation and transaction system-time clauses cannot be mixed", clause.Location)
		}
		transaction = isTransaction
		count++
	}
	for _, clause := range parsed.TemporalClauses {
		if clause.RelationLocation == -1 {
			command := statements[clause.StatementIndex].Stmt.GetVariableSetStmt()
			if command == nil || command.Kind != zpgparse.VariableSetKind_VAR_SET_MULTI || command.Name != "TRANSACTION" || len(command.Args) != 0 || command.IsLocal {
				return nil, false, systemTimeError("22023", "transaction system-time sidecar does not identify SET TRANSACTION", clause.Location)
			}
		}
	}
	if online {
		return nil, false, systemTimeError("0A000", "ONLINE cannot be executed by historical preparation", -1)
	}
	if count == 0 {
		return nil, false, nil
	}
	if uint64(count) > options.MaxBytes/128 {
		return nil, false, systemTimeError("54000", "system-time sidecar metadata limit exceeded", -1)
	}
	clauses := make([]zpgparse.TemporalClause, 0, count)
	for _, clause := range parsed.TemporalClauses {
		if clause.StatementIndex == statementIndex {
			clauses = append(clauses, clause)
		}
	}
	return clauses, transaction, nil
}

func systemTimeLocation(statements []*zpgparse.RawStmt, index int, location int32, sourceBytes uint32) bool {
	if index < 0 || index >= len(statements) || location < statements[index].StmtLocation {
		return false
	}
	if sourceBytes != 0 && uint64(location) >= uint64(sourceBytes) {
		return false
	}
	statement := statements[index]
	if statement.StmtLen > 0 && int64(location) >= int64(statement.StmtLocation)+int64(statement.StmtLen) {
		return false
	}
	return index+1 == len(statements) || location < statements[index+1].StmtLocation
}

type systemTimeWalker struct {
	ctx       context.Context
	limits    ir.Limits
	budget    *systemTimeBudget
	remaining uint32
	relations map[int32]bool
}

func (w *systemTimeWalker) walk(message protoreflect.Message, depth uint32, scalar bool) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if !message.IsValid() {
		return systemTimeError("22023", "invalid system-time AST node", -1)
	}
	if depth > w.limits.MaxDepth || w.remaining == 0 {
		return systemTimeError("54000", "system-time AST limit exceeded", -1)
	}
	w.remaining--
	if err := w.budget.charge(32); err != nil {
		return err
	}
	if len(message.GetUnknown()) != 0 {
		return systemTimeError("0A000", "unknown AST extensions cannot be used for system time", -1)
	}
	_, function := message.Interface().(*zpgparse.FuncCall)
	if !function {
		if err := w.classify(message, scalar); err != nil {
			return err
		}
	}
	fields := message.Descriptor().Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		if !message.Has(field) {
			continue
		}
		if field.IsMap() {
			return systemTimeError("0A000", "map-valued AST extensions cannot be used for system time", -1)
		}
		value := message.Get(field)
		if field.Name() == "location" && field.Kind() == protoreflect.Int32Kind && w.limits.SourceBytes != 0 && value.Int() >= int64(w.limits.SourceBytes) {
			return systemTimeError("22023", "system-time AST location exceeds the original source", int32(value.Int()))
		}
		if !field.IsList() {
			if err := w.value(field.Kind(), value, depth, scalar); err != nil {
				return err
			}
			continue
		}
		list := value.List()
		if uint64(list.Len()) > uint64(w.limits.MaxOperands) {
			return systemTimeError("54000", "system-time AST operand limit exceeded", -1)
		}
		for j := range list.Len() {
			if err := w.value(field.Kind(), list.Get(j), depth, scalar); err != nil {
				return err
			}
		}
	}
	if function {
		return w.classify(message, scalar)
	}
	return nil
}

func (w *systemTimeWalker) value(kind protoreflect.Kind, value protoreflect.Value, depth uint32, scalar bool) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	if err := w.budget.charge(32); err != nil {
		return err
	}
	switch kind {
	case protoreflect.MessageKind:
		return w.walk(value.Message(), depth+1, scalar)
	case protoreflect.StringKind:
		return w.budget.charge(uint64(len(value.String())))
	case protoreflect.BytesKind:
		return w.budget.charge(uint64(len(value.Bytes())))
	}
	return nil
}

func (w *systemTimeWalker) classify(message protoreflect.Message, scalar bool) error {
	switch node := message.Interface().(type) {
	case *zpgparse.InsertStmt, *zpgparse.UpdateStmt, *zpgparse.DeleteStmt, *zpgparse.MergeStmt, *zpgparse.IntoClause, *zpgparse.LockingClause:
		return systemTimeError("25006", "historical statements cannot write or lock rows", -1)
	case *zpgparse.RangeVar:
		if scalar {
			return systemTimeError("0A000", "system-time expressions cannot reference relations", node.Location)
		}
		if found, relevant := w.relations[node.Location]; relevant {
			if found {
				return systemTimeError("22023", "system-time relation location is ambiguous", node.Location)
			}
			w.relations[node.Location] = true
		}
	}
	if !scalar {
		return nil
	}
	switch node := message.Interface().(type) {
	case *zpgparse.ColumnRef, *zpgparse.Var, *zpgparse.CurrentOfExpr, *zpgparse.SubLink, *zpgparse.SubPlan, *zpgparse.SelectStmt, *zpgparse.Query, *zpgparse.RangeSubselect:
		return systemTimeError("0A000", "system-time expressions cannot depend on rows or subqueries", -1)
	case *zpgparse.SQLValueFunction, *zpgparse.NextValueExpr:
		return systemTimeError("0A000", "system-time expressions must be immutable", -1)
	case *zpgparse.Aggref, *zpgparse.WindowFunc, *zpgparse.WindowDef:
		return systemTimeError("0A000", "system-time expressions cannot contain aggregates or windows", -1)
	case *zpgparse.FuncCall:
		aggregate := node.AggStar || node.AggDistinct || node.AggWithinGroup
		orderedOrFiltered := len(node.AggOrder) != 0 || node.AggFilter != nil
		if aggregate || orderedOrFiltered || node.Over != nil {
			return systemTimeError("0A000", "system-time expressions cannot contain aggregates or windows", node.Location)
		}
		if len(node.Funcname) < 1 || len(node.Funcname) > 2 {
			return systemTimeError("0A000", "system-time function effects cannot be established without a catalog", node.Location)
		}
		if len(node.Funcname) == 2 && node.Funcname[0].GetString_().GetSval() != "pg_catalog" {
			return systemTimeError("0A000", "system-time function effects cannot be established without a catalog", node.Location)
		}
		name := node.Funcname[len(node.Funcname)-1].GetString_().GetSval()
		for signature := range builtin.SignaturesNamed(name) {
			if signature.Volatility == 'i' && signature.Kind == 'f' && !signature.ReturnsSet {
				return nil
			}
		}
		return systemTimeError("0A000", "system-time function must have an immutable scalar implementation", node.Location)
	}
	return nil
}
