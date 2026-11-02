package modify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/controlplane/catalog"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
	"github.com/gosuda/supersql-alpha/internal/query/explain"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func (fixture *sessionExecutionFixture) statementOptions(t *testing.T, sql string, parameters ...expr.ParameterValue) session.StatementExecution {
	t.Helper()
	execution := fixture.invocation(t, parameters...)
	spans := []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}
	for _, layout := range fixture.catalog.Layouts() {
		execution.Dependencies = append(execution.Dependencies, txn.ReadDependency{Target: layout.Target, KeyLayout: fixture.catalog.KeyLayout(), Spans: spans})
	}
	request := fixture.store.request(t, 1)
	execution.Dependencies = append(execution.Dependencies, txn.ReadDependency{Target: request.Target, KeyLayout: fixture.store.options.KeyLayout, Spans: spans})
	types := make([]model.TypeLayout, len(parameters))
	for i, parameter := range parameters {
		types[i] = parameter.Layout
	}
	return session.StatementExecution{Execution: execution, Parameters: types, Planning: func(_ context.Context, snapshot *catalog.CatalogSnapshot, reader storage.SnapshotReader) (session.PrepareOptions, error) {
		options := fixture.planning(sql)
		options.Optimize.Access = mutationAccess{reader: reader, target: request.Target, layout: request.Layout, keys: fixture.store.options.KeyLayout}
		return options, nil
	}}
}

func runStatementSQL(t *testing.T, fixture *sessionExecutionFixture, sql string, options session.StatementExecution, consume func(context.Context, *session.Prepared, *session.Rows) error) (session.ExecutionResult, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	return fixture.session.RunStatement(t.Context(), parsed.Stmts[0], options, consume)
}

