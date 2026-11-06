package sort_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/gosuda/supersql-alpha/internal/columnar"
	"github.com/gosuda/supersql-alpha/internal/dataplane/runtime"
	"github.com/gosuda/supersql-alpha/internal/dataplane/storage"
	"github.com/gosuda/supersql-alpha/internal/model"
	"github.com/gosuda/supersql-alpha/internal/query/builtin"
	externalsort "github.com/gosuda/supersql-alpha/internal/query/exec/sort"
)

var integer = model.TypeLayout{Type: model.Type{ID: 20, Storage: model.TypeInt64}}
var text = model.TypeLayout{Type: model.Type{ID: 25, Storage: model.TypeString, Collation: 950}}

type source struct {
	builder  *columnar.Builder
	rows     [][]model.Value
	index    int
	empty    bool
	closed   bool
	closeErr error
}

func newSource(t *testing.T, layouts []model.TypeLayout, rows [][]model.Value) *source {
	t.Helper()
	b, err := columnar.NewBuilder(layouts, columnar.BuilderOptions{MaxRows: 1024, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &source{builder: b, rows: rows, empty: true}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (s *source) Next(ctx context.Context, out *columnar.Batch) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.builder.Reset()
	if s.empty {
		s.empty = false
		return s.builder.Publish(out)
	}
	if s.index == len(s.rows) {
		return io.EOF
	}
	limit := 1024
	if out.MaxRows > 0 {
		limit = min(limit, out.MaxRows)
	}
	for range min(limit, len(s.rows)-s.index) {
		if err := s.builder.Append(ctx, s.rows[s.index]); err != nil {
			return err
		}
		s.index++
	}
	s.empty = true
	return s.builder.Publish(out)
}

func (s *source) Close() error {
	s.closed = true
	return errors.Join(s.builder.Close(), s.closeErr)
}

func query(t *testing.T, fs runtime.FileSystem, disabled bool, directory string) *runtime.Query {
	t.Helper()
	pool, err := runtime.NewPool(runtime.PoolConfig{Parallelism: 1, QueueCapacity: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	budget, err := runtime.NewBudget(64<<20, 1024)
	if err != nil {
		t.Fatal(err)
	}
	config := runtime.QueryConfig{WorkBytes: 32 << 20, OutputBytes: 4 << 20, SpillBytes: 32 << 20, Directory: directory, MaxFiles: 3, MaxReaders: 2}
	if disabled {
		config.SpillMode = runtime.SpillDisabled
	}
	q, err := runtime.NewQuery(t.Context(), pool, budget, fs, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func options(t *testing.T, q *runtime.Query, layouts []model.TypeLayout, keys []externalsort.Key, runBytes uint64) externalsort.Options {
	t.Helper()
	encoder, err := builtin.NewKeyEncoder(950)
	if err != nil {
		t.Fatal(err)
	}
	return externalsort.Options{Layouts: layouts, Keys: keys, Encoder: encoder, Query: q, BatchRows: 1024, BatchBytes: 1 << 20, MaxRecordBytes: 4096, RunBytes: runBytes, MaxRuns: 4, MergeFanIn: 2, WorkBytes: 16 << 20}
}

func open(t *testing.T, ctx context.Context, input storage.BatchCursor, opts externalsort.Options) storage.BatchCursor {
	t.Helper()
	c, err := externalsort.New(ctx, input, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func ids(t *testing.T, c storage.BatchCursor, column, batchRows int) []int64 {
	t.Helper()
	var result []int64
	out := columnar.Batch{MaxRows: batchRows}
	for {
		err := c.Next(t.Context(), &out)
		if err == io.EOF {
			return result
		}
		if err != nil {
			t.Fatal(err)
		}
		if out.Rows() > batchRows {
			t.Fatalf("batch rows = %d, cap = %d", out.Rows(), batchRows)
		}
		for i := range out.Rows() {
			v, _, err := columnar.CopyValue(t.Context(), &out.Columns[column], out.Lane(i), 4096)
			if err != nil {
				t.Fatal(err)
			}
			result = append(result, v.IntVal)
		}
	}
}

type payloadChecker struct {
	storage.BatchCursor
	t *testing.T
}

func (c payloadChecker) Next(ctx context.Context, out *columnar.Batch) error {
	if err := c.BatchCursor.Next(ctx, out); err != nil {
		return err
	}
	for i := range out.Rows() {
		lane := out.Lane(i)
		id, _, err := columnar.CopyValue(ctx, &out.Columns[1], lane, 4096)
		if err != nil {
			c.t.Fatal(err)
		}
		key, _, err := columnar.CopyValue(ctx, &out.Columns[0], lane, 4096)
		if err != nil {
			c.t.Fatal(err)
		}
		payload, _, err := columnar.CopyValue(ctx, &out.Columns[2], lane, 4096)
		if err != nil {
			c.t.Fatal(err)
		}
		if key.IntVal != (96-id.IntVal)%7 || payload.StrVal != fmt.Sprintf("payload-%03d", id.IntVal) {
			c.t.Fatalf("row %d lost payload: key=%d, text=%q", id.IntVal, key.IntVal, payload.StrVal)
		}
	}
	return nil
}

func assertReleased(t *testing.T, q *runtime.Query, dir string) {
	t.Helper()
	u := q.Usage()
	workRetained := u.Work.Live != 0 || u.Work.Reservations != 0
	outputRetained := u.Output.Live != 0 || u.Output.Reservations != 0
	spillRetained := u.Spill.Live != 0 || u.Files != 0 || u.Readers != 0
	if workRetained || outputRetained || spillRetained {
		t.Fatalf("live sort resources: %+v", u)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("remaining spill files: %v, %v", files, err)
	}
}

func TestMemoryAndMultipassSpillHaveIdenticalStableOrder(t *testing.T) {
	layouts := []model.TypeLayout{integer, integer, text}
	var rows [][]model.Value
	var want []int64
	for i := range 97 {
		rows = append(rows, []model.Value{model.Int64Val(int64((96 - i) % 7)), model.Int64Val(int64(i)), model.StringVal(fmt.Sprintf("payload-%03d", i))})
	}
	for key := range 7 {
		for i := range 97 {
			if (96-i)%7 == key {
				want = append(want, int64(i))
			}
		}
	}
	for _, batchRows := range []int{1, 17, 1024} {
		for _, spill := range []bool{false, true} {
			t.Run(fmt.Sprintf("batch%d/spill%v", batchRows, spill), func(t *testing.T) {
				dir := t.TempDir()
				q := query(t, runtime.OSFileSystem{}, !spill, dir)
				runBytes := uint64(1 << 20)
				if spill {
					runBytes = 512
				}
				input := newSource(t, layouts, rows)
				opts := options(t, q, layouts, []externalsort.Key{{Column: 0}}, runBytes)
				opts.BatchRows = batchRows
				c := open(t, t.Context(), input, opts)
				got := ids(t, payloadChecker{c, t}, 1, batchRows)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("stable ids = %v, want %v", got, want)
				}
				var gotChecksum, wantChecksum uint64
				for i := range got {
					gotChecksum = gotChecksum*131 + uint64(got[i]+1)
					wantChecksum = wantChecksum*131 + uint64(want[i]+1)
				}
				if gotChecksum != wantChecksum {
					t.Fatalf("ordered checksum = %x, want %x", gotChecksum, wantChecksum)
				}
				if !input.closed {
					t.Fatal("input ownership not released")
				}
				usage := q.Usage()
				if spill && usage.PeakFiles == 0 {
					t.Fatal("forced spill did not use files")
				}
				if !spill && usage.PeakFiles != 0 {
					t.Fatal("memory sort spilled")
				}
				if usage.PeakFiles > 3 || usage.PeakReaders > 2 {
					t.Fatalf("unbounded merge: %+v", usage)
				}
				assertReleased(t, q, dir)
			})
		}
	}
}

func TestCompositeNumericDescendingNullsAndStablePeers(t *testing.T) {
	numeric := model.TypeLayout{Type: model.Type{ID: 1700, Storage: model.TypeNumeric}}
	layouts := []model.TypeLayout{numeric, text, integer}
	values := []string{"9007199254740992", "NaN", "1.0", "9007199254740993", "1", "-Infinity", "Infinity", "NULL", "0", "-0", "NaN", "1.000"}
	labels := []string{"x", "b", "b", "x", "a", "x", "x", "x", "x", "x", "a", "b"}
	var rows [][]model.Value
	for i, value := range values {
		v := model.Value{Type: model.TypeNumeric, StrVal: value}
		if value == "NULL" {
			v = model.NullVal(model.TypeNumeric)
		}
		rows = append(rows, []model.Value{v, model.StringVal(labels[i]), model.Int64Val(int64(i))})
	}
	want := []int64{7, 10, 1, 6, 3, 0, 4, 2, 11, 8, 9, 5}
	for _, runBytes := range []uint64{512, 1 << 20} {
		t.Run(fmt.Sprint(runBytes), func(t *testing.T) {
			dir := t.TempDir()
			q := query(t, runtime.OSFileSystem{}, false, dir)
			opts := options(t, q, layouts, []externalsort.Key{{Column: 0, Descending: true, NullsFirst: true}, {Column: 1}}, runBytes)
			c := open(t, t.Context(), newSource(t, layouts, rows), opts)
			if got := ids(t, c, 2, 17); !reflect.DeepEqual(got, want) {
				t.Fatalf("numeric order = %v, want %v", got, want)
			}
			assertReleased(t, q, dir)
		})
	}
}

func TestFloatNaNInfinityAndSignedZeroPeers(t *testing.T) {
	floating := model.TypeLayout{Type: model.Type{ID: 701, Storage: model.TypeFloat64}}
	layouts := []model.TypeLayout{floating, integer}
	var rows [][]model.Value
	for i, value := range []float64{math.NaN(), math.Inf(1), 0, math.Inf(-1), math.Copysign(0, -1), -1, math.NaN()} {
		rows = append(rows, []model.Value{model.Float64Val(value), model.Int64Val(int64(i))})
	}
	rows = append(rows, []model.Value{model.NullVal(model.TypeFloat64), model.Int64Val(7)})
	dir := t.TempDir()
	q := query(t, runtime.OSFileSystem{}, false, dir)
	c := open(t, t.Context(), newSource(t, layouts, rows), options(t, q, layouts, []externalsort.Key{{Column: 0}}, 256))
	want := []int64{3, 5, 2, 4, 1, 0, 6, 7}
	if got := ids(t, c, 1, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("float order = %v, want %v", got, want)
	}
	assertReleased(t, q, dir)
}

func TestEmptyInputAndConstructorOwnership(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		dir := t.TempDir()
		q := query(t, runtime.OSFileSystem{}, true, dir)
		input := newSource(t, []model.TypeLayout{integer}, nil)
		c := open(t, t.Context(), input, options(t, q, []model.TypeLayout{integer}, []externalsort.Key{{Column: 0}}, 512))
		if got := ids(t, c, 0, 1); len(got) != 0 {
			t.Fatalf("empty rows = %v", got)
		}
		assertReleased(t, q, dir)
	})
	t.Run("invalid", func(t *testing.T) {
		input := newSource(t, []model.TypeLayout{integer}, nil)
		cleanup := errors.New("input cleanup")
		input.closeErr = cleanup
		_, err := externalsort.New(t.Context(), input, externalsort.Options{})
		if !errors.Is(err, runtime.ErrInvalidConfig) || !errors.Is(err, cleanup) || !input.closed {
			t.Fatalf("constructor ownership: %v, closed=%v", err, input.closed)
		}
	})
}

func TestEarlyCloseAndCancellationReleaseRuns(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			dir := t.TempDir()
			q := query(t, runtime.OSFileSystem{}, false, dir)
			layouts := []model.TypeLayout{integer}
			var rows [][]model.Value
			for i := range 40 {
				rows = append(rows, []model.Value{model.Int64Val(int64(40 - i))})
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			input := newSource(t, layouts, rows)
			c := open(t, ctx, input, options(t, q, layouts, []externalsort.Key{{Column: 0}}, 256))
			out := columnar.Batch{MaxRows: 1}
			if err := c.Next(t.Context(), &out); err != nil {
				t.Fatal(err)
			}
			if q.Usage().Files == 0 {
				t.Fatal("scenario did not retain spill runs")
			}
			if canceled {
				cancel()
				if err := c.Next(t.Context(), &out); !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled Next = %v", err)
				}
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if !input.closed {
				t.Fatal("input remains open")
			}
			assertReleased(t, q, dir)
		})
	}
}

type diskFullFS struct{ runtime.OSFileSystem }
type diskFullFile struct{ *os.File }

func (diskFullFS) CreateTemp(dir, pattern string) (runtime.SpillFile, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return diskFullFile{f}, nil
}
func (f diskFullFile) WriteAt(p []byte, offset int64) (int, error) {
	n, err := f.File.WriteAt(p[:min(3, len(p))], offset)
	return n, errors.Join(err, syscall.ENOSPC)
}

func TestSpillFailureAndOversizedRecordsReleaseOwnership(t *testing.T) {
	shortRows := [][]model.Value{{model.StringVal("c")}, {model.StringVal("b")}, {model.StringVal("a")}, {model.StringVal("d")}}
	for _, scenario := range []struct {
		name     string
		fs       runtime.FileSystem
		disabled bool
		rows     [][]model.Value
		want     error
	}{
		{"disabled", runtime.OSFileSystem{}, true, shortRows, runtime.ErrSpillDisabled},
		{"disk-full", diskFullFS{}, false, shortRows, syscall.ENOSPC},
		{"oversized", runtime.OSFileSystem{}, false, [][]model.Value{{model.StringVal(strings.Repeat("x", 8192))}}, runtime.ErrBudget},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			dir := t.TempDir()
			q := query(t, scenario.fs, scenario.disabled, dir)
			layouts := []model.TypeLayout{text}
			input := newSource(t, layouts, scenario.rows)
			c := open(t, t.Context(), input, options(t, q, layouts, []externalsort.Key{{Column: 0}}, 256))
			var out columnar.Batch
			err := c.Next(t.Context(), &out)
			if !errors.Is(err, scenario.want) {
				t.Fatalf("%s error = %v, want %v", scenario.name, err, scenario.want)
			}
			if !input.closed {
				t.Fatal("failed sort retained input")
			}
			assertReleased(t, q, dir)
		})
	}
}

type selectedSource struct{ *source }

func (s selectedSource) Next(ctx context.Context, out *columnar.Batch) error {
	if err := s.source.Next(ctx, out); err != nil {
		return err
	}
	if out.Rows() > 0 {
		out.Selection = []uint32{2, 0, 1}
	}
	return nil
}

func TestSelectionOrderDefinesStableTiesAndOptionsAreOwned(t *testing.T) {
	dir := t.TempDir()
	q := query(t, runtime.OSFileSystem{}, false, dir)
	layouts := []model.TypeLayout{integer, integer}
	rows := [][]model.Value{
		{model.Int64Val(1), model.Int64Val(0)},
		{model.Int64Val(1), model.Int64Val(1)},
		{model.Int64Val(1), model.Int64Val(2)},
		{model.Int64Val(-9), model.Int64Val(3)},
	}
	input := selectedSource{newSource(t, layouts, rows)}
	opts := options(t, q, layouts, []externalsort.Key{{Column: 0}}, 256)
	c := open(t, t.Context(), input, opts)
	opts.Keys[0].Column = 1
	opts.Layouts[0] = text
	want := []int64{2, 0, 1}
	if got := ids(t, c, 1, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("selection order = %v, want %v", got, want)
	}
	assertReleased(t, q, dir)
}

func TestCanceledConstructorClosesInputWithoutReading(t *testing.T) {
	dir := t.TempDir()
	q := query(t, runtime.OSFileSystem{}, false, dir)
	input := newSource(t, []model.TypeLayout{integer}, [][]model.Value{{model.Int64Val(1)}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := externalsort.New(ctx, input, options(t, q, []model.TypeLayout{integer}, []externalsort.Key{{Column: 0}}, 256))
	if !errors.Is(err, context.Canceled) || !input.closed || input.index != 0 {
		t.Fatalf("canceled construction: %v, closed=%v, read=%d", err, input.closed, input.index)
	}
	assertReleased(t, q, dir)
}
