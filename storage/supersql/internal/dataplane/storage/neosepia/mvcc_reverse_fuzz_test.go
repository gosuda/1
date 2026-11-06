package neosepia

import (
	"bytes"
	"io"
	"slices"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
)

func FuzzReverseMVCCVisibility(f *testing.F) {
	f.Add([]byte{0, 11, 1, 2, 5, 0, 0, 0, 5, 0, 2, 0, 11, 0, 0, 0})
	f.Add([]byte{0, 8, 3, 2, 1, 5, 2, 2, 3, 1, 0, 0, 3, 1, 2, 0, 8, 1, 1, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 192 {
			data = data[:192]
		}
		type event struct {
			key, end  byte
			timestamp model.Timestamp
			ordinal   uint64
			kind      storage.MutationOp
		}
		var events []event
		var groups [2][]event
		for i := 0; i+3 < len(data); i += 4 {
			key := byte('a') + data[i]%12
			item := event{key: key, timestamp: model.Timestamp(1 + data[i+2]%4), ordinal: uint64(i/4 + 1), kind: storage.MutationUpsert}
			switch data[i+3] % 3 {
			case 1:
				item.kind = storage.MutationDelete
			case 2:
				item.kind = storage.MutationDeleteRange
				item.end = key + 1 + data[i+1]%(byte('m')-key)
			}
			events = append(events, item)
			groups[i/4%2] = append(groups[i/4%2], item)
		}
		var runs []*sstFile
		for _, group := range groups {
			if len(group) == 0 {
				continue
			}
			slices.SortFunc(group, func(a, b event) int {
				if a.key < b.key {
					return -1
				}
				if a.key > b.key {
					return 1
				}
				return storage.CompareVersion(b.timestamp, b.ordinal, a.timestamp, a.ordinal)
			})
			blocks := make([][]byte, 0, len(group))
			for _, item := range group {
				versions := blockVersions{Keys: encTestKeyVector(string([]byte{item.key})), CommitTS: []model.Timestamp{item.timestamp}, Ordinals: []uint64{item.ordinal}, Kinds: []storage.MutationOp{item.kind}}
				batch := columnar.Batch{Length: 1}
				var columns []model.ColumnID
				if item.kind == storage.MutationDeleteRange {
					versions.EndKeys = encTestKeyVector(string([]byte{item.end}))
				}
				if item.kind == storage.MutationUpsert {
					columns = []model.ColumnID{1}
					batch.Columns = []columnar.Vector{{Type: model.Type{ID: 20, Storage: model.TypeInt64}, Length: 1, Int64: []int64{int64(item.ordinal)}}}
				}
				block, err := encodeBlock(t.Context(), encTestTarget(), versions, &batch, columns, encTestLimits())
				if err != nil {
					t.Fatal(err)
				}
				blocks = append(blocks, block)
			}
			runs = append(runs, openRun(t, writeRun(t, blocks...)))
		}
		for timestamp := model.Timestamp(1); timestamp <= 4; timestamp++ {
			var sources []*runVersionCursor
			for _, run := range runs {
				source, err := newReverseRunVersionCursor(run, 0, run.count, 1<<20, mergeLimits{MaxBytes: 4096, MaxKeyBytes: 64}, nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := source.Close(); err != nil {
						t.Error(err)
					}
				})
				sources = append(sources, source)
			}
			cursor, err := newMVCCCursor(t.Context(), sources, timestamp, mergeLimits{MaxBytes: 64 << 10, MaxKeyBytes: 64}, storage.ScanReverse)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cursor.Close(); err != nil {
					t.Error(err)
				}
			})
			var got, want []uint64
			var previous []byte
			for {
				row, err := cursor.Next(t.Context())
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				if previous != nil && bytes.Compare(previous, row.key) <= 0 {
					t.Fatalf("keys not strictly descending: %q then %q", previous, row.key)
				}
				previous = bytes.Clone(row.key)
				got = append(got, row.ordinal)
			}
			for key := byte('l'); key >= byte('a'); key-- {
				var point, interval event
				for _, item := range events {
					if item.timestamp > timestamp {
						continue
					}
					if item.kind == storage.MutationDeleteRange {
						if item.key <= key && key < item.end && storage.CompareVersion(item.timestamp, item.ordinal, interval.timestamp, interval.ordinal) > 0 {
							interval = item
						}
					} else if item.key == key && storage.CompareVersion(item.timestamp, item.ordinal, point.timestamp, point.ordinal) > 0 {
						point = item
					}
				}
				if point.ordinal == 0 || point.kind != storage.MutationUpsert {
					continue
				}
				if storage.CompareVersion(interval.timestamp, interval.ordinal, point.timestamp, point.ordinal) >= 0 {
					continue
				}
				want = append(want, point.ordinal)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("snapshot %d events %+v: got ordinals %v, want %v", timestamp, events, got, want)
			}
		}
	})
}
