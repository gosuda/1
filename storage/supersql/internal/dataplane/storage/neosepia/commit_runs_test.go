package neosepia

import (
	"crypto/sha256"
	"errors"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func commitDirectoryFixture(t *testing.T, path string) *databaseDirectory {
	t.Helper()
	directory, err := openDatabaseDirectory(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	return directory
}

func validateRunBatch(t *testing.T, batch storage.MutationBatch) validatedMutationBatch {
	t.Helper()
	validated, err := validateMutationBatch(t.Context(), batch)
	if err != nil {
		t.Fatal(err)
	}
	return validated
}

func commitRunSetFixture(t *testing.T) *runSet {
	t.Helper()
	set, err := newRunSet(runSetLimits{MaxBytes: 2 << 20, MaxFiles: 32, MaxPins: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	return set
}

func TestCommitRunFailureBeforeDecisionDiscardsArtifacts(t *testing.T) {
	directory := commitDirectoryFixture(t, t.TempDir())
	set := commitRunSetFixture(t)
	failure := errors.New("stopped before publication")
	_, err := commitMutationRuns(t.Context(), directory, set, 1, validateRunBatch(t, runPreparationFixture(t)), preparationLimits(), 1<<20, 1<<20, func(point commitPoint) error {
		if point == commitFileSynced {
			return failure
		}
		return nil
	})
	if !errors.Is(err, failure) || errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("pre-decision outcome: %v", err)
	}
	if _, err := directory.commits.Stat(commitName(1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed commit left a published decision: %v", err)
	}
	files, err := directory.runs.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, readErr := files.ReadDir(-1)
	if err := errors.Join(readErr, files.Close()); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("definite failure retained prepared data: %v", entries)
	}
}

func TestCommitRunIndeterminateRetainsRecoverableArtifacts(t *testing.T) {
	for _, point := range []commitPoint{commitPublished, commitDirectorySynced} {
		name := "published_before_directory_sync"
		if point == commitDirectorySynced {
			name = "directory_synced_before_return"
		}
		t.Run(name, func(t *testing.T) {
			path := t.TempDir()
			directory := commitDirectoryFixture(t, path)
			set := commitRunSetFixture(t)
			failure := errors.New("lost acknowledgement")
			submitted, err := commitMutationRuns(t.Context(), directory, set, 1, validateRunBatch(t, runPreparationFixture(t)), preparationLimits(), 1<<20, 1<<20, func(current commitPoint) error {
				if current == point {
					return failure
				}
				return nil
			})
			if !errors.Is(err, failure) || !errors.Is(err, storage.ErrCommitIndeterminate) {
				t.Fatalf("uncertain commit outcome: %v", err)
			}
			if err := set.Close(); err != nil {
				t.Fatal(err)
			}
			if err := directory.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := commitDirectoryFixture(t, path)
			position, err := recoverCommits(t.Context(), reopened.commits, recoveredPosition{}, recoveryLimits{MaxFiles: 16, Batch: commitLimits()}, func(record *storedCommit) error {
				if record.Identity != submitted.Decision.Identity || record.Result.CommitTS != submitted.Decision.CommitTS {
					return storage.ErrCorruptStorage
				}
				files, err := loadRunManifest(t.Context(), reopened.runs, submitted.Decision, preparationLimits())
				if err != nil {
					return err
				}
				var rows uint64
				for _, file := range files {
					data, err := reopened.runs.ReadFile(file.Name)
					if err != nil {
						return err
					}
					if uint64(len(data)) != file.Bytes || sha256.Sum256(data) != file.Checksum {
						return storage.ErrCorruptStorage
					}
					rows += file.Rows
				}
				if rows != record.Result.AppliedCount {
					return storage.ErrCorruptStorage
				}
				return nil
			})
			if err != nil {
				t.Fatalf("uncertain commit lost required artifacts: %v", err)
			}
			if position.LogPosition != 1 || position.CommitTS != 20 {
				t.Fatalf("recovered another decision: %+v", position)
			}
		})
	}
}
