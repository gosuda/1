package sort_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
)

type tapeFS struct {
	runtime.OSFileSystem
	mu             sync.Mutex
	created        int
	transfers      map[int64]int
	reads          int
	initialRuns    int
	failGeneration int
	cancel         context.CancelFunc
	cleanupErr     error
}

type tapeFile struct {
	*os.File
	fs         *tapeFS
	generation int
}

func (fs *tapeFS) CreateTemp(dir, pattern string) (runtime.SpillFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.created++
	return &tapeFile{File: f, fs: fs, generation: fs.created}, nil
}

func (f *tapeFile) WriteAt(p []byte, offset int64) (int, error) {
	// These fixtures fit in a single physical write. Inspect actual RSW1 records
	// to count transfers, not sorter bookkeeping or elapsed time.
	if len(p) < 32 || binary.LittleEndian.Uint32(p[:4]) != 0x31575352 {
		return 0, fmt.Errorf("unexpected spill frame of %d bytes", len(p))
	}
	value, err := storage.DecodeValue(context.Background(), p[32:], 32768)
	if err != nil {
		return 0, err
	}
	ordinal := value.ArrayVal[1].IntVal
	f.fs.mu.Lock()
	fail := f.generation == f.fs.failGeneration && ordinal >= 0
	cancel := f.fs.cancel
	if ordinal < 0 {
		if f.generation == 1 {
			f.fs.initialRuns++
		}
	} else if f.fs.transfers != nil {
		f.fs.transfers[ordinal]++
	}
	f.fs.mu.Unlock()
	if fail {
		if cancel != nil {
			n, err := f.File.WriteAt(p, offset)
			cancel()
			return n, err
		}
		n, err := f.File.WriteAt(p[:3], offset)
		return n, errors.Join(err, syscall.ENOSPC)
	}
	return f.File.WriteAt(p, offset)
}

func (f *tapeFile) ReadAt(p []byte, offset int64) (int, error) {
	f.fs.mu.Lock()
	f.fs.reads++
	f.fs.mu.Unlock()
	return f.File.ReadAt(p, offset)
}

func (f *tapeFile) Close() error {
	return errors.Join(f.File.Close(), f.fs.cleanupErr)
}

func tapeRows(count int) [][]model.Value {
	rows := make([][]model.Value, count)
	for i := range rows {
		rows[i] = []model.Value{model.Int64Val(int64(i % 7)), model.Int64Val(int64(i)), model.StringVal(strings.Repeat("p", 256))}
	}
	return rows
}

