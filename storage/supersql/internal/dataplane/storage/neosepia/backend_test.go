package neosepia

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

var errLostWALAcknowledgement = errors.New("lost WAL acknowledgement")
var errLostMetadataAcknowledgement = errors.New("lost metadata acknowledgement")

type backendIntEncoder struct{}

func (backendIntEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, maxBytes uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(layout) != 1 || len(values) != 1 || layout[0].Type != (model.Type{ID: 20, Storage: model.TypeInt64}) || values[0].Type != model.TypeInt64 || values[0].IsNull {
		return nil, storage.ErrInvalidScanPlan
	}
	if maxBytes < 8 {
		return nil, columnar.ErrBatchBudget
	}
	result := backendKey(values[0].IntVal)
	if layout[0].Descending {
		for i := range result {
			result[i] = ^result[i]
		}
	}
	return result, nil
}

func backendKey(value int64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, uint64(value)^(uint64(1)<<63))
	return key
}

type backendFixture struct {
	backend   *Backend
	config    Config
	authority *txn.FenceAuthority
	epoch     clock.Epoch
}

func openBackendFixture(t *testing.T) backendFixture {
	t.Helper()
	state := databaseStateFixture()
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Directory: t.TempDir(), Scale: state.Scales[0].Scale, KeyEncoder: backendIntEncoder{}, FenceVerifier: authority}
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

func (fixture backendFixture) batch(t *testing.T, id byte, timestamp model.Timestamp, first, second int64) storage.MutationBatch {
	t.Helper()
	batch := storage.MutationBatch{TxID: model.TxnID{id}, CommitTS: timestamp, Epoch: fixture.epoch, EncodingVersion: storage.MutationEncodingVersion}
	for i, value := range []int64{first, second} {
		target := encTestTarget()
		target.ObjectID += model.ObjectID(i)
		batch.Layouts = append(batch.Layouts, storage.ObjectLayout{Target: target, Columns: scanFixtureSpec().Layout})
		batch.Mutations = append(batch.Mutations, storage.Mutation{Op: storage.MutationUpsert, Target: target, Key: backendKey(1), Ordinal: uint64(i + 1), ColumnIDs: []model.ColumnID{1}, Values: []model.Value{model.Int64Val(value)}})
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return batch
}

func (fixture backendFixture) snapshot(t *testing.T, timestamp model.Timestamp) storage.SnapshotReader {
	t.Helper()
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fence, err := fixture.authority.Issue(t.Context(), max(timestamp, state.ClosedTS), state.LogPosition, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: timestamp, Fence: fence})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	})
	return reader
}

func backendBalance(t *testing.T, reader storage.SnapshotReader, object int) int64 {
	t.Helper()
	target := encTestTarget()
	target.ObjectID += model.ObjectID(object)
	row, err := reader.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(1), ColumnIDs: []model.ColumnID{1}, Layout: scanFixtureSpec().Layout})
	if err != nil {
		t.Fatal(err)
	}
	return row.Values[0].IntVal
}

func TestBackendAtomicSnapshotsRestartAndIdempotency(t *testing.T) {
	fixture := openBackendFixture(t)
	first := fixture.batch(t, 1, 20, 100, 100)
	if _, err := fixture.backend.ApplyMutations(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	old := fixture.snapshot(t, 20)
	transfer := fixture.batch(t, 2, 30, 90, 110)
	applied, err := fixture.backend.ApplyMutations(t.Context(), transfer)
	if err != nil {
		t.Fatal(err)
	}
	latest := fixture.snapshot(t, 30)
	if got := []int64{backendBalance(t, old, 0), backendBalance(t, old, 1)}; got[0] != 100 || got[1] != 100 {
		t.Fatalf("old transfer snapshot: %v", got)
	}
	if got := []int64{backendBalance(t, latest, 0), backendBalance(t, latest, 1)}; got[0] != 90 || got[1] != 110 {
		t.Fatalf("new transfer snapshot: %v", got)
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
	if retry, err := reopened.ApplyMutations(t.Context(), transfer); err != nil || retry != applied {
		t.Fatalf("recovered idempotency: %+v, %v", retry, err)
	}
	if _, err := reopened.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 91, 109)); !errors.Is(err, storage.ErrTxnIDReused) {
		t.Fatalf("reused transaction identity: %v", err)
	}
	recovered := fixture.snapshot(t, 30)
	if got := []int64{backendBalance(t, recovered, 0), backendBalance(t, recovered, 1)}; got[0] != 90 || got[1] != 110 {
		t.Fatalf("recovered transfer: %v", got)
	}
	if backendBalance(t, old, 0) != 100 {
		t.Fatal("owner restart invalidated pinned history")
	}
}

func TestBackendUnknownCommitFencesNewWorkUntilResolution(t *testing.T) {
	fixture := openBackendFixture(t)
	fixture.snapshot(t, 10)
	fixture.backend.commitFault = func(point commitPoint) error {
		if point == commitPublished {
			return errLostWALAcknowledgement
		}
		return nil
	}
	submitted := fixture.batch(t, 1, 20, 100, 100)
	if _, err := fixture.backend.ApplyMutations(t.Context(), submitted); !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("uncertain submission: %v", err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 90, 110)); !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("unresolved commit did not fence writes: %v", err)
	}
	historical := fixture.snapshot(t, 10)
	target := encTestTarget()
	if _, err := historical.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(1), ColumnIDs: []model.ColumnID{1}, Layout: scanFixtureSpec().Layout}); !errors.Is(err, storage.ErrRowNotFound) {
		t.Fatalf("unpublished data leaked: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	resolution, err := fixture.backend.ResolveCommit(ctx, submitted.TxID)
	if resolution.State != storage.CommitUnknown || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolution inferred abort: %+v, %v", resolution, err)
	}
	resolution, err = fixture.backend.ResolveCommit(t.Context(), submitted.TxID)
	if err != nil || resolution.State != storage.CommitCommitted {
		t.Fatalf("durable resolution: %+v, %v", resolution, err)
	}
	fixture.backend.commitFault = nil
	reader := fixture.snapshot(t, 20)
	if backendBalance(t, reader, 0) != 100 || backendBalance(t, reader, 1) != 100 {
		t.Fatal("resolution did not publish the complete transaction")
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 90, 110)); err != nil {
		t.Fatal(err)
	}
}

func TestBackendReconcilesUncertainClosedReadBarrier(t *testing.T) {
	fixture := openBackendFixture(t)
	fixture.snapshot(t, 10)
	fixture.backend.stateFault = func(point statePoint) error {
		if point == stateAfterRename {
			return errLostMetadataAcknowledgement
		}
		return nil
	}
	fence, err := fixture.authority.Issue(t.Context(), 20, 0, fixture.epoch)
	if err != nil {
		t.Fatal(err)
	}
	if reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: 20, Fence: fence}); !errors.Is(err, storage.ErrCommitIndeterminate) {
		if reader != nil {
			reader.Close()
		}
		t.Fatalf("uncertain read barrier: %v", err)
	}
	fixture.snapshot(t, 10)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 21, 100, 100)); !errors.Is(err, storage.ErrCommitIndeterminate) {
		t.Fatalf("uncertain barrier did not fence writes: %v", err)
	}
	fixture.backend.stateFault = nil
	fixture.snapshot(t, 20)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); !errors.Is(err, storage.ErrInvalidMutation) {
		t.Fatalf("commit reused closed timestamp: %v", err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 21, 100, 100)); err != nil {
		t.Fatal(err)
	}
}
