package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestBindExplainOptionsAndNestedStatement(t *testing.T) {
	sql := `EXPLAIN (ANALYZE false, VERBOSE, COSTS off, SETTINGS, TIMING false, SUMMARY false, FORMAT JSON, GENERIC_PLAN) SELECT $1::integer`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bind.BindCommand(t.Context(), parsed.Stmts[0], bindingOptions(sql))
	if err != nil {
		t.Fatal(err)
	}
	command, ok := bound.Command().(bind.ExplainCommand)
	if !ok || command.Query.GetSelectStmt() == nil {
		t.Fatalf("EXPLAIN command=%T query=%v", bound.Command(), command.Query)
	}
	options := command.Options
	if options.Analyze || !options.Verbose || options.Costs || !options.Settings || options.Timing || options.Summary || !options.GenericPlan || options.Format != bind.ExplainJSON || options.Serialize != bind.ExplainNoSerialization {
		t.Fatalf("EXPLAIN options=%+v", options)
	}
}

func TestBindExplainAnalyzeSerializationDefaults(t *testing.T) {
	sql := `EXPLAIN (ANALYZE, SERIALIZE BINARY) SELECT 1`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bind.BindCommand(t.Context(), parsed.Stmts[0], bindingOptions(sql))
	if err != nil {
		t.Fatal(err)
	}
	options := bound.Command().(bind.ExplainCommand).Options
	if !options.Analyze || !options.Timing || !options.Summary || options.Serialize != bind.ExplainSerializeBinary {
		t.Fatalf("EXPLAIN ANALYZE options=%+v", options)
	}
}

func TestBindExplainBuffersOption(t *testing.T) {
	for _, tc := range []struct {
		sql     string
		analyze bool
		buffers bool
		format  bind.ExplainFormat
	}{
		{`EXPLAIN (ANALYZE, BUFFERS) SELECT 1`, true, true, bind.ExplainText},
		{`EXPLAIN (ANALYZE, BUFFERS true, FORMAT JSON) SELECT 1`, true, true, bind.ExplainJSON},
		{`EXPLAIN (BUFFERS, FORMAT JSON) SELECT 1`, false, true, bind.ExplainJSON},
		{`EXPLAIN (BUFFERS) SELECT 1`, false, true, bind.ExplainText},
		{`EXPLAIN (ANALYZE, BUFFERS false) SELECT 1`, true, false, bind.ExplainText},
		{`EXPLAIN (ANALYZE) SELECT 1`, true, false, bind.ExplainText},
	} {
		parsed, err := zpgparse.Parse(tc.sql)
		if err != nil {
			t.Fatalf("%s: %v", tc.sql, err)
		}
		bound, err := bind.BindCommand(t.Context(), parsed.Stmts[0], bindingOptions(tc.sql))
		if err != nil {
			t.Fatalf("%s: unexpected bind error: %v", tc.sql, err)
		}
		options := bound.Command().(bind.ExplainCommand).Options
		if options.Analyze != tc.analyze || options.Buffers != tc.buffers || options.Format != tc.format {
			t.Fatalf("%s: options=%+v, want analyze=%v buffers=%v format=%v", tc.sql, options, tc.analyze, tc.buffers, tc.format)
		}
	}
}

// TestBindExplainRejectsObservabilitySections pins the embedded-profile decision
// for EXPLAIN WAL/MEMORY: they parse, the ANALYZE consistency checks run
// first, and an otherwise valid statement is rejected with 0A000
// server_observability instead of silently dropping the section.
func TestBindExplainRejectsObservabilitySections(t *testing.T) {
	for _, sql := range []string{
		`EXPLAIN (ANALYZE, WAL) SELECT 1`,
		`EXPLAIN (MEMORY) SELECT 1`,
		`EXPLAIN (ANALYZE, MEMORY) SELECT 1`,
		`EXPLAIN (ANALYZE, BUFFERS, WAL, MEMORY) SELECT 1`,
	} {
		parsed, err := zpgparse.Parse(sql)
		if err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		_, err = bind.BindCommand(t.Context(), parsed.Stmts[0], bindingOptions(sql))
		var sqlErr *model.Error
		if !errors.As(err, &sqlErr) {
			t.Fatalf("%s: error = %v, want SQLSTATE 0A000", sql, err)
		}
		if sqlErr.Code != "0A000" || sqlErr.FeatureID != string(bind.ServerObservability) || sqlErr.Message != "feature is outside embedded_pg18: server_observability" {
			t.Fatalf("%s: error = %+v, want 0A000 feature is outside embedded_pg18: server_observability", sql, sqlErr)
		}
	}

	// An explicit false selects nothing, so the statement binds.
	sql := `EXPLAIN (ANALYZE, BUFFERS false, WAL false, MEMORY false) SELECT 1`
	parsed, err := zpgparse.Parse(sql)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bind.BindCommand(t.Context(), parsed.Stmts[0], bindingOptions(sql)); err != nil {
		t.Fatalf("EXPLAIN with false observability sections: %v", err)
	}
}

func TestBindExplainRejectsExecutionOnlyOptionsWithoutAnalyze(t *testing.T) {
	for _, sql := range []string{
		`EXPLAIN (TIMING true) SELECT 1`,
		`EXPLAIN (WAL true) SELECT 1`,
		`EXPLAIN (SERIALIZE TEXT) SELECT 1`,
		`EXPLAIN (ANALYZE, GENERIC_PLAN) SELECT 1`,
	} {
		parsed, err := zpgparse.Parse(sql)
		if err != nil {
			t.Fatal(err)
		}
		_, err = bind.BindCommand(t.Context(), parsed.Stmts[0], bindingOptions(sql))
		requireCode(t, err, "22023")
	}
}
