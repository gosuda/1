package neosepia

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

// TestMaintenanceBacklogCountsPublishedSegments pins the probe the root
// maintenance loop uses: one count of published commit segments and one count of
// live run files, both from a single lock-consistent view.
func TestMaintenanceBacklogCountsPublishedSegments(t *testing.T) {
	fixture := openBackendFixture(t)
	for _, batch := range []storage.MutationBatch{
		fixture.batch(t, 1, 10, 100, 100),
		fixture.batch(t, 2, 20, 95, 105),
		fixture.batch(t, 3, 30, 90, 110),
	} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	walSegments, runFiles, err := fixture.backend.MaintenanceBacklog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if walSegments != 3 {
		t.Fatalf("published commit segments: got %d, want 3", walSegments)
	}
	if runFiles == 0 {
		t.Fatal("live run files: got 0, want at least one")
	}
	// A pending commit body is not a published segment.
	position, pending, err := commitFilePosition(pendingCommitName(4))
	if err != nil || !pending || position != 4 {
		t.Fatalf("pending name classification: %d, %t, %v", position, pending, err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
		t.Fatal(err)
	}
	walSegments, reclaimedFiles, err := fixture.backend.MaintenanceBacklog(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if walSegments != 0 {
		t.Fatalf("commit segments after collection: got %d, want 0", walSegments)
	}
	if reclaimedFiles >= runFiles {
		t.Fatalf("live run files after collection: got %d, want fewer than %d", reclaimedFiles, runFiles)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := fixture.backend.MaintenanceBacklog(t.Context()); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("closed backend backlog: %v", err)
	}
}

// crashMaintenance kills a child process at one durable publication boundary of
// a collection. The child exits without any deferred cleanup, so the on-disk
// state matches a kill -9.
func crashMaintenance(t *testing.T, fixture backendFixture, point maintenancePoint) {
	t.Helper()
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMaintenanceCrashHelper$")
	command.Env = []string{
		"NEOSEPIA_MAINTENANCE_DIRECTORY=" + fixture.config.Directory,
		"NEOSEPIA_MAINTENANCE_POINT=" + strconv.Itoa(int(point)),
		"TMPDIR=" + t.TempDir(),
		"GOMAXPROCS=2",
	}
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("crash point %d: child did not terminate at the boundary: %v\n%s", point, err, output)
	}
}

// baseManifestFiles counts durable base manifests in runs/.
func baseManifestFiles(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(directory, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "base-") && strings.HasSuffix(entry.Name(), ".runs") {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names
}

// TestMaintenanceCrashRecovery requires every collection publication boundary to
// be crash-safe: committed rows survive, no unpublished manifest is left behind,
// and both a later commit and a later collection succeed on the reopened state.
func TestMaintenanceCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	for _, point := range []maintenancePoint{
		maintenanceAfterCapture,
		maintenanceBeforeBase,
		maintenanceAfterBase,
		maintenanceAfterState,
	} {
		t.Run(strconv.Itoa(int(point)), func(t *testing.T) {
			fixture := openBackendFixture(t)
			batches := []storage.MutationBatch{
				atomicRecoveryBatch(t, fixture, 1, 10, false),
				atomicRecoveryBatch(t, fixture, 2, 20, true),
				atomicRecoveryBatch(t, fixture, 3, 30, false),
			}
			for _, batch := range batches {
				if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
					t.Fatal(err)
				}
			}
			crashMaintenance(t, fixture, point)
			fixture = reopenAtomicRecoveryFixture(t, fixture)
			state, err := fixture.backend.State(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			baseGeneration := fixture.backend.state.BaseGeneration
			if got := baseManifestFiles(t, fixture.config.Directory); len(got) != boolCount(baseGeneration != 0) {
				t.Fatalf("base manifests after crash at %d: %v, want %d", point, got, boolCount(baseGeneration != 0))
			}
			if _, err := os.Stat(filepath.Join(fixture.config.Directory, "runs", baseManifestPending)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("crash at %d retained a pending base manifest: %v", point, err)
			}
			reader := fixture.snapshot(t, 30)
			requireAtomicRecoveryState(t, reader, false)
			if err := reader.Close(); err != nil {
				t.Fatal(err)
			}
			next := atomicRecoveryBatch(t, fixture, 4, 40, true)
			if result, err := fixture.backend.ApplyMutations(t.Context(), next); err != nil || result.LogPosition != 4 {
				t.Fatalf("commit after crash at %d: %+v, %v", point, result, err)
			}
			if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: state.HistoryFloor}); err != nil {
				t.Fatalf("collection after crash at %d: %v", point, err)
			}
			latest := fixture.snapshot(t, 40)
			requireAtomicRecoveryState(t, latest, true)
			if retry, err := fixture.backend.ApplyMutations(t.Context(), next); err != nil || retry.LogPosition != 4 {
				t.Fatalf("committed retry after crash at %d: %+v, %v", point, retry, err)
			}
		})
	}
}

func boolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

// TestMaintenanceCrashHelper is the child of TestMaintenanceCrashRecovery. It
// reopens one directory and collects at one boundary, exiting inside the fault
// hook so the crash is exact.
func TestMaintenanceCrashHelper(t *testing.T) {
	directory := os.Getenv("NEOSEPIA_MAINTENANCE_DIRECTORY")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	point, err := strconv.ParseUint(os.Getenv("NEOSEPIA_MAINTENANCE_POINT"), 10, 8)
	if err != nil {
		t.Fatal(err)
	}
	state := databaseStateFixture()
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Directory: directory, Scale: state.Scales[0].Scale, KeyEncoder: backendIntEncoder{}, FenceVerifier: authority}
	backend, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	backend.maintenanceFault = func(_ context.Context, current maintenancePoint) error {
		if current == maintenancePoint(point) {
			os.Exit(23)
		}
		return nil
	}
	if err := backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
		t.Fatalf("crash boundary %d was not reached: %v", point, err)
	}
	t.Fatalf("crash boundary %d was not reached", point)
}

// TestNeoSepiaSoakBoundedHistory is the env-gated soak: sustained commits with a
// periodic floor-preserving collection keep commits/ bounded, leave at most one
// base manifest, retire every run segment the base covers, and reopen quickly.
func TestNeoSepiaSoakBoundedHistory(t *testing.T) {
	if os.Getenv("NEOSEPIA_SOAK") == "" {
		t.Skip("set NEOSEPIA_SOAK=1 to run the bounded-history soak")
	}
	fixture := openBackendFixture(t)
	for id := 1; id <= 10000; id++ {
		batch := maintenanceBatch(t, fixture.epoch, id)
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatalf("commit %d: %v", id, err)
		}
		if id%64 != 0 {
			continue
		}
		state, err := fixture.backend.RetentionState(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: state.HistoryFloor}); err != nil {
			t.Fatalf("collection at commit %d: %v", id, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(fixture.config.Directory, "commits"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) >= 64 {
		t.Fatalf("commits/ holds %d segments, want fewer than 64", len(entries))
	}
	manifests := baseManifestFiles(t, fixture.config.Directory)
	if len(manifests) != 1 {
		t.Fatalf("base manifests: %v, want exactly one", manifests)
	}
	if fixture.backend.state.BaseGeneration == 0 {
		t.Fatal("soak published no durable base")
	}
	limits := DefaultLimits()
	base, err := loadRunBase(t.Context(), fixture.backend.directory.runs, fixture.backend.state.BaseGeneration, limits.MetadataBytes, limits.IdentityBytes, limits.MaxFiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range runSegmentNames(t, fixture.config.Directory) {
		position, err := strconv.ParseUint(name[:20], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if position <= base.Position {
			t.Fatalf("run segment %q survives its durable base at position %d", name, base.Position)
		}
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	reopened, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	t.Logf("soak: %d commit segments, base position %d, reopen %s", len(entries), base.Position, elapsed)
	if elapsed > 5*time.Second {
		t.Fatalf("soak reopen took %s, want less than 5s", elapsed)
	}
}

// runSegmentNames lists the position-named run segments of one directory.
func runSegmentNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(directory, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if len(name) == 25 && strings.HasSuffix(name, ".runs") && decimalPrefix(name[:20]) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
