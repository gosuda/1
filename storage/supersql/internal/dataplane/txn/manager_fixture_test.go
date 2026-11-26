package txn_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type managerFixtureClock struct {
	mu      sync.Mutex
	reading clock.Reading
	scale   clock.ScaleDescriptor
}

func newManagerFixtureClock() *managerFixtureClock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: sha256.Sum256(history), CanonicalLeapHistory: history}
	return &managerFixtureClock{scale: scale, reading: clock.Reading{Earliest: 100, Latest: 110, Status: clock.Synced, Epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{2}, LeapHistory: scale.LeapHistoryID}}}
}

func (provider *managerFixtureClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	return provider.reading, nil
}

func (provider *managerFixtureClock) WaitUntilAfter(ctx context.Context, target clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.reading.Epoch != epoch {
		return clock.ErrClockEpochChanged
	}
	if provider.reading.Status != clock.Synced {
		return clock.ErrClockUnavailable
	}
	if target < 0 || target == math.MaxInt64 {
		return clock.ErrClockRange
	}
	provider.reading.Earliest = max(provider.reading.Earliest, target+1)
	provider.reading.Latest = max(provider.reading.Latest, provider.reading.Earliest)
	return nil
}

func (provider *managerFixtureClock) Scale() clock.ScaleDescriptor {
	scale := provider.scale
	scale.CanonicalLeapHistory = slices.Clone(scale.CanonicalLeapHistory)
	return scale
}

func (*managerFixtureClock) FromUTC(value time.Time) (clock.Instant, error) {
	if value.Before(time.Unix(0, 0)) || value.After(time.Unix(0, math.MaxInt64)) {
		return 0, clock.ErrClockRange
	}
	return clock.Instant(value.UnixNano()), nil
}

func (*managerFixtureClock) ToUTC(value clock.Instant) (clock.UTCProjection, error) {
	if value < 0 {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	return clock.UTCProjection{Time: time.Unix(0, int64(value)).UTC()}, nil
}

type managerFixtureEncoder struct{}

func (managerFixtureEncoder) EncodePrefix(ctx context.Context, layout []storage.KeyColumn, values []model.Value, maximum uint64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(layout) != 1 || len(values) != 1 || layout[0].Type != (model.Type{ID: 20, Storage: model.TypeInt64}) || values[0].Type != model.TypeInt64 || values[0].IsNull {
		return nil, storage.ErrInvalidScanPlan
	}
	if maximum < 8 {
		return nil, columnar.ErrBatchBudget
	}
	encoded := uint64(values[0].IntVal) ^ (uint64(1) << 63)
	if layout[0].Descending {
		encoded = ^encoded
	}
	return binary.BigEndian.AppendUint64(nil, encoded), nil
}

type managerStoreFixture struct {
	backend   *neosepia.Backend
	config    neosepia.Config
	clock     *managerFixtureClock
	authority *txn.FenceAuthority
	request   storage.ReadRequest
}

func openManagerStore(t *testing.T) managerStoreFixture {
	t.Helper()
	provider := newManagerFixtureClock()
	authority, err := txn.NewFenceAuthority([32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	config := neosepia.Config{Directory: t.TempDir(), Scale: provider.Scale(), KeyEncoder: managerFixtureEncoder{}, FenceVerifier: authority}
	backend, err := neosepia.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	})
	request := storage.ReadRequest{Target: storage.ObjectRef{ObjectID: 1, Generation: 1, DescriptorVersion: 1, EncodingVersion: 1}, Key: binary.BigEndian.AppendUint64(nil, uint64(1)<<63), ColumnIDs: []model.ColumnID{1}, Layout: []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}}}
	batch := storage.MutationBatch{TxID: model.TxnID{1}, CommitTS: 20, Epoch: provider.reading.Epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: request.Target, Columns: request.Layout}}, Mutations: []storage.Mutation{{Op: storage.MutationUpsert, Target: request.Target, Key: request.Key, Ordinal: 1, ColumnIDs: request.ColumnIDs, Values: []model.Value{model.Int64Val(100)}}}}
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
	return managerStoreFixture{backend: backend, config: config, clock: provider, authority: authority, request: request}
}

func (fixture managerStoreFixture) managerConfig(t *testing.T) txn.ManagerConfig {
	t.Helper()
	state, err := fixture.backend.State(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return txn.ManagerConfig{Backend: fixture.backend, Clock: fixture.clock, Authority: fixture.authority, MaxOperations: 8, Recovered: txn.RecoveredState{LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS, HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID}}
}

func (fixture managerStoreFixture) requireBalance(t *testing.T, reader storage.SnapshotReader) {
	t.Helper()
	row, err := reader.Get(t.Context(), fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if row.Deleted || len(row.Values) != 1 || row.Values[0].IsNull || row.Values[0].Type != model.TypeInt64 || row.Values[0].IntVal != 100 {
		t.Fatalf("historical balance: %+v", row)
	}
}
