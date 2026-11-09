package neosepia

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
)

func publicationBackendFixture(t *testing.T, limits Limits) backendFixture {
	t.Helper()
	state := databaseStateFixture()
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Directory: t.TempDir(), Scale: state.Scales[0].Scale, KeyEncoder: backendIntEncoder{}, FenceVerifier: authority, Limits: limits}
	backend, err := Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	return backendFixture{backend: backend, config: config, authority: authority, epoch: state.Closed.Epoch}
}

func admissionChecksum(t *testing.T, batch *storage.MutationBatch) {
	t.Helper()
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
}

func requireAdmissionAborted(t *testing.T, fixture backendFixture, batch storage.MutationBatch, position uint64) {
	t.Helper()
	resolution, err := fixture.backend.ResolveCommit(t.Context(), batch.TxID)
	if err != nil || resolution.State != storage.CommitNotCommitted {
		t.Fatalf("rejected transaction resolution: %+v, %v", resolution, err)
	}
	for _, path := range []string{
		filepath.Join(fixture.config.Directory, "commits", commitName(position)),
		filepath.Join(fixture.config.Directory, "commits", pendingCommitName(position)),
		filepath.Join(fixture.config.Directory, "runs", runManifestName(position)),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("aborted submission left artifact %q: %v", path, err)
		}
	}
	files, err := filepath.Glob(filepath.Join(fixture.config.Directory, "runs", fmt.Sprintf("%020d-*.sst", position)))
	if err != nil || len(files) != 0 {
		t.Errorf("aborted submission left prepared SSTs: %v, %v", files, err)
	}
}

func TestBackendFileAdmissionRejectsBeforeDurableDecision(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxFiles = 2
	fixture := publicationBackendFixture(t, limits)
	initial := fixture.batch(t, 1, 10, 0, 0)
	for i := range initial.Mutations {
		initial.Mutations[i].Op = storage.MutationDelete
		initial.Mutations[i].ColumnIDs = nil
		initial.Mutations[i].Values = nil
	}
	admissionChecksum(t, &initial)
	if _, err := fixture.backend.ApplyMutations(t.Context(), initial); err != nil {
		t.Fatal(err)
	}
	rejected := fixture.batch(t, 2, 20, 90, 110)
	if _, err := fixture.backend.ApplyMutations(t.Context(), rejected); !errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("over-capacity submission must remain abortable: %v", err)
	}
	requireAdmissionAborted(t, fixture, rejected, 2)
	// Collection can reclaim the initial tombstones because rejection did not
	// leave a durable transaction fencing maintenance.
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 10}); err != nil {
		t.Fatal(err)
	}
	if result, err := fixture.backend.ApplyMutations(t.Context(), rejected); err != nil || result.LogPosition != 2 {
		t.Fatalf("write after collection: %+v, %v", result, err)
	}
	reader := fixture.snapshot(t, 20)
	if backendBalance(t, reader, 0) != 90 || backendBalance(t, reader, 1) != 110 {
		t.Fatal("subsequent transaction was not atomically published")
	}
}

func TestBackendPublicationBudgetRejectsBeforeDurableDecision(t *testing.T) {
	limits := DefaultLimits()
	limits.PublicationBytes = 4 << 10
	limits.BlockRows = 1
	fixture := publicationBackendFixture(t, limits)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 100, 100)); err != nil {
		t.Fatal(err)
	}
	rejected := fixture.batch(t, 2, 20, 90, 110)
	for i := 2; i < 80; i++ {
		mutation := rejected.Mutations[i%2]
		mutation.Key = backendKey(int64(i + 1))
		mutation.Ordinal = uint64(i + 1)
		rejected.Mutations = append(rejected.Mutations, mutation)
	}
	admissionChecksum(t, &rejected)
	if _, err := fixture.backend.ApplyMutations(t.Context(), rejected); !errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("publication staging overflow must remain abortable: %v", err)
	}
	requireAdmissionAborted(t, fixture, rejected, 2)
	if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 3, 20, 95, 105)); err != nil {
		t.Fatalf("valid write after staging rejection: %v", err)
	}
}

