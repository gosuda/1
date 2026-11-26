package neosepia

import (
	"errors"
	"os"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
)

func TestScrubRangeCountsHistoryAndDetectsPayloadDamage(t *testing.T) {
	fixture := openBackendFixture(t)
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 1, 20, 100, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.backend.ApplyMutations(t.Context(), fixture.batch(t, 2, 30, 90, 110)); err != nil {
		t.Fatal(err)
	}
	run := fixture.backend.runs.head.files[0]
	request := storage.ScrubRequest{Target: run.target, StartKey: backendKey(0), EndKey: backendKey(2)}
	report, err := fixture.backend.ScrubRange(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if report.ScannedRows != 2 || report.BlockCount != 2 {
		t.Fatalf("physical history scrub: %+v", report)
	}
	entry := run.entry(0)
	file, err := fixture.backend.directory.runs.OpenFile(preparedRunName(2, 0), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	var damage [1]byte
	offset := int64(entry.offset + entry.length - 1)
	if _, err := file.ReadAt(damage[:], offset); err != nil {
		file.Close()
		t.Fatal(err)
	}
	damage[0] ^= 255
	if _, err := file.WriteAt(damage[:], offset); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	report, err = fixture.backend.ScrubRange(t.Context(), request)
	if !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("damaged PAX payload: %v", err)
	}
	if report.ScannedRows != 0 || report.BlockCount != 0 {
		t.Fatal("failed scrub exposed partial-success counters")
	}
}
