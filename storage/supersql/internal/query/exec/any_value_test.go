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

func assertAnyValueBatch(t *testing.T, batch *columnar.Batch, oids []model.TypeID, want [][]model.Value) {
	t.Helper()
	if batch.Rows() != len(want) || len(batch.Columns) != len(oids) {
		t.Fatalf("batch shape=%dx%d, want %dx%d", batch.Rows(), len(batch.Columns), len(want), len(oids))
	}
	for column, oid := range oids {
		if got := batch.Columns[column].Type.ID; got != oid {
			t.Errorf("column %d OID=%d, want %d", column, got, oid)
		}
	}
	for row, expected := range want {
		for column, value := range expected {
			got, _, err := columnar.CopyValue(t.Context(), &batch.Columns[column], batch.Lane(row), 1<<20)
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

func assertAnyValueRows(t *testing.T, portal *exec.Portal, oids []model.TypeID, want [][]model.Value) {
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
		assertAnyValueBatch(t, &batch, oids, want[seen:seen+batch.Rows()])
		seen += batch.Rows()
	}
	if seen != len(want) {
		t.Fatalf("rows=%d, want %d", seen, len(want))
	}
}

func TestSQLAnyValueScalarTypesAndNulls(t *testing.T) {
	i, s, b := model.Int64Val, model.StringVal, model.BytesVal
	null := model.NullVal(model.TypeInt64)
	for _, scenario := range []struct {
		name, sql string
		oids      []model.TypeID
		want      [][]model.Value
	}{
		{
			"integer widths and inferred literals",
			`SELECT any_value(7::int2),any_value(2147483647),any_value(2147483648),any_value(1.25)`,
			[]model.TypeID{21, 23, 20, 1700},
			[][]model.Value{{i(7), i(2147483647), i(2147483648), {Type: model.TypeNumeric, StrVal: "1.25"}}},
		},
		{
			"first nonnull in opposite orders",
			`SELECT any_value(x ORDER BY k),any_value(x ORDER BY k DESC),any_value(y ORDER BY k),any_value(z ORDER BY k)
			FROM (VALUES(1,NULL::int4,NULL::text,NULL::bytea),(2,7,'Ω','\x00ff'),(3,9,'later','\x80'),(4,NULL,NULL,NULL)) v(k,x,y,z)`,
			[]model.TypeID{23, 23, 25, 17},
			[][]model.Value{{i(7), i(9), s("Ω"), b([]byte{0, 255})}},
		},
		{
			"empty payload is not null",
			`SELECT any_value(x ORDER BY k),any_value(y ORDER BY k) FROM (VALUES(1,''::text,'\x'::bytea),(2,'later','\x01')) v(k,x,y)`,
			[]model.TypeID{25, 17},
			[][]model.Value{{s(""), b(nil)}},
		},
		{
			"null only",
			`SELECT any_value(x),any_value(y),any_value(z) FROM (VALUES(NULL::int4,NULL::text,NULL::bytea),(NULL,NULL,NULL)) v(x,y,z)`,
			[]model.TypeID{23, 25, 17},
			[][]model.Value{{null, model.NullVal(model.TypeString), model.NullVal(model.TypeBytes)}},
		},
		{
			"empty input",
			`SELECT any_value(x),any_value(y),any_value(z) FROM (VALUES(7,'value'::text,'\x00'::bytea)) v(x,y,z) WHERE false`,
			[]model.TypeID{23, 25, 17},
			[][]model.Value{{null, model.NullVal(model.TypeString), model.NullVal(model.TypeBytes)}},
		},
		{
			"primitive distinct and filter precedence",
			`SELECT any_value(DISTINCT x ORDER BY x) FILTER(WHERE enabled),any_value(DISTINCT y ORDER BY y DESC) FILTER(WHERE enabled),any_value(DISTINCT z ORDER BY z)
			FROM (VALUES(0,'z'::text,'\x02'::bytea,false),(7,'a','\x01',true),(7,'a','\x01',true),(9,'b',NULL,true),(NULL,NULL,NULL,true),(1,'y','\x02',NULL)) v(x,y,z,enabled)`,
			[]model.TypeID{23, 25, 17},
			[][]model.Value{{i(7), s("b"), b([]byte{1})}},
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
			assertAnyValueRows(t, portal, scenario.oids, scenario.want)
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLAnyValueNestedLayouts(t *testing.T) {
	array := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{model.Int64Val(7), model.NullVal(model.TypeInt64)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: 1}}}
	record := model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{model.StringVal("Ω"), array}}
	for _, window := range []bool{false, true} {
		t.Run(fmt.Sprintf("window=%t", window), func(t *testing.T) {
			over := ""
			if window {
				over = " OVER ()"
			}
			graph := sqlGraph(t, `SELECT any_value(ARRAY[7,NULL::int4])`+over+`,any_value(ROW('Ω'::text,ARRAY[7,NULL::int4]))`+over, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 4096
			env.Aggregate.MaxRecordBytes = 4096
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			if err := portal.Next(t.Context(), &batch); err != nil {
				t.Fatal(err)
			}
			assertAnyValueBatch(t, &batch, []model.TypeID{1007, 2249}, [][]model.Value{{array, record}})
			a, r := &batch.Columns[0], &batch.Columns[1]
			if len(a.Children) != 1 || a.Children[0].Type.ID != 23 {
				t.Fatalf("array children=%+v, want one int4 child", a.Children)
			}
			if len(r.Children) != 2 || r.Children[0].Type.ID != 25 || r.Children[0].Type.Collation != 100 || r.Children[1].Type.ID != 1007 {
				t.Fatalf("record children=%+v, want collated text and int4[]", r.Children)
			}
			if nested := r.Children[1].Children; len(nested) != 1 || nested[0].Type.ID != 23 {
				t.Fatalf("record array children=%+v, want one int4 child", nested)
			}
			if err := portal.Next(t.Context(), &batch); err != io.EOF {
				t.Fatalf("Next=%v, want EOF", err)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLAnyValueFilterMasksArgumentErrors(t *testing.T) {
	for _, window := range []bool{false, true} {
		t.Run(fmt.Sprintf("window=%t", window), func(t *testing.T) {
			argument, over := "10/x ORDER BY id", ""
			if window {
				argument, over = "10/x", " OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
			}
			graph := sqlGraph(t, `SELECT any_value(`+argument+`) FILTER(WHERE x<>0)`+over+`,any_value(`+argument+`) FILTER(WHERE false)`+over+`,any_value(`+argument+`) FILTER(WHERE NULL::boolean)`+over+` FROM (VALUES(1,0),(2,2),(3,NULL),(4,5)) v(id,x)`, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 1024
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			null := model.NullVal(model.TypeInt64)
			want := [][]model.Value{{model.Int64Val(5), null, null}}
			if window {
				want = append(want, want[0], want[0], want[0])
			}
			assertAnyValueRows(t, portal, []model.TypeID{23, 23, 23}, want)
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLAnyValueSelectedValueDoesNotHideLaterErrors(t *testing.T) {
	for _, window := range []bool{false, true} {
		t.Run(fmt.Sprintf("window=%t", window), func(t *testing.T) {
			over := ""
			if window {
				over = " OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW)"
			}
			graph := sqlGraph(t, `SELECT any_value(10/x)`+over+` FROM (VALUES(1,2),(2,5),(3,0)) v(id,x)`, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Limits.BatchRows = 1
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			var batch columnar.Batch
			for {
				err = portal.Next(t.Context(), &batch)
				if err != nil {
					break
				}
				assertAnyValueBatch(t, &batch, []model.TypeID{23}, [][]model.Value{{model.Int64Val(5)}})
			}
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != "22012" {
				t.Fatalf("Next error=%v, want SQLSTATE 22012", err)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLAnyValueGroupedSpill(t *testing.T) {
	var values []string
	var want [][]model.Value
	for group := range 16 {
		values = append(values, fmt.Sprintf(`(%[1]d,3,'later'::text),(%[1]d,1,NULL),(%[1]d,2,'selected-%[1]d')`, group))
		want = append(want, []model.Value{model.Int64Val(int64(group)), model.StringVal(fmt.Sprintf("selected-%d", group))})
	}
	graph := sqlGraph(t, `SELECT g,any_value(x ORDER BY k) FROM (VALUES`+strings.Join(values, ",")+`) v(g,k,x) GROUP BY g ORDER BY g`, nil)
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
			assertAnyValueRows(t, portal, []model.TypeID{23, 25}, want)
			if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
				t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLAnyValueGroupingSetsReplay(t *testing.T) {
	graph := sqlGraph(t, `SELECT g,any_value(x ORDER BY g,k) FROM (VALUES(1,1,NULL::text),(1,2,'a'),(2,1,'b'),(2,2,'later')) v(g,k,x) GROUP BY GROUPING SETS ((g),(),(g),()) ORDER BY g NULLS LAST`, nil)
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay_spill=%t", spill), func(t *testing.T) {
			env, dir := groupingSetsEnvironment(t, spill)
			env.Aggregate.PartitionMemoryBytes = 1024
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			i, s := model.Int64Val, model.StringVal
			null := model.NullVal(model.TypeInt64)
			assertAnyValueRows(t, portal, []model.TypeID{23, 25}, [][]model.Value{{i(1), s("a")}, {i(1), s("a")}, {i(2), s("b")}, {i(2), s("b")}, {null, s("a")}, {null, s("a")}})
			if spill && env.Query.Usage().Spill.Peak == 0 {
				t.Fatal("grouping-set replay did not spill")
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLAnyValueWindowFrames(t *testing.T) {
	null := model.NullVal(model.TypeString)
	a, b := model.StringVal("α"), model.StringVal("β")
	for _, scenario := range []struct {
		name, frame string
		want        [][]model.Value
	}{
		{"growing", "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW", [][]model.Value{{null}, {a}, {a}, {a}, {a}}},
		{"moving", "ROWS BETWEEN 1 PRECEDING AND CURRENT ROW", [][]model.Value{{null}, {a}, {a}, {b}, {b}}},
		{"excluded current", "ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW", [][]model.Value{{null}, {null}, {a}, {null}, {b}}},
		{"moving to empty", "ROWS BETWEEN 1 FOLLOWING AND 1 FOLLOWING", [][]model.Value{{a}, {null}, {b}, {null}, {null}}},
	} {
		for _, spill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/spill=%t", scenario.name, spill), func(t *testing.T) {
				graph := sqlGraph(t, `SELECT any_value(x) OVER (ORDER BY id `+scenario.frame+`) FROM (VALUES(1,NULL::text),(2,'α'),(3,NULL),(4,'β'),(5,NULL)) v(id,x) ORDER BY id`, nil)
				env, dir := groupingSetsEnvironment(t, false)
				env.Limits.BatchRows = 1
				if spill {
					env.Window.PartitionMemoryBytes = 0
				}
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				assertAnyValueRows(t, portal, []model.TypeID{25}, scenario.want)
				if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
					t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
				}
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}

func TestSQLAnyValueActiveSpillCleanup(t *testing.T) {
	for _, window := range []bool{false, true} {
		for _, ending := range []string{"early close", "cancel"} {
			t.Run(fmt.Sprintf("window=%t/%s", window, ending), func(t *testing.T) {
				projection := "any_value(x ORDER BY id),any_value(y ORDER BY id)"
				suffix := " GROUP BY GROUPING SETS ((),(),())"
				if window {
					projection = "any_value(x) OVER w,any_value(y) OVER w"
					suffix = " WINDOW w AS (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
				}
				graph := sqlGraph(t, "SELECT "+projection+` FROM (VALUES(1,NULL::text,NULL::bytea),(2,'owned','\x00ff'),(3,'later','\x80')) v(id,x,y)`+suffix, nil)
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
				assertAnyValueBatch(t, &batch, []model.TypeID{25, 17}, [][]model.Value{{model.StringVal("owned"), model.BytesVal([]byte{0, 255})}})
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
