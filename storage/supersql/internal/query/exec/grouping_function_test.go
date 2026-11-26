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
)

func TestSQLGroupingFunctionMasks(t *testing.T) {
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	const source = ` FROM (VALUES(1,10,2),(1,20,4),(2,10,8)) v(a,b,x) `
	rollup := [][]model.Value{
		{i(1), i(10), i(2), i(0)},
		{i(1), i(20), i(4), i(0)},
		{i(2), i(10), i(8), i(0)},
		{i(1), null, i(6), i(1)},
		{i(2), null, i(8), i(1)},
		{null, null, i(14), i(3)},
	}
	cube := append(append([][]model.Value(nil), rollup...),
		[]model.Value{null, i(10), i(10), i(2)},
		[]model.Value{null, i(20), i(4), i(2)})
	for _, tc := range []struct {
		name    string
		sql     string
		masks   []int
		want    [][]model.Value
		ordered bool
	}{
		{
			name:  "ordinary grouping includes actual null",
			sql:   `SELECT a,count(*),GROUPING(a) FROM (VALUES(1),(1),(NULL::integer)) v(a) GROUP BY a`,
			masks: []int{2},
			want:  [][]model.Value{{i(1), i(2), i(0)}, {null, i(1), i(0)}},
		},
		{
			name:  "rollup rightmost bit",
			sql:   `SELECT a,b,sum(x),GROUPING(a,b)` + source + `GROUP BY ROLLUP(a,b)`,
			masks: []int{3},
			want:  rollup,
		},
		{
			name:  "cube every membership mask",
			sql:   `SELECT a,b,sum(x),GROUPING(a,b)` + source + `GROUP BY CUBE(a,b)`,
			masks: []int{3},
			want:  cube,
		},
		{
			name:  "reordered grouping keys",
			sql:   `SELECT a,b,sum(x),GROUPING(a,b)` + source + `GROUP BY GROUPING SETS ((b,a),(a),())`,
			masks: []int{3},
			want:  rollup,
		},
		{
			name:  "argument order and repeated arguments",
			sql:   `SELECT a,b,GROUPING(a,b),GROUPING(b,a),GROUPING(a,b,a) FROM (VALUES(1,10)) v(a,b) GROUP BY CUBE(a,b)`,
			masks: []int{2, 3, 4},
			want: [][]model.Value{
				{i(1), i(10), i(0), i(0), i(0)},
				{i(1), null, i(1), i(2), i(2)},
				{null, i(10), i(2), i(1), i(5)},
				{null, null, i(3), i(3), i(7)},
			},
		},
		{
			name:  "actual null differs from rolled up null",
			sql:   `SELECT a,b,sum(x),GROUPING(a,b) FROM (VALUES(1,NULL::integer,2),(1,10,3),(NULL,10,7)) v(a,b,x) GROUP BY ROLLUP(a,b)`,
			masks: []int{3},
			want: [][]model.Value{
				{i(1), null, i(2), i(0)}, {i(1), i(10), i(3), i(0)},
				{null, i(10), i(7), i(0)}, {i(1), null, i(5), i(1)},
				{null, null, i(7), i(1)}, {null, null, i(12), i(3)},
			},
		},
		{
			name:  "repeated sets preserve bags",
			sql:   `SELECT a,sum(x),GROUPING(a) FROM (VALUES(NULL::integer,5)) v(a,x) GROUP BY GROUPING SETS ((a),(),(a),())`,
			masks: []int{2},
			want:  [][]model.Value{{null, i(5), i(0)}, {null, i(5), i(1)}, {null, i(5), i(0)}, {null, i(5), i(1)}},
		},
		{
			name:  "distinct sets retain both null groups",
			sql:   `SELECT a,sum(x),GROUPING(a) FROM (VALUES(NULL::integer,5)) v(a,x) GROUP BY DISTINCT GROUPING SETS ((a),(),(a),())`,
			masks: []int{2},
			want:  [][]model.Value{{null, i(5), i(0)}, {null, i(5), i(1)}},
		},
		{
			name:  "distinct cartesian rollups",
			sql:   `SELECT a,b,sum(x),GROUPING(a,b)` + source + `GROUP BY DISTINCT ROLLUP(a,b),ROLLUP(a)`,
			masks: []int{3},
			want:  rollup,
		},
		{
			name:  "empty input repeated global sets",
			sql:   `SELECT a,count(*),sum(x),GROUPING(a) FROM (VALUES(1,2)) v(a,x) WHERE false GROUP BY GROUPING SETS ((a),(),())`,
			masks: []int{3},
			want:  [][]model.Value{{null, i(0), null, i(1)}, {null, i(0), null, i(1)}},
		},
		{
			name:  "empty input nonempty sets",
			sql:   `SELECT a,b,GROUPING(a,b)` + source + `WHERE false GROUP BY GROUPING SETS ((a),(b))`,
			masks: []int{2},
		},
		{
			name:  "grouping without aggregate calls",
			sql:   `SELECT x,GROUPING(x) FROM (VALUES(1),(1),(2)) v(x) GROUP BY ROLLUP(x)`,
			masks: []int{1},
			want:  [][]model.Value{{i(1), i(0)}, {i(2), i(0)}, {null, i(1)}},
		},
		{
			name:  "computed key qualified equivalent",
			sql:   `SELECT x%2,count(*),GROUPING(v.x%2) FROM (VALUES(1),(3),(2)) v(x) GROUP BY ROLLUP(x%2)`,
			masks: []int{2},
			want:  [][]model.Value{{i(1), i(2), i(0)}, {i(0), i(1), i(0)}, {null, i(3), i(1)}},
		},
		{
			name:  "computed key ordinal",
			sql:   `SELECT x+1,count(*),GROUPING(x+1) FROM (VALUES(1),(1),(2)) v(x) GROUP BY GROUPING SETS ((1),())`,
			masks: []int{2},
			want:  [][]model.Value{{i(2), i(2), i(0)}, {i(3), i(1), i(0)}, {null, i(3), i(1)}},
		},
		{
			name:  "computed key output alias",
			sql:   `SELECT x+1 AS bucket,count(*),GROUPING(x+1) FROM (VALUES(1),(1),(2)) v(x) GROUP BY ROLLUP(bucket)`,
			masks: []int{2},
			want:  [][]model.Value{{i(2), i(2), i(0)}, {i(3), i(1), i(0)}, {null, i(3), i(1)}},
		},
		{
			name:  "qualified grouping key unqualified argument",
			sql:   `SELECT a,GROUPING(a),GROUPING(v.a) FROM (VALUES(1),(2)) v(a) GROUP BY ROLLUP(v.a)`,
			masks: []int{1, 2},
			want:  [][]model.Value{{i(1), i(0), i(0)}, {i(2), i(0), i(0)}, {null, i(1), i(1)}},
		},
		{
			name:    "having and hidden ordering masks",
			sql:     `SELECT a,sum(x)` + source + `GROUP BY ROLLUP(a,b) HAVING GROUPING(b)=1 ORDER BY GROUPING(a,b) DESC,a`,
			want:    [][]model.Value{{null, i(14)}, {i(1), i(6)}, {i(2), i(8)}},
			ordered: true,
		},
		{
			name:    "scalar expression orders by grouping alias",
			sql:     `SELECT a,CASE WHEN GROUPING(a)=1 THEN 99 ELSE sum(x) END AS total,GROUPING(a) AS mask` + source + `GROUP BY ROLLUP(a) ORDER BY mask DESC,a DESC`,
			masks:   []int{2},
			want:    [][]model.Value{{null, i(99), i(1)}, {i(2), i(8), i(0)}, {i(1), i(6), i(0)}},
			ordered: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := sqlGraph(t, tc.sql, nil)
			root, err := graph.Plan().CloneNode(t.Context(), graph.Plan().Root(), 4<<20)
			if err != nil {
				t.Fatal(err)
			}
			for _, index := range tc.masks {
				if root.Output[index].Nullable {
					t.Errorf("GROUPING output %d is nullable", index)
				}
			}
			for _, spill := range []bool{false, true} {
				t.Run(fmt.Sprintf("replay_spill=%t", spill), func(t *testing.T) {
					env, dir := groupingSetsEnvironment(t, spill)
					portal, err := exec.Open(t.Context(), graph, env)
					if err != nil {
						t.Fatal(err)
					}
					defer closePortal(t, portal)
					got := groupingFunctionRows(t, portal, tc.masks)
					if tc.ordered {
						if !reflect.DeepEqual(got, tc.want) {
							t.Fatalf("GROUPING rows=%v, want ordered rows %v", got, tc.want)
						}
					} else {
						assertGroupingSetsBag(t, got, tc.want)
					}
					groupingSetsReleased(t, env, dir)
				})
			}
		})
	}
}

