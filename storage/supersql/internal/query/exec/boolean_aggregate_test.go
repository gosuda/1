package exec_test

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func TestSQLBooleanAggregatesIgnoreNulls(t *testing.T) {
	unknown := model.NullVal(model.TypeBoolean)
	for _, sample := range []struct {
		name string
		from string
		want []model.Value
	}{
		{"mixed", "FROM (VALUES(NULL::boolean),(true),(false),(NULL)) t(x)", []model.Value{model.BoolVal(false), model.BoolVal(true), model.BoolVal(false)}},
		{"all true", "FROM (VALUES(NULL::boolean),(true),(true),(NULL)) t(x)", []model.Value{model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}},
		{"all false", "FROM (VALUES(NULL::boolean),(false),(false),(NULL)) t(x)", []model.Value{model.BoolVal(false), model.BoolVal(false), model.BoolVal(false)}},
		{"all null", "FROM (VALUES(NULL::boolean),(NULL)) t(x)", []model.Value{unknown, unknown, unknown}},
		{"empty", "FROM (VALUES(true)) t(x) WHERE false", []model.Value{unknown, unknown, unknown}},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := sqlGraph(t, "SELECT bool_and(x),bool_or(x),every(x) "+sample.from, nil)
			env := statefulEnvironment(t)
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			want := [][]model.Value{sample.want}
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows=%v, want %v", got, want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLBooleanGroupedAggregatesPreserveResultsAcrossSpill(t *testing.T) {
	graph := sqlGraph(t, `SELECT g,bool_and(x),bool_or(x),every(x)
		FROM (VALUES
			(1,NULL::boolean),(2,true),(3,false),(4,true),(5,NULL),(6,true),(7,false),(8,true),
			(1,NULL),(2,NULL),(3,NULL),(4,NULL),(5,NULL),(6,NULL),(7,NULL),(8,NULL),
			(1,NULL),(2,true),(3,false),(4,false),(5,NULL),(6,true),(7,false),(8,false)
		) t(g,x) GROUP BY g ORDER BY g`, nil)
	unknown := model.NullVal(model.TypeBoolean)
	want := [][]model.Value{
		{model.Int64Val(1), unknown, unknown, unknown},
		{model.Int64Val(2), model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)},
		{model.Int64Val(3), model.BoolVal(false), model.BoolVal(false), model.BoolVal(false)},
		{model.Int64Val(4), model.BoolVal(false), model.BoolVal(true), model.BoolVal(false)},
		{model.Int64Val(5), unknown, unknown, unknown},
		{model.Int64Val(6), model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)},
		{model.Int64Val(7), model.BoolVal(false), model.BoolVal(false), model.BoolVal(false)},
		{model.Int64Val(8), model.BoolVal(false), model.BoolVal(true), model.BoolVal(false)},
	}
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("spill=%t", spill), func(t *testing.T) {
			env := spillingWindowEnvironment(t)
			env.Aggregate = exaggregate.Options{MaxRecordBytes: 1024, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 128, MaxPartitions: 16}
			if spill {
				env.Aggregate.GroupBytes = 32 << 10
			}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, want) {
				t.Fatalf("rows=%v, want %v", got, want)
			}
			usage := env.Query.Usage()
			if spilled := usage.Spill.Peak != 0; spilled != spill {
				t.Fatalf("spill=%t, want %t; usage=%+v", spilled, spill, usage)
			}
			if usage.Spill.Live != 0 {
				t.Fatalf("retained spill bytes=%d", usage.Spill.Live)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLBooleanAggregateFilterMasksErroringArguments(t *testing.T) {
	unknown := model.NullVal(model.TypeBoolean)
	for _, sample := range []struct {
		name string
		sql  string
		want [][]model.Value
	}{
		{
			"grouped",
			`SELECT g,bool_and(DISTINCT 10/x>0) FILTER(WHERE x<>0),bool_or(DISTINCT 10/x<0) FILTER(WHERE x<>0),every(DISTINCT 10/x>0) FILTER(WHERE x<>0) FROM (VALUES(1,0),(1,2),(1,NULL),(1,5),(2,0),(2,NULL)) t(g,x) GROUP BY g ORDER BY g`,
			[][]model.Value{{model.Int64Val(1), model.BoolVal(true), model.BoolVal(false), model.BoolVal(true)}, {model.Int64Val(2), unknown, unknown, unknown}},
		},
		{
			"running window",
			`SELECT id,bool_and(10/x>0) FILTER(WHERE x<>0) OVER w,bool_or(10/x<0) FILTER(WHERE x<>0) OVER w,every(10/x>0) FILTER(WHERE x<>0) OVER w FROM (VALUES(1,0),(2,2),(3,NULL),(4,5)) t(id,x) WINDOW w AS (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) ORDER BY id`,
			[][]model.Value{{model.Int64Val(1), unknown, unknown, unknown}, {model.Int64Val(2), model.BoolVal(true), model.BoolVal(false), model.BoolVal(true)}, {model.Int64Val(3), model.BoolVal(true), model.BoolVal(false), model.BoolVal(true)}, {model.Int64Val(4), model.BoolVal(true), model.BoolVal(false), model.BoolVal(true)}},
		},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := sqlGraph(t, sample.sql, nil)
			env := spillingWindowEnvironment(t)
			env.Aggregate = exaggregate.Options{MaxRecordBytes: 1024, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitions: 4}
			env.Window.PartitionMemoryBytes = 0
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, sample.want) {
				t.Fatalf("rows=%v, want %v", got, sample.want)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLBooleanAggregateWindowsResetTruth(t *testing.T) {
	unknown := model.NullVal(model.TypeBoolean)
	for _, sample := range []struct {
		name  string
		frame string
		want  [][]model.Value
	}{
		{
			"running",
			"ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW",
			[][]model.Value{{unknown, unknown, unknown}, {model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}, {model.BoolVal(false), model.BoolVal(true), model.BoolVal(false)}, {model.BoolVal(false), model.BoolVal(true), model.BoolVal(false)}, {model.BoolVal(false), model.BoolVal(true), model.BoolVal(false)}},
		},
		{
			"moving to empty",
			"ROWS BETWEEN 1 FOLLOWING AND 1 FOLLOWING",
			[][]model.Value{{model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}, {model.BoolVal(false), model.BoolVal(false), model.BoolVal(false)}, {model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}, {unknown, unknown, unknown}, {unknown, unknown, unknown}},
		},
		{
			"exclude current row",
			"ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW",
			[][]model.Value{{unknown, unknown, unknown}, {unknown, unknown, unknown}, {model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}, {model.BoolVal(false), model.BoolVal(false), model.BoolVal(false)}, {model.BoolVal(true), model.BoolVal(true), model.BoolVal(true)}},
		},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := sqlGraph(t, `SELECT bool_and(x) OVER w,bool_or(x) OVER w,every(x) OVER w FROM (VALUES(1,NULL::boolean),(2,true),(3,false),(4,true),(5,NULL)) t(id,x) WINDOW w AS (ORDER BY id `+sample.frame+`) ORDER BY id`, nil)
			env := spillingWindowEnvironment(t)
			env.Limits.BatchRows = 1
			env.Window.PartitionMemoryBytes = 0
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			if got := rows(t, portal); !reflect.DeepEqual(got, sample.want) {
				t.Fatalf("rows=%v, want %v", got, sample.want)
			}
			usage := env.Query.Usage()
			if usage.Spill.Peak == 0 || usage.Spill.Live != 0 {
				t.Fatalf("expected released spilled partition, usage=%+v", usage)
			}
			aggregateProjectionReleased(t, env.Query)
		})
	}
}

func TestSQLBooleanAggregatesDoNotSkipErrorsAfterDecisiveTruth(t *testing.T) {
	for _, function := range []string{"bool_and", "bool_or", "every"} {
		for _, window := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/window=%t", function, window), func(t *testing.T) {
				comparison := "<"
				if function == "bool_or" {
					comparison = ">"
				}
				over := ""
				if window {
					over = " OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
				}
				graph := sqlGraph(t, "SELECT "+function+"(10/x"+comparison+"0)"+over+" FROM (VALUES(1,2),(2,0)) t(id,x)", nil)
				env := spillingWindowEnvironment(t)
				env.Aggregate = exaggregate.Options{MaxRecordBytes: 1024, GroupBytes: 1 << 20, WorkBytes: 16 << 20, PartitionMemoryBytes: 64 << 10, MaxPartitions: 4}
				env.Limits.BatchRows = 1
				env.Window.PartitionMemoryBytes = 0
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				var batch columnar.Batch
				err = portal.Next(t.Context(), &batch)
				var sqlErr *model.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
					t.Fatalf("Next error=%v, want division by zero after decisive truth", err)
				}
				aggregateProjectionReleased(t, env.Query)
			})
		}
	}
}
