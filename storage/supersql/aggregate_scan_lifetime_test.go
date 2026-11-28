package supersql

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

// Each scalar-subquery invocation owns its scan plan. Closing the invocation
// must return that plan to the shared reader's bounded metadata allowance.
func TestAggregateScanPlanLifetime(t *testing.T) {
	ctx := t.Context()
	db := sql.OpenDB(mustConnector(t, ctx))
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	})
	for _, query := range []string{
		"CREATE TABLE tenk1(unique1 integer, unique2 integer)",
		"INSERT INTO tenk1 SELECT i, i FROM generate_series(0, 99) AS s(i)",
		"CREATE TABLE onek(unique1 integer)",
		"INSERT INTO onek SELECT i FROM generate_series(0, 99) AS s(i)",
	} {
		if _, err := conn.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name  string
		query string
		want  string
		state string
	}{
		{"direct-filter", "SELECT sum(unique1) FILTER (WHERE unique1 < 100) FROM tenk1", "4950", ""},
		{"original-filter", "SELECT sum(unique1) FILTER (WHERE\n  unique1 IN (SELECT unique1 FROM onek WHERE unique1 < 100)) FROM tenk1", "4950", ""},
		{"all-matches", "SELECT count(*) FILTER (WHERE unique1 IN (SELECT unique1 FROM onek)) FROM tenk1", "100", ""},
		{"no-matches", "SELECT count(*) FILTER (WHERE unique1 NOT IN (SELECT unique1 FROM onek)) FROM tenk1", "0", ""},
		{"null-preserving-filter", "SELECT sum(unique1) FILTER (WHERE unique1 IN (SELECT unique1 FROM onek WHERE unique1 < 50 UNION ALL SELECT NULL::integer)) FROM tenk1", "1225", ""},
		{"correlated-exists", "SELECT sum(o.unique1) FILTER (WHERE EXISTS (SELECT 1 FROM onek i WHERE i.unique1 = o.unique1 AND i.unique1 < 50)) FROM tenk1 o", "1225", ""},
		{"scalar-sum", "SELECT sum((SELECT i.unique1 FROM onek i WHERE i.unique1 = o.unique1)) FROM tenk1 o", "4950", ""},
		{"early-exists", "SELECT count(*) FILTER (WHERE EXISTS (SELECT 1 FROM onek i WHERE i.unique1 = tenk1.unique1 LIMIT 1)) FROM tenk1", "100", ""},
		{"division-error", "SELECT sum(unique1) FILTER (WHERE unique1 IN (SELECT unique1 / (unique1 - 1) FROM onek)) FROM tenk1", "", "22012"},
		{"after-error", "SELECT sum(unique1) FILTER (WHERE unique1 IN (SELECT unique1 FROM onek)) FROM tenk1", "4950", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			actual := runDiffStep(ctx, conn, diffStep{sql: test.query, query: true, ordered: true})
			if actual.state != test.state {
				t.Fatalf("state = %q, want %q: %s", actual.state, test.state, actual.summary())
			}
			if test.state == "" && (len(actual.rows) != 1 || len(actual.rows[0]) != 1 || actual.rows[0][0] != test.want) {
				t.Fatalf("result = %s, want %s", actual.summary(), test.want)
			}
		})
	}
}
