package neosepia

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

func TestArchivePublishFailsClosedWithoutNoReplace(t *testing.T) {
	for _, flagErr := range []error{unix.ENOSYS, unix.EINVAL} {
		t.Run(flagErr.Error(), func(t *testing.T) {
			for _, occupied := range []bool{false, true} {
				directory := t.TempDir()
				root, err := os.OpenRoot(directory)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = root.Close() })
				const pending, final = ".00000000000000000007.wal.pending", "00000000000000000007.wal"
				if err := os.WriteFile(filepath.Join(directory, pending), []byte("pending"), 0600); err != nil {
					t.Fatal(err)
				}
				if occupied {
					if err := os.WriteFile(filepath.Join(directory, final), []byte("original"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				previousLink, previousRename := archiveLink, renameNoReplaceFunc
				archiveLink = func(*os.Root, string, string) error {
					return &os.LinkError{Op: "link", Old: pending, New: final, Err: syscall.EOPNOTSUPP}
				}
				renameNoReplaceFunc = func(int, string, int, string, uint) error { return flagErr }
				t.Cleanup(func() { archiveLink = previousLink })
				t.Cleanup(func() { renameNoReplaceFunc = previousRename })
				err = publishArchivedFile(root, pending, final)
				if err == nil {
					t.Fatal("publish without a no-replace operation succeeded, want failure")
				}
				if errors.Is(err, os.ErrExist) {
					t.Fatalf("unverifiable publish = %v, must not report already exists", err)
				}
				if !errors.Is(err, errors.ErrUnsupported) {
					t.Fatalf("unverifiable publish = %v, want the unsupported-operation cause", err)
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
					t.Fatalf("failed publication changed pending bytes: %q, %v", got, err)
				}
			}
		})
	}
}

func TestArchiveConcurrentPublishersFailClosedWithoutNoReplace(t *testing.T) {
	directory := t.TempDir()
	previousLink, previousRename := archiveLink, renameNoReplaceFunc
	archiveLink = func(*os.Root, string, string) error {
		return &os.LinkError{Op: "link", Old: "pending", New: "final", Err: syscall.EOPNOTSUPP}
	}
	renameNoReplaceFunc = func(int, string, int, string, uint) error { return unix.ENOSYS }
	t.Cleanup(func() { archiveLink = previousLink })
	t.Cleanup(func() { renameNoReplaceFunc = previousRename })
	const publishers = 8
	const rounds = 25
	for round := 0; round < rounds; round++ {
		final := fmt.Sprintf("%020d.wal", round+1)
		start := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, publishers)
		for i := 0; i < publishers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				root, err := os.OpenRoot(directory)
				if err != nil {
					errs[i] = err
					return
				}
				defer func() { _ = root.Close() }()
				pending := fmt.Sprintf(".%s.%d.pending", final, i)
				if err := os.WriteFile(filepath.Join(directory, pending), []byte{byte(i)}, 0600); err != nil {
					errs[i] = err
					return
				}
				errs[i] = publishArchivedFile(root, pending, final)
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err == nil {
				t.Fatalf("round %d publisher %d: publish succeeded without a no-replace operation, want failure", round, i)
			}
			if errors.Is(err, os.ErrExist) {
				t.Fatalf("round %d publisher %d: unverifiable publish = %v, must not report already exists", round, i, err)
			}
		}
		if _, err := os.Lstat(filepath.Join(directory, final)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("round %d: a publisher replaced the archive without a no-replace operation", round)
		}
	}
}

func TestArchivePublishesViaRenameWhenLinkUnsupported(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	pending := ".00000000000000000001.wal.pending-link-test"
	final := "00000000000000000001.wal"
	content := []byte("archived-segment-bytes")
	if err := os.WriteFile(filepath.Join(dir, pending), content, 0600); err != nil {
		t.Fatal(err)
	}
	oldLink := archiveLink
	archiveLink = func(root *os.Root, oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EOPNOTSUPP}
	}
	t.Cleanup(func() { archiveLink = oldLink })
	if err := publishArchivedFile(root, pending, final); err != nil {
		t.Fatalf("publish with failing link = %v, want rename fallback", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, final))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("final content = %q, want %q", got, content)
	}
	if _, err := os.Lstat(filepath.Join(dir, pending)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending remains after rename fallback: %v", err)
	}
}

func TestArchivePreservesNoOverwriteWhenLinkUnsupported(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	final := "00000000000000000002.wal"
	original := []byte("original-archive")
	if err := os.WriteFile(filepath.Join(dir, final), original, 0600); err != nil {
		t.Fatal(err)
	}
	pending := ".00000000000000000002.wal.pending-link-test"
	if err := os.WriteFile(filepath.Join(dir, pending), []byte("replacement-bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	oldLink := archiveLink
	archiveLink = func(root *os.Root, oldname, newname string) error {
		return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
	}
	t.Cleanup(func() { archiveLink = oldLink })
	if err := publishArchivedFile(root, pending, final); !errors.Is(err, os.ErrExist) {
		t.Fatalf("publish over existing final = %v, want already exists", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, final))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("final overwritten = %q, want %q", got, original)
	}
}
