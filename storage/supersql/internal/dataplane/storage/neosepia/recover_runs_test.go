package neosepia

import (
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestRecoveryPublishesCompleteNativeFileGroups(t *testing.T) {
	path := t.TempDir()
	directory, err := openDatabaseDirectory(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	batch := runPreparationFixture(t)
	staging := commitRunSetFixture(t)
	committed, err := commitMutationRuns(t.Context(), directory, staging, 1, validateRunBatch(t, batch), preparationLimits(), 1<<20, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := staging.Close(); err != nil {
		t.Fatal(err)
	}
	if err := directory.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openDatabaseDirectory(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	limits := recoveryLimits{Batch: commitLimits(), MaxFiles: 16}
	resident := runSetLimits{MaxBytes: 1 << 20, MaxFiles: 16, MaxPins: 4}
	recoveryBudget := runRecoveryLimits{Log: limits, Preparation: preparationLimits(), Resident: resident, StagingBytes: 1 << 19, IdentityBytes: 1 << 19}
	recovered, err := recoverPublishedRuns(t.Context(), reopened, databaseState{}, recoveryBudget, nil)
	if err != nil {
		t.Fatal(err)
	}
	set, position := recovered.Set, recovered.Position
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	if position.LogPosition != 1 || position.CommitTS != 20 {
		t.Fatalf("recovered frontier: %+v", position)
	}
	pin, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pin.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := runPinTargetValues(t, pin, batch.Layouts[0].Target); !slices.Equal(got, []int64{30}) {
		t.Fatalf("recovered base values: %v", got)
	}
	if got := runPinTargetValues(t, pin, batch.Layouts[1].Target); !slices.Equal(got, []int64{300, 200}) {
		t.Fatalf("recovered related values: %v", got)
	}
	if err := pin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := set.Close(); err != nil {
		t.Fatal(err)
	}
	files, err := loadRunManifest(t.Context(), reopened.runs, committed.Decision, preparationLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.runs.Remove(files[len(files)-1].Name); err != nil {
		t.Fatal(err)
	}
	partial, err := recoverPublishedRuns(t.Context(), reopened, databaseState{}, recoveryBudget, nil)
	if partial.Set != nil {
		t.Fatalf("artifact loss returned a partially recovered database: %v", errors.Join(err, partial.Set.Close()))
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing committed run: %v", err)
	}
}

func TestRecoveryRejectsRepeatedTransactionIdentity(t *testing.T) {
	directory, err := openDatabaseDirectory(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := directory.Close(); err != nil {
			t.Error(err)
		}
	})
	batch := runPreparationFixture(t)
	staging := commitRunSetFixture(t)
	first, err := commitMutationRuns(t.Context(), directory, staging, 1, validateRunBatch(t, batch), preparationLimits(), 1<<20, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Admission.publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch.CommitTS = 30
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commitMutationRuns(t.Context(), directory, staging, 2, validateRunBatch(t, batch), preparationLimits(), 1<<20, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	recovered, err := recoverPublishedRuns(t.Context(), directory, databaseState{}, runRecoveryLimits{
		Log: recoveryLimits{Batch: commitLimits(), MaxFiles: 16}, Preparation: preparationLimits(),
		Resident: runSetLimits{MaxBytes: 1 << 20, MaxFiles: 32, MaxPins: 4}, StagingBytes: 1 << 19, IdentityBytes: 1 << 19,
	}, nil)
	if recovered.Set != nil {
		t.Fatalf("repeated transaction identity returned a recovered database: %v", errors.Join(err, recovered.Set.Close()))
	}
	if !errors.Is(err, storage.ErrCorruptStorage) || !errors.Is(err, storage.ErrTxnIDReused) {
		t.Fatalf("repeated durable transaction identity: %v", err)
	}
}
