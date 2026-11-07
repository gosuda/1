package neosepia

import (
	"context"
	"errors"
	"math"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestSnapshotReleaseReclaimsProbeBudgets(t *testing.T) {
	for _, budget := range []string{"plan_count", "plan_bytes"} {
		t.Run(budget, func(t *testing.T) {
			view, _ := snapshotFixture(t)
			reader := view.(*snapshotReader)
			request := snapshotRequest()
			planBudget := reader.limits.PlanBytes
			if budget == "plan_bytes" {
				planBudget = minimumReleasePlanBudget(t, reader, request)
			}
			first, err := reader.PlanScan(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if budget == "plan_count" {
				reader.limits.MaxPlans = 1
			} else {
				reader.limits.PlanBytes = planBudget
			}
			if _, err := reader.PlanScan(t.Context(), request); !errors.Is(err, columnar.ErrBatchBudget) && !errors.Is(err, errPredicateBudget) {
				t.Fatalf("retained plan did not exhaust %s: %v", budget, err)
			}
			if err := reader.ReleasePlan(first); err != nil {
				t.Fatal(err)
			}
			for probe := range 128 {
				plan, err := reader.PlanScan(t.Context(), request)
				if err != nil {
					t.Fatalf("probe %d retained released budget: %v", probe, err)
				}
				if cursor, err := reader.VectorizedScan(t.Context(), first); !errors.Is(err, storage.ErrInvalidScanPlan) {
					if cursor != nil {
						snapshotContractClose(t, cursor)
					}
					t.Fatalf("probe %d revived a released binding: %v", probe, err)
				}
				cursor, err := reader.VectorizedScan(t.Context(), plan)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { snapshotContractClose(t, cursor) })
				got := collectScanInts(t, cursor)
				snapshotContractClose(t, cursor)
				if err := reader.ReleasePlan(plan); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got, []int64{10, 20, 30, 40}) {
					t.Fatalf("probe %d: %v", probe, got)
				}
			}
		})
	}
}

func TestSnapshotReleaseRequiresEveryCursorClosed(t *testing.T) {
	reader, _ := snapshotFixture(t)
	foreign, _ := snapshotFixture(t)
	plan, err := reader.PlanScan(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	foreignPlan, err := foreign.PlanScan(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []storage.PushdownPlan{{}, plan} {
		if err := foreign.ReleasePlan(invalid); !errors.Is(err, storage.ErrInvalidScanPlan) {
			t.Fatalf("foreign or unknown release: %v", err)
		}
	}
	foreignCursor, err := foreign.VectorizedScan(t.Context(), foreignPlan)
	if err != nil {
		t.Fatalf("foreign release retired this reader's binding: %v", err)
	}
	snapshotContractClose(t, foreignCursor)
	if err := foreign.ReleasePlan(foreignPlan); err != nil {
		t.Fatal(err)
	}
	first, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snapshotContractClose(t, first) })
	second, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snapshotContractClose(t, second) })
	if err := reader.ReleasePlan(plan); !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("released two active cursors: %v", err)
	}
	snapshotContractClose(t, first)
	snapshotContractClose(t, first)
	if err := reader.ReleasePlan(plan); !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("duplicate Close retired the other cursor: %v", err)
	}
	if got := collectScanInts(t, second); !slices.Equal(got, []int64{10, 20, 30, 40}) {
		t.Fatalf("rejected release invalidated active scan: %v", got)
	}
	snapshotContractClose(t, second)
	reused, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatalf("closed cursors prevented plan reuse: %v", err)
	}
	snapshotContractClose(t, reused)
	tampered := plan
	tampered.Request.Direction = storage.ScanReverse
	if cursor, err := reader.VectorizedScan(t.Context(), tampered); !errors.Is(err, storage.ErrInvalidScanPlan) {
		if cursor != nil {
			snapshotContractClose(t, cursor)
		}
		t.Fatalf("tampered plan opened: %v", err)
	}
	if err := reader.ReleasePlan(plan); err != nil {
		t.Fatal(err)
	}
	if err := reader.ReleasePlan(plan); !errors.Is(err, storage.ErrInvalidScanPlan) {
		t.Fatalf("stale release: %v", err)
	}
	if cursor, err := reader.VectorizedScan(t.Context(), plan); !errors.Is(err, storage.ErrInvalidScanPlan) {
		if cursor != nil {
			snapshotContractClose(t, cursor)
		}
		t.Fatalf("stale scan: %v", err)
	}
}

