package exec_test

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	exaggregate "github.com/gosuda/supersql-alpha/internal/query/exec/aggregate"
)

func TestSQLBitwiseAggregateSignedWidths(t *testing.T) {
	for _, width := range []struct {
		name string
		oid  model.TypeID
		min  int64
		max  int64
	}{
		{"int2", 21, -32768, 32767},
		{"int4", 23, -2147483648, 2147483647},
		{"int8", 20, -9223372036854775808, 9223372036854775807},
	} {
		for _, sample := range []struct {
			name  string
			input string
			want  []model.Value
		}{
			{"negative high bits", fmt.Sprintf("(NULL::%s),(%d::%s),(%d::%s)", width.name, width.min+1, width.name, width.min+2, width.name), []model.Value{model.Int64Val(width.min), model.Int64Val(width.min + 3), model.Int64Val(3)}},
			{"opposite signs", fmt.Sprintf("('%d'::%s),('%d'::%s)", width.min, width.name, width.max, width.name), []model.Value{model.Int64Val(0), model.Int64Val(-1), model.Int64Val(-1)}},
			{"all null", fmt.Sprintf("(NULL::%s),(NULL::%s)", width.name, width.name), []model.Value{model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)}},
			{"empty", fmt.Sprintf("(NULL::%s)", width.name), []model.Value{model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64), model.NullVal(model.TypeInt64)}},
		} {
			t.Run(width.name+"/"+sample.name, func(t *testing.T) {
				sql := "SELECT bit_and(x),bit_or(x),bit_xor(x) FROM (VALUES" + sample.input + ") v(x)"
				if sample.name == "empty" {
					sql += " WHERE FALSE"
				}
				graph := sqlGraph(t, sql, nil)
				env := statefulEnvironment(t)
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				assertBitwiseAggregateRows(t, portal, []model.TypeID{width.oid, width.oid, width.oid}, [][]model.Value{sample.want})
				aggregateProjectionReleased(t, env.Query)
			})
		}
	}
}

func TestSQLBitwiseAggregateDistinctFiltersBeforeDeduplication(t *testing.T) {
	graph := sqlGraph(t, `SELECT g,bit_and(x) FILTER(WHERE enabled),bit_or(x) FILTER(WHERE enabled),bit_xor(x) FILTER(WHERE enabled),bit_xor(DISTINCT x) FILTER(WHERE enabled) FROM (VALUES(1,5,false),(1,5,true),(1,5,true),(1,3,true),(1,NULL,true),(1,7,NULL),(2,0,false),(3,0,NULL)) v(g,x,enabled) GROUP BY g ORDER BY g`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	null := model.NullVal(model.TypeInt64)
	want := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(1), model.Int64Val(7), model.Int64Val(3), model.Int64Val(6)},
		{model.Int64Val(2), null, null, null, null},
		{model.Int64Val(3), null, null, null, null},
	}
	assertBitwiseAggregateRows(t, portal, []model.TypeID{23, 23, 23, 23, 23}, want)
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLBitwiseAggregateFilterSuppressesArgumentErrors(t *testing.T) {
	graph := sqlGraph(t, `SELECT bit_and(10/x) FILTER(WHERE x<>0),bit_or(10/x) FILTER(WHERE x<>0),bit_xor(10/x) FILTER(WHERE x<>0),bit_xor(DISTINCT 10/x) FILTER(WHERE x<>0),bit_and(10/x) FILTER(WHERE FALSE),bit_or(10/x) FILTER(WHERE NULL::boolean),bit_xor(10/x) FILTER(WHERE FALSE) FROM (VALUES(0),(2),(2),(NULL),(5)) v(x)`, nil)
	env := statefulEnvironment(t)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	null := model.NullVal(model.TypeInt64)
	assertBitwiseAggregateRows(t, portal, []model.TypeID{23, 23, 23, 23, 23, 23, 23}, [][]model.Value{{model.Int64Val(0), model.Int64Val(7), model.Int64Val(2), model.Int64Val(7), null, null, null}})
	aggregateProjectionReleased(t, env.Query)
}

