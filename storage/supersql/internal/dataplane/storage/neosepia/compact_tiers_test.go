package neosepia

import (
	"errors"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func applyTierTestBatch(t *testing.T, fixture backendFixture, id byte, ts model.Timestamp, mutations ...storage.Mutation) {
	t.Helper()
	target := encTestTarget()
	for i := range mutations {
		mutations[i].Target = target
		if mutations[i].Ordinal == 0 {
			mutations[i].Ordinal = uint64(i + 1)
		}
	}
	batch := storage.MutationBatch{
		TxID:            model.TxnID{id},
		CommitTS:        ts,
		Epoch:           fixture.epoch,
		EncodingVersion: storage.MutationEncodingVersion,
		Layouts:         []storage.ObjectLayout{{Target: target, Columns: scanFixtureSpec().Layout}},
		Mutations:       mutations,
	}
	var err error
	batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), batch); err != nil {
		t.Fatal(err)
	}
}

func tierUpsert(key string, value int64) storage.Mutation {
	return storage.Mutation{
		Op:        storage.MutationUpsert,
		Key:       []byte(key),
		ColumnIDs: []model.ColumnID{1},
		Values:    []model.Value{model.Int64Val(value)},
	}
}

func tierDeleteRange(start, end string) storage.Mutation {
	return storage.Mutation{
		Op:     storage.MutationDeleteRange,
		Key:    []byte(start),
		EndKey: []byte(end),
	}
}