func TestSnapshotReleaseReclaimsExactMiddleRegistration(t *testing.T) {
	view, _ := snapshotFixture(t)
	reader := view.(*snapshotReader)
	reader.limits.MaxPlans = 3
	request := snapshotRequest()
	request.ColumnIDs = nil
	minimum := minimumReleasePlanBudget(t, reader, request)
	var middleBytes uint64
	plans := make([]storage.PushdownPlan, 3)
	for i := range plans {
		if i == 1 {
			request.ColumnIDs = nil
		} else {
			request.ColumnIDs = snapshotRequest().ColumnIDs
		}
		var err error
		before := reader.planBytes
		plans[i], err = reader.PlanScan(t.Context(), request)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			middleBytes = reader.planBytes - before
		}
	}
	reader.limits.PlanBytes = reader.planBytes + minimum - middleBytes
	if err := reader.ReleasePlan(plans[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.PlanScan(t.Context(), snapshotRequest()); !errors.Is(err, columnar.ErrBatchBudget) && !errors.Is(err, errPredicateBudget) {
		t.Fatalf("middle release reclaimed more than its charge: %v", err)
	}
	request.ColumnIDs = nil
	replacement, err := reader.PlanScan(t.Context(), request)
	if err != nil {
		t.Fatalf("middle release failed to reclaim its charge: %v", err)
	}
	for _, plan := range []storage.PushdownPlan{plans[0], plans[2], replacement} {
		cursor, err := reader.VectorizedScan(t.Context(), plan)
		if err != nil {
			t.Fatal(err)
		}
		snapshotContractClose(t, cursor)
		if err := reader.ReleasePlan(plan); err != nil {
			t.Fatal(err)
		}
	}
}

// Plan construction also needs temporary predicate workspace; the live plan's
// retained charge alone is not sufficient to construct its replacement.
func minimumReleasePlanBudget(t *testing.T, reader *snapshotReader, request storage.PushdownRequest) uint64 {
	t.Helper()
	original := reader.limits.PlanBytes
	defer func() { reader.limits.PlanBytes = original }()
	lower, upper := uint64(1), original
	for lower < upper {
		middle := lower + (upper-lower)/2
		reader.limits.PlanBytes = middle
		plan, err := reader.PlanScan(t.Context(), request)
		if err == nil {
			if err := reader.ReleasePlan(plan); err != nil {
				t.Fatal(err)
			}
			upper = middle
		} else {
			if !errors.Is(err, columnar.ErrBatchBudget) && !errors.Is(err, errPredicateBudget) {
				t.Fatal(err)
			}
			lower = middle + 1
		}
	}
	return lower
}

func TestSnapshotReleaseAfterOpenFailure(t *testing.T) {
	for _, sample := range []struct {
		name    string
		prepare func(*snapshotReader, context.CancelFunc)
		want    error
	}{
		{"canceled", func(_ *snapshotReader, cancel context.CancelFunc) { cancel() }, context.Canceled},
		{"source_budget", func(reader *snapshotReader, _ context.CancelFunc) { reader.limits.SourceBytes = 1 }, columnar.ErrBatchBudget},
	} {
		t.Run(sample.name, func(t *testing.T) {
			view, set := snapshotFixture(t)
			reader := view.(*snapshotReader)
			reader.limits.MaxPlans = 1
			plan, err := reader.PlanScan(t.Context(), snapshotRequest())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sample.prepare(reader, cancel)
			cursor, err := reader.VectorizedScan(ctx, plan)
			if cursor != nil {
				snapshotContractClose(t, cursor)
			}
			if !errors.Is(err, sample.want) {
				t.Fatalf("open: %v, want %v", err, sample.want)
			}
			verifyFailurePlanRetired(t, reader, plan)
			if set.pins != 1 {
				t.Fatalf("failure retained cursor pins: %d", set.pins)
			}
		})
	}
}

func TestSnapshotReleaseAfterIterationFailure(t *testing.T) {
	for _, sample := range []struct {
		name string
		run  func(context.Context, context.CancelFunc, storage.BatchCursor) error
		want error
	}{
		{"canceled", func(ctx context.Context, cancel context.CancelFunc, cursor storage.BatchCursor) error {
			cancel()
			var out columnar.Batch
			return cursor.Next(ctx, &out)
		}, context.Canceled},
		{"invalid_batch", func(ctx context.Context, _ context.CancelFunc, cursor storage.BatchCursor) error {
			return cursor.Next(ctx, nil)
		}, columnar.ErrInvalidBatch},
	} {
		t.Run(sample.name, func(t *testing.T) {
			view, set := snapshotFixture(t)
			reader := view.(*snapshotReader)
			reader.limits.MaxPlans = 1
			plan, err := reader.PlanScan(t.Context(), snapshotRequest())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cursor, err := reader.VectorizedScan(ctx, plan)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { snapshotContractClose(t, cursor) })
			if err := sample.run(ctx, cancel, cursor); !errors.Is(err, sample.want) {
				t.Fatalf("iteration: %v, want %v", err, sample.want)
			}
			snapshotContractClose(t, cursor)
			verifyFailurePlanRetired(t, reader, plan)
			if set.pins != 1 {
				t.Fatalf("failure retained cursor pins: %d", set.pins)
			}
		})
	}
}

