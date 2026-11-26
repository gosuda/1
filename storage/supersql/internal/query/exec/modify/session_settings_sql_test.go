package modify_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"testing/synctest"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/session"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func settingsSQL(t *testing.T, f *sessionExecutionFixture, sql string) (session.ExecutionResult, [][]model.Value, error) {
	t.Helper()
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	var output [][]model.Value
	result, err := f.session.RunStatement(t.Context(), parsed.Stmts[0], session.StatementExecution{}, func(ctx context.Context, prepared *session.Prepared, rows *session.Rows) error {
		var batch columnar.Batch
		for {
			if err := rows.Next(ctx, &batch); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			values := make([]model.Value, len(batch.Columns))
			for i := range batch.Columns {
				value, _, err := columnar.CopyValue(ctx, &batch.Columns[i], 0, 1<<20)
				if err != nil {
					return err
				}
				values[i] = value
			}
			output = append(output, values)
		}
	})
	return result, output, err
}

func requireSettingsSQL(t *testing.T, f *sessionExecutionFixture, sql string) [][]model.Value {
	t.Helper()
	_, rows, err := settingsSQL(t, f, sql)
	if err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return rows
}

func TestSessionSettingsCommandsNeedNoSnapshotOrQuery(t *testing.T) {
	f := newSessionExecutionFixture(t)
	requireSettingsSQL(t, f, `SET application_name = 'owned'`)
	rows := requireSettingsSQL(t, f, `SHOW application_name`)
	if len(rows) != 1 || rows[0][0].StrVal != "owned" {
		t.Fatalf("SHOW = %v", rows)
	}
	result, _, err := settingsSQL(t, f, `SET LOCAL application_name = 'expired'`)
	if err != nil || len(result.Warnings) != 1 || result.Warnings[0].Code != "25P01" {
		t.Fatalf("SET LOCAL = %+v, %v", result, err)
	}
	rows = requireSettingsSQL(t, f, `SHOW application_name`)
	if rows[0][0].StrVal != "owned" || f.session.Active() {
		t.Fatalf("local escaped idle statement: %v", rows)
	}
	requireSettingsSQL(t, f, `SET search_path = "Mixed,Case", public`)
	path := f.settings.SearchPath()
	if len(path) != 2 || path[0] != "Mixed,Case" || path[1] != "public" {
		t.Fatalf("path = %q", path)
	}
	requireSettingsSQL(t, f, `SET TIME ZONE INTERVAL '-05:30' HOUR TO MINUTE`)
	if zone := f.settings.TimeZone(); !zone.Fixed || zone.OffsetSeconds != -19800 {
		t.Fatalf("zone = %+v", zone)
	}
	requireSettingsSQL(t, f, `SET TIME ZONE 5.5`)
	if f.settings.TimeZone().OffsetSeconds != 19800 {
		t.Fatal("numeric timezone lost fractional hours")
	}
}

func TestSessionInvalidIsolationAbortsAndSavepointRecoversSettings(t *testing.T) {
	f := newSessionExecutionFixture(t)
	requireSettingsSQL(t, f, `BEGIN`)
	requireSettingsSQL(t, f, `SET application_name = 'before'`)
	requireSettingsSQL(t, f, `SAVEPOINT keep`)
	requireSettingsSQL(t, f, `SET LOCAL application_name = 'after'`)
	_, _, err := settingsSQL(t, f, `BEGIN ISOLATION LEVEL READ COMMITTED`)
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "0A000" || !f.session.Failed() {
		t.Fatalf("weak BEGIN = %v", err)
	}
	_, _, err = settingsSQL(t, f, `SHOW application_name`)
	if !errors.As(err, &state) || state.Code != "25P02" {
		t.Fatalf("aborted SHOW = %v", err)
	}
	requireSettingsSQL(t, f, `ROLLBACK TO keep`)
	rows := requireSettingsSQL(t, f, `SHOW application_name`)
	if rows[0][0].StrVal != "before" {
		t.Fatalf("rollback settings = %v", rows)
	}
	requireSettingsSQL(t, f, `COMMIT`)
}

func TestSessionSetConfigRollsBackOnConsumerFailure(t *testing.T) {
	f := newSessionExecutionFixture(t)
	requireSettingsSQL(t, f, `SET application_name = 'before'`)
	plan := f.prepare(t, `SELECT set_config('application_name', 'changed', false), current_setting('application_name')`, nil)
	consumerFailure := errors.New("consumer failed")
	_, err := f.session.RunPrepared(t.Context(), plan, f.invocation(t), func(ctx context.Context, rows *session.Rows) error {
		var batch columnar.Batch
		if err := rows.Next(ctx, &batch); err != nil {
			return err
		}
		value, _, err := columnar.CopyValue(ctx, &batch.Columns[0], 0, 1<<20)
		if err != nil {
			return err
		}
		if value.StrVal != "changed" {
			t.Fatalf("set_config = %v", value)
		}
		return consumerFailure
	})
	if !errors.Is(err, consumerFailure) {
		t.Fatalf("execution = %v", err)
	}
	rows := requireSettingsSQL(t, f, `SHOW application_name`)
	if rows[0][0].StrVal != "before" {
		t.Fatalf("failed invocation retained mutation: %v", rows)
	}
}

