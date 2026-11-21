package session

import (
	"context"
	"errors"

	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/procedural"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type inlineRoutineCompiler interface {
	CompileInline(context.Context, *zpgparse.DoStmt, bind.CatalogSnapshot, bind.Options) (*procedural.Program, []model.Dependency, error)
}

func runInlineRoutineStatement(ctx context.Context, owner *Transactions, transaction *txn.Transaction, command bind.InlineRoutineCommand, options PreparedExecution, environment *exec.Environment, nonAtomic bool) (affected uint64, err error) {
	if owner == nil || transaction == nil || command.Statement == nil || options.Catalog == nil || options.Routines == nil || options.RoutineDDL == nil || environment == nil {
		return 0, errPreparedInvocation
	}
	compiler, ok := options.RoutineDDL.Compiler.(inlineRoutineCompiler)
	if !ok {
		return 0, errPreparedInvocation
	}
	// The DO body is compiled and bound against this view, and binding resolves
	// catalog objects by name. Declare the discovery read set as well, so those
	// bind-time scans are covered instead of failing as undeclared reads. The
	// caller's dependencies describe the outer statement, not the routine body.
	discovery, err := catalogDiscoveryDependencies(ctx, transaction, options.Catalog, catalogDiscovery{}, options.Dependencies, options.Environment.Limits.MetadataBytes)
	if err != nil {
		return 0, err
	}
	view, err := transaction.ReadView(ctx, discovery)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, view.Close()) }()
	snapshot, err := options.Catalog.TransactionSnapshot(view, catalog.BorrowReader)
	if err != nil {
		return 0, err
	}
	binding := owner.commandBindOptions()
	binding.SearchPath = owner.settings.SearchPath()
	binding.Builtin = environment.Builtin
	binding.Temporal = owner.temporal
	program, dependencies, compileErr := compiler.CompileInline(ctx, command.Statement, snapshot, binding)
	err = errors.Join(compileErr, snapshot.Close())
	if err != nil {
		return 0, err
	}
	scope := &preparedMutationScope{transaction: transaction, ambient: !nonAtomic}
	scope.active.Store(true)
	defer scope.active.Store(false)
	ctx = context.WithValue(ctx, preparedScopeKey{}, scope)
	environment.Reader = view
	environment.Settings = owner.settingRuntime(ctx)
	environment.Modify.Transaction = scope
	environment.Modify.Deferral = owner
	environment.Catalog, err = owner.attachCatalogSettings(ctx, environment.Catalog)
	if err != nil {
		return 0, err
	}
	environment.Catalog, err = attachCatalogView(environment.Catalog, view)
	if err != nil {
		return 0, err
	}
	ctx, closeCapabilities, attachErr := attachRoutineExecution(ctx, owner, transaction, view, scope, environment, options.Environment.Catalog, options.Catalog, options, options.Routines, dependencies, nonAtomic)
	if attachErr != nil {
		var detail *model.Error
		if errors.As(attachErr, &detail) {
		} else {
		}
		return 0, attachErr
	}
	defer func() { err = errors.Join(err, closeCapabilities()) }()
	_, err = program.Execute(ctx, nil)
	return 0, err
}
