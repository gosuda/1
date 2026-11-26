package bind_test

import (
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/query/ir"
	"github.com/gosuda/supersql-alpha/internal/zpgparse"
)

func TestOrderedSetSeparatesDirectAndAggregatedArguments(t *testing.T) {
	statement, err := bindSQL(t, `SELECT percentile_disc(g / 10.0) WITHIN GROUP (ORDER BY x DESC NULLS LAST) FILTER (WHERE x <> 0) FROM (VALUES (2, 1), (2, 3)) t(g, x) GROUP BY g`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	id, call := boundOrderedAggregate(t, graph)
	if call.Function != 3972 || call.DirectArgumentCount() != 1 || call.ArgumentCount() != 1 || call.OrderCount() != 1 || call.Filter == 0 {
		t.Fatalf("ordered aggregate=%+v", call)
	}
	direct, directOK := graph.AggregateDirectArgument(id, 0, 0)
	argument, argumentOK := graph.AggregateArgument(id, 0, 0)
	order, orderOK := graph.AggregateOrder(id, 0, 0)
	if !directOK || !argumentOK || !orderOK || direct == argument || order.Expr != argument || order.Direction != ir.Descending || order.Nulls != ir.NullsLast {
		t.Fatalf("direct=%d/%v argument=%d/%v order=%+v/%v", direct, directOK, argument, argumentOK, order, orderOK)
	}
}

func TestHypotheticalSetUnifiesArgumentPairs(t *testing.T) {
	statement, err := bindSQL(t, `SELECT rank(1::smallint, 'm') WITHIN GROUP (ORDER BY x::bigint, y::varchar) FROM (VALUES (1, 'a')) t(x, y)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	id, call := boundOrderedAggregate(t, graph)
	if call.Function != 3986 || call.DirectArgumentCount() != 2 || call.ArgumentCount() != 2 || call.OrderCount() != 2 {
		t.Fatalf("hypothetical aggregate=%+v", call)
	}
	for i := uint32(0); i < 2; i++ {
		direct, _ := graph.AggregateDirectArgument(id, 0, i)
		argument, _ := graph.AggregateArgument(id, 0, i)
		directScalar, _ := graph.Scalar(direct)
		argumentScalar, _ := graph.Scalar(argument)
		if directScalar.Layout != argumentScalar.Layout {
			t.Fatalf("pair %d layouts=%d,%d, want identical", i, directScalar.Layout, argumentScalar.Layout)
		}
		order, _ := graph.AggregateOrder(id, 0, i)
		if order.Expr != argument {
			t.Fatalf("pair %d order=%d, argument=%d", i, order.Expr, argument)
		}
	}
}

func TestOrderedSetRejectsInvalidSyntaxAndScope(t *testing.T) {
	for _, scenario := range []struct {
		name string
		sql  string
		code string
	}{
		{"window", `SELECT percentile_disc(.5) WITHIN GROUP (ORDER BY x) OVER () FROM (VALUES (1)) t(x)`, "0A000"},
		{"ungrouped direct", `SELECT percentile_disc(g / 10.0) WITHIN GROUP (ORDER BY x) FROM (VALUES (2, 1)) t(g, x)`, "42803"},
		{"hypothetical arity", `SELECT rank(1, 2) WITHIN GROUP (ORDER BY x) FROM (VALUES (1)) t(x)`, "42883"},
		{"ordinary aggregate", `SELECT sum(1) WITHIN GROUP (ORDER BY x) FROM (VALUES (1)) t(x)`, "42883"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := bindSQL(t, scenario.sql, nil, nil)
			requireCode(t, err, scenario.code)
		})
	}
	// DISTINCT and VARIADIC are rejected by the grammar's func_expr rule before
	// any binder runs (gram.y: func_application within_group_clause), so the
	// 42601 belongs to the parser, not to binding.
	for _, sql := range []string{
		`SELECT percentile_disc(DISTINCT .5) WITHIN GROUP (ORDER BY x) FROM (VALUES (1)) t(x)`,
		`SELECT percentile_disc(VARIADIC ARRAY[.5]) WITHIN GROUP (ORDER BY x) FROM (VALUES (1)) t(x)`,
	} {
		t.Run(sql, func(t *testing.T) {
			_, err := zpgparse.Parse(sql)
			var state interface{ SQLState() string }
			if !errors.As(err, &state) || state.SQLState() != "42601" {
				t.Fatalf("parse error = %v, want SQLSTATE 42601", err)
			}
		})
	}
}
