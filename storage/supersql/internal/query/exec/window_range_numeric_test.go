package exec_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func TestSQLNumericOffsetRangeFrames(t *testing.T) {
	wide := strings.Repeat("9", 256)
	for _, scenario := range []struct {
		name, sql string
		sums      []model.Value
	}{
		{
			"exact fractional boundary and scaled peers",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN '0.1'::numeric PRECEDING AND CURRENT ROW) FROM (VALUES('0.1'::numeric,1::int4),('0.20'::numeric,2),('0.2000000000000000000000000000000000000000'::numeric,4),('0.3000000000000000000000000000000000000001'::numeric,8),('0.4'::numeric,16)) t(k,v)`,
			[]model.Value{model.Int64Val(1), model.Int64Val(7), model.Int64Val(7), model.Int64Val(8), model.Int64Val(24)},
		},
		{
			"following exact endpoint leaves empty frames",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN '0.1'::numeric FOLLOWING AND '0.1'::numeric FOLLOWING) FROM (VALUES('0.1'::numeric,1::int4),('0.2'::numeric,2),('0.3000000000000000000000000000000000000001'::numeric,4),('0.4'::numeric,8)) t(k,v)`,
			[]model.Value{model.Int64Val(2), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)},
		},
		{
			"descending exact boundaries with nulls last",
			`SELECT sum(v) OVER (ORDER BY k DESC NULLS LAST RANGE BETWEEN '0.1'::numeric PRECEDING AND '0.1'::numeric FOLLOWING) FROM (VALUES(NULL::numeric,5::int4),(NULL::numeric,6),('0.1'::numeric,10),('0.2'::numeric,20),('0.3000000000000000000000000000000000000001'::numeric,40)) t(k,v)`,
			[]model.Value{model.Int64Val(40), model.Int64Val(30), model.Int64Val(30), model.Int64Val(11), model.Int64Val(11)},
		},
		{
			"zero width includes scaled and null peers",
			`SELECT sum(v) OVER (ORDER BY k NULLS FIRST RANGE BETWEEN '0.00'::numeric PRECEDING AND '0'::numeric FOLLOWING) FROM (VALUES(NULL::numeric,5::int4),(NULL::numeric,6),('0.1'::numeric,10),('0.1000'::numeric,20),('0.1000000000000000000000000000000000000001'::numeric,40)) t(k,v)`,
			[]model.Value{model.Int64Val(11), model.Int64Val(11), model.Int64Val(30), model.Int64Val(30), model.Int64Val(40)},
		},
		{
			"filter masks division across exact frame movement",
			`SELECT sum(10/x) FILTER (WHERE x<>0) OVER (ORDER BY k RANGE BETWEEN '0.1'::numeric PRECEDING AND CURRENT ROW) FROM (VALUES('0.1'::numeric,0::int4),('0.2'::numeric,2),('0.25'::numeric,NULL::int4),('0.3000000000000000000000000000000000000001'::numeric,5)) t(k,x)`,
			[]model.Value{model.NullVal(model.TypeInt64), model.Int64Val(5), model.Int64Val(5), model.Int64Val(2)},
		},
		{
			"exclusion removes differently scaled peers",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN '0.1'::numeric PRECEDING AND '0.1'::numeric FOLLOWING EXCLUDE GROUP) FROM (VALUES('0.1'::numeric,1::int4),('0.2'::numeric,2),('0.2000'::numeric,4),('0.3000000000000000000000000000000000000001'::numeric,8)) t(k,v)`,
			[]model.Value{model.Int64Val(6), model.Int64Val(1), model.Int64Val(1), model.NullVal(model.TypeInt64)},
		},
		{
			"wide coefficient keeps fractional boundary",
			fmt.Sprintf(`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN '0.1'::numeric PRECEDING AND CURRENT ROW) FROM (VALUES('%s.1'::numeric,1::int4),('%s.2'::numeric,2),('%s.3000000000000000000000000000000000000001'::numeric,4)) t(k,v)`, wide, wide, wide),
			[]model.Value{model.Int64Val(1), model.Int64Val(3), model.Int64Val(4)},
		},
		{
			"large positive exponent stays finite",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN '1e1000'::numeric PRECEDING AND CURRENT ROW) FROM (VALUES('1e1000'::numeric,1::int4),('2e1000'::numeric,2),('3.0000000000000000000000000000000000000001e1000'::numeric,4)) t(k,v)`,
			[]model.Value{model.Int64Val(1), model.Int64Val(3), model.Int64Val(4)},
		},
		{
			"large negative exponent does not underflow",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN '1e-1000'::numeric PRECEDING AND CURRENT ROW) FROM (VALUES('1e-1000'::numeric,1::int4),('2e-1000'::numeric,2),('3.0000000000000000000000000000000000000001e-1000'::numeric,4)) t(k,v)`,
			[]model.Value{model.Int64Val(1), model.Int64Val(3), model.Int64Val(4)},
		},
		{
			"finite offset preserves infinity and NaN peers",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN '1'::numeric PRECEDING AND '1'::numeric FOLLOWING) FROM (VALUES('-Infinity'::numeric,1::int4),('-1'::numeric,2),('Infinity'::numeric,4),('NaN'::numeric,8),('NaN'::numeric,16)) t(k,v)`,
			[]model.Value{model.Int64Val(1), model.Int64Val(2), model.Int64Val(4), model.Int64Val(24), model.Int64Val(24)},
		},
		{
			"infinite preceding includes negative infinity but not NaN",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 'Infinity'::numeric PRECEDING AND CURRENT ROW) FROM (VALUES('-Infinity'::numeric,1::int4),('-1'::numeric,2),('Infinity'::numeric,4),('NaN'::numeric,8),('NaN'::numeric,16)) t(k,v)`,
			[]model.Value{model.Int64Val(1), model.Int64Val(3), model.Int64Val(7), model.Int64Val(24), model.Int64Val(24)},
		},
		{
			"infinite following includes positive infinity but not NaN",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN CURRENT ROW AND 'Infinity'::numeric FOLLOWING) FROM (VALUES('-Infinity'::numeric,1::int4),('-1'::numeric,2),('Infinity'::numeric,4),('NaN'::numeric,8),('NaN'::numeric,16)) t(k,v)`,
			[]model.Value{model.Int64Val(7), model.Int64Val(6), model.Int64Val(4), model.Int64Val(24), model.Int64Val(24)},
		},
		{
			"infinite bounds include all non NaNs",
			`SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 'Infinity'::numeric PRECEDING AND 'Infinity'::numeric FOLLOWING) FROM (VALUES('-Infinity'::numeric,1::int4),('-1'::numeric,2),('Infinity'::numeric,4),('NaN'::numeric,8),('NaN'::numeric,16)) t(k,v)`,
			[]model.Value{model.Int64Val(7), model.Int64Val(7), model.Int64Val(7), model.Int64Val(24), model.Int64Val(24)},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := spillingWindowEnvironment(t)
			env.Limits.BatchRows = 1
			env.Window.PartitionMemoryBytes = 0
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var got []model.Value
			for _, row := range rows(t, portal) {
				if len(row) != 1 {
					t.Fatalf("output shape=%v", row)
				}
				got = append(got, row[0])
			}
			if !reflect.DeepEqual(got, scenario.sums) {
				t.Fatalf("numeric RANGE sums=%v want=%v", got, scenario.sums)
			}
			if usage := env.Query.Usage(); usage.PeakFiles == 0 {
				t.Fatalf("numeric RANGE did not spill: %+v", usage)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLNumericRangeRejectsInvalidOffsetBeforeArguments(t *testing.T) {
	for _, literal := range []string{"'-0.0000000000000000000000000000000000000001'::numeric", "'-Infinity'::numeric", "'NaN'::numeric"} {
		for _, frame := range []string{literal + " PRECEDING AND CURRENT ROW", "'0.1'::numeric PRECEDING AND " + literal + " FOLLOWING"} {
			t.Run(frame, func(t *testing.T) {
				graph := sqlGraph(t, `SELECT sum(1/x) OVER (ORDER BY k RANGE BETWEEN `+frame+`) FROM (VALUES('0.1'::numeric,0::int4)) t(k,x)`, nil)
				env := spillingWindowEnvironment(t)
				env.Window.PartitionMemoryBytes = 0
				portal, err := exec.Open(t.Context(), graph, env)
				if portal != nil {
					if closeErr := portal.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				}
				var sqlErr *model.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != "22013" {
					t.Fatalf("invalid numeric RANGE offset=%v want=22013", err)
				}
				aggregateProjectionReleased(t, env.Query)
			})
		}
	}
}

func TestSQLNumericRangeParameterLifetime(t *testing.T) {
	layout := model.TypeLayout{Type: model.Type{ID: 1700, Modifier: -1, Storage: model.TypeNumeric}}
	graph := sqlGraph(t, `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN $1::numeric PRECEDING AND $1::numeric FOLLOWING) FROM (VALUES('0.1'::numeric,1::int4),('0.2'::numeric,2),('0.3000000000000000000000000000000000000001'::numeric,4),('0.4'::numeric,8)) t(k,v)`, []model.TypeLayout{layout})
	for _, stop := range []string{"EOF", "early close", "cancellation"} {
		t.Run(stop, func(t *testing.T) {
			env := spillingWindowEnvironment(t)
			env.Limits.BatchRows = 1
			env.Window.PartitionMemoryBytes = 0
			env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.Value{Type: model.TypeNumeric, StrVal: "0.1"}}}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			env.Parameters[0].Value.StrVal = "1"
			var batch columnar.Batch
			if err := portal.Next(t.Context(), &batch); err != nil {
				t.Fatal(err)
			}
			if batch.Rows() != 1 || len(batch.Columns) != 1 {
				t.Fatalf("first batch shape=%+v", batch)
			}
			first, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(0), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(first, model.Int64Val(3)) {
				t.Fatalf("numeric RANGE parameter snapshot first sum=%v want=3", first)
			}
			if usage := env.Query.Usage(); usage.Files == 0 || usage.Spill.Live == 0 {
				t.Fatalf("numeric RANGE has no active spilled partition: %+v", usage)
			}
			switch stop {
			case "EOF":
				want := [][]model.Value{{model.Int64Val(3)}, {model.Int64Val(12)}, {model.Int64Val(12)}}
				if got := rows(t, portal); !reflect.DeepEqual(got, want) {
					t.Fatalf("numeric RANGE parameter snapshot remaining rows=%v want=%v", got, want)
				}
			case "early close":
				closePortal(t, portal)
			case "cancellation":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled numeric RANGE Next=%v want=%v", err, context.Canceled)
				}
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