func TestBackendAdmissionReleasesReservationBeforeDecisionFailure(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "io_failure"
		if canceled {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			limits := DefaultLimits()
			limits.MaxFiles = 2
			fixture := publicationBackendFixture(t, limits)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			failure := errors.New("WAL sync acknowledgement failed")
			fixture.backend.commitFault = func(point commitPoint) error {
				if point != commitFileSynced {
					return nil
				}
				if canceled {
					cancel()
					return nil
				}
				return failure
			}
			batch := fixture.batch(t, 1, 10, 100, 100)
			_, err := fixture.backend.ApplyMutations(ctx, batch)
			want := failure
			if canceled {
				want = context.Canceled
			}
			if !errors.Is(err, want) || errors.Is(err, storage.ErrCommitIndeterminate) {
				t.Fatalf("pre-decision failure: %v, want %v", err, want)
			}
			requireAdmissionAborted(t, fixture, batch, 1)
			fixture.backend.commitFault = nil
			if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
				t.Fatalf("reservation was not released on abort: %v", err)
			}
		})
	}
}

func TestBackendAdmissionSurvivesCanceledPublication(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxFiles = 2
	fixture := publicationBackendFixture(t, limits)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fixture.backend.commitFault = func(point commitPoint) error {
		if point == commitDirectorySynced {
			cancel()
		}
		return nil
	}
	batch := fixture.batch(t, 1, 10, 90, 110)
	if _, err := fixture.backend.ApplyMutations(ctx, batch); !errors.Is(err, storage.ErrCommitIndeterminate) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled publication lost durable outcome: %v", err)
	}
	fixture.backend.commitFault = nil
	for range 2 {
		resolution, err := fixture.backend.ResolveCommit(t.Context(), batch.TxID)
		if err != nil || resolution.State != storage.CommitCommitted || resolution.Result.LogPosition != 1 {
			t.Fatalf("retry publication using retained reservation: %+v, %v", resolution, err)
		}
	}
	reader := fixture.snapshot(t, 10)
	if backendBalance(t, reader, 0) != 90 || backendBalance(t, reader, 1) != 110 {
		t.Fatal("resolved transaction was not atomically published")
	}
}

func TestBackendResidentAdmissionRejectsBeforeDurableDecision(t *testing.T) {
	limits := DefaultLimits()
	limits.ResidentBytes = 2 << 10
	fixture := publicationBackendFixture(t, limits)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 100, 100)); err != nil {
		t.Fatal(err)
	}
	rejected := fixture.batch(t, 2, 20, 90, 110)
	if _, err := fixture.backend.ApplyMutations(t.Context(), rejected); !errors.Is(err, columnar.ErrBatchBudget) || errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("resident capacity overflow must remain abortable: %v", err)
	}
	requireAdmissionAborted(t, fixture, rejected, 2)
}

func TestBackendAdmissionProtectsCapacityFromSnapshotPins(t *testing.T) {
	limits := DefaultLimits()
	limits.ResidentBytes = 4 << 10
	fixture := publicationBackendFixture(t, limits)
	var pins []*runPin
	t.Cleanup(func() {
		for _, pin := range pins {
			if err := pin.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	fixture.backend.commitFault = func(point commitPoint) error {
		if point != commitFileSynced {
			return nil
		}
		// Snapshot/cursor pins share runSet.mu, not the backend writer lock.
		// Exhaust their capacity while the admitted WAL is still abortable.
		for {
			pin, err := fixture.backend.runs.pin(t.Context())
			if errors.Is(err, columnar.ErrBatchBudget) {
				return nil
			}
			if err != nil {
				return err
			}
			pins = append(pins, pin)
		}
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 10, 90, 110)); err != nil {
		t.Fatalf("snapshot pins consumed reserved publication capacity: %v", err)
	}
	for _, pin := range pins {
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	}
	reader := fixture.snapshot(t, 10)
	if backendBalance(t, reader, 0) != 90 || backendBalance(t, reader, 1) != 110 {
		t.Fatal("reserved transaction was not atomically published")
	}
}
