package supersql

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIdleCatalogDoesNotRepublishState proves an idle database does not rewrite
// database.state. The index-job scheduler polls durable state at PollInterval;
// polling must not certify a new read fence, because every certified fence
// republishes database.state with two fsyncs. Before the poller used the closed
// frontier this test observed roughly thirty republications in 300 ms.
func TestIdleCatalogDoesNotRepublishState(t *testing.T) {
	ctx := t.Context()
	directory := testDirectory(t)
	database, err := Open(ctx, OpenOptions{Directory: directory, Clock: newTestClock()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	path := filepath.Join(directory, "database.state")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database.state: %v", err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database.state: %v", err)
	}

	time.Sleep(300 * time.Millisecond)

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database.state after idle: %v", err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat database.state after idle: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("database.state republished while idle: %d bytes -> %d bytes", len(before), len(after))
	}
	if !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatalf("database.state rewritten while idle: mtime %s -> %s", beforeInfo.ModTime(), afterInfo.ModTime())
	}
}