func TestManyRunsTransferOncePerBalancedPassAndStreamFinalMerge(t *testing.T) {
	if testing.Short() {
		t.Skip("long end-to-end fixture; run without -short")
	}
	for _, maxRuns := range []int{2, 3, 5} {
		t.Run(fmt.Sprint(maxRuns), func(t *testing.T) {
			dir := t.TempDir()
			fs := &tapeFS{transfers: make(map[int64]int)}
			q := query(t, fs, false, dir)
			layouts := []model.TypeLayout{integer, integer, text}
			const count = 1025
			opts := options(t, q, layouts, []externalsort.Key{{Column: 0}}, 512)
			opts.MaxRuns = maxRuns
			c := open(t, t.Context(), newSource(t, layouts, tapeRows(count)), opts)
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			first, _, err := columnar.CopyValue(t.Context(), &out.Columns[1], out.Lane(0), 4096)
			if err != nil {
				t.Fatal(err)
			}
			fs.mu.Lock()
			initialRuns, firstReads, filesCreated := fs.initialRuns, fs.reads, fs.created
			transfers := make(map[int64]int, len(fs.transfers))
			for ordinal, n := range fs.transfers {
				transfers[ordinal] = n
			}
			fs.mu.Unlock()
			if initialRuns != count {
				t.Fatalf("one-row runs = %d, want %d", initialRuns, count)
			}
			passes := 0
			for runs := initialRuns; runs > opts.MergeFanIn; runs = (runs + opts.MergeFanIn - 1) / opts.MergeFanIn {
				passes++
			}
			if len(transfers) != count {
				t.Fatalf("transferred identities = %d, want %d", len(transfers), count)
			}
			for ordinal := range count {
				if n := transfers[int64(ordinal)]; n != passes+1 {
					t.Fatalf("row %d transferred %d times, want initial write plus %d passes", ordinal, n, passes)
				}
			}
			if filesCreated != passes+1 {
				t.Fatalf("created %d tapes for %d passes", filesCreated, passes)
			}
			usage := q.Usage()
			if usage.Files != 1 || usage.Readers > opts.MergeFanIn || usage.PeakFiles > 3 || usage.PeakReaders > opts.MergeFanIn || usage.Work.Peak > int64(opts.WorkBytes) {
				t.Fatalf("unbounded tape resources: %+v", usage)
			}
			got := append([]int64{first.IntVal}, ids(t, c, 1, 17)...)
			var want []int64
			for key := range 7 {
				for ordinal := key; ordinal < count; ordinal += 7 {
					want = append(want, int64(ordinal))
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("balanced merge lost rows, crossed run boundaries, or reordered stable ties")
			}
			fs.mu.Lock()
			defer fs.mu.Unlock()
			if fs.reads <= firstReads || fs.created != filesCreated || !reflect.DeepEqual(fs.transfers, transfers) {
				t.Fatalf("final output was not read-only streaming: reads %d -> %d, files %d -> %d", firstReads, fs.reads, filesCreated, fs.created)
			}
			assertReleased(t, q, dir)
		})
	}
}

func TestIntermediatePassFailuresCloseBothTapes(t *testing.T) {
	for _, scenario := range []string{"disk-full", "canceled", "cleanup-failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			fs := &tapeFS{failGeneration: 2}
			if scenario == "canceled" {
				fs.cancel = cancel
			}
			cleanup := errors.New("spill close failed")
			if scenario == "cleanup-failure" {
				fs.cleanupErr = cleanup
			}
			q := query(t, fs, false, dir)
			layouts := []model.TypeLayout{integer, integer, text}
			input := newSource(t, layouts, tapeRows(17))
			c := open(t, ctx, input, options(t, q, layouts, []externalsort.Key{{Column: 0}}, 512))
			var out columnar.Batch
			err := c.Next(t.Context(), &out)
			if scenario == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled pass = %v", err)
				}
			} else if !errors.Is(err, syscall.ENOSPC) {
				t.Fatalf("partial pass write lost ENOSPC: %v", err)
			}
			if scenario == "cleanup-failure" && !errors.Is(err, cleanup) {
				t.Fatalf("pass lost cleanup cause: %v", err)
			}
			if !input.closed || out.Rows() != 0 || fs.created != 2 {
				t.Fatalf("failed intermediate pass: input closed=%v, rows=%d, tapes=%d", input.closed, out.Rows(), fs.created)
			}
			if scenario != "cleanup-failure" {
				assertReleased(t, q, dir)
				return
			}
			// Runtime retains disk admission when close fails, even if unlink
			// succeeds; the sort must still release readers and work ownership.
			usage := q.Usage()
			if usage.Work.Live != 0 || usage.Output.Live != 0 || usage.Files != 0 || usage.Readers != 0 || usage.Spill.Live == 0 {
				t.Fatalf("failed-close ownership: %+v", usage)
			}
			files, readErr := os.ReadDir(dir)
			if readErr != nil || len(files) != 0 {
				t.Fatalf("remaining tape paths: %v, %v", files, readErr)
			}
		})
	}
}

func TestFinalMergeReadFailureReleasesTape(t *testing.T) {
	dir := t.TempDir()
	fs := &tapeFS{}
	q := query(t, fs, false, dir)
	layouts := []model.TypeLayout{integer, integer, text}
	c := open(t, t.Context(), newSource(t, layouts, tapeRows(17)), options(t, q, layouts, []externalsort.Key{{Column: 0}}, 512))
	out := columnar.Batch{MaxRows: 1}
	if err := c.Next(t.Context(), &out); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("final tape: %v, %v", files, err)
	}
	if err := os.Truncate(dir+string(os.PathSeparator)+files[0].Name(), 0); err != nil {
		t.Fatal(err)
	}
	// At most fan-in heads are already decoded; reading a successor must fail.
	for range 3 {
		err = c.Next(t.Context(), &out)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated final tape = %v", err)
	}
	assertReleased(t, q, dir)
}
