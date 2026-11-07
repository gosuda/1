package exec_test

import (
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
	"github.com/gosuda/supersql-alpha/internal/query/exec/expr"
)

func arrayAggregateLayout(oid model.TypeID, element model.TypeLayout) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: oid, Modifier: -1, Collation: element.Type.Collation, Storage: model.TypeArray}, Children: []model.TypeLayout{element}}
}

func arrayAggregateValue(elements ...model.Value) model.Value {
	return model.Value{Type: model.TypeArray, ArrayVal: elements, Dimensions: []model.ArrayDimension{{Length: int32(len(elements)), LowerBound: 1}}}
}

func assertArrayAggregateLayout(t *testing.T, vector *columnar.Vector, want model.TypeLayout) {
	t.Helper()
	if vector.Type != want.Type || len(vector.Children) != len(want.Children) {
		t.Fatalf("layout type=%+v children=%d, want type=%+v children=%d", vector.Type, len(vector.Children), want.Type, len(want.Children))
	}
	for i := range want.Children {
		assertArrayAggregateLayout(t, &vector.Children[i], want.Children[i])
	}
}

func assertArrayAggregateBatch(t *testing.T, batch *columnar.Batch, layouts []model.TypeLayout, want [][]model.Value) {
	t.Helper()
	if batch.Rows() != len(want) || len(batch.Columns) != len(layouts) {
		t.Fatalf("batch shape=%dx%d, want %dx%d", batch.Rows(), len(batch.Columns), len(want), len(layouts))
	}
	for column, layout := range layouts {
		assertArrayAggregateLayout(t, &batch.Columns[column], layout)
	}
	for row, expected := range want {
		for column, value := range expected {
			got, _, err := columnar.CopyValue(t.Context(), &batch.Columns[column], batch.Lane(row), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, value) {
				t.Errorf("row %d column %d=%#v, want %#v", row, column, got, value)
			}
		}
	}
}

func assertArrayAggregateRows(t *testing.T, portal *exec.Portal, layouts []model.TypeLayout, want [][]model.Value) {
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
		assertArrayAggregateBatch(t, &batch, layouts, want[seen:seen+batch.Rows()])
		seen += batch.Rows()
	}
	if seen != len(want) {
		t.Fatalf("rows=%d, want %d", seen, len(want))
	}
}