func verifyTierSnapshot(t *testing.T, fixture backendFixture, ts model.Timestamp, keys []string, wantPoints map[string]int64, wantForward []int64) {
	t.Helper()
	reader := fixture.snapshot(t, ts)
	target := encTestTarget()
	layout := scanFixtureSpec().Layout

	for _, key := range keys {
		row, err := reader.Get(t.Context(), storage.ReadRequest{
			Target:    target,
			Key:       []byte(key),
			ColumnIDs: []model.ColumnID{1},
			Layout:    layout,
		})
		wantVal, wantFound := wantPoints[key]
		if !wantFound {
			if !errors.Is(err, storage.ErrRowNotFound) {
				t.Fatalf("snapshot %d Get(%q): got row=%+v err=%v, want ErrRowNotFound", ts, key, row, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("snapshot %d Get(%q): unexpected error %v (want %d)", ts, key, err, wantVal)
		}
		if len(row.Values) != 1 || row.Values[0].IntVal != wantVal {
			t.Fatalf("snapshot %d Get(%q): got %+v, want %d", ts, key, row.Values, wantVal)
		}
	}

	for _, dir := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
		req := snapshotRequest()
		req.Direction = dir
		plan, err := reader.PlanScan(t.Context(), req)
		if err != nil {
			t.Fatalf("snapshot %d PlanScan(%v): %v", ts, dir, err)
		}
		cursor, err := reader.VectorizedScan(t.Context(), plan)
		if err != nil {
			t.Fatalf("snapshot %d VectorizedScan(%v): %v", ts, dir, err)
		}
		got := collectScanInts(t, cursor)
		if err := cursor.Close(); err != nil {
			t.Fatalf("snapshot %d cursor.Close(%v): %v", ts, dir, err)
		}
		want := slices.Clone(wantForward)
		if dir == storage.ScanReverse {
			slices.Reverse(want)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("snapshot %d scan(%v): got %v, want %v", ts, dir, got, want)
		}
	}

	if err := reader.Close(); err != nil {
		t.Fatalf("snapshot %d reader.Close: %v", ts, err)
	}
}

func TestCompactionFutureDeleteRangePreservesFloorAnchor(t *testing.T) {
	t.Run("future_delete_range_preserves_floor_anchor", func(t *testing.T) {
		fixture := openBackendFixture(t)

		// Historical versions at <= floor (50), including an old DeleteRange at TS=35
		// superseded by an upsert at TS=40.
		applyTierTestBatch(t, fixture, 1, 30,
			tierUpsert("a", 10),
			tierUpsert("b", 20),
			tierUpsert("c", 30),
		)
		applyTierTestBatch(t, fixture, 2, 35,
			tierDeleteRange("b", "c"),
		)
		applyTierTestBatch(t, fixture, 3, 40,
			tierUpsert("b", 25),
		)
		// Future DeleteRange > floor (50), followed by a newer upsert at TS=90.
		applyTierTestBatch(t, fixture, 4, 80,
			tierDeleteRange("a", "d"),
		)
		applyTierTestBatch(t, fixture, 5, 90,
			tierUpsert("a", 99),
		)

		if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 50}); err != nil {
			t.Fatal(err)
		}
		state, err := fixture.backend.State(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if state.HistoryFloor != 50 || fixture.backend.state.BaseGeneration == 0 {
			t.Fatalf("expected compacted state at HistoryFloor=50, got state=%+v baseGen=%d", state, fixture.backend.state.BaseGeneration)
		}

		keys := []string{"a", "b", "c", "d"}
		checkAll := func(f backendFixture) {
			verifyTierSnapshot(t, f, 50, keys, map[string]int64{"a": 10, "b": 25, "c": 30}, []int64{10, 25, 30})
			verifyTierSnapshot(t, f, 80, keys, map[string]int64{}, nil)
			verifyTierSnapshot(t, f, 90, keys, map[string]int64{"a": 99}, []int64{99})
		}
		checkAll(fixture)

		// Verify the inactive file's historyUntil is the earliest superseding timestamp (80, not 90)
		// so advancing the floor to 80 reclaims the inactive SST cleanly.
		pin, err := fixture.backend.runs.pin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var inactiveCount int
		if err := pin.visit(t.Context(), encTestTarget(), func(file *sstFile) error {
			if file.tier == inactiveTier {
				inactiveCount++
				if file.historyUntil != 80 {
					t.Errorf("inactive file %q historyUntil = %d, want 80", file.descriptor.Name, file.historyUntil)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
		if inactiveCount == 0 {
			t.Fatal("expected at least one inactive tier SST holding floor anchors")
		}

		// Reopen and verify persistence of compacted tiers.
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
		checkAll(fixture)

		// Advancing floor to 80 must drop the inactive SST (historyUntil=80 <= 80) while keeping TS=80/90 intact.
		if err := reopened.Collect(t.Context(), storage.GCRequest{Boundary: 80}); err != nil {
			t.Fatal(err)
		}
		verifyTierSnapshot(t, fixture, 80, keys, map[string]int64{}, nil)
		verifyTierSnapshot(t, fixture, 90, keys, map[string]int64{"a": 99}, []int64{99})
	})

	t.Run("overlapping_future_delete_ranges_expire", func(t *testing.T) {
		fixture := openBackendFixture(t)

		applyTierTestBatch(t, fixture, 1, 30,
			tierUpsert("a", 10),
			tierUpsert("b", 20),
			tierUpsert("c", 30),
			tierUpsert("d", 40),
			tierUpsert("e", 50),
		)
		// Overlapping future DeleteRanges:
		//   ["b", "d") at TS=70
		//   ["a", "c") at TS=80 (higher TS, ends earlier at "c")
		// Neither covers "d" or "e".
		applyTierTestBatch(t, fixture, 2, 70,
			tierDeleteRange("b", "d"),
		)
		applyTierTestBatch(t, fixture, 3, 80,
			tierDeleteRange("a", "c"),
		)
		applyTierTestBatch(t, fixture, 4, 90,
			tierUpsert("b", 92),
		)

		if err := fixture.backend.Collect(t.Context(), storage.GCRequest{Boundary: 50}); err != nil {
			t.Fatal(err)
		}

		keys := []string{"a", "b", "c", "d", "e"}
		verifyTierSnapshot(t, fixture, 50, keys, map[string]int64{"a": 10, "b": 20, "c": 30, "d": 40, "e": 50}, []int64{10, 20, 30, 40, 50})
		verifyTierSnapshot(t, fixture, 70, keys, map[string]int64{"a": 10, "d": 40, "e": 50}, []int64{10, 40, 50})
		verifyTierSnapshot(t, fixture, 80, keys, map[string]int64{"d": 40, "e": 50}, []int64{40, 50})
		verifyTierSnapshot(t, fixture, 90, keys, map[string]int64{"b": 92, "d": 40, "e": 50}, []int64{92, 40, 50})
	})
}
