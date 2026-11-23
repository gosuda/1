package neosepia

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestNeoSepiaMixedWorkload(t *testing.T) {
	if os.Getenv("NEOSEPIA_MIXED") != "1" { t.Skip("set NEOSEPIA_MIXED=1 to run the bounded mixed smoke") }
	const payloadBytes = 64 << 10
	logicalBytes := int64(512 << 10)
	if configured := os.Getenv("NEOSEPIA_MIXED_BYTES"); configured != "" {
		var err error
		logicalBytes, err = strconv.ParseInt(configured, 10, 64)
		if err != nil { t.Fatal(err) }
	}
	if logicalBytes < payloadBytes || logicalBytes > 4<<20 || logicalBytes%payloadBytes != 0 {
		t.Fatal("NEOSEPIA_MIXED_BYTES must be a multiple of 65536 between 65536 and 4194304; the task fixture cap is 64MiB")
	}
	totalRows := logicalBytes/payloadBytes
	fixture := openBackendFixture(t)
	const batchRows = 32
	target := encTestTarget()
	layout := []storage.ColumnLayout{{ID: 1, Type: model.Type{ID: 20, Storage: model.TypeInt64}}, {ID: 2, Type: model.Type{ID: 17, Storage: model.TypeBytes}}}
	var position uint64
	apply := func(first, count int64) error {
		position++
		batch := storage.MutationBatch{CommitTS: model.Timestamp(position), Epoch: fixture.epoch, EncodingVersion: storage.MutationEncodingVersion, Layouts: []storage.ObjectLayout{{Target: target, Columns: layout}}}
		binary.LittleEndian.PutUint64(batch.TxID[:], position)
		batch.Mutations = make([]storage.Mutation, count)
		for i := int64(0); i < count; i++ {
			key := (first+i)%totalRows
			payload := make([]byte, payloadBytes)
			state := uint64(key+1)^position*0x9e3779b97f4a7c15
			for j := 0; j < len(payload); j += 8 {
				state ^= state << 13; state ^= state >> 7; state ^= state << 17
				binary.LittleEndian.PutUint64(payload[j:], state)
			}
			mutation := storage.NewUpsertMutation(target, backendKey(key), []model.ColumnID{1, 2}, []model.Value{model.Int64Val(key), model.BytesVal(payload)})
			mutation.Ordinal = uint64(i+1)
			batch.Mutations[i] = mutation
		}
		var err error
		batch.ChecksumCRC, err = batch.ComputeChecksum(t.Context())
		if err != nil { return err }
		_, err = fixture.backend.ApplyMutations(t.Context(), batch)
		return err
	}
	seedStart := time.Now()
	for first := int64(0); first < totalRows; first += batchRows {
		if err := apply(first, min(batchRows, totalRows-first)); err != nil { t.Fatalf("seed row %d: %v", first, err) }
	}
	t.Logf("seeded %d logical payload bytes in %s", uint64(totalRows)*payloadBytes, time.Since(seedStart))
	historical := fixture.snapshot(t, model.Timestamp(position))
	request := storage.PushdownRequest{Target: target, Layout: layout, ColumnIDs: []model.ColumnID{1}, KeyLayout: []storage.KeyColumn{{ColumnID: 1, Type: layout[0].Type}}, Spans: []storage.KeySpan{{Lower: storage.KeyBound{Unbounded: true}, Upper: storage.KeyBound{Unbounded: true}}}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scans := make(chan error, 1)
	firstScan := make(chan struct{})
	var completedScans atomic.Uint64
	go func() {
		for {
			if err := ctx.Err(); err != nil { scans <- nil; return }
			plan, err := historical.PlanScan(ctx, request)
			if err != nil { if ctx.Err() != nil { scans <- nil } else { scans <- err }; return }
			cursor, err := historical.VectorizedScan(ctx, plan)
			if err != nil {
				releaseErr := historical.ReleasePlan(plan)
				if ctx.Err() != nil { scans <- releaseErr } else { scans <- errors.Join(err, releaseErr) }
				return
			}
			var count, sum int64
			out := columnar.Batch{MaxRows: 1024}
			for {
				err = cursor.Next(ctx, &out)
				if err != nil { break }
				for row := range out.Rows() {
					value, _, valueErr := columnar.CopyValue(ctx, &out.Columns[0], out.Lane(row), 1024)
					if valueErr != nil { err = valueErr; break }
					count++; sum += value.IntVal
				}
				if err != nil { break }
			}
			closeErr := errors.Join(cursor.Close(), historical.ReleasePlan(plan))
			if ctx.Err() != nil { scans <- closeErr; return }
			if err != io.EOF || closeErr != nil { scans <- errors.Join(err, closeErr); return }
			if count != totalRows || sum != int64(totalRows)*(totalRows-1)/2 { scans <- fmt.Errorf("historical checksum: rows=%d sum=%d", count, sum); return }
			if completedScans.Add(1) == 1 { close(firstScan) }
		}
	}()
	joined := false
	defer func() { if !joined { cancel(); if err := <-scans; err != nil { t.Error(err) } } }()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	latencies := make([]time.Duration, 0, 8)
	start := time.Now()
	var writes uint64
	for writes < 8 {
		key := int64((writes*7919)%uint64(totalRows))
		if err := apply(key, 1); err != nil { t.Fatal(err) }
		writes++
		pointStart := time.Now()
		state, err := fixture.backend.State(t.Context())
		if err != nil { t.Fatal(err) }
		fence, err := fixture.authority.Issue(t.Context(), state.ClosedTS, state.LogPosition, fixture.epoch)
		if err != nil { t.Fatal(err) }
		reader, err := fixture.backend.OpenSnapshot(t.Context(), storage.SnapshotSpec{Timestamp: state.CommitTS, Fence: fence})
		if err != nil { t.Fatal(err) }
		row, readErr := reader.Get(t.Context(), storage.ReadRequest{Target: target, Key: backendKey(key), Layout: layout, ColumnIDs: []model.ColumnID{1}})
		if err := errors.Join(readErr, reader.Close()); err != nil { t.Fatal(err) }
		if len(row.Values) != 1 || row.Values[0].IntVal != key { t.Fatalf("point key %d returned %+v", key, row) }
		if len(latencies) < cap(latencies) { latencies = append(latencies, time.Since(pointStart)) }
	}
	select {
	case <-firstScan:
	case err := <-scans:
		joined = true
		if err != nil { t.Fatal(err) }
		t.Fatal("historical scan stopped before verifying a complete result")
	}
	cancel()
	scanErr := <-scans
	joined = true
	if scanErr != nil { t.Fatal(scanErr) }
	if completedScans.Load() == 0 { t.Fatal("mixed workload completed no checksum-verified historical scan") }
	if err := historical.Close(); err != nil { t.Fatal(err) }
	runtime.ReadMemStats(&after)
	slices.Sort(latencies)
	if len(latencies) == 0 { t.Fatal("mixed workload completed no point reads") }
	p99 := latencies[(len(latencies)-1)*99/100]
	elapsed := time.Since(start)
	t.Logf("mixed pairs=%d scans=%d elapsed=%s ns/op=%.0f B/op=%.0f allocs/op=%.2f point_samples=%d p99=%s max=%s live_heap_bytes=%d", writes, completedScans.Load(), elapsed, float64(elapsed.Nanoseconds())/float64(writes), float64(after.TotalAlloc-before.TotalAlloc)/float64(writes), float64(after.Mallocs-before.Mallocs)/float64(writes), len(latencies), p99, latencies[len(latencies)-1], after.HeapAlloc)
}
