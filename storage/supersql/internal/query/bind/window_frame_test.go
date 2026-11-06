package bind_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/ir"
)

func TestBindNamedWindowInheritance(t *testing.T) {
	for _, sql := range []string{
		`SELECT sum(v) OVER w FROM (VALUES(1,2)) t(k,v) WINDOW w AS (ORDER BY k ROWS BETWEEN 1 PRECEDING AND CURRENT ROW)`,
		`SELECT sum(v) OVER (w ORDER BY k ROWS CURRENT ROW) FROM (VALUES(1,2,3)) t(p,k,v) WINDOW w AS (PARTITION BY p)`,
		`SELECT sum(v) OVER w2 FROM (VALUES(1,2,3)) t(p,k,v) WINDOW w1 AS (PARTITION BY p), w2 AS (w1 ORDER BY k RANGE BETWEEN 1 PRECEDING AND CURRENT ROW)`,
	} {
		if _, err := bindSQL(t, sql, nil, nil); err != nil {
			t.Fatalf("bind %q: %v", sql, err)
		}
	}
}

func TestBindNamedWindowInheritanceErrors(t *testing.T) {
	for _, scenario := range []struct {
		name, sql, code, message string
	}{
		{"duplicate", `SELECT row_number() OVER w FROM (VALUES(1)) t(k) WINDOW w AS (), w AS ()`, "42P20", `window "w" is already defined`},
		{"forward reference", `SELECT row_number() OVER w2 FROM (VALUES(1)) t(k) WINDOW w2 AS (w1), w1 AS ()`, "42704", `window "w1" does not exist`},
		{"partition override", `SELECT row_number() OVER w2 FROM (VALUES(1)) t(k) WINDOW w1 AS (PARTITION BY k), w2 AS (w1 PARTITION BY k)`, "42P20", `cannot override PARTITION BY clause of window "w1"`},
		{"order override", `SELECT row_number() OVER w2 FROM (VALUES(1)) t(k) WINDOW w1 AS (ORDER BY k), w2 AS (w1 ORDER BY k)`, "42P20", `cannot override ORDER BY clause of window "w1"`},
		{"copy framed definition", `SELECT row_number() OVER (w1) FROM (VALUES(1)) t(k) WINDOW w1 AS (ROWS CURRENT ROW)`, "42P20", `cannot copy window "w1" because it has a frame clause`},
		{"extend framed definition", `SELECT row_number() OVER w2 FROM (VALUES(1)) t(k) WINDOW w1 AS (ROWS CURRENT ROW), w2 AS (w1)`, "42P20", `cannot copy window "w1" because it has a frame clause`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			_, err := bindSQL(t, scenario.sql, nil, nil)
			var diagnostic *model.Error
			if !errors.As(err, &diagnostic) || diagnostic.Code != scenario.code || diagnostic.Message != scenario.message {
				t.Fatalf("error = %#v, want %s %q", diagnostic, scenario.code, scenario.message)
			}
		})
	}
}

func TestBindTemporalRangeCarriesExactInRangeSignature(t *testing.T) {
	statement, err := bindSQL(t, `SELECT sum(v) OVER (ORDER BY k USING > RANGE BETWEEN interval '1 day' PRECEDING AND CURRENT ROW) FROM (VALUES('2024-03-10'::date,1::int4)) t(k,v)`, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	graph := statement.Graph()
	for id := ir.RelID(1); uint32(id) <= graph.RelCount(); id++ {
		relation, _ := graph.Rel(id)
		if relation.Kind != ir.Window {
			continue
		}
		if relation.OrderCount() != 1 {
			t.Fatalf("window order count = %d", relation.OrderCount())
		}
		order, _ := graph.Order(id, 0)
		if order.Direction != ir.Descending {
			t.Fatalf("USING > direction = %v", order.Direction)
		}
		signature, ok := graph.Signature(relation.Frame.Start.InRange)
		if !ok || signature.Function != 4133 || signature.ParameterCount() != 5 {
			t.Fatalf("in_range signature = %+v, found=%v", signature, ok)
		}
		var got []model.TypeID
		for index := range signature.ParameterCount() {
			layout, _ := graph.SignatureParameter(relation.Frame.Start.InRange, index)
			typ, _ := graph.Type(layout)
			got = append(got, typ.ID)
		}
		if want := []model.TypeID{1082, 1082, 1186, 16, 16}; !reflect.DeepEqual(got, want) {
			t.Fatalf("in_range parameters = %v, want %v", got, want)
		}
		return
	}
	t.Fatal("missing window relation")
}
