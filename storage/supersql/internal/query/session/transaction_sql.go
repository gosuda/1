package session

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type TransactionCommandResult struct {
	Tag     string
	Warning *model.Error
	Commit  *storage.ApplyResult
}

// ExecuteTransaction shares the bound command dispatcher with RunStatement.
func ExecuteTransaction(ctx context.Context, transactions *Transactions, statement *zpgparse.TransactionStmt) (TransactionCommandResult, error) {
	if transactions == nil {
		return TransactionCommandResult{}, transactionSQLError("22023", "missing transaction controller", -1)
	}
	if statement == nil {
		return TransactionCommandResult{}, transactionSQLError("42601", "missing transaction statement", -1)
	}
	parsed := &zpgparse.RawStmt{Stmt: &zpgparse.Node{Node: &zpgparse.Node_TransactionStmt{TransactionStmt: statement}}}
	execution, err := transactions.RunStatement(ctx, parsed, StatementExecution{}, nil)
	result := TransactionCommandResult{Tag: execution.Tag}
	if statement.Kind == zpgparse.TransactionStmtKind_TRANS_STMT_START && result.Tag == "BEGIN" {
		result.Tag = "START TRANSACTION"
	}
	if len(execution.Warnings) != 0 {
		result.Warning = &execution.Warnings[0]
	}
	if execution.Commit.Committed {
		result.Commit = &execution.Commit.Result
	}
	return result, err
}

func transactionSQLError(code, message string, location int32) *model.Error {
	return &model.Error{Code: code, Message: message, Position: max(0, int(location)+1)}
}
