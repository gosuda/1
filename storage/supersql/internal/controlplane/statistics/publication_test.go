package statistics_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/controlplane/statistics"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage/neosepia"
	"github.com/gosuda/supersql-alpha/internal/dataplane/txn"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type publicationClock struct {
	mu      sync.Mutex
	reading clock.Reading
	scale   clock.ScaleDescriptor
}

func newPublicationClock() *publicationClock {
	history := make([]byte, 13)
	copy(history, "GSTL1")
	binary.BigEndian.PutUint32(history[5:9], 10)
	scale := clock.ScaleDescriptor{Name: clock.ScaleName, LeapHistoryID: sha256.Sum256(history), CanonicalLeapHistory: history}
	return &publicationClock{scale: scale, reading: clock.Reading{Earliest: 100, Latest: 110, Status: clock.Synced, Epoch: clock.Epoch{Assurance: [16]byte{1}, Config: [32]byte{2}, LeapHistory: scale.LeapHistoryID}}}
}
func (c *publicationClock) Read(ctx context.Context) (clock.Reading, error) {
	if err := ctx.Err(); err != nil {
		return clock.Reading{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reading, nil
}
func (c *publicationClock) WaitUntilAfter(ctx context.Context, instant clock.Instant, epoch clock.Epoch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.reading.Epoch {
		return clock.ErrClockEpochChanged
	}
	if instant == math.MaxInt64 {
		return clock.ErrClockRange
	}
	c.reading.Earliest = max(c.reading.Earliest, instant+1)
	c.reading.Latest = max(c.reading.Latest, c.reading.Earliest)
	return nil
}
func (c *publicationClock) Scale() clock.ScaleDescriptor {
	s := c.scale
	s.CanonicalLeapHistory = bytes.Clone(s.CanonicalLeapHistory)
	return s
}
func (*publicationClock) FromUTC(t time.Time) (clock.Instant, error) {
	if t.Before(time.Unix(0, 0)) || t.After(time.Unix(0, math.MaxInt64)) {
		return 0, clock.ErrClockRange
	}
	return clock.Instant(t.UnixNano()), nil
}
func (*publicationClock) ToUTC(t clock.Instant) (clock.UTCProjection, error) {
	if t < 0 {
		return clock.UTCProjection{}, clock.ErrClockRange
	}
	return clock.UTCProjection{Time: time.Unix(0, int64(t)).UTC()}, nil
}

type publicationResolver struct{ layouts []storage.ObjectLayout }

func (r publicationResolver) ResolveMutation(ctx context.Context, target storage.ObjectRef) (*txn.RowNormalizer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, layout := range r.layouts {
		if layout.Target != target {
			continue
		}
		rules := make([]txn.ColumnRule, len(layout.Columns))
		for i, c := range layout.Columns {
			rules[i].Layout = c
		}
		rules[0].KeyPosition = 1
		return txn.NewRowNormalizer(ctx, target, rules, 4<<20)
	}
	return nil, storage.ErrInvalidTarget
}

func TestAnalyzePublicationSurvivesTransactionBackendReopen(t *testing.T) {
	ctx := t.Context()
	keys := encoder(t)
	store, err := statistics.New(ctx, storage.ObjectRef{ObjectID: 900, Generation: 1, DescriptorVersion: 1, EncodingVersion: statistics.EncodingVersion}, keys, limits(), 4096)
	if err != nil {
		t.Fatal(err)
	}
	c := newPublicationClock()
	authority, err := txn.NewFenceAuthority([32]byte{7})
	if err != nil {
		t.Fatal(err)
	}
	config := neosepia.Config{Directory: t.TempDir(), Scale: c.Scale(), KeyEncoder: keys, FenceVerifier: authority}
	var backend *neosepia.Backend
	var manager *txn.Manager
	t.Cleanup(func() {
		if manager != nil {
			if err := manager.Close(); err != nil {
				t.Error(err)
			}
		}
		if backend != nil {
			if err := backend.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	open := func() {
		var err error
		backend, err = neosepia.Open(ctx, config)
		if err != nil {
			t.Fatal(err)
		}
		state, err := backend.State(ctx)
		if err != nil {
			t.Fatal(err)
		}
		manager, err = txn.NewManager(ctx, txn.ManagerConfig{Backend: backend, Clock: c, Authority: authority, MaxOperations: 16, Recovered: txn.RecoveredState{LogPosition: state.LogPosition, CommitTS: state.CommitTS, ClosedTS: state.ClosedTS, HistoryFloor: state.HistoryFloor, Epoch: state.Epoch, LeapHistory: state.Scale.LeapHistoryID}})
		if err != nil {
			t.Fatal(err)
		}
	}
	open()
	o := options()
	layouts := append([]storage.ObjectLayout{{Target: o.Target, Columns: o.Layout}}, store.Layouts()...)
	commit := func(mutations []storage.Mutation) model.Timestamp {
		transaction, err := manager.Begin(ctx, txn.TransactionOptions{Mode: txn.ReadWrite, Resolver: publicationResolver{layouts: layouts}, KeyEncoder: keys, MemoryBytes: 16 << 20, SpillBytes: 16 << 20, SpillDirectory: t.TempDir(), MaxViews: 8})
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := transaction.Close(ctx); err != nil {
				t.Error(err)
			}
		}()
		if err := transaction.ApplyMutations(ctx, mutations); err != nil {
			t.Fatal(err)
		}
		result, err := transaction.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return result.CommitTS
	}
	mutations := make([]storage.Mutation, 40)
	for i := range mutations {
		value := model.Int64Val(int64(i))
		key, err := keys.EncodePrefix(ctx, o.KeyLayout, []model.Value{value}, 4096)
		if err != nil {
			t.Fatal(err)
		}
		mutations[i] = storage.NewInsertMutation(o.Target, key, []model.ColumnID{1}, []model.Value{value})
	}
	o.Snapshot = commit(mutations)
	reader, err := manager.HistoricalSnapshot(ctx, o.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	sampleReader := reader
	defer func() {
		if err := sampleReader.Close(); err != nil {
			t.Error(err)
		}
	}()
	analyzed, err := statistics.Analyze(ctx, reader, keys, o)
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.Snapshot(reader, o.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	change, err := store.Publish(ctx, view, analyzed)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	published := commit(change.Mutations)
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	open()
	reader, err = manager.HistoricalSnapshot(ctx, published)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Error(err)
		}
	}()
	view, err = store.Snapshot(reader, published)
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := view.Lookup(ctx, o.Target)
	if err != nil {
		t.Fatal(err)
	}
	if !found || got.Epoch != o.Epoch || got.Snapshot != o.Snapshot || got.LiveRows != (statistics.Count{Known: true, Value: 40}) {
		t.Fatalf("reopened statistics: found=%t table=%+v", found, got)
	}
	before, err := manager.HistoricalSnapshot(ctx, o.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := before.Close(); err != nil {
			t.Error(err)
		}
	}()
	oldView, err := store.Snapshot(before, o.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := oldView.Lookup(ctx, o.Target); err != nil || found {
		t.Fatalf("statistics visible before publication: found=%t error=%v", found, err)
	}
	wrong := o.Target
	wrong.DescriptorVersion++
	if _, found, err := view.Lookup(ctx, wrong); err != nil || found {
		t.Fatalf("statistics reused across descriptor version: found=%t error=%v", found, err)
	}
}
