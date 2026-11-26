package neosepia

import (
	"crypto/sha256"
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func runPreparationFixture(t *testing.T) storage.MutationBatch {
	t.Helper()
	target := encTestTarget()
	other := target
	other.ObjectID++
	batch := storage.MutationBatch{TxID: model.TxnID{1}, CommitTS: 20, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: scanFixtureSpec().Layout}, {Target: other, Columns: scanFixtureSpec().Layout}}}
	for i, row := range []struct {
		target   storage.ObjectRef
		key, end string
		op       storage.MutationOp
		value    int64
	}{
		{target, "a", "", storage.MutationUpsert, 10},
		{other, "a", "", storage.MutationUpsert, 100},
		{target, "a", "", storage.MutationUpsert, 20},
		{other, "b", "", storage.MutationUpsert, 200},
		{target, "a", "c", storage.MutationDeleteRange, 0},
		{other, "a", "", storage.MutationUpsert, 300},
		{target, "b", "", storage.MutationUpsert, 30},
	} {
		mutation := scanOwnMutation(row.op, row.key, row.end, row.value)
		mutation.Target = row.target
		mutation.Ordinal = uint64(i + 1)
		batch.Mutations = append(batch.Mutations, mutation)
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	identity, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	root := commitTestRoot(t)
	if _, err := writeCommit(t.Context(), root, 1, batch, identity, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	record, err := openCommit(t.Context(), root, 1, commitLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := record.Close(); err != nil {
			t.Error(err)
		}
	})
	return record.Batch
}

func preparationLimits() runPreparationLimits {
	return runPreparationLimits{MaxBuilders: 1, MaxRuns: 16, MetadataBytes: 1 << 20, Chunk: mutationBlockLimits{BuildBytes: 1 << 20, SortBytes: 1 << 20, Block: encTestLimits()}, File: runLimits()}
}

func TestPreparedRunsStreamAcrossTargetEvictions(t *testing.T) {
	batch := runPreparationFixture(t)
	root := commitTestRoot(t)
	identity, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareMutationRuns(t.Context(), root, 1, batch, identity, preparationLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			t.Error(err)
		}
	})
	prepared.detach()
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := loadRunManifest(t.Context(), root, runDecision{Position: 1, TxID: batch.TxID, CommitTS: batch.CommitTS, Identity: identity, MutationCount: batch.Count()}, preparationLimits())
	if err != nil {
		t.Fatal(err)
	}
	runs := make(map[storage.ObjectRef][]*sstFile)
	for _, descriptor := range files {
		data, err := root.ReadFile(descriptor.Name)
		if err != nil {
			t.Fatal(err)
		}
		if uint64(len(data)) != descriptor.Bytes || sha256.Sum256(data) != descriptor.Checksum {
			t.Fatal("prepared file integrity metadata did not describe the published bytes")
		}
		file, err := root.Open(descriptor.Name)
		if err != nil {
			t.Fatal(err)
		}
		run, err := openSST(t.Context(), file, runLimits())
		if err != nil {
			t.Fatal(errors.Join(err, file.Close()))
		}
		t.Cleanup(func() {
			if err := run.Close(); err != nil {
				t.Error(err)
			}
		})
		runs[descriptor.Target] = append(runs[descriptor.Target], run)
	}
	for i, layout := range batch.Layouts {
		expected := []visibleVersion{{"b", 30}}
		if i == 1 {
			expected = []visibleVersion{{"a", 300}, {"b", 200}}
		}
		if got := visibleRunVersions(t, runs[layout.Target], 20); !slices.Equal(got, expected) {
			t.Fatalf("object %d visible rows = %v, want %v", layout.Target.ObjectID, got, expected)
		}
	}
}

func TestPreparedRunsFailureRemovesOnlyOwnedArtifacts(t *testing.T) {
	root := commitTestRoot(t)
	if err := root.WriteFile("keep", []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	limits := preparationLimits()
	limits.MaxRuns = 1
	batch := runPreparationFixture(t)
	identity, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	_, err = prepareMutationRuns(t.Context(), root, 1, batch, identity, limits)
	if !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("run count limit: %v", err)
	}
	directory, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, readErr := directory.ReadDir(-1)
	if err := errors.Join(readErr, directory.Close()); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatalf("failed preparation leaked or deleted files: %v", entries)
	}
	data, err := root.ReadFile("keep")
	if err != nil || string(data) != "unrelated" {
		t.Fatalf("unrelated file changed: %q, %v", data, err)
	}
}
