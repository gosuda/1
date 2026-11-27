//go:build !linux

package neosepia

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestArchivePublishFailsClosedWithoutNoReplace(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		directory := t.TempDir()
		root, err := os.OpenRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := root.Close(); err != nil {
				t.Error(err)
			}
		})
		const pending, final = ".00000000000000000001.wal.pending", "00000000000000000001.wal"
		if err := os.WriteFile(filepath.Join(directory, pending), []byte("pending"), 0600); err != nil {
			t.Fatal(err)
		}
		if occupied {
			if err := os.WriteFile(filepath.Join(directory, final), []byte("original"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		previous := archiveLink
		t.Cleanup(func() { archiveLink = previous })
		archiveLink = func(*os.Root, string, string) error { return errors.ErrUnsupported }
		err = publishArchivedFile(root, pending, final)
		if err == nil {
			t.Fatal("publication without a no-replace operation succeeded, want failure")
		}
		if errors.Is(err, os.ErrExist) {
			t.Fatalf("unverifiable publication = %v, must not report already exists", err)
		}
		got, readErr := os.ReadFile(filepath.Join(directory, final))
		if occupied {
			if readErr != nil || string(got) != "original" {
				t.Fatalf("existing publication changed: %q, %v", got, readErr)
			}
		} else if !errors.Is(readErr, os.ErrNotExist) {
			t.Fatalf("failed publication created the destination: %q, %v", got, readErr)
		}
		if got, err := os.ReadFile(filepath.Join(directory, pending)); err != nil || string(got) != "pending" {
			t.Fatalf("failed publication changed the pending file: %q, %v", got, err)
		}
	}
}
