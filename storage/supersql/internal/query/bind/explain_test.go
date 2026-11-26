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

// TestBindExplainRejectsObservabilitySections pins the embedded-profile decision
// for EXPLAIN BUFFERS/WAL/MEMORY: they parse, the ANALYZE consistency checks run
// first, and an otherwise valid statement is rejected with 0A000
// server_observability instead of silently dropping the section.
func TestBindExplainRejectsObservabilitySections(t *testing.T) {
	for _, sql := range []string{
		`EXPLAIN (ANALYZE, BUFFERS) SELECT 1`,
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

	// Without ANALYZE the option-consistency check still reports 22023 first.
	parsed, err := zpgparse.Parse(`EXPLAIN (BUFFERS) SELECT 1`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bind.BindCommand(t.Context(), parsed.Stmts[0], bindingOptions(`EXPLAIN (BUFFERS) SELECT 1`)); err != nil {
		requireCode(t, err, "22023")
	} else {
		t.Fatal("EXPLAIN (BUFFERS) without ANALYZE bound")
	}

	// An explicit false selects nothing, so the statement binds.
	sql := `EXPLAIN (ANALYZE, BUFFERS false, WAL false, MEMORY false) SELECT 1`
	parsed, err = zpgparse.Parse(sql)
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
		`EXPLAIN (BUFFERS true) SELECT 1`,
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
