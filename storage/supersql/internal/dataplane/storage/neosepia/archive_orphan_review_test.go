package neosepia

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func assertOrphanPreserved(t *testing.T, directory string, position uint64, metadata []byte) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(directory, walMetaName(position)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, metadata) {
		t.Fatal("failed repair changed the original archive identity")
	}
	if _, err := os.Stat(filepath.Join(directory, commitName(position))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed repair published a replacement WAL: %v", err)
	}
}

func TestWALArchiveRejectsDifferentOrphanDecision(t *testing.T) {
	original := openBackendFixture(t)
	archive := t.TempDir()
	if err := original.backend.EnableWALArchive(t.Context(), archive, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := original.backend.ApplyMutations(t.Context(), original.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(archive, walMetaName(1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(archive, commitName(1))); err != nil {
		t.Fatal(err)
	}
	other := openBackendFixture(t)
	if _, err := other.backend.ApplyMutations(t.Context(), other.batch(t, 1, 20, 999, 999)); err != nil {
		t.Fatal(err)
	}
	if err := other.backend.EnableWALArchive(t.Context(), archive, nil); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("repair with another decision at the same position/timestamp = %v, want corrupt storage", err)
	}
	assertOrphanPreserved(t, archive, 1, metadata)
	if err := original.backend.EnableWALArchive(t.Context(), archive, nil); err != nil {
		t.Fatalf("retry with the original decision: %v", err)
	}
}

func TestWALArchiveRejectsOrphanKeyChange(t *testing.T) {
	fixture := openBackendFixture(t)
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, encryptionKeySize)
	fixture.config.Directory = t.TempDir()
	fixture.config.EncryptionKey = key
	backend, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = backend
	archive := t.TempDir()
	if err := backend.EnableWALArchive(t.Context(), archive, key); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	if err := backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.config.Directory, "commits", commitName(1))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture did not retire the first local WAL: %v", err)
	}
	if _, err := backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 101, 201)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fixture.config.Directory, "commits", commitName(2))); err != nil {
		t.Fatalf("fixture lost the unretired repair source: %v", err)
	}
	metadata, err := os.ReadFile(filepath.Join(archive, walMetaName(2)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(archive, commitName(2))); err != nil {
		t.Fatal(err)
	}
	otherKey := bytes.Repeat([]byte{9}, encryptionKeySize)
	if err := backend.EnableWALArchive(t.Context(), archive, otherKey); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("repair with another archive key = %v, want corrupt storage", err)
	}
	assertOrphanPreserved(t, archive, 2, metadata)
	if err := backend.EnableWALArchive(t.Context(), archive, key); err != nil {
		t.Fatalf("retry with the original archive key: %v", err)
	}
	want, err := os.ReadFile(filepath.Join(fixture.config.Directory, "commits", commitName(2)))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(archive, commitName(2)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("same-key repair did not restore the original encrypted bytes")
	}
}

func TestWALArchiveRejectsOrphanReencryption(t *testing.T) {
	fixture := openBackendFixture(t)
	archive := t.TempDir()
	key := bytes.Repeat([]byte{7}, encryptionKeySize)
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, key); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	metadata, err := os.ReadFile(filepath.Join(archive, walMetaName(1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(archive, commitName(1))); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, key); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("repair requiring new encryption bytes = %v, want corrupt storage", err)
	}
	assertOrphanPreserved(t, archive, 1, metadata)
}

