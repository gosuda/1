package neosepia

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var errLostCollectionPublication = errors.New("lost collection publication acknowledgement")
var errLostCollectionCompletion = errors.New("collection completion acknowledgement lost")

func TestBackendCollectCompactsReopensAndReclaimsAfterPins(t *testing.T) {
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
	atFloor := fixture.snapshot(t, 20)
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 21}); !errors.Is(err, storage.ErrSnapshotPinned) {
		t.Fatalf("collection crossed an admitted snapshot: %v", err)
	}
	obsolete := runSSTNames(t, fixture.config.Directory)
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
		t.Fatal(err)
	}
	if got := []int64{backendBalance(t, atFloor, 0), backendBalance(t, atFloor, 1)}; !slices.Equal(got, []int64{95, 105}) {
		t.Fatalf("snapshot at retained floor after collection: got %v, want [95 105]", got)
	}
	for _, name := range obsolete {
		if _, err := os.Stat(filepath.Join(fixture.config.Directory, "runs", name)); err != nil {
			t.Fatalf("pinned run %q was reclaimed early: %v", name, err)
		}
	}
	stateAfterCollect, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stateAfterCollect.HistoryFloor != 20 || fixture.backend.state.BaseGeneration == 0 {
		t.Fatalf("published collection state: %+v, base generation %d", stateAfterCollect, fixture.backend.state.BaseGeneration)
	}
	baseGenerationAfterCollect := fixture.backend.state.BaseGeneration
	target := encTestTarget()
	task, err := fixture.backend.ScheduleCompaction(t.Context(), storage.CompactionRequest{Target: target, GCWatermark: 20, TargetBlock: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if task.TaskID() == "" {
		t.Fatal("scheduled compaction returned an empty task identity")
	}
	if err := task.Wait(t.Context()); err != nil {
		t.Fatal(err)
	}
	stateAfterTask, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if fixture.backend.state.BaseGeneration <= baseGenerationAfterCollect || stateAfterTask.HistoryFloor != 20 {
		t.Fatalf("scheduled compaction did not publish a new durable base: generation %d, state %+v", fixture.backend.state.BaseGeneration, stateAfterTask)
	}
	if err := atFloor.Close(); err != nil {
		t.Fatal(err)
	}
	for _, name := range obsolete {
		if _, err := os.Stat(filepath.Join(fixture.config.Directory, "runs", name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("obsolete run %q remains after its final pin closed: %v", name, err)
		}
	}
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), state.ClosedTS, state.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 19, Fence: fence}); !errors.Is(err, storage.ErrSnapshotTooOld) {
		if reader != nil {
			reader.Close()
		}
		t.Fatalf("snapshot below durable floor: %v", err)
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
	fixture.backend = reopened
	for _, snapshot := range []struct {
		timestamp model.Timestamp
		want      []int64
	}{{20, []int64{95, 105}}, {30, []int64{90, 110}}} {
		reader := fixture.snapshot(t, snapshot.timestamp)
		got := []int64{backendBalance(t, reader, 0), backendBalance(t, reader, 1)}
		if !slices.Equal(got, snapshot.want) {
			t.Fatalf("reopened snapshot %d: got %v, want %v", snapshot.timestamp, got, snapshot.want)
		}
	}
	if retry, err := reopened.ApplyMutations(t.Context(), fixture.batch(t, 3, 30, 90, 110)); err != nil || retry.LogPosition != 3 {
		t.Fatalf("committed retry after compacted recovery: %+v, %v", retry, err)
	}
}

