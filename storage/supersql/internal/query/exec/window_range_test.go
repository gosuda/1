package exec_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func TestSQLIntegerOffsetRangeFrames(t *testing.T) {
	for _, scenario := range []struct {
		name, sql string
		sums      []model.Value
	}{
		{"preceding includes boundary peers", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 2 PRECEDING AND CURRENT ROW) FROM (VALUES(1,10),(2,20),(4,40),(4,41),(7,70)) t(k,v)`, []model.Value{model.Int64Val(10), model.Int64Val(30), model.Int64Val(101), model.Int64Val(101), model.Int64Val(70)}},
		{"descending preceding", `SELECT sum(v) OVER (ORDER BY k DESC RANGE BETWEEN 2 PRECEDING AND CURRENT ROW) FROM (VALUES(1,10),(2,20),(4,40),(4,41),(7,70)) t(k,v)`, []model.Value{model.Int64Val(70), model.Int64Val(81), model.Int64Val(81), model.Int64Val(101), model.Int64Val(30)}},
		{"following gaps produce empty frames", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 1 FOLLOWING AND 2 FOLLOWING) FROM (VALUES(1,10),(2,20),(4,40),(4,41),(7,70)) t(k,v)`, []model.Value{model.Int64Val(20), model.Int64Val(81), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)}},
		{"null peers and zero width", `SELECT sum(v) OVER (ORDER BY k NULLS FIRST RANGE BETWEEN 0 PRECEDING AND 0 FOLLOWING) FROM (VALUES(NULL::integer,5),(NULL,6),(1,10),(1,11),(3,30)) t(k,v)`, []model.Value{model.Int64Val(11), model.Int64Val(11), model.Int64Val(21), model.Int64Val(21), model.Int64Val(30)}},
		{"nulls last finite boundaries", `SELECT sum(v) OVER (ORDER BY k NULLS LAST RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM (VALUES(NULL::integer,5),(NULL,6),(1,10),(2,20),(4,40)) t(k,v)`, []model.Value{model.Int64Val(30), model.Int64Val(30), model.Int64Val(40), model.Int64Val(11), model.Int64Val(11)}},
		{"int64 virtual overflow boundaries", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 9223372036854775807::bigint PRECEDING AND 9223372036854775807::bigint FOLLOWING) FROM (VALUES('-9223372036854775808'::bigint,1),(0,2),('9223372036854775807'::bigint,3)) t(k,v)`, []model.Value{model.Int64Val(1), model.Int64Val(5), model.Int64Val(5)}},
		{"computed ordering key", `SELECT sum(v) OVER (ORDER BY k+1 RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM (VALUES(1,10),(2,20),(4,40)) t(k,v)`, []model.Value{model.Int64Val(10), model.Int64Val(30), model.Int64Val(40)}},
		{"exclusion uses offset frame", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE GROUP) FROM (VALUES(1,10),(2,20),(2,21),(3,30)) t(k,v)`, []model.Value{model.Int64Val(41), model.Int64Val(40), model.Int64Val(40), model.Int64Val(41)}},
		{"descending null placement", `SELECT sum(v) OVER (ORDER BY k DESC NULLS LAST RANGE BETWEEN 1 PRECEDING AND 1 FOLLOWING) FROM (VALUES(NULL::integer,5),(NULL,6),(1,10),(2,20),(4,40)) t(k,v)`, []model.Value{model.Int64Val(40), model.Int64Val(30), model.Int64Val(30), model.Int64Val(11), model.Int64Val(11)}},
		{"smallint endpoint is not wrapped", `SELECT sum(v) OVER (ORDER BY k RANGE BETWEEN 1 FOLLOWING AND 1 FOLLOWING) FROM (VALUES(32766::smallint,1),(32767::smallint,2)) t(k,v)`, []model.Value{model.Int64Val(2), model.NullVal(model.TypeInt64)}},
		{"partition resets cached bounds", `SELECT sum(v) OVER (PARTITION BY p ORDER BY k RANGE BETWEEN 1 PRECEDING AND CURRENT ROW) FROM (VALUES(1,1,10),(1,2,20),(2,1,100),(2,3,300)) t(p,k,v) ORDER BY p,k`, []model.Value{model.Int64Val(10), model.Int64Val(30), model.Int64Val(100), model.Int64Val(300)}},
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
			var got []model.Value
			for _, row := range rows(t, portal) {
				if len(row) != 1 {
					t.Fatalf("output shape=%v", row)
				}
				got = append(got, row[0])
			}
			if !reflect.DeepEqual(got, scenario.sums) {
				t.Fatalf("RANGE sums=%v want=%v", got, scenario.sums)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLRangeRejectsNegativeOffsetBeforeArguments(t *testing.T) {
	graph := sqlGraph(t, `SELECT sum(1/x) OVER (ORDER BY k RANGE BETWEEN -1 PRECEDING AND CURRENT ROW) FROM (VALUES(1,0)) t(k,x)`, nil)
	env := spillingWindowEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if portal != nil {
		if closeErr := portal.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != "22013" {
		t.Fatalf("negative RANGE offset=%v want=22013", err)
	}
	aggregateProjectionReleased(t, env.Query)
}
