package exec

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

type stubBufferCacheStats struct {
	hits   uint64
	misses uint64
}

func (s *stubBufferCacheStats) BlockCacheStats() (hits, misses uint64) {
	return s.hits, s.misses
}

func TestInstrumentationBufferStats(t *testing.T) {
	inst := NewInstrumentation(true)
	inst.nodes = make([]nodeCounters, 3)

	provider := &stubBufferCacheStats{hits: 100, misses: 40}
	inst.beginBuffers(provider)

	// Simulate cache activity during execution.
	provider.hits = 117
	provider.misses = 43

	// Simulate scan node reporting 5 physical blocks read (greater than cache misses delta 3).
	inst.nodes[1].ObserveScan(storage.ScanObservation{
		BlocksRead: storage.ObservationCount{Value: 2, Known: true},
	})
	inst.nodes[2].ObserveScan(storage.ScanObservation{
		BlocksRead: storage.ObservationCount{Value: 3, Known: true},
	})

	// Simulate temporary spill usage (20 KiB = 3 8-KiB blocks).
	inst.observeUsage(runtime.QueryUsage{
		Spill:       runtime.Usage{Peak: 20480},
		PeakFiles:   1,
		PeakReaders: 1,
	})
	inst.finish()

	obs := inst.Snapshot()
	if obs.Buffers.SharedHitBlocks != 17 {
		t.Fatalf("SharedHitBlocks = %d, want 17", obs.Buffers.SharedHitBlocks)
	}
	if obs.Buffers.SharedReadBlocks != 5 {
		t.Fatalf("SharedReadBlocks = %d, want 5", obs.Buffers.SharedReadBlocks)
	}
	if obs.Buffers.TempWrittenBytes != 20480 || obs.Buffers.TempWrittenBlocks != 3 {
		t.Fatalf("TempWritten = %d bytes / %d blocks, want 20480 / 3", obs.Buffers.TempWrittenBytes, obs.Buffers.TempWrittenBlocks)
	}
	if obs.Buffers.TempReadBytes != 20480 || obs.Buffers.TempReadBlocks != 3 {
		t.Fatalf("TempRead = %d bytes / %d blocks, want 20480 / 3", obs.Buffers.TempReadBytes, obs.Buffers.TempReadBlocks)
	}
}
