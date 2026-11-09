package supersql

import (
	"bytes"
	"strings"
	"testing"
)

// TestCopyCallerStreams proves COPY FROM/TO STDIN/STDOUT move records through
// caller-owned streams, server-side COPY stays excluded, and a malformed
// record fails the statement atomically.
func TestCopyCallerStreams(t *testing.T) {
	ctx := t.Context()
	database, err := Open(ctx, OpenOptions{Directory: testDirectory(t), Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()

	execSQL(t, ctx, database, `CREATE TABLE ct (id bigint, note text)`)

	outcome, err := database.CopyFrom(ctx, `COPY ct FROM STDIN`,
		strings.NewReader("1\tone\n2\ttwo\n"), CopyOptions{MaxRecordBytes: 1 << 20})
	if err != nil {
		t.Fatalf("COPY FROM: %v", err)
	}
	if outcome.Tag != "COPY" || outcome.RowsAffected != 2 {
		t.Fatalf("COPY FROM outcome = %q/%d, want COPY/2", outcome.Tag, outcome.RowsAffected)
	}

	var buf bytes.Buffer
	outcome, err = database.CopyTo(ctx, `COPY ct TO STDOUT`, &buf, CopyOptions{MaxRecordBytes: 1 << 20})
	if err != nil {
		t.Fatalf("COPY TO: %v", err)
	}
	if outcome.Tag != "COPY" || buf.String() != "1\tone\n2\ttwo\n" {
		t.Fatalf("COPY TO outcome = %q %q, want COPY round-trip", outcome.Tag, buf.String())
	}

	// Server-side COPY is the server_io exclusion, not a stream request.
	for _, statement := range []string{
		`COPY ct FROM '/tmp/data'`,
		`COPY ct TO '/tmp/data'`,
		`COPY ct FROM PROGRAM 'cat /tmp/data'`,
	} {
		_, err = database.Exec(ctx, statement, 0)
		if code := sqlStateOf(err); code != "0A000" {
			t.Fatalf("%q: expected SQLSTATE 0A000, got %q (%v)", statement, code, err)
		}
	}

	// Direction mismatch and a missing bound stream are typed errors.
	if _, err = database.CopyTo(ctx, `COPY ct FROM STDIN`, &buf, CopyOptions{MaxRecordBytes: 1 << 20}); sqlStateOf(err) != "22023" {
		t.Fatalf("wrong-direction CopyTo: expected 22023, got %q (%v)", sqlStateOf(err), err)
	}
	if _, err = database.CopyFrom(ctx, `COPY ct FROM STDIN`, strings.NewReader(""), CopyOptions{}); sqlStateOf(err) != "22023" {
		t.Fatalf("unbounded CopyFrom: expected 22023, got %q (%v)", sqlStateOf(err), err)
	}

	// A malformed record fails the whole statement: no partial rows survive.
	_, err = database.CopyFrom(ctx, `COPY ct FROM STDIN`,
		strings.NewReader("3\tthree\nnotanumber\tbad\n"), CopyOptions{MaxRecordBytes: 1 << 20})
	if err == nil {
		t.Fatal("malformed COPY FROM record succeeded")
	}
	if got := queryValue(t, ctx, database, `SELECT count(*) FROM ct`); got.IntVal != 2 {
		t.Fatalf("rows after failed COPY = %d, want 2", got.IntVal)
	}
}
