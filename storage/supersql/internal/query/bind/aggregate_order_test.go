package bind_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestAggregateOrderRejectsNonargumentDistinctKeys(t *testing.T) {
	for _, sql := range []string{
		`SELECT sum(DISTINCT x ORDER BY y) FROM (VALUES(1,2)) t(x,y)`,
		`SELECT sum(DISTINCT x ORDER BY x+1) FROM (VALUES(1)) t(x)`,
		`SELECT sum(DISTINCT x ORDER BY 1) FROM (VALUES(1)) t(x)`,
		`SELECT sum(DISTINCT a.x ORDER BY b.x) FROM (VALUES(1)) a(x), (VALUES(2)) b(x)`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := bindSQL(t, sql, nil, nil)
			requireCode(t, err, "42P10")
			var diagnostic *model.Error
			start := strings.Index(sql, "ORDER BY ") + len("ORDER BY ")
			end := start + strings.Index(sql[start:], ")")
			if !errors.As(err, &diagnostic) || diagnostic.Position <= start || diagnostic.Position > end {
				t.Fatalf("diagnostic = %#v, want position within aggregate sort expression", diagnostic)
			}
		})
	}
}

func TestAggregateOrderRejectsNestedCalls(t *testing.T) {
	for _, scenario := range []struct{ name, sql, code string }{
		{"aggregate", `SELECT sum(x ORDER BY max(x)) FROM (VALUES(1)) t(x)`, "42803"},
		{"window", `SELECT sum(x ORDER BY row_number() OVER()) FROM (VALUES(1)) t(x)`, "42P20"},
		{"grouping", `SELECT sum(x ORDER BY GROUPING(x)) FROM (VALUES(1)) t(x) GROUP BY x`, "42803"},
		{"output alias", `SELECT sum(x ORDER BY total) AS total FROM (VALUES(1)) t(x)`, "42703"},
		{"window aggregate ordering", `SELECT sum(x ORDER BY x) OVER() FROM (VALUES(1)) t(x)`, "0A000"},
		{"matching window argument", `SELECT sum(row_number() OVER() ORDER BY row_number() OVER()) FROM (VALUES(1)) t(x)`, "42P20"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := bindSQL(t, scenario.sql, nil, nil)
			requireCode(t, err, scenario.code)
		})
	}
}

func boundOrderedAggregate(t *testing.T, graph *ir.Graph) (ir.RelID, ir.AggregateCall) {
	t.Helper()
	for id := ir.RelID(1); uint32(id) <= graph.RelCount(); id++ {
		if call, ok := graph.Aggregate(id, 0); ok && call.OrderCount() != 0 {
			return id, call
		}
	}
	t.Fatal("missing ordered aggregate")
	return 0, ir.AggregateCall{}
}

func TestAggregateOrderBindsComputedCountKeys(t *testing.T) {
	statement, err := bindSQL(t, `SELECT count(1 ORDER BY x+1 DESC NULLS LAST, txt ASC NULLS FIRST, 99 USING >) FROM (VALUES(1,'a'::text)) t(x,txt)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	id, call := boundOrderedAggregate(t, graph)
	if call.OrderCount() != 3 {
		t.Fatalf("order keys = %d; want 3", call.OrderCount())
	}
	for index, want := range []struct {
		direction ir.Direction
		nulls     ir.NullOrder
		collation model.CollationID
	}{
		{ir.Descending, ir.NullsLast, 0},
		{ir.Ascending, ir.NullsFirst, 100},
		{ir.Descending, ir.NullsFirst, 0},
	} {
		key, ok := graph.AggregateOrder(id, 0, uint32(index))
		unresolved := !ok || key.Expr == 0 || key.Operator == 0
		wrongOrder := key.Direction != want.direction || key.Nulls != want.nulls
		if unresolved || wrongOrder || key.Collation != want.collation {
			t.Errorf("order key %d = %+v, want direction %v, nulls %v, collation %v", index, key, want.direction, want.nulls, want.collation)
		}
	}
}

func TestAggregateOrderDistinctReusesArgumentTargets(t *testing.T) {
	for _, sql := range []string{
		`SELECT sum(DISTINCT x ORDER BY t.x) FROM (VALUES(1)) t(x)`,
		`SELECT sum(DISTINCT t.x+1 ORDER BY x+1) FROM (VALUES(1)) t(x)`,
		`SELECT sum(DISTINCT 2 ORDER BY 2)`,
		`SELECT sum(DISTINCT random() ORDER BY random())`,
		`SELECT sum(DISTINCT CASE random() WHEN 0 THEN x ELSE 2 END ORDER BY CASE random() WHEN 0 THEN t.x ELSE 2 END) FROM (VALUES(1)) t(x)`,
	} {
		t.Run(sql, func(t *testing.T) {
			statement, err := bindSQL(t, sql, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			graph := statement.Graph()
			id, call := boundOrderedAggregate(t, graph)
			if !call.Distinct {
				t.Fatal("aggregate lost DISTINCT")
			}
			argument, ok := graph.AggregateArgument(id, 0, 0)
			key, ordered := graph.AggregateOrder(id, 0, 0)
			if !ok || !ordered || key.Expr != argument {
				t.Fatalf("order expression = %d, argument = %d; want one evaluation target", key.Expr, argument)
			}
		})
	}
}

func TestAggregateOrderPreservesVolatileKeyEffects(t *testing.T) {
	statement, err := bindSQL(t, `SELECT count(1 ORDER BY random(), random() DESC)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	id, call := boundOrderedAggregate(t, graph)
	if call.Effects.Volatility != ir.Volatile || statement.Effects().Volatility != ir.Volatile {
		t.Fatalf("aggregate effects = %+v, statement effects = %+v; want volatile", call.Effects, statement.Effects())
	}
	first, _ := graph.AggregateOrder(id, 0, 0)
	second, _ := graph.AggregateOrder(id, 0, 1)
	if first.Expr != second.Expr {
		t.Fatalf("repeated volatile sort targets = %d, %d; want one evaluation target", first.Expr, second.Expr)
	}
}
