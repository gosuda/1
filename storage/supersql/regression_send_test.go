package supersql

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegressionEchoStandaloneSendPostgres184(t *testing.T) {
	var fixture struct {
		Version int    `json:"postgres_version"`
		Source  string `json:"source"`
		Output  string `json:"output"`
		Cases   []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("testdata/postgres184/review/standalone-send.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 180004 || len(fixture.Cases) != 8 {
		t.Fatalf("native send fixture identity: version=%d cases=%d", fixture.Version, len(fixture.Cases))
	}
	corpus := t.TempDir()
	if err := os.Mkdir(filepath.Join(corpus, "expected"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corpus, "expected", "send.out"), []byte(fixture.Output), 0600); err != nil {
		t.Fatal(err)
	}
	statements, err := splitRegressionSQL(fixture.Source)
	if err != nil {
		t.Fatal(err)
	}
	matches := expectedErrorSet(t, corpus, "send", statements)
	states := make(map[string]string, len(fixture.Cases))
	for _, test := range fixture.Cases {
		states[test.ID] = test.State
	}
	last, count := -1, 0
	for i, statement := range statements {
		if statement.kind == regSQL {
			last = i
			continue
		}
		fields := strings.Fields(statement.text)
		if len(fields) != 4 || fields[0] != "\\echo" || fields[1] != "SEND_STATE" {
			continue
		}
		state, found := states[fields[2]]
		if last < 0 || !found {
			t.Fatalf("native send marker lacks its SQL: %q", statement.text)
		}
		if matches[last] != (state != "00000") {
			t.Errorf("%s: matched rejection=%t, native state=%s", fields[2], matches[last], state)
		}
		count++
	}
	if count != len(fixture.Cases) {
		t.Fatalf("native send markers=%d, want%d", count, len(fixture.Cases))
	}
	source, err := os.ReadFile("testdata/postgres184/sql/errors.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err = splitRegressionSQL(string(source))
	if err != nil {
		t.Fatal(err)
	}
	matches = expectedErrorSet(t, "testdata/postgres184", "errors", statements)
	found := false
	for i, statement := range statements {
		if statement.line == 285 && statement.kind == regSQL && statement.text == "CREATE TABLE" {
			found = true
			if !matches[i] {
				t.Error("vendored CREATE TABLE followed by standalone send lost its native rejection")
			}
		}
	}
	if !found {
		t.Fatal("vendored standalone send witness is missing")
	}
}

func TestRegressionEchoStandaloneSendBoundaries(t *testing.T) {
	// These are matcher-boundary controls, not additional PostgreSQL claims.
	for _, test := range []struct {
		name   string
		output string
		client string
		want   bool
	}{
		{"physical-line", "SELECT missing_send_probe()\n\\g\nERROR: rejected\n", "\\g", true},
		{"crlf-indent", "SELECT missing_send_probe()\r\n \t\\g \t\r\nERROR: rejected\r\n", "\\g", true},
		{"inline-is-not-standalone", "SELECT missing_send_probe() \\g\nERROR: rejected\n", "\\g", false},
		{"gset-is-not-send", "SELECT missing_send_probe()\n\\gset\nERROR: rejected\n", "\\g", false},
		{"gexec-is-not-send", "SELECT missing_send_probe()\n\\gexec\nERROR: rejected\n", "\\g", false},
		{"g-prefix-is-not-send", "SELECT missing_send_probe()\n\\get\nERROR: rejected\n", "\\g", false},
		{"send-arguments-outside-scope", "SELECT missing_send_probe()\n\\g output.txt\nERROR: rejected\n", "\\g", false},
		{"non-send-source", "SELECT missing_send_probe()\n\\g\nERROR: rejected\n", "\\p", false},
		{"longer-sql-is-not-echo", "SELECT missing_send_probe() + 1\n\\g\nERROR: rejected\n", "\\g", false},
		{"diagnostic-is-not-execution", "LINE 1: SELECT missing_send_probe()\n\\g\nERROR: repeated\n", "\\g", false},
		{"semicolon-remains-valid", "SELECT missing_send_probe();\nERROR: rejected\n", "\\g", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			corpus := t.TempDir()
			if err := os.Mkdir(filepath.Join(corpus, "expected"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(corpus, "expected", "boundary.out"), []byte(test.output), 0600); err != nil {
				t.Fatal(err)
			}
			statements := []regStatement{
				{text: "SELECT missing_send_probe()", kind: regSQL},
				{text: test.client, kind: regClientCommand},
			}
			got := expectedErrorSet(t, corpus, "boundary", statements)
			if got[0] != test.want || got[1] {
				t.Fatalf("matched SQL/client=%v, want[%t false]", got, test.want)
			}
		})
	}
}