func TestSessionStatementTimeoutCancelsConsumer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSessionExecutionFixture(t)
		requireSettingsSQL(t, f, `SET statement_timeout = '1ms'`)
		plan := f.prepare(t, `SELECT 1`, nil)
		_, err := f.session.RunPrepared(t.Context(), plan, f.invocation(t), func(ctx context.Context, rows *session.Rows) error { <-ctx.Done(); return ctx.Err() })
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout = %v", err)
		}
		if f.session.Active() {
			t.Fatal("timeout left autocommit active")
		}
	})
}

func TestSessionWorkMemoryCannotExceedSettingOrHost(t *testing.T) {
	f := newSessionExecutionFixture(t)
	requireSettingsSQL(t, f, `SET work_mem = '1MB'`)
	plan := f.prepare(t, `SELECT 1`, nil)
	invocation := f.invocation(t)
	_, err := f.session.RunPrepared(t.Context(), plan, invocation, func(ctx context.Context, rows *session.Rows) error {
		claim, err := invocation.Environment.Query.TryReserve(runtime.WorkMemory, 1048577)
		if claim != nil {
			claim.Release()
		}
		if !errors.Is(err, runtime.ErrBudget) {
			t.Fatalf("oversized reservation = %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSessionParseUsesSettingsAndFiltersWarnings(t *testing.T) {
	f := newSessionExecutionFixture(t)
	requireSettingsSQL(t, f, `SET standard_conforming_strings = off`)
	parsed, err := f.session.Parse(t.Context(), `SELECT 'a\nb'`, zpgparse.ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	value := parsed.ParseResult.Stmts[0].Stmt.GetSelectStmt().TargetList[0].GetResTarget().Val.GetAConst().GetSval().Sval
	if value != "a\nb" || len(parsed.Diagnostics) != 1 {
		t.Fatalf("literal=%q diagnostics=%v", value, parsed.Diagnostics)
	}
	requireSettingsSQL(t, f, `SET client_min_messages = error`)
	parsed, err = f.session.Parse(t.Context(), `SELECT 'a\nb'`, zpgparse.ParseOptions{})
	if err != nil || len(parsed.Diagnostics) != 0 {
		t.Fatalf("filtered diagnostics=%v, %v", parsed, err)
	}
}

func TestSessionCharacteristicsRejectWeakIsolationWhileIdle(t *testing.T) {
	f := newSessionExecutionFixture(t)
	_, _, err := settingsSQL(t, f, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL REPEATABLE READ`)
	var state *model.Error
	if !errors.As(err, &state) || state.Code != "0A000" || f.session.Active() {
		t.Fatalf("isolation error=%v active=%t", err, f.session.Active())
	}
	requireSettingsSQL(t, f, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL SERIALIZABLE, READ ONLY`)
	requireSettingsSQL(t, f, `BEGIN`)
	rows := requireSettingsSQL(t, f, `SHOW transaction_read_only`)
	if rows[0][0].StrVal != "on" {
		t.Fatalf("transaction mode=%v", rows)
	}
	requireSettingsSQL(t, f, `ROLLBACK`)
}

func TestSessionTransactionTimeoutTerminatesConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newSessionExecutionFixture(t)
		plan := f.prepare(t, `SELECT 1`, nil)
		requireSettingsSQL(t, f, `SET transaction_timeout = '1ms'`)
		requireSettingsSQL(t, f, `BEGIN`)
		_, err := f.session.RunPrepared(t.Context(), plan, f.invocation(t), func(ctx context.Context, rows *session.Rows) error {
			<-ctx.Done()
			return context.Cause(ctx)
		})
		var state *model.Error
		if !errors.As(err, &state) || state.Code != "25P04" {
			t.Fatalf("transaction timeout=%v", err)
		}
		_, _, err = settingsSQL(t, f, `SHOW application_name`)
		if !errors.As(err, &state) || state.Code != "25P04" {
			t.Fatalf("expired session=%v", err)
		}
	})
}

func TestSessionZeroTemporaryFileLimitDoesNotBecomeDefault(t *testing.T) {
	f := newSessionExecutionFixture(t)
	requireSettingsSQL(t, f, `SET temp_file_limit = 0`)
	plan := f.prepare(t, `SELECT 1`, nil)
	invocation := f.invocation(t)
	_, err := f.session.RunPrepared(t.Context(), plan, invocation, func(ctx context.Context, rows *session.Rows) error {
		spill, err := invocation.Environment.Query.NewSpill(ctx)
		if err != nil {
			if !errors.Is(err, runtime.ErrBudget) {
				return err
			}
			return nil
		}
		_, appendErr := spill.Append(ctx, []byte{1})
		closeErr := spill.Close()
		if !errors.Is(appendErr, runtime.ErrBudget) {
			t.Fatalf("zero-byte spill limit admitted write: %v", appendErr)
		}
		return closeErr
	})
	if err != nil {
		t.Fatal(err)
	}
}
