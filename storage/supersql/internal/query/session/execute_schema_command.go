package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (transactions *Transactions) runSchemaCommand(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution) (ExecutionResult, bool, error) {
	if statement == nil || statement.Stmt == nil {
		return ExecutionResult{}, false, nil
	}
	configured := options.Execution.Schema
	alter := statement.Stmt.GetAlterTableStmt()
	drop := statement.Stmt.GetDropStmt()
	index := statement.Stmt.GetIndexStmt()
	reindex := statement.Stmt.GetReindexStmt()
	routine := statement.Stmt.GetCreateFunctionStmt()
	trigger := statement.Stmt.GetCreateTrigStmt()
	lifecycle := bind.IsRoutineLifecycleCommand(statement.Stmt)
	alterOnline := false
	if configured != nil && configured.Online != nil {
		sidecar := *configured.Online
		alterOnline = alter != nil && sidecar.StatementIndex == configured.StatementIndex && sidecar.Location >= 0
	}
	dropConcurrent := drop != nil && drop.Concurrent && drop.RemoveType == zpgparse.ObjectType_OBJECT_INDEX
	createConcurrent := index != nil && index.Concurrent
	reindexConcurrent := bind.ReindexConcurrent(reindex)
	if !alterOnline && !dropConcurrent && !createConcurrent && !reindexConcurrent && routine == nil && trigger == nil && !lifecycle {
		return ExecutionResult{}, false, nil
	}
	if (alterOnline || dropConcurrent || createConcurrent || reindexConcurrent) && (configured == nil || configured.Snapshot == nil) {
		return ExecutionResult{}, true, errOnlineSchemaOptions
	}
	if (routine != nil || trigger != nil || lifecycle) && options.Execution.RoutineDDL == nil {
		return ExecutionResult{}, true, errPreparedInvocation
	}
	if len(options.Parameters) != 0 || len(options.Execution.Environment.Parameters) != 0 {
		return ExecutionResult{}, true, errors.Join(settingError("42P02", "schema command does not accept parameters"), rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	if err := rejectPreparedEnvironment(options.Execution.Environment, nil); err != nil {
		return ExecutionResult{}, true, err
	}
	if lifecycle {
		objectOptions, err := routineObjectCommandOptions(*options.Execution.RoutineDDL)
		if err != nil {
			return ExecutionResult{}, true, err
		}
		result, err := ExecuteObjectCommand(ctx, transactions, statement.Stmt, objectOptions)
		return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
	}
	if routine != nil {
		result, err := ExecuteCreateRoutine(ctx, transactions, routine, *options.Execution.RoutineDDL)
		return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
	}
	if trigger != nil {
		result, err := ExecuteCreateTrigger(ctx, transactions, trigger, *options.Execution.RoutineDDL)
		return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit}, true, err
	}
	if createConcurrent {
		result, err := ExecuteCreateIndexConcurrently(ctx, transactions, index, configured.Snapshot, configured.ConcurrentIndex)
		warnings := make([]model.Error, 0, 1)
		if result.Warning != nil {
			warnings = append(warnings, *result.Warning)
		}
		return ExecutionResult{Tag: result.Tag, Warnings: warnings, Job: result.Job}, true, err
	}
	if reindexConcurrent {
		result, err := ExecuteReindexConcurrently(ctx, transactions, reindex, configured.Snapshot, configured.AlterDrop)
		return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit, Job: result.Job}, true, err
	}
	var result SchemaCommandResult
	var err error
	if alterOnline {
		result, err = ExecuteAlterTableOnline(ctx, transactions, alter, configured.Snapshot, configured.AlterDrop)
	} else {
		result, err = ExecuteDropIndexConcurrently(ctx, transactions, drop, configured.Snapshot, configured.AlterDrop)
	}
	return ExecutionResult{Tag: result.Tag, Warnings: result.Warnings, Commit: result.Commit, Job: result.Job}, true, err
}
