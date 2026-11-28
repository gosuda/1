package supersql

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"
)

type aggregateScopeCase struct {
	ID    string     `json:"id"`
	SQL   string     `json:"sql"`
	Rows  [][]string `json:"rows"`
	State string     `json:"state"`
}

type aggregateScopeFixture struct {
	Version int                  `json:"postgres_version"`
	Setup   []string             `json:"setup"`
	Cases   []aggregateScopeCase `json:"cases"`
}

func readAggregateScopeFixture(t *testing.T) aggregateScopeFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/postgres184/review/aggregate-scope.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture aggregateScopeFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 180004 || len(fixture.Cases) != 50 {
		t.Fatalf("aggregate fixture identity: version=%d cases=%d", fixture.Version, len(fixture.Cases))
	}
	return fixture
}

func TestAggregateSemanticScope(t *testing.T) {
	fixture := readAggregateScopeFixture(t)
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
	for _, statement := range fixture.Setup {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range fixture.Cases {
		t.Run(test.ID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			actual := runDiffStep(ctx, conn, diffStep{sql: test.SQL, query: true, ordered: true})
			if actual.state != test.State {
				t.Fatalf("state=%q, want%q: %s", actual.state, test.State, actual.summary())
			}
			if test.State == "" && !reflect.DeepEqual(actual.rows, test.Rows) {
				t.Fatalf("rows=%v, want%v", actual.rows, test.Rows)
			}
		})
	}
}

func TestAggregateUngroupedOuterDirectArgumentReturns42803(t *testing.T) {
	ctx := t.Context()
	db := sql.OpenDB(mustConnector(t, ctx))
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	actual := runDiffStep(ctx, conn, diffStep{
		sql:   `SELECT (SELECT percentile_disc(o.p) WITHIN GROUP (ORDER BY o.v)) FROM (VALUES (0.5::float8, 10::float8)) o(p, v)`,
		query: true,
	})
	if actual.state != "42803" {
		t.Fatalf("ungrouped outer direct arg state=%q, want 42803 (%s)", actual.state, actual.summary())
	}
}