func verifyFailurePlanRetired(t *testing.T, reader *snapshotReader, plan storage.PushdownPlan) {
	t.Helper()
	if err := reader.ReleasePlan(plan); err != nil {
		t.Fatal(err)
	}
	replacement, err := reader.PlanScan(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatalf("failure retained plan budget: %v", err)
	}
	if err := reader.ReleasePlan(replacement); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotReleaseSerialDoesNotWrap(t *testing.T) {
	view, _ := snapshotFixture(t)
	reader := view.(*snapshotReader)
	reader.planSerial = math.MaxUint64 - 1
	plan, err := reader.PlanScan(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.ReleasePlan(plan); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.PlanScan(t.Context(), snapshotRequest()); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("exhausted binding serial wrapped: %v", err)
	}
}

func TestSnapshotCursorCloseFailureReleasesOwnership(t *testing.T) {
	view, set := snapshotFixture(t)
	reader := view.(*snapshotReader)
	file := reader.pin.generation.files[0]
	plan, err := reader.PlanScan(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snapshotContractClose(t, cursor) })
	bound := reader.plans
	snapshotContractClose(t, reader)
	snapshotContractClose(t, set)
	if err := file.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cursor.Close(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("lost native close failure: %v", err)
	}
	if bound.active.Load() != 0 || set.pins != 0 {
		t.Fatalf("failed close retained ownership: cursors=%d pins=%d", bound.active.Load(), set.pins)
	}
	snapshotContractClose(t, cursor)
	if err := reader.ReleasePlan(plan); err != nil {
		t.Fatalf("closed-reader cleanup: %v", err)
	}
}

func TestSnapshotReleaseRacesReaderAndCursorClose(t *testing.T) {
	view, set := snapshotFixture(t)
	reader := view.(*snapshotReader)
	file := reader.pin.generation.files[0]
	plan, err := reader.PlanScan(t.Context(), snapshotRequest())
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := reader.VectorizedScan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { snapshotContractClose(t, cursor) })
	start := make(chan struct{})
	var workers sync.WaitGroup
	for _, close := range []func() error{reader.Close, cursor.Close, cursor.Close, func() error { return reader.ReleasePlan(plan) }} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			if err := close(); err != nil && !errors.Is(err, storage.ErrInvalidScanPlan) {
				t.Error(err)
			}
		}()
	}
	close(start)
	workers.Wait()
	if set.pins != 0 {
		t.Fatalf("concurrent cleanup retained %d pins", set.pins)
	}
	snapshotContractClose(t, set)
	if _, err := file.ReadBlock(t.Context(), 0, 1<<20, nil); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("last cursor retained native file: %v", err)
	}
	if _, err := reader.VectorizedScan(t.Context(), plan); !errors.Is(err, storage.ErrClosed) {
		t.Fatalf("closed reader reopened a plan: %v", err)
	}
}
