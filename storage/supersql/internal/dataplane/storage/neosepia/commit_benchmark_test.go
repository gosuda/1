package neosepia

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

// BenchmarkBackendApplyMutations measures one durable commit of the fixture
// batch shape, so the per-commit cost of the soak workloads is measured rather
// than guessed.
func BenchmarkBackendApplyMutations(b *testing.B) {
	state := databaseStateFixture()
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		b.Fatal(err)
	}
	config := Config{Directory: b.TempDir(), Scale: state.Scales[0].Scale, KeyEncoder: backendIntEncoder{}, FenceVerifier: authority}
	backend, err := Open(b.Context(), config)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() {
		if err := backend.Close(); err != nil {
			b.Error(err)
		}
	})
	ctx := context.Background()
	for i := 0; b.Loop(); i++ {
		batch := maintenanceBatch(b, state.Closed.Epoch, i+1)
		if _, err := backend.ApplyMutations(ctx, batch); err != nil {
			b.Fatal(err)
		}
	}
}

// maintenanceBatch builds one two-row batch with a distinct durable identity, so
// sustained maintenance workloads never repeat a transaction identity.
func maintenanceBatch(t testing.TB, epoch clock.Epoch, id int) storage.MutationBatch {
	t.Helper()
	var identity model.TxnID
	binary.BigEndian.PutUint64(identity[8:], uint64(id))
	batch := storage.MutationBatch{TxID: identity, CommitTS: model.Timestamp(id) * 10, Epoch: epoch, EncodingVersion: storage.MutationEncodingVersion}
	for i, value := range []int64{int64(id), int64(id + 1)} {
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
