package exec_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/bind"
	"github.com/gosuda/supersql-alpha/internal/query/exec"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func collationTextLayout(collation model.CollationID) model.TypeLayout {
	return model.TypeLayout{Type: model.Type{ID: 25, Modifier: -1, Collation: collation, Storage: model.TypeString}}
}

func assertCollationSQL(t *testing.T, sql string, layouts []model.TypeLayout, want [][]model.Value) {
	t.Helper()
	graph := sqlGraph(t, sql, nil)
	env, dir := groupingSetsEnvironment(t, false)
	env.Aggregate.PartitionMemoryBytes = 4096
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatal(err)
	}
	defer closePortal(t, portal)
	assertArrayAggregateRows(t, portal, layouts, want)
	groupingSetsReleased(t, env, dir)
}

func TestSQLCollationResultIdentities(t *testing.T) {
	s := model.StringVal
	assertCollationSQL(t, `SELECT 'a' COLLATE "C", 'b' COLLATE pg_catalog."POSIX",
		'c' COLLATE "default", 'd'::text, NULL::text COLLATE "POSIX"`,
		[]model.TypeLayout{collationTextLayout(950), collationTextLayout(951), collationTextLayout(100), collationTextLayout(100), collationTextLayout(951)},
		[][]model.Value{{s("a"), s("b"), s("c"), s("d"), model.NullVal(model.TypeString)}})
}

func TestSQLCollationDerivation(t *testing.T) {
	s := model.StringVal
	for _, scenario := range []struct {
		name, sql string
		collation model.CollationID
		want      [][]model.Value
	}{
		{"implicit nondefault beats default", `SELECT a || 'b'::text FROM (VALUES('a' COLLATE "POSIX")) v(a)`, 951, [][]model.Value{{s("ab")}}},
		{"explicit default beats implicit nondefault", `SELECT a || ('b' COLLATE "default") FROM (VALUES('a' COLLATE "POSIX")) v(a)`, 100, [][]model.Value{{s("ab")}}},
		{"explicit overrides implicit conflict", `SELECT coalesce(a,b,'c' COLLATE "C") FROM (VALUES(NULL::text COLLATE "C",'b' COLLATE "POSIX")) v(a,b)`, 950, [][]model.Value{{s("b")}}},
		{"case implicit conflict remains indeterminate", `SELECT CASE WHEN choose THEN a ELSE b END FROM (VALUES(true,'a' COLLATE "C",'b' COLLATE "POSIX"),(false,'c','d')) v(choose,a,b)`, 0, [][]model.Value{{s("a")}, {s("d")}}},
		{"case explicit default wins", `SELECT CASE WHEN choose THEN a ELSE b COLLATE "default" END FROM (VALUES(true,'a' COLLATE "C",'b' COLLATE "POSIX"),(false,'c','d')) v(choose,a,b)`, 100, [][]model.Value{{s("a")}, {s("d")}}},
		{"coalesce implicit conflict remains indeterminate", `SELECT coalesce(a,b) FROM (VALUES(NULL::text COLLATE "C",'b' COLLATE "POSIX"),('a',NULL)) v(a,b)`, 0, [][]model.Value{{s("b")}, {s("a")}}},
		{"outer collate resolves indeterminate result", `SELECT coalesce(a,b) COLLATE "POSIX" FROM (VALUES(NULL::text COLLATE "C",'b' COLLATE "POSIX")) v(a,b)`, 951, [][]model.Value{{s("b")}}},
		{"cast preserves explicit strength", `SELECT (('a' COLLATE "POSIX")::varchar::text) || b FROM (VALUES('b' COLLATE "C")) v(b)`, 951, [][]model.Value{{s("ab")}}},
		{"set operation default yields", `SELECT 'a'::text UNION ALL SELECT 'b' COLLATE "POSIX"`, 951, [][]model.Value{{s("a")}, {s("b")}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assertCollationSQL(t, scenario.sql, []model.TypeLayout{collationTextLayout(scenario.collation)}, scenario.want)
		})
	}
}

func TestSQLCollationArrayRelabelPreservesElements(t *testing.T) {
	defaultArray := arrayAggregateLayout(1009, collationTextLayout(100))
	relabeled := defaultArray
	relabeled.Type.Collation = 951
	assertCollationSQL(t, `SELECT ARRAY['a'::text,NULL] COLLATE "POSIX", ARRAY['b' COLLATE "C",NULL]`,
		[]model.TypeLayout{relabeled, arrayAggregateLayout(1009, collationTextLayout(950))},
		[][]model.Value{{arrayAggregateValue(model.StringVal("a"), model.NullVal(model.TypeString)), arrayAggregateValue(model.StringVal("b"), model.NullVal(model.TypeString))}})
}

