package neosepia

import (
	"io"
	"os"
	"os/exec"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

func TestBackendReopenPreservesLiveCheckpointLease(t *testing.T) {
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), 20, 1, fixture.epoch)
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
	for _, file := range checkpointManifest(t, checkpoint).Files {
		stream, err := checkpoint.OpenFile(t.Context(), file.Name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, stream); err != nil {
			stream.Close()
			t.Fatal(err)
		}
		if err := stream.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckpointProcessExitLeavesReclaimableLinks(t *testing.T) {
	if directory := os.Getenv("SUPERSQL_CHECKPOINT_CHILD_DIR"); directory != "" {
		state := databaseStateFixture()
		authority, err := txn.NewFenceAuthority([32]byte{1})
		if err != nil {
			t.Fatal(err)
		}
		backend, err := Open(t.Context(), Config{Directory: directory, Scale: state.Scales[0].Scale, KeyEncoder: backendIntEncoder{}, FenceVerifier: authority})
		if err != nil {
			t.Fatal(err)
		}
		recovered, err := backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		fence, err := authority.Issue(t.Context(), recovered.ClosedTS, recovered.LogPosition, state.Closed.Epoch)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := backend.Checkpoint(t.Context(), storage.CheckpointRequest{Snapshot: storage.SnapshotSpec{Timestamp: recovered.ClosedTS, Fence: fence}}); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCheckpointProcessExitLeavesReclaimableLinks$")
	command.Env = append(os.Environ(), "SUPERSQL_CHECKPOINT_CHILD_DIR="+fixture.config.Directory)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("checkpoint child: %v\n%s", err, output)
	}
	entries, err := os.ReadDir(fixture.config.Directory)
	if err != nil {
		t.Fatal(err)
	}
	abandoned := 0
	for _, entry := range entries {
		if checkpointDirectoryName(entry.Name()) {
			abandoned++
		}
	}
	if abandoned != 1 {
		t.Fatalf("abrupt process exit did not leave its checkpoint namespace: %d", abandoned)
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
	entries, err = os.ReadDir(fixture.config.Directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if checkpointDirectoryName(entry.Name()) {
			t.Fatalf("abandoned checkpoint retained after recovery: %s", entry.Name())
		}
	}
	fixture.backend = reopened
	reader := fixture.snapshot(t, 20)
	if backendBalance(t, reader, 0) != 100 || backendBalance(t, reader, 1) != 100 {
		t.Fatal("checkpoint reclamation removed live database data")
	}
}