func TestSQLArrayAggregateScalarNullsDistinctAndFilter(t *testing.T) {
	i, a := model.Int64Val, arrayAggregateValue
	null := model.NullVal(model.TypeInt64)
	layout := arrayAggregateLayout(1007, model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"null elements", `SELECT array_agg(x ORDER BY id) FROM (VALUES(1,NULL::int4),(2,7),(3,NULL),(4,0)) v(id,x)`, [][]model.Value{{a(null, i(7), null, i(0))}}},
		{"all null elements", `SELECT array_agg(x) FROM (VALUES(NULL::int4),(NULL)) v(x)`, [][]model.Value{{a(null, null)}}},
		{"no rows", `SELECT array_agg(x) FROM (VALUES(7)) v(x) WHERE false`, [][]model.Value{{model.NullVal(model.TypeArray)}}},
		{"distinct includes one null", `SELECT array_agg(DISTINCT x ORDER BY x NULLS FIRST) FROM (VALUES(2),(NULL::int4),(1),(2),(NULL)) v(x)`, [][]model.Value{{a(null, i(1), i(2))}}},
		{"descending filtered", `SELECT array_agg(x ORDER BY id DESC) FILTER (WHERE id <> 2) FROM (VALUES(1,4),(2,99),(3,NULL::int4),(4,8)) v(id,x)`, [][]model.Value{{a(i(8), null, i(4))}}},
		{"filter masks argument error", `SELECT array_agg(10/x ORDER BY x) FILTER (WHERE x <> 0) FROM (VALUES(0),(2)) v(x)`, [][]model.Value{{a(i(5))}}},
		{"all filtered", `SELECT array_agg(10/x) FILTER (WHERE false) FROM (VALUES(0)) v(x)`, [][]model.Value{{model.NullVal(model.TypeArray)}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			graph := sqlGraph(t, scenario.sql, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 4096
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertArrayAggregateRows(t, portal, []model.TypeLayout{layout}, scenario.want)
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLArrayAggregateNestedElementLayouts(t *testing.T) {
	i, s, a := model.Int64Val, model.StringVal, arrayAggregateValue
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}}
	binary := model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
	record := model.TypeLayout{Type: model.Type{ID: 2249, Modifier: -1, Storage: model.TypeComposite}, Children: []model.TypeLayout{text, arrayAggregateLayout(1007, integer)}}
	layouts := []model.TypeLayout{arrayAggregateLayout(1009, text), arrayAggregateLayout(1001, binary), arrayAggregateLayout(2287, record)}
	want := [][]model.Value{{a(s("Ω")), a(model.BytesVal([]byte{0, 255})), a(model.Value{Type: model.TypeComposite, ArrayVal: []model.Value{s("Ω"), a(i(7), model.NullVal(model.TypeInt64))}})}}
	for _, window := range []bool{false, true} {
		t.Run(fmt.Sprintf("window=%t", window), func(t *testing.T) {
			over := ""
			if window {
				over = " OVER ()"
			}
			graph := sqlGraph(t, `SELECT array_agg('Ω'::text)`+over+`,array_agg('\x00ff'::bytea)`+over+`,array_agg(ROW('Ω'::text,ARRAY[7,NULL::int4]))`+over, nil)
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.MaxRecordBytes = 4096
			env.Aggregate.PartitionMemoryBytes = 4096
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertArrayAggregateRows(t, portal, layouts, want)
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLArrayAggregateArrayInputDimensions(t *testing.T) {
	i, a := model.Int64Val, arrayAggregateValue
	layout := arrayAggregateLayout(1007, model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	for _, scenario := range []struct {
		name  string
		input model.Value
	}{
		{"one dimensional", a(i(7), model.NullVal(model.TypeInt64))},
		{"nondefault bounds", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(7), i(8)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -3}}}},
		{"multidimensional", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(7), model.NullVal(model.TypeInt64), i(8), i(9)}, Dimensions: []model.ArrayDimension{{Length: 2, LowerBound: -2}, {Length: 2, LowerBound: 4}}}},
		{"six output dimensions", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(7)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 0}, {Length: 1, LowerBound: -1}, {Length: 1, LowerBound: 2}, {Length: 1, LowerBound: 3}, {Length: 1, LowerBound: 4}}}},
	} {
		for _, window := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/window=%t", scenario.name, window), func(t *testing.T) {
				projection := "array_agg(x ORDER BY id)"
				if window {
					projection = "array_agg(x) OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
				}
				graph := sqlGraph(t, "SELECT "+projection+" FROM (VALUES(1,$1),(2,$1)) v(id,x)", []model.TypeLayout{layout})
				env, dir := groupingSetsEnvironment(t, false)
				env.Aggregate.MaxRecordBytes = 4096
				env.Aggregate.PartitionMemoryBytes = 4096
				env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: scenario.input}}
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatal(err)
				}
				defer closePortal(t, portal)
				result := model.Value{Type: model.TypeArray, Dimensions: append([]model.ArrayDimension{{Length: 2, LowerBound: 1}}, scenario.input.Dimensions...)}
				result.ArrayVal = append(append([]model.Value(nil), scenario.input.ArrayVal...), scenario.input.ArrayVal...)
				want := [][]model.Value{{result}}
				if window {
					want = append(want, []model.Value{result})
				}
				assertArrayAggregateRows(t, portal, []model.TypeLayout{layout}, want)
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}

