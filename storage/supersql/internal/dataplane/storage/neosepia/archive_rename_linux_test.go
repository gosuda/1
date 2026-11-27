package neosepia

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArchiveRenameRejectsLateDestination(t *testing.T) {
	for _, final := range []string{"00000000000000000001.wal", "00000000000000000001.meta"} {
		t.Run(final, func(t *testing.T) {
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
			pending := "." + final + ".pending"
			if err := os.WriteFile(filepath.Join(directory, pending), []byte("second publisher"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := root.Lstat(final); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("destination initially exists: %v", err)
			}
			// A competing publisher wins after an absence check. The final
			// filesystem operation must still refuse to replace its bytes.
			if err := os.WriteFile(filepath.Join(directory, final), []byte("first publisher"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := renameArchivedFileNoReplace(root, pending, final); !errors.Is(err, os.ErrExist) {
				t.Fatalf("publish after competitor = %v, want already exists", err)
			}
			got, err := os.ReadFile(filepath.Join(directory, final))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "first publisher" {
				t.Fatalf("published bytes changed to %q", got)
			}
			got, err = os.ReadFile(filepath.Join(directory, pending))
			if err != nil || string(got) != "second publisher" {
				t.Fatalf("failed publication changed the pending file: %q, %v", got, err)
			}
		})
	}
}

func TestArchiveRenameUnsupportedFlagFailsClosed(t *testing.T) {
	for _, flagErr := range []error{unix.EINVAL, unix.ENOSYS} {
		t.Run(flagErr.Error(), func(t *testing.T) {
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
				previousLink, previousRename := archiveLink, renameNoReplaceFunc
				t.Cleanup(func() { archiveLink = previousLink })
				t.Cleanup(func() { renameNoReplaceFunc = previousRename })
				archiveLink = func(*os.Root, string, string) error { return errors.ErrUnsupported }
				renameNoReplaceFunc = func(int, string, int, string, uint) error { return flagErr }
				err = publishArchivedFile(root, pending, final)
				if err == nil {
					t.Fatal("unsupported-flag publication succeeded, want failure")
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
		})
	}
}

func TestArchiveRenameFatalErrorFailsClosed(t *testing.T) {
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
	const pending, final = ".00000000000000000003.wal.pending", "00000000000000000003.wal"
	if err := os.WriteFile(filepath.Join(directory, pending), []byte("pending"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := root.Lstat(final); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination initially exists: %v", err)
	}
	linkErr := &os.LinkError{Op: "link", Old: pending, New: final, Err: syscall.EOPNOTSUPP}
	previousLink, previousRename := archiveLink, renameNoReplaceFunc
	t.Cleanup(func() { archiveLink = previousLink })
	t.Cleanup(func() { renameNoReplaceFunc = previousRename })
	archiveLink = func(*os.Root, string, string) error { return linkErr }
	renameNoReplaceFunc = func(int, string, int, string, uint) error { return unix.EACCES }
	err = publishArchivedFile(root, pending, final)
	if !errors.Is(err, unix.EACCES) {
		t.Fatalf("fatal rename publication = %v, want the rename error", err)
	}
	if !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Fatalf("fatal rename publication = %v, want the joined link error", err)
	}
	if errors.Is(err, os.ErrExist) {
		t.Fatalf("fatal rename publication = %v, must not report already exists", err)
	}
	if _, statErr := root.Lstat(final); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fatal rename error fell through to the checked rename: %v", statErr)
	}
	got, readErr := os.ReadFile(filepath.Join(directory, pending))
	if readErr != nil || string(got) != "pending" {
		t.Fatalf("failed publication changed the pending file: %q, %v", got, readErr)
	}
}
