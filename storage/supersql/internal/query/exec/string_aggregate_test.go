package exec_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
)

func assertStringAggregateBatch(t *testing.T, batch *columnar.Batch, want [][]model.Value) {
	t.Helper()
	if batch.Rows() != len(want) {
		t.Fatalf("batch rows=%d, want %d", batch.Rows(), len(want))
	}
	for row, expected := range want {
		if len(batch.Columns) != len(expected) {
			t.Fatalf("row %d columns=%d, want %d", row, len(batch.Columns), len(expected))
		}
		for column, value := range expected {
			vector := &batch.Columns[column]
			switch value.Type {
			case model.TypeString:
				if vector.Type.ID != 25 || vector.Type.Storage != model.TypeString {
					t.Errorf("column %d type=%+v, want text OID 25", column, vector.Type)
				}
			case model.TypeBytes:
				if vector.Type.ID != 17 || vector.Type.Storage != model.TypeBytes {
					t.Errorf("column %d type=%+v, want bytea OID 17", column, vector.Type)
				}
			}
			got, _, err := columnar.CopyValue(t.Context(), vector, batch.Lane(row), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			equal := reflect.DeepEqual(got, value)
			if value.Type == model.TypeBytes {
				equal = got.Type == value.Type && got.IsNull == value.IsNull && bytes.Equal(got.BytesVal, value.BytesVal)
			}
			if !equal {
				t.Errorf("row %d column %d=%#v, want %#v", row, column, got, value)
			}
		}
	}
}

func assertStringAggregateRows(t *testing.T, portal *exec.Portal, want [][]model.Value) {
	t.Helper()
	var batch columnar.Batch
	seen := 0
	for {
		err := portal.Next(t.Context(), &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if seen+batch.Rows() > len(want) {
			t.Fatalf("received more than %d expected rows", len(want))
		}
		assertStringAggregateBatch(t, &batch, want[seen:seen+batch.Rows()])
		seen += batch.Rows()
	}
	if seen != len(want) {
		t.Fatalf("rows=%d, want %d", seen, len(want))
	}
}

func TestSQLStringAggregateNullEmptyAndOrdering(t *testing.T) {
	s, b := model.StringVal, model.BytesVal
	textNull, bytesNull := model.NullVal(model.TypeString), model.NullVal(model.TypeBytes)
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{
			"varying delimiters and opposite orders",
			`SELECT string_agg(x,d ORDER BY id),string_agg(y,e ORDER BY id),
				string_agg(x,d ORDER BY id DESC),string_agg(y,e ORDER BY id DESC)
			FROM (VALUES
				(1,NULL::text,'discard',NULL::bytea,'\xff'::bytea),
				(2,'','^','\x','\xff'),(3,'Ω',':','\x00ff','\x80'),
				(4,'β',NULL,'\xfe',NULL),(5,'','!','\x','\x00'),
				(6,'終','','\x0100','\x')) v(id,x,d,y,e)`,
			[][]model.Value{{s(":Ωβ!終"), b([]byte{0x80, 0, 0xff, 0xfe, 0, 1, 0}), s("終!β:Ω^"), b([]byte{1, 0, 0, 0xfe, 0x80, 0, 0xff, 0xff})}},
		},
		{
			"all null values",
			`SELECT string_agg(x,d),string_agg(y,e) FROM (VALUES(NULL::text,'!',NULL::bytea,'\xff'::bytea),(NULL,NULL,NULL,NULL)) v(x,d,y,e)`,
			[][]model.Value{{textNull, bytesNull}},
		},
		{
			"no input rows",
			`SELECT string_agg(''::text,NULL::text),string_agg('\x'::bytea,NULL::bytea) FROM (VALUES(1)) v(x) WHERE false`,
			[][]model.Value{{textNull, bytesNull}},
		},
		{
			"accepted empty value with null delimiter",
			`SELECT string_agg(''::text,NULL::text),string_agg('\x'::bytea,NULL::bytea)`,
			[][]model.Value{{s(""), b(nil)}},
		},
		{
			"distinct keeps full tuples including null delimiters",
			`SELECT string_agg(DISTINCT x,d ORDER BY x,d NULLS FIRST),
				string_agg(DISTINCT y,e ORDER BY y,e NULLS FIRST)
			FROM (VALUES('a'::text,NULL::text,'\x01'::bytea,NULL::bytea),
				('a',NULL,'\x01',NULL),('a','','\x01','\x'),
				('a',',','\x01','\xff'),('a',',','\x01','\xff'),
				('b',NULL,'\x02',NULL),('b',';','\x02','\x80'),
				(NULL,'!',NULL,'\x00')) v(x,d,y,e)`,
			[][]model.Value{{s("aa,ab;b"), b([]byte{1, 1, 0xff, 1, 2, 0x80, 2})}},
		},
		{
			"unordered distinct distinguishes null and empty delimiters",
			`SELECT string_agg(DISTINCT x,d),string_agg(DISTINCT y,e)
			FROM (VALUES('a'::text,NULL::text,'\x01'::bytea,NULL::bytea),
				('a',NULL,'\x01',NULL),('a','','\x01','\x'),
				('a','','\x01','\x'),(NULL,'!',NULL,'\xff')) v(x,d,y,e)`,
			[][]model.Value{{s("aa"), b([]byte{1, 1})}},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 1024
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertStringAggregateRows(t, portal, scenario.want)
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLStringAggregateGroupPartitions(t *testing.T) {
	var values []string
	var want [][]model.Value
	for group := range 16 {
		values = append(values, fmt.Sprintf(`(%[1]d,3,'b'::text,'/'::text,'\x02'::bytea,'\xff'::bytea),
			(%[1]d,1,'','ignored','\x','\xee'),(%[1]d,2,'a','|','\x01','\x80'),
			(%[1]d,2,'a','|','\x01','\x80'),(%[1]d,4,NULL,'!',NULL,'\x00')`, group))
		want = append(want, []model.Value{model.Int64Val(int64(group)), model.StringVal("|a|a/b"), model.BytesVal([]byte{0x80, 1, 0x80, 1, 0xff, 2}), model.StringVal("|a/b"), model.BytesVal([]byte{0x80, 1, 0xff, 2})})
	}
	graph := sqlGraph(t, `SELECT g,string_agg(x,d ORDER BY k),string_agg(y,e ORDER BY k),
		string_agg(DISTINCT x,d ORDER BY x,d),string_agg(DISTINCT y,e ORDER BY y,e)
		FROM (VALUES`+strings.Join(values, ",")+`) v(g,k,x,d,y,e) GROUP BY g ORDER BY g`, nil)
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("spill=%t", spill), func(t *testing.T) {
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 512 << 10
			if spill {
				env.Aggregate.GroupBytes = 32 << 10
				env.Aggregate.PartitionMemoryBytes = 1024
			}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertStringAggregateRows(t, portal, want)
			if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
				t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLStringAggregateFilterMasksBothArguments(t *testing.T) {
	for _, window := range []bool{false, true} {
		t.Run(fmt.Sprintf("window=%t", window), func(t *testing.T) {
			over := ""
			if window {
				over = " OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
			}
			graph := sqlGraph(t, `SELECT string_agg((10/x)::text,(100/x)::text) FILTER(WHERE x<>0)`+over+`,
				string_agg((10/x)::text::bytea,(100/x)::text::bytea) FILTER(WHERE x<>0)`+over+`,
				string_agg((10/x)::text,(100/x)::text) FILTER(WHERE false)`+over+`,
				string_agg((10/x)::text::bytea,(100/x)::text::bytea) FILTER(WHERE NULL::boolean)`+over+`
				FROM (VALUES(1,0),(2,2),(3,NULL),(4,5)) v(id,x)`, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 1024
			env.Window.PartitionMemoryBytes = 0
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			want := [][]model.Value{{model.StringVal("5202"), model.BytesVal([]byte("5202")), model.NullVal(model.TypeString), model.NullVal(model.TypeBytes)}}
			if window {
				want = append(want, want[0], want[0], want[0])
			}
			assertStringAggregateRows(t, portal, want)
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLStringAggregateNullValueDoesNotMaskDelimiterError(t *testing.T) {
	for _, typ := range []string{"text", "bytea"} {
		for _, window := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/window=%t", typ, window), func(t *testing.T) {
				over := ""
				if window {
					over = " OVER (ORDER BY x ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
				}
				graph := sqlGraph(t, "SELECT string_agg(NULL::"+typ+",(10/x)::text::"+typ+")"+over+" FROM (VALUES(0)) v(x)", nil)
				env, dir := groupingSetsEnvironment(t, false)
				env.Aggregate.PartitionMemoryBytes = 1024
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
					t.Fatalf("Next error=%v, want SQLSTATE 22012", err)
				}
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}

func TestSQLStringAggregateGroupingSetsReplay(t *testing.T) {
	graph := sqlGraph(t, `SELECT g,string_agg(x,d ORDER BY g,k),string_agg(y,e ORDER BY g,k)
		FROM (VALUES(1,1,''::text,'ignored'::text,'\x'::bytea,'\xff'::bytea),
			(1,2,'a','|','\x01','\x80'),(2,1,NULL,'!',NULL,'\xff'),
			(2,2,'b',NULL,'\x02',NULL)) v(g,k,x,d,y,e)
		GROUP BY GROUPING SETS ((g),(),(g),()) ORDER BY g NULLS LAST`, nil)
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay_spill=%t", spill), func(t *testing.T) {
			env, dir := groupingSetsEnvironment(t, spill)
			env.Aggregate.PartitionMemoryBytes = 1024
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			i, s, b := model.Int64Val, model.StringVal, model.BytesVal
			null := model.NullVal(model.TypeInt64)
			assertStringAggregateRows(t, portal, [][]model.Value{
				{i(1), s("|a"), b([]byte{0x80, 1})}, {i(1), s("|a"), b([]byte{0x80, 1})},
				{i(2), s("b"), b([]byte{2})}, {i(2), s("b"), b([]byte{2})},
				{null, s("|ab"), b([]byte{0x80, 1, 2})}, {null, s("|ab"), b([]byte{0x80, 1, 2})},
			})
			if spill && env.Query.Usage().Spill.Peak == 0 {
				t.Fatal("grouping-set replay did not spill")
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLStringAggregateWindowFrames(t *testing.T) {
	s, b := model.StringVal, model.BytesVal
	null := []model.Value{model.NullVal(model.TypeString), model.NullVal(model.TypeBytes)}
	empty := []model.Value{s(""), b(nil)}
	for _, scenario := range []struct {
		name, frame string
		want        [][]model.Value
	}{
		{"growing", "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW", [][]model.Value{null, empty, {s(":α"), b([]byte{0x80, 0})}, {s(":αβ"), b([]byte{0x80, 0, 0xff})}, {s(":αβ!"), b([]byte{0x80, 0, 0xff, 0x7f})}}},
		{"moving", "ROWS BETWEEN 1 PRECEDING AND CURRENT ROW", [][]model.Value{null, empty, {s(":α"), b([]byte{0x80, 0})}, {s("αβ"), b([]byte{0, 0xff})}, {s("β!"), b([]byte{0xff, 0x7f})}}},
		{"excluded current", "ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW", [][]model.Value{null, null, empty, {s("α"), b([]byte{0})}, {s("β"), b([]byte{0xff})}}},
		{"moving to empty frame", "ROWS BETWEEN 1 FOLLOWING AND 1 FOLLOWING", [][]model.Value{empty, {s("α"), b([]byte{0})}, {s("β"), b([]byte{0xff})}, empty, null}},
	} {
		for _, spill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/spill=%t", scenario.name, spill), func(t *testing.T) {
				graph := sqlGraph(t, `SELECT string_agg(x,d) OVER w,string_agg(y,e) OVER w
					FROM (VALUES(1,NULL::text,'~'::text,NULL::bytea,'\x00'::bytea),
						(2,'','^','\x','\xff'),(3,'α',':','\x00','\x80'),
						(4,'β',NULL,'\xff',NULL),(5,'','!','\x','\x7f')) v(id,x,d,y,e)
					WINDOW w AS (ORDER BY id `+scenario.frame+`) ORDER BY id`, nil)
				env, dir := groupingSetsEnvironment(t, false)
				env.Aggregate.PartitionMemoryBytes = 1024
				env.Limits.BatchRows = 1
				if spill {
					env.Window.PartitionMemoryBytes = 0
				}
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				assertStringAggregateRows(t, portal, scenario.want)
				if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
					t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
				}
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}

func TestSQLStringAggregateOutputBudget(t *testing.T) {
	for _, typ := range []string{"text", "bytea"} {
		for _, window := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/window=%t", typ, window), func(t *testing.T) {
				over := ""
				if window {
					over = " OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
				}
				payload := strings.Repeat("x", 400)
				graph := sqlGraph(t, "SELECT string_agg(x,''::"+typ+")"+over+" FROM (VALUES(1,'"+payload+"'::"+typ+"),(2,'"+payload+"'),(3,'"+payload+"')) v(id,x)", nil)
				env, dir := groupingSetsEnvironment(t, false)
				env.Aggregate.PartitionMemoryBytes = 1024
				env.Aggregate.MaxRecordBytes = 1024
				env.Window.MaxRecordBytes = 1024
				env.Window.PartitionMemoryBytes = 0
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				var batch columnar.Batch
				err = portal.Next(t.Context(), &batch)
				var sqlErr *model.Error
				if !errors.As(err, &sqlErr) || sqlErr.Code != "53200" {
					t.Fatalf("Next error=%v, want output limit SQLSTATE 53200", err)
				}
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}

func TestSQLStringAggregateActiveSpillCleanup(t *testing.T) {
	for _, window := range []bool{false, true} {
		for _, ending := range []string{"early close", "cancel"} {
			t.Run(fmt.Sprintf("window=%t/%s", window, ending), func(t *testing.T) {
				projection := "string_agg(x,d ORDER BY id),string_agg(y,e ORDER BY id)"
				suffix := " GROUP BY GROUPING SETS ((),(),())"
				if window {
					projection = "string_agg(x,d) OVER w,string_agg(y,e) OVER w"
					suffix = " WINDOW w AS (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
				}
				graph := sqlGraph(t, "SELECT "+projection+` FROM (VALUES
					(1,''::text,'ignored'::text,'\x'::bytea,'\xff'::bytea),
					(2,'a','|','\x00','\x80'),(3,'b',NULL,'\xff',NULL)) v(id,x,d,y,e)`+suffix, nil)
				env, dir := groupingSetsEnvironment(t, true)
				env.Aggregate.PartitionMemoryBytes = 1024
				env.Window.PartitionMemoryBytes = 0
				env.Limits.BatchRows = 1
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				portal, err := exec.Open(ctx, graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				var batch columnar.Batch
				if err := portal.Next(ctx, &batch); err != nil {
					t.Fatal(err)
				}
				assertStringAggregateBatch(t, &batch, [][]model.Value{{model.StringVal("|ab"), model.BytesVal([]byte{0x80, 0, 0xff})}})
				if usage := env.Query.Usage(); usage.Spill.Live == 0 || usage.Files == 0 {
					t.Fatalf("spill was not active before %s: %+v", ending, usage)
				}
				if ending == "cancel" {
					cancel()
					if err := portal.Next(ctx, &batch); !errors.Is(err, context.Canceled) {
						t.Fatalf("Next error=%v, want context cancellation", err)
					}
				} else {
					closePortal(t, portal)
				}
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}
