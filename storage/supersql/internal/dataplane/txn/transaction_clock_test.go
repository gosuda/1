package txn

import (
	"context"
	"errors"
	"testing"

	"github.com/gosuda/supersql-alpha/clock"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestManagedCommitRejectsUncertifiedClock(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		invalidate func(*clock.Reading)
		want       error
	}{
		{"unavailable", func(reading *clock.Reading) { reading.Status = 0 }, clock.ErrClockUnavailable},
		{"reversed_interval", func(reading *clock.Reading) { reading.Latest = reading.Earliest - 1 }, clock.ErrClockRange},
		{"changed_history_epoch", func(reading *clock.Reading) { reading.Epoch.LeapHistory[0] ^= 1 }, clock.ErrClockEpochChanged},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fixture := newCommitFixture(t, nil)
			writer := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
			contractApply(t, writer, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
			contractValues(t, writer, fixture.request, 90)
			pinned, err := fixture.manager.HistoricalSnapshot(t.Context(), 20)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := pinned.Close(); err != nil {
					t.Error(err)
				}
			})
			before, err := fixture.backend.State(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			fixture.clock.mu.Lock()
			scenario.invalidate(&fixture.clock.reading)
			fixture.clock.mu.Unlock()
			result, err := writer.Commit(t.Context())
			if !errors.Is(err, scenario.want) || errors.Is(err, storage.ErrCommitIndeterminate) || result.LogPosition != 0 {
				t.Fatalf("uncertified commit result=%+v err=%v want=%v", result, err, scenario.want)
			}
			for _, open := range []func(context.Context) (storage.SnapshotReader, error){fixture.manager.StrongSnapshot, func(ctx context.Context) (storage.SnapshotReader, error) {
				return fixture.manager.HistoricalSnapshot(ctx, before.ClosedTS+1)
			}} {
				reader, err := open(t.Context())
				if reader != nil {
					if closeErr := reader.Close(); closeErr != nil {
						t.Error(closeErr)
					}
				}
				if !errors.Is(err, scenario.want) {
					t.Errorf("uncertified new barrier=%v want=%v", err, scenario.want)
				}
			}
			fixture.balance(t, pinned, 100)
			exact, err := fixture.manager.HistoricalSnapshot(t.Context(), 20)
			if err != nil {
				t.Fatal(err)
			}
			fixture.balance(t, exact, 100)
			if err := exact.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := fixture.backend.State(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if after.CommitTS != before.CommitTS || after.LogPosition != before.LogPosition || after.ClosedTS != before.ClosedTS || after.HistoryFloor != before.HistoryFloor {
				t.Fatalf("uncertified work changed durable boundaries: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestManagedCommitAcceptsBoundedHoldover(t *testing.T) {
	fixture := newCommitFixture(t, nil)
	writer := contractBegin(t, fixture.manager, contractBalanceOptions(t, fixture))
	contractApply(t, writer, storage.NewUpdateMutation(fixture.request.Target, fixture.request.Key, fixture.request.ColumnIDs, []model.Value{model.Int64Val(90)}))
	fixture.clock.mu.Lock()
	fixture.clock.reading.Status = clock.Holdover
	fixture.clock.mu.Unlock()
	result, err := writer.Commit(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		timestamp model.Timestamp
		balance   int64
	}{{20, 100}, {result.CommitTS, 90}} {
		reader, err := fixture.manager.HistoricalSnapshot(t.Context(), sample.timestamp)
		if err != nil {
			t.Fatal(err)
		}
		fixture.balance(t, reader, sample.balance)
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := fixture.manager.StrongSnapshot(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	fixture.balance(t, fresh, 90)
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
}