func TestBackendCollectionFailureRecoversPublishedBase(t *testing.T) {
	fixture := openBackendFixture(t)
	for _, batch := range []storage.MutationBatch{
		fixture.batch(t, 1, 10, 100, 100),
		fixture.batch(t, 2, 20, 90, 110),
	} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	historical := fixture.snapshot(t, 20)
	fixture.backend.stateFault = func(point statePoint) error {
		if point == stateAfterRename {
			return errLostCollectionPublication
		}
		return nil
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("uncertain collection publication: %v", err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 3, 30, 80, 120)); !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("uncertain maintenance did not fence writes: %v", err)
	}
	oldFence, err := fixture.authority.Issue(t.Context(), 20, 2, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	tooOld, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 10, Fence: oldFence})
	if tooOld != nil {
		if closeErr := tooOld.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if !errors.Is(err, storage.ErrSnapshotTooOld) {
		t.Fatalf("uncertain collection admitted snapshot below protected floor: %v", err)
	}
	if got := []int64{backendBalance(t, historical, 0), backendBalance(t, historical, 1)}; !slices.Equal(got, []int64{90, 110}) {
		t.Fatalf("pinned boundary snapshot during uncertain maintenance: got %v, want [90 110]", got)
	}
	fixture.backend.stateFault = nil
	retryFence, err := fixture.authority.Issue(t.Context(), 30, 2, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 20, Fence: retryFence})
	if err != nil {
		t.Fatal(err)
	}
	if got := []int64{backendBalance(t, reconciled, 0), backendBalance(t, reconciled, 1)}; !slices.Equal(got, []int64{90, 110}) {
		t.Fatalf("new barrier did not reconcile maintenance publication: got %v, want [90 110]", got)
	}
	if err := reconciled.Close(); err != nil {
		t.Fatal(err)
	}
	if err := historical.Close(); err != nil {
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
	fixture.backend = reopened
	reader := fixture.snapshot(t, 20)
	if got := []int64{backendBalance(t, reader, 0), backendBalance(t, reader, 1)}; !slices.Equal(got, []int64{90, 110}) {
		t.Fatalf("recovered compacted atomic state: got %v, want [90 110]", got)
	}
	if retry, err := reopened.ApplyMutations(t.Context(), fixture.batch(t, 2, 20, 90, 110)); err != nil || retry.LogPosition != 2 {
		t.Fatalf("recovered transaction identity after uncertain collection: %+v, %v", retry, err)
	}
}
func TestBackendCollectionFailureRecoversOnReopen(t *testing.T) {
	fixture := openBackendFixture(t)
	first := fixture.batch(t, 1, 10, 100, 100)
	second := fixture.batch(t, 2, 20, 90, 110)
	for _, batch := range []storage.MutationBatch{first, second} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	fixture.backend.stateFault = func(point statePoint) error {
		if point == stateAfterDirectorySync {
			return errLostCollectionCompletion
		}
		return nil
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("uncertain durable collection: %v", err)
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
	fixture.backend = reopened
	reader := fixture.snapshot(t, 20)
	if got := []int64{backendBalance(t, reader, 0), backendBalance(t, reader, 1)}; !slices.Equal(got, []int64{90, 110}) {
		t.Fatalf("reopened uncertain compacted state: got %v, want [90 110]", got)
	}
	if retry, err := reopened.ApplyMutations(t.Context(), second); err != nil || retry.LogPosition != 2 {
		t.Fatalf("reopened compacted retry identity: %+v, %v", retry, err)
	}
}

func TestBackendCompactionRetainsDescriptorVersionHistory(t *testing.T) {
	fixture := openBackendFixture(t)
	first := fixture.batch(t, 1, 10, 100, 100)
	second := fixture.batch(t, 2, 20, 90, 110)
	for i := range second.Layouts {
		second.Layouts[i].Target.DescriptorVersion++
		second.Mutations[i].Target.DescriptorVersion++
	}
	var err error
	second.ChecksumCRC, err = second.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, batch := range []storage.MutationBatch{first, second} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 10}); err != nil {
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
	fixture.backend = reopened
	for _, snapshot := range []struct {
		timestamp model.Timestamp
		layouts   []storage.ObjectLayout
		want      []int64
	}{{10, first.Layouts, []int64{100, 100}}, {20, second.Layouts, []int64{90, 110}}} {
		reader := fixture.snapshot(t, snapshot.timestamp)
		for i, layout := range snapshot.layouts {
			row, err := reader.Get(t.Context(), storage.ReadRequest{Target: layout.Target, Key: backendKey(1), ColumnIDs: []model.ColumnID{1}, Layout: layout.Columns})
			if err != nil {
				t.Fatal(err)
			}
			if len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].IntVal != snapshot.want[i] {
				t.Fatalf("descriptor-version snapshot %d object %d: got %+v, want %d", snapshot.timestamp, layout.Target.ObjectID, row.Values, snapshot.want[i])
			}
		}
	}
}

