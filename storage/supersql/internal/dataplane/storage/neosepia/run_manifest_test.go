package neosepia

import (
	"errors"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func manifestFixture(t *testing.T) (*os.Root, *preparedRuns, runDecision) {
	t.Helper()
	batch := runPreparationFixture(t)
	identity, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	root := commitTestRoot(t)
	prepared, err := prepareMutationRuns(t.Context(), root, 1, batch, identity, preparationLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	})
	return root, prepared, runDecision{Position: 1, TxID: batch.TxID, CommitTS: batch.CommitTS, Identity: identity, MutationCount: batch.Count()}
}

func TestRunManifestRejectsDifferentCommitIdentity(t *testing.T) {
	root, _, decision := manifestFixture(t)
	decision.Identity[0] ^= 1
	if _, err := loadRunManifest(t.Context(), root, decision, preparationLimits()); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("accepted artifacts of another mutation batch: %v", err)
	}
}

func TestRunManifestRejectsCorruptedFileMetadata(t *testing.T) {
	root, _, decision := manifestFixture(t)
	file, err := root.OpenFile(runManifestName(decision.Position), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var value [1]byte
	_, readErr := file.ReadAt(value[:], runManifestHeaderBytes+48)
	value[0] ^= 1
	_, writeErr := file.WriteAt(value[:], runManifestHeaderBytes+48)
	if err := errors.Join(readErr, writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRunManifest(t.Context(), root, decision, preparationLimits()); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("accepted corrupt file checksum metadata: %v", err)
	}
}

func TestPreparedRunCloseRemovesUnpublishedManifest(t *testing.T) {
	root, prepared, _ := manifestFixture(t)
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	directory, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, readErr := directory.ReadDir(-1)
	if err := errors.Join(readErr, directory.Close()); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("unpublished transaction retained artifacts: %v", entries)
	}
}
