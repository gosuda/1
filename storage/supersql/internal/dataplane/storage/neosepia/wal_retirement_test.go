package neosepia

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func reopenRetirementBackend(t *testing.T, fixture *backendFixture) {
	t.Helper()
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
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
}

func retirementFixture(t *testing.T) backendFixture {
	t.Helper()
	fixture := openBackendFixture(t)
	fixture.config.Limits = DefaultLimits()
	fixture.config.Limits.RecoveryFiles = 2
	reopenRetirementBackend(t, &fixture)
	return fixture
}

func requireRetainedWAL(t *testing.T, directory string, want ...uint64) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(directory, "commits"))
	if err != nil {
		t.Fatal(err)
	}
	var got []uint64
	for _, entry := range entries {
		position, pending, err := commitFilePosition(entry.Name())
		if err != nil || pending {
			t.Fatalf("unexpected WAL artifact %q: pending=%v, error=%v", entry.Name(), pending, err)
		}
		got = append(got, position)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("retained WAL positions = %v, want %v", got, want)
	}
}

func TestCompactionRetiresLifetimeWALWithoutLosingSnapshotsOrTxnIDs(t *testing.T) {
	fixture := retirementFixture(t)
	first := fixture.batch(t, 1, 10, 99, 101)
	if _, err := fixture.backend.ApplyMutations(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	historical := fixture.snapshot(t, 10)
	for id := byte(2); id <= 6; id++ {
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, id, model.Timestamp(id)*10, 100-int64(id), 100+int64(id))); err != nil {
			t.Fatal(err)
		}
		if id%2 == 0 {
			if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
				t.Fatal(err)
			}
			requireRetainedWAL(t, fixture.config.Directory)
		}
	}
	if got := backendBalance(t, historical, 0); got != 99 {
		t.Fatalf("pinned historical balance after retirement = %d, want 99", got)
	}
	if err := historical.Close(); err != nil {
		t.Fatal(err)
	}
	reopenRetirementBackend(t, &fixture)
	if got := backendBalance(t, fixture.snapshot(t, 60), 0); got != 94 {
		t.Fatalf("reopened current balance = %d, want 94", got)
	}
	if got := backendBalance(t, fixture.snapshot(t, 10), 0); got != 99 {
		t.Fatalf("reopened historical balance = %d, want 99", got)
	}
	if result, err := fixture.backend.ApplyMutations(t.Context(), first); err != nil || result.LogPosition != 1 {
		t.Fatalf("retired transaction retry = %+v, %v; want original position 1", result, err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 98, 102)); !errors.Is(err, storage.ErrTxnIDReused) {
		t.Fatalf("changed retired transaction identity = %v, want ErrTxnIDReused", err)
	}
}

func TestCapturedCheckpointOwnsBaseAndSuffixWhileCompactionRetiresSources(t *testing.T) {
	fixture := retirementFixture(t)
	for id := byte(1); id <= 5; id++ {
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, id, model.Timestamp(id)*10, 100-int64(id), 100+int64(id))); err != nil {
			t.Fatal(err)
		}
		if id%2 == 0 {
			if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
				t.Fatal(err)
			}
		}
	}
	fence, err := fixture.authority.Issue(t.Context(), 50, 5, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	fixture.backend.mu.Lock()
	builder, err := fixture.backend.captureCheckpoint(t.Context(), storage.SnapshotSpec{Timestamp: 50, Fence: fence})
	fixture.backend.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := builder.handle.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 6, 60, 94, 106)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
		t.Fatal(err)
	}
	requireRetainedWAL(t, fixture.config.Directory)
	if err := builder.finish(t.Context()); err != nil {
		t.Fatalf("finish captured checkpoint after source retirement: %v", err)
	}
	var logs []string
	for _, file := range checkpointManifest(t, builder.handle).Files {
		if strings.HasPrefix(file.Name, "commits/") {
			logs = append(logs, file.Name)
		}
	}
	if !slices.Equal(logs, []string{"commits/" + commitName(5)}) {
		t.Fatalf("checkpoint WAL = %v, want only suffix position 5", logs)
	}
	destination := t.TempDir()
	copyCheckpointFiles(t, builder.handle, destination)
	fixture.config.Directory = destination
	reopenRetirementBackend(t, &fixture)
	if got := backendBalance(t, fixture.snapshot(t, 50), 0); got != 95 {
		t.Fatalf("restored checkpoint balance = %d, want 95", got)
	}
	if got := backendBalance(t, fixture.snapshot(t, 10), 0); got != 99 {
		t.Fatalf("restored base history = %d, want 99", got)
	}
	if result, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 99, 101)); err != nil || result.LogPosition != 1 {
		t.Fatalf("restored retired transaction retry = %+v, %v", result, err)
	}
}

