package neosepia

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestResolveRunsCancellationDoesNotInferAbort(t *testing.T) {
	directory := commitDirectoryFixture(t, t.TempDir())
	set := commitRunSetFixture(t)
	submitted, err := commitMutationRuns(t.Context(), directory, set, 1, validateRunBatch(t, runPreparationFixture(t)), preparationLimits(), 1<<20, 1<<20, func(point commitPoint) error {
		if point == commitPublished {
			return context.Canceled
		}
		return nil
	})
	if !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("submission was not indeterminate: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	resolution, _, err := resolveMutationRuns(ctx, directory, submitted.Decision, preparationLimits(), commitLimits())
	if resolution.State != storage.CommitUnknown || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation inferred an outcome: %+v, %v", resolution, err)
	}
	resolution, files, err := resolveMutationRuns(t.Context(), directory, submitted.Decision, preparationLimits(), commitLimits())
	if err != nil {
		t.Fatal(err)
	}
	if resolution.State != storage.CommitCommitted || resolution.Result.LogPosition != 1 || resolution.Result.CommitTS != 20 || resolution.Result.AppliedCount != 7 {
		t.Fatalf("resolved another commit: %+v", resolution)
	}
	var rows uint64
	for _, file := range files {
		rows += file.Rows
	}
	if rows != resolution.Result.AppliedCount {
		t.Fatalf("resolution omitted durable mutations: %d, want %d", rows, resolution.Result.AppliedCount)
	}
}

func TestResolveRunsDoesNotTreatPreparedManifestAsCommit(t *testing.T) {
	directory := commitDirectoryFixture(t, t.TempDir())
	batch := runPreparationFixture(t)
	identity, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareMutationRuns(t.Context(), directory.runs, 1, batch, identity, preparationLimits())
	if err != nil {
		t.Fatal(err)
	}
	prepared.detach()
	decision := runDecision{Position: 1, TxID: batch.TxID, CommitTS: batch.CommitTS, Identity: identity, MutationCount: batch.Count()}
	resolution, _, err := resolveMutationRuns(t.Context(), directory, decision, preparationLimits(), commitLimits())
	if err != nil {
		t.Fatal(err)
	}
	if resolution.State != storage.CommitNotCommitted {
		t.Fatalf("uncommitted artifacts became a decision: %+v", resolution)
	}
	if _, err := directory.runs.Stat(runManifestName(1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned manifest retained: %v", err)
	}
}

func TestResolveRunsArtifactLossDoesNotUndoDurableDecision(t *testing.T) {
	directory := commitDirectoryFixture(t, t.TempDir())
	set := commitRunSetFixture(t)
	submitted, err := commitMutationRuns(t.Context(), directory, set, 1, validateRunBatch(t, runPreparationFixture(t)), preparationLimits(), 1<<20, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := directory.runs.Remove(runManifestName(1)); err != nil {
		t.Fatal(err)
	}
	resolution, _, err := resolveMutationRuns(t.Context(), directory, submitted.Decision, preparationLimits(), commitLimits())
	if resolution.State != storage.CommitCommitted || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("artifact loss changed the durable outcome: %+v, %v", resolution, err)
	}
}