func TestSQLArrayAggregateArrayInputWithoutTransitions(t *testing.T) {
	layout := arrayAggregateLayout(1007, model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	for _, sql := range []string{
		"SELECT array_agg($1) FROM (VALUES(1)) v(id) WHERE false",
		"SELECT array_agg($1) FILTER (WHERE false) FROM (VALUES(1)) v(id)",
		"SELECT array_agg($1) FILTER (WHERE false) OVER () FROM (VALUES(1)) v(id)",
	} {
		t.Run(sql, func(t *testing.T) {
			graph := sqlGraph(t, sql, []model.TypeLayout{layout})
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 4096
			env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: model.NullVal(model.TypeArray)}}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertArrayAggregateRows(t, portal, []model.TypeLayout{layout}, [][]model.Value{{model.NullVal(model.TypeArray)}})
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLArrayAggregateArrayInputErrorsAndFilter(t *testing.T) {
	i, a := model.Int64Val, arrayAggregateValue
	layout := arrayAggregateLayout(1007, model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	for _, scenario := range []struct {
		name, code string
		invalid    model.Value
	}{
		{"null", "22004", model.NullVal(model.TypeArray)},
		{"empty", "2202E", model.Value{Type: model.TypeArray}},
		{"different length", "2202E", a(i(1), i(2))},
		{"different lower bound", "2202E", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(1)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: -1}}}},
		{"different dimensionality", "2202E", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(1)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}}}},
		{"seven output dimensions", "54000", model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(1)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}}}},
	} {
		for _, window := range []bool{false, true} {
			for _, filtered := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/window=%t/filtered=%t", scenario.name, window, filtered), func(t *testing.T) {
					projection := arrayAggregateProjection(window, filtered)
					// The dimension ceiling applies to the first transition, independently of shape agreement.
					first := a(i(7))
					if scenario.code == "54000" && !filtered {
						first = scenario.invalid
					}
					graph := sqlGraph(t, "SELECT "+projection+" FROM (VALUES(1,$1),(2,$2)) v(id,x)", []model.TypeLayout{layout, layout})
					env, dir := groupingSetsEnvironment(t, false)
					env.Aggregate.MaxRecordBytes = 4096
					env.Aggregate.PartitionMemoryBytes = 4096
					env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: first}, {Number: 2, Layout: layout, Value: scenario.invalid}}
					portal, err := exec.Open(t.Context(), graph, env)
					if err != nil {
						t.Fatal(err)
					}
					defer closePortal(t, portal)
					if filtered {
						result := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(7)}, Dimensions: []model.ArrayDimension{{Length: 1, LowerBound: 1}, {Length: 1, LowerBound: 1}}}
						want := [][]model.Value{{result}}
						if window {
							want = append(want, []model.Value{result})
						}
						assertArrayAggregateRows(t, portal, []model.TypeLayout{layout}, want)
					} else {
						assertArrayAggregateFailure(t, portal, scenario.code)
					}
					groupingSetsReleased(t, env, dir)
				})
			}
		}
	}
}

func TestSQLArrayAggregateGroupedSpill(t *testing.T) {
	i, a := model.Int64Val, arrayAggregateValue
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	layout := arrayAggregateLayout(1007, integer)
	var values []string
	var want [][]model.Value
	for group := range 24 {
		values = append(values, fmt.Sprintf("(%[1]d,2,NULL::int4),(%[1]d,1,%[1]d),(%[1]d,3,%[1]d)", group))
		null := model.NullVal(model.TypeInt64)
		matrix := model.Value{Type: model.TypeArray, ArrayVal: []model.Value{i(int64(group)), null, i(int64(group))}, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: 1}, {Length: 1, LowerBound: 1}}}
		want = append(want, []model.Value{i(int64(group)), a(i(int64(group)), null, i(int64(group))), a(null, i(int64(group))), matrix})
	}
	graph := sqlGraph(t, `SELECT g,array_agg(x ORDER BY k),array_agg(DISTINCT x ORDER BY x NULLS FIRST),array_agg(ARRAY[x] ORDER BY k) FROM (VALUES`+strings.Join(values, ",")+`) v(g,k,x) GROUP BY g ORDER BY g`, nil)
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("spill=%t", spill), func(t *testing.T) {
			env, dir := groupingSetsEnvironment(t, false)
			env.Aggregate.PartitionMemoryBytes = 512 << 10
			if spill {
				env.Aggregate.GroupBytes = 64 << 10
				env.Aggregate.PartitionMemoryBytes = 1024
			}
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertArrayAggregateRows(t, portal, []model.TypeLayout{integer, layout, layout, layout}, want)
			if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
				t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLArrayAggregateGroupingSetsReplay(t *testing.T) {
	i, a := model.Int64Val, arrayAggregateValue
	null := model.NullVal(model.TypeInt64)
	integer := model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}}
	layout := arrayAggregateLayout(1007, integer)
	graph := sqlGraph(t, `SELECT g,array_agg(x ORDER BY g,k),array_agg(ARRAY[x] ORDER BY g,k) FROM (VALUES(1,1,NULL::int4),(1,2,7),(2,1,8)) v(g,k,x) GROUP BY GROUPING SETS ((g),(),(g),()) ORDER BY g NULLS LAST`, nil)
	matrix := func(elements ...model.Value) model.Value {
		return model.Value{Type: model.TypeArray, ArrayVal: elements, Dimensions: []model.ArrayDimension{{Length: int32(len(elements)), LowerBound: 1}, {Length: 1, LowerBound: 1}}}
	}
	want := [][]model.Value{{i(1), a(null, i(7)), matrix(null, i(7))}, {i(1), a(null, i(7)), matrix(null, i(7))}, {i(2), a(i(8)), matrix(i(8))}, {i(2), a(i(8)), matrix(i(8))}, {null, a(null, i(7), i(8)), matrix(null, i(7), i(8))}, {null, a(null, i(7), i(8)), matrix(null, i(7), i(8))}}
	for _, spill := range []bool{false, true} {
		t.Run(fmt.Sprintf("replay_spill=%t", spill), func(t *testing.T) {
			env, dir := groupingSetsEnvironment(t, spill)
			env.Aggregate.PartitionMemoryBytes = 4096
			portal, err := exec.Open(t.Context(), graph, env)
			if err != nil {
				t.Fatal(err)
			}
			defer closePortal(t, portal)
			assertArrayAggregateRows(t, portal, []model.TypeLayout{integer, layout, layout}, want)
			if spill && env.Query.Usage().Spill.Peak == 0 {
				t.Fatal("grouping-set replay did not spill")
			}
			groupingSetsReleased(t, env, dir)
		})
	}
}