func TestSQLCollationComparisonInputIsNotResultCollation(t *testing.T) {
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	assertCollationSQL(t, `SELECT a < (b COLLATE "POSIX"), (a COLLATE "default") = b,
		(a COLLATE "C") || b FROM (VALUES('A' COLLATE "C",'a' COLLATE "POSIX")) v(a,b)`,
		[]model.TypeLayout{boolean, boolean, collationTextLayout(950)},
		[][]model.Value{{model.BoolVal(true), model.BoolVal(false), model.StringVal("Aa")}})
}

func TestSQLCollationKeys(t *testing.T) {
	s := model.StringVal
	text := collationTextLayout(951)
	integer := model.TypeLayout{Type: model.Type{ID: 20, Modifier: -1, Storage: model.TypeInt64}}
	for _, scenario := range []struct {
		name, sql string
		layouts   []model.TypeLayout
		want      [][]model.Value
	}{
		{"ordering", `SELECT x COLLATE "POSIX" FROM (VALUES('b'::text),('A'),('ä'),('b')) v(x) ORDER BY x COLLATE "POSIX"`, []model.TypeLayout{text}, [][]model.Value{{s("A")}, {s("b")}, {s("b")}, {s("ä")}}},
		{"distinct", `SELECT DISTINCT x COLLATE "POSIX" FROM (VALUES('b'::text),('A'),('ä'),('b')) v(x) ORDER BY 1`, []model.TypeLayout{text}, [][]model.Value{{s("A")}, {s("b")}, {s("ä")}}},
		{"grouping", `SELECT x COLLATE "POSIX",count(*) FROM (VALUES('b'::text),('A'),('ä'),('b')) v(x) GROUP BY 1 ORDER BY 1`, []model.TypeLayout{text, integer}, [][]model.Value{{s("A"), model.Int64Val(1)}, {s("b"), model.Int64Val(2)}, {s("ä"), model.Int64Val(1)}}},
		{"aggregate input and ordered distinct", `SELECT min(x COLLATE "POSIX"),max(x COLLATE "POSIX"),string_agg(DISTINCT x COLLATE "POSIX",',' ORDER BY x COLLATE "POSIX") FROM (VALUES('b'::text),('A'),('ä'),('b')) v(x)`, []model.TypeLayout{text, text, text}, [][]model.Value{{s("A"), s("ä"), s("A,b,ä")}}},
		{"window order and aggregate input", `SELECT x COLLATE "POSIX",rank() OVER w,min(x COLLATE "POSIX") OVER w,max(x COLLATE "POSIX") OVER w FROM (VALUES('b'::text),('A'),('ä'),('b')) v(x) WINDOW w AS (ORDER BY x COLLATE "POSIX") ORDER BY 1`, []model.TypeLayout{text, integer, text, text}, [][]model.Value{{s("A"), model.Int64Val(1), s("A"), s("A")}, {s("b"), model.Int64Val(2), s("A"), s("b")}, {s("b"), model.Int64Val(2), s("A"), s("b")}, {s("ä"), model.Int64Val(4), s("A"), s("ä")}}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assertCollationSQL(t, scenario.sql, scenario.layouts, scenario.want)
		})
	}
}

func TestSQLCollationRejectedDuringBinding(t *testing.T) {
	for _, scenario := range []struct {
		name, expression, code string
	}{
		{"explicit conflict", `('a' COLLATE "C") = ('b' COLLATE "POSIX")`, "42P21"},
		{"explicit conflict in inactive case", `CASE WHEN false THEN ('a' COLLATE "C") = ('b' COLLATE "POSIX") ELSE true END`, "42P21"},
		{"unknown name", `'a' COLLATE "missing_collation"`, "42704"},
		{"wrong namespace", `'a' COLLATE public."C"`, "42704"},
		{"noncollatable input", `1 COLLATE "C"`, "42804"},
		{"resolved fixed exclusion", `'a' COLLATE pg_catalog.ucs_basic`, "0A000"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			parsed, err := zpgparse.Parse(`SELECT 1/0, ` + scenario.expression)
			if err != nil {
				t.Fatal(err)
			}
			bound, err := bind.Bind(t.Context(), parsed.Stmts[0], nil, bind.Options{SearchPath: []string{"public"}, Collation: 950, DefaultCollation: 950, Limits: irLimits()}, nil)
			var sqlErr *model.Error
			if !errors.As(err, &sqlErr) || sqlErr.Code != scenario.code {
				t.Fatalf("Bind error=%v, want SQLSTATE %s before division evaluation", err, scenario.code)
			}
			if bound != nil {
				t.Fatal("binding rejection returned an executable statement")
			}
		})
	}
}

