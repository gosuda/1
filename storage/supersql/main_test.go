package supersql

import (
	"context"
	"os"
	"sync"
	"testing"
)

var (
	templateOnce sync.Once
	templateDir  string
	templateErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
	}
	os.Exit(code)
}

// testDirectory returns a fresh database directory cloned from one bootstrapped
// template, so each test pays a directory copy (~5 ms) and a reopen (~0.1 s)
// instead of a bootstrap (5-9 s). The clone carries the template's fence.secret,
// so the reopened database verifies the same fence identity.
func testDirectory(t testing.TB) string {
	t.Helper()
	templateOnce.Do(func() {
		dir, err := os.MkdirTemp("", "supersql-template-*")
		if err != nil {
			templateErr = err
			return
		}
		templateDir = dir
		database, err := Open(context.Background(), OpenOptions{Directory: dir, Clock: newTestClock()})
		if err != nil {
			templateErr = err
			return
		}
		templateErr = database.Close()
	})
	if templateErr != nil {
		t.Fatalf("bootstrap test template: %v", templateErr)
	}
	directory := t.TempDir()
	if err := os.CopyFS(directory, os.DirFS(templateDir)); err != nil {
		t.Fatalf("clone test template: %v", err)
	}
	return directory
}
