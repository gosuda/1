package neosepia

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func TestSmallVersionsFitDefaultNativeSourceBudget(t *testing.T) {
	set, err := newRunSet(runSetLimits{MaxBytes: 4 << 20, MaxFiles: 64, MaxPins: 4})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := set.Close(); err != nil {
			t.Error(err)
		}
	})
	for version := 1; version <= 32; version++ {
		timestamp := model.Timestamp(version * 10)
		file := openRun(t, writeRun(t, runBlock(t, "k", "", timestamp, storage.MutationUpsert, int64(version))))
		if err := set.publish(t.Context(), uint64(version), timestamp, []*sstFile{file}); err != nil {
			t.Fatal(err)
		}
	}
	pin, err := set.pin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pin.Close(); err != nil {
			t.Error(err)
		}
	})
	defaults := DefaultLimits()
	spec := scanFixtureSpec()
	point := pinnedPointLimits{SourceBytes: defaults.ReadSourceBytes, BlockBytes: defaults.ReadBlockBytes,
		Merge: mergeLimits{MaxBytes: defaults.ReadMergeBytes, MaxKeyBytes: defaults.KeyBytes},
		Point: pointReadLimits{ScanBytes: defaults.ScanBytes, RowBytes: defaults.RowBytes}}
	request := storage.ReadRequest{Target: spec.Target, Key: []byte("k"), ColumnIDs: spec.Projection, Layout: spec.Layout}
	for _, at := range []struct {
		timestamp model.Timestamp
		value     int64
	}{{75, 7}, {320, 32}} {
		row, err := pin.get(t.Context(), at.timestamp, nil, request, point)
		if err != nil {
			t.Fatal(err)
		}
		if len(row.Values) != 1 || row.Values[0].IntVal != at.value {
			t.Fatalf("snapshot %d returned %+v, want %d", at.timestamp, row.Values, at.value)
		}
	}
	for _, direction := range []storage.ScanDirection{storage.ScanForward, storage.ScanReverse} {
		cursor, err := pin.scan(t.Context(), 320, direction, spec, pinnedScanLimits{SourceBytes: defaults.ReadSourceBytes, BlockBytes: defaults.ReadBlockBytes,
			Merge: point.Merge, Reverse: mergeLimits{MaxBytes: defaults.ReadMergeBytes, MaxKeyBytes: defaults.KeyBytes}})
		if err != nil {
			t.Fatal(err)
		}
		var values []int64
		batch := columnar.Batch{MaxRows: 1}
		for {
			err := cursor.Next(t.Context(), &batch)
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				break
			}
			for row := range batch.Rows() {
				value, _, err := columnar.CopyValue(t.Context(), &batch.Columns[0], batch.Lane(row), 1024)
				if err != nil {
					t.Error(err)
					break
				}
				values = append(values, value.IntVal)
			}
			batch.Reset()
		}
		batch.Reset()
		if err := cursor.Close(); err != nil {
			t.Error(err)
		}
		if len(values) != 1 || values[0] != 32 {
			t.Fatalf("direction %d returned %v", direction, values)
		}
	}
}

func TestSharedBlockBudgetReleasesClosedBlocks(t *testing.T) {
	file := openRun(t, writeRun(t,
		runBlock(t, strings.Repeat("a", 16<<10), "", 10, storage.MutationUpsert, 1),
		runBlock(t, strings.Repeat("b", 16<<10), "", 10, storage.MutationUpsert, 2)))
	memory := &sourceMemory{limit: 24 << 10}
	first, err := file.ReadBlock(t.Context(), 0, 1<<20, memory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := first.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := file.ReadBlock(t.Context(), 1, 1<<20, memory); !errors.Is(err, columnar.ErrBatchBudget) {
		t.Fatalf("two retained blocks: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := file.ReadBlock(t.Context(), 1, 1<<20, memory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Error(err)
		}
	})
	column, err := second.Column(t.Context(), 1)
	if err != nil || column.Int64[0] != 2 {
		t.Fatalf("replacement block: %v, %v", column.Int64, err)
	}
}

func TestSharedBlockBudgetReleasesCorruptReads(t *testing.T) {
	first := runBlock(t, strings.Repeat("a", 16<<10), "", 10, storage.MutationUpsert, 1)
	path := writeRun(t, first, runBlock(t, strings.Repeat("b", 16<<10), "", 10, storage.MutationUpsert, 2))
	writable, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	position := int64(sstHeaderSize + paxHeaderSize + 1)
	var value [1]byte
	_, readErr := writable.ReadAt(value[:], position)
	value[0] ^= 1
	_, writeErr := writable.WriteAt(value[:], position)
	if err := errors.Join(readErr, writeErr, writable.Close()); err != nil {
		t.Fatal(err)
	}
	file := openRun(t, path)
	memory := &sourceMemory{limit: 24 << 10}
	if _, err := file.ReadBlock(t.Context(), 0, 1<<20, memory); !errors.Is(err, storage.ErrCorruptStorage) {
		t.Fatalf("corrupt block: %v", err)
	}
	block, err := file.ReadBlock(t.Context(), 1, 1<<20, memory)
	if err != nil {
		t.Fatal(err)
	}
	if err := block.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFailedLazyDecodeDoesNotConsumeSharedBudget(t *testing.T) {
	batch := &columnar.Batch{Length: 1, Columns: []columnar.Vector{{Type: model.Type{ID: 25, Storage: model.TypeString}, Length: 1, Encoding: columnar.Flat, Data: []byte("value"), Ranges: []columnar.Range{{Length: 5}}}}}
	raw, err := encodeBlock(t.Context(), encTestTarget(), blockVersions{Keys: encTestKeyVector("a"), CommitTS: []model.Timestamp{10}, Ordinals: []uint64{1}, Kinds: []storage.MutationOp{storage.MutationUpsert}}, batch, []model.ColumnID{1}, encTestLimits())
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := parsePAXDescriptor(raw[paxHeaderSize:], false)
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.encoding != encodingPlain || descriptor.nulls != 0 {
		t.Fatal("fixture requires plain non-null text")
	}
	binary.LittleEndian.PutUint32(raw[descriptor.payload:], ^uint32(0))
	binary.LittleEndian.PutUint32(raw[56:], paxChecksum(raw))
	file := openRun(t, writeRun(t, raw))
	memory := &sourceMemory{limit: 4096}
	block, err := file.ReadBlock(t.Context(), 0, 1<<20, memory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := block.Close(); err != nil {
			t.Error(err)
		}
	})
	for attempt := 0; attempt < 1024; attempt++ {
		if _, err := block.Column(t.Context(), 1); !errors.Is(err, errPAXBlock) || errors.Is(err, columnar.ErrBatchBudget) {
			t.Fatalf("decode attempt %d changed error class: %v", attempt, err)
		}
	}
}