func TestSQLCollationIndeterminateComparisonMasks(t *testing.T) {
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	for _, scenario := range []struct {
		name, sql string
		want      [][]model.Value
	}{
		{"inactive case lanes", `SELECT CASE WHEN active THEN a=b ELSE false END FROM (VALUES(false,'a' COLLATE "C",'b' COLLATE "POSIX"),(true,NULL,'b')) v(active,a,b)`, [][]model.Value{{model.BoolVal(false)}, {model.NullVal(model.TypeBoolean)}}},
		{"strict null lanes", `SELECT a=b FROM (VALUES(NULL::text COLLATE "C",'b' COLLATE "POSIX"),('a',NULL)) v(a,b)`, [][]model.Value{{model.NullVal(model.TypeBoolean)}, {model.NullVal(model.TypeBoolean)}}},
		{"filtered inactive comparison", `SELECT a=b FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) v(a,b) WHERE false`, nil},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			assertCollationSQL(t, scenario.sql, []model.TypeLayout{boolean}, scenario.want)
		})
	}
}

func TestSQLCollationIndeterminateComparisonFailsOnActiveRow(t *testing.T) {
	graph := sqlGraph(t, `SELECT CASE WHEN active THEN a=b ELSE false END FROM
		(VALUES(false,'a' COLLATE "C",'b' COLLATE "POSIX"),(true,'a','b')) v(active,a,b)`, nil)
	env, dir := groupingSetsEnvironment(t, false)
	env.Limits.BatchRows = 1
	portal, err := exec.Open(t.Context(), graph, env)
	if err != nil {
		t.Fatalf("Open rejected a comparison before its active row: %v", err)
	}
	defer closePortal(t, portal)
	var batch columnar.Batch
	if err := portal.Next(t.Context(), &batch); err != nil {
		t.Fatal(err)
	}
	boolean := model.TypeLayout{Type: model.Type{ID: 16, Modifier: -1, Storage: model.TypeBoolean}}
	assertArrayAggregateBatch(t, &batch, []model.TypeLayout{boolean}, [][]model.Value{{model.BoolVal(false)}})
	assertArrayAggregateFailure(t, portal, "42P22")
	groupingSetsReleased(t, env, dir)
}

func TestSQLCollationIndeterminateMinMaxWithoutComparison(t *testing.T) {
	for _, window := range []bool{false, true} {
		for _, filtered := range []bool{false, true} {
			t.Run(fmt.Sprintf("window=%t/filtered=%t", window, filtered), func(t *testing.T) {
				suffix := ""
				if filtered {
					suffix = " FILTER (WHERE false)"
				}
				if window {
					suffix += " OVER ()"
				}
				want := model.StringVal("a")
				if filtered {
					want = model.NullVal(model.TypeString)
				}
				assertCollationSQL(t, `SELECT min(coalesce(a,b))`+suffix+`,max(coalesce(a,b))`+suffix+` FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX")) v(a,b)`,
					[]model.TypeLayout{collationTextLayout(0), collationTextLayout(0)}, [][]model.Value{{want, want}})
			})
		}
	}
}

func TestSQLCollationIndeterminateMinMaxRequireComparison(t *testing.T) {
	for _, function := range []string{"min", "max"} {
		for _, window := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/window=%t", function, window), func(t *testing.T) {
				suffix := ""
				if window {
					suffix = " OVER ()"
				}
				graph := sqlGraph(t, `SELECT `+function+`(coalesce(a,b))`+suffix+` FROM (VALUES('a' COLLATE "C",'b' COLLATE "POSIX"),('c','d')) v(a,b)`, nil)
				env, dir := groupingSetsEnvironment(t, false)
				env.Aggregate.PartitionMemoryBytes = 4096
				portal, err := exec.Open(t.Context(), graph, env)
				if err != nil {
					t.Fatalf("Open rejected min/max before a comparison: %v", err)
				}
				defer closePortal(t, portal)
				assertArrayAggregateFailure(t, portal, "42P22")
				groupingSetsReleased(t, env, dir)
			})
		}
	}
}

func TestSQLCollationActiveSpillCleanup(t *testing.T) {
	for _, window := range []bool{false, true} {
		for _, ending := range []string{"early close", "cancel"} {
			t.Run(fmt.Sprintf("window=%t/%s", window, ending), func(t *testing.T) {
				projection := `min(x COLLATE "POSIX"),max(x COLLATE "POSIX")`
				suffix := " GROUP BY GROUPING SETS ((),(),())"
				if window {
					projection = `min(x COLLATE "POSIX") OVER w,max(x COLLATE "POSIX") OVER w`
					suffix = ` WINDOW w AS (ORDER BY x COLLATE "POSIX" ROWS BETWEEN UNBOUNDED PRECEDING AND UNBOUNDED FOLLOWING)`
				}
				graph := sqlGraph(t, "SELECT "+projection+` FROM (VALUES('b'::text),('A'),('ä')) v(x)`+suffix, nil)
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
				assertArrayAggregateBatch(t, &batch, []model.TypeLayout{collationTextLayout(951), collationTextLayout(951)}, [][]model.Value{{model.StringVal("A"), model.StringVal("ä")}})
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
