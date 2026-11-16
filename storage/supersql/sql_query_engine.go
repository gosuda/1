package supersql

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	expr "github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

// QueryMetrics reports per-statement execution counters. The native engine
// does not maintain the legacy block-level counters; the fields remain part
// of the QueryResult contract.
type QueryMetrics struct {
	ExecutionTimeMicros int64
	BlocksScanned       int
	BlocksPruned        int
	RowsScanned         int
	RowsMatched         int
}

// QueryResult is the materialized result of one Execute call.
type QueryResult struct {
	Columns       []string
	IntegerBits   []uint8
	HiddenColumns []bool
	Rows          [][]any
	RowsAffected  int
	Metrics       QueryMetrics
	Explain       string
	Message       string
}

// String renders the result for display: the EXPLAIN text when present, the
// command tag when there are no rows, otherwise the materialized row table.
func (r *QueryResult) String() string {
	if r.Explain != "" {
		return r.Explain
	}
	if len(r.Rows) == 0 {
		return r.Message
	}

	var sb strings.Builder
	sb.WriteString(strings.Join(r.Columns, " | "))
	sb.WriteString("\n")
	sb.WriteString(strings.Repeat("-", max(len(r.Columns)*15, 20)))
	sb.WriteString("\n")
	for _, row := range r.Rows {
		var rowStrs []string
		for _, v := range row {
			rowStrs = append(rowStrs, fmt.Sprintf("%v", v))
		}
		sb.WriteString(strings.Join(rowStrs, " | "))
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("(%d rows)\n", len(r.Rows)))
	return sb.String()
}

// SQLQueryEngine is a materializing convenience over the native session
// pipeline. It executes SQL text against one catalog through a dedicated
// session: one engine owns one session scope, so transactions, settings,
// prepared statements and cursors persist across Execute calls exactly like a
// database/sql connection. Rows are converted with the same model.Value mapping
// the driver uses. It is not safe for concurrent use; concurrent callers create
// one engine each or go through OpenConnector.
type SQLQueryEngine struct {
	catalog *Catalog

	mu    sync.Mutex
	scope *sessionScope
}

// NewSQLQueryEngineWithCatalog returns an engine bound to catalog. The catalog
// stays owned by its opener; the engine's session is established lazily on the
// first Execute.
func NewSQLQueryEngineWithCatalog(catalog *Catalog) *SQLQueryEngine {
	return &SQLQueryEngine{catalog: catalog}
}

func (engine *SQLQueryEngine) session(ctx context.Context) (*sessionScope, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.scope != nil {
		return engine.scope, nil
	}
	if engine.catalog == nil {
		return nil, ErrDatabaseClosed
	}
	scope, err := engine.catalog.newSession(ctx)
	if err != nil {
		return nil, err
	}
	engine.scope = scope
	return scope, nil
}

// Execute runs every statement in sqlText through the engine's session and
// returns the last statement's materialized result. Parameters are not part of
// this surface; callers needing $n arguments use OpenConnector.
func (engine *SQLQueryEngine) Execute(ctx context.Context, sqlText string) (*QueryResult, error) {
	return engine.executeParameters(ctx, sqlText, nil)
}

// executeParameters carries typed $n parameters into the session pipeline;
// binding infers undeclared layouts and the session restates them against the
// bound parameter list. Parameterized input is limited to one statement.
func (engine *SQLQueryEngine) executeParameters(ctx context.Context, sqlText string, parameters []expr.ParameterValue) (*QueryResult, error) {
	scope, err := engine.session(ctx)
	if err != nil {
		return nil, err
	}
	database := engine.catalog
	if err := database.checkOpen(); err != nil {
		return nil, err
	}
	parsed, err := scope.txns.Parse(ctx, sqlText, zpgparse.ParseOptions{AllowSystemTime: true, AllowOnlineDDL: true})
	if err != nil {
		return nil, err
	}
	if parsed == nil || parsed.ParseResult == nil || len(parsed.ParseResult.Stmts) == 0 {
		return nil, ErrMissingStatement
	}
	if len(parameters) != 0 && len(parsed.ParseResult.Stmts) != 1 {
		return nil, &model.Error{Code: "42601", Message: "cannot insert multiple commands into a parameterized statement"}
	}
	var outcome Outcome
	for index := range parsed.ParseResult.Stmts {
		if outcome, err = database.execStatement(ctx, scope, parsed, index, parameters, 0, nil, nil); err != nil {
			return nil, err
		}
	}
	return engine.queryResult(outcome, scope)
}

func (engine *SQLQueryEngine) queryResult(outcome Outcome, scope *sessionScope) (*QueryResult, error) {
	columns := make([]bind.ResultColumn, len(outcome.Columns))
	for index, column := range outcome.Columns {
		columns[index] = bind.ResultColumn{Name: column.Name, Layout: column.Layout, Nullable: column.Nullable}
	}
	builtins := scope.builtins()
	if outcome.builtins.Types != nil {
		builtins.Types = outcome.builtins.Types
	}
	rows := &queryRows{columns: columns, builtins: builtins}
	result := &QueryResult{
		Columns:      rows.Columns(),
		RowsAffected: int(outcome.RowsAffected),
		Message:      outcome.Tag,
	}
	if len(outcome.Rows) == 0 {
		return result, nil
	}
	result.Rows = make([][]any, len(outcome.Rows))
	for rowIndex, row := range outcome.Rows {
		values := make([]any, len(row))
		for index, value := range row {
			converted, err := rows.driverValue(index, value)
			if err != nil {
				return nil, err
			}
			values[index] = converted
		}
		result.Rows[rowIndex] = values
	}
	return result, nil
}

// Close releases the engine's session. The catalog stays open.
func (engine *SQLQueryEngine) Close() error {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.scope == nil {
		return nil
	}
	err := engine.scope.close()
	engine.scope = nil
	return err
}
