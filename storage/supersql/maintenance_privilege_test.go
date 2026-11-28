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

type maintenancePrivilegeStep struct {
	ID            string
	SQL           string
	State         string
	EmbeddedState *string `json:"embedded_state"`
	Query         bool
	Rows          [][]string
}

func TestMaintenancePrivilegesPostgres184(t *testing.T) {
	var fixture struct {
		Version int `json:"postgres_version"`
		Cases   []struct {
			Name  string
			Steps []maintenancePrivilegeStep
		}
	}
	data, err := os.ReadFile("testdata/postgres184/review/maintenance-privileges.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 180004 || len(fixture.Cases) != 4 {
		t.Fatalf("maintenance fixture identity: version=%d cases=%d", fixture.Version, len(fixture.Cases))
	}
	total := 0
	for _, test := range fixture.Cases {
		t.Run(test.Name, func(t *testing.T) {
			database := sql.OpenDB(mustConnector(t, t.Context()))
			t.Cleanup(func() {
				if err := database.Close(); err != nil {
					t.Error(err)
				}
			})
			connection, err := database.Conn(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := connection.Close(); err != nil {
					t.Error(err)
				}
			})
			for _, step := range test.Steps {
				ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
				actual := runDiffStep(ctx, connection, diffStep{sql: step.SQL, query: step.Query, ordered: true})
				cancel()
				want := step.State
				if step.EmbeddedState != nil {
					// PostgreSQL warns and skips explicit ANALYZE without a
					// grant. Keep the existing embedded denial policy.
					if test.Name != "direct" || step.ID != "revoked-analyze" || *step.EmbeddedState != "42501" || step.State != "" {
						t.Fatalf("unexpected native-policy exception: %s/%s", test.Name, step.ID)
					}
					want = *step.EmbeddedState
				}
				if actual.state != want {
					t.Errorf("%s: state=%q want%q: %s", step.ID, actual.state, want, actual.summary())
				}
				if step.Rows != nil && !reflect.DeepEqual(actual.rows, step.Rows) {
					t.Errorf("%s: rows=%v want%v", step.ID, actual.rows, step.Rows)
				}
				total++
			}
		})
	}
	if total != 97 {
		t.Fatalf("maintenance contracts=%d, want97", total)
	}
}
