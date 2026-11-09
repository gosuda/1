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

func TestSQLTimestamptzSeriesCalendarDaysAndExplicitZone(t *testing.T) {
	for _, tc := range []struct {
		name, step, zone string
		want             []string
	}{
		{"calendar day", "1 day", "", []string{"2024-03-09 17:00:00", "2024-03-10 16:00:00", "2024-03-11 16:00:00"}},
		{"elapsed hours", "24 hours", "", []string{"2024-03-09 17:00:00", "2024-03-10 17:00:00", "2024-03-11 17:00:00"}},
		{"explicit UTC overrides session", "1 day", ",'UTC'", []string{"2024-03-09 17:00:00", "2024-03-10 17:00:00", "2024-03-11 17:00:00"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := builtin.ResolveOptions{Collation: 950, DefaultCollation: 950, TimeZone: builtin.TimeZoneContext{Name: "America/New_York"}}
			prepared := prepareSemanticSQL(t, `SELECT (t AT TIME ZONE 'UTC')::text FROM generate_series(make_timestamp(2024,3,9,12,0,0) AT TIME ZONE 'America/New_York',make_timestamp(2024,3,11,13,0,0) AT TIME ZONE 'America/New_York',interval '`+tc.step+`'`+tc.zone+`) g(t)`, nil, options)
			env := statefulEnvironment(t)
			env.Builtin = options
			env.Limits.BatchRows = 1
			portal, err := prepared.Open(t.Context(), env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			rows := make([][]model.Value, len(tc.want))
			for i, value := range tc.want {
				rows[i] = []model.Value{model.StringVal(value)}
			}
			assertAnyValueRows(t, portal, []model.TypeID{25}, rows)
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLTimestamptzSeriesLateralZoneOrdinality(t *testing.T) {
	graph := sqlGraph(t, `SELECT (t AT TIME ZONE 'UTC')::text,n FROM (VALUES('America/New_York'),('UTC')) v(zone),generate_series(make_timestamp(2024,3,9,17,0,0) AT TIME ZONE 'UTC',make_timestamp(2024,3,10,17,0,0) AT TIME ZONE 'UTC',interval '1 day',v.zone) WITH ORDINALITY g(t,n)`, nil)
	env := applyEnvironment(t)
	env.Limits.BatchRows = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertAnyValueRows(t, portal, []model.TypeID{25, 20}, [][]model.Value{
		{model.StringVal("2024-03-09 17:00:00"), model.Int64Val(1)},
		{model.StringVal("2024-03-10 16:00:00"), model.Int64Val(2)},
		{model.StringVal("2024-03-09 17:00:00"), model.Int64Val(1)},
		{model.StringVal("2024-03-10 17:00:00"), model.Int64Val(2)},
	})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLTimestamptzSeriesStrictNullSuppressesValidation(t *testing.T) {
	for _, args := range []string{
		`NULL::timestamptz,to_timestamp(0),interval '0','not/a/zone'`,
		`to_timestamp(0),to_timestamp(0),interval 'infinity',NULL::text`,
	} {
		graph := sqlGraph(t, `SELECT count(*) FROM generate_series(`+args+`)`, nil)
		env := statefulEnvironment(t)
		portal, err := exec.Open(t.Context(), graph, env)
		if err != nil {
			t.Fatal(err)
		}
		defer closePortal(t, portal)
		assertAnyValueRows(t, portal, []model.TypeID{20}, [][]model.Value{{model.Int64Val(0)}})
		aggregateProjectionReleased(t, env.Query)
	}
}

func TestSQLTimestamptzSeriesStepErrorsReleaseResources(t *testing.T) {
	for _, step := range []string{"0", "1 month -30 days", "infinity"} {
		t.Run(step, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT * FROM generate_series(to_timestamp(1),to_timestamp(0),interval '`+step+`','UTC')`, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			err = portal.Next(t.Context(), &batch)
			var sqlError *model.Error
			if !errors.As(err, &sqlError) || sqlError.Code != "22023" || batch.Length != 0 {
				t.Fatalf("invalid step published %d rows: %v", batch.Length, err)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLTimestamptzSeriesInfiniteEndpointDemandAndCancellation(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1184, Storage: model.TypeTimestamp, Modifier: -1}}
	for _, stop := range []string{"close", "cancel"} {
		t.Run(stop, func(t *testing.T) {
			prepared := prepareSemanticSQL(t, `SELECT t,n FROM generate_series($1,$1,interval '1 day','UTC') WITH ORDINALITY g(t,n)`, []model.TypeLayout{layout}, builtin.ResolveOptions{Collation: 950, DefaultCollation: 950})
			env := statefulEnvironment(t)
			env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeTimestamp, IntVal: math.MaxInt64}}}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			portal, err := prepared.Open(ctx, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			batch := columnar.Batch{MaxRows: 1}
			for n := int64(1); n <= 2; n++ {
				if err := portal.Next(t.Context(), &batch); err != nil {
					t.Fatal(err)
				}
				if batch.Length != 1 || batch.Columns[0].Int64[0] != math.MaxInt64 || batch.Columns[1].Int64[0] != n {
					t.Fatalf("infinite endpoint demand %d: %+v", n, batch)
				}
			}
			batch.Reset()
			if stop == "close" {
				closePortal(t, portal)
			} else {
				cancel()
				if err := portal.Next(t.Context(), &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled series: %v", err)
				}
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLTimestamptzSeriesBytePressureKeepsPendingRow(t *testing.T) {
	graph := sqlGraph(t, `SELECT t,n FROM generate_series(make_timestamp(2000,1,1,0,0,0) AT TIME ZONE 'UTC',(make_timestamp(2000,1,1,0,0,0) AT TIME ZONE 'UTC')+interval '999 days',interval '1 day','UTC') WITH ORDINALITY g(t,n)`, nil)
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
		if batch.Length == 0 || batch.Length >= 1000 {
			t.Fatalf("byte pressure returned %d rows", batch.Length)
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
