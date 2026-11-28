package supersql

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegressionEchoPostgres184(t *testing.T) {
	var fixture struct {
		Version int    `json:"postgres_version"`
		Source  string `json:"source"`
		Cases   []struct {
			ID    string `json:"id"`
			SQL   string `json:"sql"`
			State string `json:"state"`
		} `json:"cases"`
	}
	data, err := os.ReadFile("testdata/postgres184/review/echo-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 180004 || len(fixture.Cases) != 15 {
		t.Fatalf("native fixture identity: version=%d cases=%d", fixture.Version, len(fixture.Cases))
	}
	statements := make([]regStatement, len(fixture.Cases))
	for i, test := range fixture.Cases {
		statements[i] = regStatement{text: test.SQL, kind: regSQL}
	}
	matches := expectedErrorSet(t, "testdata/postgres184/review", "echo", statements)
	for i, test := range fixture.Cases {
		if want := test.State != "00000"; matches[i] != want {
			t.Errorf("%s: matched rejection=%t, native state=%s", test.ID, matches[i], test.State)
		}
	}
	// The runtime consumes every statement, including the setup and teardown
	// between repeated SQL. Keep that complete native input as a separate
	// contract from the selected-record diagnostic-copy control above.
	allStatements, err := splitRegressionSQL(fixture.Source)
	if err != nil {
		t.Fatal(err)
	}
	allMatches := expectedErrorSet(t, "testdata/postgres184/review", "echo", allStatements)
	states := make(map[string]string, len(fixture.Cases))
	for _, test := range fixture.Cases {
		states[test.ID] = test.State
	}
	last, count := -1, 0
	for i, statement := range allStatements {
		if statement.kind == regSQL {
			last = i
			continue
		}
		fields := strings.Fields(statement.text)
		if last < 0 || len(fields) < 2 || fields[0] != "\\echo" {
			continue
		}
		state, found := states[fields[1]]
		if !found {
			t.Fatalf("unknown native echo marker %s", fields[1])
		}
		count++
		if allMatches[last] != (state != "00000") {
			t.Errorf("complete native input %s: matched rejection=%t, state=%s", fields[1], allMatches[last], state)
		}
	}
	if count != 15 {
		t.Fatalf("complete native echo covered %d markers, want15", count)
	}
	source, err := os.ReadFile("testdata/postgres184/sql/plpgsql.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements, err = splitRegressionSQL(string(source))
	if err != nil {
		t.Fatal(err)
	}
	matches = expectedErrorSet(t, "testdata/postgres184", "plpgsql", statements)
	found := false
	for i, statement := range statements {
		if statement.line == 2697 {
			found = true
			if !matches[i] {
				t.Error("vendored strict_multi_assignment rejection was not recognized")
			}
		}
	}
	if !found {
		t.Fatal("vendored strict_multi_assignment fixture missing")
	}
}

func TestRegressionEchoKeepsLiteralIdentity(t *testing.T) {
	corpus := t.TempDir()
	if err := os.Mkdir(filepath.Join(corpus, "expected"), 0700); err != nil {
		t.Fatal(err)
	}
	// These are matcher controls, not PostgreSQL outcome claims. Distinct
	// literal whitespace must not steal an earlier statement's diagnostic.
	output := "SELECT 'a  b';\nERROR: first literal\nSELECT 'a b';\n a b\n"
	if err := os.WriteFile(filepath.Join(corpus, "expected", "identity.out"), []byte(output), 0600); err != nil {
		t.Fatal(err)
	}
	got := expectedErrorSet(t, corpus, "identity", []regStatement{{text: "SELECT 'a b'", kind: regSQL}})
	if got[0] {
		t.Fatal("different quoted literal acquired an earlier ERROR")
	}
}

func TestRegressionEchoKeepsEncodingBytes(t *testing.T) {
	corpus := t.TempDir()
	if err := os.Mkdir(filepath.Join(corpus, "expected"), 0700); err != nil {
		t.Fatal(err)
	}
	output := "SELECT '\xe1bc';\nERROR: first encoded literal\nSELECT '\xf1bc';\n encoded result\n"
	if err := os.WriteFile(filepath.Join(corpus, "expected", "encoding.out"), []byte(output), 0600); err != nil {
		t.Fatal(err)
	}
	got := expectedErrorSet(t, corpus, "encoding", []regStatement{{text: "SELECT '\xf1bc'", kind: regSQL}})
	if got[0] {
		t.Fatal("different non-UTF8 bytes acquired an earlier ERROR")
	}
	got = expectedErrorSet(t, corpus, "encoding", []regStatement{{text: "SELECT '\xe1bc'", kind: regSQL}})
	if !got[0] {
		t.Fatal("exact non-UTF8 bytes lost their ERROR")
	}
}

func TestRegressionEchoIgnoresAlignedResults(t *testing.T) {
	// Captured from PostgreSQL18.4 in private native proofs45914618891
	// and45915690117. Result rows and column names can contain complete
	// SQL, while indented echoes and non-result dividers remain executable.
	for _, fixture := range []struct {
		name string
		want []bool
	}{
		{"result-row-echo", []bool{false, true}},
		{"header-echo", []bool{false, true}},
		{"layout-controls", []bool{true, true}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			input, err := os.ReadFile(filepath.Join("testdata/postgres184/review", fixture.name+".sql"))
			if err != nil {
				t.Fatal(err)
			}
			statements, err := splitRegressionSQL(string(input))
			if err != nil {
				t.Fatal(err)
			}
			got := expectedErrorSet(t, "testdata/postgres184/review", fixture.name, statements)
			count := 0
			for i, statement := range statements {
				if statement.kind != regSQL {
					continue
				}
				if count >= len(fixture.want) {
					t.Fatalf("unexpected SQL statement%d", count)
				}
				if got[i] != fixture.want[count] {
					t.Errorf("statement%d: rejection=%t, want%t", count, got[i], fixture.want[count])
				}
				count++
			}
			if count != len(fixture.want) {
				t.Fatalf("native fixture contains%d SQL statements, want%d", count, len(fixture.want))
			}
		})
	}
}