func TestCheckpointCapturedBeforeCollectionRestoresHistoricalCommit(t *testing.T) {
	fixture := openBackendFixture(t)
	first := fixture.batch(t, 1, 10, 100, 100)
	second := fixture.batch(t, 2, 20, 95, 105)
	for _, batch := range []storage.MutationBatch{first, second} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	fence, err := fixture.authority.Issue(t.Context(), 20, 2, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 20, Fence: fence}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 3, 30, 90, 110)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 20}); err != nil {
		t.Fatal(err)
	}
	destination := t.TempDir()
	copyCheckpointFiles(t, checkpoint, destination)
	config := fixture.config
	config.Directory = destination
	restored, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = restored
	reader := fixture.snapshot(t, 20)
	if got := []int64{backendBalance(t, reader, 0), backendBalance(t, reader, 1)}; !slices.Equal(got, []int64{95, 105}) {
		t.Fatalf("restored historical checkpoint: got %v, want [95 105]", got)
	}
	if retry, err := restored.ApplyMutations(t.Context(), second); err != nil || retry.LogPosition != 2 {
		t.Fatalf("restored checkpoint lost committed retry identity: %+v, %v", retry, err)
	}
}

func TestScheduledCompactionPropagatesFailureAndCancellation(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		fixture := openBackendFixture(t)
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 100, 100)); err != nil {
			t.Fatal(err)
		}
		injected := errors.New("compaction worker failed")
		fixture.backend.maintenanceFault = func(context.Context, maintenancePoint) error { return injected }
		task, err := fixture.backend.ScheduleCompaction(t.Context(), storage.CompactionRequest{Target: encTestTarget()})
		if err != nil {
			t.Fatal(err)
		}
		if err := task.Wait(t.Context()); !errors.Is(err, injected) {
			t.Fatalf("scheduled failure: %v", err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		fixture := openBackendFixture(t)
		if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 100, 100)); err != nil {
			t.Fatal(err)
		}
		started := make(chan struct{})
		fixture.backend.maintenanceFault = func(ctx context.Context, _ maintenancePoint) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		ctx, cancel := context.WithCancel(t.Context())
		task, err := fixture.backend.ScheduleCompaction(ctx, storage.CompactionRequest{Target: encTestTarget()})
		if err != nil {
			t.Fatal(err)
		}
		<-started
		cancel()
		if err := task.Wait(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatalf("scheduled cancellation: %v", err)
		}
	})
}

func TestCheckpointAfterCollectionRestoresCompactedBase(t *testing.T) {
	fixture := openBackendFixture(t)
	first := fixture.batch(t, 1, 10, 100, 100)
	second := fixture.batch(t, 2, 20, 90, 110)
	for _, batch := range []storage.MutationBatch{first, second} {
		if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
			t.Fatal(err)
		}
	}
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 10}); err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), 20, 2, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := fixture.backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: 20, Fence: fence}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := checkpoint.Close(); err != nil {
			t.Error(err)
		}
	})
	destination := t.TempDir()
	copyCheckpointFiles(t, checkpoint, destination)
	config := fixture.config
	config.Directory = destination
	restored, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restored.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = restored
	for _, snapshot := range []struct {
		timestamp model.Timestamp
		want      []int64
	}{{10, []int64{100, 100}}, {20, []int64{90, 110}}} {
		reader := fixture.snapshot(t, snapshot.timestamp)
		got := []int64{backendBalance(t, reader, 0), backendBalance(t, reader, 1)}
		if !slices.Equal(got, snapshot.want) {
			t.Fatalf("checkpoint snapshot %d: got %v, want %v", snapshot.timestamp, got, snapshot.want)
		}
	}
	if retry, err := restored.ApplyMutations(t.Context(), second); err != nil || retry.LogPosition != 2 {
		t.Fatalf("compacted checkpoint lost committed retry identity: %+v, %v", retry, err)
	}
}

func runSSTNames(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(directory, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sst") {
			names = append(names, entry.Name())
		}
	}
	slices.Sort(names)
	return names
}