func TestSessionStatementSeesOwnWritesThroughBindingSnapshot(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO items VALUES(1,10)`
	result, err := runStatementSQL(t, fixture, insert, fixture.statementOptions(t, insert), func(context.Context, *session.Prepared, *session.Rows) error { return nil })
	if err != nil || result.RowsAffected != 1 || result.Commit.Committed {
		t.Fatalf("staged insert=%+v,%v", result, err)
	}
	selectSQL := `SELECT value+$1::bigint FROM items WHERE id=1`
	parameter := expr.ParameterValue{Number: 1, Layout: integer, Value: model.Int64Val(7)}
	invocation := fixture.statementOptions(t, selectSQL, parameter)
	seen := 0
	_, err = runStatementSQL(t, fixture, selectSQL, invocation, func(ctx context.Context, prepared *session.Prepared, rows *session.Rows) error {
		if len(prepared.Columns()) != 1 {
			t.Fatal("missing statement result metadata")
		}
		for {
			var batch columnar.Batch
			err := rows.Next(ctx, &batch)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], 0, 4096)
			if err != nil {
				return err
			}
			if value.IntVal != 17 {
				t.Fatalf("own-write result=%v", value)
			}
			seen += batch.Rows()
		}
	})
	if err != nil || seen != 1 {
		t.Fatalf("same-snapshot SELECT rows=%d error=%v", seen, err)
	}
	if _, err := fixture.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if value, err := fixture.committedValue(t, 1); err != nil || value != 10 {
		t.Fatalf("committed prior write=%d,%v", value, err)
	}
	usage := invocation.Execution.Environment.Query.Usage()
	if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 {
		t.Fatalf("statement resources retained: %+v", usage)
	}
}

func TestExplainAnalyzeExecutesExactlyOnceAndPublishesMeasuredReport(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	limits := explain.Limits{MaxOutputBytes: 1 << 20, MaxNodeBytes: 1 << 20, MaxValueDepth: 64}
	readReport := func(target *map[string]any) func(context.Context, *session.Prepared, *session.Rows) error {
		return func(ctx context.Context, prepared *session.Prepared, rows *session.Rows) error {
			if columns := prepared.Columns(); len(columns) != 1 || columns[0].Name != "QUERY PLAN" {
				t.Fatalf("EXPLAIN columns=%+v", columns)
			}
			var batch columnar.Batch
			if err := rows.Next(ctx, &batch); err != nil {
				return err
			}
			if batch.Rows() != 1 || len(batch.Columns) != 1 {
				t.Fatalf("EXPLAIN report shape=%d/%d", batch.Rows(), len(batch.Columns))
			}
			value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], batch.Lane(0), 1<<20)
			if err != nil {
				return err
			}
			if err := json.Unmarshal([]byte(value.StrVal), target); err != nil {
				return err
			}
			if err := rows.Next(ctx, &batch); err != io.EOF {
				t.Fatalf("EXPLAIN emitted more than one report row: %v", err)
			}
			return nil
		}
	}

	plainSQL := `EXPLAIN (FORMAT JSON) INSERT INTO items VALUES(1,10)`
	plainOptions := fixture.statementOptions(t, plainSQL)
	plainOptions.Execution.Explain = &session.ExplainExecutionOptions{Limits: limits}
	var plain map[string]any
	result, err := runStatementSQL(t, fixture, plainSQL, plainOptions, readReport(&plain))
	if err != nil || result.RowsAffected != 0 || plain["Execution Nanoseconds"] != nil {
		t.Fatalf("plain EXPLAIN result=%+v report=%v error=%v", result, plain, err)
	}
	if _, err := fixture.committedValue(t, 1); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("plain EXPLAIN persisted row: %v", err)
	}

	analyzeSQL := `EXPLAIN (ANALYZE, FORMAT JSON) INSERT INTO items VALUES(1,10)`
	analyzeOptions := fixture.statementOptions(t, analyzeSQL)
	analyzeOptions.Execution.Explain = &session.ExplainExecutionOptions{Limits: limits}
	var analyzed map[string]any
	result, err = runStatementSQL(t, fixture, analyzeSQL, analyzeOptions, readReport(&analyzed))
	if err != nil || result.RowsAffected != 1 || analyzed["Affected Rows"] != float64(1) || analyzed["Execution Nanoseconds"] == nil {
		t.Fatalf("EXPLAIN ANALYZE result=%+v report=%v error=%v", result, analyzed, err)
	}
	nodes, ok := analyzed["Nodes"].([]any)
	if !ok || len(nodes) == 0 {
		t.Fatalf("EXPLAIN ANALYZE nodes=%v", analyzed["Nodes"])
	}
	measured := false
	for _, value := range nodes {
		node, _ := value.(map[string]any)
		if loops, present := node["Actual Loops"]; present && loops.(float64) > 0 {
			measured = true
			break
		}
	}
	if !measured {
		t.Fatalf("EXPLAIN ANALYZE has no executed node: %v", nodes)
	}
	if value, err := fixture.committedValue(t, 1); err != nil || value != 10 {
		t.Fatalf("EXPLAIN ANALYZE write = %d, %v", value, err)
	}
}

func TestSessionStatementBindingErrorFailsBlockWithoutLosingEarlierWrite(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{}); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO items VALUES(1,10)`
	if _, err := runStatementSQL(t, fixture, insert, fixture.statementOptions(t, insert), func(context.Context, *session.Prepared, *session.Rows) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := fixture.session.Savepoint(t.Context(), "keep"); err != nil {
		t.Fatal(err)
	}
	invalid := `SELECT absent_column FROM items`
	_, err := runStatementSQL(t, fixture, invalid, fixture.statementOptions(t, invalid), func(context.Context, *session.Prepared, *session.Rows) error {
		t.Fatal("binding failure reached consumer")
		return nil
	})
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "42703" || !fixture.session.Failed() {
		t.Fatalf("binding error=%v failed=%v", err, fixture.session.Failed())
	}
	if err := fixture.session.RollbackTo(t.Context(), "keep"); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	if value, err := fixture.committedValue(t, 1); err != nil || value != 10 {
		t.Fatalf("earlier write lost=%d,%v", value, err)
	}
}

func TestSessionStatementHistoricalBindingUsesFrozenCatalog(t *testing.T) {
	fixture := newSessionExecutionFixture(t)
	insert := `INSERT INTO items VALUES(1,10)`
	first, err := runStatementSQL(t, fixture, insert, fixture.statementOptions(t, insert), func(context.Context, *session.Prepared, *session.Rows) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	insert = `INSERT INTO items VALUES(2,20)`
	if _, err := runStatementSQL(t, fixture, insert, fixture.statementOptions(t, insert), func(context.Context, *session.Prepared, *session.Rows) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := fixture.session.Begin(t.Context(), session.TransactionStart{Historical: first.Commit.Result.CommitTS}); err != nil {
		t.Fatal(err)
	}
	fixture.store.clock.mu.Lock()
	fixture.store.clock.unavailable = true
	fixture.store.clock.mu.Unlock()
	sql := `SELECT value FROM items`
	seen := 0
	_, err = runStatementSQL(t, fixture, sql, fixture.statementOptions(t, sql), func(ctx context.Context, prepared *session.Prepared, rows *session.Rows) error {
		if prepared.Timestamp() != first.Commit.Result.CommitTS {
			t.Fatal("prepared statement lost historical timestamp")
		}
		for {
			var batch columnar.Batch
			err := rows.Next(ctx, &batch)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], 0, 4096)
			if err != nil {
				return err
			}
			if value.IntVal != 10 {
				t.Fatalf("historical value=%v", value)
			}
			seen += batch.Rows()
		}
	})
	if err != nil || seen != 1 {
		t.Fatalf("historical statement rows=%d error=%v", seen, err)
	}
	if _, err := fixture.session.Commit(t.Context(), false); err != nil {
		t.Fatal(err)
	}
}