func TestSQLBitwiseAggregateGroupedSpillPreservesMultiplicity(t *testing.T) {
	inputs := [3][4]string{
		{"NULL", "-32767", "-32766", "-32767"},
		{"NULL", "5", "5", "2"},
		{"NULL", "NULL", "NULL", "NULL"},
	}
	var values []string
	for round := range 4 {
		for group := range 9 {
			values = append(values, fmt.Sprintf("(%d,%s::int2)", group, inputs[group%3][round]))
		}
	}
	graph := sqlGraph(t, "SELECT g,bit_and(x),bit_or(x),bit_xor(x),bit_xor(DISTINCT x) FROM (VALUES"+strings.Join(values, ",")+") v(g,x) GROUP BY g ORDER BY g", nil)
	null := model.NullVal(model.TypeInt64)
	results := [][]model.Value{
		{model.Int64Val(-32768), model.Int64Val(-32765), model.Int64Val(-32766), model.Int64Val(3)},
		{model.Int64Val(0), model.Int64Val(7), model.Int64Val(2), model.Int64Val(7)},
		{null, null, null, null},
	}
	var want [][]model.Value
	for group := range 9 {
		want = append(want, append([]model.Value{model.Int64Val(int64(group))}, results[group%3]...))
	}
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("spill=%t", spill), func(t *testing.T) {
			var env exec.Environment
			if spill {
				env = spillingWindowEnvironment(t)
				env.Aggregate = exaggregate.Options{MaxRecordBytes: 1024, GroupBytes: 64 << 10, WorkBytes: 16 << 20, PartitionMemoryBytes: 128, MaxPartitions: 16}
			} else {
				env = statefulEnvironment(t)
			}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertBitwiseAggregateRows(t, portal, []model.TypeID{23, 21, 21, 21, 21}, want)
			usage := env.Query.Usage()
			if spill && (usage.Spill.Peak == 0 || usage.PeakFiles == 0) {
				t.Fatalf("grouped fixture did not spill: %+v", usage)
			}
			if !spill && usage.Spill.Peak != 0 {
				t.Fatalf("resident fixture spilled: %+v", usage)
			}
			aggregateProjectionReleased(t, env.Query)
			if usage.Spill.Live != 0 || usage.Spill.Reservations != 0 {
				t.Fatalf("aggregate retained spill reservations: %+v", usage.Spill)
			}
		})
	}
}

