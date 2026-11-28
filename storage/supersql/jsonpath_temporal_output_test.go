package supersql

import (
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestJSONPathTemporalOutputPostgres184(t *testing.T) {
	var fixture struct {
		Version int `json:"postgres_version"`
		Cases   []struct {
			ID     string `json:"id"`
			SQL    string `json:"sql"`
			Native struct {
				State   string     `json:"state"`
				Rows    [][]string `json:"rows"`
				Columns int        `json:"columns"`
			} `json:"native"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile("testdata/postgres184/review/temporal.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 180004 || len(fixture.Cases) != 256 {
		t.Fatalf("native fixture identity: version=%d cases=%d", fixture.Version, len(fixture.Cases))
	}
	db := sql.OpenDB(mustConnector(t, t.Context()))
	t.Cleanup(func() { _ = db.Close() })
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	setting := ""
	for _, test := range fixture.Cases {
		parts := strings.Split(test.ID, "/")
		if len(parts) < 4 {
			t.Fatalf("invalid case ID %q", test.ID)
		}
		zone, style := strings.Join(parts[:len(parts)-3], "/"), parts[len(parts)-3]
		next := zone + "/" + style
		if setting != next {
			for _, query := range []string{"SET TimeZone = '" + zone + "'", "SET DateStyle = '" + style + "'"} {
				if _, err := conn.ExecContext(t.Context(), query); err != nil {
					t.Fatal(err)
				}
			}
			setting = next
		}
		t.Run(test.ID, func(t *testing.T) {
			actual := runDiffStep(t.Context(), conn, diffStep{sql: test.SQL, query: true, ordered: true})
			want := diffOutcome{state: test.Native.State, rows: test.Native.Rows, columns: test.Native.Columns}
			if reason := compareDiffOutcome(want, actual, true); reason != "" {
				t.Fatalf("%s: %s; native=%s embedded=%s", test.SQL, reason, want.summary(), actual.summary())
			}
		})
	}
}