func TestSQLGroupingFunctionThirtyOneArguments(t *testing.T) {
	arguments := strings.TrimSuffix(strings.Repeat("x,", 31), ",")
	graph := sqlGraph(t, `SELECT GROUPING(`+arguments+`),GROUPING(x) FROM (VALUES(1)) v(x) GROUP BY ROLLUP(x)`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	assertGroupingSetsBag(t, groupingFunctionRows(t, portal, []int{0, 1}), [][]model.Value{{i(0), i(0)}, {i(2147483647), i(1)}})
	groupingSetsReleased(t, env, dir)
}

func TestSQLGroupingFunctionSpilledCorrelatedInvocations(t *testing.T) {
	var values []string
	for group := range 9 {
		values = append(values, fmt.Sprintf("(%d,2),(%d,2),(%d,5)", group, group, group))
	}
	graph := sqlGraph(t, `SELECT a.x,b.g,b.mask,b.total FROM (VALUES(0),(2),(0),(20)) a(x) CROSS JOIN LATERAL (
		SELECT v.g,GROUPING(v.g) AS mask,sum(v.x+a.x) AS total
		FROM (VALUES`+strings.Join(values, ",")+`) v(g,x) WHERE v.g>=a.x
		GROUP BY GROUPING SETS ((),(v.g),(),(v.g))) b`, nil)
	env, dir := groupingSetsEnvironment(t, true)
	env.Limits.BatchRows = 1
	env.Aggregate.GroupBytes = 32 << 10
	env.Apply.MaxRowBytes, env.Apply.WorkBytes = 4096, 64<<10
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	i := model.Int64Val
	null := model.NullVal(model.TypeInt64)
	var want [][]model.Value
	for _, outer := range []int64{0, 2, 0, 20} {
		for range 2 {
			if outer == 20 {
				want = append(want, []model.Value{i(outer), null, i(1), null})
				continue
			}
			for group := outer; group < 9; group++ {
				want = append(want, []model.Value{i(outer), i(group), i(0), i(9 + 3*outer)})
			}
			total := int64(81)
			if outer == 2 {
				total = 105
			}
			want = append(want, []model.Value{i(outer), null, i(1), i(total)})
		}
	}
	assertGroupingSetsBag(t, groupingFunctionRows(t, portal, []int{2}), want)
	if usage := env.Query.Usage(); usage.Spill.Peak == 0 || usage.PeakFiles < 2 {
		t.Fatalf("correlated replay and aggregate did not spill together: %+v", usage)
	}
	groupingSetsReleased(t, env, dir)
}

func groupingFunctionRows(t *testing.T, portal *exec.Portal, masks []int) [][]model.Value {
	t.Helper()
	var result [][]model.Value
	var batch columnar.Batch
	for {
		err := portal.Next(t.Context(), &batch)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, index := range masks {
			if index >= len(batch.Columns) {
				t.Fatalf("missing GROUPING output %d in %d columns", index, len(batch.Columns))
			}
			if typ := batch.Columns[index].Type; typ.ID != 23 || typ.Storage != model.TypeInt64 {
				t.Fatalf("GROUPING output %d type=%+v, want int4 OID 23", index, typ)
			}
		}
		for index := range batch.Rows() {
			lane := batch.Lane(index)
			for _, mask := range masks {
				if batch.Columns[mask].IsNull(lane) {
					t.Fatalf("GROUPING output %d is NULL at row %d", mask, len(result))
				}
			}
			row := make([]model.Value, len(batch.Columns))
			for column := range row {
				row[column], _, err = columnar.CopyValue(t.Context(), &batch.Columns[column], lane, 1<<20)
				if err != nil {
					t.Fatal(err)
				}
			}
			result = append(result, row)
		}
	}
	if err := portal.Next(t.Context(), &batch); err != io.EOF {
		t.Fatalf("repeated EOF=%v", err)
	}
	return result
}
