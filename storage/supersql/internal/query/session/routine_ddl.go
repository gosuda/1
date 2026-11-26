package session

import (
	"context"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type RoutineCommandOptions struct {
	TableCommandOptions
	Compiler       bind.RoutineCompiler
	MaxDescriptors uint32
}

func routineObjectCommandOptions(options RoutineCommandOptions) (ObjectCommandOptions, error) {
	if options.MaxDescriptors == 0 {
		return ObjectCommandOptions{}, errSessionTransactionConfig
	}
	return ObjectCommandOptions{
		Binding: options.Binding, Dependencies: options.Dependencies, Columns: options.Columns,
		Indexes: options.Indexes, Limits: options.Limits, PolicyBytes: options.PolicyBytes,
		MaxDescriptors: options.MaxDescriptors,
	}, nil
}

func ExecuteCreateRoutine(ctx context.Context, transactions *Transactions, statement *zpgparse.CreateFunctionStmt, options RoutineCommandOptions) (TableCommandResult, error) {
	if statement == nil {
		return TableCommandResult{}, settingError("42601", "missing CREATE FUNCTION or PROCEDURE statement")
	}
	tag := "CREATE FUNCTION"
	if statement.IsProcedure {
		tag = "CREATE PROCEDURE"
	}
	return executeTableDeclaration(ctx, transactions, options.TableCommandOptions, tag, false, func(ctx context.Context, snapshot bind.CatalogSnapshot, declaration bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		checkBodies := transactions.settings.Semantics().CheckFunctionBodies
		currentSetting := func(name string) (string, error) {
			value, err := transactions.settings.Show(name)
			if err != nil {
				return "", err
			}
			if value.IsNull || value.Type != model.TypeString {
				return "", settingError("22023", "session setting has no textual representation")
			}
			return value.StrVal, nil
		}
		return bind.BindCreateRoutine(ctx, statement, snapshot, bind.CreateRoutineOptions{Declaration: declaration, Compiler: options.Compiler, CheckFunctionBodies: checkBodies, CurrentSetting: currentSetting})
	})
}

func ExecuteCreateTrigger(ctx context.Context, transactions *Transactions, statement *zpgparse.CreateTrigStmt, options RoutineCommandOptions) (TableCommandResult, error) {
	if statement == nil {
		return TableCommandResult{}, settingError("42601", "missing CREATE TRIGGER statement")
	}
	return executeTableDeclaration(ctx, transactions, options.TableCommandOptions, "CREATE TRIGGER", false, func(ctx context.Context, snapshot bind.CatalogSnapshot, declaration bind.CreateDeclarationOptions) (*bind.BoundDeclaration, error) {
		return bind.BindCreateTrigger(ctx, statement, snapshot, declaration)
	})
}