func TestWALArchiveRepairKeepsSidecarBeforeSync(t *testing.T) {
	fixture := openBackendFixture(t)
	archive := t.TempDir()
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	metaName, walName := walMetaName(1), commitName(1)
	metadata, err := os.ReadFile(filepath.Join(archive, metaName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(archive, walName)); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(archive)
	if err != nil {
		t.Fatal(err)
	}
	// Stop after the repair helper publishes the WAL but before archiveCommit
	// syncs the directory. A retry must accept this partial publication.
	if err := fixture.backend.archiver.healMissingArchivedWAL(t.Context(), fixture.backend.directory.commits, root, 1, walName, metaName, 20); err != nil {
		_ = root.Close()
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(archive, metaName))
	if err != nil {
		t.Fatalf("repair lost its sidecar before directory sync: %v", err)
	}
	if !bytes.Equal(got, metadata) {
		t.Fatal("repair replaced its sidecar before directory sync")
	}
	if _, err := os.Stat(filepath.Join(archive, walName)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	var synced int
	previousFault := reopened.archiveFault
	reopened.archiveFault = func(point archiveFaultPoint) error {
		if point == archiveDirectorySynced {
			synced++
		}
		return nil
	}
	defer func() { reopened.archiveFault = previousFault }()
	if err := reopened.EnableWALArchive(t.Context(), archive, nil); err != nil {
		t.Fatalf("retry after publication before directory sync: %v", err)
	}
	if synced == 0 {
		t.Fatal("retry published the repaired WAL without syncing the archive directory")
	}
	if err := reopened.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
		t.Fatal(err)
	}
}

func TestWALArchiveRetryFailsWithoutDirectorySync(t *testing.T) {
	fixture := openBackendFixture(t)
	archive := t.TempDir()
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	metaName, walName := walMetaName(1), commitName(1)
	metadata, err := os.ReadFile(filepath.Join(archive, metaName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(archive, walName)); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("test archive directory sync failure")
	previousFault := fixture.backend.archiveFault
	fixture.backend.archiveFault = func(point archiveFaultPoint) error {
		if point == archiveDirectorySynced {
			return injected
		}
		return nil
	}
	defer func() { fixture.backend.archiveFault = previousFault }()
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, nil); !errors.Is(err, injected) {
		t.Fatalf("retry with failing directory sync = %v, want the injected failure", err)
	}
	got, err := os.ReadFile(filepath.Join(archive, metaName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, metadata) {
		t.Fatal("failed retry changed the original archive identity")
	}
	var synced int
	fixture.backend.archiveFault = func(point archiveFaultPoint) error {
		if point == archiveDirectorySynced {
			synced++
		}
		return nil
	}
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, nil); err != nil {
		t.Fatalf("retry after sync recovery: %v", err)
	}
	if synced == 0 {
		t.Fatal("recovery retry published without syncing the archive directory")
	}
}

func TestWALArchiveRetryReportsSyncFailure(t *testing.T) {
	fixture := openBackendFixture(t)
	archive := t.TempDir()
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	metaName, walName := walMetaName(1), commitName(1)
	metadata, err := os.ReadFile(filepath.Join(archive, metaName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(archive, walName)); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("test archive directory sync failure")
	previousSync, previousFault := fixture.backend.archiveSyncDir, fixture.backend.archiveFault
	var hooked int
	fixture.backend.archiveSyncDir = func(*os.Root) error { return injected }
	fixture.backend.archiveFault = func(point archiveFaultPoint) error {
		if point == archiveDirectorySynced {
			hooked++
		}
		return nil
	}
	defer func() { fixture.backend.archiveSyncDir, fixture.backend.archiveFault = previousSync, previousFault }()
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, nil); !errors.Is(err, injected) {
		t.Fatalf("retry with failing directory sync = %v, want the injected failure", err)
	}
	if hooked != 0 {
		t.Fatalf("fault hook fired %d times despite the failed sync", hooked)
	}
	got, err := os.ReadFile(filepath.Join(archive, metaName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, metadata) {
		t.Fatal("failed retry changed the original archive identity")
	}
	fixture.backend.archiveSyncDir = previousSync
	var synced int
	fixture.backend.archiveFault = func(point archiveFaultPoint) error {
		if point == archiveDirectorySynced {
			synced++
		}
		return nil
	}
	if err := fixture.backend.EnableWALArchive(t.Context(), archive, nil); err != nil {
		t.Fatalf("retry after sync recovery: %v", err)
	}
	if synced == 0 {
		t.Fatal("recovery retry published without syncing the archive directory")
	}
}

func TestWALArchivePerBackendArchiveHooksIsolated(t *testing.T) {
	first := openBackendFixture(t)
	second := openBackendFixture(t)
	archiveA := t.TempDir()
	archiveB := t.TempDir()
	if err := first.backend.EnableWALArchive(t.Context(), archiveA, nil); err != nil {
		t.Fatal(err)
	}
	if err := second.backend.EnableWALArchive(t.Context(), archiveB, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := first.backend.ApplyMutations(t.Context(), first.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	if _, err := second.backend.ApplyMutations(t.Context(), second.batch(t, 1, 20, 100, 200)); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("test per-backend archive fault")
	previousFault := first.backend.archiveFault
	first.backend.archiveFault = func(point archiveFaultPoint) error {
		if point == archiveDirectorySynced {
			return injected
		}
		return nil
	}
	defer func() { first.backend.archiveFault = previousFault }()
	if err := second.backend.EnableWALArchive(t.Context(), archiveB, nil); err != nil {
		t.Fatalf("fault set on another backend leaked: %v", err)
	}
	if err := first.backend.EnableWALArchive(t.Context(), archiveA, nil); !errors.Is(err, injected) {
		t.Fatalf("fault on the owning backend did not fire: %v", err)
	}
	first.backend.archiveFault = previousFault
	syncInjected := errors.New("test per-backend archive sync failure")
	previousSync := first.backend.archiveSyncDir
	first.backend.archiveSyncDir = func(*os.Root) error { return syncInjected }
	defer func() { first.backend.archiveSyncDir = previousSync }()
	if err := second.backend.EnableWALArchive(t.Context(), archiveB, nil); err != nil {
		t.Fatalf("sync failure set on another backend leaked: %v", err)
	}
	if err := first.backend.EnableWALArchive(t.Context(), archiveA, nil); !errors.Is(err, syncInjected) {
		t.Fatalf("sync failure on the owning backend did not fire: %v", err)
	}
}

func TestWALArchiveHealSkipsFileSyncOnMismatch(t *testing.T) {
	t.Run("different archive key", func(t *testing.T) {
		fixture := openBackendFixture(t)
		archive := t.TempDir()
		key := bytes.Repeat([]byte{7}, encryptionKeySize)
		if err := fixture.backend.EnableWALArchive(t.Context(), archive, key); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 200)); err != nil {
			t.Fatal(err)
		}
		metadata, err := os.ReadFile(filepath.Join(archive, walMetaName(1)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(archive, commitName(1))); err != nil {
			t.Fatal(err)
		}
		var syncs int
		fixture.backend.archiveFileSync = func(*os.File) error { syncs++; return nil }
		defer func() { fixture.backend.archiveFileSync = nil }()
		if err := fixture.backend.EnableWALArchive(t.Context(), archive, key); !errors.Is(err, storage.ErrCorruptStorage) {
			t.Fatalf("repair with a different archive key = %v, want corrupt storage", err)
		}
		if syncs != 0 {
			t.Fatalf("doomed repair synced its temp file %d times", syncs)
		}
		assertOrphanPreserved(t, archive, 1, metadata)
	})
	t.Run("different decision", func(t *testing.T) {
		original := openBackendFixture(t)
		archive := t.TempDir()
		if err := original.backend.EnableWALArchive(t.Context(), archive, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := original.backend.ApplyMutations(t.Context(), original.batch(t, 1, 20, 100, 200)); err != nil {
			t.Fatal(err)
		}
		metadata, err := os.ReadFile(filepath.Join(archive, walMetaName(1)))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(archive, commitName(1))); err != nil {
			t.Fatal(err)
		}
		other := openBackendFixture(t)
		if _, err := other.backend.ApplyMutations(t.Context(), other.batch(t, 1, 20, 999, 999)); err != nil {
			t.Fatal(err)
		}
		var syncs int
		other.backend.archiveFileSync = func(*os.File) error { syncs++; return nil }
		defer func() { other.backend.archiveFileSync = nil }()
		if err := other.backend.EnableWALArchive(t.Context(), archive, nil); !errors.Is(err, storage.ErrCorruptStorage) {
			t.Fatalf("repair with another decision = %v, want corrupt storage", err)
		}
		if syncs != 0 {
			t.Fatalf("doomed repair synced its temp file %d times", syncs)
		}
		assertOrphanPreserved(t, archive, 1, metadata)
	})
}
