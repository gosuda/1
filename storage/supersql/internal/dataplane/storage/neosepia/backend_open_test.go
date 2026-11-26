package neosepia

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestBackendRejectsLegacyDirectoryWithoutChangingIt(t *testing.T) {
	fixture := openBackendFixture(t)
	config := fixture.config
	config.Directory = t.TempDir()
	path := filepath.Join(config.Directory, "legacy.neosepia")
	if err := os.WriteFile(path, []byte("preserve legacy data"), 0600); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(t.Context(), config)
	if opened != nil {
		opened.Close()
	}
	if !errors.Is(err, ErrIncompatibleFormat) {
		t.Fatalf("legacy open: %v", err)
	}
	entries, err := os.ReadDir(config.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "legacy.neosepia" {
		t.Fatalf("legacy directory was changed: %v", entries)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "preserve legacy data" {
		t.Fatalf("legacy payload changed: %q, %v", data, err)
	}
}

func TestBackendRejectsAuthenticFenceOmittingEarlierCommit(t *testing.T) {
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 100, 90, 110)); err != nil {
		t.Fatal(err)
	}
	omitted, err := fixture.authority.Issue(t.Context(), 50, 0, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 50, Fence: omitted})
	if reader != nil {
		reader.Close()
	}
	if !errors.Is(err, storage.ErrSnapshotNotClosed) {
		t.Fatalf("omitted commit was certified closed: %v", err)
	}
	complete, err := fixture.authority.Issue(t.Context(), 50, 1, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	historical, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 50, Fence: complete})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := historical.Close(); err != nil {
			t.Error(err)
		}
	})
	if backendBalance(t, historical, 0) != 100 || backendBalance(t, historical, 1) != 100 {
		t.Fatal("historical fence admitted later committed values")
	}
}

func TestBackendPersistsVersionedFutureLeapHistory(t *testing.T) {
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	history := make([]byte, 29)
	copy(history, fixture.config.Scale.CanonicalLeapHistory)
	binary.BigEndian.PutUint32(history[9:13], 1)
	binary.BigEndian.PutUint64(history[13:21], 1)
	history[21] = 1
	fixture.config.Scale = clock.ScaleDescriptor{Name: clock.ScaleName, CanonicalLeapHistory: history, LeapHistoryID: sha256.Sum256(history)}
	upgraded, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := upgraded.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = upgraded
	fixture.epoch.LeapHistory = fixture.config.Scale.LeapHistoryID
	if _, err := upgraded.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 90, 110)); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.Close(); err != nil {
		t.Fatal(err)
	}
	fixture.config.Scale = clock.ScaleDescriptor{}
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = reopened
	reader := fixture.snapshot(t, 30)
	if backendBalance(t, reader, 0) != 90 || backendBalance(t, reader, 1) != 110 {
		t.Fatal("recovery lost transactions across leap-history versions")
	}
	state, err := reopened.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := clock.ValidateScale(t.Context(), state.Scale, 4096); err != nil {
		t.Fatalf("persisted conversion metadata: %v", err)
	}
	if state.Scale.LeapHistoryID != fixture.epoch.LeapHistory {
		t.Fatal("clock-free reopen selected an earlier conversion history")
	}
}
