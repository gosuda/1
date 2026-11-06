package exec_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLFloatOffsetRangeFrames(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		sums      []int64
	}{
		{"fractional preceding", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 0.5 PRECEDING AND CURRENT ROW) FROM (VALUES(0.0::float8,1),(0.5,2),(1.5,3),(2.0,4)) t(k,v)`, []int64{1, 3, 3, 7}},
		{"fractional descending", `SELECT sum(v) OVER (ORDER BY k DESC RANGE BETWEEN 0.5 PRECEDING AND CURRENT ROW) FROM (VALUES(0.0::float8,1),(0.5,2),(1.5,3),(2.0,4)) t(k,v)`, []int64{4, 7, 2, 3}},
		{"real key uses double offset", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 0.1::float8 PRECEDING AND CURRENT ROW) FROM (VALUES('0.1'::real,1),('0.2'::real,2),('0.3'::real,3)) t(k,v)`, []int64{1, 2, 3}},
		{"NaN peers follow infinities", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 1.0 PRECEDING AND CURRENT ROW) FROM (VALUES('-Infinity'::float8,1),(0.0,2),('Infinity',3),('NaN',4),('NaN',5)) t(k,v)`, []int64{1, 2, 3, 9, 9}},
		{"infinite offset includes finite and infinite values", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 'Infinity'::float8 PRECEDING AND 'Infinity'::float8 FOLLOWING) FROM (VALUES('-Infinity'::float8,1),(0.0,2),('Infinity',3),('NaN',4),('NaN',5)) t(k,v)`, []int64{6, 6, 6, 9, 9}},
		{"finite arithmetic overflow yields infinite boundary", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN '1.7976931348623157e308'::float8 PRECEDING AND '1.7976931348623157e308'::float8 FOLLOWING) FROM (VALUES('-1.7976931348623157e308'::float8,1),(0.0,2),('1.7976931348623157e308'::float8,3)) t(k,v)`, []int64{3, 6, 5}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env := spillingWindowEnvironment(t)
			env.Window.PartitionMemoryBytes = 0
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			got := rows(t, portal)
			want := make([][]model.Value, len(scenario.sums))
			for i, value := range scenario.sums {
				want[i] = []model.Value{model.Int64Val(value)}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("floating RANGE rows=%v want=%v", got, want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLFloatRangeRejectsInvalidOffsetBeforeArguments(t *testing.T) {
	for _, literal := range []string{"'NaN'::float8", "'-Infinity'::float8"} {
		t.Run(literal, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT sum(1/x) OVER (ORDER BY k RANGE BETWEEN `+literal+` PRECEDING AND CURRENT ROW) FROM (VALUES(1.0::float8,0)) t(k,x)`, nil)
			env := spillingWindowEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if portal != nil {
				if closeErr := portal.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22013" {
				t.Fatalf("invalid floating RANGE offset=%v want=22013", err)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}
