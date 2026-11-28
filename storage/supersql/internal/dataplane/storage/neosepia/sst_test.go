package neosepia

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func runLimits() sstLimits {
	return sstLimits{Block: encTestLimits(), MaxBytes: 1 << 24, MaxIndexBytes: 1 << 20, MaxBlocks: 1024}
}

func runBlock(t *testing.T, key, end string, timestamp model.Timestamp, op storage.MutationOp, value int64) []byte {
	t.Helper()
	versions := blockVersions{Keys: encTestKeyVector(key), CommitTS: []model.Timestamp{timestamp}, Ordinals: []uint64{1}, Kinds: []storage.MutationOp{op}}
	batch := columnar.Batch{Length: 1}
	var columns []model.ColumnID
	if op == storage.MutationDeleteRange {
		versions.EndKeys = encTestKeyVector(end)
	}
	if op == storage.MutationUpsert {
		columns = []model.ColumnID{1}
		batch.Columns = []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 1, Int64: []int64{value}}}
	}
	data, err := encodeBlock(t.Context(), encTestTarget(), versions, &batch, columns, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeRun(t *testing.T, blocks ...[]byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.sst")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	writer, err := newSSTWriter(file, encTestTarget(), runLimits())
	if err != nil {
		t.Fatal(err)
	}
	for _, block := range blocks {
		if err := writer.Append(t.Context(), block); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Finish(t.Context()); err != nil {
		t.Fatal(err)
	}
	return path
}

func openRun(t *testing.T, path string) *sstFile {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	run, err := openSST(t.Context(), file, runLimits())
	if err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	t.Cleanup(func() {
		if err := run.Close(); err != nil {
			t.Error(err)
		}
	})
	return run
}

func TestSparseRunRetainsEarlierRangeTombstoneCandidates(t *testing.T) {
	path := writeRun(t, runBlock(t, "a", "z", 2, storage.MutationDeleteRange, 0), runBlock(t, "m", "", 1, storage.MutationUpsert, 40))
	run := openRun(t, path)
	first, end, err := run.Candidates(t.Context(), []byte("m"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if first != 0 || end != 2 {
		t.Fatal("sparse lookup hid an earlier range tombstone")
	}
	block, err := run.ReadBlock(t.Context(), first, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	_, rangeEnd, ts, _, op, err := block.Version(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(rangeEnd) != "z" || ts != 2 || op != storage.MutationDeleteRange {
		t.Fatal("range shadowing metadata changed")
	}
	if _, err := run.ReadBlock(t.Context(), first, 1, nil); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("block budget: %v", err)
	}
}

func TestSparseRunChecksOnlyOpenedBlockPayloads(t *testing.T) {
	first := runBlock(t, "a", "", 1, storage.MutationUpsert, 10)
	path := writeRun(t, first, runBlock(t, "b", "", 1, storage.MutationUpsert, 20))
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	position := int64(sstHeaderSize + len(first) + paxHeaderSize + 1)
	var value [1]byte
	_, readErr := file.ReadAt(value[:], position)
	value[0] ^= 1
	_, writeErr := file.WriteAt(value[:], position)
	if err := errors.Join(readErr, writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	run := openRun(t, path)
	block, err := run.ReadBlock(t.Context(), 0, 1<<20, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	vector, err := block.Column(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if vector.Int64[0] != 10 {
		t.Fatal("unrelated block changed")
	}
	if _, err := run.ReadBlock(t.Context(), 1, 1<<20, nil); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("selected corrupt block was accepted: %v", err)
	}
}

func TestSparseRunRejectsOutOfOrderBlockWithoutLosingPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.sst")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := newSSTWriter(file, encTestTarget(), runLimits())
	if err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err := writer.Append(t.Context(), runBlock(t, "b", "", 1, storage.MutationUpsert, 20)); err != nil {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err := writer.Append(t.Context(), runBlock(t, "a", "", 1, storage.MutationUpsert, 10)); !errors.Is(err, errPAXEncode) {
		t.Fatal(errors.Join(err, file.Close()))
	}
	if err := writer.Append(t.Context(), runBlock(t, "b", "", 2, storage.MutationUpsert, 30)); !errors.Is(err, errPAXEncode) {
		t.Fatalf("ascending versions across blocks accepted: %v", errors.Join(err, file.Close()))
	}
	if err := errors.Join(writer.Finish(t.Context()), file.Close()); err != nil {
		t.Fatal(err)
	}
	run := openRun(t, path)
	first, end, err := run.Candidates(t.Context(), []byte("b"), 0)
	if err != nil || first != 0 || end != 1 {
		t.Fatalf("valid run prefix lost: %v", err)
	}
}