func TestSQLArrayAggregateWindowFrames(t *testing.T) {
	i, a := model.Int64Val, arrayAggregateValue
	null, empty := model.NullVal(model.TypeInt64), model.NullVal(model.TypeArray)
	layout := arrayAggregateLayout(1007, model.TypeLayout{Type: model.Type{ID: 23, Modifier: -1, Storage: model.TypeInt64}})
	for _, scenario := range []struct {
		name, frame string
		want        []model.Value
	}{
		{"growing", "ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW", []model.Value{a(null), a(null, i(7)), a(null, i(7), i(8))}},
		{"moving", "ROWS BETWEEN 1 PRECEDING AND CURRENT ROW", []model.Value{a(null), a(null, i(7)), a(i(7), i(8))}},
		{"excluded current", "ROWS BETWEEN 1 PRECEDING AND CURRENT ROW EXCLUDE CURRENT ROW", []model.Value{empty, a(null), a(i(7))}},
		{"moving to empty", "ROWS BETWEEN 1 FOLLOWING AND 1 FOLLOWING", []model.Value{a(i(7)), a(i(8)), empty}},
	} {
		for _, spill := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/spill=%t", scenario.name, spill), func(t *testing.T) {
				graph := sqlGraph(t, `SELECT array_agg(x) OVER w,array_agg(ARRAY[x]) OVER w FROM (VALUES(1,NULL::int4),(2,7),(3,8)) v(id,x) WINDOW w AS (ORDER BY id `+scenario.frame+`) ORDER BY id`, nil)
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
				var want [][]model.Value
				for _, scalar := range scenario.want {
					matrix := scalar
					if !scalar.IsNull {
						matrix.Dimensions = []model.ArrayDimension{{Length: int32(len(scalar.ArrayVal)), LowerBound: 1}, {Length: 1, LowerBound: 1}}
					}
					want = append(want, []model.Value{scalar, matrix})
				}
				assertArrayAggregateRows(t, portal, []model.TypeLayout{layout, layout}, want)
				if usage := env.Query.Usage(); (usage.Spill.Peak != 0) != spill {
					t.Fatalf("spill=%t, want %t; usage=%+v", usage.Spill.Peak != 0, spill, usage)
				}
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}

func TestSQLArrayAggregateRetainedStateBudget(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 950, Storage: model.TypeString}}
	for _, count := range []int{4, 32} {
		var values []string
		for id := range count {
			values = append(values, fmt.Sprintf("(%d)", id))
		}
		for _, arrayInput := range []bool{false, true} {
			for _, window := range []bool{false, true} {
				t.Run(fmt.Sprintf("rows=%d/array_input=%t/window=%t", count, arrayInput, window), func(t *testing.T) {
					payload := model.StringVal(strings.Repeat("x", 400))
					layout, value := arrayAggregateParameter(arrayInput, text, payload)
					over := arrayAggregateWindow(window)
					graph := sqlGraph(t, "SELECT array_agg($1)"+over+" FROM (VALUES"+strings.Join(values, ",")+") v(id)", []model.TypeLayout{layout})
					env, dir := groupingSetsEnvironment(t, false)
					env.Aggregate.MaxRecordBytes = 1024
					env.Aggregate.PartitionMemoryBytes = 4096
					env.Window.MaxRecordBytes = 1024
					env.Parameters = []expr.ParameterValue{{Number: 1, Layout: layout, Value: value}}
					portal, err := exec.Open(t.Context(), graph, env)
					if err != nil {
						t.Fatal(err)
					}
					defer closePortal(t, portal)
					if count == 4 {
						result := arrayAggregateValue(payload, payload, payload, payload)
						if arrayInput {
							result.Dimensions = append(result.Dimensions, model.ArrayDimension{Length: 1, LowerBound: 1})
						}
						want := [][]model.Value{{result}}
						if window {
							want = [][]model.Value{{result}, {result}, {result}, {result}}
						}
						assertArrayAggregateRows(t, portal, []model.TypeLayout{arrayAggregateLayout(1009, text)}, want)
					} else {
						assertArrayAggregateFailure(t, portal, "53200")
					}
					groupingSetsReleased(t, env, dir)
				})
			}
		}
	}
}

