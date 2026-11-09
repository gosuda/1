package neosepia

import (
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

type recordingScanObserver struct {
	readBytes, decodedBytes storage.ObservationCount
	blocksRead              storage.ObservationCount
}

func (observer *recordingScanObserver) ObserveScan(observation storage.ScanObservation) {
	addObservation := func(target *storage.ObservationCount, value storage.ObservationCount) {
		if value.Known {
			target.Known = true
			target.Value += value.Value
		}
	}
	addObservation(&observer.readBytes, observation.ReadBytes)
	addObservation(&observer.decodedBytes, observation.DecodedBytes)
	addObservation(&observer.blocksRead, observation.BlocksRead)
}

func TestPhysicalBlockReadAndDecodeReportMeasuredBytes(t *testing.T) {
	run := openRun(t, writeRun(t, runBlock(t, "a", "", 1, storage.MutationUpsert, 7)))
	observer := &recordingScanObserver{}
	memory := &sourceMemory{limit: 1 << 20, observer: observer}
	block, err := run.ReadBlock(t.Context(), 0, 1<<20, memory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	if !observer.readBytes.Known || observer.readBytes.Value == 0 || !observer.blocksRead.Known || observer.blocksRead.Value != 1 || observer.decodedBytes.Known {
		t.Fatalf("read observation=%+v", observer)
	}
	vector, err := block.Column(t.Context(), model.ColumnID(1))
	if err != nil || vector.Length != 1 {
		t.Fatalf("decoded vector=%+v error=%v", vector, err)
	}
	if !observer.decodedBytes.Known || observer.decodedBytes.Value == 0 {
		t.Fatalf("decode observation=%+v", observer)
	}
}
