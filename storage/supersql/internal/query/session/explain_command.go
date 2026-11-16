package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/locks"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	qexplain "github.com/gosuda/supersql-alpha/internal/query/explain"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

type explainInvocation struct {
	mu              sync.Mutex
	command         bind.ExplainCommand
	report          *Prepared
	limits          qexplain.Limits
	settings        []qexplain.Setting
	consume         func(context.Context, *Prepared, *Rows) error
	instrumentation *exec.Instrumentation
	serializedBytes *uint64
	published       bool
}

func (transactions *Transactions) runExplainStatement(ctx context.Context, statement *zpgparse.RawStmt, options StatementExecution, consume func(context.Context, *Prepared, *Rows) error) (ExecutionResult, error) {
	configuration := options.Execution.Explain
	if configuration == nil || !validExplainLimits(configuration.Limits) || options.Execution.explain != nil || options.Execution.Environment.Instrumentation != nil {
		return ExecutionResult{}, errors.Join(errPreparedInvocation, rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	bound, err := bind.BindCommand(ctx, statement, transactions.commandBindOptions())
	if err != nil {
		return ExecutionResult{}, errors.Join(err, rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	command, ok := bound.Command().(bind.ExplainCommand)
	if !ok || command.Query == nil {
		return ExecutionResult{}, errors.Join(errPreparedInvocation, rejectPreparedEnvironment(options.Execution.Environment, nil))
	}
	invocation := &explainInvocation{command: command, report: &Prepared{bound: bound}, limits: configuration.Limits, consume: consume}
	if command.Options.Settings {
		inventory := transactions.settings.ShowAll()
		invocation.settings = make([]qexplain.Setting, len(inventory))
		for index, setting := range inventory {
			invocation.settings[index] = qexplain.Setting{Name: strings.Clone(setting.Name), Value: strings.Clone(setting.Value.StrVal)}
		}
	}
	nested := options
	nested.Execution.explain = invocation
	if command.Options.Analyze {
		invocation.instrumentation = exec.NewInstrumentation(command.Options.Timing)
		nested.Execution.Environment.Instrumentation = invocation.instrumentation
		ctx = locks.WithLockWaitObserver(ctx, invocation.instrumentation)
	}
	nestedStatement := normalizeSelectInto(&zpgparse.RawStmt{Stmt: command.Query, StmtLocation: statement.StmtLocation, StmtLen: statement.StmtLen})
	if !command.Options.Analyze {
		if create := nestedStatement.Stmt.GetCreateTableAsStmt(); create != nil && create.Query != nil {
			nestedStatement = &zpgparse.RawStmt{Stmt: proto.Clone(create.Query).(*zpgparse.Node), StmtLocation: statement.StmtLocation, StmtLen: statement.StmtLen}
		}
	}
	result, err := transactions.RunStatement(ctx, nestedStatement, nested, nil)
	if err == nil && !invocation.wasPublished() {
		err = invocation.publishEmpty(ctx)
	}
	result.Tag = "EXPLAIN"
	result.Warnings = append(result.Warnings, bound.Warnings()...)
	return result, err
}

func validExplainLimits(limits qexplain.Limits) bool {
	return limits.MaxOutputBytes != 0 && limits.MaxNodeBytes != 0 && limits.MaxValueDepth != 0 && limits.MaxValueDepth <= 64
}

func (invocation *explainInvocation) wasPublished() bool {
	invocation.mu.Lock()
	defer invocation.mu.Unlock()
	return invocation.published
}

func (invocation *explainInvocation) publishEmpty(ctx context.Context) (err error) {
	if invocation == nil {
		return errPreparedInvocation
	}
	invocation.mu.Lock()
	if invocation.published {
		invocation.mu.Unlock()
		return errPreparedInvocation
	}
	invocation.published = true
	invocation.mu.Unlock()
	rows := &Rows{ctx: ctx, command: &commandCursor{columns: invocation.report.Columns()}}
	defer func() {
		rows.mu.Lock()
		rows.closed = true
		rows.command, rows.ctx = nil, nil
		if rows.terminal != nil && rows.terminal != io.EOF {
			err = errors.Join(err, rows.terminal)
		}
		rows.mu.Unlock()
	}()
	if invocation.consume != nil {
		err = invocation.consume(ctx, invocation.report, rows)
	}
	return err
}

func (invocation *explainInvocation) publish(ctx context.Context, prepared *Prepared, affected *uint64) (err error) {
	if invocation == nil || prepared == nil || prepared.graph == nil {
		return errPreparedInvocation
	}
	invocation.mu.Lock()
	if invocation.published {
		invocation.mu.Unlock()
		return errPreparedInvocation
	}
	invocation.published = true
	invocation.mu.Unlock()
	options := qexplain.Options{
		Limits:          invocation.limits,
		Planning:        &qexplain.Planning{Duration: prepared.planningDuration, BudgetExhausted: prepared.budgetExhausted},
		AffectedRows:    affected,
		SettingValues:   invocation.settings,
		Verbose:         invocation.command.Options.Verbose,
		Costs:           invocation.command.Options.Costs,
		Summary:         invocation.command.Options.Summary,
		SerializedBytes: invocation.serializedBytes,
	}
	if invocation.instrumentation != nil {
		observation := invocation.instrumentation.Snapshot()
		options.Analysis = &observation
	}
	switch invocation.command.Options.Serialize {
	case bind.ExplainSerializeText:
		options.Serialization = "text"
	case bind.ExplainSerializeBinary:
		options.Serialization = "binary"
	}
	format := qexplain.FormatText
	switch invocation.command.Options.Format {
	case bind.ExplainText:
		format = qexplain.FormatText
	case bind.ExplainXML:
		format = qexplain.FormatXML
	case bind.ExplainJSON:
		format = qexplain.FormatJSON
	case bind.ExplainYAML:
		format = qexplain.FormatYAML
	default:
		return errPreparedInvocation
	}
	var output strings.Builder
	if err := qexplain.Write(ctx, &output, prepared.graph, format, options); err != nil {
		return err
	}
	cursor := &commandCursor{columns: invocation.report.Columns(), values: [][]model.Value{{model.StringVal(output.String())}}}
	rows := &Rows{ctx: ctx, command: cursor}
	defer func() {
		rows.mu.Lock()
		rows.closed = true
		rows.command, rows.ctx = nil, nil
		if rows.terminal != nil && rows.terminal != io.EOF {
			err = errors.Join(err, rows.terminal)
		}
		rows.mu.Unlock()
	}()
	if invocation.consume != nil {
		err = invocation.consume(ctx, invocation.report, rows)
	}
	return err
}

func runExplainAnalysis(ctx context.Context, prepared *Prepared, portal *exec.Portal, invocation *explainInvocation, serializer *explainSerializer) (affected uint64, err error) {
	if portal == nil || invocation == nil || invocation.instrumentation == nil {
		return 0, errPreparedInvocation
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, portal.Close())
		}
	}()
	var batch columnar.Batch
	for {
		nextErr := portal.Next(ctx, &batch)
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return 0, nextErr
		}
		if err := serializer.consume(ctx, &batch); err != nil {
			return 0, err
		}
	}
	if serializer != nil {
		bytes := serializer.bytes
		invocation.serializedBytes = &bytes
	}
	cleanup := portal.Close()
	closed = true
	affected = portal.RowsAffected()
	if cleanup != nil {
		return 0, cleanup
	}
	var affectedRows *uint64
	if preparedWrites(prepared) {
		affectedRows = &affected
	}
	return affected, invocation.publish(ctx, prepared, affectedRows)
}