func TestSQLArrayAggregateActiveSpillCleanup(t *testing.T) {
	text := model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: 100, Storage: model.TypeString}}
	binary := model.TypeLayout{Type: model.Type{ID: 17, Modifier: -1, Storage: model.TypeBytes}}
	layouts := []model.TypeLayout{arrayAggregateLayout(1009, text), arrayAggregateLayout(1001, binary)}
	want := [][]model.Value{{arrayAggregateValue(model.NullVal(model.TypeString), model.StringVal("owned"), model.StringVal("later")), {Type: model.TypeArray, ArrayVal: []model.Value{model.NullVal(model.TypeBytes), model.BytesVal([]byte{0, 255}), model.BytesVal([]byte{128})}, Dimensions: []model.ArrayDimension{{Length: 3, LowerBound: 1}, {Length: 1, LowerBound: 1}}}}}
	for _, window := range []bool{false, true} {
		for _, ending := range []string{"early close", "cancel"} {
			t.Run(fmt.Sprintf("window=%t/%s", window, ending), func(t *testing.T) {
				projection := "array_agg(x ORDER BY id),array_agg(ARRAY[y] ORDER BY id)"
				suffix := " GROUP BY GROUPING SETS ((),(),())"
				if window {
					projection = "array_agg(x) OVER w,array_agg(ARRAY[y]) OVER w"
					suffix = " WINDOW w AS (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
				}
				graph := sqlGraph(t, "SELECT "+projection+` FROM (VALUES(1,NULL::text,NULL::bytea),(2,'owned','\x00ff'),(3,'later','\x80')) v(id,x,y)`+suffix, nil)
				env, dir := groupingSetsEnvironment(t, true)
				env.Aggregate.PartitionMemoryBytes = 4096
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
				assertArrayAggregateBatch(t, &batch, layouts, want)
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

func arrayAggregateWindow(window bool) string {
	if window {
		return " OVER (ORDER BY id ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)"
	}
	return ""
}

func arrayAggregateProjection(window, filtered bool) string {
	projection := "array_agg(x ORDER BY id)"
	if window {
		projection = "array_agg(x)"
	}
	if filtered {
		projection += " FILTER (WHERE id = 1)"
	}
	return projection + arrayAggregateWindow(window)
}

func arrayAggregateParameter(arrayInput bool, layout model.TypeLayout, value model.Value) (model.TypeLayout, model.Value) {
	if arrayInput {
		return arrayAggregateLayout(1009, layout), arrayAggregateValue(value)
	}
	return layout, value
}

func assertArrayAggregateFailure(t *testing.T, portal *exec.Portal, code string) {
	t.Helper()
	var batch columnar.Batch
	err := portal.Next(t.Context(), &batch)
	var sqlErr *model.Error
	if !errors.As(err, &sqlErr) || sqlErr.Code != code {
		t.Fatalf("Next error=%v, want SQLSTATE %s", err, code)
	}
	if batch.Rows() != 0 {
		t.Fatalf("error returned %d partial rows", batch.Rows())
	}
}
