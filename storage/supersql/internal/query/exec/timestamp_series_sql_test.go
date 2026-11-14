package exec_test

import (
	"context"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLTimestampSeriesClampsEachCalendarStep(t *testing.T) {
	graph := sqlGraph(t, `SELECT t::date::text,n FROM generate_series(make_timestamp(2024,1,31,0,0,0),make_timestamp(2024,3,31,0,0,0),interval '1 month') WITH ORDINALITY AS g(t,n)`, nil)
	env := statefulEnvironment(t)
	env.Limits.BatchRows = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{{model.StringVal("2024-01-31"), model.Int64Val(1)}, {model.StringVal("2024-02-29"), model.Int64Val(2)}, {model.StringVal("2024-03-29"), model.Int64Val(3)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestampSeriesStepErrorsReleaseResources(t *testing.T) {
	for _, step := range []string{"1 month -30 days", "infinity"} {
		graph := sqlGraph(t, `SELECT * FROM generate_series(make_timestamp(2000,1,1,0,0,0),make_timestamp(2000,1,2,0,0,0),interval '`+step+`')`, nil)
		env := statefulEnvironment(t)
		portal, err := exec.Open(t.Context(), graph, env)
		if err != nil {
			t.Fatal(err)
		}
		defer closePortal(t, portal)
		var batch columnar.Batch
		err = portal.Next(t.Context(), &batch)
		var sqlError *model.Error
		if !errors.As(err, &sqlError) || sqlError.Code != "22023" {
			t.Fatalf("step %s: %v", step, err)
		}
		aggregateProjectionReleased(t, env.Query)
	}
}

func TestSQLTimestampSeriesInfiniteEndpointRemainsCancellable(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1114, Storage: model.TypeTimestamp, Modifier: -1}}
	prepared := prepareSemanticSQL(t, `SELECT t FROM generate_series($1,$1,interval '1 day') AS g(t)`, []model.TypeLayout{layout}, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	env := statefulEnvironment(t)
	env.Limits.BatchRows = 1
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: math.MaxInt64}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	portal, err := prepared.Open(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	if err := portal.Next(ctx, &batch); err != nil {
		t.Fatal(err)
	}
	if batch.Length != 1 || batch.Columns[0].Int64[0] != math.MaxInt64 {
		t.Fatalf("infinite endpoint batch: %+v", batch)
	}
	batch.Reset()
	cancel()
	if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled timestamp series: %v", err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestampSeriesLateralNegativeCalendarSteps(t *testing.T) {
	graph := sqlGraph(t, `SELECT t::date::text,n FROM (VALUES(interval '-1 month')) v(step),generate_series(make_timestamp(2024,3,31,0,0,0),make_timestamp(2024,1,1,0,0,0),v.step) WITH ORDINALITY AS g(t,n)`, nil)
	env := applyEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{{model.StringVal("2024-03-31"), model.Int64Val(1)}, {model.StringVal("2024-02-29"), model.Int64Val(2)}, {model.StringVal("2024-01-29"), model.Int64Val(3)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestampSeriesNullSuppressesZeroStep(t *testing.T) {
	graph := sqlGraph(t, `SELECT count(*) FROM generate_series(NULL::timestamp,make_timestamp(2000,1,1,0,0,0),interval '0')`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{20}, [][]model.Value{{model.Int64Val(0)}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestampSeriesFinalStepOverflow(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1114, Storage: model.TypeTimestamp, Modifier: -1}}
	prepared := prepareSemanticSQL(t, `SELECT t FROM generate_series($1,$1,interval '1 microsecond') AS g(t)`, []model.TypeLayout{layout}, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
	env := statefulEnvironment(t)
	env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: 9223371331199999999}}}
	portal, err := prepared.Open(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	err = portal.Next(t.Context(), &batch)
	var sqlError *model.Error
	if !errors.As(err, &sqlError) || sqlError.Code != "22008" || batch.Length != 0 {
		t.Fatalf("final step overflow returned a row: length=%d error=%v", batch.Length, err)
	}
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestampSeriesByteFlushDoesNotSkipValues(t *testing.T) {
	graph := sqlGraph(t, `SELECT t,n FROM generate_series(make_timestamp(2000,1,1,0,0,0),make_timestamp(2000,1,1,0,0,0)+interval '999 days',interval '1 day') WITH ORDINALITY AS g(t,n)`, nil)
	env := statefulEnvironment(t)
	env.Limits.BatchRows, env.Limits.BatchBytes = 1000, 2048
	env.Limits.Expressions.MaxRows = 1000
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	defer batch.Reset()
	var next int64
	for {
		err := portal.Next(t.Context(), &batch)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for row := 0; row < batch.Length; row++ {
			if next >= 1000 || batch.Columns[0].Int64[row] != next*86400000000 || batch.Columns[1].Int64[row] != next+1 {
				t.Fatalf("timestamp or ordinality skipped at row %d", next)
			}
			next++
		}
	}
	if next != 1000 {
		t.Fatalf("series ended after %d rows", next)
	}
	aggregateProjectionReleased(t, env.Query)
}
