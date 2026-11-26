//go:build unix

package sqlite3_test

import (
	"os"
	"testing"

	"gosuda.org/sqlite3/sqlite3/sqlite3srv"
)

func TestMain(m *testing.M) {
	code := m.Run()
	sqlite3srv.Cleanup()
	os.Exit(code)
}
