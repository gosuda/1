package neosepia

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func atomicRecoveryBatch(t *testing.T, fixture backendFixture, id byte, timestamp model.Timestamp, transferred bool) storage.MutationBatch {
	t.Helper()
	first, second, revision := int64(100), int64(100), int64(1)
	if transferred {
		first, second, revision = 90, 110, 2
	}
	batch := fixture.batch(t, id, timestamp, first, second)
	index, catalog := encTestTarget(), encTestTarget()
	index.ObjectID += 2
	catalog.ObjectID += 3
	batch.Layouts = append(batch.Layouts,
		storage.ObjectLayout{Target: index, Columns: scanFixtureSpec().Layout},
		storage.ObjectLayout{Target: catalog, Columns: scanFixtureSpec().Layout},
	)
	// The account-ID index includes each account's balance; catalog stores its revision.
	for _, row := range []struct {
		target     storage.ObjectRef
		key, value int64
	}{{index, 1, first}, {index, 2, second}, {catalog, 1, revision}} {
		mutation := storage.NewUpsertMutation(row.target, backendKey(row.key), []model.ColumnID{1}, []model.Value{model.Int64Val(row.value)})
		mutation.Ordinal = uint64(len(batch.Mutations) + 1)
		batch.Mutations = append(batch.Mutations, mutation)
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func requireAtomicRecoveryState(t *testing.T, reader storage.SnapshotReader, transferred bool) {
	t.Helper()
	first, second, revision := int64(100), int64(100), int64(1)
	if transferred {
		first, second, revision = 90, 110, 2
	}
	for _, want := range []struct {
		name       string
		object     int
		key, value int64
	}{
		{"debit account", 0, 1, first},
		{"credit account", 1, 1, second},
		{"covering index debit", 2, 1, first},
		{"covering index credit", 2, 2, second},
		{"catalog revision", 3, 1, revision},
	} {
		target := encTestTarget()
		target.ObjectID += model.ObjectID(want.object)
		row, err := reader.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(want.key), ColumnIDs: []model.ColumnID{1}, Layout: scanFixtureSpec().Layout})
		if err != nil {
			t.Fatalf("%s: %v", want.name, err)
		}
		if len(row.Values) != 1 {
			t.Fatalf("%s: got %d values, want one", want.name, len(row.Values))
		}
		value := row.Values[0]
		if value.IsNull || value.Type != model.TypeInt64 || value.IntVal != want.value {
			t.Errorf("%s: got %+v, want int64 %d", want.name, value, want.value)
		}
	}
}

func reopenAtomicRecoveryFixture(t *testing.T, fixture backendFixture) backendFixture {
	t.Helper()
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend, err := Open(t.Context(), fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	fixture.backend = backend
	return fixture
}

func crashAtomicRecovery(t *testing.T, fixture backendFixture, point commitPoint, write int) {
	t.Helper()
	if err := fixture.backend.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAtomicCommitRecoveryCrashHelper$")
	command.Env = []string{
		"NEOSEPIA_ATOMIC_DIRECTORY=" + fixture.config.Directory,
		"NEOSEPIA_ATOMIC_POINT=" + strconv.Itoa(int(point)),
		"NEOSEPIA_ATOMIC_WRITE=" + strconv.Itoa(write),
		"TMPDIR=" + t.TempDir(),
		"GOMAXPROCS=2",
	}
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("crash point %d write %d: child did not terminate at checkpoint: %v\n%s", point, write, err, output)
	}
}

func requireCompletePendingBody(t *testing.T, fixture backendFixture, batch storage.MutationBatch) {
	t.Helper()
	file, err := os.Open(filepath.Join(fixture.config.Directory, "commits", pendingCommitName(2)))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	length := info.Size() - commitHeaderSize
	if length <= 0 {
		t.Fatalf("missing uncommitted mutation body: %d bytes", length)
	}
	decoded, err := storage.OpenEncodedMutationBatch(t.Context(), io.NewSectionReader(file, commitHeaderSize, length), uint64(length), commitLimits())
	if err != nil {
		t.Fatalf("pending segment does not contain a complete canonical body: %v", err)
	}
	identity, err := decoded.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want, err := batch.Identity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if identity != want || decoded.Count() != batch.Count() {
		t.Fatal("complete uncommitted body differs from the submitted transaction")
	}
	if _, err := os.Stat(filepath.Join(fixture.config.Directory, "commits", commitName(2))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted body acquired a decision filename: %v", err)
	}
}

func TestAtomicCommitRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	bodyWrites := 0
	t.Run("measure_canonical_body_boundaries", func(t *testing.T) {
		fixture := openBackendFixture(t)
		if _, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 1, 20, false)); err != nil {
			t.Fatal(err)
		}
		fixture.backend.commitFault = func(point commitPoint) error {
			if point == commitBodyWrite {
				bodyWrites++
			}
			return nil
		}
		if _, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 2, 30, true)); err != nil {
			t.Fatal(err)
		}
		requireAtomicRecoveryState(t, fixture.snapshot(t, 30), true)
		if bodyWrites == 0 {
			t.Fatal("no canonical body write boundaries observed")
		}
		t.Logf("crashing after each of %d canonical body writes", bodyWrites)
	})
	type crashCase struct {
		name      string
		point     commitPoint
		write     int
		committed bool
	}
	cases := []crashCase{
		{name: "BEGIN", point: commitBeginWritten},
		{name: "complete_uncommitted_body", point: commitBodyWritten},
		{name: "COMMIT_before_file_sync", point: commitMarkerWritten},
		{name: "file_synced_without_decision_filename", point: commitFileSynced},
		{name: "decision_filename_before_directory_sync", point: commitPublished, committed: true},
		{name: "file_and_decision_directory_synced", point: commitDirectorySynced, committed: true},
		{name: "before_reader_root_publication", point: commitBeforeReaderPublication, committed: true},
		{name: "after_reader_root_publication", point: commitReaderPublished, committed: true},
	}
	for write := 1; write <= bodyWrites; write++ {
		cases = append(cases, crashCase{name: fmt.Sprintf("body_write_%03d", write), point: commitBodyWrite, write: write})
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			fixture := openBackendFixture(t)
			if _, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 1, 20, false)); err != nil {
				t.Fatal(err)
			}
			transfer := atomicRecoveryBatch(t, fixture, 2, 30, true)
			crashAtomicRecovery(t, fixture, test.point, test.write)
			if test.point == commitBodyWritten {
				requireCompletePendingBody(t, fixture, transfer)
			}
			fixture = reopenAtomicRecoveryFixture(t, fixture)
			requireAtomicRecoveryState(t, fixture.snapshot(t, 30), test.committed)
			resolution, err := fixture.backend.ResolveCommit(t.Context(), transfer.TxID)
			want := storage.CommitNotCommitted
			position := uint64(1)
			if test.committed {
				want, position = storage.CommitCommitted, 2
			}
			if err != nil || resolution.State != want {
				t.Fatalf("recovered decision: %+v, %v; want %v", resolution, err, want)
			}
			state, err := fixture.backend.State(t.Context())
			if err != nil || state.LogPosition != position {
				t.Fatalf("recovered frontier: %+v, %v; want position %d", state, err, position)
			}
			if _, err := os.Stat(filepath.Join(fixture.config.Directory, "commits", pendingCommitName(2))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("recovery retained pending segment: %v", err)
			}
		})
	}

	t.Run("committed_replay_is_idempotent_by_TxID", func(t *testing.T) {
		fixture := openBackendFixture(t)
		if _, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 1, 20, false)); err != nil {
			t.Fatal(err)
		}
		transfer := atomicRecoveryBatch(t, fixture, 2, 30, true)
		crashAtomicRecovery(t, fixture, commitDirectorySynced, 0)
		want := storage.ApplyResult{AppliedCount: transfer.Count(), CommitTS: 30, ChecksumCRC: transfer.ChecksumCRC, LogPosition: 2}
		for range 2 {
			fixture = reopenAtomicRecoveryFixture(t, fixture)
			for range 2 {
				result, err := fixture.backend.ApplyMutations(t.Context(), transfer)
				if err != nil || result != want {
					t.Fatalf("replayed transaction: %+v, %v; want %+v", result, err, want)
				}
			}
			if _, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 2, 30, false)); !errors.Is(err, storage.ErrTxnIDReused) {
				t.Fatalf("changed transaction reused durable TxID: %v", err)
			}
			requireAtomicRecoveryState(t, fixture.snapshot(t, 30), true)
		}
		result, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 3, 40, false))
		if err != nil || result.LogPosition != 3 {
			t.Fatalf("replay consumed another decision position: %+v, %v", result, err)
		}
		requireAtomicRecoveryState(t, fixture.snapshot(t, 40), false)
	})

	t.Run("interior_published_WAL_corruption_fails_Open", func(t *testing.T) {
		fixture := openBackendFixture(t)
		for _, batch := range []storage.MutationBatch{
			atomicRecoveryBatch(t, fixture, 1, 20, false),
			atomicRecoveryBatch(t, fixture, 2, 30, true),
			atomicRecoveryBatch(t, fixture, 3, 40, false),
		} {
			if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
				t.Fatal(err)
			}
		}
		if err := fixture.backend.Close(); err != nil {
			t.Fatal(err)
		}
		file, err := os.OpenFile(filepath.Join(fixture.config.Directory, "commits", commitName(2)), os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		var value [1]byte
		if _, err := file.ReadAt(value[:], commitHeaderSize+20); err != nil {
			t.Fatal(errors.Join(err, file.Close()))
		}
		value[0] ^= 1
		_, writeErr := file.WriteAt(value[:], commitHeaderSize+20)
		if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
			t.Fatal(err)
		}
		backend, err := Open(t.Context(), fixture.config)
		if backend != nil {
			if closeErr := backend.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if !errors.Is(err, storage.ErrCorruptStorage) {
			t.Fatalf("interior published corruption became a truncated recovery: %v", err)
		}
	})

	t.Run("post_directory_sync_cancellation_requires_resolution", func(t *testing.T) {
		fixture := openBackendFixture(t)
		if _, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 1, 20, false)); err != nil {
			t.Fatal(err)
		}
		old := fixture.snapshot(t, 20)
		transfer := atomicRecoveryBatch(t, fixture, 2, 30, true)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		fixture.backend.commitFault = func(point commitPoint) error {
			if point == commitDirectorySynced {
				cancel()
			}
			return nil
		}
		if _, err := fixture.backend.ApplyMutations(ctx, transfer); !errors.Is(err, storage.ErrCommitIndeterminate) || !errors.Is(err, context.Canceled) {
			t.Fatalf("post-durability cancellation: %v", err)
		}
		fixture.backend.commitFault = nil
		requireAtomicRecoveryState(t, old, false)
		next := atomicRecoveryBatch(t, fixture, 3, 40, false)
		if _, err := fixture.backend.ApplyMutations(t.Context(), next); !errors.Is(err, storage.ErrCommitIndeterminate) {
			t.Fatalf("new write bypassed unresolved durable TxID: %v", err)
		}
		resolution, err := fixture.backend.ResolveCommit(ctx, transfer.TxID)
		if !errors.Is(err, context.Canceled) || resolution.State != storage.CommitUnknown {
			t.Fatalf("canceled resolution inferred a decision: %+v, %v", resolution, err)
		}
		for range 2 {
			resolution, err = fixture.backend.ResolveCommit(t.Context(), transfer.TxID)
			if err != nil || resolution.State != storage.CommitCommitted || resolution.Result.LogPosition != 2 || resolution.Result.AppliedCount != transfer.Count() {
				t.Fatalf("durable TxID resolution: %+v, %v", resolution, err)
			}
		}
		requireAtomicRecoveryState(t, fixture.snapshot(t, 30), true)
		requireAtomicRecoveryState(t, old, false)
		if result, err := fixture.backend.ApplyMutations(t.Context(), next); err != nil || result.LogPosition != 3 {
			t.Fatalf("resolved transaction still blocks later write: %+v, %v", result, err)
		}
		requireAtomicRecoveryState(t, fixture.snapshot(t, 40), false)
	})

	for _, point := range []commitPoint{commitBeforeReaderPublication, commitReaderPublished} {
		name := "reader_root_failure_preserves_retryable_admission"
		if point == commitReaderPublished {
			name = "reader_root_ack_failure_preserves_installed_decision"
		}
		t.Run(name, func(t *testing.T) {
			fixture := openBackendFixture(t)
			if _, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 1, 20, false)); err != nil {
				t.Fatal(err)
			}
			failure := errors.New("reader root acknowledgement lost")
			fixture.backend.commitFault = func(current commitPoint) error {
				if current == point {
					return failure
				}
				return nil
			}
			transfer := atomicRecoveryBatch(t, fixture, 2, 30, true)
			if _, err := fixture.backend.ApplyMutations(t.Context(), transfer); !errors.Is(err, storage.ErrCommitIndeterminate) || !errors.Is(err, failure) {
				t.Fatalf("reader publication failure: %v", err)
			}
			fixture.backend.commitFault = nil
			for range 2 {
				resolution, err := fixture.backend.ResolveCommit(t.Context(), transfer.TxID)
				if err != nil || resolution.State != storage.CommitCommitted || resolution.Result.LogPosition != 2 {
					t.Fatalf("reader publication orphaned durable admission: %+v, %v", resolution, err)
				}
			}
			requireAtomicRecoveryState(t, fixture.snapshot(t, 30), true)
			if result, err := fixture.backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 3, 40, false)); err != nil || result.LogPosition != 3 {
				t.Fatalf("reader publication leaked admission capacity: %+v, %v", result, err)
			}
			requireAtomicRecoveryState(t, fixture.snapshot(t, 40), false)
		})
	}
}

func TestAtomicCommitRecoveryCrashHelper(t *testing.T) {
	directory := os.Getenv("NEOSEPIA_ATOMIC_DIRECTORY")
	if directory == "" {
		t.Skip("subprocess helper")
	}
	point, err := strconv.ParseUint(os.Getenv("NEOSEPIA_ATOMIC_POINT"), 10, 8)
	if err != nil {
		t.Fatal(err)
	}
	write, err := strconv.Atoi(os.Getenv("NEOSEPIA_ATOMIC_WRITE"))
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
	fixture := backendFixture{backend: backend, config: config, authority: authority, epoch: state.Closed.Epoch}
	writes := 0
	backend.commitFault = func(current commitPoint) error {
		if current == commitBodyWrite {
			writes++
		}
		if current == commitPoint(point) && (current != commitBodyWrite || writes == write) {
			os.Exit(23)
		}
		return nil
	}
	_, err = backend.ApplyMutations(t.Context(), atomicRecoveryBatch(t, fixture, 2, 30, true))
	t.Fatalf("crash checkpoint was not reached: %v", err)
}
