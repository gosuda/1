package session

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// SetSystemTime selects one historical snapshot for an explicit read-only block
// before its first data access. It does not replace the transaction or marks.
func (transactions *Transactions) SetSystemTime(ctx context.Context, timestamp model.Timestamp) error {
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return err
	}
	defer finish()
	return transactions.setSystemTime(operation, timestamp)
}

func (transactions *Transactions) setSystemTime(ctx context.Context, timestamp model.Timestamp) error {
	if err := transactions.ordinary(); err != nil {
		return err
	}
	if len(transactions.named.temporary) != 0 {
		return transactions.fail(settingError("0A000", "AS OF SYSTEM TIME cannot use a session with temporary objects"))
	}
	if !transactions.settings.TransactionPolicy().ReadOnly {
		return transactions.fail(settingError("25006", "AS OF SYSTEM TIME requires a read-only transaction"))
	}
	if err := transactions.transaction.SetHistoricalSnapshot(ctx, timestamp); err != nil {
		return transactions.fail(err)
	}
	transactions.historical = timestamp
	return nil
}

// ExecuteSystemTimeTransaction consumes the parser sidecar rather than executing
// its empty VariableSetStmt as an ordinary SET TRANSACTION command.
func ExecuteSystemTimeTransaction(ctx context.Context, transactions *Transactions, parsed *zpgparse.ExtendedParseResult, statementIndex int, parameters []expr.ParameterValue, options SystemTimeOptions) (TransactionCommandResult, error) {
	var result TransactionCommandResult
	if parsed == nil || parsed.ParseResult == nil || statementIndex < 0 || statementIndex >= len(parsed.ParseResult.Stmts) {
		return result, settingError("42601", "invalid system-time transaction statement")
	}
	raw := parsed.ParseResult.Stmts[statementIndex]
	if raw == nil || raw.Stmt == nil {
		return result, settingError("42601", "missing system-time transaction statement")
	}
	statement := raw.Stmt.GetVariableSetStmt()
	if statement == nil || statement.Kind != zpgparse.VariableSetKind_VAR_SET_MULTI || statement.Name != "TRANSACTION" || statement.IsLocal || len(statement.Args) != 0 {
		return result, settingError("42601", "system-time transaction sidecar has no matching SET TRANSACTION")
	}
	if transactions == nil {
		return result, settingError("22023", "missing transaction controller")
	}
	operation, finish, err := transactions.enter(ctx)
	if err != nil {
		return result, err
	}
	defer finish()
	if err := transactions.ordinary(); err != nil {
		return result, err
	}
	if !transactions.settings.TransactionPolicy().ReadOnly {
		return result, transactions.fail(settingError("25006", "AS OF SYSTEM TIME requires a read-only transaction"))
	}
	options.Bind.Temporal = transactions.temporal
	selection, err := ResolveSystemTime(operation, parsed, statementIndex, parameters, options)
	if err != nil {
		return result, transactions.fail(err)
	}
	if !selection.Present || !selection.Transaction {
		return result, transactions.fail(settingError("42601", "missing transaction system-time clause"))
	}
	if err := transactions.setSystemTime(operation, selection.Timestamp); err != nil {
		return result, err
	}
	result.Tag = "SET"
	return result, nil
}

// ExecuteTransactionCommand dispatches a transaction command together with its
// extension sidecars. The ordinary parse tree alone is not a complete command.
func ExecuteTransactionCommand(ctx context.Context, transactions *Transactions, parsed *zpgparse.ExtendedParseResult, statementIndex int, parameters []expr.ParameterValue, options SystemTimeOptions) (TransactionCommandResult, error) {
	var result TransactionCommandResult
	if parsed == nil || parsed.ParseResult == nil || statementIndex < 0 || statementIndex >= len(parsed.ParseResult.Stmts) {
		return result, settingError("42601", "invalid transaction command index")
	}
	if options.MaxClauses < 0 || len(parsed.TemporalClauses) > options.MaxClauses || len(parsed.OnlineDDLClauses) > options.MaxClauses-len(parsed.TemporalClauses) {
		return result, settingError("53200", "transaction extension sidecar limit exceeded")
	}
	raw := parsed.ParseResult.Stmts[statementIndex]
	if raw == nil || raw.Stmt == nil {
		return result, settingError("42601", "missing transaction command")
	}
	temporal := false
	for _, clause := range parsed.TemporalClauses {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if clause.StatementIndex < 0 || clause.StatementIndex >= len(parsed.ParseResult.Stmts) {
			return result, settingError("42601", "invalid temporal sidecar index")
		}
		if clause.StatementIndex == statementIndex {
			temporal = true
		}
	}
	for _, clause := range parsed.OnlineDDLClauses {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if clause.StatementIndex < 0 || clause.StatementIndex >= len(parsed.ParseResult.Stmts) {
			return result, settingError("42601", "invalid online sidecar index")
		}
		if clause.StatementIndex == statementIndex {
			return result, settingError("42601", "ONLINE does not apply to a transaction command")
		}
	}
	if temporal {
		return ExecuteSystemTimeTransaction(ctx, transactions, parsed, statementIndex, parameters, options)
	}
	statement := raw.Stmt.GetTransactionStmt()
	if statement == nil {
		return result, settingError("42601", "statement is not a transaction command")
	}
	return ExecuteTransaction(ctx, transactions, statement)
}