func TestSQLBitwiseAggregateWindowFrameResets(t *testing.T) {
	null := model.NullVal(model.TypeInt64)
	a, b := model.Int64Val(-9223372036854775807), model.Int64Val(-9223372036854775806)
	for _, sample := range []struct {
		name string
		sql  string
		oids []model.TypeID
		want [][]model.Value
	}{
		{
			"excluded duplicates and partition reset",
			`SELECT p,id,bit_and(x) OVER w,bit_or(x) OVER w,bit_xor(x) OVER w FROM (VALUES(1,1,NULL::int8),(1,2,-9223372036854775807::int8),(1,3,-9223372036854775806::int8),(1,4,-9223372036854775807::int8),(1,5,NULL::int8),(2,1,NULL::int8),(2,2,NULL::int8),(3,1,5::int8)) v(p,id,x) WINDOW w AS (PARTITION BY p ORDER BY id ROWS BETWEEN 1 PRECEDING AND 1 FOLLOWING EXCLUDE CURRENT ROW) ORDER BY p,id`,
			[]model.TypeID{23, 23, 20, 20, 20},
			[][]model.Value{
				{model.Int64Val(1), model.Int64Val(1), a, a, a},
				{model.Int64Val(1), model.Int64Val(2), b, b, b},
				{model.Int64Val(1), model.Int64Val(3), a, a, model.Int64Val(0)},
				{model.Int64Val(1), model.Int64Val(4), b, b, b},
				{model.Int64Val(1), model.Int64Val(5), a, a, a},
				{model.Int64Val(2), model.Int64Val(1), null, null, null},
				{model.Int64Val(2), model.Int64Val(2), null, null, null},
				{model.Int64Val(3), model.Int64Val(1), null, null, null},
			},
		},
		{
			"following frame becomes null then empty",
			`SELECT bit_and(x) OVER w,bit_or(x) OVER w,bit_xor(x) OVER w FROM (VALUES(1,NULL::int4),(2,-2147483647::int4),(3,1073741824::int4),(4,NULL::int4)) v(id,x) WINDOW w AS (ORDER BY id ROWS BETWEEN 1 FOLLOWING AND 1 FOLLOWING) ORDER BY id`,
			[]model.TypeID{23, 23, 23},
			[][]model.Value{
				{model.Int64Val(-2147483647), model.Int64Val(-2147483647), model.Int64Val(-2147483647)},
				{model.Int64Val(1073741824), model.Int64Val(1073741824), model.Int64Val(1073741824)},
				{null, null, null},
				{null, null, null},
			},
		},
		{
			"filter suppresses division in moving frame",
			`SELECT bit_and(10/x) FILTER(WHERE x<>0) OVER w,bit_or(10/x) FILTER(WHERE x<>0) OVER w,bit_xor(10/x) FILTER(WHERE x<>0) OVER w FROM (VALUES(1,0),(2,2),(3,2),(4,NULL),(5,0)) v(id,x) WINDOW w AS (ORDER BY id ROWS BETWEEN 1 PRECEDING AND CURRENT ROW) ORDER BY id`,
			[]model.TypeID{23, 23, 23},
			[][]model.Value{
				{null, null, null},
				{model.Int64Val(5), model.Int64Val(5), model.Int64Val(5)},
				{model.Int64Val(5), model.Int64Val(5), model.Int64Val(0)},
				{model.Int64Val(5), model.Int64Val(5), model.Int64Val(5)},
				{null, null, null},
			},
		},
	} {
		t.Run(sample.name, func(t *testing.T) {
			graph := sqlGraph(t, sample.sql, nil)
			env := spillingWindowEnvironment(t)
			env.Window.PartitionMemoryBytes = 0
			env.Limits.BatchRows = 1
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertBitwiseAggregateRows(t, portal, sample.oids, sample.want)
			usage := env.Query.Usage()
			if usage.Spill.Peak == 0 || usage.PeakFiles == 0 {
				t.Fatalf("window fixture did not spill: %+v", usage)
			}
			aggregateProjectionReleased(t, env.Query)
			if usage.Spill.Live != 0 || usage.Spill.Reservations != 0 {
				t.Fatalf("window retained spill reservations: %+v", usage.Spill)
			}
		})
	}
}

func assertBitwiseAggregateRows(t *testing.T, portal *exec.Portal, oids []model.TypeID, want [][]model.Value) {
	t.Helper()
	var batch columnar.Batch
	row := 0
	for {
		err := portal.Next(t.Context(), &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(batch.Columns) != len(oids) {
			t.Fatalf("columns=%d, want %d", len(batch.Columns), len(oids))
		}
		for column, oid := range oids {
			if typ := batch.Columns[column].Type; typ.ID != oid || typ.Storage != model.TypeInt64 {
				t.Fatalf("column %d type=%+v, want integer OID %d", column, typ, oid)
			}
		}
		for i := range batch.Rows() {
			if row >= len(want) {
				t.Fatalf("unexpected row %d, want %d rows", row, len(want))
			}
			for column := range oids {
				vector, lane := &batch.Columns[column], batch.Lane(i)
				if vector.IsNull(lane) != want[row][column].IsNull {
					t.Fatalf("row %d column %d null=%t, want %t", row, column, vector.IsNull(lane), want[row][column].IsNull)
				}
				got, _, err := columnar.CopyValue(t.Context(), vector, lane, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want[row][column]) {
					t.Fatalf("row %d column %d=%v, want %v", row, column, got, want[row][column])
				}
			}
			row++
		}
	}
	if row != len(want) {
		t.Fatalf("rows=%d, want %d", row, len(want))
	}
	if err := portal.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("repeated EOF=%v", err)
	}
}