func TestRecoveryResumesInterruptedWALRetirementBeyondFileBudget(t *testing.T) {
	fixture := retirementFixture(t)
	for id := byte(1); id <= 6; id++ {
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, id, model.Timestamp(id)*10, 100-int64(id), 100+int64(id))); err != nil {
			t.Fatal(err)
		}
	}
	interrupted := errors.New("retirement interrupted after durable base publication")
	fixture.backend.stateFault = func(point statePoint) error {
		if point == stateAfterDirectorySync {
			return interrupted
		}
		return nil
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); !errors.Is(err, storage.ErrCommitIndeterminate) || !errors.Is(err, interrupted) {
		t.Fatalf("interrupted base publication = %v", err)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	// A crash can leave any subset of unlinked covered names. The four remaining
	// records exceed RecoveryFiles, but none is needed to recover the durable base.
	for _, position := range []uint64{1, 3} {
		if err := os.Remove(filepath.Join(fixture.config.Directory, "commits", commitName(position))); err != nil {
			t.Fatal(err)
		}
	}
	reopenRetirementBackend(t, &fixture)
	requireRetainedWAL(t, fixture.config.Directory)
	if got := backendBalance(t, fixture.snapshot(t, 60), 0); got != 94 {
		t.Fatalf("recovered interrupted retirement balance = %d, want 94", got)
	}
	if result, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 99, 101)); err != nil || result.LogPosition != 1 {
		t.Fatalf("interrupted retirement lost transaction identity: %+v, %v", result, err)
	}
}

func TestRetiredBaseDoesNotHideRequiredSuffixDamage(t *testing.T) {
	for _, damage := range []string{"gap", "corrupt", "missing closed tail"} {
		t.Run(damage, func(t *testing.T) {
			fixture := retirementFixture(t)
			for id := byte(1); id <= 4; id++ {
				if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, id, model.Timestamp(id)*10, 100-int64(id), 100+int64(id))); err != nil {
					t.Fatal(err)
				}
				if id == 2 {
					if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 0}); err != nil {
						t.Fatal(err)
					}
				}
			}
			requireRetainedWAL(t, fixture.config.Directory, 3, 4)
			reader := fixture.snapshot(t, 40)
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fixture.backend.Close(); err != nil {
				t.Fatal(err)
			}
			position := uint64(3)
			if damage == "missing closed tail" {
				position = 4
			}
			path := filepath.Join(fixture.config.Directory, "commits", commitName(position))
			if damage == "corrupt" {
				if err := os.WriteFile(path, []byte("corrupt required decision"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			backend, err := Open(t.Context(), fixture.config)
			if backend != nil {
				err = errors.Join(err, backend.Close())
				t.Fatalf("damaged suffix returned a backend: %v", err)
			}
			if !errors.Is(err, storage.ErrCorruptStorage) {
				t.Fatalf("required suffix %s = %v, want ErrCorruptStorage", damage, err)
			}
		})
	}
}

func TestUninstalledDurableBaseKeepsRecoveryInputsAndCollectionFence(t *testing.T) {
	fixture := retirementFixture(t)
	fixture.config.Limits.ResidentBytes = 64 << 10
	fixture.config.Limits.MaxPins = 2048
	reopenRetirementBackend(t, &fixture)
	for id := byte(1); id <= 2; id++ {
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, id, model.Timestamp(id)*10, 100-int64(id), 100+int64(id))); err != nil {
			t.Fatal(err)
		}
	}
	reader := fixture.snapshot(t, 20).(*snapshotReader)
	var retained []*runPin
	t.Cleanup(func() {
		for _, pin := range retained {
			if err := pin.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	fixture.backend.stateFault = func(point statePoint) error {
		if point != stateAfterDirectorySync {
			return nil
		}
		// Derived cursors retain snapshot pins without taking backend.mu.
		for {
			pin, err := reader.pin.retain(t.Context())
			if errors.Is(err, columnar.ErrBatchBudget) {
				return nil
			}
			if err != nil {
				return err
			}
			retained = append(retained, pin)
		}
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); !errors.Is(err, storage.ErrCommitIndeterminate) || !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("base installation after pin retention = %v, want indeterminate capacity failure", err)
	}
	fixture.backend.stateFault = nil
	requireRetainedWAL(t, fixture.config.Directory, 1, 2)
	for _, position := range []uint64{1, 2} {
		if _, err := os.Stat(filepath.Join(fixture.config.Directory, "runs", runManifestName(position))); err != nil {
			t.Fatalf("uninstalled base retired recovery manifest %d: %v", position, err)
		}
	}
	fence, err := fixture.authority.Issue(t.Context(), 20, 2, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	tooOld, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 10, Fence: fence})
	if tooOld != nil {
		err = errors.Join(err, tooOld.Close())
		t.Fatalf("uncertain GC admitted history below its durable floor: %v", err)
	}
	if !errors.Is(err, storage.ErrSnapshotTooOld) {
		t.Fatalf("uncertain collection boundary = %v, want ErrSnapshotTooOld", err)
	}
	for _, pin := range retained {
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	}
	retained = nil
	if got := backendBalance(t, reader, 0); got != 98 {
		t.Fatalf("retained snapshot after failed installation = %d, want 98", got)
	}
	fence, err = fixture.authority.Issue(t.Context(), 30, 2, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 20, Fence: fence})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reconciled.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := backendBalance(t, reconciled, 0); got != 98 {
		t.Fatalf("reconciled compacted balance = %d, want 98", got)
	}
	requireRetainedWAL(t, fixture.config.Directory)
	for _, position := range []uint64{1, 2} {
		if _, err := os.Stat(filepath.Join(fixture.config.Directory, "runs", runManifestName(position))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("installed base retained obsolete manifest %d: %v", position, err)
		}
	}
}
